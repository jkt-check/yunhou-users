package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/model"
)

// sticky_candidates_test.go — Task 16 覆盖率补强：pinSessionCandidates 的
// 绑定/解析/迁移分支（真实库 SessionBinder）与小工具函数。

func stickyFixture(t *testing.T) (*fixture, []routing.Candidate) {
	t.Helper()
	f := newFixture(t, newUpstream(t, sseHandler(
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"data: [DONE]",
	)))
	cands := []routing.Candidate{{
		Deployment: f.snap.Deployments[f.deploymentID],
		Account:    domain.UpstreamAccount{ID: f.accountUpID, ConcurrencyLimit: 8},
	}}
	return f, cands
}

func TestPinSessionCandidates_NotConfigured(t *testing.T) {
	f, cands := stickyFixture(t)
	// 未 SetSessionBinder → 显式内部错误（不静默换号也不 panic）。
	_, err := f.gateway.pinSessionCandidates(context.Background(), "sess-x", f.modelID, cands, nil)
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("err = %v, want internal (sticky not configured)", err)
	}
}

func TestPinSessionCandidates_BindResolveMigrate(t *testing.T) {
	f, cands := stickyFixture(t)
	ctx := context.Background()
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))

	// 首轮：无绑定 → 绑定到首选账号并钉住。
	pinned, err := f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != 1 || pinned[0].Account.ID != f.accountUpID {
		t.Fatalf("first-turn pin = %+v", pinned)
	}
	// 数据库层面有活跃绑定（归属校验的事实源）。
	b, _, err := routing.NewSessionBinder(f.store, nil).Resolve(ctx, "sess-1", f.modelID)
	if err != nil || b.AccountID != f.accountUpID {
		t.Fatalf("binding = %+v/%v", b, err)
	}

	// 次轮：已有绑定 → 直接钉住（不重复绑定）。
	pinned, err = f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands, nil)
	if err != nil || len(pinned) != 1 {
		t.Fatalf("re-pin = %+v/%v", pinned, err)
	}

	// 会话不同 → 独立绑定（失效前完成，避免候选全失真的路径混合）。
	pinned2, err := f.gateway.pinSessionCandidates(ctx, "sess-2", f.modelID, cands, nil)
	if err != nil || len(pinned2) != 1 {
		t.Fatalf("second session pin = %+v/%v", pinned2, err)
	}

	// 绑定失效（账号被禁用）→ 不得为失效绑定静默服务：显式迁移或明确
	// upstream_unavailable（Bind/Migrate 对不可调度账号 fail-closed）。
	if _, err := f.store.SetUpstreamAccountStatusConditional(ctx, f.accountUpID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountDisabled); err != nil {
		t.Fatal(err)
	}
	pinned, err = f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands, nil)
	if err == nil && len(pinned) == 1 && pinned[0].Account.ID == f.accountUpID {
		t.Fatal("disabled bound account must not keep serving silently")
	}
}

func TestFilterCandidatesByAccount(t *testing.T) {
	mk := func(id string) routing.Candidate {
		return routing.Candidate{Account: domain.UpstreamAccount{ID: id}}
	}
	cands := []routing.Candidate{mk("a"), mk("b"), mk("a")}
	out := filterCandidatesByAccount(cands, "a")
	if len(out) != 2 || out[0].Account.ID != "a" || out[1].Account.ID != "a" {
		t.Fatalf("filter = %+v", out)
	}
	if got := filterCandidatesByAccount(cands, "zzz"); len(got) != 0 {
		t.Fatalf("filter none = %+v", got)
	}
}

func TestErrorsAsDispatch(t *testing.T) {
	var de *providers.DispatchError
	if !errorsAsDispatch(&providers.DispatchError{StatusCode: 500}, &de) || de == nil {
		t.Fatal("dispatch error not recognized")
	}
	if errorsAsDispatch(domain.NewError(domain.CodeInternal, "x"), &de) {
		t.Fatal("non-dispatch error misclassified")
	}
}

