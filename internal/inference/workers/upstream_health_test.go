// upstream_health_test.go — Task 12 健康 worker 真库验收：额度观测
// （observed_at/source/reset_at，不可得保持未知）、401 → 即时刷新 /
// reauth 传播、可重试失败冷却与恢复、自托管服务认证适配器。

package workers

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

func (e refreshEnv) healthWorker() *UpstreamHealth {
	return NewUpstreamHealth(e.store, e.vault,
		&connector.Client{HTTP: e.vendor.srv.Client()}, e.refresher(), e.registry, e.store,
		UpstreamHealthConfig{Cooldown: 5 * time.Minute}, nil)
}

func TestUpstreamHealthObservesQuotaSnapshot(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Healthy != 1 || m.QuotaObserved != 1 {
		t.Fatalf("expected healthy+quota observed: %+v", m)
	}
	acct, err := env.store.GetUpstreamAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Quota.LimitMicros == nil || *acct.Quota.LimitMicros != 1000000 {
		t.Fatalf("quota limit not cached: %+v", acct.Quota)
	}
	if acct.Quota.RemainingMicros == nil || *acct.Quota.RemainingMicros != 400000 {
		t.Fatalf("quota remaining not cached: %+v", acct.Quota)
	}
	if acct.Quota.ObservedAt == nil || acct.Quota.Source == nil || *acct.Quota.Source != "reported" || acct.Quota.ResetAt == nil {
		t.Fatalf("observed_at/source/reset_at must be recorded: %+v", acct.Quota)
	}
	if acct.Status != domain.AccountActive {
		t.Fatalf("healthy account must stay active: %s", acct.Status)
	}
}

// 额度单调守卫：更旧的 observed_at 不得覆盖更新的观测。
func TestUpstreamHealthQuotaMonotonicObservedAt(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	now := time.Now().UTC()
	newer := domain.UpstreamQuota{ObservedAt: &now}
	limit := domain.Microcredit(1000)
	remain := domain.Microcredit(500)
	newer.LimitMicros, newer.RemainingMicros = &limit, &remain
	src := "reported"
	newer.Source = &src
	ok, err := env.store.UpdateUpstreamAccountQuota(ctx, accountID, newer)
	if err != nil || !ok {
		t.Fatalf("first write must land: %v %v", ok, err)
	}
	older := now.Add(-time.Hour)
	stale := domain.UpstreamQuota{ObservedAt: &older}
	zero := domain.Microcredit(0)
	stale.LimitMicros, stale.RemainingMicros = &zero, &zero
	stale.Source = &src
	ok, err = env.store.UpdateUpstreamAccountQuota(ctx, accountID, stale)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale observation must be rejected by the monotonic guard")
	}
	acct, _ := env.store.GetUpstreamAccount(ctx, accountID)
	if *acct.Quota.RemainingMicros != 500 {
		t.Fatalf("stale observation overwrote newer quota: %+v", acct.Quota)
	}
}

// 验收场景"限额耗尽"：观测到 remaining=0 且 reset 未到的账号不再被调度
// （路由侧跳过由 routing 包测试钉牢；这里钉持久层→调度读面的一致口径）。
func TestUpstreamHealthQuotaExhaustedInvisibleToScheduler(t *testing.T) {
	env := newRefreshEnv(t)
	env.seedExpiringCredential(t)
	ctx := context.Background()

	env.vendor.mu.Lock()
	env.vendor.quotaBody = `{"limit_micros":1000,"remaining_micros":0,"reset_at":"2999-01-01T00:00:00Z"}`
	env.vendor.mu.Unlock()

	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.QuotaObserved != 1 {
		t.Fatalf("expected quota observed: %+v", m)
	}
	acts, err := env.store.ListActiveUpstreamAccounts(ctx, env.provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("exhaustion keeps status active (信号在额度缓存): %d", len(acts))
	}
	if acts[0].Quota.RemainingMicros == nil || *acts[0].Quota.RemainingMicros != 0 {
		t.Fatalf("exhausted quota must be visible to routing: %+v", acts[0].Quota)
	}
}

// 健康 401 + 刷新成功：账号保持 active，凭据换代。
func TestUpstreamHealth401TriggersSuccessfulRefresh(t *testing.T) {
	env := newRefreshEnv(t)
	credID, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	env.vendor.mu.Lock()
	env.vendor.health = 401
	env.vendor.mu.Unlock()

	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReauthRequired != 0 {
		t.Fatalf("refreshable 401 must not reauth: %+v", m)
	}
	var gen int64
	var acctStatus string
	env.db.QueryRow(`SELECT generation FROM inference_credentials WHERE id = $1`, credID).Scan(&gen)
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	if gen != 2 || acctStatus != "active" {
		t.Fatalf("401+refresh must rotate and stay active: gen=%d acct=%s", gen, acctStatus)
	}
}

