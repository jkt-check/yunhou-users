package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// ============================================================================
// OrderRepo — pre-payment intent (design doc §"Order")
// ============================================================================

type OrderRepo interface {
	Create(ctx context.Context, o *model.Order) error
	FindByID(ctx context.Context, id string) (*model.Order, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Order, error)
	CancelPending(ctx context.Context, id, userID string) (bool, error)
	// SweepExpired flips pending orders past expires_at to 'expired'.
	// Returns number of rows flipped (for sweeper observability).
	// Sweeper interval must be < expiry window (design doc §"v1 decisions").
	SweepExpired(ctx context.Context, now time.Time) (int64, error)
}

type orderRepo struct{ db *sqlx.DB }

func NewOrderRepo(db *sqlx.DB) *orderRepo { return &orderRepo{db: db} }

func (r *orderRepo) Create(ctx context.Context, o *model.Order) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES (:id, :user_id, :plan_id, :amount, :currency, :status, :expires_at)
	`, o)
	return err
}

func (r *orderRepo) FindByID(ctx context.Context, id string) (*model.Order, error) {
	var o model.Order
	err := r.db.GetContext(ctx, &o, `SELECT * FROM orders WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *orderRepo) ListByUserID(ctx context.Context, userID string) ([]model.Order, error) {
	var list []model.Order
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC
	`, userID)
	return list, err
}

// CancelPending atomically transitions pending → cancelled. Returns false if
// the order doesn't exist, isn't owned by userID, or isn't pending (already
// paid / failed / refunded / expired). The WHERE clause is the guard.
func (r *orderRepo) CancelPending(ctx context.Context, id, userID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE orders SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'pending'
	`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *orderRepo) SweepExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE orders SET status = 'expired', updated_at = now()
		WHERE status = 'pending' AND expires_at < $1
	`, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ============================================================================
// PaymentRepo — channel-side transaction (design doc §"Payment", webhook doc §5.2)
// ============================================================================

type PaymentRepo interface {
	// InsertPaidOnConflictDoNothing is the business-level idempotency key
	// for webhook + confirm flows. UNIQUE (channel, external_txn_id) absorbs
	// duplicates. Returns (id, true) if inserted, (uuid.Zero, false) if
	// a row already exists for the same (channel, external_txn_id).
	InsertPaidOnConflictDoNothing(ctx context.Context, p *model.Payment) (string, bool, error)
	FindByID(ctx context.Context, id string) (*model.Payment, error)
	FindByChannelTxnID(ctx context.Context, channel, externalTxnID string) (*model.Payment, error)
	FindPaidByOrderID(ctx context.Context, orderID string) (*model.Payment, error) // channel mismatch pre-check (design doc confirm endpoint)
	ListByOrderID(ctx context.Context, orderID string) ([]model.Payment, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Payment, error) // GET /payments — joins via orders
	MarkPaid(ctx context.Context, id string, paidAt time.Time) error
	MarkFailed(ctx context.Context, id, reason string) error
	MarkRefunded(ctx context.Context, id string) error
	SetDisputed(ctx context.Context, id string, disputedAt time.Time) error
	ClearDisputed(ctx context.Context, id string) error
}

type paymentRepo struct{ db *sqlx.DB }

func NewPaymentRepo(db *sqlx.DB) *paymentRepo { return &paymentRepo{db: db} }

func (r *paymentRepo) InsertPaidOnConflictDoNothing(ctx context.Context, p *model.Payment) (string, bool, error) {
	var id string
	err := r.db.QueryRowxContext(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES (:order_id, :channel, :external_txn_id, :amount, :currency, 'paid', now(), :raw_payload)
		ON CONFLICT (channel, external_txn_id) DO NOTHING
		RETURNING id
	`, p).Scan(&id)
	if err != nil {
		// sql.ErrNoRows means the ON CONFLICT absorbed a duplicate. The
		// payment row already exists; caller should re-read it.
		if err.Error() == "sql: no rows in result set" {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

func (r *paymentRepo) FindByID(ctx context.Context, id string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `SELECT * FROM payments WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) FindByChannelTxnID(ctx context.Context, channel, externalTxnID string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2
	`, channel, externalTxnID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) FindPaidByOrderID(ctx context.Context, orderID string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `
		SELECT * FROM payments WHERE order_id = $1 AND status = 'paid' LIMIT 1
	`, orderID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) ListByOrderID(ctx context.Context, orderID string) ([]model.Payment, error) {
	var list []model.Payment
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM payments WHERE order_id = $1 ORDER BY created_at
	`, orderID)
	return list, err
}

// ListByUserID joins via orders.user_id for ownership-scoped listing (GET /payments).
func (r *paymentRepo) ListByUserID(ctx context.Context, userID string) ([]model.Payment, error) {
	var list []model.Payment
	err := r.db.SelectContext(ctx, &list, `
		SELECT p.* FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE o.user_id = $1
		ORDER BY p.created_at DESC
	`, userID)
	return list, err
}

func (r *paymentRepo) MarkPaid(ctx context.Context, id string, paidAt time.Time) error {
	// Guard: only allow pending → paid at the row level (terminal states
	// won't match the WHERE; webhook doc §5.6 handles the no-op).
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'paid', paid_at = $1, updated_at = now()
		WHERE id = $2 AND status = 'pending'
	`, paidAt, id)
	return err
}

func (r *paymentRepo) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'failed', failed_reason = $1, updated_at = now()
		WHERE id = $2 AND status IN ('pending', 'paid')
	`, reason, id)
	return err
}

func (r *paymentRepo) MarkRefunded(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'refunded', updated_at = now()
		WHERE id = $1 AND status = 'paid'
	`, id)
	return err
}

