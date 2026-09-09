// responses_client_test.go — Responses 客户端面翻译的契约级测试（Task 13）。
// Fixture 取自 OpenAI Responses API 官方文档形状与 Codex 实际请求特征
// （store:false、function_call/function_call_output 链、reasoning 项回放）。

package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Codex 风格多轮:instructions + message + function_call + function_call_output
// + reasoning(丢弃)。call_id ↔ tool_call_id 逐字保留。
func TestParseResponsesRequest_CodexShape(t *testing.T) {
	body := `{
	  "model": "gpt-5.2-codex",
	  "instructions": "You are Codex.",
	  "store": false,
	  "stream": true,
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"add tests"}]},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},
	    {"type":"function_call","id":"fc_x","call_id":"call_abc","name":"shell","arguments":"{\"cmd\":\"go test\"}"},
	    {"type":"function_call_output","call_id":"call_abc","output":"ok"},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done?"}]},
	    {"type":"message","role":"user","content":"yes, commit"}
	  ],
	  "tools": [{"type":"function","name":"shell","description":"run","parameters":{"type":"object"},"strict":false}],
	  "tool_choice": "auto",
	  "parallel_tool_calls": true,
	  "reasoning": {"effort":"high","summary":"auto"}
	}`
	in, err := ParseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if in.Store {
		t.Error("store must be false")
	}
	if in.Instructions != "You are Codex." {
		t.Errorf("instructions = %q", in.Instructions)
	}
	req := in.Request
	if req.ThinkingEnabled == nil || !*req.ThinkingEnabled {
		t.Error("reasoning effort=high must enable thinking")
	}
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	// system(instructions) + user + assistant(tool_call) + tool + assistant + user
	want := []string{"system", "user", "assistant", "tool", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	tc := req.Messages[2].ToolCalls[0]
	if tc.ID != "call_abc" || tc.Function.Name != "shell" || tc.Function.Arguments != `{"cmd":"go test"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if req.Messages[3].ToolCallID != "call_abc" || req.Messages[3].Content != "ok" {
		t.Fatalf("tool output = %+v", req.Messages[3])
	}
	if len(in.InputItems) != 6 {
		t.Fatalf("canonical items = %d, want 6 (verbatim for transcript)", len(in.InputItems))
	}
	// tools flattened → OpenAI envelope
	if !strings.Contains(string(req.Tools[0]), `"type":"function"`) || !strings.Contains(string(req.Tools[0]), `"name":"shell"`) {
		t.Fatalf("tools = %s", req.Tools[0])
	}
	if req.Passthrough["parallel_tool_calls"] != true {
		t.Fatalf("passthrough = %+v", req.Passthrough)
	}
}

// 能力拒绝:background/include/item_reference/多模态/服务端工具项/truncation。
func TestParseResponsesRequest_ExplicitRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"background", `{"model":"m","input":"x","background":true}`, "background"},
		{"include encrypted", `{"model":"m","input":"x","include":["reasoning.encrypted_content"]}`, "include"},
		{"item_reference", `{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`, "item_reference"},
		{"input_image", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x"}]}]}`, "input_image"},
		{"server tool item", `{"model":"m","input":[{"type":"web_search_call","id":"ws_1"}]}`, "web_search_call"},
		{"non-function tool", `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`, "web_search"},
		{"truncation auto", `{"model":"m","input":"x","truncation":"auto"}`, "truncation"},
		{"json_schema text format", `{"model":"m","input":"x","text":{"format":{"type":"json_schema","name":"s","schema":{}}}}`, "text"},
		{"missing input", `{"model":"m"}`, "input"},
		{"bad max_output_tokens", `{"model":"m","input":"x","max_output_tokens":0}`, "max_output_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseResponsesRequest([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 非流式响应对象:status/output items/usage 口径(Responses input 含 cache)。
func TestResponsesFromCompletion_Shape(t *testing.T) {
	payload := `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-name",
	  "choices":[{"index":0,"finish_reason":"stop","message":{
	    "role":"assistant","content":"All set.","reasoning_content":"some thinking",
	    "tool_calls":[{"id":"call_9","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]
	  }}],
	  "usage":{"prompt_tokens":50,"completion_tokens":12,"total_tokens":62,
	    "prompt_tokens_details":{"cached_tokens":20},
	    "completion_tokens_details":{"reasoning_tokens":5}}}`
	body, items, err := ResponsesFromCompletion([]byte(payload), "resp_test1", "gpt-public", 1700000000, true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["id"] != "resp_test1" || resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("resp = %v", resp)
	}
	if resp["model"] != "gpt-public" {
		t.Errorf("public model mapping: %v", resp["model"])
	}
	usage := resp["usage"].(map[string]any)
	// Responses 口径:input 含 cached(50 已含 20,不重复加)。
	if usage["input_tokens"].(float64) != 50 {
		t.Errorf("input_tokens = %v, want 50 (inclusive)", usage["input_tokens"])
	}
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"].(float64) != 20 {
		t.Errorf("cached_tokens = %v", usage["input_tokens_details"])
	}
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"].(float64) != 5 {
		t.Errorf("reasoning_tokens = %v", usage["output_tokens_details"])
	}
	output := resp["output"].([]any)
	// reasoning + message + function_call
	if len(output) != 3 {
		t.Fatalf("output items = %d, want 3", len(output))
	}
	fc := output[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["name"] != "shell" {
		t.Fatalf("function_call item = %v", fc)
	}
	// transcript items exclude reasoning.
	if len(items) != 2 {
		t.Fatalf("transcript items = %d, want 2 (reasoning excluded)", len(items))
	}
	// Anthropic-origin wire (cacheReadInInput=false): input 加回 cache。
	_, _, err = ResponsesFromCompletion([]byte(payload), "r2", "m", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	payloadNoReasoning := strings.Replace(payload, `,"reasoning_content":"some thinking"`, "", 1)
	_ = payloadNoReasoning
}

// length → status incomplete + incomplete_details。
func TestResponsesFromCompletion_IncompleteOnLength(t *testing.T) {
	payload := `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"trunc"}}]}`
	body, _, err := ResponsesFromCompletion([]byte(payload), "resp_l", "m", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"status":"incomplete"`) || !strings.Contains(s, `"reason":"max_output_tokens"`) {
		t.Fatalf("resp = %s", s)
	}
}

// 流式黄金序列:created → in_progress → message item(text deltas) →
// function_call item(args deltas) → completed(带 usage 与组装 output)。
func TestTranslateOpenAIToResponsesStream_GoldenSequence(t *testing.T) {
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_z","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
	}
	var wire strings.Builder
	for _, c := range chunks {
		wire.WriteString("data: " + c + "\n\n")
	}
	wire.WriteString("data: [DONE]\n\n")

	r, state := TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire.String())),
		"resp_g1", "gpt-public", 1700000000, true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	anchors := []string{
		"event: response.created", `"status":"in_progress"`,
		"event: response.in_progress",
		"event: response.output_item.added", `"type":"message"`,
		"event: response.content_part.added",
		`"delta":"Hel"`,
		`"delta":"lo"`,
		"event: response.output_text.done", `"text":"Hello"`,
		"event: response.content_part.done",
		"event: response.output_item.done",
		`"call_id":"call_z"`, `"name":"shell"`, `"type":"function_call"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done", `"arguments":"{\"cmd\":\"ls\"}"`,
		"event: response.completed",
		`"status":"completed"`, `"input_tokens":9`, `"output_tokens":4`,
	}
	cursor := 0
	for _, a := range anchors {
		i := strings.Index(s[cursor:], a)
		if i < 0 {
			t.Errorf("missing anchor %q after offset %d in:\n%s", a, cursor, s)
			continue
		}
		cursor += i + len(a)
	}
	if !state.Completed() {
		t.Error("state.Completed must be true after [DONE]")
	}
	items := state.FinalOutputItems()
	if len(items) != 2 {
		t.Fatalf("final output items = %d, want 2 (message + function_call)", len(items))
	}
	if !strings.Contains(string(items[1]), `"call_id":"call_z"`) {
		t.Errorf("call_id not preserved in assembled item: %s", items[1])
	}
}

// 中断语义:EOF 无 [DONE] → error 事件,绝无 response.completed;不落链。
func TestTranslateOpenAIToResponsesStream_BrokenEnd(t *testing.T) {
	wire := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n"
	r, state := TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire)), "resp_b", "m", 1, true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "event: error") {
		t.Errorf("missing error event:\n%s", s)
	}
	if strings.Contains(s, "response.completed") {
		t.Errorf("broken stream must never emit response.completed:\n%s", s)
	}
	if state.Completed() {
		t.Error("state.Completed must be false on broken stream (chain not persisted)")
	}
}

// 会话链回放:transcript items + 新输入 → 完整消息序列(ID 保留)。
func TestResponsesReplayToChat_ChainReplay(t *testing.T) {
	transcript := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"r1"}`),
	}
	newInput := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":"q2"}`),
	}
	msgs, err := ResponsesReplayToChat(append(transcript, newInput...))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[1].ToolCalls[0].ID != "call_1" || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("replayed = %+v", msgs)
	}
}