// 健康 401 + 刷新被厂商明确拒绝：reauth_required + 绑定终止 + 审计。
func TestUpstreamHealth401ReauthPropagates(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	binding := &domain.SessionBinding{
		SessionKey: "sess-health", ModelID: env.modelID, AccountID: accountID,
		Status: domain.BindingActive, ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := env.store.InsertSessionBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}

	env.vendor.mu.Lock()
	env.vendor.health = 401
	env.vendor.mu.Unlock()
	env.vendor.push(stubResponse{status: 400, body: `{"error":"invalid_grant"}`})

	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReauthRequired != 1 {
		t.Fatalf("expected reauth propagation: %+v", m)
	}
	var acctStatus, bindStatus string
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	env.db.QueryRow(`SELECT status FROM inference_session_bindings WHERE id = $1`, binding.ID).Scan(&bindStatus)
	if acctStatus != "reauth_required" || bindStatus != "ended" {
		t.Fatalf("acct=%s binding=%s", acctStatus, bindStatus)
	}
}

// 可重试失败（5xx）→ cooldown（停止调度）→ 冷却到期复测恢复 active。
func TestUpstreamHealthCooldownAndRecovery(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	env.vendor.mu.Lock()
	env.vendor.health = 503
	env.vendor.mu.Unlock()

	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.CooledDown != 1 {
		t.Fatalf("expected cooldown: %+v", m)
	}
	acts, err := env.store.ListActiveUpstreamAccounts(ctx, env.provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("cooling account must leave the pool: %d", len(acts))
	}

	// 冷却期内复测：该账号未被恢复（其他包的残留账号可能同轮被扫到，
	// 断言钉目标账号而非全局计数）。
	env.vendor.mu.Lock()
	env.vendor.health = 200
	env.vendor.mu.Unlock()
	if _, err := env.healthWorker().RunPass(ctx); err != nil {
		t.Fatal(err)
	}
	var midStatus string
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&midStatus)
	if midStatus != "cooldown" {
		t.Fatalf("account must stay cooling within the window: %s", midStatus)
	}

	// 回拨 updated_at 越过冷却期 → 复测恢复。
	if _, err := env.db.Exec(`UPDATE inference_upstream_accounts SET updated_at = now() - interval '10 minutes' WHERE id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	m3, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m3.Recovered != 1 {
		t.Fatalf("expected recovery: %+v", m3)
	}
	var status string
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&status)
	if status != "active" {
		t.Fatalf("recovered account must be active: %s", status)
	}
}

// 自托管服务认证：独立适配器，不走 OAuth。401 → reauth（静态凭据被上游
// 轮换）；200 → healthy。
func TestUpstreamHealthSelfHostedServiceAuth(t *testing.T) {
	env := newRefreshEnv(t)
	ctx := context.Background()

	// self_hosted provider + service credential + active deployment。
	if _, err := env.db.Exec(`INSERT INTO inference_providers (code, display_name, access_type)
		VALUES ('task12selfhost','Self Hosted','self_hosted') ON CONFLICT (code) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(`DELETE FROM inference_providers WHERE code = 'task12selfhost'`)
	var selfProv string
	env.db.Get(&selfProv, `SELECT id FROM inference_providers WHERE code = 'task12selfhost'`)
	if _, err := env.db.Exec(`INSERT INTO inference_deployments (provider_id, upstream_model, base_url, protocol, status)
		VALUES ($1,'m',$2,'openai_chat','active')`, selfProv, env.vendor.srv.URL); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(`DELETE FROM inference_deployments WHERE provider_id = $1`, selfProv)

	bundle := []byte("static-service-token")
	credID := "33333333-3333-4333-8333-333333333333"
	ct, kv, err := env.vault.Encrypt(credID, selfProv, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: credID, ProviderID: selfProv, Label: "svc", AuthType: "service",
		Ciphertext: ct, KeyVersion: kv, Generation: 1, Status: "active",
	}
	if err := env.store.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: selfProv, CredentialID: credID, DisplayName: "svc-acct",
		Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := env.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(`DELETE FROM inference_upstream_accounts WHERE id = $1`, acct.ID)
	defer env.db.Exec(`DELETE FROM inference_credentials WHERE id = $1`, credID)

	// 200 → healthy（且没有 OAuth 语义介入：connector 列保持空）。
	env.vendor.mu.Lock()
	env.vendor.health = 200
	env.vendor.mu.Unlock()
	m, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Healthy != 1 {
		t.Fatalf("self-hosted probe must report healthy: %+v", m)
	}
	var credConnector, acctStatus string
	env.db.QueryRow(`SELECT connector FROM inference_credentials WHERE id = $1`, credID).Scan(&credConnector)
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, acct.ID).Scan(&acctStatus)
	if credConnector != "" {
		t.Fatalf("self-hosted credential must not gain oauth semantics: %q", credConnector)
	}
	if acctStatus != "active" {
		t.Fatalf("healthy self-hosted account must stay active: %s", acctStatus)
	}

	// 401 → reauth_required（静态凭据被上游轮换/吊销）。
	env.vendor.mu.Lock()
	env.vendor.health = 401
	env.vendor.mu.Unlock()
	m2, err := env.healthWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m2.ReauthRequired != 1 {
		t.Fatalf("static credential rejection must reauth: %+v", m2)
	}
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, acct.ID).Scan(&acctStatus)
	if acctStatus != "reauth_required" {
		t.Fatalf("rejected static credential must stop scheduling: %s", acctStatus)
	}
}
