// upstream_health.go — 上游账号健康与额度观测 worker（Task 12，设计 §8:
// 账号容量、冷却、健康度、上游额度 observed_at/source/reset_at）。
//
// 每轮扫描 active / 冷却到期 / refreshing 的账号：
//   - oauth 账号：解出 bundle 用连接器 Health 探测；401/403 → 立刻走
//     Refresher 刷新（成功则保持 active；厂商明确拒绝则 Refresher 已翻转
//     reauth_required + 终止绑定）；可重试失败 → cooldown，冷却到期复测
//     恢复 active。探测成功后读取 Quota（不可得保持未知，绝不编造），经
//     observed_at 单调守卫落缓存。
//   - 自托管/静态凭据账号（service/api_key）：走独立的 ServiceAuth 适配器
//     探测（不强迫 OSS 模型使用 OAuth）；401/403 → reauth_required（静态
//     凭据被上游轮换，需运营手工轮换）；可重试失败 → cooldown。
//
// 额度只进运营/调度视图：本 worker 不触碰任何客户侧读面（设计 §8: 不得
// 由客户余额推算上游余额，亦不得反向展示账号池总额度给客户）。

package workers

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// UpstreamHealthStore is the persistence surface the worker needs;
// satisfied by *postgres.Store.
type UpstreamHealthStore interface {
	ListUpstreamAccountsForHealth(ctx context.Context, cooldownDueBefore time.Time, limit int) ([]domain.UpstreamAccount, error)
	GetUpstreamAccount(ctx context.Context, id string) (*domain.UpstreamAccount, error)
	GetProvider(ctx context.Context, id string) (*domain.Provider, error)
	GetCredential(ctx context.Context, id string) (*domain.Credential, error)
	ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error)
	UpdateUpstreamAccountQuota(ctx context.Context, id string, q domain.UpstreamQuota) (bool, error)
	SetUpstreamAccountStatusConditional(ctx context.Context, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error)
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	EndSessionBindingsForAccountTx(ctx context.Context, w domain.UnitOfWork, accountID, reason string) ([]string, error)
	SetUpstreamAccountStatusConditionalTx(ctx context.Context, w domain.UnitOfWork, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error)
}

// UpstreamHealthConfig tunes the worker; zero values take the defaults.
type UpstreamHealthConfig struct {
	// Interval between passes (default 60s).
	Interval time.Duration
	// BatchLimit caps accounts per pass (default 100).
	BatchLimit int
	// Cooldown is how long a failed account stays 'cooldown' before the
	// next re-probe may flip it back (default 5min; the account is NOT
	// scheduled while cooling — ListActiveUpstreamAccounts only reads
	// 'active').
	Cooldown time.Duration
}

func (c *UpstreamHealthConfig) withDefaults() UpstreamHealthConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = time.Minute
	}
	if out.BatchLimit <= 0 {
		out.BatchLimit = 100
	}
	if out.Cooldown <= 0 {
		out.Cooldown = 5 * time.Minute
	}
	return out
}

// UpstreamHealth is the account probe worker.
type UpstreamHealth struct {
	store     UpstreamHealthStore
	vault     *credentials.Vault
	client    *connector.Client
	service   *connector.ServiceAuth
	refresher *credentials.Refresher
	registry  connector.Registry
	audit     management.AuditRecorder
	clock     domain.Clock
	cfg       UpstreamHealthConfig
}

func NewUpstreamHealth(store UpstreamHealthStore, vault *credentials.Vault, client *connector.Client, refresher *credentials.Refresher, registry connector.Registry, audit management.AuditRecorder, cfg UpstreamHealthConfig, clock domain.Clock) *UpstreamHealth {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	if client == nil {
		client = &connector.Client{}
	}
	return &UpstreamHealth{
		store: store, vault: vault, client: client, service: &connector.ServiceAuth{HTTP: client.HTTP},
		refresher: refresher, registry: registry, audit: audit, clock: clock, cfg: cfg.withDefaults(),
	}
}

// UpstreamHealthMetrics is one pass's structured-log snapshot.
type UpstreamHealthMetrics struct {
	Scanned        int `json:"scanned"`
	Healthy        int `json:"healthy"`
	CooledDown     int `json:"cooled_down"`
	Recovered      int `json:"recovered"`
	ReauthRequired int `json:"reauth_required"`
	QuotaObserved  int `json:"quota_observed"`
	Errors         int `json:"errors"`
}

// RunPass executes one health round.
func (w *UpstreamHealth) RunPass(ctx context.Context) (UpstreamHealthMetrics, error) {
	var m UpstreamHealthMetrics
	dueBefore := w.clock.Now().Add(-w.cfg.Cooldown)
	accounts, err := w.store.ListUpstreamAccountsForHealth(ctx, dueBefore, w.cfg.BatchLimit)
	if err != nil {
		return m, err
	}
	m.Scanned = len(accounts)
	for i := range accounts {
		w.probe(ctx, &accounts[i], &m)
	}
	return m, nil
}