func TestBindSession_RequiresBinder(t *testing.T) {
	f := newFixture(t, newUpstream(t, sseHandler(
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"data: [DONE]",
	)))
	if err := f.gateway.BindSession(context.Background(), "s", f.modelID, uuid.NewString()); err == nil ||
		domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("BindSession without binder = %v, want internal", err)
	}
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))
	if err := f.gateway.BindSession(context.Background(), "s", f.modelID, f.accountUpID); err != nil {
		t.Fatalf("BindSession = %v", err)
	}
}

func TestEstimateInputTokens_ToolPayloads(t *testing.T) {
	// 工具定义/工具调用计入估算（bytes/2 + 16 口径）。
	req := &providers.ChatRequest{
		Messages: []model.ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "c1", Type: "function",
				Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`},
			}}},
		},
		Tools:      []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)},
		ToolChoice: json.RawMessage(`"auto"`),
	}
	plain := &providers.ChatRequest{Messages: []model.ChatMessage{{Role: "user", Content: "hi"}}}
	if EstimateInputTokens(req) <= EstimateInputTokens(plain) {
		t.Errorf("tool-bearing estimate %d <= plain %d", EstimateInputTokens(req), EstimateInputTokens(plain))
	}
}

func TestEstimateInputTokens_ImageParts(t *testing.T) {
	plain := &providers.ChatRequest{Messages: []model.ChatMessage{{Role: "user", Content: "hi"}}}
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`)); err != nil {
		t.Fatal(err)
	}
	withImg := &providers.ChatRequest{Messages: []model.ChatMessage{m}}
	if got := EstimateInputTokens(withImg) - EstimateInputTokens(plain); got != 1105 {
		t.Fatalf("image estimate delta=%d want 1105", got)
	}
}

// ---------------------------------------------------------------------------
// T3 — provider 停用与 sticky 候选一致性
//
// sticky 候选必须复用普通候选的同一个「已发布 route/provider/deployment
// 可调度」判定：被停用的 provider 不得经由 sticky 会话出站，也不得由
// sticky 分支另开后门；另一个 active provider 仍然可用。
// ---------------------------------------------------------------------------

// addSecondProvider 给 fixture 追加第二个 provider/deployment/account/route
// （同一模型、更高 priority），返回新账号 ID。它让测试能证明「停用一个
// provider 不得停掉整个模型」。
func addSecondProvider(t *testing.T, f *fixture, up *upstream) string {
	t.Helper()
	ctx := context.Background()
	prov := &domain.Provider{Code: "moonshot", DisplayName: "Moonshot",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := f.store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	credSvc := credentials.NewService(vault, f.store, f.store)
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov.ID, "main", "api_key", "sk-second", "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{ProviderID: prov.ID, CredentialID: cv.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 4}
	if err := f.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "kimi-k2", BaseURL: up.URL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := f.store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	route := &domain.ModelRoute{ModelID: f.modelID, DeploymentID: dep.ID,
		Priority: 2, Weight: 1, Enabled: true}
	if err := f.store.InsertRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	f.snap.Providers[prov.ID] = *prov
	f.snap.Deployments[dep.ID] = *dep
	f.snap.RoutesByModel[f.modelID] = append(f.snap.RoutesByModel[f.modelID], *route)
	return acct.ID
}

// disableFixtureProvider 在已发布快照里停用 fixture 自带的 provider。
func disableFixtureProvider(f *fixture) {
	prov := f.snap.Providers[f.providerID]
	prov.Status = "disabled"
	f.snap.Providers[f.providerID] = prov
}

// TestGateway_DisabledProviderNeverDispatches：已发布快照里 provider 被
// 停用后，它的部署不得出站；同一模型的另一个 active provider 仍然可用。
func TestGateway_DisabledProviderNeverDispatches(t *testing.T) {
	up1 := newUpstream(t, completionHandler)
	up2 := newUpstream(t, completionHandler)
	f := newFixture(t, up1)
	addSecondProvider(t, f, up2)
	disableFixtureProvider(f)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	if up1.calls.Load() != 0 {
		t.Errorf("disabled provider received %d upstream calls, want 0", up1.calls.Load())
	}
	if up2.calls.Load() != 1 {
		t.Errorf("active provider received %d upstream calls, want 1", up2.calls.Load())
	}
}

// TestGateway_StickyCandidates_DisabledProviderNotDispatched：sticky 会话
// 绑在停用 provider 的账号上 → 不得出站；绑定失效也不得静默切到别的账号
// （sticky 契约：绝不无条件切号续接）。
func TestGateway_StickyCandidates_DisabledProviderNotDispatched(t *testing.T) {
	up1 := newUpstream(t, completionHandler)
	up2 := newUpstream(t, completionHandler)
	f := newFixture(t, up1)
	addSecondProvider(t, f, up2)
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))

	// 会话绑定到 fixture provider（随后被停用）的账号。
	if err := f.gateway.BindSession(context.Background(), "sess-1", f.modelID, f.accountUpID); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	disableFixtureProvider(f)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletionsSticky(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"), "sess-1")
	if err == nil {
		t.Fatal("a sticky session bound to a disabled provider must not be served")
	}
	if code := domain.CodeOf(err); code != domain.CodeUpstreamUnavailable {
		t.Fatalf("code = %v, want upstream_unavailable（不得伪装成无权限）: %v", code, err)
	}
	if up1.calls.Load() != 0 || up2.calls.Load() != 0 {
		t.Errorf("upstream calls = %d/%d, want 0/0（停用的 provider 不得出站，绑定也不得静默切号）",
			up1.calls.Load(), up2.calls.Load())
	}
}

