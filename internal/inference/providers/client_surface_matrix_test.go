package providers

import (
	"encoding/json"
	"strings"
	"testing"
)

// client_surface_matrix_test.go — Task 16 覆盖率补强：Messages/Responses
// 解析器与完成体渲染的备选分支矩阵（system 块、tool_choice 各型、
// tool_result is_error、thinking 校验、passthrough 白名单外拒绝、usage
// 形状、finish 映射、会话链回放元素）。

func TestMessagesParser_AltBranches(t *testing.T) {
	mustOK := func(name, body string) *ChatRequest {
		t.Helper()
		req, err := ParseAnthropicMessagesRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return req
	}
	mustErr := func(name, body, want string) {
		t.Helper()
		if _, err := ParseAnthropicMessagesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want mention %q", name, err, want)
		}
	}

	// system 为文本块数组（拼接 \n\n）。
	req := mustOK("system blocks", `{"model":"m","max_tokens":64,
		"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],
		"messages":[{"role":"user","content":"hi"}]}`)
	if len(req.Messages) == 0 {
		t.Fatal("messages empty")
	}
	// system 非文本块 → 400。
	mustErr("system non-text", `{"model":"m","max_tokens":1,
		"system":[{"type":"image","source":{}}],
		"messages":[{"role":"user","content":"hi"}]}`, "system")
	// system 形状非法 → 400。
	mustErr("system shape", `{"model":"m","max_tokens":1,"system":42,
		"messages":[{"role":"user","content":"hi"}]}`, "system")

	// tool_choice 各型：auto/none/any/tool。
	for _, tc := range []string{`{"type":"auto"}`, `{"type":"none"}`, `{"type":"any"}`, `{"type":"tool","name":"run_shell"}`} {
		mustOK("tool_choice "+tc, `{"model":"m","max_tokens":64,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"name":"run_shell","input_schema":{"type":"object"}}],
			"tool_choice":`+tc+`}`)
	}
	// 未知 tool_choice 类型 → 400。
	mustErr("tool_choice bad type", `{"model":"m","max_tokens":64,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"run_shell","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"sometimes"}}`, "tool_choice")
	// disable_parallel_tool_use 接受。
	mustOK("tool_choice disable parallel", `{"model":"m","max_tokens":64,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"run_shell","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)

	// tool_result 带 is_error（文本内容照常传递，标志不转发）。
	req = mustOK("tool_result is_error", `{"model":"m","max_tokens":64,
		"messages":[
		  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"boom","is_error":true}]}
		]}`)
	foundTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "boom") {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("tool_result text not forwarded: %+v", req.Messages)
	}

	// thinking：enabled + budget 校验（<1024、>= max_tokens 拒绝）。
	mustErr("budget too small", `{"model":"m","max_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":512},
		"messages":[{"role":"user","content":"hi"}]}`, "budget_tokens")
	mustErr("budget >= max_tokens", `{"model":"m","max_tokens":2048,
		"thinking":{"type":"enabled","budget_tokens":4096},
		"messages":[{"role":"user","content":"hi"}]}`, "budget_tokens")
	mustErr("bad thinking type", `{"model":"m","max_tokens":2048,
		"thinking":{"type":"auto"},
		"messages":[{"role":"user","content":"hi"}]}`, "thinking")

	// 空内容/空块/非法角色/缺 max_tokens。
	mustErr("empty string content", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":""}]}`, "content")
	mustErr("empty blocks", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[]}]}`, "content")
	mustErr("bad role", `{"model":"m","max_tokens":1,"messages":[{"role":"system2","content":"x"}]}`, "role")
	mustErr("missing max_tokens", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "max_tokens")

	// passthrough：temperature/top_p/top_k/stop_sequences 白名单内放行。
	req = mustOK("passthrough", `{"model":"m","max_tokens":64,
		"temperature":0.7,"top_p":0.9,"top_k":40,"stop_sequences":["END"],
		"messages":[{"role":"user","content":"hi"}]}`)
	if req.Passthrough == nil {
		t.Fatal("passthrough not collected")
	}
	// metadata/cache_control 接受不消费。
	mustOK("metadata+cache_control", `{"model":"m","max_tokens":64,
		"metadata":{"user_id":"u1"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
}

func TestMessagesCompletionRender_AltBranches(t *testing.T) {
	// finish=length → max_tokens；tool_calls → tool_use 块 + stop_reason。
	payload := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[
		  {"id":"call_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
		         "prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":2}}`
	out, err := AnthropicMessageFromCompletion([]byte(payload), "claude-pro", true)
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	found := false
	for _, b := range content {
		blk := b.(map[string]any)
		if blk["type"] == "tool_use" && blk["id"] == "call_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool_use block missing: %s", out)
	}
	usage := msg["usage"].(map[string]any)
	// cacheReadInInput=true：input 10 含缓存读 3 → Anthropic 语义排除后 7。
	if usage["input_tokens"] != float64(7) || usage["cache_read_input_tokens"] != float64(3) ||
		usage["cache_creation_input_tokens"] != float64(2) {
		t.Errorf("usage shape = %v, want input 7 + cache_read 3 + cache_creation 2", usage)
	}
	// finish=length。
	payload2 := strings.Replace(payload, `"finish_reason":"tool_calls"`, `"finish_reason":"length"`, 1)
	payload2 = strings.Replace(payload2, `"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}]`, `"content":"text"`, 1)
	out2, err := AnthropicMessageFromCompletion([]byte(payload2), "claude-pro", false)
	if err != nil {
		t.Fatal(err)
	}
	var msg2 map[string]any
	if err := json.Unmarshal(out2, &msg2); err != nil {
		t.Fatal(err)
	}
	if msg2["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", msg2["stop_reason"])
	}
	// 无法解析 → 显式错误。
	if _, err := AnthropicMessageFromCompletion([]byte(`garbage`), "m", false); err == nil {
		t.Error("unparseable completion must error")
	}
}