func (w *UpstreamHealth) probe(ctx context.Context, a *domain.UpstreamAccount, m *UpstreamHealthMetrics) {
	cred, err := w.store.GetCredential(ctx, a.CredentialID)
	if err != nil {
		m.Errors++
		log.Printf("WARN upstream health: load credential account=%s: %v", a.ID, err)
		return
	}
	if cred.Status == "revoked" {
		// 凭据吊销的传播由 Task 4 SetStatus 负责；这里只兜底对齐状态。
		if _, err := w.store.SetUpstreamAccountStatusConditional(ctx, a.ID,
			[]domain.UpstreamAccountStatus{domain.AccountActive, domain.AccountCooldown, domain.AccountRefreshing},
			domain.AccountDisabled); err != nil {
			m.Errors++
		}
		return
	}
	if cred.AuthType == "oauth" {
		w.probeOAuth(ctx, a, cred, m)
		return
	}
	w.probeStatic(ctx, a, cred, m)
}

func (w *UpstreamHealth) probeOAuth(ctx context.Context, a *domain.UpstreamAccount, cred *domain.Credential, m *UpstreamHealthMetrics) {
	spec, ok := w.registry[cred.Connector]
	if !ok {
		m.Errors++
		log.Printf("WARN upstream health: connector %q not configured (account=%s)", cred.Connector, a.ID)
		return
	}
	if w.vault == nil {
		m.Errors++
		return
	}
	plain, err := w.vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		m.Errors++
		log.Printf("WARN upstream health: decrypt account=%s: %v", a.ID, err)
		return
	}
	bundle, err := credentials.UnmarshalBundle(plain)
	if err != nil {
		m.Errors++
		return
	}
	err = w.client.Health(ctx, spec, bundle.AccessToken)
	if err == nil {
		w.onHealthy(ctx, a, m)
		w.observeQuota(ctx, a, spec, bundle.AccessToken, m)
		return
	}
	if connector.KindOf(err) == connector.KindReauthRequired {
		// 访问令牌死了但 refresh 可能仍有效：立刻刷新一次，让 Refresher
		// 决定轮换或 reauth_required（含绑定终止与审计）。
		outcome, rerr := w.refresher.RefreshCredential(ctx, cred.ID, "health probe 401")
		switch {
		case rerr != nil:
			// 连接器暂时不可用：冷却而非误判失效。
			w.onRetryableFailure(ctx, a, m)
		case outcome.ReauthRequired:
			m.ReauthRequired++
		default: // rotated/converged/no-op → 凭据已更新，保持 active
			w.onHealthy(ctx, a, m)
		}
		return
	}
	w.onRetryableFailure(ctx, a, m)
}

func (w *UpstreamHealth) probeStatic(ctx context.Context, a *domain.UpstreamAccount, cred *domain.Credential, m *UpstreamHealthMetrics) {
	prov, err := w.store.GetProvider(ctx, a.ProviderID)
	if err != nil {
		m.Errors++
		return
	}
	if prov.AccessType != domain.AccessSelfHosted {
		// official_api 静态凭据的健康由真实调度错误信号覆盖（Task 8 冷却
		// 路径），主动探测没有通用端点——跳过。
		return
	}
	if w.vault == nil {
		m.Errors++
		return
	}
	plain, err := w.vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		m.Errors++
		return
	}
	// 独立接入适配器（不强迫 OSS 模型使用 OAuth）：探测该 provider 任一
	// active 部署的 base URL。
	deploys, err := w.store.ListDeployments(ctx, domain.DeploymentFilter{
		ProviderID: a.ProviderID, Status: domain.DeploymentActive, Limit: 1,
	})
	if err != nil {
		m.Errors++
		return
	}
	if len(deploys) == 0 {
		return // 无 active 部署：没有可探测端点，账号状态不变
	}
	err = w.service.Check(ctx, strings.TrimSuffix(deploys[0].BaseURL, "/")+"/", plain)
	if err == nil {
		w.onHealthy(ctx, a, m)
		return
	}
	if connector.KindOf(err) == connector.KindReauthRequired {
		// 静态凭据被上游拒绝 = 凭据被轮换/吊销：停止调度 + 终止绑定，
		// 等运营手工轮换凭据（Task 4 Rotate 后需人工恢复账号状态）。
		if rerr := w.markAccountReauth(ctx, a, "static service credential rejected"); rerr != nil {
			m.Errors++
			return
		}
		m.ReauthRequired++
		return
	}
	w.onRetryableFailure(ctx, a, m)
}

