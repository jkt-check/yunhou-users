package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// streamFunc is the mock StreamChat signature (kept named so mock field
// declarations stay readable).
type streamFunc func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error)

// usageCall records one RecordUsage invocation for assertions.
type usageCall struct {
	userID, appID, status string
	route                 *service.ChatRoute
	inputTokens           int
	outputTokens          int
}

// mockChatStreamer implements chatStreamer with injectable results.
type mockChatStreamer struct {
	streamFn      streamFunc
	usageEvents   []usageCall
	allowedModels []service.ChatModelInfo
	allowedErr    error
}

func (m *mockChatStreamer) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
	return m.streamFn(ctx, userID, appID, logicalModel, messages, tools, thinkingEnabled)
}

func (m *mockChatStreamer) RecordUsage(_ context.Context, userID, appID string, route *service.ChatRoute, status string, inputTokens, outputTokens int) {
	m.usageEvents = append(m.usageEvents, usageCall{userID: userID, appID: appID, status: status, route: route, inputTokens: inputTokens, outputTokens: outputTokens})
}

func (m *mockChatStreamer) AllowedModels(_ context.Context, _, _ string) ([]service.ChatModelInfo, error) {
	return m.allowedModels, m.allowedErr
}

// chatCall captures one StreamChat invocation for assertions.
type chatCall struct {
	userID, appID, logicalModel string
	messages                    []model.ChatMessage
	tools                       []json.RawMessage
	thinkingEnabled             *bool
}

// sseResp builds a streaming upstream response carrying sse.
func sseResp(sse string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

// streamReply returns a streamFn that records the call into got (when
// non-nil) and replies with a 200 SSE response carrying sse plus a resolved
// route, mimicking a successful upstream call.
func streamReply(sse string, got *chatCall) streamFunc {
	return func(_ context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
		if got != nil {
			*got = chatCall{userID: userID, appID: appID, logicalModel: logicalModel, messages: messages, tools: tools, thinkingEnabled: thinkingEnabled}
		}
		return sseResp(sse), &service.ChatRoute{LogicalModel: "deepseek-flash"}, nil
	}
}

// streamFails returns a streamFn that fails with err before any upstream
// response exists (no route either).
func streamFails(err error) streamFunc {
	return func(context.Context, string, string, string, []model.ChatMessage, []json.RawMessage, *bool) (*http.Response, *service.ChatRoute, error) {
		return nil, nil, err
	}
}

// chatTestRouter wires a ChatHandler behind a fake JWT identity
// (user_id/app_id set directly) and returns the router + mock.
func chatTestRouter(svc *mockChatStreamer) (*gin.Engine, *mockChatStreamer) {
	gin.SetMode(gin.TestMode)
	h := NewChatHandler(svc, nil)
	r := gin.New()
	r.POST("/chat", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u-1")
		c.Set(middleware.ContextAppID, "yunhou-website")
		h.StreamChat(c)
	})
	return r, svc
}

// chatTestRouterWithLog wires the same handler with an in-memory access log
// and returns the router + log buffer.
func chatTestRouterWithLog(svc *mockChatStreamer) (*gin.Engine, *bytes.Buffer) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	h := NewChatHandler(svc, log.New(&buf, "", 0))
	r := gin.New()
	r.POST("/chat", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u-1")
		c.Set(middleware.ContextAppID, "yunhou-website")
		h.StreamChat(c)
	})
	return r, &buf
}

// chatModelsTestRouter wires GetModels (GET /chat/models) behind the same
// fake JWT identity.
func chatModelsTestRouter(svc *mockChatStreamer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewChatHandler(svc, nil)
	r := gin.New()
	r.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u-1")
		c.Set(middleware.ContextAppID, "yunhou-website")
		h.GetModels(c)
	})
	return r
}

