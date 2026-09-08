package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

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
		     effective_to = COALESCE($4, effective_to)
		 WHERE id = $1 AND revision = $5 AND status = 'active'
		 RETURNING *`,
		id, patch.PolicyVersionID, patchStrArr(patch.ModelIDs), patch.EffectiveTo,
		patch.ExpectedRevision)
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