// markAccountReauth flips one account to reauth_required and terminates its
// live session bindings + audit in ONE UnitOfWork (账号失效传播原子性).
func (w *UpstreamHealth) markAccountReauth(ctx context.Context, a *domain.UpstreamAccount, reason string) error {
	txw, err := w.store.Begin(ctx)
	if err != nil {
		return domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = txw.Rollback(ctx)
		}
	}()
	flipped, err := w.store.SetUpstreamAccountStatusConditionalTx(ctx, txw, a.ID,
		[]domain.UpstreamAccountStatus{domain.AccountActive, domain.AccountCooldown, domain.AccountRefreshing},
		domain.AccountReauthRequired)
	if err != nil {
		return err
	}
	if !flipped {
		_ = txw.Rollback(ctx)
		committed = true
		return nil // 并发已处理
	}
	bindings, err := w.store.EndSessionBindingsForAccountTx(ctx, txw, a.ID, domain.BindingEndedAccountInvalid)
	if err != nil {
		return err
	}
	if w.audit != nil {
		if rec, ok := w.audit.(interface {
			RecordTx(context.Context, domain.UnitOfWork, management.AuditEvent) error
		}); ok {
			if err := rec.RecordTx(ctx, txw, management.AuditEvent{
				Action: "upstream_account.reauth_required", ObjectType: "upstream_account", ObjectID: a.ID,
				Reason: reason, ActorApp: credentials.WorkerActorApp,
				Detail: management.SanitizeDetail(map[string]any{
					"provider_id": a.ProviderID, "credential_id": a.CredentialID,
					"session_bindings_ended": len(bindings),
				}),
			}); err != nil {
				return domain.WrapError(domain.CodeInternal, "audit write failed", err)
			}
		}
	}
	if err := txw.Commit(ctx); err != nil {
		return domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return nil
}

func (w *UpstreamHealth) onHealthy(ctx context.Context, a *domain.UpstreamAccount, m *UpstreamHealthMetrics) {
	if a.Status == domain.AccountActive {
		m.Healthy++
		return
	}
	ok, err := w.store.SetUpstreamAccountStatusConditional(ctx, a.ID,
		[]domain.UpstreamAccountStatus{domain.AccountCooldown, domain.AccountRefreshing},
		domain.AccountActive)
	if err != nil {
		m.Errors++
		return
	}
	if ok {
		m.Recovered++
	}
}

func (w *UpstreamHealth) onRetryableFailure(ctx context.Context, a *domain.UpstreamAccount, m *UpstreamHealthMetrics) {
	if a.Status != domain.AccountActive {
		return // 已在冷却/刷新中，等下一轮
	}
	ok, err := w.store.SetUpstreamAccountStatusConditional(ctx, a.ID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountCooldown)
	if err != nil {
		m.Errors++
		return
	}
	if ok {
		m.CooledDown++
		log.Printf("WARN upstream account cooled down after failed probe account=%s", a.ID)
	}
}

func (w *UpstreamHealth) observeQuota(ctx context.Context, a *domain.UpstreamAccount, spec connector.Spec, accessToken string, m *UpstreamHealthMetrics) {
	snap, err := w.client.Quota(ctx, spec, accessToken, w.clock.Now())
	if err != nil {
		// 限额读取失败不影响健康结论；下一轮再试。
		return
	}
	if snap == nil {
		return // 上游额度不可得：保持未知（设计 §8）
	}
	q := domain.UpstreamQuota{
		ObservedAt: &snap.ObservedAt,
		ResetAt:    snap.ResetAt,
	}
	if snap.LimitMicros != nil {
		v := domain.Microcredit(*snap.LimitMicros)
		q.LimitMicros = &v
	}
	if snap.RemainingMicros != nil {
		v := domain.Microcredit(*snap.RemainingMicros)
		q.RemainingMicros = &v
	}
	source := "reported"
	q.Source = &source
	wrote, err := w.store.UpdateUpstreamAccountQuota(ctx, a.ID, q)
	if err != nil {
		m.Errors++
		return
	}
	if wrote {
		m.QuotaObserved++
	}
}

// Start runs the worker loop until ctx is done (first pass immediate).
func (w *UpstreamHealth) Start(ctx context.Context) {
	if m, err := w.RunPass(ctx); err != nil {
		log.Printf("WARN upstream health pass failed: %v", err)
	} else if m.Scanned > 0 {
		log.Printf("upstream health pass: %+v", m)
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m, err := w.RunPass(ctx)
			if err != nil {
				log.Printf("WARN upstream health pass failed: %v", err)
				continue
			}
			if m.CooledDown > 0 || m.Recovered > 0 || m.ReauthRequired > 0 || m.Errors > 0 {
				log.Printf("upstream health pass: %+v", m)
			}
		}
	}
}