func (r *paymentRepo) SetDisputed(ctx context.Context, id string, disputedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET disputed = true, disputed_at = $1, updated_at = now()
		WHERE id = $2
	`, disputedAt, id)
	return err
}

func (r *paymentRepo) ClearDisputed(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET disputed = false, disputed_at = NULL, updated_at = now()
		WHERE id = $1
	`, id)
	return err
}

// ============================================================================
// RefundRepo (design doc §"Refund", POST /refunds contract)
// ============================================================================

type RefundRepo interface {
	// FindByIdempotencyKey is the caller-retry gate. POST /refunds MUST
	// call this before invoking the channel refund API; if a row exists,
	// return it without calling the channel.
	FindByIdempotencyKey(ctx context.Context, key string) (*model.Refund, error)
	InsertPending(ctx context.Context, r *model.Refund) error
	FindByID(ctx context.Context, id string) (*model.Refund, error)
	// FindByChannelRefundID matches a webhook refund event to its row.
	FindByChannelRefundID(ctx context.Context, channel, externalRefundID string) (*model.Refund, error)
	ListByPaymentID(ctx context.Context, paymentID string) ([]model.Refund, error)
	// SumByPaymentID returns the current refund total for an amount invariant check.
	SumByPaymentID(ctx context.Context, paymentID string) (float64, error)
	MarkPaid(ctx context.Context, id string) error
}

type refundRepo struct{ db *sqlx.DB }

func NewRefundRepo(db *sqlx.DB) *refundRepo { return &refundRepo{db: db} }

func (r *refundRepo) FindByIdempotencyKey(ctx context.Context, key string) (*model.Refund, error) {
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `SELECT * FROM refunds WHERE idempotency_key = $1`, key)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) InsertPending(ctx context.Context, ref *model.Refund) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO refunds (id, payment_id, channel, amount, reason, idempotency_key, external_refund_id, status)
		VALUES (:id, :payment_id, :channel, :amount, :reason, :idempotency_key, :external_refund_id, :status)
	`, ref)
	return err
}

func (r *refundRepo) FindByID(ctx context.Context, id string) (*model.Refund, error) {
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `SELECT * FROM refunds WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) FindByChannelRefundID(ctx context.Context, channel, externalRefundID string) (*model.Refund, error) {
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `
		SELECT * FROM refunds WHERE channel = $1 AND external_refund_id = $2
	`, channel, externalRefundID)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) ListByPaymentID(ctx context.Context, paymentID string) ([]model.Refund, error) {
	var list []model.Refund
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM refunds WHERE payment_id = $1 ORDER BY created_at
	`, paymentID)
	return list, err
}

func (r *refundRepo) SumByPaymentID(ctx context.Context, paymentID string) (float64, error) {
	var sum float64
	err := r.db.GetContext(ctx, &sum, `
		SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1
	`, paymentID)
	return sum, err
}

func (r *refundRepo) MarkPaid(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE refunds SET status = 'paid', updated_at = now()
		WHERE id = $1 AND status = 'pending'
	`, id)
	return err
}

// ============================================================================
// WebhookEventRepo (webhook doc §5.1 — event-level dedup)
// ============================================================================

type WebhookEventRepo interface {
	// InsertOnConflictDoNothing is the event-level gate. If a row already
	// exists for (channel, event_id), returns (uuid.Zero, false) — the
	// handler should ack 200 and stop.
	InsertOnConflictDoNothing(ctx context.Context, e *model.WebhookEvent) (string, bool, error)
	MarkProcessed(ctx context.Context, id string) error
}

type webhookEventRepo struct{ db *sqlx.DB }

func NewWebhookEventRepo(db *sqlx.DB) *webhookEventRepo { return &webhookEventRepo{db: db} }

func (r *webhookEventRepo) InsertOnConflictDoNothing(ctx context.Context, e *model.WebhookEvent) (string, bool, error) {
	var id string
	err := r.db.QueryRowxContext(ctx, `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload)
		VALUES (:channel, :event_id, :event_type, :raw_payload)
		ON CONFLICT (channel, event_id) DO NOTHING
		RETURNING id
	`, e).Scan(&id)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

func (r *webhookEventRepo) MarkProcessed(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE webhook_events SET processed_at = now() WHERE id = $1
	`, id)
	return err
}

// ============================================================================
// AuditLogRepo (design doc §AuditLog)
// ============================================================================

type AuditLogRepo interface {
	Insert(ctx context.Context, a *model.AuditLog) error
}

type auditLogRepo struct{ db *sqlx.DB }

func NewAuditLogRepo(db *sqlx.DB) *auditLogRepo { return &auditLogRepo{db: db} }

func (r *auditLogRepo) Insert(ctx context.Context, a *model.AuditLog) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target, tags, context)
		VALUES (:actor, :action, :target, :tags, :context)
	`, a)
	return err
}

// Compile-time interface conformance assertions.
var (
	_ OrderRepo      = (*orderRepo)(nil)
	_ PaymentRepo    = (*paymentRepo)(nil)
	_ RefundRepo     = (*refundRepo)(nil)
	_ WebhookEventRepo = (*webhookEventRepo)(nil)
	_ AuditLogRepo   = (*auditLogRepo)(nil)
)