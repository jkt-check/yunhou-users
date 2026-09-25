// accounts.go — 可调度账号（upstream account）的运营生命周期：凭据→账号
// 绑定创建、运营启停、并发/显示名调整。补的是静态 key 路径的管理端闭环
// （POST /admin/credentials 只建 credential，不建账号）；oauth 凭据的账号
// 仍只能由授权回调创建（oauth.go CompleteAuthorization），本服务显式拒绝。
//
// 口径（与需求文档固定）：
//   - 账号是运行时调度状态，不在 catalog 发布快照里：落库 active 即入路由
//     池，无需 publish；
//   - 幂等创建依赖表上 UNIQUE(provider_id, credential_id)（migration 025）：
//     重复创建返回 AccountExistsError（409 + 已存在视图），不建行；
//   - 写操作一律与审计行同事务（runAtomicAccounts + TxStore/TxRecorder，与
//     credential 生命周期同模式）；纯内存 fake 走顺序回退；
//   - 运营态只开放 active/disabled：refreshing/cooldown/reauth_required 是
//     运行时状态机的内部态，不接受外部写入；激活只允许从 disabled 恢复，
//     且绑定凭据为 revoked 时拒绝（先恢复凭据，避免账号活了但 key 解不开）；
//   - 停用账号同事务终止其活跃会话绑定（与吊销传播同一失效语义，
//     审查修复 I-2：悬垂 active 绑定会挡住同 (session_key, model_id) 重绑）。

package credentials

import (
	"context"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AccountStore is the persistence boundary AccountService needs. Satisfied
// by inference/postgres.Store; defined here so unit tests can fake it
// without a database.
type AccountStore interface {
	GetProvider(ctx context.Context, id string) (*domain.Provider, error)
	GetCredential(ctx context.Context, id string) (*domain.Credential, error)
	GetUpstreamAccount(ctx context.Context, id string) (*domain.UpstreamAccount, error)
	// GetUpstreamAccountByPair reads the UNIQUE(provider_id, credential_id)
	// pair — the idempotent-create conflict path re-reads the existing row.
	GetUpstreamAccountByPair(ctx context.Context, providerID, credentialID string) (*domain.UpstreamAccount, error)
	InsertUpstreamAccount(ctx context.Context, a *domain.UpstreamAccount) error
	SetUpstreamAccountStatusConditional(ctx context.Context, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error)
	UpdateUpstreamAccountProfile(ctx context.Context, id string, displayName *string, concurrencyLimit *int) error
	EndSessionBindingsForAccount(ctx context.Context, accountID, reason string) (int64, error)
}

// AccountTxStore is the transaction-aware upgrade of AccountStore: account
// mutation + session-binding propagation + audit row commit in ONE
// UnitOfWork (same discipline as credentials.TxStore).
type AccountTxStore interface {
	AccountStore
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	InsertUpstreamAccountTx(ctx context.Context, w domain.UnitOfWork, a *domain.UpstreamAccount) error
	SetUpstreamAccountStatusConditionalTx(ctx context.Context, w domain.UnitOfWork, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error)
	UpdateUpstreamAccountProfileTx(ctx context.Context, w domain.UnitOfWork, id string, displayName *string, concurrencyLimit *int) error
	EndSessionBindingsForAccountTx(ctx context.Context, w domain.UnitOfWork, accountID, reason string) ([]string, error)
}

// credentialLockTx is the optional store upgrade that closes the
// create-vs-revoke TOCTOU race: re-reading the credential row FOR UPDATE
// inside the create transaction serializes with a concurrent revoke/restore
// (both UPDATE the same row), so an account can never commit active while
// its credential is already revoked (审查修复:事务外预检存在竞态窗口).
type credentialLockTx interface {
	GetCredentialForUpdateTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Credential, error)
}

// maxConcurrencyLimit caps upstream-account concurrency_limit (审查修复
// M-9). The value is the per-account in-flight lease ceiling enforced by
// routing.AcquireUpstreamLease against ONE upstream credential — it bounds
// how much traffic a single key absorbs before the pool spreads load to
// the next account. Operators configure 1–16 in practice (fixtures and
// tests use 1–8); the old >= 0-only check accepted 2^31 silently, which
// effectively disabled per-account concurrency control. 128 stays an
// order of magnitude above any legitimate single-credential workload while
// refusing absurd values. Explicit 0 (备而不用) remains legal.
const maxConcurrencyLimit = 128