func performChatRequest(r *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestChatHandler_Validation(t *testing.T) {
	longContent := strings.Repeat("a", model.ChatMaxMessageBytes+1)
	cases := []struct {
		name string
		body string
	}{
		{"empty messages", `{}`},
		{"empty messages array", `{"messages":[]}`},
		{"invalid role", `{"messages":[{"role":"robot","content":"hi"}]}`},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`},
		{"content too long", `{"messages":[{"role":"user","content":"` + longContent + `"}]}`},
		{"system content too long", `{"messages":[{"role":"system","content":"` + strings.Repeat("a", model.ChatMaxSystemBytes+1) + `"}]}`},
		{"malformed json", `{"messages":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatStreamer{})
			w := performChatRequest(r, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			if resp["code"] != float64(http.StatusBadRequest) {
				t.Errorf("code = %v, want 400", resp["code"])
			}
		})
	}
}

// TestChatHandler_ModelTooLong: a model id beyond ChatMaxModelLen is
// rejected with 400 before any upstream spend (the service is never called).
func TestChatHandler_ModelTooLong(t *testing.T) {
	called := false
	mock := &mockChatStreamer{streamFn: func(context.Context, string, string, string, []model.ChatMessage, []json.RawMessage, *bool) (*http.Response, *service.ChatRoute, error) {
		called = true
		return nil, nil, errors.New("must not be called")
	}}
	r, _ := chatTestRouter(mock)
	body := `{"model":"` + strings.Repeat("m", model.ChatMaxModelLen+1) + `","messages":[{"role":"user","content":"hi"}]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["message"] != "model id too long" {
		t.Errorf("message = %v, want %q", resp["message"], "model id too long")
	}
	if called {
		t.Error("streamFn was called for an over-long model id")
	}
}

// TestChatHandler_SystemMessageBudget verifies that a system message is
// judged against the system budget (ChatMaxSystemBytes), not the general
// per-message cap (ChatMaxMessageBytes). The per-message cap now exceeds
// the system budget (32 KiB vs 24 KiB), so the system budget is the binding
// constraint — a system message right at the budget must be accepted.
func TestChatHandler_SystemMessageBudget(t *testing.T) {
	// kaya's rendered system prompt is ~21 KB; build one right at the
	// 24576-byte system budget.
	bigSystem := strings.Repeat("a", model.ChatMaxSystemBytes)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	var got chatCall
	r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, &got)})
	body := `{"messages":[{"role":"system","content":"` + bigSystem + `"},{"role":"user","content":"hi"}]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (system budget allows content up to ChatMaxSystemBytes)", w.Code)
	}
	if len(got.messages) != 2 || got.messages[0].Role != "system" {
		t.Errorf("messages relayed = %+v, want [system ..., user hi]", got.messages)
	}
}

func TestChatHandler_ToolsValidation(t *testing.T) {
	tooMany := strings.Builder{}
	tooMany.WriteString(`{"messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i := 0; i < model.ChatMaxTools+1; i++ {
		if i > 0 {
			tooMany.WriteString(",")
		}
		tooMany.WriteString(`{"type":"function"}`)
	}
	tooMany.WriteString(`]}`)

	bigTool := strings.Builder{}
	bigTool.WriteString(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","description":"`)
	bigTool.WriteString(strings.Repeat("a", model.ChatMaxToolsBytes+1))
	bigTool.WriteString(`"}]}`)

	cases := []struct {
		name string
		body string
	}{
		{"too many tools", tooMany.String()},
		{"tools too large", bigTool.String()},
		{"invalid tool element (non-object)", `{"messages":[{"role":"user","content":"hi"}],"tools":[null]}`},
		{"invalid tool element (empty)", `{"messages":[{"role":"user","content":"hi"}],"tools":[""]}`},
		{"invalid tool element (array)", `{"messages":[{"role":"user","content":"hi"}],"tools":[[]]}`},
		{"body too large", `{"messages":[{"role":"user","content":"hi"}],"padding":"` + strings.Repeat("a", chatMaxBodyBytes+1) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatStreamer{})
			w := performChatRequest(r, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}

	// 合法 tools + thinking 透传到 svc。
	t.Run("valid tools and thinking relayed", func(t *testing.T) {
		sse := "data: [DONE]\n\n"
		var got chatCall
		r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, &got)})
		w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"ls"}],"thinking_enabled":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if len(got.tools) != 1 {
			t.Fatalf("tools = %d, want 1", len(got.tools))
		}
		if got.thinkingEnabled == nil || !*got.thinkingEnabled {
			t.Fatalf("thinking_enabled not relayed: %v", got.thinkingEnabled)
		}
	})
}