func TestResponsesParser_AltBranches(t *testing.T) {
	mustErr := func(name, body, want string) {
		t.Helper()
		if _, err := ParseResponsesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want mention %q", name, err, want)
		}
	}
	mustOK := func(name, body string) {
		t.Helper()
		if _, err := ParseResponsesRequest([]byte(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// input 为字符串（单轮简形）。
	mustOK("string input", `{"model":"m","input":"hello"}`)
	// instructions + 多 item 输入。
	mustOK("instructions", `{"model":"m","instructions":"be terse",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	// function_call_output 项（工具结果）。
	mustOK("function_call_output", `{"model":"m",
		"input":[
		  {"type":"message","role":"user","content":[{"type":"input_text","text":"ls?"}]},
		  {"type":"function_call","call_id":"call_1","name":"run_shell","arguments":"{}"},
		  {"type":"function_call_output","call_id":"call_1","output":"a.txt"}
		]}`)
	// 未知 item 类型 → 400。
	mustErr("unknown item", `{"model":"m","input":[{"type":"mystery","role":"user"}]}`, "")
	// 服务端工具/background/include 拒绝。
	mustErr("server tool", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`, "")
	mustErr("background", `{"model":"m","input":"hi","background":true}`, "background")
	mustErr("include", `{"model":"m","input":"hi","include":["reasoning.encrypted_content"]}`, "include")
	// max_output_tokens 非法 → 400。
	mustErr("bad max_output", `{"model":"m","input":"hi","max_output_tokens":-1}`, "")
}

func TestResponsesReplayAndRender_AltBranches(t *testing.T) {
	// 回放：message/function_call/function_call_output 混合 items → chat。
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"ls?"}]}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"run_shell","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"a.txt"}`),
	}
	msgs, err := ResponsesReplayToChat(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("replay empty")
	}
	// 非法 item → 显式错误。
	if _, err := ResponsesReplayToChat([]json.RawMessage{json.RawMessage(`{bad json`)}); err == nil {
		t.Error("unparseable replay item must error")
	}
	// 完成体渲染：文本 + usage。
	payload := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
	out, chainItems, err := ResponsesFromCompletion([]byte(payload), "resp_1", "codex-pro", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chainItems) == 0 {
		t.Fatal("chain items empty")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["id"] != "resp_1" || doc["status"] != "completed" {
		t.Errorf("response doc = %s", out)
	}
	if _, ok := doc["usage"]; !ok {
		t.Errorf("usage missing: %s", out)
	}
	// finish=length → incomplete。
	payload2 := strings.Replace(payload, `"finish_reason":"stop"`, `"finish_reason":"length"`, 1)
	out2, _, err := ResponsesFromCompletion([]byte(payload2), "resp_2", "codex-pro", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc2 map[string]any
	if err := json.Unmarshal(out2, &doc2); err != nil {
		t.Fatal(err)
	}
	if doc2["status"] != "incomplete" {
		t.Errorf("status = %v, want incomplete (length)", doc2["status"])
	}
}