func validateConcurrencyLimit(n int) error {
	if n < 0 {
		return domain.NewError(domain.CodeInvalidInput, "concurrency_limit must be >= 0")
	}
	if n > maxConcurrencyLimit {
		return domain.NewError(domain.CodeInvalidInput,
			fmt.Sprintf("concurrency_limit must be <= %d (per-account in-flight ceiling for one upstream credential; a huge value disables per-account concurrency control)", maxConcurrencyLimit))
	}
	return nil
}

// AccountExistsError reports an idempotent-create hit on
// UNIQUE(provider_id, credential_id): no new row was written, Existing is
// the account already bound. The admin surface maps it to 409 + the
// existing account view (safe replay).
type AccountExistsError struct {
	Existing *domain.UpstreamAccount
}

func (e *AccountExistsError) Error() string {
	return "upstream account already exists for (provider_id, credential_id)"
}

// Unwrap exposes the conflict domain error so domain.CodeOf maps the typed
// error to CodeConflict (HTTP 409).
func (e *AccountExistsError) Unwrap() error {
	return domain.NewError(domain.CodeConflict, "upstream account already exists for (provider_id, credential_id)")
}

// CreateAccountInput is the validated payload of the admin create endpoint.
type CreateAccountInput struct {
	ProviderID   string
	CredentialID string
	// DisplayName defaults to the credential label when empty.
	DisplayName      string
	ConcurrencyLimit *int // nil → 1; explicit 0 = provisioned but unschedulable
	// Quota fields are a group: providing any requires limit+remaining
	// (≥0); a reported group pins source=reported + observed_at=now.
	QuotaLimitMicros     *int64
	QuotaRemainingMicros *int64
	QuotaResetAt         *time.Time
	Reason               string
}

// AccountService is the operator-facing upstream-account lifecycle.
type AccountService struct {
	store AccountStore
	audit management.AuditRecorder
}

func NewAccountService(store AccountStore, audit management.AuditRecorder) *AccountService {
	return &AccountService{store: store, audit: audit}
}

func (s *AccountService) runAtomicAccounts(ctx context.Context, fn func(w domain.UnitOfWork) error) (bool, error) {
	ts, ok := s.store.(AccountTxStore)
	if !ok {
		return false, nil
	}
	if _, ok := s.audit.(TxRecorder); !ok {
		return false, nil
	}
	w, err := ts.Begin(ctx)
	if err != nil {
		return true, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Rollback(ctx)
		}
	}()
	if err := fn(w); err != nil {
		return true, err
	}
	if err := w.Commit(ctx); err != nil {
		return true, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return true, nil
}

// assertCredentialActiveTx re-checks the bound credential's status FOR
// UPDATE inside the current UnitOfWork (审查修复 I-5). The pre-tx read is
// only an early-exit optimization — a revoke landing between the pre-check
// and the commit would otherwise reactivate an account on a revoked
// credential. The FOR UPDATE read serializes with revoke/restore (both
// UPDATE the same row): either ordering ends consistent (revoke first →
// this 409s and the whole tx rolls back; activate first → the revoke
// cascade disables the account right after). Stores without the lock
// interface keep the pre-tx check as their only guard.
func (s *AccountService) assertCredentialActiveTx(ctx context.Context, w domain.UnitOfWork, credID string) error {
	cl, ok := s.store.(credentialLockTx)
	if !ok {
		return nil
	}
	fresh, err := cl.GetCredentialForUpdateTx(ctx, w, credID)
	if err != nil {
		return err
	}
	if fresh.Status == "revoked" {
		return domain.NewError(domain.CodeConflict,
			"credential is revoked; restore the credential before reactivating its account")
	}
	return nil
}

func accountAuditEvent(op Operator, action, objectID, reason string, detail map[string]any) management.AuditEvent {
	return management.AuditEvent{
		Action:     action,
		ObjectType: "upstream_account",
		ObjectID:   objectID,
		Reason:     reason,
		ActorUser:  op.UserID,
		ActorApp:   op.AppID,
		Detail:     management.SanitizeDetail(detail),
	}
}