func TestChatHandler_ServiceErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not enabled", service.ErrChatNotEnabled, http.StatusNotFound},
		{"no access", service.ErrChatNoAccess, http.StatusForbidden},
		{"upstream rate limited", service.ErrChatRateLimited, http.StatusTooManyRequests},
		{"upstream rejected", service.ErrChatUpstreamRejected, http.StatusBadGateway},
		{"upstream error", service.ErrChatUpstreamError, http.StatusBadGateway},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamFails(tc.err)})
			w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			if resp["code"] != float64(tc.status) {
				t.Errorf("code = %v, want %d", resp["code"], tc.status)
			}
			if resp["data"] != nil {
				t.Errorf("data = %v, want null", resp["data"])
			}
		})
	}
}

// TestChatHandler_UnknownModelMapped locks the two model-selection error
// mappings: an unknown catalog id is a client error (400), a known model the
// plan excludes is forbidden (403).
func TestChatHandler_UnknownModelMapped(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"unknown model", service.ErrChatUnknownModel, http.StatusBadRequest},
		{"model not allowed", service.ErrChatModelNotAllowed, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamFails(tc.err)})
			w := performChatRequest(r, `{"model":"some-model","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			if resp["message"] != tc.err.Error() {
				t.Errorf("message = %v, want %q", resp["message"], tc.err.Error())
			}
		})
	}
}

func TestChatHandler_StreamSuccess(t *testing.T) {
	// Upstream SSE with two chunks — the relay must emit both and flush.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n"
	var got chatCall
	r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, &got)})
	w := performChatRequest(r, `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if body := w.Body.String(); body != sse {
		t.Errorf("relayed body = %q, want %q", body, sse)
	}
	if got.userID != "u-1" {
		t.Errorf("userID = %q, want u-1", got.userID)
	}
	if got.appID != "yunhou-website" {
		t.Errorf("appID = %q, want yunhou-website", got.appID)
	}
	if len(got.messages) != 2 || got.messages[1].Role != "user" || got.messages[1].Content != "hi" {
		t.Errorf("messages relayed = %+v, want [system be brief, user hi]", got.messages)
	}
}

// TestChatHandler_ModelFieldRelayed: the optional logical model id reaches
// the service verbatim; omitted (pre-multi-model clients) it arrives as "".
func TestChatHandler_ModelFieldRelayed(t *testing.T) {
	sse := "data: [DONE]\n\n"
	var got chatCall
	r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, &got)})
	w := performChatRequest(r, `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got.logicalModel != "kimi-k3" {
		t.Errorf("logicalModel = %q, want kimi-k3", got.logicalModel)
	}

	got = chatCall{}
	w = performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no model field)", w.Code)
	}
	if got.logicalModel != "" {
		t.Errorf("logicalModel = %q, want empty (server picks the default)", got.logicalModel)
	}
}

// TestChatHandler_RecordsUsageAfterStream: a completed stream meters exactly
// one usage event, with tokens parsed from the terminal usage chunk and the
// resolved route from the service.
func TestChatHandler_RecordsUsageAfterStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	mock := &mockChatStreamer{
		streamFn: func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sse)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}, &service.ChatRoute{LogicalModel: "kimi-k3"}, nil
		},
	}
	r, _ := chatTestRouter(mock)
	w := performChatRequest(r, `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(mock.usageEvents) != 1 {
		t.Fatalf("usage events = %d, want 1", len(mock.usageEvents))
	}
	ev := mock.usageEvents[0]
	if ev.inputTokens != 7 || ev.outputTokens != 2 || ev.status != "ok" || ev.route.LogicalModel != "kimi-k3" {
		t.Errorf("usage event = %+v", ev)
	}
	if ev.userID != "u-1" || ev.appID != "yunhou-website" {
		t.Errorf("usage event identity = %s/%s, want u-1/yunhou-website", ev.userID, ev.appID)
	}
}

