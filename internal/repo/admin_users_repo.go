package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// This file backs the dashboard ops admin API (dashboard-admin-api spec):
// GET /admin/ops/metrics, GET /admin/users/search, GET /admin/users/:id and
// POST /admin/users/:id/vip. Query semantics mirror the dashboard's former
// SSH+psql path (yunhou-deploy dashboard/lib/ops.js + lib/users.js) exactly:
//
//   - metrics share one payment fact source (per-order latest paid payment)
//     so a multi-attempt order never double-counts users or revenue;
//   - day/week/month boundaries arrive as pre-computed UTC instants (the
//     service layer resolves the requested IANA tz; no AT TIME ZONE in SQL);
//   - VIP writes are scoped to product_code='kaya-membership' and run inside
//     AdminUsersRepo.WithTx together with the audit_log row and the
//     idempotency-key row (migration 037).

// AdminUserSearchRow is one row of the users ⨝ social_identities join used
// by both search and user detail. Identity columns are NULL when the user
// has no social_identities row (LEFT JOIN).
type AdminUserSearchRow struct {
	ID          string    `db:"id"`
	Nickname    *string   `db:"nickname"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	Provider    *string   `db:"provider"`
	ProviderUID *string   `db:"provider_uid"`
	Email       *string   `db:"email"`
}

// AdminActiveSubscriptionRow is the user's single active kaya-membership
// subscription (idx_subscriptions_user_product_active guarantees at most
// one) joined with the plan row. PlanIsActive/Price are NULL when the plan
// row is missing (LEFT JOIN) — the service treats that as a retired plan.
type AdminActiveSubscriptionRow struct {
	PlanID       string     `db:"plan_id"`
	StartedAt    time.Time  `db:"started_at"`
	ExpiresAt    *time.Time `db:"expires_at"`
	PlanIsActive *bool      `db:"plan_is_active"`
	Price        *float64   `db:"price"`
}

// AdminSubscriptionHistoryRow is one kaya-membership subscription history
// entry (any status), most recent first.
type AdminSubscriptionHistoryRow struct {
	PlanID    string     `db:"plan_id"`
	Status    string     `db:"status"`
	StartedAt time.Time  `db:"started_at"`
	ExpiresAt *time.Time `db:"expires_at"`
	CreatedAt time.Time  `db:"created_at"`
}

// AdminOpsCounts is the total/today/week/month shape shared by the three
// count metrics. The boundaries are exclusive-lower-bound instants
// (created_at >= dayStart etc.) computed by the service in the requested
// timezone and converted to UTC.
type AdminOpsCounts struct {
	Total int64 `db:"total"`
	Today int64 `db:"today"`
	Week  int64 `db:"week"`
	Month int64 `db:"month"`
}

// AdminOpsAmounts is the same shape for the revenue metric (元).
type AdminOpsAmounts struct {
	Total float64 `db:"total"`
	Today float64 `db:"today"`
	Week  float64 `db:"week"`
	Month float64 `db:"month"`
}

// AdminUsersTx is the transactional surface for POST /admin/users/:id/vip.
// Production wraps *sqlx.Tx; unit tests inject a fake. Every method joins
// the caller-managed transaction so the subscription write, the audit row
// and the idempotency key commit (or roll back) together.
type AdminUsersTx interface {
	// UserExists reports whether the users row exists at all (any status).
	UserExists(ctx context.Context, userID string) (bool, error)
	// FindActiveMembershipForUpdate reads the user's active kaya-membership
	// subscription under a FOR UPDATE row lock. Returns (nil, nil) when the
	// user has no active membership row.
	FindActiveMembershipForUpdate(ctx context.Context, userID string) (*AdminActiveSubscriptionRow, error)
	// InsertMembershipSub creates a new active 'monthly' kaya-membership
	// subscription expiring in days days and returns its plan/expiry.
	InsertMembershipSub(ctx context.Context, userID string, days int) (planID string, expiresAt time.Time, err error)
	// ExtendMembershipSub adds days to the active kaya-membership row
	// (GREATEST(expires_at, now()) + days). updated=false means the UPDATE
	// matched 0 rows — the pre-read active row was concurrently cancelled.
	ExtendMembershipSub(ctx context.Context, userID string, days int) (updated bool, planID string, expiresAt time.Time, err error)
	// GetIdempotencyResponse returns the stored first-success response for
	// (appID, key), or (nil, nil) when the key has never succeeded.
	GetIdempotencyResponse(ctx context.Context, appID, key string) (json.RawMessage, error)
	// InsertIdempotencyKey records a successful response under
	// (appID, key). inserted=false means a concurrent same-key request
	// committed first — the caller must roll back and replay that response.
	InsertIdempotencyKey(ctx context.Context, appID, key, action, target string, response json.RawMessage) (inserted bool, err error)
	// InsertAudit appends an audit_log row (actor 'admin:<appID>').
	InsertAudit(ctx context.Context, actor, action, target string, ctxData map[string]any) error
}

// AdminUsersRepo is the read surface for search/detail/metrics plus the
// VIP write transaction runner.
type AdminUsersRepo interface {
	// SearchUsers runs the 4-way match (exact id / nickname / identity
	// email / identity provider_uid). exact is the raw query; likePattern
	// is the same query with \, %, _ escaped for ILIKE ... ESCAPE '\'.
	// Row-level LIMIT 60, ORDER BY users.created_at DESC — per-user
	// aggregation and the 20-user cap live in the service layer.
	SearchUsers(ctx context.Context, exact, likePattern string) ([]AdminUserSearchRow, error)
	// FindUserWithIdentities returns the user row joined to all identities.
	// Empty slice = user does not exist.
	FindUserWithIdentities(ctx context.Context, userID string) ([]AdminUserSearchRow, error)
	// FindActiveMembership returns the user's active kaya-membership
	// subscription (nil when none). Read-only variant for user detail.
	FindActiveMembership(ctx context.Context, userID string) (*AdminActiveSubscriptionRow, error)
	// ListMembershipHistory returns the user's kaya-membership
	// subscriptions (any status) ordered by created_at DESC, capped.
	ListMembershipHistory(ctx context.Context, userID string, limit int) ([]AdminSubscriptionHistoryRow, error)

	// Ops metrics — boundaries are UTC instants resolved by the service
	// from the requested tz (day = tz-local 00:00, week = tz-local Monday
	// 00:00 matching PG date_trunc('week'), month = tz-local 1st 00:00).
	OpsUserCounts(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error)
	OpsPaidUsersCumulative(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error)
	OpsPaidUsersActive(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error)
	OpsRevenue(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsAmounts, error)

	// GetIdempotencyResponse is the non-transactional re-read used after a
	// same-key race forced a rollback: the winner's row is committed by
	// then, so its response can be replayed.
	GetIdempotencyResponse(ctx context.Context, appID, key string) (json.RawMessage, error)

	// WithTx runs fn inside a single transaction (BeginTxx/commit/rollback
	// dance identical to PlanRepo.WithTx).
	WithTx(ctx context.Context, fn func(tx AdminUsersTx) error) error
}

type adminUsersRepo struct {
	db *sqlx.DB
}

func NewAdminUsersRepo(db *sqlx.DB) *adminUsersRepo { return &adminUsersRepo{db: db} }

var _ AdminUsersRepo = (*adminUsersRepo)(nil)

// The kaya-membership scope constant: dashboard user management operates
// on the membership product only (spec §4 — 027 made subscriptions
// multi-product; coding-plan rows must never leak in).
const adminMembershipProduct = "kaya-membership"

// ErrAdminSubscriptionConflict marks a unique-constraint violation
// (SQLSTATE 23505) from InsertMembershipSub: a concurrent grant for the
// same (user, kaya-membership) won the partial unique index
// idx_subscriptions_user_product_active. The service layer turns it into
// an idempotent replay when the request carries an Idempotency-Key, and
// into a vip.reject 409 (with the audit row committed in the same tx)
// when it does not. InsertMembershipSub rolls back to a savepoint before
// returning this error so the tx stays usable for the audit write.
var ErrAdminSubscriptionConflict = errors.New("admin: subscription insert conflict")

// isAdminUniqueViolation reports whether err is a Postgres
// unique-constraint violation (SQLSTATE 23505), lib/pq flavour (same
// detection as service.isDuplicateKey).
func isAdminUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

func (r *adminUsersRepo) SearchUsers(ctx context.Context, exact, likePattern string) ([]AdminUserSearchRow, error) {
	rows := []AdminUserSearchRow{}
	err := r.db.SelectContext(ctx, &rows, `
		SELECT u.id, u.nickname, u.status, u.created_at,
		       s.provider, s.provider_uid, s.email
		  FROM users u
		  LEFT JOIN social_identities s ON s.user_id = u.id
		 WHERE u.id::text = $1
		    OR u.nickname ILIKE '%' || $2 || '%' ESCAPE '\'
		    OR s.email ILIKE '%' || $2 || '%' ESCAPE '\'
		    OR s.provider_uid ILIKE '%' || $2 || '%' ESCAPE '\'
		 ORDER BY u.created_at DESC
		 LIMIT 60
	`, exact, likePattern)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *adminUsersRepo) FindUserWithIdentities(ctx context.Context, userID string) ([]AdminUserSearchRow, error) {
	rows := []AdminUserSearchRow{}
	err := r.db.SelectContext(ctx, &rows, `
		SELECT u.id, u.nickname, u.status, u.created_at,
		       s.provider, s.provider_uid, s.email
		  FROM users u
		  LEFT JOIN social_identities s ON s.user_id = u.id
		 WHERE u.id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

const adminActiveMembershipSQL = `
	SELECT s.plan_id, s.started_at, s.expires_at,
	       p.is_active AS plan_is_active, p.price
	  FROM subscriptions s
	  LEFT JOIN plans p ON p.id = s.plan_id
	 WHERE s.user_id = $1 AND s.status = 'active' AND s.product_code = '` + adminMembershipProduct + `'`

func (r *adminUsersRepo) FindActiveMembership(ctx context.Context, userID string) (*AdminActiveSubscriptionRow, error) {
	var row AdminActiveSubscriptionRow
	err := r.db.GetContext(ctx, &row, adminActiveMembershipSQL, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (r *adminUsersRepo) ListMembershipHistory(ctx context.Context, userID string, limit int) ([]AdminSubscriptionHistoryRow, error) {
	rows := []AdminSubscriptionHistoryRow{}
	err := r.db.SelectContext(ctx, &rows, `
		SELECT s.plan_id, s.status, s.started_at, s.expires_at, s.created_at
		  FROM subscriptions s
		 WHERE s.user_id = $1 AND s.product_code = $2
		 ORDER BY s.created_at DESC
		 LIMIT $3
	`, userID, adminMembershipProduct, limit)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *adminUsersRepo) OpsUserCounts(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error) {
	var c AdminOpsCounts
	err := r.db.GetContext(ctx, &c, `
		SELECT
		  (SELECT count(*) FROM users WHERE status IS DISTINCT FROM 'deleted') AS total,
		  (SELECT count(*) FROM users WHERE status IS DISTINCT FROM 'deleted' AND created_at >= $1) AS today,
		  (SELECT count(*) FROM users WHERE status IS DISTINCT FROM 'deleted' AND created_at >= $2) AS week,
		  (SELECT count(*) FROM users WHERE status IS DISTINCT FROM 'deleted' AND created_at >= $3) AS month
	`, dayStart, weekStart, monthStart)
	return c, err
}

// paidPaymentsFactSource is the shared payment fact source (spec §2): one
// row per order with its latest successful payment. Joining payments
// directly would double-count orders with multiple payment attempts.
const paidPaymentsFactSource = `
	SELECT order_id, max(paid_at) AS paid_at
	  FROM payments
	 WHERE status = 'paid'
	 GROUP BY order_id
`

func (r *adminUsersRepo) OpsPaidUsersCumulative(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error) {
	var c AdminOpsCounts
	err := r.db.GetContext(ctx, &c, `
		SELECT
		  (SELECT count(DISTINCT o.user_id) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id) AS total,
		  (SELECT count(DISTINCT o.user_id) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $1) AS today,
		  (SELECT count(DISTINCT o.user_id) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $2) AS week,
		  (SELECT count(DISTINCT o.user_id) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $3) AS month
	`, dayStart, weekStart, monthStart)
	return c, err
}

func (r *adminUsersRepo) OpsPaidUsersActive(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsCounts, error) {
	var c AdminOpsCounts
	err := r.db.GetContext(ctx, &c, `
		SELECT
		  (SELECT count(DISTINCT s.user_id) FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		    WHERE s.status = 'active' AND p.price > 0 AND (s.expires_at IS NULL OR s.expires_at > now())) AS total,
		  (SELECT count(DISTINCT s.user_id) FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		    WHERE s.status = 'active' AND p.price > 0 AND (s.expires_at IS NULL OR s.expires_at > now()) AND s.created_at >= $1) AS today,
		  (SELECT count(DISTINCT s.user_id) FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		    WHERE s.status = 'active' AND p.price > 0 AND (s.expires_at IS NULL OR s.expires_at > now()) AND s.created_at >= $2) AS week,
		  (SELECT count(DISTINCT s.user_id) FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		    WHERE s.status = 'active' AND p.price > 0 AND (s.expires_at IS NULL OR s.expires_at > now()) AND s.created_at >= $3) AS month
	`, dayStart, weekStart, monthStart)
	return c, err
}

func (r *adminUsersRepo) OpsRevenue(ctx context.Context, dayStart, weekStart, monthStart time.Time) (AdminOpsAmounts, error) {
	var a AdminOpsAmounts
	err := r.db.GetContext(ctx, &a, `
		SELECT
		  (SELECT COALESCE(sum(o.amount), 0) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id) AS total,
		  (SELECT COALESCE(sum(o.amount), 0) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $1) AS today,
		  (SELECT COALESCE(sum(o.amount), 0) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $2) AS week,
		  (SELECT COALESCE(sum(o.amount), 0) FROM orders o JOIN (`+paidPaymentsFactSource+`) p ON p.order_id = o.id WHERE p.paid_at >= $3) AS month
	`, dayStart, weekStart, monthStart)
	return a, err
}

func (r *adminUsersRepo) GetIdempotencyResponse(ctx context.Context, appID, key string) (json.RawMessage, error) {
	return getIdempotencyResponse(ctx, r.db, appID, key)
}

// idemQuerier abstracts *sqlx.DB and *sqlx.Tx for the shared idempotency
// read helper.
type idemQuerier interface {
	QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row
}

func getIdempotencyResponse(ctx context.Context, q idemQuerier, appID, key string) (json.RawMessage, error) {
	var response json.RawMessage
	err := q.QueryRowxContext(ctx, `
		SELECT response FROM admin_idempotency_keys WHERE app_id = $1 AND key = $2
	`, appID, key).Scan(&response)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return response, nil
}

func (r *adminUsersRepo) WithTx(ctx context.Context, fn func(tx AdminUsersTx) error) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // safe to call after Commit
	if err := fn(&adminUsersTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

type adminUsersTx struct {
	tx *sqlx.Tx
}

var _ AdminUsersTx = (*adminUsersTx)(nil)

func (t *adminUsersTx) UserExists(ctx context.Context, userID string) (bool, error) {
	var exists bool
	err := t.tx.QueryRowxContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
	return exists, err
}

func (t *adminUsersTx) FindActiveMembershipForUpdate(ctx context.Context, userID string) (*AdminActiveSubscriptionRow, error) {
	var row AdminActiveSubscriptionRow
	// FOR UPDATE OF s locks only the subscription row; the plans side of
	// the LEFT JOIN stays unlocked (and a missing plan row must not error).
	err := t.tx.GetContext(ctx, &row, adminActiveMembershipSQL+` FOR UPDATE OF s`, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func (t *adminUsersTx) InsertMembershipSub(ctx context.Context, userID string, days int) (string, time.Time, error) {
	var planID string
	var expiresAt time.Time
	// Explicit product_code matches the 'monthly' plan row, so the
	// trg_subscriptions_plan_product trigger (027) passes; a missing
	// 'monthly' plan makes the trigger raise → 500 at the handler.
	//
	// The INSERT runs behind a savepoint: a 23505 (concurrent grant won
	// the partial unique index) aborts the whole Postgres transaction
	// otherwise, and the service could not write the vip.reject audit row
	// in the same tx. ROLLBACK TO SAVEPOINT keeps the tx usable.
	const sp = "admin_vip_insert"
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+sp); err != nil {
		return "", time.Time{}, fmt.Errorf("savepoint: %w", err)
	}
	err := t.tx.QueryRowxContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now(), now() + make_interval(days => $2), $3)
		RETURNING plan_id, expires_at
	`, userID, days, adminMembershipProduct).Scan(&planID, &expiresAt)
	if err != nil {
		if _, rbErr := t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+sp); rbErr != nil {
			return "", time.Time{}, fmt.Errorf("rollback to savepoint after insert failure: %v (insert err: %w)", rbErr, err)
		}
		if isAdminUniqueViolation(err) {
			// 并发 grant(同 key 或无 key)赢了部分唯一索引 —— 由 service
			// 层决定重放(带 Idempotency-Key)还是 vip.reject 409。
			return "", time.Time{}, ErrAdminSubscriptionConflict
		}
		return "", time.Time{}, err
	}
	return planID, expiresAt, nil
}

func (t *adminUsersTx) ExtendMembershipSub(ctx context.Context, userID string, days int) (bool, string, time.Time, error) {
	var planID string
	var expiresAt time.Time
	err := t.tx.QueryRowxContext(ctx, `
		UPDATE subscriptions
		   SET expires_at = GREATEST(expires_at, now()) + make_interval(days => $2),
		       updated_at = now()
		 WHERE user_id = $1 AND status = 'active' AND product_code = $3
		RETURNING plan_id, expires_at
	`, userID, days, adminMembershipProduct).Scan(&planID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The pre-read active row was concurrently cancelled/expired.
			return false, "", time.Time{}, nil
		}
		return false, "", time.Time{}, err
	}
	return true, planID, expiresAt, nil
}

func (t *adminUsersTx) GetIdempotencyResponse(ctx context.Context, appID, key string) (json.RawMessage, error) {
	return getIdempotencyResponse(ctx, t.tx, appID, key)
}

func (t *adminUsersTx) InsertIdempotencyKey(ctx context.Context, appID, key, action, target string, response json.RawMessage) (bool, error) {
	res, err := t.tx.ExecContext(ctx, `
		INSERT INTO admin_idempotency_keys (app_id, key, action, target, response)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (app_id, key) DO NOTHING
	`, appID, key, action, target, response)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (t *adminUsersTx) InsertAudit(ctx context.Context, actor, action, target string, ctxData map[string]any) error {
	data, err := json.Marshal(ctxData)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target, context)
		VALUES ($1, $2, $3, $4::jsonb)
	`, actor, action, target, data)
	return err
}
