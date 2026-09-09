// refresh.go — OAuth 凭据轮换：跨实例分布式刷新锁 + generation CAS
// （设计 §8: 刷新使用跨实例互斥与 credential generation CAS，避免旧
// refresh token 覆盖新值）。
//
// 协议（RefreshCredential）:
//  1. 读凭据 + 代次 g0，解出 bundle 拿 refresh token；
//  2. 无锁调用厂商 refresh 端点（慢路径不持锁，不阻塞他人）；
//     - invalid_grant / 401 / 403 → 授权已失效：锁内把绑定账号翻转为
//      reauth_required（停止分配新请求）、终止其会话绑定、审计，整体一个
//      事务；
//     - 429/5xx/传输错误 → 可重试，不动账号状态（连接器暂时不可用不等于
//       授权失效）；
//  3. 成功 → 写阶段：pg_advisory_xact_lock 互斥 + 锁内重读，代次仍等于
//     g0 才 CAS 落库（UPDATE ... WHERE generation=g0）；代次已移动说明另
//     一实例/手工轮换已提交更新的 token——本结果整体丢弃，收敛到当前值
//     （旧 token 晚返回不得覆盖新 token）。
//
// 审计归因：worker 驱动的刷新没有操作员身份，actor_app 固定
// "system:inference-worker"，actor_user 为空（审计表允许）。

package credentials

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// WorkerActorApp is the audit service-attribution for worker-driven
// refresh/health mutations (person attribution is empty by design — no
// operator identity exists in a background pass).
const WorkerActorApp = "system:inference-worker"

// RefreshStore is the persistence surface of the refresh protocol;
// satisfied by inference/postgres.Store.
type RefreshStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GetCredential(ctx context.Context, id string) (*domain.Credential, error)
	// AcquireCredentialRefreshLockTx takes pg_advisory_xact_lock on the
	// credential inside the caller's transaction (跨实例互斥，随事务自动
	// 释放).
	AcquireCredentialRefreshLockTx(ctx context.Context, w domain.UnitOfWork, credentialID string) error
	GetCredentialTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Credential, error)
	// RotateCredentialSecretCAS commits the new bundle only when the stored
	// generation still equals expectedGeneration (旧 token 不得覆盖新值).
	RotateCredentialSecretCAS(ctx context.Context, w domain.UnitOfWork, id string, ciphertext []byte, keyVersion int, expectedGeneration int64, expiresAt *time.Time) (int64, error)
	// SetUpstreamAccountsStatusByCredentialTx flips the bound accounts
	// (reauth propagation) inside the same UnitOfWork as the audit row.
	SetUpstreamAccountsStatusByCredentialTx(ctx context.Context, w domain.UnitOfWork, credentialID string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) ([]string, error)
	// EndSessionBindingsForAccountTx terminates live sticky bindings of an
	// invalidated account (绑定可迁移但必须先终止，设计 §8).
	EndSessionBindingsForAccountTx(ctx context.Context, w domain.UnitOfWork, accountID, reason string) ([]string, error)
}

// RefreshOutcome reports what one refresh pass did.
type RefreshOutcome struct {
	CredentialID string `json:"credential_id"`
	// Rotated: a new bundle was committed (generation bumped).
	Rotated bool `json:"rotated"`
	// Converged: another actor committed a newer generation while this
	// pass called the vendor — this pass's result was DISCARDED and the
	// stored (newer) value stands.
	Converged bool `json:"converged"`
	// ReauthRequired: the vendor definitively rejected the grant; bound
	// accounts are now reauth_required and their session bindings ended.
	ReauthRequired bool  `json:"reauth_required"`
	Generation     int64 `json:"generation"`
}

// Refresher rotates oauth credentials through the vendor connector.
type Refresher struct {
	vault    *Vault
	store    RefreshStore
	audit    management.AuditRecorder
	client   *connector.Client
	registry connector.Registry
	clock    domain.Clock
}