// TestChatHandler_NoUsageRecordedWhenUpstreamCallFails: no upstream response
// means no spend to meter.
func TestChatHandler_NoUsageRecordedWhenUpstreamCallFails(t *testing.T) {
	mock := &mockChatStreamer{
		streamFn: func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
			return nil, nil, service.ErrChatUpstreamError
		},
	}
	r, _ := chatTestRouter(mock)
	w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if len(mock.usageEvents) != 0 {
		t.Errorf("usage events = %d, want 0 (no upstream spend happened)", len(mock.usageEvents))
	}
}

// failAfterWriter is an http.ResponseWriter whose body writes start failing
// once limit bytes have been written — a client that disconnected mid-stream.
type failAfterWriter struct {
	header  http.Header
	limit   int
	written int
}

func (w *failAfterWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failAfterWriter) WriteHeader(int) {}
func (w *failAfterWriter) Flush()          {}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.written >= w.limit {
		return 0, errors.New("client gone")
	}
	w.written += len(p)
	return len(p), nil
}

// scriptReader yields one chunk per Read — an upstream whose SSE events
// arrive in separate packets (so the relay writes them separately too).
type scriptReader struct {
	chunks []string
	idx    int
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

// TestChatHandler_RecordsUsageOnClientDisconnect: the client disconnects
// mid-stream, right before the chunk carrying the terminal usage object —
// so the usage never reaches the client, but the tokens were spent upstream
// and must still be metered (status "disconnected").
func TestChatHandler_RecordsUsageOnClientDisconnect(t *testing.T) {
	chunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	usageChunk := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n"
	mock := &mockChatStreamer{streamFn: func(context.Context, string, string, string, []model.ChatMessage, []json.RawMessage, *bool) (*http.Response, *service.ChatRoute, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&scriptReader{chunks: []string{chunk1, usageChunk, "data: [DONE]\n\n"}}),
		}, &service.ChatRoute{LogicalModel: "deepseek-flash"}, nil
	}}
	h := NewChatHandler(mock, nil)
	w := &failAfterWriter{limit: len(chunk1)} // first chunk relays, then the client is gone
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(middleware.ContextUserID, "u-1")
	c.Set(middleware.ContextAppID, "yunhou-website")
	h.StreamChat(c)

	if w.written != len(chunk1) {
		t.Errorf("client received %d bytes, want exactly the first chunk (%d)", w.written, len(chunk1))
	}
	if len(mock.usageEvents) != 1 {
		t.Fatalf("usage events = %d, want 1", len(mock.usageEvents))
	}
	ev := mock.usageEvents[0]
	if ev.status != "disconnected" {
		t.Errorf("status = %q, want disconnected", ev.status)
	}
	if ev.inputTokens != 7 || ev.outputTokens != 2 {
		t.Errorf("usage = %d/%d tokens, want 7/2 (spent tokens recorded despite disconnect)", ev.inputTokens, ev.outputTokens)
	}
}

func TestChatHandler_GetModels(t *testing.T) {
	mock := &mockChatStreamer{allowedModels: []service.ChatModelInfo{
		{ID: "deepseek-flash", DisplayName: "DeepSeek Flash", Provider: "deepseek", Default: true},
	}}
	r := chatModelsTestRouter(mock)
	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":0`) {
		t.Errorf("body missing code 0: %s", body)
	}
	if !strings.Contains(body, "deepseek-flash") {
		t.Errorf("body missing model id: %s", body)
	}
	if !strings.Contains(body, `"default":true`) {
		t.Errorf("body missing default flag: %s", body)
	}
}

