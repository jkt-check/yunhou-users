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
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "ghost-model", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUnknownModel) {
		t.Fatalf("err = %v, want ErrChatUnknownModel", err)
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
