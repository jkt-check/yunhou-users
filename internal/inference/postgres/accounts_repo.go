package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// accounts_repo.go — 表组 inference_credentials/upstream_accounts/
// billing_accounts/api_keys (migration 025).
//
// 账本边界：billing_accounts 引用 users(id) 但不级联删除；客户删除走
// 去标识化策略（后续任务），这里保留行。

// InsertBillingAccount creates the per-user billing account
// (UNIQUE(user_id); 重复建立撞唯一键 → CodeConflict).
func (s *Store) InsertBillingAccount(ctx context.Context, a *domain.BillingAccount) error {
	subject := a.SubjectType
	if subject == "" {
		subject = "user"
	}
	status := a.Status
	if status == "" {
		status = "active"
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_billing_accounts (user_id, subject_type, status)
		 VALUES ($1,$2,$3) RETURNING id, created_at, updated_at`,
		a.UserID, subject, status).
		Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	return mapError("insert billing account", err)
}

// GetBillingAccountByUser loads the account owned by a user.
func (s *Store) GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error) {
	var row struct {
		ID          string    `db:"id"`
		UserID      string    `db:"user_id"`
		SubjectType string    `db:"subject_type"`
		Status      string    `db:"status"`
		CreatedAt   time.Time `db:"created_at"`
		UpdatedAt   time.Time `db:"updated_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_billing_accounts WHERE user_id = $1`, userID)
	if err != nil {
		return nil, mapError("get billing account", err)
	}
	return &domain.BillingAccount{
		ID: row.ID, UserID: row.UserID, SubjectType: row.SubjectType,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// InsertAPIKey stores a key's lookup prefix + verification digest. The
// plaintext is never stored (设计 §9.2: 新 Key 仅创建时展示明文).
func (s *Store) InsertAPIKey(ctx context.Context, k *domain.APIKey, keyHash string) error {
	status := string(k.Status)
	if status == "" {
		status = string(domain.APIKeyActive)
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_api_keys
		 (billing_account_id, name, key_prefix, key_hash, model_allow,
		  budget_limit_micros, budget_used_micros, rpm_limit, concurrency_limit,
		  status, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at, updated_at`,
		k.BillingAccountID, k.Name, k.Prefix, keyHash, pq.Array(k.ModelAllow),
		microPtr(k.BudgetLimit), int64(k.BudgetUsed), intPtr(k.RPMLimit), intPtr(k.ConcurrencyLimit),
		status, k.ExpiresAt).
		Scan(&k.ID, &k.CreatedAt, &k.UpdatedAt)
	return mapError("insert api key", err)
}

// GetAPIKeyByPrefix is the lookup path for authentication. It returns the
// digest for the access layer to verify — never the plaintext.
func (s *Store) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, string, error) {
	var row struct {
		ID          string         `db:"id"`
		AccountID   string         `db:"billing_account_id"`
		Name        string         `db:"name"`
		Prefix      string         `db:"key_prefix"`
		KeyHash     string         `db:"key_hash"`
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
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_api_keys WHERE key_prefix = $1`, prefix)
	if err != nil {
		return nil, "", mapError("get api key", err)
	}
	return &domain.APIKey{
		ID: row.ID, BillingAccountID: row.AccountID, Name: row.Name, Prefix: row.Prefix,
		ModelAllow:       []string(row.ModelAllow),
		BudgetLimit:      microFromNull(row.BudgetLimit),
		BudgetUsed:       domain.Microcredit(row.BudgetUsed),
		RPMLimit:         intFromNull(row.RPMLimit),
		ConcurrencyLimit: intFromNull(row.ConcLimit),
		Status:           domain.APIKeyStatus(row.Status),
		ExpiresAt:        row.ExpiresAt, RevokedAt: row.RevokedAt, LastUsedAt: row.LastUsedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, row.KeyHash, nil
}

// InsertCredential stores AEAD ciphertext with its key version. Callers
// must encrypt BEFORE insert; this method never sees a usable secret
// contract beyond bytes it cannot interpret (Task 4 owns encryption).
func (s *Store) InsertCredential(ctx context.Context, c *domain.Credential) error {
	status := c.Status
	if status == "" {
		status = "active"
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_credentials
		 (provider_id, label, auth_type, ciphertext, key_version, generation, expires_at, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING id, created_at, updated_at`,
		c.ProviderID, c.Label, c.AuthType, c.Ciphertext, c.KeyVersion, c.Generation, c.ExpiresAt, status).
		Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	return mapError("insert credential", err)
}

// GetCredential loads a credential reference (ciphertext included for the
// credentials package's decrypt path; HTTP layers must never serialize it).
func (s *Store) GetCredential(ctx context.Context, id string) (*domain.Credential, error) {
	var row struct {
		ID         string     `db:"id"`
		ProviderID string     `db:"provider_id"`
		Label      string     `db:"label"`
		AuthType   string     `db:"auth_type"`
		Ciphertext []byte     `db:"ciphertext"`
		KeyVersion int        `db:"key_version"`
		Generation int64      `db:"generation"`
		ExpiresAt  *time.Time `db:"expires_at"`
		Status     string     `db:"status"`
		CreatedAt  time.Time  `db:"created_at"`
		UpdatedAt  time.Time  `db:"updated_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_credentials WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get credential", err)
	}
	return &domain.Credential{
		ID: row.ID, ProviderID: row.ProviderID, Label: row.Label, AuthType: row.AuthType,
		Ciphertext: row.Ciphertext, KeyVersion: row.KeyVersion, Generation: row.Generation,
		ExpiresAt: row.ExpiresAt, Status: row.Status,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// InsertUpstreamAccount registers one schedulable upstream account.
func (s *Store) InsertUpstreamAccount(ctx context.Context, a *domain.UpstreamAccount) error {
	status := string(a.Status)
	if status == "" {
		status = string(domain.AccountActive)
	}
	conc := a.ConcurrencyLimit
	if conc == 0 {
		conc = 1
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_upstream_accounts
		 (provider_id, credential_id, external_account_id, display_name, status, concurrency_limit,
		  quota_limit_micros, quota_remaining_micros, quota_observed_at, quota_source, quota_reset_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at, updated_at`,
		a.ProviderID, a.CredentialID, a.ExternalAccountID, a.DisplayName, status, conc,
		microPtr(a.Quota.LimitMicros), microPtr(a.Quota.RemainingMicros),
		a.Quota.ObservedAt, strPtr(a.Quota.Source), a.Quota.ResetAt).
		Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	return mapError("insert upstream account", err)
}

// GetUpstreamAccount loads one account with its quota cache.
func (s *Store) GetUpstreamAccount(ctx context.Context, id string) (*domain.UpstreamAccount, error) {
	var row struct {
		ID            string         `db:"id"`
		ProviderID    string         `db:"provider_id"`
		CredentialID  string         `db:"credential_id"`
		ExternalID    string         `db:"external_account_id"`
		DisplayName   string         `db:"display_name"`
		Status        string         `db:"status"`
		ConcLimit     int            `db:"concurrency_limit"`
		QuotaLimit    sql.NullInt64  `db:"quota_limit_micros"`
		QuotaRemain   sql.NullInt64  `db:"quota_remaining_micros"`
		QuotaObserved *time.Time     `db:"quota_observed_at"`
		QuotaSource   sql.NullString `db:"quota_source"`
		QuotaReset    *time.Time     `db:"quota_reset_at"`
		CreatedAt     time.Time      `db:"created_at"`
		UpdatedAt     time.Time      `db:"updated_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_upstream_accounts WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get upstream account", err)
	}
	return &domain.UpstreamAccount{
		ID: row.ID, ProviderID: row.ProviderID, CredentialID: row.CredentialID,
		ExternalAccountID: row.ExternalID, DisplayName: row.DisplayName,
		Status: domain.UpstreamAccountStatus(row.Status), ConcurrencyLimit: row.ConcLimit,
		Quota: domain.UpstreamQuota{
			LimitMicros:     microFromNull(row.QuotaLimit),
			RemainingMicros: microFromNull(row.QuotaRemain),
			ObservedAt:      row.QuotaObserved,
			Source:          strFromNull(row.QuotaSource),
			ResetAt:         row.QuotaReset,
		},
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// --- nullable helpers -------------------------------------------------------

func microPtr(m *domain.Microcredit) interface{} {
	if m == nil {
		return nil
	}
	return int64(*m)
}

func microFromNull(n sql.NullInt64) *domain.Microcredit {
	if !n.Valid {
		return nil
	}
	m := domain.Microcredit(n.Int64)
	return &m
}

func intPtr(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func intFromNull(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func strPtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

func strFromNull(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
