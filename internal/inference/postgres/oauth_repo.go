package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// oauth_repo.go — Task 12 表组 inference_oauth_grants/session_bindings
// (migration 032) + OAuth 凭据轮换 CAS/分布式刷新锁/账号状态与额度快照写面。
//
// 口径:
//   - state 一次性消费用单语句 UPDATE ... WHERE consumed_at IS NULL 原子翻转，
//     并发回调只有一个赢家（另一个拿 CodeConflict）。
//   - 轮换写库是 generation CAS：UPDATE ... WHERE generation=$expected，
//     0 行 = 已有更新的代次提交 → CodeConflict（旧 token 晚返回不得覆盖新
//     token，设计 §8）。
//   - 跨实例刷新互斥用 pg_advisory_xact_lock：随事务提交/回滚自动释放，
//     崩溃不留死锁（会话级 advisory lock 需要显式释放，这里刻意不用）。
//   - 额度快照写带 observed_at 单调守卫：更旧的观测不得覆盖更新的观测。

// InsertOAuthGrant stores a pending authorization state.
func (s *Store) InsertOAuthGrant(ctx context.Context, g *domain.OAuthGrant) error {
	return insertOAuthGrant(ctx, s.db, g)
}

// InsertOAuthGrantTx is InsertOAuthGrant inside an open UnitOfWork.
func (s *Store) InsertOAuthGrantTx(ctx context.Context, w domain.UnitOfWork, g *domain.OAuthGrant) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return insertOAuthGrant(ctx, tx, g)
}

func insertOAuthGrant(ctx context.Context, ex sqlxExecutor, g *domain.OAuthGrant) error {
	err := ex.QueryRowxContext(ctx,
		`INSERT INTO inference_oauth_grants
		 (state, connector, provider_id, account_label, code_verifier,
		  operator_user_id, operator_app_id, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING id, created_at`,
		g.State, g.Connector, g.ProviderID, g.AccountLabel, g.CodeVerifier,
		g.OperatorUserID, g.OperatorAppID, g.ExpiresAt).
		Scan(&g.ID, &g.CreatedAt)
	return mapError("insert oauth grant", err)
}

// ConsumeOAuthGrant atomically flips one pending state to consumed. A state
// that is unknown, already consumed, or expired at `now` yields CodeConflict
// (one-time callback validation, 设计 §8). The returned row includes the
// PKCE verifier — it stays inside the credentials boundary.
func (s *Store) ConsumeOAuthGrant(ctx context.Context, state string, now time.Time) (*domain.OAuthGrant, error) {
	var g domain.OAuthGrant
	err := s.db.QueryRowxContext(ctx,
		`UPDATE inference_oauth_grants
		    SET consumed_at = now()
		  WHERE state = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING id, state, connector, provider_id, account_label, code_verifier,
		          operator_user_id, operator_app_id, expires_at, created_at`,
		state, now).
		Scan(&g.ID, &g.State, &g.Connector, &g.ProviderID, &g.AccountLabel, &g.CodeVerifier,
			&g.OperatorUserID, &g.OperatorAppID, &g.ExpiresAt, &g.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, domain.NewError(domain.CodeConflict, "oauth state is unknown, consumed or expired")
		}
		return nil, mapError("consume oauth grant", err)
	}
	return &g, nil
}

// RotateCredentialSecretCAS is the refresh-rotation write (设计 §8: 旧
// refresh token 不得覆盖新值): the new ciphertext/expires_at commit only
// when the stored generation still equals expectedGeneration. A mismatch
// means another instance rotated first — CodeConflict, and the caller
// re-reads the current row instead of overwriting it.
func (s *Store) RotateCredentialSecretCAS(ctx context.Context, w domain.UnitOfWork, id string, ciphertext []byte, keyVersion int, expectedGeneration int64, expiresAt *time.Time) (int64, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return 0, err
	}
	var newGen int64
	err = tx.QueryRowxContext(ctx,
		`UPDATE inference_credentials
		    SET ciphertext = $2, key_version = $3, generation = generation + 1,
		        expires_at = $4, last_rotated_at = now(), updated_at = now()
		  WHERE id = $1 AND generation = $5
		RETURNING generation`,
		id, ciphertext, keyVersion, expiresAt, expectedGeneration).
		Scan(&newGen)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, domain.NewError(domain.CodeConflict, "credential generation moved during refresh")
		}
		return 0, mapError("rotate credential (cas)", err)
	}
	return newGen, nil
}

