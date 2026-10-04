package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/model"
)

// mockLLMUsageRepo records InsertEvent calls for assertions.
type mockLLMUsageRepo struct {
	events []model.LLMUsageEvent
	err    error
}

func (m *mockLLMUsageRepo) InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error {
	if m.err != nil {
		return m.err
	}
	m.events = append(m.events, ev)
	return nil
}

func (m *mockLLMUsageRepo) SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	return nil, nil
}

// testCatalog builds a single-provider catalog pointing at baseURL
// (typically an httptest server).
func testCatalog(baseURL string) *llm.Catalog {
	return &llm.Catalog{
		DefaultModel: "deepseek-flash",
		Providers: map[string]llm.Provider{
			"deepseek": {Protocol: llm.ProtocolOpenAI, BaseURL: baseURL, APIKeys: []string{"test-key"}},
		},
		Models: map[string]llm.Model{
			"deepseek-flash": {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", InputPerMtok: 2, OutputPerMtok: 8},
		},
	}
}

// chatTestFixture wires a ChatService with mock repos and an optional
// upstream stub.
func chatTestFixture(t *testing.T, upstream http.Handler) (*ChatService, *mockSubscriptionRepo, *mockPlanRepo, *mockLLMUsageRepo) {
	t.Helper()
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	usageRepo := &mockLLMUsageRepo{}
	baseURL := "https://upstream.invalid"
	var srv *httptest.Server
	if upstream != nil {
		srv = httptest.NewServer(upstream)
		t.Cleanup(srv.Close)
		baseURL = srv.URL
	}
	svc := NewChatService(testCatalog(baseURL), subRepo, planRepo, usageRepo)
	return svc, subRepo, planRepo, usageRepo
}

// seedChatActiveSub adds an active, non-expired subscription for userID
// pointing at planID.
func seedChatActiveSub(repo *mockSubscriptionRepo, userID, planID string) {
	now := time.Now()
	future := now.Add(30 * 24 * time.Hour)
	repo.byUserID[userID] = &model.Subscription{
		ID:        "sub-" + userID,
		UserID:    userID,
		PlanID:    planID,
		Status:    "active",
		StartedAt: now,
		ExpiresAt: &future,
	}
}

func chatMessages() []model.ChatMessage {
	return []model.ChatMessage{{Role: "user", Content: "hi"}}
}

func TestChatService_NotEnabled(t *testing.T) {
	svc := NewChatService(nil, newMockSubscriptionRepo(), newMockPlanRepo(), &mockLLMUsageRepo{})
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want ErrChatNotEnabled", err)
	}
}

func TestChatService_UnknownModel(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	_, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "ghost-model", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUnknownModel) {
		t.Fatalf("err = %v, want ErrChatUnknownModel", err)
	}
	if route != nil {
		t.Errorf("route = %+v, want nil — 解析从未发生，handler 回退裸客户端值", route)
	}
}

// TestChatService_ErrorReturnsResolvedRoute: per the ChatStreamer contract,
// errors AFTER model resolution carry the resolved route, so the handler's
// audit line attributes the failure to the effective model (旧客户端不带
// model → 默认模型 id)。
func TestChatService_ErrorReturnsResolvedRoute(t *testing.T) {
	// Access denied (no subscription) — resolution succeeded, gate failed.
	svc, _, _, _ := chatTestFixture(t, nil)
	_, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("err = %v, want ErrChatNoAccess", err)
	}
	if route == nil || route.LogicalModel != "deepseek-flash" {
		t.Errorf("route = %+v, want resolved LogicalModel deepseek-flash", route)
	}

	// Upstream unreachable (gate passed) — same contract on the post-gate
	// failure path.
	svc2, subRepo2, planRepo2, _ := chatTestFixture(t, nil)
	seedChatActiveSub(subRepo2, "u-1", "monthly")
	planRepo2.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	_, route2, err2 := svc2.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err2, ErrChatUpstreamError) {
		t.Fatalf("err = %v, want ErrChatUpstreamError", err2)
	}
	if route2 == nil || route2.LogicalModel != "deepseek-flash" {
		t.Errorf("route = %+v, want resolved LogicalModel deepseek-flash", route2)
	}
}

