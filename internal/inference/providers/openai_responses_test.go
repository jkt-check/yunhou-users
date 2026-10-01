package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

func TestOpenAIResponsesPayload_Minimal(t *testing.T) {
	body, err := NewOpenAIResponses().BuildPayload(testCall("gpt-6-luna", true, 8192,
		[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	m := decode(t, body)
	if m["model"] != "gpt-6-luna" || m["stream"] != true {
		t.Errorf("model/stream = %v/%v", m["model"], m["stream"])
	}
	if m["max_output_tokens"] != float64(8192) {
		t.Errorf("max_output_tokens = %v, want 8192 (强制输出上限)", m["max_output_tokens"])
	}
	if m["store"] != false {
		t.Errorf("store = %v, want false (网关自建会话链，不要求上游存储)", m["store"])
	}
	input, ok := m["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v, want one message item", m["input"])
	}
	item := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Errorf("item = %v", item)
	}
	parts := item["content"].([]any)
	if parts[0].(map[string]any)["type"] != "input_text" || parts[0].(map[string]any)["text"] != "hi" {
		t.Errorf("content = %v", parts)
	}
	if _, ok := m["reasoning"]; ok {
		t.Errorf("reasoning must be absent when thinking is not requested: %s", body)
	}
}

func TestOpenAIResponsesPayload_SystemBecomesInstructions(t *testing.T) {
	body, err := NewOpenAIResponses().BuildPayload(testCall("m", false, 1024,
		[]model.ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "system", Content: "be kind"},
			{Role: "user", Content: "hi"},
		}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	m := decode(t, body)
	if m["instructions"] != "be brief\n\nbe kind" {
		t.Errorf("instructions = %v, want joined system turns", m["instructions"])
	}
	if input := m["input"].([]any); len(input) != 1 {
		t.Errorf("input = %v, want only the user item", input)
	}
}

func TestOpenAIResponsesPayload_ToolRoundTrip(t *testing.T) {
	body, err := NewOpenAIResponses().BuildPayload(testCall("m", true, 4096,
		[]model.ChatMessage{
			{Role: "user", Content: "ls?"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_1", Type: "function",
				Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "file.go"},
			{Role: "assistant", Content: "one file"},
		}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	m := decode(t, body)
	input := m["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input len = %d, want 4: %s", len(input), body)
	}
	fc := input[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "run_shell" {
		t.Errorf("function_call item = %v (call_id 逐字保留)", fc)
	}
	out := input[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || out["output"] != "file.go" {
		t.Errorf("function_call_output item = %v", out)
	}
	msg := input[3].(map[string]any)
	if msg["role"] != "assistant" || msg["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Errorf("assistant item = %v", msg)
	}
}

func TestOpenAIResponsesPayload_ToolsToolChoiceThinkingPassthrough(t *testing.T) {
	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell","description":"d","parameters":{"type":"object"}}}`)}
	call := testCall("m", true, 8192, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, &thinking)
	call.Request.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)
	call.Request.Passthrough = map[string]any{"temperature": 0.5, "top_p": 0.9, "parallel_tool_calls": false}
	body, err := NewOpenAIResponses().BuildPayload(call)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	m := decode(t, body)
	tool := m["tools"].([]any)[0].(map[string]any)
	// Responses 工具是扁平形状（envelope 解包），name/parameters 提升。
	if tool["type"] != "function" || tool["name"] != "run_shell" || tool["parameters"] == nil {
		t.Errorf("tool = %v, want flattened function shape", tool)
	}
	if _, ok := tool["function"]; ok {
		t.Errorf("tool must not keep the chat envelope: %v", tool)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "run_shell" {
		t.Errorf("tool_choice = %v, want flattened function choice", tc)
	}
	rs := m["reasoning"].(map[string]any)
	if rs["effort"] != "medium" || rs["summary"] != "auto" {
		t.Errorf("reasoning = %v, want medium effort + summary auto", rs)
	}
	if m["temperature"] != 0.5 || m["top_p"] != 0.9 || m["parallel_tool_calls"] != false {
		t.Errorf("passthrough = %v", m)
	}
}

func TestOpenAIResponsesPayload_ThinkingDisabled(t *testing.T) {
	off := false
	body, err := NewOpenAIResponses().BuildPayload(testCall("m", true, 1024,
		[]model.ChatMessage{{Role: "user", Content: "x"}}, nil, &off))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if rs := decode(t, body)["reasoning"].(map[string]any); rs["effort"] != "none" {
		t.Errorf("reasoning = %v, want effort none", rs)
	}
}

// 不支持的采样参数显式 400 —— 不静默丢字段（设计）。
func TestOpenAIResponsesPayload_RejectsUnsupportedPassthrough(t *testing.T) {
	for _, key := range []string{"stop", "seed", "presence_penalty", "frequency_penalty", "top_k"} {
		call := testCall("m", true, 1024, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, nil)
		call.Request.Passthrough = map[string]any{key: 1}
		if _, err := NewOpenAIResponses().BuildPayload(call); domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%s: err = %v, want invalid_input", key, err)
		}
	}
}

func TestOpenAIResponsesPayload_RequiresPositiveOutputCap(t *testing.T) {
	call := testCall("m", true, 0, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, nil)
	if _, err := NewOpenAIResponses().BuildPayload(call); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input (不允许无限输出)", err)
	}
}

func TestOpenAIResponsesPayload_ReasoningContentNotReplayed(t *testing.T) {
	body, err := NewOpenAIResponses().BuildPayload(testCall("m", true, 4096,
		[]model.ChatMessage{
			{Role: "assistant", Content: "answer", ReasoningContent: "chain of thought"},
			{Role: "user", Content: "next"},
		}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if strings.Contains(string(body), "chain of thought") {
		t.Errorf("reasoning_content must not be replayed upstream: %s", body)
	}
	if !strings.Contains(string(body), "answer") {
		t.Errorf("assistant text must be kept: %s", body)
	}
}

func TestOpenAIResponsesDecodeNonStream_Full(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","model":"gpt-6-luna","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking hard"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"function_call","call_id":"call_9","name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}
		],
		"usage":{"input_tokens":120,"output_tokens":45,"total_tokens":165,
			"input_tokens_details":{"cached_tokens":80},
			"output_tokens_details":{"reasoning_tokens":30}}}`)
	res, err := NewOpenAIResponses().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if !res.UsageReported || res.UpstreamRequestID != "resp_1" {
		t.Fatalf("res = %+v", res)
	}
	u := res.Usage
	if u.InputTokens == nil || *u.InputTokens != 120 || u.OutputTokens == nil || *u.OutputTokens != 45 {
		t.Errorf("usage = %+v, want 120/45", u)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 80 {
		t.Errorf("cache read = %v, want 80", u.CacheReadTokens)
	}
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 30 {
		t.Errorf("reasoning = %v, want 30", u.ReasoningTokens)
	}
	if res.ContentBytes != int64(len("done")+len("thinking hard")) {
		t.Errorf("ContentBytes = %d (content + reasoning 口径)", res.ContentBytes)
	}
	// 翻译后的内部 chat.completion：finish_reason=tool_calls，usage 以
	// OpenAI 形状随线（客户端面渲染 cached/reasoning）。
	m := decode(t, res.Payload)
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["reasoning_content"] != "thinking hard" || msg["content"] != "done" {
		t.Errorf("message = %v", msg)
	}
	calls := msg["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_9" {
		t.Errorf("tool_calls = %v (call_id 逐字保留)", calls)
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(80) {
		t.Errorf("wire usage = %v", usage)
	}
	if usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(30) {
		t.Errorf("wire usage = %v", usage)
	}
	if !strings.Contains(string(res.UsageRaw), `"schema_version":1`) {
		t.Errorf("raw usage must be schema-versioned: %s", res.UsageRaw)
	}
}

func TestOpenAIResponsesDecodeNonStream_IncompleteMapsLength(t *testing.T) {
	body := []byte(`{"id":"resp_2","object":"response","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	res, err := NewOpenAIResponses().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	ch := decode(t, res.Payload)["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length (客户端面渲染回 response.incomplete)", ch["finish_reason"])
	}
}

func TestOpenAIResponsesDecodeNonStream_FailedIsError(t *testing.T) {
	body := []byte(`{"id":"resp_3","object":"response","status":"failed",
		"error":{"code":"server_error","message":"model exploded"}}`)
	if _, err := NewOpenAIResponses().DecodeNonStream(body); err == nil {
		t.Error("failed response must surface as an error (consumption unknown)")
	}
}

func TestOpenAIResponsesDecodeNonStream_NoUsageIsNotZero(t *testing.T) {
	body := []byte(`{"id":"resp_4","object":"response","status":"completed",
		"output":[{"type":"message","content":[{"type":"output_text","text":"visible answer"}]}]}`)
	res, err := NewOpenAIResponses().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if res.UsageReported {
		t.Error("UsageReported = true, want false — missing usage is never zero")
	}
	if res.Usage.InputTokens != nil || res.Usage.OutputTokens != nil {
		t.Errorf("usage = %+v, want all-nil buckets", res.Usage)
	}
	if res.ContentBytes != int64(len("visible answer")) {
		t.Errorf("ContentBytes = %d", res.ContentBytes)
	}
}

func TestOpenAIResponsesInclusion(t *testing.T) {
	inc := NewOpenAIResponses().Inclusion()
	if !inc.ReasoningInOutput || !inc.CacheReadInInput {
		t.Errorf("inclusion = %+v, want both overlaps declared (Responses 口径)", inc)
	}
}

// M1：缺 call_id 的工具回合在付费往返之前本地 400（不外包给上游）。
func TestOpenAIResponsesPayload_MissingCallIDRejected(t *testing.T) {
	cases := map[string][]model.ChatMessage{
		"assistant tool_call without id": {
			{Role: "user", Content: "x"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{
				Type: "function", Function: model.ToolCallFunction{Name: "f", Arguments: "{}"},
			}}},
		},
		"tool message without tool_call_id": {
			{Role: "user", Content: "x"},
			{Role: "tool", Content: "out"},
		},
	}
	for name, messages := range cases {
		if _, err := NewOpenAIResponses().BuildPayload(testCall("m", true, 100, messages, nil, nil)); domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%s: err = %v, want invalid_input", name, err)
		}
	}
}

// m5：纯 system 请求翻译为空 input，本地显式失败（同 anthropic 守卫）。
func TestOpenAIResponsesPayload_SystemOnlyRejected(t *testing.T) {
	_, err := NewOpenAIResponses().BuildPayload(testCall("m", true, 100,
		[]model.ChatMessage{{Role: "system", Content: "be brief"}}, nil, nil))
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input", err)
	}
}

// m4：content_filter 截断保留原生 finish reason，绝不伪装成正常结束。
func TestOpenAIResponsesDecodeNonStream_ContentFilter(t *testing.T) {
	body := []byte(`{"id":"resp_cf","object":"response","status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`)
	res, err := NewOpenAIResponses().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	ch := decode(t, res.Payload)["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "content_filter" {
		t.Errorf("finish_reason = %v, want content_filter", ch["finish_reason"])
	}
}