// AcquireCredentialRefreshLockTx takes the cross-instance refresh mutex for
// one credential inside the caller's transaction (pg_advisory_xact_lock:
// auto-released at commit/rollback, crash-safe). It blocks until the lock
// is available — two instances refreshing the same credential serialize
// here.
func (s *Store) AcquireCredentialRefreshLockTx(ctx context.Context, w domain.UnitOfWork, credentialID string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"inference-credential-refresh:"+credentialID); err != nil {
		return mapError("acquire credential refresh lock", err)
	}
	return nil
}

// GetCredentialTx re-reads a credential inside the refresh-lock transaction
// (post-lock authoritative read for the generation CAS).
func (s *Store) GetCredentialTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Credential, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var row struct {
		ID            string     `db:"id"`
		ProviderID    string     `db:"provider_id"`
		Label         string     `db:"label"`
		AuthType      string     `db:"auth_type"`
		Ciphertext    []byte     `db:"ciphertext"`
		KeyVersion    int        `db:"key_version"`
		Generation    int64      `db:"generation"`
		ExpiresAt     *time.Time `db:"expires_at"`
		LastRotatedAt *time.Time `db:"last_rotated_at"`
		Status        string     `db:"status"`
		Connector     string     `db:"connector"`
		CreatedAt     time.Time  `db:"created_at"`
		UpdatedAt     time.Time  `db:"updated_at"`
	}
	if err := tx.GetContext(ctx, &row,
		`SELECT * FROM inference_credentials WHERE id = $1`, id); err != nil {
		return nil, mapError("get credential (tx)", err)
	}
	return &domain.Credential{
		ID: row.ID, ProviderID: row.ProviderID, Label: row.Label, AuthType: row.AuthType,
		Ciphertext: row.Ciphertext, KeyVersion: row.KeyVersion, Generation: row.Generation,
		ExpiresAt: row.ExpiresAt, LastRotatedAt: row.LastRotatedAt, Status: row.Status,
		Connector: row.Connector, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// ListOAuthCredentialsExpiring returns active oauth credentials whose
// expires_at falls before `before` (the refresh worker's scan). Credentials
// without an expiry are not proactively refreshed.
//
// Scan-set membership additionally requires at least one SCHEDULABLE bound
// account (active/refreshing/cooldown): after a definitive vendor rejection
// the reauth propagation flips every bound account to reauth_required while
// the credential row itself stays active+expired — without this filter the
// credential would be re-scanned every pass, re-calling the vendor for a
// grant already known dead and re-writing the same audit row (Task 13 M-4).
// The EXISTS form keeps this fix DDL-free (no new credential status value).
func (s *Store) ListOAuthCredentialsExpiring(ctx context.Context, before time.Time, limit int) ([]domain.Credential, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []struct {
		ID            string     `db:"id"`
		ProviderID    string     `db:"provider_id"`
		Label         string     `db:"label"`
		AuthType      string     `db:"auth_type"`
		Ciphertext    []byte     `db:"ciphertext"`
		KeyVersion    int        `db:"key_version"`
		Generation    int64      `db:"generation"`
		ExpiresAt     *time.Time `db:"expires_at"`
		LastRotatedAt *time.Time `db:"last_rotated_at"`
		Status        string     `db:"status"`
		Connector     string     `db:"connector"`
		CreatedAt     time.Time  `db:"created_at"`
		UpdatedAt     time.Time  `db:"updated_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_credentials
		  WHERE auth_type = 'oauth' AND status = 'active'
		    AND expires_at IS NOT NULL AND expires_at < $1
		    AND EXISTS (SELECT 1 FROM inference_upstream_accounts a
		                 WHERE a.credential_id = inference_credentials.id
		                   AND a.status IN ('active', 'refreshing', 'cooldown'))
		  ORDER BY expires_at LIMIT $2`, before, limit); err != nil {
		return nil, mapError("list expiring oauth credentials", err)
	}
	out := make([]domain.Credential, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Credential{
			ID: r.ID, ProviderID: r.ProviderID, Label: r.Label, AuthType: r.AuthType,
			Ciphertext: r.Ciphertext, KeyVersion: r.KeyVersion, Generation: r.Generation,
			ExpiresAt: r.ExpiresAt, LastRotatedAt: r.LastRotatedAt, Status: r.Status,
			Connector: r.Connector, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// SetUpstreamAccountStatusConditional flips one account to `to` only when
// its current status is in `from` (乐观守卫：已被并发翻转的账号不重蹈).
// Returns whether the flip happened.
func (s *Store) SetUpstreamAccountStatusConditional(ctx context.Context, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_upstream_accounts
		    SET status = $2, updated_at = now()
		  WHERE id = $1 AND status = ANY($3)`,
		id, string(to), pq.Array(statusStrings(from)))
	if err != nil {
		return false, mapError("set upstream account status", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetUpstreamAccountStatusConditionalTx is the conditional flip inside an
// open UnitOfWork (commits together with binding termination + audit row).
func (s *Store) SetUpstreamAccountStatusConditionalTx(ctx context.Context, w domain.UnitOfWork, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_upstream_accounts
		    SET status = $2, updated_at = now()
		  WHERE id = $1 AND status = ANY($3)`,
		id, string(to), pq.Array(statusStrings(from)))
	if err != nil {
		return false, mapError("set upstream account status (tx)", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetUpstreamAccountsStatusByCredentialTx flips every account bound to a
// credential from `from` to `to` inside the caller's UnitOfWork (refresh
// reauth propagation commits with the audit row). Returns the flipped ids
// so the caller can end their session bindings in the same transaction.
func (s *Store) SetUpstreamAccountsStatusByCredentialTx(ctx context.Context, w domain.UnitOfWork, credentialID string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) ([]string, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := tx.SelectContext(ctx, &ids,
		`UPDATE inference_upstream_accounts
		    SET status = $3, updated_at = now()
		  WHERE credential_id = $1 AND status = ANY($2)
		RETURNING id`,
		credentialID, pq.Array(statusStrings(from)), string(to)); err != nil {
		return nil, mapError("set accounts status by credential", err)
	}
	return ids, nil
}

// UpdateUpstreamAccountQuota stores one observed quota snapshot. The write
// carries a monotonic observed_at guard: an older observation never
// overwrites a newer one (健康任务乱序/重试不得回拨额度视图). Passing nil
// fields keeps "unknown" unknown (设计 §8). 审查修复 M-3：ObservedAt=nil
// 的写入只允许在"尚无已知观测"时落库——已有观测的行拒绝被无时间戳的写
// 入覆盖（无观测时刻的快照不是有效观测）。评审轮1 M2：每列
// COALESCE($n, 旧值)——部分观测（只带了其中几项的快照）不得把已知字段
// 抹回 NULL（例如只带 remaining 的探测不得清空已知的 limit/耗尽视图），
// 与上面注释的"unknown 保持 unknown/已知不被无观测覆盖"口径一致。
func (s *Store) UpdateUpstreamAccountQuota(ctx context.Context, id string, q domain.UpstreamQuota) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_upstream_accounts
		    SET quota_limit_micros = COALESCE($2::bigint, quota_limit_micros),
		        quota_remaining_micros = COALESCE($3::bigint, quota_remaining_micros),
		        quota_observed_at = COALESCE($4::timestamptz, quota_observed_at),
		        quota_source = COALESCE($5::text, quota_source),
		        quota_reset_at = COALESCE($6::timestamptz, quota_reset_at),
		        updated_at = now()
		  WHERE id = $1
		    AND (($4::timestamptz IS NOT NULL
		          AND (quota_observed_at IS NULL OR quota_observed_at <= $4::timestamptz))
		         OR ($4::timestamptz IS NULL AND quota_observed_at IS NULL))`,
		id, microPtr(q.LimitMicros), microPtr(q.RemainingMicros),
		q.ObservedAt, strPtr(q.Source), q.ResetAt)
	if err != nil {
		return false, mapError("update upstream account quota", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListUpstreamAccountsForHealth returns the accounts the health worker
// probes this pass: active accounts, plus cooling accounts whose updated_at
// is at/before cooldownDueBefore (冷却到期复测), plus refreshing accounts
// (刷新中健康复核). Bounded and deterministically ordered.
func (s *Store) ListUpstreamAccountsForHealth(ctx context.Context, cooldownDueBefore time.Time, limit int) ([]domain.UpstreamAccount, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []struct {
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
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT a.* FROM inference_upstream_accounts a
		 WHERE a.status = 'active'
		    OR (a.status = 'cooldown' AND a.updated_at <= $1)
		    OR a.status = 'refreshing'
		 ORDER BY a.id LIMIT $2`, cooldownDueBefore, limit); err != nil {
		return nil, mapError("list accounts for health", err)
	}
	out := make([]domain.UpstreamAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.UpstreamAccount{
			ID: r.ID, ProviderID: r.ProviderID, CredentialID: r.CredentialID,
			ExternalAccountID: r.ExternalID, DisplayName: r.DisplayName,
			Status: domain.UpstreamAccountStatus(r.Status), ConcurrencyLimit: r.ConcLimit,
			Quota: domain.UpstreamQuota{
				LimitMicros:     microFromNull(r.QuotaLimit),
				RemainingMicros: microFromNull(r.QuotaRemain),
				ObservedAt:      r.QuotaObserved,
				Source:          strFromNull(r.QuotaSource),
				ResetAt:         r.QuotaReset,
			},
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// ListUpstreamAccounts returns accounts of one provider in any status
// (运营视图；含额度缓存，仅供管理端 — 客户读面永远看不到上游额度).
func (s *Store) ListUpstreamAccounts(ctx context.Context, providerID string, limit int) ([]domain.UpstreamAccount, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []struct {
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
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_upstream_accounts
		  WHERE provider_id = $1 ORDER BY created_at, id LIMIT $2`, providerID, limit); err != nil {
		return nil, mapError("list upstream accounts", err)
	}
	out := make([]domain.UpstreamAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.UpstreamAccount{
			ID: r.ID, ProviderID: r.ProviderID, CredentialID: r.CredentialID,
			ExternalAccountID: r.ExternalID, DisplayName: r.DisplayName,
			Status: domain.UpstreamAccountStatus(r.Status), ConcurrencyLimit: r.ConcLimit,
			Quota: domain.UpstreamQuota{
				LimitMicros:     microFromNull(r.QuotaLimit),
				RemainingMicros: microFromNull(r.QuotaRemain),
				ObservedAt:      r.QuotaObserved,
				Source:          strFromNull(r.QuotaSource),
				ResetAt:         r.QuotaReset,
			},
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// InsertUpstreamAccountTx is InsertUpstreamAccount inside an open
// UnitOfWork — the OAuth callback commits credential + account + audit
// together.
func (s *Store) InsertUpstreamAccountTx(ctx context.Context, w domain.UnitOfWork, a *domain.UpstreamAccount) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	status := string(a.Status)
	if status == "" {
		status = string(domain.AccountActive)
	}
	conc := a.ConcurrencyLimit
	if conc == 0 {
		conc = 1
	}
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_upstream_accounts
		 (provider_id, credential_id, external_account_id, display_name, status, concurrency_limit,
		  quota_limit_micros, quota_remaining_micros, quota_observed_at, quota_source, quota_reset_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at, updated_at`,
		a.ProviderID, a.CredentialID, a.ExternalAccountID, a.DisplayName, status, conc,
		microPtr(a.Quota.LimitMicros), microPtr(a.Quota.RemainingMicros),
		a.Quota.ObservedAt, strPtr(a.Quota.Source), a.Quota.ResetAt).
		Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	return mapError("insert upstream account (tx)", err)
}

// ---------------------------------------------------------------------------
// Session bindings (设计 §8 粘性会话)
// ---------------------------------------------------------------------------

// InsertSessionBinding pins (session_key, model_id) to one account. The
// partial unique index makes a second ACTIVE binding for the same pair a
// CodeConflict — callers must end/migrate explicitly (不能无条件切账号续接).
func (s *Store) InsertSessionBinding(ctx context.Context, b *domain.SessionBinding) error {
	return insertSessionBinding(ctx, s.db, b)
}

// InsertSessionBindingTx is InsertSessionBinding inside an open UnitOfWork.
func (s *Store) InsertSessionBindingTx(ctx context.Context, w domain.UnitOfWork, b *domain.SessionBinding) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return insertSessionBinding(ctx, tx, b)
}

func insertSessionBinding(ctx context.Context, ex sqlxExecutor, b *domain.SessionBinding) error {
	status := string(b.Status)
	if status == "" {
		status = string(domain.BindingActive)
	}
	err := ex.QueryRowxContext(ctx,
		`INSERT INTO inference_session_bindings
		 (session_key, model_id, account_id, status, expires_at)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING id, bound_at, last_used_at, created_at, updated_at`,
		b.SessionKey, b.ModelID, b.AccountID, status, b.ExpiresAt).
		Scan(&b.ID, &b.BoundAt, &b.LastUsedAt, &b.CreatedAt, &b.UpdatedAt)
	return mapError("insert session binding", err)
}

// GetActiveSessionBinding returns the live binding of one (session_key,
// model_id) pair at `now` (expired rows are not live). CodeNotFound when
// none exists.
func (s *Store) GetActiveSessionBinding(ctx context.Context, sessionKey, modelID string, now time.Time) (*domain.SessionBinding, error) {
	var b domain.SessionBinding
	err := s.db.QueryRowxContext(ctx,
		`SELECT id, session_key, model_id, account_id, status, ended_reason,
		        bound_at, last_used_at, expires_at, created_at, updated_at
		   FROM inference_session_bindings
		  WHERE session_key = $1 AND model_id = $2 AND status = 'active' AND expires_at > $3`,
		sessionKey, modelID, now).
		Scan(&b.ID, &b.SessionKey, &b.ModelID, &b.AccountID, &b.Status, &b.EndedReason,
			&b.BoundAt, &b.LastUsedAt, &b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, mapError("get session binding", err)
	}
	return &b, nil
}

// TouchSessionBinding refreshes last_used_at on dispatch (best-effort
// bookkeeping, not an authorization decision).
func (s *Store) TouchSessionBinding(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_session_bindings SET last_used_at = now(), updated_at = now()
		  WHERE id = $1 AND status = 'active'`, id)
	if err != nil {
		return mapError("touch session binding", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("touch session binding", sql.ErrNoRows)
	}
	return nil
}

// EndSessionBinding terminates one binding with an audit-readable reason.
// Idempotent: ending an already-ended binding reports ended=false.
func (s *Store) EndSessionBinding(ctx context.Context, id, reason string) (bool, error) {
	return endSessionBinding(ctx, s.db, id, reason)
}

// EndSessionBindingTx is EndSessionBinding inside an open UnitOfWork.
func (s *Store) EndSessionBindingTx(ctx context.Context, w domain.UnitOfWork, id, reason string) (bool, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return false, err
	}
	return endSessionBinding(ctx, tx, id, reason)
}

func endSessionBinding(ctx context.Context, ex sqlxExecutor, id, reason string) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE inference_session_bindings
		    SET status = 'ended', ended_reason = $2, updated_at = now()
		  WHERE id = $1 AND status = 'active'`,
		id, reason)
	if err != nil {
		return false, mapError("end session binding", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// EndSessionBindingsForAccountTx terminates every live binding pointing at
// one account (账号失效传播：绑定可迁移但必须先终止，停止向失效账号分配新
// 请求). Runs inside the account status flip's UnitOfWork so propagation is
// atomic. Returns the ended ids.
func (s *Store) EndSessionBindingsForAccountTx(ctx context.Context, w domain.UnitOfWork, accountID, reason string) ([]string, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := tx.SelectContext(ctx, &ids,
		`UPDATE inference_session_bindings
		    SET status = 'ended', ended_reason = $2, updated_at = now()
		  WHERE account_id = $1 AND status = 'active'
		RETURNING id`, accountID, reason); err != nil {
		return nil, mapError("end session bindings for account", err)
	}
	return ids, nil
}

// EndSessionBindingsForCredentialTx terminates every live binding pointing
// at ANY account bound to one credential — the revoke/invalidation
// propagation (审查修复 I-2：吊销与 invalid_grant 走同一绑定终止语义，不
// 留悬垂 active 绑定阻塞同 (session_key, model_id) 的重新绑定). Runs
// inside the caller's UnitOfWork. Returns the number ended.
func (s *Store) EndSessionBindingsForCredentialTx(ctx context.Context, w domain.UnitOfWork, credentialID, reason string) (int64, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_session_bindings
		    SET status = 'ended', ended_reason = $2, updated_at = now()
		  WHERE status = 'active' AND account_id IN (
		    SELECT id FROM inference_upstream_accounts WHERE credential_id = $1)`,
		credentialID, reason)
	if err != nil {
		return 0, mapError("end session bindings for credential", err)
	}
	return res.RowsAffected()
}

// EndSessionBindingsForCredential is the non-transactional variant used by
// the sequential fallback path in credentials.Service (plain test fakes).
func (s *Store) EndSessionBindingsForCredential(ctx context.Context, credentialID, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_session_bindings
		    SET status = 'ended', ended_reason = $2, updated_at = now()
		  WHERE status = 'active' AND account_id IN (
		    SELECT id FROM inference_upstream_accounts WHERE credential_id = $1)`,
		credentialID, reason)
	if err != nil {
		return 0, mapError("end session bindings for credential", err)
	}
	return res.RowsAffected()
}

// EndExpiredSessionBindings sweeps live bindings whose expires_at passed.
// Returns the number ended (worker metrics).
func (s *Store) EndExpiredSessionBindings(ctx context.Context, now time.Time, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_session_bindings
		    SET status = 'ended', ended_reason = 'expired', updated_at = now()
		  WHERE id IN (
		    SELECT id FROM inference_session_bindings
		     WHERE status = 'active' AND expires_at <= $1
		     ORDER BY expires_at LIMIT $2)`,
		now, limit)
	if err != nil {
		return 0, mapError("end expired session bindings", err)
	}
	return res.RowsAffected()
}

func statusStrings(ss []domain.UpstreamAccountStatus) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}