func TestChatService_ModelNotInPlanAllowlist(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}, ChatModels: pq.StringArray{"kimi-k3"}}
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "deepseek-flash", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatModelNotAllowed) {
		t.Fatalf("err = %v, want ErrChatModelNotAllowed", err)
	}
	// Same plan, allowed model passes the gate (fails later at the
	// unreachable upstream, which proves the gate didn't block it).
	planRepo.plans["monthly"].ChatModels = pq.StringArray{"deepseek-flash"}
	_, _, err = svc.StreamChat(context.Background(), "u-1", "yunhou-website", "deepseek-flash", chatMessages(), nil, nil)
	if errors.Is(err, ErrChatModelNotAllowed) || errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("err = %v, want upstream-level error (gate passed)", err)
	}
}

func TestChatService_AccessGating(t *testing.T) {
	now := time.Now()
	activePlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	inactivePlan := &model.Plan{ID: "retired", IsActive: false, Apps: pq.StringArray{"yunhou-website"}}
	wrongAppPlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}

	cases := []struct {
		name    string
		subRepo *mockSubscriptionRepo
		plan    *model.Plan
		wantErr error
	}{
		{"no subscription", newMockSubscriptionRepo(), activePlan, ErrChatNoAccess},
		{"expired subscription", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			expiredAt := now.Add(-1 * time.Hour)
			r.byUserID["u-1"] = &model.Subscription{ID: "s1", UserID: "u-1", PlanID: "monthly", Status: "active", StartedAt: now.Add(-60 * 24 * time.Hour), ExpiresAt: &expiredAt}
			return r
		}(), activePlan, ErrChatNoAccess},
		{"plan inactive", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "retired")
			return r
		}(), inactivePlan, ErrChatNoAccess},
		{"plan lacks app", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "monthly")
			return r
		}(), wrongAppPlan, ErrChatNoAccess},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planRepo := newMockPlanRepo()
			planRepo.plans[tc.plan.ID] = tc.plan
			svc := NewChatService(testCatalog("https://upstream.invalid"), tc.subRepo, planRepo, &mockLLMUsageRepo{})
			_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestChatService_RepoError(t *testing.T) {
	subRepo := newMockSubscriptionRepo()
	subRepo.findErr = errors.New("db down")
	svc := NewChatService(testCatalog("https://upstream.invalid"), subRepo, newMockPlanRepo(), &mockLLMUsageRepo{})
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err == nil || errors.Is(err, ErrChatNoAccess) || errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want a wrapped repo error", err)
	}
}

func TestChatService_StreamSuccess(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	var gotAuth string
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "",
		[]model.ChatMessage{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want Bearer test-key", gotAuth)
	}
	if route == nil || route.LogicalModel != "deepseek-flash" || route.Provider != "deepseek" ||
		route.Protocol != llm.ProtocolOpenAI || route.UpstreamModel != "deepseek-v4-flash" {
		t.Errorf("route = %+v", route)
	}
	body := string(gotBody)
	for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`, `"include_usage":true`, `"content":"hi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream body missing %s: %s", want, body)
		}
	}
	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(streamed) != sse {
		t.Errorf("stream = %q, want verbatim %q", streamed, sse)
	}
}

func TestChatService_ToolsAndThinkingRelay(t *testing.T) {
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), tools, &thinking)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	body := string(gotBody)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream body missing %s: %s", want, body)
		}
	}
}

// TestChatService_KeyPoolFailover: a 429 from key A must cool A and retry
// on key B, succeeding transparently.
func TestChatService_KeyPoolFailover(t *testing.T) {
	var keyAuths []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyAuths = append(keyAuths, r.Header.Get("Authorization"))
		if len(keyAuths) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	})
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "m",
		Providers:    map[string]llm.Provider{"p": {Protocol: llm.ProtocolOpenAI, BaseURL: srv.URL, APIKeys: []string{"key-a", "key-b"}}},
		Models:       map[string]llm.Model{"m": {Provider: "p", UpstreamModel: "u"}},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "m", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v (want transparent failover)", err)
	}
	resp.Body.Close()
	if len(keyAuths) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failover)", len(keyAuths))
	}
	if keyAuths[0] == keyAuths[1] {
		t.Errorf("failover must use the other key: %v", keyAuths)
	}
}

