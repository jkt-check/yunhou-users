// isolation_test.go — R7-N3 故障隔离回归（gateway 级，真实 PostgreSQL +
// stub upstream）：单点故障（上游 5xx、凭据解密失败、账号冷却）只影响故障
// 模型/候选，健康模型照常服务、照常结算。
//
// 每个用例里模型 B 是一条完全独立的链路（独立 provider/account/credential/
// deployment/route），与模型 A 唯一的共享点是进程本身 —— B 的 200 SSE 证明
// 故障没有越界。

package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
)

// modelB 是隔离用例里的「健康模型」：独立链路，不得被模型 A 的任何故障波及。
const modelB = "glm-4.6-flash"

// failSecretFor 手搓 SecretResolver（CLAUDE.md：hand-rolled doubles）：对指定
// 凭据 ID 注入解密失败（轮换中/密钥损坏的形态），其余 ID 透传真实 credSvc。
type failSecretFor struct {
	badID string
	err   error
	inner SecretResolver
}

func (s failSecretFor) ResolveSecret(ctx context.Context, id string, pinGeneration *int64) ([]byte, *domain.Credential, error) {
	if id == s.badID {
		return nil, nil, s.err
	}
	return s.inner.ResolveSecret(ctx, id, pinGeneration)
}

// addModelB 在既有 fixture 上接入第二条完全独立的模型链路（provider B /
// account B / credential B / deployment B → up2），并把策略、权益、价目、
// 静态快照同步覆盖到 modelB。
func addModelB(t *testing.T, f *fixture, up2 *upstream) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: modelB, DisplayName: "GLM 4.6 Flash",
		ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("insert model B: %v", err)
	}
	prov2 := &domain.Provider{Code: "moonshot", DisplayName: "Moonshot",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := f.store.InsertProvider(ctx, prov2); err != nil {
		t.Fatalf("insert provider B: %v", err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	credSvc := credentials.NewService(vault, f.store, f.store)
	cv2, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov2.ID, "main", "api_key", "sk-model-b", "seed", nil)
	if err != nil {
		t.Fatalf("create credential B: %v", err)
	}
	acct2 := &domain.UpstreamAccount{
		ProviderID: prov2.ID, CredentialID: cv2.ID, DisplayName: "pool-b",
		Status: domain.AccountActive, ConcurrencyLimit: 8,
	}
	if err := f.store.InsertUpstreamAccount(ctx, acct2); err != nil {
		t.Fatalf("insert account B: %v", err)
	}
	dep2 := &domain.Deployment{
		ProviderID: prov2.ID, UpstreamModel: "glm-flash-up", BaseURL: up2.URL,
		Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := f.store.InsertDeployment(ctx, dep2); err != nil {
		t.Fatalf("insert deployment B: %v", err)
	}
	route2 := &domain.ModelRoute{ModelID: modelB, DeploymentID: dep2.ID, Weight: 1, Enabled: true}
	if err := f.store.InsertRoute(ctx, route2); err != nil {
		t.Fatalf("insert route B: %v", err)
	}
	// 权益覆盖：策略与既有赠送权益都扩展到 modelB（否则 B 被准入拒绝，
	// 测的就不是隔离而是授权）。
	if _, err := f.db.Exec(`UPDATE inference_policy_versions
		SET model_ids = array_append(model_ids, $1) WHERE id = $2`, modelB, f.policyID); err != nil {
		t.Fatalf("extend policy: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements
		SET model_ids = array_append(model_ids, $1) WHERE id = $2`, modelB, f.entID); err != nil {
		t.Fatalf("extend entitlement: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	if err := f.store.InsertPriceVersion(ctx, &postgres.PriceVersion{
		ModelID: modelB, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("insert price B: %v", err)
	}
	if err := f.store.InsertPriceVersion(ctx, &postgres.PriceVersion{
		ModelID: modelB, Kind: postgres.PriceUpstreamCost, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 500_000, OutputPerMtok: 1_000_000,
		Revision: 1, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("insert cost price B: %v", err)
	}
	// 快照 pin 同步（gateway 每请求只读快照，不读 DB 目录表）。
	f.snap.Models[modelB] = domain.Model{
		ID: modelB, DisplayName: "GLM 4.6 Flash", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:         []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities:   []string{"text"},
		OutputModalities:  []string{"text"},
		SupportsTools:     true,
		SupportsReasoning: true,
	}
	f.snap.Providers[prov2.ID] = *prov2
	f.snap.Deployments[dep2.ID] = *dep2
	f.snap.RoutesByModel[modelB] = []domain.ModelRoute{*route2}
}

// chatReqModel 是 chatReq 的模型参数化版本（chatReq 硬编码 fixture 模型）。
func chatReqModel(modelID string, stream bool) *providers.ChatRequest {
	req := chatReq(stream, "hi")
	req.Model = modelID
	return req
}

// assertModelBServes 断言健康模型 B 完成一次流式调用并正常结算 —— 每个隔离
// 用例的「故障未越界」证据。
func assertModelBServes(t *testing.T, f *fixture) {
	t.Helper()
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReqModel(modelB, true))
	if err != nil {
		t.Fatalf("model B must be unaffected, got: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	if !strings.Contains(body, `"content":"你好"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("model B stream = %s", body)
	}
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "reported" || settled != 76 {
		t.Errorf("model B request = %s/%s/%d, want settled/reported/76（照常结算）", status, usageStatus, settled)
	}
}

// Step 1a：模型 A 上游 500 → A 有界失败后 upstream_unavailable（502 类）；
// 模型 B 的独立链路照常 200 SSE。
func TestIsolation_Upstream500OnModelA_DoesNotAffectModelB(t *testing.T) {
	upA := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	upB := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, upA)
	addModelB(t, f, upB)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("model A err = %v, want upstream_unavailable", err)
	}
	if upA.calls.Load() != 1 {
		t.Errorf("model A upstream calls = %d, want 1（单候选有界，不重放）", upA.calls.Load())
	}
	assertModelBServes(t, f)
	if upB.calls.Load() != 1 {
		t.Errorf("model B upstream calls = %d, want 1", upB.calls.Load())
	}
}

// Step 1b：key 失效（A 唯一账号的凭据解密失败）→ A upstream_unavailable 且
// 零上游调用（秘钥解析在 dispatch 之前）；模型 B 凭据不同，照常服务。
func TestIsolation_CredentialFailureOnModelA_DoesNotAffectModelB(t *testing.T) {
	upA := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	upB := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, upA)
	addModelB(t, f, upB)

	var credA string
	if err := f.db.QueryRow(`SELECT credential_id FROM inference_upstream_accounts WHERE id = $1`,
		f.accountUpID).Scan(&credA); err != nil {
		t.Fatal(err)
	}
	gw := NewService(&staticSnapshot{f.snap}, f.store, f.gateway.entitlements, f.gateway.quotaSvc,
		f.routing, failSecretFor{badID: credA, err: errors.New("decrypt: bad key generation"), inner: f.gateway.secrets},
		f.gateway.client, f.gateway.egress, nil)

	p, key := f.principal()
	_, err := gw.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("model A err = %v, want upstream_unavailable", err)
	}
	if upA.calls.Load() != 0 {
		t.Errorf("model A upstream calls = %d, want 0（凭据失败不得触达上游）", upA.calls.Load())
	}
	attempts := f.attemptRows(t, mustRequestID(t, f.db))
	if len(attempts) != 1 || attempts[0].Status != "failed" || attempts[0].Kind != "credential" {
		t.Errorf("attempts = %+v, want one failed/credential attempt", attempts)
	}
	// B 走同一 stub resolver（凭据 ID 不同 → 透传真实 credSvc）。
	p2, key2 := f.principal()
	out, err := gw.ChatCompletions(context.Background(), p2, key2, domain.ProtocolOpenAIChat, chatReqModel(modelB, true))
	if err != nil {
		t.Fatalf("model B must be unaffected, got: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("model B stream = %s", body)
	}
}