func (s *AccountService) recordAccount(ctx context.Context, op Operator, action, objectID, reason string, detail map[string]any) error {
	if err := s.audit.Record(ctx, accountAuditEvent(op, action, objectID, reason, detail)); err != nil {
		return domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	return nil
}

func validReason(reason string) error {
	if reason == "" {
		return domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	if len(reason) > 200 {
		return domain.NewError(domain.CodeInvalidInput, "reason must be at most 200 characters")
	}
	return nil
}

// Create binds a static-key credential to a schedulable upstream account.
// The new row is status=active immediately — accounts are runtime routing
// state, not part of a published catalog snapshot, so no publish is needed.
func (s *AccountService) Create(ctx context.Context, op Operator, in CreateAccountInput) (*domain.UpstreamAccount, error) {
	if in.ProviderID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "provider_id is required")
	}
	if in.CredentialID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "credential_id is required")
	}
	if err := validReason(in.Reason); err != nil {
		return nil, err
	}
	if len(in.DisplayName) > 128 {
		return nil, domain.NewError(domain.CodeInvalidInput, "display_name must be at most 128 characters")
	}
	if in.ConcurrencyLimit != nil {
		if err := validateConcurrencyLimit(*in.ConcurrencyLimit); err != nil {
			return nil, err
		}
	}
	var quota domain.UpstreamQuota
	if in.QuotaLimitMicros != nil || in.QuotaRemainingMicros != nil || in.QuotaResetAt != nil {
		if in.QuotaLimitMicros == nil || in.QuotaRemainingMicros == nil {
			return nil, domain.NewError(domain.CodeInvalidInput,
				"quota_limit_micros and quota_remaining_micros must be provided together")
		}
		if *in.QuotaLimitMicros < 0 || *in.QuotaRemainingMicros < 0 {
			return nil, domain.NewError(domain.CodeInvalidInput, "quota values must be >= 0")
		}
		limit := domain.Microcredit(*in.QuotaLimitMicros)
		remaining := domain.Microcredit(*in.QuotaRemainingMicros)
		source := "reported"
		now := time.Now().UTC()
		quota = domain.UpstreamQuota{
			LimitMicros: &limit, RemainingMicros: &remaining,
			ObservedAt: &now, Source: &source, ResetAt: in.QuotaResetAt,
		}
	}

	prov, err := s.store.GetProvider(ctx, in.ProviderID)
	if err != nil {
		return nil, err
	}
	if prov.Status != "active" {
		return nil, domain.NewError(domain.CodeConflict, "provider is disabled")
	}
	cred, err := s.store.GetCredential(ctx, in.CredentialID)
	if err != nil {
		return nil, err
	}
	if cred.ProviderID != in.ProviderID {
		return nil, domain.NewError(domain.CodeInvalidInput, "credential does not belong to provider")
	}
	if cred.AuthType != "api_key" && cred.AuthType != "service" {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"oauth credentials are bound to accounts by the authorization flow, not manually")
	}
	if cred.Status != "active" {
		return nil, domain.NewError(domain.CodeConflict, "credential is not active ("+cred.Status+")")
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = cred.Label
	}
	conc := 1
	if in.ConcurrencyLimit != nil {
		conc = *in.ConcurrencyLimit
	}
	account := &domain.UpstreamAccount{
		ProviderID: in.ProviderID, CredentialID: in.CredentialID,
		DisplayName: displayName, Status: domain.AccountActive,
		ConcurrencyLimit: conc, Quota: quota,
	}
	detail := map[string]any{
		"provider_id": in.ProviderID, "credential_id": in.CredentialID,
		"concurrency_limit": conc,
	}
	if ran, err := s.runAtomicAccounts(ctx, func(w domain.UnitOfWork) error {
		if cl, ok := s.store.(credentialLockTx); ok {
			// 事务内持行锁复查凭据状态:并发吊销会 UPDATE 同一行,两种
			// 定序都一致(先吊销→这里见 revoked 409;先建→吊销级联随后
			// 停用本账号),不会出现"账号 active 但凭据 revoked"的稳态。
			fresh, err := cl.GetCredentialForUpdateTx(ctx, w, in.CredentialID)
			if err != nil {
				return err
			}
			if fresh.Status != "active" {
				return domain.NewError(domain.CodeConflict, "credential is not active ("+fresh.Status+")")
			}
		}
		if err := s.store.(AccountTxStore).InsertUpstreamAccountTx(ctx, w, account); err != nil {
			return err
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, accountAuditEvent(op, "upstream_account.create", account.ID, in.Reason, detail))
	}); ran {
		if err != nil {
			return nil, s.createConflictView(ctx, in.ProviderID, in.CredentialID, err)
		}
		return account, nil
	}
	// Sequential fallback for plain (non-transactional) test stores.
	if err := s.store.InsertUpstreamAccount(ctx, account); err != nil {
		return nil, s.createConflictView(ctx, in.ProviderID, in.CredentialID, err)
	}
	if err := s.recordAccount(ctx, op, "upstream_account.create", account.ID, in.Reason, detail); err != nil {
		return nil, err
	}
	return account, nil
}