// TestChatService_NoToolsNoThinkingKeepsLegacyPayload locks back-compat:
// a client that sends neither tools nor thinking_enabled gets an upstream
// payload without those keys — and without reasoning_content, which
// omitempty must drop when absent so non-thinking payloads stay
// byte-identical to the pre-thinking-proxy shape.
func TestChatService_NoToolsNoThinkingKeepsLegacyPayload(t *testing.T) {
	sse := "data: [DONE]\n\n"
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	body := string(gotBody)
	if strings.Contains(body, `"tools"`) || strings.Contains(body, `"thinking"`) || strings.Contains(body, `"reasoning_content"`) {
		t.Errorf("legacy payload must not contain tools/thinking/reasoning_content: %s", body)
	}
}

// TestChatService_SingleKey429NoRetry: with one key, a 429 maps to
// ErrChatRateLimited without a retry.
func TestChatService_SingleKey429NoRetry(t *testing.T) {
	var calls int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatRateLimited) {
		t.Fatalf("err = %v, want ErrChatRateLimited", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry with a single key)", calls)
	}
}

func TestChatService_UpstreamErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"rate limited", http.StatusTooManyRequests, ErrChatRateLimited},
		{"upstream 500", http.StatusInternalServerError, ErrChatUpstreamError},
		{"upstream 400", http.StatusBadRequest, ErrChatUpstreamRejected},
		{"upstream 401", http.StatusUnauthorized, ErrChatUpstreamRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":"boom"}`))
			})
			svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
			seedChatActiveSub(subRepo, "u-1", "monthly")
			planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

			_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestChatService_AnthropicRoute: an anthropic-protocol provider gets the