func NewRefresher(vault *Vault, store RefreshStore, audit management.AuditRecorder, client *connector.Client, registry connector.Registry, clock domain.Clock) *Refresher {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	if client == nil {
		client = &connector.Client{}
	}
	return &Refresher{vault: vault, store: store, audit: audit, client: client, registry: registry, clock: clock}
}

func (r *Refresher) record(ctx context.Context, action, objectID, reason string, detail map[string]any) error {
	if r.audit == nil {
		return nil
	}
	if err := r.audit.Record(ctx, management.AuditEvent{
		Action: action, ObjectType: "credential", ObjectID: objectID,
		Reason: reason, ActorApp: WorkerActorApp,
		Detail: management.SanitizeDetail(detail),
	}); err != nil {
		return domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	return nil
}

// RefreshCredential runs the full rotation protocol for one credential.
// Returns the outcome; a retryable vendor/transport failure is returned as
// an error (the caller backs off and retries), while a definitive rejection
// is reported via the outcome (ReauthRequired) with a nil error — the state
// transition already committed.
func (r *Refresher) RefreshCredential(ctx context.Context, credentialID, reason string) (*RefreshOutcome, error) {
	if r.vault == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	cred, err := r.store.GetCredential(ctx, credentialID)
	if err != nil {
		return nil, err
	}
	if cred.AuthType != "oauth" {
		return nil, domain.NewError(domain.CodeInvalidInput, "only oauth credentials refresh through a connector")
	}
	if cred.Status != "active" && cred.Status != "rotating" {
		return nil, domain.NewError(domain.CodeConflict, "credential is disabled")
	}
	plain, err := r.vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "credential decrypt failed", err)
	}
	bundle, err := UnmarshalBundle(plain)
	if err != nil {
		return nil, err
	}
	if bundle.RefreshToken == "" {
		// Access-token-only grant: nothing to rotate. Operator re-runs the
		// authorization flow instead.
		return &RefreshOutcome{CredentialID: credentialID, Generation: cred.Generation}, nil
	}
	spec, ok := r.registry[cred.Connector]
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "oauth connector "+cred.Connector+" not configured")
	}
	g0 := cred.Generation

	// Slow vendor call WITHOUT the lock: two instances may both be here
	// concurrently; the write phase below serializes and discards the loser.
	ts, err := r.client.RefreshToken(ctx, spec, bundle.RefreshToken)
	if err != nil {
		if connector.KindOf(err) == connector.KindReauthRequired {
			outcome, rerr := r.markReauthRequired(ctx, cred, reason, err)
			if rerr != nil {
				return nil, rerr
			}
			return outcome, nil
		}
		return nil, domain.WrapError(domain.CodeUpstreamUnavailable, "oauth refresh request failed", err)
	}

	newBundle, err := MarshalBundle(&OAuthBundle{
		AccessToken: ts.AccessToken,
		// 厂商可能不轮换 refresh token：保留旧值继续用（RFC 6749 §6 允许）。
		RefreshToken: orElse(ts.RefreshToken, bundle.RefreshToken),
		TokenType:    orElse(ts.TokenType, bundle.TokenType),
		ObtainedAt:   r.clock.Now(),
	})
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "marshal oauth bundle", err)
	}
	ct, keyVersion, err := r.vault.Encrypt(cred.ID, cred.ProviderID, newBundle)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "encrypt oauth bundle", err)
	}
	var expiry *time.Time
	if !ts.Expiry.IsZero() {
		exp := ts.Expiry.UTC()
		expiry = &exp
	}

	// Write phase: lock + post-lock re-read + CAS.
	w, err := r.store.Begin(ctx)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Rollback(ctx)
		}
	}()
	if err := r.store.AcquireCredentialRefreshLockTx(ctx, w, credentialID); err != nil {
		return nil, err
	}
	current, err := r.store.GetCredentialTx(ctx, w, credentialID)
	if err != nil {
		return nil, err
	}
	if current.Generation != g0 {
		// 另一实例/手工轮换在我们调用厂商期间已提交更新的 token。
		// 丢弃本次结果，收敛到当前值——绝不覆盖。
		_ = w.Rollback(ctx)
		committed = true // rollback done; suppress deferred rollback
		return &RefreshOutcome{
			CredentialID: credentialID, Converged: true, Generation: current.Generation,
		}, nil
	}
	if current.Status != "active" && current.Status != "rotating" {
		_ = w.Rollback(ctx)
		committed = true
		return nil, domain.NewError(domain.CodeConflict, "credential disabled during refresh")
	}
	newGen, err := r.store.RotateCredentialSecretCAS(ctx, w, credentialID, ct, keyVersion, g0, expiry)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			_ = w.Rollback(ctx)
			committed = true
			latest, lerr := r.store.GetCredential(ctx, credentialID)
			if lerr != nil {
				return nil, lerr
			}
			return &RefreshOutcome{CredentialID: credentialID, Converged: true, Generation: latest.Generation}, nil
		}
		return nil, err
	}
	if r.audit != nil {
		if rec, ok := r.audit.(TxRecorder); ok {
			if err := rec.RecordTx(ctx, w, management.AuditEvent{
				Action: "oauth.refresh.rotated", ObjectType: "credential", ObjectID: credentialID,
				Reason: reason, ActorApp: WorkerActorApp,
				Detail: management.SanitizeDetail(map[string]any{
					"provider_id": cred.ProviderID, "connector": cred.Connector,
					"generation": newGen, "key_version": keyVersion,
				}),
			}); err != nil {
				return nil, domain.WrapError(domain.CodeInternal, "audit write failed", err)
			}
		}
	}
	if err := w.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return &RefreshOutcome{
		CredentialID: credentialID, Rotated: true, Generation: newGen,
	}, nil
}