// createConflictView maps a UNIQUE(provider_id, credential_id) collision to
// AccountExistsError carrying the already-bound account (409 + 已存在视图);
// any other error passes through unchanged.
func (s *AccountService) createConflictView(ctx context.Context, providerID, credentialID string, err error) error {
	if domain.CodeOf(err) != domain.CodeConflict {
		return err
	}
	existing, lookupErr := s.store.GetUpstreamAccountByPair(ctx, providerID, credentialID)
	if lookupErr != nil {
		return err
	}
	return &AccountExistsError{Existing: existing}
}

// SetStatus flips the operational state (active|disabled) of one account.
// Runtime state-machine values are rejected as targets; activation is only
// allowed from disabled and never while the bound credential is revoked.
// Disabling ends the account's live session bindings in the same
// transaction (失效传播与凭据吊销同语义).
func (s *AccountService) SetStatus(ctx context.Context, op Operator, id, status, reason string) (*domain.UpstreamAccount, error) {
	if status != string(domain.AccountActive) && status != string(domain.AccountDisabled) {
		return nil, domain.NewError(domain.CodeInvalidInput, "status must be active or disabled")
	}
	if err := validReason(reason); err != nil {
		return nil, err
	}
	account, err := s.store.GetUpstreamAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	to := domain.UpstreamAccountStatus(status)
	from := account.Status

	// 幂等：已是目标态 → 直接返回当前视图，仍记审计（noop 标记）。
	if from == to {
		if to == domain.AccountActive {
			// 幂等激活也守同一护栏：账号已 active 但凭据 revoked（正常
			// 路径不可达——吊销级联同事务停用账号，仅直写库可造成）时
			// 按正常激活路径一样 409,而不是 noop 成功。
			cred, err := s.store.GetCredential(ctx, account.CredentialID)
			if err != nil {
				return nil, err
			}
			if cred.Status == "revoked" {
				return nil, domain.NewError(domain.CodeConflict,
					"credential is revoked; restore the credential before reactivating its account")
			}
			noopDetail := map[string]any{
				"from": string(from), "to": string(to),
				"credential_id": account.CredentialID, "provider_id": account.ProviderID,
				"noop": true,
			}
			// 审查修复 I-5: 事务内持行锁复查凭据（与 Create 同模式）。
			// 预检与审计提交之间存在竞态窗口——凭据在预检后、审计前被
			// 吊销会留下"账号 active + 凭据 revoked"的稳态;FOR UPDATE 复查
			// 与并发吊销 UPDATE 同一行互斥,两种定序都一致。
			if ran, err := s.runAtomicAccounts(ctx, func(w domain.UnitOfWork) error {
				if err := s.assertCredentialActiveTx(ctx, w, account.CredentialID); err != nil {
					return err
				}
				return s.audit.(TxRecorder).RecordTx(ctx, w, accountAuditEvent(op, "upstream_account.status", id, reason, noopDetail))
			}); ran {
				if err != nil {
					return nil, err
				}
				return account, nil
			}
			if err := s.recordAccount(ctx, op, "upstream_account.status", id, reason, noopDetail); err != nil {
				return nil, err
			}
			return account, nil
		}
		if err := s.recordAccount(ctx, op, "upstream_account.status", id, reason, map[string]any{
			"from": string(from), "to": string(to),
			"credential_id": account.CredentialID, "provider_id": account.ProviderID,
			"noop": true,
		}); err != nil {
			return nil, err
		}
		return account, nil
	}

	var fromStates []domain.UpstreamAccountStatus
	if to == domain.AccountActive {
		// 恢复调度：绑定凭据 revoked 时拒绝（先恢复凭据）。
		cred, err := s.store.GetCredential(ctx, account.CredentialID)
		if err != nil {
			return nil, err
		}
		if cred.Status == "revoked" {
			return nil, domain.NewError(domain.CodeConflict,
				"credential is revoked; restore the credential before reactivating its account")
		}
		// 运营恢复只允许从 disabled：refreshing/cooldown/reauth_required
		// 归运行时状态机管。
		if from != domain.AccountDisabled {
			return nil, domain.NewError(domain.CodeConflict,
				"account status "+string(from)+" is managed by the runtime state machine")
		}
		fromStates = []domain.UpstreamAccountStatus{domain.AccountDisabled}
	} else {
		// 停用：从任何非 disabled 态都允许（运营兜底操作）。
		fromStates = []domain.UpstreamAccountStatus{
			domain.AccountActive, domain.AccountRefreshing,
			domain.AccountCooldown, domain.AccountReauthRequired,
		}
	}

	detail := map[string]any{
		"from": string(from), "to": string(to),
		"credential_id": account.CredentialID, "provider_id": account.ProviderID,
	}
	apply := func(set func() (bool, error), endBindings func() error) error {
		flipped, err := set()
		if err != nil {
			return err
		}
		if !flipped {
			return domain.NewError(domain.CodeConflict, "account status changed concurrently; retry")
		}
		if to == domain.AccountDisabled && endBindings != nil {
			if err := endBindings(); err != nil {
				return err
			}
		}
		return nil
	}
	if ran, err := s.runAtomicAccounts(ctx, func(w domain.UnitOfWork) error {
		if to == domain.AccountActive {
			// 审查修复 I-5: 凭据 revoked 复查必须在事务内完成——预检
			// （下方）与状态写入之间存在竞态窗口，凭据在两者之间被吊销
			// 会提交出"账号 active + 凭据 revoked"。GetCredentialForUpdateTx
			// 与并发吊销 UPDATE 同一行互斥，先吊销→这里见 revoked 409 整体
			// 回滚；先激活→吊销级联随后停用本账号，两种定序都一致。
			if err := s.assertCredentialActiveTx(ctx, w, account.CredentialID); err != nil {
				return err
			}
		}
		ts := s.store.(AccountTxStore)
		var ended []string
		err := apply(
			func() (bool, error) { return ts.SetUpstreamAccountStatusConditionalTx(ctx, w, id, fromStates, to) },
			func() error {
				var err error
				ended, err = ts.EndSessionBindingsForAccountTx(ctx, w, id, domain.BindingEndedAccountInvalid)
				return err
			},
		)
		if err != nil {
			return err
		}
		if len(ended) > 0 {
			detail["session_bindings_ended"] = len(ended)
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, accountAuditEvent(op, "upstream_account.status", id, reason, detail))
	}); ran {
		if err != nil {
			return nil, err
		}
	} else {
		var ended int64
		err := apply(
			func() (bool, error) { return s.store.SetUpstreamAccountStatusConditional(ctx, id, fromStates, to) },
			func() error {
				var err error
				ended, err = s.store.EndSessionBindingsForAccount(ctx, id, domain.BindingEndedAccountInvalid)
				return err
			},
		)
		if err != nil {
			return nil, err
		}
		if ended > 0 {
			detail["session_bindings_ended"] = ended
		}
		if err := s.recordAccount(ctx, op, "upstream_account.status", id, reason, detail); err != nil {
			return nil, err
		}
	}
	account.Status = to
	return account, nil
}

