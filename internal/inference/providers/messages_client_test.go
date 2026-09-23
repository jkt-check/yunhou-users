// messages_client_test.go — Messages 客户端面翻译的契约级测试（Task 13）。
// Fixture 取自 Anthropic Messages API 2023-06-01 官方文档形状与 Claude
// Code 实际请求特征（多轮 tool_use/tool_result、system 数组、thinking 块）。

package providers

import (
	"io"
	"strings"
	"testing"
)

// 多轮工具调用历史:tool_use id 与 tool_result.tool_use_id 必须逐字保留
// 到内部形状(验收:多轮工具调用 ID 保留).
func TestParseAnthropicMessagesRequest_ToolRoundTripPreservesIDs(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-4-5",
	  "max_tokens": 8192,
	  "system": [{"type":"text","text":"You are a coding agent.","cache_control":{"type":"ephemeral"}}],
	  "messages": [
	    {"role":"user","content":"list files"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"let me run ls","signature":"sig1"},
	      {"type":"text","text":"I'll check."},
	      {"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{"command":"ls -la"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_01ABC","content":"a.txt\nb.txt","is_error":false},
	      {"type":"text","text":"what about go files?"}
	    ]}
	  ],
	  "tools": [{"name":"Bash","description":"run shell","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
	  "tool_choice": {"type":"auto"},
	  "thinking": {"type":"enabled","budget_tokens":2048},
	  "temperature": 0.5
	}`
	req, err := ParseAnthropicMessagesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Model != "claude-sonnet-4-5" || req.MaxTokens == nil || *req.MaxTokens != 8192 {
		t.Fatalf("req = %+v", req)
	}
	if req.ThinkingEnabled == nil || !*req.ThinkingEnabled || req.ThinkingBudget == nil || *req.ThinkingBudget != 2048 {
		t.Fatalf("thinking = %+v/%+v", req.ThinkingEnabled, req.ThinkingBudget)
	}
	// system + user + assistant(tool_calls) + tool + user
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	assistant := req.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "toolu_01ABC" {
		t.Fatalf("tool call id not preserved: %+v", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Function.Name != "Bash" ||
		assistant.ToolCalls[0].Function.Arguments != `{"command":"ls -la"}` {
		t.Fatalf("tool call = %+v", assistant.ToolCalls[0])
	}
	tool := req.Messages[3]
	if tool.ToolCallID != "toolu_01ABC" || tool.Content != "a.txt\nb.txt" {
		t.Fatalf("tool result = %+v", tool)
	}
	// tools → OpenAI envelope
	if len(req.Tools) != 1 || !strings.Contains(string(req.Tools[0]), `"name":"Bash"`) {
		t.Fatalf("tools = %s", req.Tools)
	}
	if string(req.ToolChoice) != `"auto"` {
		t.Fatalf("tool_choice = %s", req.ToolChoice)
	}
	if req.Passthrough["temperature"] != 0.5 {
		t.Fatalf("passthrough = %+v", req.Passthrough)
	}
}

// 能力拒绝:多模态块/服务端工具/非法 thinking —— 显式 400,不静默丢弃.
func TestParseAnthropicMessagesRequest_ExplicitRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"image block", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`, "image"},
		{"server tool web_search", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, "web_search"},
		{"thinking budget >= max_tokens", `{"model":"m","max_tokens":2048,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":2048}}`, "budget_tokens"},
		{"thinking budget below floor", `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":100}}`, ">= 1024"},
		{"missing max_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`, "max_tokens"},
		{"system-only / empty blocks", `{"model":"m","max_tokens":10,"system":"hi","messages":[{"role":"user","content":[]}]}`, "content blocks"},
		{"bad role", `{"model":"m","max_tokens":10,"messages":[{"role":"tool","content":"x"}]}`, "role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAnthropicMessagesRequest([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 非流式响应翻译:thinking/text/tool_use 块、stop_reason 映射、usage 的
// cache 口径(cacheReadInInput=true 时 input 扣减 cache_read).
func TestAnthropicMessageFromCompletion_Shape(t *testing.T) {
	payload := `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-name",
	  "choices":[{"index":0,"finish_reason":"tool_calls","message":{
	    "role":"assistant","content":"Let me run that.","reasoning_content":"thinking hard",
	    "tool_calls":[{"id":"call_42","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]
	  }}],
	  "usage":{"prompt_tokens":100,"completion_tokens":25,"total_tokens":125,
	    "prompt_tokens_details":{"cached_tokens":40},
	    "completion_tokens_details":{"reasoning_tokens":10}}}`
	out, err := AnthropicMessageFromCompletion([]byte(payload), "claude-public", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	s := string(out)
	for _, kw := range []string{
		`"type":"message"`, `"role":"assistant"`, `"model":"claude-public"`,
		`"type":"thinking"`, `"thinking":"thinking hard"`,
		`"type":"tool_use"`, `"id":"call_42"`, `"name":"Bash"`,
		`"stop_reason":"tool_use"`,
		`"input_tokens":60`, `"cache_read_input_tokens":40`, `"output_tokens":25`,
	} {
		if !strings.Contains(s, kw) {
			t.Errorf("missing %q in %s", kw, s)
		}
	}
	if strings.Contains(s, "msg_") && !strings.Contains(s, `"id":"msg_`) {
		t.Errorf("id should be msg_-shaped: %s", s)
	}
	// cacheReadInInput=false (Anthropic-origin wire): input 不重复扣减.
	out2, err := AnthropicMessageFromCompletion([]byte(payload), "claude-public", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), `"input_tokens":100`) {
		t.Errorf("anthropic-origin input must not be double-subtracted: %s", out2)
	}
}

// 流式翻译黄金序列:text + tool_use + thinking + usage + [DONE] →
// message_start/content_block_*/message_delta/message_stop;ID 与参数片段
// 逐字;终止 message_delta 携带真实 usage.
func TestTranslateOpenAIToAnthropicStream_GoldenSequence(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`,
	}
	var wire strings.Builder
	for _, c := range chunks {
		wire.WriteString("data: " + c + "\n\n")
	}
	wire.WriteString("data: [DONE]\n\n")

	r := TranslateOpenAIToAnthropicStream(io.NopCloser(strings.NewReader(wire.String())), "msg_test1", "claude-public", true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)

	// 事件顺序断言(顺序游标推进;Go map 序列化键按字典序).
	anchors := []string{
		"event: message_start",
		`"id":"msg_test1"`, `"model":"claude-public"`,
		"event: content_block_start\n", `"type":"thinking"`,
		`"thinking":"think","type":"thinking_delta"`,
		`"type":"text"`,
		`"text":"Hel","type":"text_delta"`,
		`"text":"lo","type":"text_delta"`,
		`"id":"call_7","input":{},"name":"Bash","type":"tool_use"`,
		`"partial_json":"{\"command\":","type":"input_json_delta"`,
		`"partial_json":"\"ls\"}","type":"input_json_delta"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		`"cache_read_input_tokens":3,"input_tokens":8,"output_tokens":7`,
		"event: message_stop",
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
	// content_block 索引:text=1(thinking=0), tool_use=2.
	if !strings.Contains(s, `"index":2,"type":"content_block_start"`) {
		t.Errorf("tool_use block index mismatch:\n%s", s)
	}
}

// 中断语义:EOF 无 [DONE] → 原生 error 事件,绝无 message_stop.
func TestTranslateOpenAIToAnthropicStream_BrokenEnd(t *testing.T) {
	wire := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n"
	r := TranslateOpenAIToAnthropicStream(io.NopCloser(strings.NewReader(wire)), "msg_b", "m", true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "event: error") || !strings.Contains(s, `"type":"api_error"`) {
		t.Errorf("missing native error event:\n%s", s)
	}
	if strings.Contains(s, "message_stop") {
		t.Errorf("broken stream must never emit message_stop:\n%s", s)
	}
}

// 评审轮2 I4：交错 parallel tool calls —— ix0 的 delta 在 ix1 的块开启后
// 才到时，必须仍落在 ix0 自己的 content-block index 上（旧实现会给 ix0
// 先 content_block_stop，迟到的 delta 以已关闭的块 index 发出，Anthropic
// SDK 拒绝）。
func TestTranslateOpenAIToAnthropicStream_InterleavedToolCalls(t *testing.T) {
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_a","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"toolu_b","type":"function","function":{"name":"Read","arguments":"{\"b\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var wire strings.Builder
	for _, c := range chunks {
		wire.WriteString("data: " + c + "\n\n")
	}
	wire.WriteString("data: [DONE]\n\n")

	r := TranslateOpenAIToAnthropicStream(io.NopCloser(strings.NewReader(wire.String())), "msg_il", "m", true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)

	// 事件序列：start(0)+delta(0) → start(1)+delta(1) → delta(0,"1}") →
	// delta(1,"2}") → stop(0) → stop(1) → message_delta → message_stop。
	// 迟到的 ix0 delta 必须在 ix1 的块开启之后、ix0 的 content_block_stop
	// 之前（块保持开启）。帧内 JSON 键按字典序：partial_json 先于 index。
	anchors := []string{
		`"id":"toolu_a","input":{},"name":"Bash","type":"tool_use"`,
		`"partial_json":"{\"a\":"`,
		`"id":"toolu_b","input":{},"name":"Read","type":"tool_use"`,
		`"partial_json":"{\"b\":"`,
		`"partial_json":"1}"`,
		`"index":0,"type":"content_block_delta"`,
		`"partial_json":"2}"`,
		`"index":0,"type":"content_block_stop"`,
		`"index":1,"type":"content_block_stop"`,
		"event: message_delta",
		"event: message_stop",
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
	// 每个块恰好一次 start/stop（不得有截断重开）。
	if strings.Count(s, "event: content_block_start") != 2 || strings.Count(s, "event: content_block_stop") != 2 {
		t.Errorf("want exactly 2 starts + 2 stops (no truncate/reopen):\n%s", s)
	}
}

// 网关注入的内联 error 块 → 原生 error 事件转发,且不重复发.
func TestTranslateOpenAIToAnthropicStream_InbandErrorChunk(t *testing.T) {
	wire := "data: {\"error\":{\"message\":\"upstream stream interrupted\",\"type\":\"server_error\"}}\n\n"
	r := TranslateOpenAIToAnthropicStream(io.NopCloser(strings.NewReader(wire)), "msg_e", "m", true)
	out, _ := io.ReadAll(r)
	s := string(out)
	if strings.Count(s, "event: error") != 1 {
		t.Errorf("want exactly one error event:\n%s", s)
	}
	if !strings.Contains(s, "upstream stream interrupted") {
		t.Errorf("error message not relayed:\n%s", s)
	}
}