// TestGateway_StickyCandidates_NewSessionBindsActiveProvider：停用一个
// provider 后，新会话仍然绑到另一个 active provider 的账号并出站（sticky
// 与普通候选共用同一个可调度判定，不因 sticky 另开后门）。
func TestGateway_StickyCandidates_NewSessionBindsActiveProvider(t *testing.T) {
	up1 := newUpstream(t, completionHandler)
	up2 := newUpstream(t, completionHandler)
	f := newFixture(t, up1)
	acct2 := addSecondProvider(t, f, up2)
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))
	disableFixtureProvider(f)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletionsSticky(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"), "sess-new"); err != nil {
		t.Fatalf("ChatCompletionsSticky: %v", err)
	}
	if up1.calls.Load() != 0 {
		t.Errorf("disabled provider received %d upstream calls, want 0", up1.calls.Load())
	}
	if up2.calls.Load() != 1 {
		t.Errorf("active provider received %d upstream calls, want 1", up2.calls.Load())
	}
	binding, _, err := routing.NewSessionBinder(f.store, nil).Resolve(context.Background(), "sess-new", f.modelID)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	if binding.AccountID != acct2 {
		t.Errorf("session bound to %s, want the active provider's account %s", binding.AccountID, acct2)
	}
}

// TestGateway_AliasToDisabledProviderNotDispatched：模型别名解析到同一个
// 模型 → 停用的 provider 不得经由别名出站，另一个 active provider 仍可用。
func TestGateway_AliasToDisabledProviderNotDispatched(t *testing.T) {
	up1 := newUpstream(t, completionHandler)
	up2 := newUpstream(t, completionHandler)
	f := newFixture(t, up1)
	addSecondProvider(t, f, up2)
	// 别名指向同一个模型（发布快照里的 aliases）。
	m := f.snap.Models[f.modelID]
	m.Aliases = []string{"glm-latest"}
	f.snap.Models[f.modelID] = m
	disableFixtureProvider(f)

	req := chatReq(false, "hi")
	req.Model = "glm-latest"
	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, req); err != nil {
		t.Fatalf("ChatCompletions (alias): %v", err)
	}
	if up1.calls.Load() != 0 {
		t.Errorf("disabled provider received %d upstream calls via alias, want 0", up1.calls.Load())
	}
	if up2.calls.Load() != 1 {
		t.Errorf("active provider received %d upstream calls, want 1", up2.calls.Load())
	}
}


