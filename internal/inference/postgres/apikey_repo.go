package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// apikey_repo.go — inference_api_keys 的 Task 5 管理/状态访问面。
// Insert 与按前缀查找在 accounts_repo.go（Task 1 认证查找路径）；本文件
// 提供管理端读取/更新/撤销与状态翻转。管理面任何查询都不返回 key_hash。

// apiKeyRow is the management-side row shape (digest excluded on purpose).
type apiKeyRow struct {
	ID          string         `db:"id"`
	AccountID   string         `db:"billing_account_id"`
	Name        string         `db:"name"`
	Prefix      string         `db:"key_prefix"`
	ModelAllow  pq.StringArray `db:"model_allow"`
	BudgetLimit sql.NullInt64  `db:"budget_limit_micros"`
	BudgetUsed  int64          `db:"budget_used_micros"`
	RPMLimit    sql.NullInt64  `db:"rpm_limit"`
	ConcLimit   sql.NullInt64  `db:"concurrency_limit"`
	Status      string         `db:"status"`
	ExpiresAt   *time.Time     `db:"expires_at"`
	RevokedAt   *time.Time     `db:"revoked_at"`
	LastUsedAt  *time.Time     `db:"last_used_at"`
	CreatedAt   time.Time      `db:"created_at"`
	UpdatedAt   time.Time      `db:"updated_at"`
}

func (r apiKeyRow) toDomain() *domain.APIKey {
	return &domain.APIKey{
		ID: r.ID, BillingAccountID: r.AccountID, Name: r.Name, Prefix: r.Prefix,
		ModelAllow:       []string(r.ModelAllow),
		BudgetLimit:      microFromNull(r.BudgetLimit),
		BudgetUsed:       domain.Microcredit(r.BudgetUsed),
		RPMLimit:         intFromNull(r.RPMLimit),
		ConcurrencyLimit: intFromNull(r.ConcLimit),
		Status:           domain.APIKeyStatus(r.Status),
		ExpiresAt:        r.ExpiresAt, RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

const apiKeyColumns = `id, billing_account_id, name, key_prefix, model_allow,
	budget_limit_micros, budget_used_micros, rpm_limit, concurrency_limit,
	status, expires_at, revoked_at, last_used_at, created_at, updated_at`

// GetAPIKeyByID loads one key for the management surface. The digest
// column is never selected here.
func (s *Store) GetAPIKeyByID(ctx context.Context, id string) (*domain.APIKey, error) {
	var row apiKeyRow
	err := s.db.GetContext(ctx, &row,
		`SELECT `+apiKeyColumns+` FROM inference_api_keys WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get api key by id", err)
	}
	return row.toDomain(), nil
}

// ListAPIKeysByAccount returns one page (newest first) plus the account's
// total key count for pagination.
func (s *Store) ListAPIKeysByAccount(ctx context.Context, accountID string, limit, offset int) ([]domain.APIKey, int64, error) {
	var total int64
	if err := s.db.GetContext(ctx, &total,
		`SELECT count(*) FROM inference_api_keys WHERE billing_account_id = $1`, accountID); err != nil {
		return nil, 0, mapError("count api keys", err)
	}
	var rows []apiKeyRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT `+apiKeyColumns+` FROM inference_api_keys
		  WHERE billing_account_id = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, 0, mapError("list api keys", err)
	}
	out := make([]domain.APIKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, total, nil
}

// UpdateAPIKey persists the management-editable fields (name, model
// narrowing, budget, rpm, expiry). Status transitions go through the
// dedicated methods below, never through this write. BudgetUsed is
// maintained by the Task 7 quota path and is not writable here either.
func (s *Store) UpdateAPIKey(ctx context.Context, k *domain.APIKey) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_api_keys
		    SET name = $2, model_allow = $3, budget_limit_micros = $4,
		        rpm_limit = $5, expires_at = $6, updated_at = now()
		  WHERE id = $1`,
		k.ID, k.Name, pq.Array(k.ModelAllow), microPtr(k.BudgetLimit),
		intPtr(k.RPMLimit), k.ExpiresAt)
	if err != nil {
		return mapError("update api key", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("update api key", sql.ErrNoRows)
	}
	return nil
}

// RevokeAPIKey flips an active key to revoked, stamping revoked_at.
// Idempotent: an already-revoked/expired row reports changed=false.
func (s *Store) RevokeAPIKey(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_api_keys
		    SET status = 'revoked', revoked_at = $2, updated_at = now()
		  WHERE id = $1 AND status = 'active'`,
		id, at)
	if err != nil {
		return false, mapError("revoke api key", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkAPIKeyExpired lazily flips an active key past its expires_at to the
// expired status (the authenticate path calls this best-effort).
func (s *Store) MarkAPIKeyExpired(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inference_api_keys
		    SET status = 'expired', updated_at = now()
		  WHERE id = $1 AND status = 'active' AND expires_at IS NOT NULL AND expires_at <= now()`,
		id)
	return mapError("mark api key expired", err)
}

// TouchAPIKeyLastUsed records the last successful authentication time.
// Best-effort signal only.
func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inference_api_keys SET last_used_at = $2 WHERE id = $1`, id, at)
	return mapError("touch api key last used", err)
}
