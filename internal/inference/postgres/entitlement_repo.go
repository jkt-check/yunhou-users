package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// entitlement_repo.go — inference_entitlements (migration 026)。
// 显式模型集合 + 来源幂等键 UNIQUE(source_type, source_id, revision)。

type entitlementRow struct {
	ID         string         `db:"id"`
	AccountID  string         `db:"billing_account_id"`
	SourceType string         `db:"source_type"`
	SourceID   string         `db:"source_id"`
	ModelIDs   pq.StringArray `db:"model_ids"`
	PolicyID   string         `db:"policy_version_id"`
	AnchorAt   time.Time      `db:"anchor_at"`
	From       time.Time      `db:"effective_from"`
	To         *time.Time     `db:"effective_to"`
	Revision   int            `db:"revision"`
	Stackable  bool           `db:"stackable"`
	Status     string         `db:"status"`
	CreatedAt  time.Time      `db:"created_at"`
	UpdatedAt  time.Time      `db:"updated_at"`
}

func (r entitlementRow) toDomain() *domain.Entitlement {
	return &domain.Entitlement{
		ID: r.ID, BillingAccountID: r.AccountID,
		SourceType: domain.EntitlementSource(r.SourceType), SourceID: r.SourceID,
		ModelIDs: []string(r.ModelIDs), PolicyVersionID: r.PolicyID,
		AnchorAt: r.AnchorAt, EffectiveFrom: r.From, EffectiveTo: r.To,
		Revision: r.Revision, Stackable: r.Stackable,
		Status:    domain.EntitlementStatus(r.Status),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// InsertEntitlement creates an entitlement. Duplicate grants from the same
// source+revision hit the idempotency UNIQUE key (CodeConflict).
func (s *Store) InsertEntitlement(ctx context.Context, e *domain.Entitlement) error {
	status := string(e.Status)
	if status == "" {
		status = string(domain.EntitlementActive)
	}
	if e.Revision == 0 {
		e.Revision = 1
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_entitlements
		 (billing_account_id, source_type, source_id, model_ids, policy_version_id,
		  anchor_at, effective_from, effective_to, revision, stackable, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at, updated_at`,
		e.BillingAccountID, string(e.SourceType), e.SourceID, strArr(e.ModelIDs),
		e.PolicyVersionID, e.AnchorAt, e.EffectiveFrom, e.EffectiveTo,
		e.Revision, e.Stackable, status).
		Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return mapError("insert entitlement", err)
}

// InsertEntitlementTx is InsertEntitlement inside the caller's UnitOfWork —
// the entitlement-sync worker commits the grant and the outbox delivery
// mark in one transaction (Task 10).
func (s *Store) InsertEntitlementTx(ctx context.Context, w domain.UnitOfWork, e *domain.Entitlement) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	status := string(e.Status)
	if status == "" {
		status = string(domain.EntitlementActive)
	}
	if e.Revision == 0 {
		e.Revision = 1
	}
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_entitlements
		 (billing_account_id, source_type, source_id, model_ids, policy_version_id,
		  anchor_at, effective_from, effective_to, revision, stackable, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at, updated_at`,
		e.BillingAccountID, string(e.SourceType), e.SourceID, strArr(e.ModelIDs),
		e.PolicyVersionID, e.AnchorAt, e.EffectiveFrom, e.EffectiveTo,
		e.Revision, e.Stackable, status).
		Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return mapError("insert entitlement", err)
}

// GetLatestEntitlementBySource returns the highest-revision entitlement row
// for one source (any status). The sync path needs the retired rows too —
// a re-purchase after refund revives the same source in place.
// sql.ErrNoRows (→ CodeNotFound) when the source never granted.
func (s *Store) GetLatestEntitlementBySource(ctx context.Context, sourceType domain.EntitlementSource, sourceID string) (*domain.Entitlement, error) {
	var row entitlementRow
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_entitlements
		 WHERE source_type = $1 AND source_id = $2
		 ORDER BY revision DESC, created_at DESC
		 LIMIT 1`, string(sourceType), sourceID)
	if err != nil {
		return nil, mapError("get entitlement by source", err)
	}
	return row.toDomain(), nil
}

// getLatestEntitlementBySourceTx is GetLatestEntitlementBySource on the
// caller's transaction (评审轮1 M3：持 tx 期间不得用 s.db 第二连接读——池
// 饱和时第二连接等不到可用连接即死锁).
func (s *Store) getLatestEntitlementBySourceTx(ctx context.Context, tx *sqlx.Tx, sourceType domain.EntitlementSource, sourceID string) (*domain.Entitlement, error) {
	var row entitlementRow
	err := tx.GetContext(ctx, &row,
		`SELECT * FROM inference_entitlements
		 WHERE source_type = $1 AND source_id = $2
		 ORDER BY revision DESC, created_at DESC
		 LIMIT 1`, string(sourceType), sourceID)
	if err != nil {
		return nil, mapError("get entitlement by source", err)
	}
	return row.toDomain(), nil
}

// ReviseEntitlementTx is ReviseEntitlement inside the caller's UnitOfWork.
func (s *Store) ReviseEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var row entitlementRow
	err = tx.GetContext(ctx, &row,
		`UPDATE inference_entitlements
		 SET revision = revision + 1, updated_at = now(),
		     policy_version_id = COALESCE($2, policy_version_id),
		     model_ids = COALESCE($3::text[], model_ids),
		     effective_to = CASE WHEN $6 THEN NULL ELSE COALESCE($4, effective_to) END
		 WHERE id = $1 AND revision = $5 AND status = 'active'
		 RETURNING *`,
		id, patch.PolicyVersionID, patchStrArr(patch.ModelIDs), patch.EffectiveTo,
		patch.ExpectedRevision, patch.ClearEffectiveTo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NewError(domain.CodeConflict,
				"revise entitlement: stale revision or entitlement not active")
		}
		return nil, mapError("revise entitlement", err)
	}
	return row.toDomain(), nil
}

// ReviveEntitlementTx returns a retired (revoked/expired) entitlement to
// active while applying the patch — the re-purchase-after-refund path.
// Same optimistic guard as ReviseEntitlement; the anchor and the
// consumption subject (ID) never change, so quota history stays attached.
func (s *Store) ReviveEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var row entitlementRow
	err = tx.GetContext(ctx, &row,
		`UPDATE inference_entitlements
		 SET revision = revision + 1, updated_at = now(), status = 'active',
		     policy_version_id = COALESCE($2, policy_version_id),
		     model_ids = COALESCE($3::text[], model_ids),
		     effective_to = CASE WHEN $6 THEN NULL ELSE COALESCE($4, effective_to) END
		 WHERE id = $1 AND revision = $5 AND status IN ('revoked', 'expired')
		 RETURNING *`,
		id, patch.PolicyVersionID, patchStrArr(patch.ModelIDs), patch.EffectiveTo,
		patch.ExpectedRevision, patch.ClearEffectiveTo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NewError(domain.CodeConflict,
				"revive entitlement: stale revision or entitlement not retired")
		}
		return nil, mapError("revive entitlement", err)
	}
	return row.toDomain(), nil
}

// RetireEntitlementTx flips an active entitlement to a terminal status
// (revoked on cancel/refund/lost payment evidence, expired on natural
// lapse) with the optimistic revision guard. 已消费账本与窗口不动 —— 状态
// 翻转只切断后续调用的授权来源（SelectEntitlement 只认 active）。
func (s *Store) RetireEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, expectedRevision int, to domain.EntitlementStatus) error {
	switch to {
	case domain.EntitlementRevoked, domain.EntitlementExpired, domain.EntitlementSuperseded:
	default:
		return domain.NewError(domain.CodeInvalidInput, "retire entitlement: invalid target status "+string(to))
	}
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_entitlements
		 SET status = $2, revision = revision + 1, updated_at = now()
		 WHERE id = $1 AND revision = $3 AND status = 'active'`,
		id, string(to), expectedRevision)
	if err != nil {
		return mapError("retire entitlement", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.NewError(domain.CodeConflict,
			"retire entitlement: stale revision or entitlement not active")
	}
	return nil
}

// MarkExpiredEntitlements flips active entitlements whose effective_to has
// passed to status='expired' (natural lapse hygiene — authorization is
// already cut off by the effective_to bound in ListActiveEntitlements /
// SelectEntitlement; this makes the stored status honest). Idempotent.
func (s *Store) MarkExpiredEntitlements(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_entitlements
		 SET status = 'expired', revision = revision + 1, updated_at = now()
		 WHERE status = 'active' AND effective_to IS NOT NULL AND effective_to <= $1`,
		now.UTC())
	if err != nil {
		return 0, mapError("mark expired entitlements", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReviseEntitlement applies an in-place revision computed by the pure rules
// (access.Upgrade / access.Renew). The entitlement ID — the consumption
// subject the quota windows key on — and the original anchor NEVER change,
// so accumulated used/reserved carry over (设计 §4.2/§6: 更新限额不清空
// used/reserved；续费不提前重置窗口). Optimistic on ExpectedRevision: a
// stale writer (or a non-active entitlement) conflicts.
func (s *Store) ReviseEntitlement(ctx context.Context, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error) {
	var row entitlementRow
	err := s.db.GetContext(ctx, &row,
		`UPDATE inference_entitlements
		 SET revision = revision + 1, updated_at = now(),
		     policy_version_id = COALESCE($2, policy_version_id),
		     model_ids = COALESCE($3::text[], model_ids),
		     effective_to = CASE WHEN $6 THEN NULL ELSE COALESCE($4, effective_to) END
		 WHERE id = $1 AND revision = $5 AND status = 'active'
		 RETURNING *`,
		id, patch.PolicyVersionID, patchStrArr(patch.ModelIDs), patch.EffectiveTo,
		patch.ExpectedRevision, patch.ClearEffectiveTo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var exists bool
			if qerr := s.db.GetContext(ctx, &exists,
				`SELECT EXISTS(SELECT 1 FROM inference_entitlements WHERE id = $1)`, id); qerr == nil && exists {
				return nil, domain.NewError(domain.CodeConflict,
					"revise entitlement: stale revision or entitlement not active")
			}
		}
		return nil, mapError("revise entitlement", err)
	}
	return row.toDomain(), nil
}

// patchStrArr encodes a patch model set: nil means "keep the current set"
// (SQL NULL → COALESCE keeps), a non-nil slice replaces it — including the
// empty slice, which grants NO models (never NULL-means-all).
func patchStrArr(s []string) interface{} {
	if s == nil {
		return nil
	}
	return pq.Array(s)
}

// GetEntitlement loads one entitlement by id.
func (s *Store) GetEntitlement(ctx context.Context, id string) (*domain.Entitlement, error) {
	var row entitlementRow
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_entitlements WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get entitlement", err)
	}
	return row.toDomain(), nil
}

// ListActiveEntitlements returns the account's active entitlements at `at`
// — explicit purchases first, gifted afterwards, matching 设计 §4.2
// 显式套餐优先.
func (s *Store) ListActiveEntitlements(ctx context.Context, billingAccountID string, at time.Time) ([]domain.Entitlement, error) {
	var rows []entitlementRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_entitlements
		 WHERE billing_account_id = $1 AND status = 'active'
		   AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
		 ORDER BY (source_type = 'subscription') DESC, created_at ASC`,
		billingAccountID, at)
	if err != nil {
		return nil, mapError("list entitlements", err)
	}
	out := make([]domain.Entitlement, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}
