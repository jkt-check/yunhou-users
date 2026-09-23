package workers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// upstream_health_edges_test.go — Task 16 覆盖率补强：健康探测的分支
// （吊销凭据对齐、未注册 connector、official_api 跳过、自托管 401 →
// reauth 传播 + 绑定终止）。主路径已在 upstream_health_test.go。

func healthWorkerFor(env refreshEnv) *UpstreamHealth {
	return NewUpstreamHealth(env.store, env.vault,
		&connector.Client{HTTP: env.vendor.srv.Client()}, env.refresher(), env.registry, env.store,
		UpstreamHealthConfig{Cooldown: 5 * time.Minute}, nil)
}

// 凭据已吊销：探测只对齐账号为 disabled（不重复 reauth 传播）。
func TestUpstreamHealthRevokedCredentialAlignsDisabled(t *testing.T) {
	env := newRefreshEnv(t)
	credID, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()
	if _, err := env.db.ExecContext(ctx,
		`UPDATE inference_credentials SET status = 'revoked' WHERE id = $1`, credID); err != nil {
		t.Fatal(err)
	}
	m, err := healthWorkerFor(env).RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReauthRequired != 0 {
		t.Errorf("revoked credential must not re-auth-propagate: %+v", m)
	}
	acct, err := env.store.GetUpstreamAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Status != domain.AccountDisabled {
		t.Errorf("account = %s, want disabled (对齐吊销凭据)", acct.Status)
	}
}

// 凭据 connector 未注册 → Errors（配置漂移响亮可见，不改动账号状态）。
func TestUpstreamHealthUnregisteredConnectorIsError(t *testing.T) {
	env := newRefreshEnv(t)
	credID, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()
	if _, err := env.db.ExecContext(ctx,
		`UPDATE inference_credentials SET connector = 'ghost-vendor' WHERE id = $1`, credID); err != nil {
		t.Fatal(err)
	}
	m, err := healthWorkerFor(env).RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Errors == 0 {
		t.Errorf("unregistered connector must count as error: %+v", m)
	}
	acct, err := env.store.GetUpstreamAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Status != domain.AccountActive {
		t.Errorf("account = %s, want unchanged (配置错误不改动账号)", acct.Status)
	}
}

// official_api 静态凭据：主动探测跳过（健康由真实调度错误信号覆盖）。
func TestUpstreamHealthStaticOfficialSkipped(t *testing.T) {
	env := newRefreshEnv(t)
	ctx := context.Background()
	credID := uuid.NewString()
	ct, kv, err := env.vault.Encrypt(credID, env.provider, []byte("sk-static"))
	if err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: credID, ProviderID: env.provider, Label: "static", AuthType: "api_key",
		Ciphertext: ct, KeyVersion: kv, Generation: 1, Status: "active",
	}
	if err := env.store.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: env.provider, CredentialID: credID, Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := env.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	m, err := healthWorkerFor(env).RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Healthy != 0 || m.CooledDown != 0 || m.Errors != 0 {
		t.Errorf("official_api static probe must skip silently: %+v", m)
	}
	got, err := env.store.GetUpstreamAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AccountActive {
		t.Errorf("account = %s, want unchanged", got.Status)
	}
}

// 自托管静态凭据被上游 401 拒绝 → reauth_required + 会话绑定终止（审计）。
func TestUpstreamHealthSelfHosted401PropagatesReauth(t *testing.T) {
	env := newRefreshEnv(t)
	env.vendor.mu.Lock()
	env.vendor.health = 401
	env.vendor.mu.Unlock()
	ctx := context.Background()

	// 自托管 provider + active 部署（探测目标）。
	if _, err := env.db.ExecContext(ctx,
		`UPDATE inference_providers SET access_type = 'self_hosted' WHERE id = $1`, env.provider); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: env.provider, UpstreamModel: "oss", BaseURL: env.vendor.srv.URL,
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: 2 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := env.store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	credID := uuid.NewString()
	ct, kv, err := env.vault.Encrypt(credID, env.provider, []byte("sk-selfhosted"))
	if err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: credID, ProviderID: env.provider, Label: "selfhosted", AuthType: "service",
		Ciphertext: ct, KeyVersion: kv, Generation: 1, Status: "active",
	}
	if err := env.store.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: env.provider, CredentialID: credID, Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := env.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	// 活跃会话绑定（失效传播必须终止它）。
	now := time.Now().UTC()
	if err := env.store.InsertSessionBinding(ctx, &domain.SessionBinding{
		SessionKey: "sess-x", ModelID: env.modelID, AccountID: acct.ID,
		BoundAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	m, err := healthWorkerFor(env).RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReauthRequired != 1 {
		t.Fatalf("self-hosted 401 = %+v, want 1 reauth", m)
	}
	acct2, err := env.store.GetUpstreamAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct2.Status != domain.AccountReauthRequired {
		t.Errorf("account = %s, want reauth_required", acct2.Status)
	}
	if _, err := env.store.GetActiveSessionBinding(ctx, "sess-x", env.modelID, time.Now().UTC()); err == nil {
		t.Error("session binding must be terminated on reauth propagation")
	}
	var audit int
	if err := env.db.Get(&audit,
		`SELECT COUNT(*) FROM inference_audit_log WHERE action LIKE '%reauth%'`); err != nil {
		t.Fatal(err)
	}
	if audit == 0 {
		t.Error("reauth propagation must be audited")
	}
}
