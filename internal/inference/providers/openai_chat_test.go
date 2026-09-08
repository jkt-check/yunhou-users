package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// testCall builds a dispatch Call for payload tests.
func testCall(upstreamModel string, stream bool, maxOut int64, messages []model.ChatMessage, tools []json.RawMessage, thinking *bool) *Call {
	return &Call{
		Deployment: &domain.Deployment{UpstreamModel: upstreamModel},
		Request: &ChatRequest{
			Messages:        messages,
			Stream:          stream,
			Tools:           tools,
			ThinkingEnabled: thinking,
		},
		OutputCap: maxOut,
	}
}

// decode is a test helper: unmarshal the payload into a generic map.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, body)
	}
	return m
}

func TestOpenAIPayload_MinimalStreamRequestsUsage(t *testing.T) {
	body, err := NewOpenAIChat().BuildPayload(testCall("deepseek-chat", true, 8192,
		[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	s := string(body)
	// 显式请求流式 usage（设计 §7.1）与强制输出上限（§7.2）必须在场。
	for _, want := range []string{`"model":"deepseek-chat"`, `"stream":true`, `"content":"hi"`,
		`"stream_options":{"include_usage":true}`, `"max_tokens":8192`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"tools"`) || strings.Contains(s, `"thinking"`) {
		t.Errorf("payload must not contain tools/thinking: %s", s)
	}
}

func TestOpenAIPayload_NonStreamOmitsStreamOptions(t *testing.T) {
	body, err := NewOpenAIChat().BuildPayload(testCall("m", false, 1024,
		[]model.ChatMessage{{Role: "user", Content: "x"}}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "stream_options") || strings.Contains(s, `"stream":true`) {
		t.Errorf("non-stream payload must not request stream usage: %s", s)
	}
	if !strings.Contains(s, `"max_tokens":1024`) {
		t.Errorf("payload missing forced cap: %s", s)
	}
}

func TestOpenAIPayload_ToolsThinkingPassthrough(t *testing.T) {
	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	call := testCall("m", true, 8192, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, &thinking)
	call.Request.ToolChoice = json.RawMessage(`"auto"`)
	call.Request.Passthrough = map[string]any{"temperature": 0.5, "seed": 7}
	body, err := NewOpenAIChat().BuildPayload(call)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`,
		`"tool_choice":"auto"`, `"temperature":0.5`, `"seed":7`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
}

// TestOpenAIPayload_AcceptsShapesAnthropicRejects: the guards are scoped to
// the Anthropic translator — the OpenAI payload builder must keep accepting
// system-only and leading-assistant histories unchanged (candidate-branch
// contract, kept).
func TestOpenAIPayload_AcceptsShapesAnthropicRejects(t *testing.T) {
	for _, messages := range [][]model.ChatMessage{
		{{Role: "system", Content: "be brief"}},
		{{Role: "assistant", Content: "hello"}, {Role: "user", Content: "hi"}},
	} {
		if _, err := NewOpenAIChat().BuildPayload(testCall("m", true, 100, messages, nil, nil)); err != nil {
			t.Errorf("BuildPayload(%v): %v, want success", messages, err)
		}
	}
}

func TestOpenAIDecodeNonStream_FullUsage(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46,
			"prompt_tokens_details":{"cached_tokens":4},
			"completion_tokens_details":{"reasoning_tokens":9}}}`)
	res, err := NewOpenAIChat().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if !res.UsageReported || res.UpstreamRequestID != "chatcmpl-1" {
		t.Fatalf("res = %+v", res)
	}
	u := res.Usage
	if u.InputTokens == nil || *u.InputTokens != 12 || u.OutputTokens == nil || *u.OutputTokens != 34 {
		t.Errorf("usage = %+v, want 12/34", u)
	}
	if res.ContentBytes != 2 { // "ok"
		t.Errorf("ContentBytes = %d, want 2 (visible content feeds the estimate path)", res.ContentBytes)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 4 {
		t.Errorf("cache read = %v, want 4", u.CacheReadTokens)
	}
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 9 {
		t.Errorf("reasoning = %v, want 9", u.ReasoningTokens)
	}
	// The client payload is the upstream body untouched.
	if string(res.Payload) != string(body) {
		t.Error("payload must pass through verbatim")
	}
	if !strings.Contains(string(res.UsageRaw), `"schema_version":1`) {
		t.Errorf("raw usage must be schema-versioned: %s", res.UsageRaw)
	}
}

func TestOpenAIDecodeNonStream_NoUsageIsNotZero(t *testing.T) {
	res, err := NewOpenAIChat().DecodeNonStream([]byte(
		`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"visible answer"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if res.UsageReported {
		t.Error("UsageReported = true, want false — missing usage is never zero")
	}
	if res.Usage.InputTokens != nil || res.Usage.OutputTokens != nil {
		t.Errorf("usage = %+v, want all-nil buckets", res.Usage)
	}
	// 可见内容字节必须在场:非流式估算的输出侧依据(不为 0)。
	if res.ContentBytes != int64(len("visible answer")) {
		t.Errorf("ContentBytes = %d, want %d", res.ContentBytes, len("visible answer"))
	}
}
