// session_binding.go — 粘性会话绑定（Task 12，设计 §8: 需要会话黏性时绑
// 定具体账号，撤销或失效后按照协议要求终止/重建会话，不能无条件切账号
// 续接）。
//
// 语义:
//   - Bind: (session_key, model_id) → 一个账号；已存在活跃绑定即冲突
//     （持久层部分唯一索引保证跨实例一致）。
//   - Resolve: 返回绑定账号；绑定账号不再可调（失效/额度耗尽/冷却）时
//     返回 CodeConflict —— 调度方必须终止该会话或按新会话语义显式重建
//     绑定，绝不静默切账号续接。
//   - Migrate: 显式迁移 = 终止旧绑定（reason=migrated）+ 同事务建立新绑
//     定；新会话内容如何重建由协议层决定，本层只保证绑定关系可审计。
//   - EndForAccount: 账号失效传播（reauth/disabled）时终止其全部活跃绑
//     定，停止向失效账号分配新请求（账号状态翻转本身停止调度；绑定终止
//     让既有会话得到明确的中断信号而非悬垂）。

package routing

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// DefaultBindingTTL caps a sticky binding's lifetime (会话不是永久的；
// 到期由 worker/惰性检查终止).
const DefaultBindingTTL = 24 * time.Hour

// BindingStore is the persistence surface of the session binder; satisfied
// by inference/postgres.Store.
type BindingStore interface {
	InsertSessionBinding(ctx context.Context, b *domain.SessionBinding) error
	GetActiveSessionBinding(ctx context.Context, sessionKey, modelID string, now time.Time) (*domain.SessionBinding, error)
	TouchSessionBinding(ctx context.Context, id string) error
	EndSessionBinding(ctx context.Context, id, reason string) (bool, error)
	GetUpstreamAccount(ctx context.Context, id string) (*domain.UpstreamAccount, error)
	// Begin + Tx variants power Migrate's atomic end-old + insert-new.
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	InsertSessionBindingTx(ctx context.Context, w domain.UnitOfWork, b *domain.SessionBinding) error
	EndSessionBindingTx(ctx context.Context, w domain.UnitOfWork, id, reason string) (bool, error)
}

// SessionBinder owns sticky-session bindings over the binding store.
type SessionBinder struct {
	store BindingStore
	clock domain.Clock
	// TTL bounds new bindings (default DefaultBindingTTL).
	TTL time.Duration
}

func NewSessionBinder(store BindingStore, clock domain.Clock) *SessionBinder {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &SessionBinder{store: store, clock: clock, TTL: DefaultBindingTTL}
}

// Bind pins (sessionKey, modelID) to accountID. The account must be
// currently active; a live binding for the same pair is a CodeConflict —
// the caller ends or migrates it explicitly.
func (b *SessionBinder) Bind(ctx context.Context, sessionKey, modelID, accountID string) (*domain.SessionBinding, error) {
	if sessionKey == "" || modelID == "" || accountID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "session_key, model_id and account_id are required")
	}
	account, err := b.store.GetUpstreamAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account.Status != domain.AccountActive {
		return nil, domain.NewError(domain.CodeConflict, "account is not schedulable")
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = DefaultBindingTTL
	}
	binding := &domain.SessionBinding{
		SessionKey: sessionKey, ModelID: modelID, AccountID: accountID,
		Status:    domain.BindingActive,
		ExpiresAt: b.clock.Now().Add(ttl),
	}
	if err := b.store.InsertSessionBinding(ctx, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// Resolve returns the account pinned for (sessionKey, modelID).
// CodeNotFound = no live binding (caller may Bind). CodeConflict = the
// binding exists but its account is no longer schedulable (失效/冷却/额度
// 耗尽): the conversation must be terminated per protocol or explicitly
// migrated — NEVER silently continued on another account.
func (b *SessionBinder) Resolve(ctx context.Context, sessionKey, modelID string) (*domain.SessionBinding, *domain.UpstreamAccount, error) {
	binding, err := b.store.GetActiveSessionBinding(ctx, sessionKey, modelID, b.clock.Now())
	if err != nil {
		return nil, nil, err
	}
	account, err := b.store.GetUpstreamAccount(ctx, binding.AccountID)
	if err != nil {
		return nil, nil, err
	}
	if account.Status != domain.AccountActive || quotaExhausted(*account, b.clock.Now()) {
		return binding, account, domain.NewError(domain.CodeConflict,
			"bound account is no longer schedulable (status="+string(account.Status)+")")
	}
	if err := b.store.TouchSessionBinding(ctx, binding.ID); err != nil {
		// 并发终止竞争：触碰失败视为绑定已失效，按不可调度处理。
		return binding, account, domain.WrapError(domain.CodeConflict, "binding ended concurrently", err)
	}
	return binding, account, nil
}

// Migrate explicitly moves one conversation to another account: the old
// binding ends (reason=migrated) and the new one inserts in the SAME
// transaction — the pair never has zero or two live bindings.
func (b *SessionBinder) Migrate(ctx context.Context, sessionKey, modelID, newAccountID string) (*domain.SessionBinding, error) {
	account, err := b.store.GetUpstreamAccount(ctx, newAccountID)
	if err != nil {
		return nil, err
	}
	if account.Status != domain.AccountActive {
		return nil, domain.NewError(domain.CodeConflict, "target account is not schedulable")
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = DefaultBindingTTL
	}
	w, err := b.store.Begin(ctx)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Rollback(ctx)
		}
	}()
	if old, err := b.store.GetActiveSessionBinding(ctx, sessionKey, modelID, b.clock.Now()); err == nil {
		if _, err := b.store.EndSessionBindingTx(ctx, w, old.ID, domain.BindingEndedMigrated); err != nil {
			return nil, err
		}
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	}
	binding := &domain.SessionBinding{
		SessionKey: sessionKey, ModelID: modelID, AccountID: newAccountID,
		Status:    domain.BindingActive,
		ExpiresAt: b.clock.Now().Add(ttl),
	}
	if err := b.store.InsertSessionBindingTx(ctx, w, binding); err != nil {
		return nil, err
	}
	if err := w.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return binding, nil
}

// End terminates one binding explicitly (运营/协议要求的会话终止).
func (b *SessionBinder) End(ctx context.Context, sessionKey, modelID, reason string) (bool, error) {
	binding, err := b.store.GetActiveSessionBinding(ctx, sessionKey, modelID, b.clock.Now())
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return false, nil
		}
		return false, err
	}
	if reason == "" {
		reason = domain.BindingEndedOperator
	}
	return b.store.EndSessionBinding(ctx, binding.ID, reason)
}