// Update patches the operator-editable fields (display_name /
// concurrency_limit) without rebinding the credential.
func (s *AccountService) Update(ctx context.Context, op Operator, id string, displayName *string, concurrencyLimit *int, reason string) (*domain.UpstreamAccount, error) {
	if displayName == nil && concurrencyLimit == nil {
		return nil, domain.NewError(domain.CodeInvalidInput, "at least one of display_name, concurrency_limit is required")
	}
	if err := validReason(reason); err != nil {
		return nil, err
	}
	if displayName != nil && len(*displayName) > 128 {
		return nil, domain.NewError(domain.CodeInvalidInput, "display_name must be at most 128 characters")
	}
	if concurrencyLimit != nil {
		if err := validateConcurrencyLimit(*concurrencyLimit); err != nil {
			return nil, err
		}
	}
	account, err := s.store.GetUpstreamAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	after := map[string]any{}
	if displayName != nil {
		after["display_name"] = *displayName
	}
	if concurrencyLimit != nil {
		after["concurrency_limit"] = *concurrencyLimit
	}
	detail := map[string]any{
		"before": map[string]any{"display_name": account.DisplayName, "concurrency_limit": account.ConcurrencyLimit},
		"after":  after,
	}
	if ran, err := s.runAtomicAccounts(ctx, func(w domain.UnitOfWork) error {
		ts := s.store.(AccountTxStore)
		if err := ts.UpdateUpstreamAccountProfileTx(ctx, w, id, displayName, concurrencyLimit); err != nil {
			return err
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, accountAuditEvent(op, "upstream_account.update", id, reason, detail))
	}); ran {
		if err != nil {
			return nil, err
		}
	} else {
		if err := s.store.UpdateUpstreamAccountProfile(ctx, id, displayName, concurrencyLimit); err != nil {
			return nil, err
		}
		if err := s.recordAccount(ctx, op, "upstream_account.update", id, reason, detail); err != nil {
			return nil, err
		}
	}
	if displayName != nil {
		account.DisplayName = *displayName
	}
	if concurrencyLimit != nil {
		account.ConcurrencyLimit = *concurrencyLimit
	}
	return account, nil
}