// markReauthRequired is the definitive-rejection path (授权撤销/过期):
// inside ONE UnitOfWork it flips every bound account to reauth_required
// (停止分配新请求: ListActiveUpstreamAccounts 只认 active), terminates
// their live session bindings (设计 §8: 撤销或失效后按照协议要求终止/重建
// 会话，不能无条件切账号续接), and writes the audit row — all-or-nothing.
func (r *Refresher) markReauthRequired(ctx context.Context, cred *domain.Credential, reason string, cause error) (*RefreshOutcome, error) {
	w, err := r.store.Begin(ctx)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Rollback(ctx)
		}
	}()
	if err := r.store.AcquireCredentialRefreshLockTx(ctx, w, cred.ID); err != nil {
		return nil, err
	}
	accountIDs, err := r.store.SetUpstreamAccountsStatusByCredentialTx(ctx, w, cred.ID,
		[]domain.UpstreamAccountStatus{
			domain.AccountActive, domain.AccountRefreshing, domain.AccountCooldown,
		}, domain.AccountReauthRequired)
	if err != nil {
		return nil, err
	}
	bindingsEnded := 0
	for _, accountID := range accountIDs {
		ids, err := r.store.EndSessionBindingsForAccountTx(ctx, w, accountID, domain.BindingEndedAccountInvalid)
		if err != nil {
			return nil, err
		}
		bindingsEnded += len(ids)
	}
	if r.audit != nil {
		if rec, ok := r.audit.(TxRecorder); ok {
			if err := rec.RecordTx(ctx, w, management.AuditEvent{
				Action: "oauth.refresh.reauth_required", ObjectType: "credential", ObjectID: cred.ID,
				Reason: reason, ActorApp: WorkerActorApp,
				Detail: management.SanitizeDetail(map[string]any{
					"provider_id": cred.ProviderID, "connector": cred.Connector,
					"accounts_reauth": len(accountIDs), "session_bindings_ended": bindingsEnded,
					"vendor_error": connector.KindOf(cause),
				}),
			}); err != nil {
				return nil, domain.WrapError(domain.CodeInternal, "audit write failed", err)
			}
		}
	}
	if err := w.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return &RefreshOutcome{
		CredentialID: cred.ID, ReauthRequired: true, Generation: cred.Generation,
	}, nil
}

func orElse(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