// ---------------------------------------------------------------------------
// T3 补正 — 停用 provider 的 503 文案不得误述为「容量不足 / 稍后重试」
//
// provider 被停用不是容量缺口，也不是瞬时故障：它只在重新启用（或换会话
// 键）之后才会变化。文案若仍说「with capacity」或「retry shortly」，客户端
// 会对一个不会自行恢复的条件发起重试循环。
// 错误码（upstream_unavailable）与 HTTP 503 形状不变，也不新增任何线上
// 错误码或响应字段。
// ---------------------------------------------------------------------------

// clientVisibleMessage 取客户端真正看到的那段文案（httpapi.safeMsg 取的
// 就是 domain.Error.Message）。
func clientVisibleMessage(t *testing.T, err error) string {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want a domain error", err)
	}
	return de.Message
}

// assertNonTransientRejection 钉住「非容量 / 非瞬时 / 不邀请重试」的口径。
func assertNonTransientRejection(t *testing.T, msg, modelID string) {
	t.Helper()
	lower := strings.ToLower(msg)
	for _, bad := range []string{"capacity", "retry", "temporar"} {
		if strings.Contains(lower, bad) {
			t.Errorf("message %q must not mention %q（停用不是容量缺口，也不是瞬时故障）", msg, bad)
		}
	}
	if !strings.Contains(msg, modelID) {
		t.Errorf("message %q must still name the model %q", msg, modelID)
	}
	if !strings.Contains(lower, "dispatchable") {
		t.Errorf("message %q must state that no upstream is dispatchable", msg)
	}
}

// TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient：普通
// 路径上 provider 被停用时的 503 文案——不得说成容量不足、不得邀请重试；
// 错误码与 503 形状不变，零出站。
func TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient(t *testing.T) {
	up := newUpstream(t, completionHandler)
	f := newFixture(t, up)
	disableFixtureProvider(f)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("a disabled provider must fail the call")
	}
	if code := domain.CodeOf(err); code != domain.CodeUpstreamUnavailable {
		t.Fatalf("code = %v, want upstream_unavailable（错误码与 503 形状不变）: %v", code, err)
	}
	assertNonTransientRejection(t, clientVisibleMessage(t, err), f.modelID)
	if up.calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", up.calls.Load())
	}
}

// TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient：
// sticky 路径上绑定到停用 provider 账号时的 503 文案——同样不得说成瞬时/
// 容量问题、不得邀请重试，并且必须说明会话没有被改绑（否则客户端会对同一个
// 永不自行恢复的会话键反复重试）。不换号的行为与错误码保持不变。
func TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient(t *testing.T) {
	up1 := newUpstream(t, completionHandler)
	up2 := newUpstream(t, completionHandler)
	f := newFixture(t, up1)
	addSecondProvider(t, f, up2)
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))
	if err := f.gateway.BindSession(context.Background(), "sess-1", f.modelID, f.accountUpID); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	disableFixtureProvider(f)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletionsSticky(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"), "sess-1")
	if err == nil {
		t.Fatal("a sticky session bound to a disabled provider must not be served")
	}
	if code := domain.CodeOf(err); code != domain.CodeUpstreamUnavailable {
		t.Fatalf("code = %v, want upstream_unavailable（错误码与 503 形状不变）: %v", code, err)
	}
	msg := clientVisibleMessage(t, err)
	assertNonTransientRejection(t, msg, f.modelID)
	if !strings.Contains(strings.ToLower(msg), "rebound") {
		t.Errorf("message %q must state that the session was not rebound to another account", msg)
	}
	// 不换号：停用的 provider 与另一个 provider 都不得出站。
	if up1.calls.Load() != 0 || up2.calls.Load() != 0 {
		t.Errorf("upstream calls = %d/%d, want 0/0", up1.calls.Load(), up2.calls.Load())
	}
}
