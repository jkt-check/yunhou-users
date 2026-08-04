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

// mockChatSvc implements chatStreamer with injectable results.
type mockChatSvc struct {
	resp               *http.Response
	err                error
	gotUID             string
	gotApp             string
	gotMsg             []model.ChatMessage
	gotTools           []json.RawMessage
	gotThinkingEnabled *bool
}

func (m *mockChatSvc) StreamChat(_ context.Context, userID, appID string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	m.gotUID = userID
	m.gotApp = appID
	m.gotMsg = messages
	m.gotTools = tools
	m.gotThinkingEnabled = thinkingEnabled
	return m.resp, m.err
}

// chatTestRouter wires a ChatHandler behind a fake JWT identity
// (user_id/app_id set directly) and returns the router + mock.
func chatTestRouter(svc *mockChatSvc) (*gin.Engine, *mockChatSvc) {
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
func chatTestRouterWithLog(svc *mockChatSvc) (*gin.Engine, *bytes.Buffer) {
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
			r, _ := chatTestRouter(&mockChatSvc{})
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

// TestChatHandler_SystemMessageBudget verifies that a system message that
// exceeds the per-message cap (ChatMaxMessageBytes) but fits within the
// system budget (ChatMaxSystemBytes) is accepted — not rejected as too long.
func TestChatHandler_SystemMessageBudget(t *testing.T) {
	// kaya's rendered system prompt is ~21 KB; build one that is larger than
	// the 8000-byte per-message cap but within the 24576-byte system budget.
	bigSystem := strings.Repeat("a", model.ChatMaxMessageBytes+1)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	svc := &mockChatSvc{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	r, mock := chatTestRouter(svc)
	body := `{"messages":[{"role":"system","content":"` + bigSystem + `"},{"role":"user","content":"hi"}]}`
	w := performChatRequest(r, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (system budget should allow >8000 bytes)", w.Code)
	}
	if len(mock.gotMsg) != 2 || mock.gotMsg[0].Role != "system" {
		t.Errorf("messages relayed = %+v, want [system ..., user hi]", mock.gotMsg)
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
		{"body too large", `{"messages":[{"role":"user","content":"hi"}],"padding":"` + strings.Repeat("a", 150<<10) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatSvc{})
			w := performChatRequest(r, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}

	// 合法 tools + thinking 透传到 svc。
	t.Run("valid tools and thinking relayed", func(t *testing.T) {
		sse := "data: [DONE]\n\n"
		svc := &mockChatSvc{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}}
		r, _ := chatTestRouter(svc)
		w := performChatRequest(r, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"ls"}],"thinking_enabled":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if len(svc.gotTools) != 1 {
			t.Fatalf("tools = %d, want 1", len(svc.gotTools))
		}
		if svc.gotThinkingEnabled == nil || !*svc.gotThinkingEnabled {
			t.Fatalf("thinking_enabled not relayed: %v", svc.gotThinkingEnabled)
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
		{"upstream error", service.ErrChatUpstreamError, http.StatusBadGateway},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := chatTestRouter(&mockChatSvc{err: tc.err})
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

func TestChatHandler_StreamSuccess(t *testing.T) {
	// Upstream SSE with two chunks — the relay must emit both and flush.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n"
	svc := &mockChatSvc{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	r, mock := chatTestRouter(svc)
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
	if mock.gotUID != "u-1" {
		t.Errorf("userID = %q, want u-1", mock.gotUID)
	}
	if mock.gotApp != "yunhou-website" {
		t.Errorf("appID = %q, want yunhou-website", mock.gotApp)
	}
	if len(mock.gotMsg) != 2 || mock.gotMsg[1].Role != "user" || mock.gotMsg[1].Content != "hi" {
		t.Errorf("messages relayed = %+v, want [system be brief, user hi]", mock.gotMsg)
	}
}

func TestChatHandler_AccessLog_Success(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"世界\"}}]}\n\ndata: [DONE]\n\n"
	svc := &mockChatSvc{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	r, logBuf := chatTestRouterWithLog(svc)
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

func TestChatHandler_AccessLog_Error(t *testing.T) {
	r, logBuf := chatTestRouterWithLog(&mockChatSvc{err: service.ErrChatNoAccess})
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
	svc := &mockChatSvc{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	r, logBuf := chatTestRouterWithLog(svc)
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
	r, logBuf := chatTestRouterWithLog(&mockChatSvc{})
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
	svc := &mockChatSvc{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&errAfterReader{data: "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n"}),
	}}
	r, logBuf := chatTestRouterWithLog(svc)
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

func TestChatHandler_AccessLog_ErrorInputTruncated(t *testing.T) {
	// Validation-failed requests carry unvalidated content — the audit line
	// must cap it (per message) instead of mirroring the full payload.
	long := strings.Repeat("滥", model.ChatMaxMessageBytes) // fails the 8000-byte validation
	body := `{"messages":[{"role":"user","content":"` + long + `"}]}`
	r, logBuf := chatTestRouterWithLog(&mockChatSvc{})
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