// Step 1c：冷却中（A 的唯一账号在路由冷却记忆内）→ 候选为空，A
// upstream_unavailable 且零上游调用；冷却按账号隔离，B 的账号不受影响。
func TestIsolation_CooldownOnModelA_DoesNotAffectModelB(t *testing.T) {
	upA := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	upB := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, upA)
	addModelB(t, f, upB)

	f.routing.CooldownAfterFailure(f.accountUpID)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("model A err = %v, want upstream_unavailable（候选为空）", err)
	}
	if upA.calls.Load() != 0 {
		t.Errorf("model A upstream calls = %d, want 0（冷却账号不参与选择）", upA.calls.Load())
	}
	assertModelBServes(t, f)
}

// Step 3：单 deployment 凭据解密失败只淘汰该候选 —— 模型 A 挂两个
// deployment（bad 优先 / good 兜底），bad 的凭据解密失败 → failover 到 good
// 成功；bad 的上游零调用，客户端只结算一次。（单 deployment 模型的凭据失败
// 不波及其它模型，由 Step 1b 覆盖。）
func TestIsolation_CredentialFailure_FailsOverToHealthyDeployment(t *testing.T) {
	upBad := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	upGood := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, upBad)

	// 第二个 provider/account/credential/deployment/route，同模型，priority
	// 更高 = 后试（结构同 TestFailoverOn429）。
	ctx := context.Background()
	prov2 := &domain.Provider{Code: "moonshot", DisplayName: "Moonshot",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := f.store.InsertProvider(ctx, prov2); err != nil {
		t.Fatal(err)
	}
	vault, _ := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	credSvc := credentials.NewService(vault, f.store, f.store)
	cv2, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov2.ID, "main", "api_key", "sk-2", "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct2 := &domain.UpstreamAccount{ProviderID: prov2.ID, CredentialID: cv2.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 4}
	if err := f.store.InsertUpstreamAccount(ctx, acct2); err != nil {
		t.Fatal(err)
	}
	dep2 := &domain.Deployment{
		ProviderID: prov2.ID, UpstreamModel: "kimi-k2", BaseURL: upGood.URL,
		Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second, Status: domain.DeploymentActive,
	}
	if err := f.store.InsertDeployment(ctx, dep2); err != nil {
		t.Fatal(err)
	}
	route2 := &domain.ModelRoute{ModelID: f.modelID, DeploymentID: dep2.ID, Priority: 2, Weight: 1, Enabled: true}
	if err := f.store.InsertRoute(ctx, route2); err != nil {
		t.Fatal(err)
	}
	f.snap.Providers[prov2.ID] = *prov2
	f.snap.Deployments[dep2.ID] = *dep2
	f.snap.RoutesByModel[f.modelID] = append(f.snap.RoutesByModel[f.modelID], *route2)

	// bad 候选 = fixture 原有链路的凭据。
	var credBad string
	if err := f.db.QueryRow(`SELECT credential_id FROM inference_upstream_accounts WHERE id = $1`,
		f.accountUpID).Scan(&credBad); err != nil {
		t.Fatal(err)
	}
	gw := NewService(&staticSnapshot{f.snap}, f.store, f.gateway.entitlements, f.gateway.quotaSvc,
		f.routing, failSecretFor{badID: credBad, err: errors.New("decrypt: bad key generation"), inner: f.gateway.secrets},
		f.gateway.client, f.gateway.egress, nil)

	p, key := f.principal()
	out, err := gw.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("failover to the healthy deployment must succeed, got: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream = %s", body)
	}
	if upBad.calls.Load() != 0 {
		t.Errorf("bad upstream calls = %d, want 0（凭据失败在 dispatch 前短路）", upBad.calls.Load())
	}
	if upGood.calls.Load() != 1 {
		t.Errorf("good upstream calls = %d, want 1（failover 恰好一次）", upGood.calls.Load())
	}
	attempts := f.attemptRows(t, out.RequestID)
	if len(attempts) != 2 || attempts[0].Status != "failed" || attempts[0].Kind != "credential" ||
		attempts[1].Status != "completed" {
		t.Errorf("attempts = %+v, want [failed/credential, completed]", attempts)
	}
	if charge := f.ledgerCharge(t, out.RequestID); charge != 76 {
		t.Errorf("charge = %d, want exactly one settlement of 76（重试不重复扣费）", charge)
	}
}
