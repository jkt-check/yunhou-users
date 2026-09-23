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
	// EndExpiredSessionBindings sweeps TTL-expired live bindings (审查修复
	// M-1：此前无生产调用方，本 worker 每轮清扫).
	EndExpiredSessionBindings(ctx context.Context, now time.Time, limit int) (int64, error)
	// DeleteExpiredResponseChains sweeps TTL-expired Responses 会话链行
	// (Task 13; 与绑定清扫同轮次).
	DeleteExpiredResponseChains(ctx context.Context, now time.Time, limit int) (int64, error)
	// DeleteTerminalLeases sweeps 终态租约行（released/expired 且终态时间
	// 早于 cutoff；评审修复批次8：准入热路径不再背负 scope 全历史行）.
	DeleteTerminalLeases(ctx context.Context, cutoff time.Time, limit int) (int64, error)
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
	// LeaseRetention is how long terminal (released/expired) concurrency
	// leases are kept before the reaper deletes them (default 7d; fencing
	// 单调性只需对 held 行成立，终态行只服务短期排查).
	LeaseRetention time.Duration
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
	if out.LeaseRetention <= 0 {
		out.LeaseRetention = 7 * 24 * time.Hour
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
	// BindingsExpired: TTL 到期清扫的活跃绑定数（审查修复 M-1）。
	BindingsExpired int64 `json:"bindings_expired"`
	// ChainsExpired: TTL 到期清扫的 Responses 会话链行数（Task 13）。
	ChainsExpired int64 `json:"chains_expired"`
	// LeasesReaped: 本轮收割的终态租约行数（评审修复批次8）。
	LeasesReaped int64 `json:"leases_reaped"`
}

// RunPass executes one health round: first sweep TTL-expired session
// bindings, response chains and terminal leases, then probe accounts.
func (w *UpstreamHealth) RunPass(ctx context.Context) (UpstreamHealthMetrics, error) {
	var m UpstreamHealthMetrics
	expired, err := w.store.EndExpiredSessionBindings(ctx, w.clock.Now(), w.cfg.BatchLimit)
	if err != nil {
		return m, err
	}
	m.BindingsExpired = expired
	chains, err := w.store.DeleteExpiredResponseChains(ctx, w.clock.Now(), w.cfg.BatchLimit)
	if err != nil {
		return m, err
	}
	m.ChainsExpired = chains
	leases, err := w.store.DeleteTerminalLeases(ctx, w.clock.Now().Add(-w.cfg.LeaseRetention), w.cfg.BatchLimit)
	if err != nil {
		return m, err
	}
	m.LeasesReaped = leases
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
		case outcome.Rotated || outcome.Converged:
			// 凭据已更新（或他实例已处理）→ 保持 active。
			w.onHealthy(ctx, a, m)
		default:
			// 审查修复 I-1：no-op = 无 refresh token 可轮换——401 是确定性
			// 失效信号，不能让账号持死 token 保持 active 可调度。走与
			// invalid_grant 同一守卫传播路径。
			outcome, merr := w.refresher.MarkReauthRequired(ctx, cred.ID, "health probe 401 (no refresh token)", err)
			switch {
			case merr != nil:
				m.Errors++
				log.Printf("WARN upstream health: reauth propagation failed account=%s: %v", a.ID, merr)
			case outcome.ReauthRequired:
				m.ReauthRequired++
			default: // converged：他实例已换代/处理
				w.onHealthy(ctx, a, m)
			}
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
			log.Printf("ERROR upstream health: reauth propagation failed account=%s: %v", a.ID, rerr)
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
		// 同事务审计 fail-closed（对齐 bulk_import，审查修复 Important-2）：
		// 记录器不支持事务写入时整个翻转失败回滚——账号翻转 + 绑定终止与
		// 审计同生共死，绝不静默提交无审计的状态变更。
		rec, ok := w.audit.(management.AuditTxRecorder)
		if !ok {
			log.Printf("ERROR upstream health: audit recorder lacks transactional support — reauth propagation aborted account=%s", a.ID)
			return domain.NewError(domain.CodeInternal, "upstream health: audit recorder lacks transactional support")
		}
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
// runGuarded 兜底每轮 pass 的 panic（审查修复 Important-1：裸 goroutine
// worker 的 panic 不再终止 API 进程）。
func (w *UpstreamHealth) Start(ctx context.Context) {
	var m UpstreamHealthMetrics
	pass := func() error {
		m = UpstreamHealthMetrics{}
		var err error
		m, err = w.RunPass(ctx)
		return err
	}
	if err := runGuarded("upstream health", pass); err != nil {
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
			if err := runGuarded("upstream health", pass); err != nil {
				log.Printf("WARN upstream health pass failed: %v", err)
				continue
			}
			if m.CooledDown > 0 || m.Recovered > 0 || m.ReauthRequired > 0 || m.Errors > 0 || m.BindingsExpired > 0 || m.LeasesReaped > 0 {
				log.Printf("upstream health pass: %+v", m)
			}
		}
	}
}
