package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

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
	_, err := f.gateway.pinSessionCandidates(context.Background(), "sess-x", f.modelID, cands)
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("err = %v, want internal (sticky not configured)", err)
	}
}

func TestPinSessionCandidates_BindResolveMigrate(t *testing.T) {
	f, cands := stickyFixture(t)
	ctx := context.Background()
	f.gateway.SetSessionBinder(routing.NewSessionBinder(f.store, nil))

	// 首轮：无绑定 → 绑定到首选账号并钉住。
	pinned, err := f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands)
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
	pinned, err = f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands)
	if err != nil || len(pinned) != 1 {
		t.Fatalf("re-pin = %+v/%v", pinned, err)
	}

	// 会话不同 → 独立绑定（失效前完成，避免候选全失真的路径混合）。
	pinned2, err := f.gateway.pinSessionCandidates(ctx, "sess-2", f.modelID, cands)
	if err != nil || len(pinned2) != 1 {
		t.Fatalf("second session pin = %+v/%v", pinned2, err)
	}

	// 绑定失效（账号被禁用）→ 不得为失效绑定静默服务：显式迁移或明确
	// upstream_unavailable（Bind/Migrate 对不可调度账号 fail-closed）。
	if _, err := f.store.SetUpstreamAccountStatusConditional(ctx, f.accountUpID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountDisabled); err != nil {
		t.Fatal(err)
	}
	pinned, err = f.gateway.pinSessionCandidates(ctx, "sess-1", f.modelID, cands)
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