// translated request at /v1/messages with the anthropic headers, and its SSE
// stream comes back translated to OpenAI chunks.
func TestChatService_AnthropicRoute(t *testing.T) {
	anthropicSSE := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var gotPath, gotAPIKey, gotVersion, gotBody string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(anthropicSSE))
	})
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "kimi-k3",
		Providers:    map[string]llm.Provider{"kimi": {Protocol: llm.ProtocolAnthropic, BaseURL: srv.URL, APIKeys: []string{"sk-kimi-1"}}},
		Models:       map[string]llm.Model{"kimi-k3": {Provider: "kimi", UpstreamModel: "kimi-k3-latest"}},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	resp, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-kimi-1" || gotVersion == "" {
		t.Errorf("anthropic headers: x-api-key=%q anthropic-version=%q", gotAPIKey, gotVersion)
	}
	if !strings.Contains(gotBody, `"max_tokens"`) || strings.Contains(gotBody, `"stream_options"`) {
		t.Errorf("anthropic payload wrong shape: %s", gotBody)
	}
	if route.Protocol != llm.ProtocolAnthropic {
		t.Errorf("route.Protocol = %q", route.Protocol)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	for _, want := range []string{`"content":"hi"`, `"prompt_tokens":9`, `"completion_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("translated stream missing %s: %s", want, s)
		}
	}
}

// TestChatService_AnthropicShapeGuardMarkedClientError: a history shape that
// Anthropic forbids (here: system-only, which would translate to zero
// messages) passes chat validation but must fail BEFORE any upstream call,
// wrapped so errors.Is finds ErrChatRequestShape — the handler maps that to
// 400, not the generic 500 a plain encode error would get.
func TestChatService_AnthropicShapeGuardMarkedClientError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "kimi-k3",
		Providers:    map[string]llm.Provider{"kimi": {Protocol: llm.ProtocolAnthropic, BaseURL: srv.URL, APIKeys: []string{"sk-kimi-1"}}},
		Models:       map[string]llm.Model{"kimi-k3": {Provider: "kimi", UpstreamModel: "kimi-k3-latest"}},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "",
		[]model.ChatMessage{{Role: "system", Content: "be brief"}}, nil, nil)
	if err == nil {
		t.Fatal("StreamChat succeeded for a system-only history on an Anthropic model")
	}
	if !errors.Is(err, ErrChatRequestShape) {
		t.Errorf("err = %v, want errors.Is(err, ErrChatRequestShape)", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("upstream called %d times, want 0 (shape guards fail before any spend)", n)
	}
}

func TestChatService_RecordUsage(t *testing.T) {
	usageRepo := &mockLLMUsageRepo{}
	svc := NewChatService(testCatalog("https://upstream.invalid"), newMockSubscriptionRepo(), newMockPlanRepo(), usageRepo)
	route := &ChatRoute{LogicalModel: "deepseek-flash", Provider: "deepseek", Protocol: llm.ProtocolOpenAI, UpstreamModel: "deepseek-v4-flash", InputPerMtok: 2, OutputPerMtok: 8}
	// cost = 100*2 + 50*8 = 600 µ¥ (see llm.Model price identity)
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 100, 50)
	if len(usageRepo.events) != 1 {
		t.Fatalf("events = %d, want 1", len(usageRepo.events))
	}
	ev := usageRepo.events[0]
	if ev.UserID != "u-1" || ev.Model != "deepseek-flash" || ev.Provider != "deepseek" ||
		ev.UpstreamModel != "deepseek-v4-flash" || ev.Status != "ok" ||
		ev.InputTokens != 100 || ev.OutputTokens != 50 || ev.CostMicros != 600 {
		t.Errorf("event = %+v", ev)
	}

	// Repo error must be swallowed (metering never breaks chat).
	usageRepo.err = errors.New("db down")
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 1, 1)

	// nil route / nil usageRepo must not panic.
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", nil, "ok", 1, 1)
	svcNoRepo := NewChatService(testCatalog("https://upstream.invalid"), newMockSubscriptionRepo(), newMockPlanRepo(), nil)
	svcNoRepo.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 1, 1)
}

func TestChatService_AllowedModels(t *testing.T) {
	catalog := &llm.Catalog{
		DefaultModel: "deepseek-flash",
		Providers: map[string]llm.Provider{
			"deepseek": {Protocol: llm.ProtocolOpenAI, BaseURL: "https://api.deepseek.com", APIKeys: []string{"k"}},
			"kimi":     {Protocol: llm.ProtocolAnthropic, BaseURL: "https://api.kimi.com/coding", APIKeys: []string{"k"}},
		},
		Models: map[string]llm.Model{
			"deepseek-flash": {Provider: "deepseek", UpstreamModel: "deepseek-chat", DisplayName: "DeepSeek Flash"},
			"kimi-k3":        {Provider: "kimi", UpstreamModel: "kimi-k3-latest", DisplayName: "Kimi K3"},
		},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	// Unrestricted plan → all models, default flagged.
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	models, err := svc.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if err != nil {
		t.Fatalf("AllowedModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	byID := map[string]ChatModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if !byID["deepseek-flash"].Default || byID["deepseek-flash"].DisplayName != "DeepSeek Flash" {
		t.Errorf("deepseek-flash info = %+v", byID["deepseek-flash"])
	}
	if byID["kimi-k3"].Default {
		t.Errorf("kimi-k3 must not be default")
	}

	// Allowlisted plan → filtered.
	planRepo.plans["monthly"].ChatModels = pq.StringArray{"kimi-k3"}
	models, err = svc.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if err != nil {
		t.Fatalf("AllowedModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "kimi-k3" {
		t.Errorf("models = %+v, want only kimi-k3", models)
	}

	// No subscription → ErrChatNoAccess.
	_, err = svc.AllowedModels(context.Background(), "u-nope", "yunhou-website")
	if !errors.Is(err, ErrChatNoAccess) {
		t.Errorf("err = %v, want ErrChatNoAccess", err)
	}
}

// TestChatService_ReasoningContentRelay locks the thinking-mode pass-through:
// DeepSeek's thinking mode rejects a continuation whose assistant tool_calls
// turn doesn't carry back the reasoning_content it was generated with ("The
// `reasoning_content` in the thinking mode must be passed back to the API").
// kaya owns conversation history and sends reasoning_content on assistant
// turns; the proxy must relay it verbatim. Messages are bound from JSON
// exactly as the handler's ShouldBindJSON does, so the test locks the wire
// tag itself — a struct literal would pass even with a mistyped tag.
func TestChatService_ReasoningContentRelay(t *testing.T) {
	sse := "data: [DONE]\n\n"
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	var messages []model.ChatMessage
	inbound := `[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":"","reasoning_content":"thinking about ls","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","content":"file_a","tool_call_id":"call_1"}
	]`
	if err := json.Unmarshal([]byte(inbound), &messages); err != nil {
		t.Fatalf("unmarshal inbound messages: %v", err)
	}

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", messages, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	body := string(gotBody)
	if !strings.Contains(body, `"reasoning_content":"thinking about ls"`) {
		t.Errorf("upstream body missing reasoning_content: %s", body)
	}
}

// TestChatService_UpstreamRejectionDetail: an upstream 4xx surfaces as a
// *ChatUpstreamRejection carrying the real status, a normalized code, and the
// upstream message — errors.Is still matches ErrChatUpstreamRejected.
func TestChatService_UpstreamRejectionDetail(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"This model's maximum context length is 65536.","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	})
	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUpstreamRejected) {
		t.Fatalf("err = %v, want ErrChatUpstreamRejected", err)
	}
	var rej *ChatUpstreamRejection
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v, want *ChatUpstreamRejection", err)
	}
	if rej.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", rej.Status)
	}
	if rej.Code != UpstreamCodeContextLengthExceeded {
		t.Errorf("Code = %q, want %q", rej.Code, UpstreamCodeContextLengthExceeded)
	}
	if !strings.Contains(rej.Message, "maximum context length") {
		t.Errorf("Message = %q, want upstream message", rej.Message)
	}
}

// TestClassifyUpstreamRejection: the classifier maps upstream status + error
// body to a normalized code and a sanitized message.
func TestClassifyUpstreamRejection(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		code    string
		msgWant string // substring; "" means message must be empty-safe
	}{
		{"402 is balance", http.StatusPaymentRequired, `{"error":{"message":"whatever"}}`, UpstreamCodeInsufficientBalance, "whatever"},
		{"insufficient keyword", http.StatusBadRequest, `{"error":{"message":"Insufficient Balance"}}`, UpstreamCodeInsufficientBalance, "Insufficient Balance"},
		{"context length code", http.StatusBadRequest, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, UpstreamCodeContextLengthExceeded, "too long"},
		{"openai context message", http.StatusBadRequest, `{"error":{"message":"This model's maximum context length is 65536 tokens."}}`, UpstreamCodeContextLengthExceeded, "maximum context length"},
		{"anthropic prompt too long", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213000 tokens > 200000 maximum"}}`, UpstreamCodeContextLengthExceeded, "prompt is too long"},
		{"content filter", http.StatusBadRequest, `{"error":{"message":"Content Exists Risk"}}`, UpstreamCodeContentFilter, "Content Exists Risk"},
		{"invalid request fallback", http.StatusBadRequest, `{"error":{"message":"messages: role not supported"}}`, UpstreamCodeInvalidRequest, "role not supported"},
		{"string error shape", http.StatusUnauthorized, `{"error":"bad key"}`, UpstreamCodeInvalidRequest, "bad key"},
		{"non-JSON body", http.StatusBadRequest, `Bad Request`, UpstreamCodeInvalidRequest, "Bad Request"},
		{"empty body", http.StatusBadRequest, ``, UpstreamCodeInvalidRequest, ""},
		{"error object without message keeps raw body", http.StatusBadRequest, `{"error":{"type":"invalid_request_error"}}`, UpstreamCodeInvalidRequest, `"type":"invalid_request_error"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rej := classifyUpstreamRejection(tc.status, []byte(tc.body))
			if rej.Status != tc.status {
				t.Errorf("Status = %d, want %d", rej.Status, tc.status)
			}
			if rej.Code != tc.code {
				t.Errorf("Code = %q, want %q", rej.Code, tc.code)
			}
			if tc.msgWant != "" && !strings.Contains(rej.Message, tc.msgWant) {
				t.Errorf("Message = %q, want substring %q", rej.Message, tc.msgWant)
			}
		})
	}
}

// TestClassifyUpstreamRejection_MessageCapped: a pathological upstream error
// body cannot push an unbounded message into the client response / audit log.
func TestClassifyUpstreamRejection_MessageCapped(t *testing.T) {
	long := strings.Repeat("x", chatUpstreamErrorBodyCap)
	rej := classifyUpstreamRejection(http.StatusBadRequest, []byte(`{"error":{"message":"`+long+`"}}`))
	if len(rej.Message) > chatUpstreamMessageCap+len("…") {
		t.Errorf("Message len = %d, want <= %d (+ellipsis)", len(rej.Message), chatUpstreamMessageCap)
	}
}

// TestClassifyUpstreamRejection_InvalidUTF8: a stray non-UTF-8 byte (GBK
// error pages, misbehaving gateways) must not discard the whole message —
// the invalid byte is replaced, the rest survives. (The JSON-path is cleaned
// by encoding/json itself; these cases exercise the raw-text fallback.)
func TestClassifyUpstreamRejection_InvalidUTF8(t *testing.T) {
	rej := classifyUpstreamRejection(http.StatusBadRequest, []byte("bad \xffrequest shape"))
	if !strings.Contains(rej.Message, "bad ") || !strings.Contains(rej.Message, "request shape") {
		t.Errorf("Message = %q, want surviving content around the invalid byte", rej.Message)
	}

	// Long message with an invalid byte before the cap: content up to the
	// cap must survive (not collapse to a bare ellipsis).
	rej = classifyUpstreamRejection(http.StatusBadRequest, []byte("\xff"+strings.Repeat("a", 400)))
	if !strings.Contains(rej.Message, strings.Repeat("a", 100)) {
		t.Errorf("Message = %.50q, want capped content preserved", rej.Message)
	}
}

// TestChatService_MaxTokensCapEnforced: 评审安全补丁（无 max_tokens 上限）——
// 客户端请求不携带 max_tokens，OpenAI 协议路径的上游 payload 必须始终写入
// 硬上限（默认 8192，setter 可调）。
func TestChatService_MaxTokensCapEnforced(t *testing.T) {
	sse := "data: [DONE]\n\n"
	var gotBodies []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(gotBodies[0], `"max_tokens":8192`) {
		t.Errorf("default cap missing from upstream body: %s", gotBodies[0])
	}

	svc.SetMaxOutputTokens(100)
	resp, _, err = svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(gotBodies[1], `"max_tokens":100`) {
		t.Errorf("configured cap missing from upstream body: %s", gotBodies[1])
	}
}

// TestChatService_AnthropicMaxTokensClamped: Anthropic 协议路径——目录里的
// operator 配置值（Model.MaxTokens）被 honored，但以硬上限封顶；缺省（0）
// 也回落硬上限。
func TestChatService_AnthropicMaxTokensClamped(t *testing.T) {
	var gotBodies []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "big",
		Providers:    map[string]llm.Provider{"kimi": {Protocol: llm.ProtocolAnthropic, BaseURL: srv.URL, APIKeys: []string{"sk-1"}}},
		Models: map[string]llm.Model{
			"big":   {Provider: "kimi", UpstreamModel: "u-big", MaxTokens: 16384},
			"small": {Provider: "kimi", UpstreamModel: "u-small", MaxTokens: 4096},
			"unset": {Provider: "kimi", UpstreamModel: "u-unset"},
		},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	for i, tc := range []struct {
		model string
		want  string
	}{
		{"big", `"max_tokens":8192`},  // operator 配置超上限 → 封顶
		{"small", `"max_tokens":4096`}, // 上限以内 → honored
		{"unset", `"max_tokens":8192`}, // 缺省 → 硬上限
	} {
		resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", tc.model, chatMessages(), nil, nil)
		if err != nil {
			t.Fatalf("StreamChat(%s): %v", tc.model, err)
		}
		resp.Body.Close()
		if !strings.Contains(gotBodies[i], tc.want) {
			t.Errorf("model %s: upstream body = %s, want %s", tc.model, gotBodies[i], tc.want)
		}
	}
}

// blockingChatUpstream returns a handler that sends SSE headers immediately
// then holds the stream open until the request ctx ends — a stand-in for a
// long generation, used by the concurrency-limit tests.
func blockingChatUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
}

// TestChatService_ConcurrencyLimit: 评审安全补丁（按用户并发约束）——同一
// 用户的并发流式请求超过上限即拒绝；错误同时匹配 ErrChatConcurrencyLimited
// 与 ErrChatRateLimited（后者让 handler 现有映射返回 429）。名额按用户
// 隔离，body 关闭后立即释放。
func TestChatService_ConcurrencyLimit(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, blockingChatUpstream())
	svc.SetMaxStreamsPerUser(2)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	seedChatActiveSub(subRepo, "u-2", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	ctx := context.Background()

	r1, _, err := svc.StreamChat(ctx, "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("stream 1: %v", err)
	}
	r2, _, err := svc.StreamChat(ctx, "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("stream 2: %v", err)
	}

	_, _, err = svc.StreamChat(ctx, "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatConcurrencyLimited) {
		t.Fatalf("err = %v, want ErrChatConcurrencyLimited", err)
	}
	if !errors.Is(err, ErrChatRateLimited) {
		t.Errorf("err = %v, want errors.Is ErrChatRateLimited (handler 429 映射)", err)
	}

	// 名额按用户隔离：u-2 不受 u-1 占满影响。
	r3, _, err := svc.StreamChat(ctx, "u-2", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("u-2 stream: %v (per-user isolation)", err)
	}
	defer r3.Body.Close()

	// body 关闭即释放名额：u-1 可以立刻再开一条。
	r1.Body.Close()
	r4, _, err := svc.StreamChat(ctx, "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("stream after Close: %v (slot must be released)", err)
	}
	r4.Body.Close()
	r2.Body.Close()
}

// TestChatService_ConcurrencySlotReleasedOnCtxCancel: body 未关闭但 ctx 取消
// （客户端断连）的路径也必须释放名额——由 reqCtx watcher 兜底。
func TestChatService_ConcurrencySlotReleasedOnCtxCancel(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, blockingChatUpstream())
	svc.SetMaxStreamsPerUser(1)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	ctx, cancel := context.WithCancel(context.Background())
	resp, _, err := svc.StreamChat(ctx, "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// 模拟调用方泄漏：不 Close body，直接取消 ctx。
	cancel()
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		r, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
		if err == nil {
			r.Body.Close()
			return // 名额已释放
		}
		if !errors.Is(err, ErrChatConcurrencyLimited) {
			t.Fatalf("err = %v, want ErrChatConcurrencyLimited while waiting", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not released after ctx cancel (watcher 兜底失效)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestChatService_ConcurrencySlotReleasedOnError: 上游立即失败的路径不能
// 占用名额——连续 N（N>上限）次失败请求后仍能再发起新请求。
func TestChatService_ConcurrencySlotReleasedOnError(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	})
	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	svc.SetMaxStreamsPerUser(1)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	for i := 0; i < 5; i++ {
		_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
		if !errors.Is(err, ErrChatUpstreamError) {
			t.Fatalf("attempt %d: err = %v, want ErrChatUpstreamError (名额泄漏会报 ErrChatConcurrencyLimited)", i, err)
		}
	}
}

// TestChatService_CrossOriginRedirectNotFollowed: 评审安全补丁（凭证边
// 界）——跨 origin 重定向绝不跟随，x-api-key/Authorization 与运营商自定义
// 鉴权头不能落进另一台主机；3xx 响应按上游错误处理。
func TestChatService_CrossOriginRedirectNotFollowed(t *testing.T) {
	var bHits int32
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srvB.Close)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvB.URL+"/chat/completions", http.StatusFound)
	}))
	t.Cleanup(srvA.Close)

	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	svc.catalog = testCatalog(srvA.URL)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUpstreamError) {
		t.Fatalf("err = %v, want ErrChatUpstreamError (3xx surfaced as upstream error)", err)
	}
	if n := atomic.LoadInt32(&bHits); n != 0 {
		t.Errorf("redirect target received %d requests, want 0 (凭证未外泄)", n)
	}
}

// TestChatService_SameOriginRedirectFollowed: 同 origin 跳转（换路径不换主
// 机）仍被允许——凭证不出主机边界，兼容性不受影响。
func TestChatService_SameOriginRedirectFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v2/chat/completions", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	svc.catalog = testCatalog(srv.URL)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v (same-origin redirect must be followed)", err)
	}
	resp.Body.Close()
}

// TestChatService_RedirectLoopFails: 同 origin 重定向循环在 5 跳后报错，
// 按上游错误处理（不会无限跳转）。
func TestChatService_RedirectLoopFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/chat/completions", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	svc.catalog = testCatalog(srv.URL)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUpstreamError) {
		t.Fatalf("err = %v, want ErrChatUpstreamError (redirect loop)", err)
	}
}