func TestChatHandler_GetModelsNoAccess(t *testing.T) {
	mock := &mockChatStreamer{allowedErr: service.ErrChatNoAccess}
	r := chatModelsTestRouter(mock)
	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["message"] != service.ErrChatNoAccess.Error() {
		t.Errorf("message = %v, want %q", resp["message"], service.ErrChatNoAccess.Error())
	}
}

func TestChatHandler_AccessLog_Success(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"世界\"}}]}\n\ndata: [DONE]\n\n"
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{streamFn: streamReply(sse, nil)})
	body := `{"session_id":"sess-abc-123","messages":[{"role":"user","content":"hi"}]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logBuf.String())
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("log line not JSON: %v (%s)", err, lines[0])
	}
	if entry.Status != "ok" {
		t.Errorf("status = %q, want ok", entry.Status)
	}
	if entry.UserID != "u-1" {
		t.Errorf("user_id = %q, want u-1", entry.UserID)
	}
	if entry.AppID != "yunhou-website" {
		t.Errorf("app_id = %q, want yunhou-website", entry.AppID)
	}
	if entry.SessionID != "sess-abc-123" {
		t.Errorf("session_id = %q, want sess-abc-123", entry.SessionID)
	}
	if len(entry.Input) != 1 || entry.Input[0].Content != "hi" {
		t.Errorf("input = %+v, want [user hi]", entry.Input)
	}
	if entry.Output != "你好世界" {
		t.Errorf("output = %q, want 你好世界 (parsed from SSE deltas)", entry.Output)
	}
	if entry.MessageCount != 1 || entry.InputBytes != 2 {
		t.Errorf("counts = %d/%d, want 1/2", entry.MessageCount, entry.InputBytes)
	}
	if entry.OutputBytes != len("你好世界") {
		t.Errorf("output_bytes = %d, want %d", entry.OutputBytes, len("你好世界"))
	}
	if entry.TS == "" {
		t.Error("ts is empty")
	}
}

// TestChatHandler_AccessLog_Model: the audit line records the RESOLVED model
// (what actually served the request), not the raw client value.
func TestChatHandler_AccessLog_Model(t *testing.T) {
	sse := "data: [DONE]\n\n"
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{streamFn: streamReply(sse, nil)})
	w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if entry.Model != "deepseek-flash" {
		t.Errorf("model = %q, want the resolved route model deepseek-flash", entry.Model)
	}
}

// TestChatHandler_AccessLog_ModelTruncated: an over-long model id is rejected
// with 400, but the raw value still reaches the audit line — it must be
// truncated there, not mirrored in full (the body cap would otherwise let one
// line carry ~300 KiB of junk).
func TestChatHandler_AccessLog_ModelTruncated(t *testing.T) {
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{})
	long := strings.Repeat("m", model.ChatMaxModelLen*4)
	w := performChatRequest(r, `{"model":"`+long+`","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if want := model.ChatMaxModelLen + len("…"); len(entry.Model) != want {
		t.Errorf("logged model len = %d, want %d (cap + ellipsis)", len(entry.Model), want)
	}
	if !strings.HasSuffix(entry.Model, "…") {
		t.Errorf("logged model = %q, want an ellipsis marker", entry.Model)
	}
	if !utf8.ValidString(entry.Model) {
		t.Error("logged model is not valid UTF-8")
	}
}

