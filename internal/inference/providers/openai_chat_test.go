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

// Multimodal parts pass through as the OpenAI content array; text-only
// messages keep the original string shape (byte-identical payload).
func TestOpenAIChatBuildPayload_Multimodal(t *testing.T) {
	var m model.ChatMessage
	if err := m.UnmarshalJSON([]byte(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}`)); err != nil {
		t.Fatal(err)
	}
	body, err := NewOpenAIChat().BuildPayload(testCall("m", true, 8192,
		[]model.ChatMessage{{Role: "system", Content: "s"}, m}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	payload := decode(t, body)
	msgs := payload["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "s" {
		t.Fatalf("text message shape changed: %v", msgs[0])
	}
	parts, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("multimodal message not array: %v", msgs[1])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != "https://x.test/a.png" {
		t.Fatalf("image part lost or reshaped: %v", img)
	}
}

// Tool 字段随行 wire-map 分支（与 candidate BuildOpenAIPayload 同契约）：
// parts 形态的 assistant 轮携带 tool_calls/reasoning_content，tool 结果轮
// 携带 tool_call_id；未设置的键必须缺席。
func TestOpenAIPayload_WireToolFields(t *testing.T) {
	messages := []model.ChatMessage{
		{
			Role:             "assistant",
			ContentParts:     []model.ContentPart{{Type: "text", Text: "let me check"}},
			ToolCalls:        []model.ToolCall{{ID: "call_1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`}}},
			ReasoningContent: "need to inspect the filesystem",
		},
		{
			Role:         "tool",
			ContentParts: []model.ContentPart{{Type: "text", Text: "ls output"}},
			ToolCallID:   "call_1",
		},
		{
			Role:         "user",
			ContentParts: []model.ContentPart{{Type: "text", Text: "plain parts"}},
		},
	}
	body, err := NewOpenAIChat().BuildPayload(testCall("m", true, 8192, messages, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	msgs := decode(t, body)["messages"].([]any)

	assistant := msgs[0].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", assistant["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["name"] != "run_shell" {
		t.Fatalf("tool_call = %v", call)
	}
	if assistant["reasoning_content"] != "need to inspect the filesystem" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	if _, present := assistant["tool_call_id"]; present {
		t.Fatalf("assistant must not carry tool_call_id: %v", assistant)
	}

	tool := msgs[1].(map[string]any)
	if tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v", tool["tool_call_id"])
	}
	if _, present := tool["tool_calls"]; present {
		t.Fatalf("tool turn must not carry tool_calls: %v", tool)
	}
	if _, present := tool["reasoning_content"]; present {
		t.Fatalf("tool turn must not carry reasoning_content: %v", tool)
	}

	plain := msgs[2].(map[string]any)
	for _, k := range []string{"tool_calls", "tool_call_id", "reasoning_content"} {
		if _, present := plain[k]; present {
			t.Fatalf("empty fields must be omitted, found %s: %v", k, plain)
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

// 评审轮1 M5：非流式 usage 块携带 cache_creation_input_tokens 时计入
// CacheWriteTokens（与流式 tap 同口径）。
func TestOpenAIDecodeNonStream_CacheCreationBucket(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46,
			"cache_creation_input_tokens":7,
			"prompt_tokens_details":{"cached_tokens":4}}}`)
	res, err := NewOpenAIChat().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if res.Usage.CacheWriteTokens == nil || *res.Usage.CacheWriteTokens != 7 {
		t.Errorf("cache write = %v, want 7 (cache_creation_input_tokens 不得丢弃)", res.Usage.CacheWriteTokens)
	}
	if res.Usage.CacheReadTokens == nil || *res.Usage.CacheReadTokens != 4 {
		t.Errorf("cache read = %v, want 4", res.Usage.CacheReadTokens)
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