func TestChatHandler_AccessLog_Error(t *testing.T) {
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{streamFn: streamFails(service.ErrChatNoAccess)})
	w := performChatRequest(r, `{"session_id":"sess-x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logBuf.String())
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if entry.Status != "error" {
		t.Errorf("status = %q, want error", entry.Status)
	}
	if entry.Error != service.ErrChatNoAccess.Error() {
		t.Errorf("error = %q, want %q", entry.Error, service.ErrChatNoAccess.Error())
	}
	if entry.SessionID != "sess-x" {
		t.Errorf("session_id = %q, want sess-x", entry.SessionID)
	}
	if entry.Output != "" {
		t.Errorf("output = %q, want empty for failed request", entry.Output)
	}
}

func TestExtractChatOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"standard stream", "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n", "你好"},
		{"empty delta skipped", "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\ndata: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n", ""},
		{"non-standard fallback", "event: error\ndata: boom\n\n", "event: error\ndata: boom\n\n"},
		{"empty input", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractChatOutput([]byte(tc.raw)); got != tc.want {
				t.Errorf("extractChatOutput = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateChatOutput_UTF8Boundary(t *testing.T) {
	// A multi-byte char straddling the cap must not be split in half.
	// Build a string whose byte 64 KiB-1 is the middle of a 3-byte char.
	filler := strings.Repeat("a", chatOutputLogCap-1)
	s := filler + "你好"
	got, truncated := truncateChatOutput(s)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8: %q", got[len(got)-8:])
	}
	if len(got) > chatOutputLogCap {
		t.Errorf("len(got) = %d > cap %d", len(got), chatOutputLogCap)
	}
	// "你" occupies bytes 65535..65537 — the cut must land before it.
	if strings.HasSuffix(got, "你") || strings.Contains(got, "你") {
		t.Errorf("truncated output contains a split multi-byte char: %q", got[len(got)-8:])
	}
}

func TestChatHandler_AccessLog_TruncatedOutput(t *testing.T) {
	// A reply longer than the log cap: the line must carry the cap-sized
	// (rune-safe) output, the truncated flag, and the REAL output_bytes.
	big := strings.Repeat("答", chatOutputLogCap/2+100) // > cap in bytes
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"" + big + "\"}}]}\n\ndata: [DONE]\n\n"
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{streamFn: streamReply(sse, nil)})
	if w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if !entry.OutputTruncated {
		t.Error("output_truncated = false, want true")
	}
	if len(entry.Output) > chatOutputLogCap {
		t.Errorf("logged output len = %d > cap %d", len(entry.Output), chatOutputLogCap)
	}
	if !utf8.ValidString(entry.Output) {
		t.Error("logged output is not valid UTF-8")
	}
	if want := len(big); entry.OutputBytes != want {
		t.Errorf("output_bytes = %d, want real length %d", entry.OutputBytes, want)
	}
}

func TestChatHandler_AccessLog_InvalidBody(t *testing.T) {
	// JSON binding failures must also leave an audit line (status=error).
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{})
	w := performChatRequest(r, `{"messages":`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if entry.Status != "error" || entry.Error != "invalid request body" {
		t.Errorf("status/error = %q/%q, want error/invalid request body", entry.Status, entry.Error)
	}
}

func TestChatHandler_AccessLog_UpstreamBroke(t *testing.T) {
	// An upstream that dies mid-stream must be audited as upstream_error
	// (not "disconnected" — that means the CLIENT went away).
	mock := &mockChatStreamer{streamFn: func(context.Context, string, string, string, []model.ChatMessage, []json.RawMessage, *bool) (*http.Response, *service.ChatRoute, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&errAfterReader{data: "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n"}),
		}, &service.ChatRoute{LogicalModel: "deepseek-flash"}, nil
	}}
	r, logBuf := chatTestRouterWithLog(mock)
	w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream had already started)", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if entry.Status != "upstream_error" {
		t.Errorf("status = %q, want upstream_error", entry.Status)
	}
	if entry.Error == "" {
		t.Error("error message is empty for upstream_error")
	}
	if entry.Output != "半" {
		t.Errorf("output = %q, want the partial answer 半", entry.Output)
	}
}

// TestChatHandler_UpstreamBrokeInjectsErrorEvent: when the upstream stream
// breaks mid-answer, the client must receive an explicit in-stream error
// event after the partial chunks — not a [DONE]-less clean EOF that kaya
// would render as a completed answer.
func TestChatHandler_UpstreamBrokeInjectsErrorEvent(t *testing.T) {
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n"
	mock := &mockChatStreamer{streamFn: func(context.Context, string, string, string, []model.ChatMessage, []json.RawMessage, *bool) (*http.Response, *service.ChatRoute, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&errAfterReader{data: partial}),
		}, &service.ChatRoute{LogicalModel: "deepseek-flash"}, nil
	}}
	r, _ := chatTestRouter(mock)
	w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream had already started)", w.Code)
	}
	want := partial + "data: {\"error\":{\"message\":\"upstream stream interrupted\"}}\n\n"
	if body := w.Body.String(); body != want {
		t.Errorf("body = %q, want partial chunk + error event %q", body, want)
	}
}

func TestChatHandler_AccessLog_ErrorInputTruncated(t *testing.T) {
	// Validation-failed requests carry unvalidated content — the audit line
	// must cap it (per message) instead of mirroring the full payload.
	long := strings.Repeat("滥", model.ChatMaxMessageBytes) // fails the per-message validation
	body := `{"messages":[{"role":"user","content":"` + long + `"}]}`
	r, logBuf := chatTestRouterWithLog(&mockChatStreamer{})
	w := performChatRequest(r, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var entry chatAccessEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if entry.Status != "error" {
		t.Errorf("status = %q, want error", entry.Status)
	}
	if !entry.InputTruncated {
		t.Error("input_truncated = false, want true")
	}
	if len(entry.Input) != 1 || len(entry.Input[0].Content) > chatErrInputLogCap {
		t.Errorf("logged input content len = %d, want <= %d", len(entry.Input[0].Content), chatErrInputLogCap)
	}
	if !utf8.ValidString(entry.Input[0].Content) {
		t.Error("logged input is not valid UTF-8")
	}
	// input_bytes still reflects the REAL (rejected) payload size.
	if entry.InputBytes != len(long) {
		t.Errorf("input_bytes = %d, want real length %d", entry.InputBytes, len(long))
	}
}

// TestChatHandler_ToolMessagesAcceptance locks the structured tool_call
// relay: role=tool messages and assistant turns carrying tool_calls must
// pass validation (including empty content, which the OpenAI convention
// allows there) and reach the service verbatim with their tool_calls /
// tool_call_id fields intact. This is the proxy half of eliminating the text
// "[tool call: ...]" annotation that broke the built-in model's tool calling.
func TestChatHandler_ToolMessagesAcceptance(t *testing.T) {
	sse := "data: [DONE]\n\n"
	var got chatCall
	r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, &got)})
	// A multi-turn tool loop: assistant (empty content + tool_calls) followed
	// by a tool result (role=tool + tool_call_id).
	body := `{"messages":[` +
		`{"role":"user","content":"list files"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}]},` +
		`{"role":"tool","content":"file_a\nfile_b","tool_call_id":"call_1"}` +
		`]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (tool messages must be accepted)", w.Code)
	}
	if len(got.messages) != 3 {
		t.Fatalf("relayed messages = %d, want 3", len(got.messages))
	}
	assistant := got.messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls not relayed: %+v", assistant)
	}
	tc := assistant.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "run_shell" {
		t.Errorf("tool_call shape wrong: %+v", tc)
	}
	if tc.Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("arguments = %q, want JSON string {\"cmd\":\"ls\"}", tc.Function.Arguments)
	}
	tool := got.messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "file_a\nfile_b" {
		t.Errorf("tool result not relayed with tool_call_id: %+v", tool)
	}
}

// TestChatHandler_ToolRoleEmptyContentAccepted: role=tool with empty content
// is legitimate (a tool that ran but produced no stdout) and must not trip the
// "message content is required" check.
func TestChatHandler_ToolRoleEmptyContentAccepted(t *testing.T) {
	sse := "data: [DONE]\n\n"
	r, _ := chatTestRouter(&mockChatStreamer{streamFn: streamReply(sse, nil)})
	body := `{"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"noop","arguments":"{}"}}]},` +
		`{"role":"tool","content":"","tool_call_id":"c1"}]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty tool content must be accepted)", w.Code)
	}
}
