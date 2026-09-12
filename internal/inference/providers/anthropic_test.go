package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// Adapted from the candidate branch internal/llm/anthropic_test.go
// (BuildAnthropicPayload suite) onto the Adapter/Call surface, plus the new
// non-stream decode and rejection-of-unsupported-passthrough coverage.

func TestAnthropicPayload_BasicAndSystem(t *testing.T) {
	body, err := NewAnthropicMessages().BuildPayload(testCall("kimi-k3", true, 8192, []model.ChatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "system", Content: "answer in Chinese"},
		{Role: "user", Content: "hi"},
	}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	p := decode(t, body)
	if p["model"] != "kimi-k3" {
		t.Errorf("model = %v", p["model"])
	}
	if p["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, want the forced cap 8192", p["max_tokens"])
	}
	if p["stream"] != true {
		t.Errorf("stream = %v", p["stream"])
	}
	if p["system"] != "be brief\n\nanswer in Chinese" {
		t.Errorf("system = %v", p["system"])
	}
	msgs := p["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1 (system hoisted)", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("role = %v", m0["role"])
	}
	content := m0["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hi" {
		t.Errorf("content = %v", content)
	}
}

func TestAnthropicPayload_ToolLoop(t *testing.T) {
	body, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, []model.ChatMessage{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", Content: "file_a\nfile_b", ToolCallID: "call_1"},
		{Role: "tool", Content: "done", ToolCallID: "call_2"},
		{Role: "assistant", Content: "here you go"},
	}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	p := decode(t, body)
	msgs := p["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (two tool results merged into one user msg)", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1].role = %v", asst["role"])
	}
	blocks := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %d, want 1 (empty text content skipped)", len(blocks))
	}
	tu := blocks[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "run_shell" {
		t.Errorf("tool_use block = %v", tu)
	}
	if tu["input"].(map[string]any)["cmd"] != "ls" {
		t.Errorf("tool_use input = %v", tu["input"])
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool result must ride a user message, got %v", toolMsg["role"])
	}
	trs := toolMsg["content"].([]any)
	if len(trs) != 2 {
		t.Fatalf("merged tool_result blocks = %d, want 2", len(trs))
	}
	tr0 := trs[0].(map[string]any)
	if tr0["type"] != "tool_result" || tr0["tool_use_id"] != "call_1" || tr0["content"] != "file_a\nfile_b" {
		t.Errorf("tool_result = %v", tr0)
	}
}

func TestAnthropicPayload_ToolsTranslation(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"run_shell","description":"run cmd","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}`),
		json.RawMessage(`{"name":"list_dir","input_schema":{"type":"object"}}`),
	}
	body, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	p := decode(t, body)
	ts := p["tools"].([]any)
	if len(ts) != 2 {
		t.Fatalf("tools len = %d", len(ts))
	}
	t0 := ts[0].(map[string]any)
	if t0["name"] != "run_shell" || t0["description"] != "run cmd" {
		t.Errorf("tool[0] = %v", t0)
	}
	schema := t0["input_schema"].(map[string]any)
	if schema["properties"] == nil {
		t.Errorf("tool[0] input_schema missing parameters passthrough: %v", schema)
	}
	t1 := ts[1].(map[string]any)
	if t1["name"] != "list_dir" || t1["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool[1] = %v", t1)
	}
	if strings.Contains(string(body), `"function"`) {
		t.Errorf("OpenAI tool envelope leaked into Anthropic payload: %s", body)
	}
}

// A tool without a name can no longer be silently dropped (设计: 不能静默
// 丢字段) — the build rejects it explicitly before any upstream spend.
func TestAnthropicPayload_RejectsNamelessTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"description":"no name here","parameters":{"type":"object"}}}`),
	}
	_, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, nil))
	if err == nil || domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input (nameless tool)", err)
	}
}

// TestAnthropicPayload_RejectsAnthropicIllegalShapes: two shapes pass
// handler validation and are legal for OpenAI-protocol models, but stock
// Anthropic 400s them — the translation must fail deliberately (as
// invalid_input, before any paid round-trip).
func TestAnthropicPayload_RejectsAnthropicIllegalShapes(t *testing.T) {
	cases := []struct {
		name     string
		messages []model.ChatMessage
	}{
		{"system-only request", []model.ChatMessage{
			{Role: "system", Content: "be brief"},
		}},
		{"leading assistant with no preceding user", []model.ChatMessage{
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "hi"},
		}},
		{"system then leading assistant", []model.ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "assistant", Content: "hello"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, tc.messages, nil, nil))
			if domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("err = %v, want invalid_input", err)
			}
		})
	}
}

func TestAnthropicPayload_Thinking(t *testing.T) {
	thinking := true
	body, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	p := decode(t, body)
	th := p["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(anthropicThinkingBudget) {
		t.Errorf("thinking = %v", th)
	}
	if p["max_tokens"].(float64) <= th["budget_tokens"].(float64) {
		t.Errorf("max_tokens %v must exceed budget_tokens", p["max_tokens"])
	}
}

// 评审轮1 I5：thinking 预算容不进 cap 时显式 400（能力错误），绝不把
// max_tokens 提到客户声明的 OutputCap 之上让上游超产多收。
func TestAnthropicPayload_ThinkingCapTooSmallRejects(t *testing.T) {
	thinking := true
	for _, cap := range []int64{1024, 1025} {
		_, err := NewAnthropicMessages().BuildPayload(testCall("m", true, cap, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking))
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("cap %d: err = %v, want invalid_input (raise max_tokens or disable thinking)", cap, err)
		}
	}
	// 最小合法 cap：预算钳到 cap-1（≥ 1024 下限），max_tokens 不动。
	body, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 1026, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking))
	if err != nil {
		t.Fatalf("cap 1026: %v", err)
	}
	p := decode(t, body)
	th := p["thinking"].(map[string]any)
	if p["max_tokens"].(float64) != 1026 {
		t.Errorf("max_tokens = %v, want 1026 (cap 绝不上调)", p["max_tokens"])
	}
	if th["budget_tokens"].(float64) != 1025 {
		t.Errorf("budget_tokens = %v, want 1025 (clamped to cap-1)", th["budget_tokens"])
	}
}

// 计费不变式：任何 cap/预算组合下，发给上游的 max_tokens ≤ 准入 OutputCap，
// budget_tokens < max_tokens。
func TestAnthropicPayload_ThinkingNeverExceedsOutputCap(t *testing.T) {
	thinking := true
	for _, cap := range []int64{1026, 2048, 4096, 5000, 8192, 32768} {
		for _, budget := range []int64{0, 1024, 4096, 16384, 1 << 20} {
			call := testCall("m", true, cap, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking)
			if budget > 0 {
				call.Request.ThinkingBudget = &budget
			}
			body, err := NewAnthropicMessages().BuildPayload(call)
			if err != nil {
				t.Fatalf("cap %d budget %d: %v", cap, budget, err)
			}
			p := decode(t, body)
			maxTokens := int64(p["max_tokens"].(float64))
			if maxTokens > cap {
				t.Errorf("cap %d budget %d: upstream max_tokens %d exceeds the admitted OutputCap", cap, budget, maxTokens)
			}
			th := p["thinking"].(map[string]any)
			bt := int64(th["budget_tokens"].(float64))
			if bt >= maxTokens {
				t.Errorf("cap %d budget %d: budget_tokens %d must stay below max_tokens %d", cap, budget, bt, maxTokens)
			}
			if bt < anthropicMinThinkingBudget {
				t.Errorf("cap %d budget %d: budget_tokens %d below the protocol floor %d", cap, budget, bt, anthropicMinThinkingBudget)
			}
		}
	}
}

func TestAnthropicPayload_MergesConsecutiveSameRole(t *testing.T) {
	body, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 8192, []model.ChatMessage{
		{Role: "user", Content: "one"},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: "a1"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "three"},
	}, nil, nil))
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	msgs := decode(t, body)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (consecutive same-role turns merged)", len(msgs))
	}
	user := msgs[0].(map[string]any)
	ub := user["content"].([]any)
	if len(ub) != 2 || ub[0].(map[string]any)["text"] != "one" || ub[1].(map[string]any)["text"] != "two" {
		t.Errorf("merged user blocks = %v, want [one two]", ub)
	}
	asst := msgs[1].(map[string]any)
	ab := asst["content"].([]any)
	if len(ab) != 2 || ab[0].(map[string]any)["text"] != "a1" || ab[1].(map[string]any)["text"] != "a2" {
		t.Errorf("merged assistant blocks = %v, want [a1 a2]", ab)
	}
}

// The zero/negative output cap cannot be forwarded: Anthropic REQUIRES a
// positive max_tokens and the design forbids unlimited output.
func TestAnthropicPayload_RequiresOutputCap(t *testing.T) {
	_, err := NewAnthropicMessages().BuildPayload(testCall("m", true, 0, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, nil))
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input (no output cap)", err)
	}
}

// OpenAI-only sampling knobs are rejected explicitly, never silently
// dropped (设计: 不能静默丢字段).
func TestAnthropicPayload_RejectsUnsupportedPassthrough(t *testing.T) {
	call := testCall("m", true, 100, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, nil)
	call.Request.Passthrough = map[string]any{"presence_penalty": 0.5}
	_, err := NewAnthropicMessages().BuildPayload(call)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input (unsupported passthrough)", err)
	}
	call.Request.Passthrough = map[string]any{"temperature": 0.5, "stop": []string{"\n"}}
	if _, err := NewAnthropicMessages().BuildPayload(call); err != nil {
		t.Errorf("temperature/stop must map: %v", err)
	}
}

func TestAnthropicDecodeNonStream_TextAndTools(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"kimi-k3",
		"content":[
			{"type":"thinking","thinking":"hmm"},
			{"type":"text","text":"answer"},
			{"type":"tool_use","id":"toolu_1","name":"run_shell","input":{"cmd":"ls"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":25,"output_tokens":17,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}`)
	res, err := NewAnthropicMessages().DecodeNonStream(body)
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if !res.UsageReported {
		t.Fatal("usage must be reported")
	}
	u := res.Usage
	if *u.InputTokens != 25 || *u.OutputTokens != 17 || *u.CacheReadTokens != 5 || *u.CacheWriteTokens != 3 {
		t.Errorf("usage = %+v, want 25/17/5/3", u)
	}
	p := decode(t, res.Payload)
	if p["object"] != "chat.completion" || p["id"] != "msg_1" {
		t.Errorf("payload = %v", p)
	}
	choice := p["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "answer" || msg["reasoning_content"] != "hmm" {
		t.Errorf("message = %v", msg)
	}
	tc := msg["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["function"].(map[string]any)["name"] != "run_shell" ||
		tc["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool_call = %v", tc)
	}
	usage := p["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(25) || usage["completion_tokens"] != float64(17) {
		t.Errorf("client usage = %v", usage)
	}
	// "answer"(6) + "hmm"(3) — 非流式估算的输出侧依据。
	if res.ContentBytes != 9 {
		t.Errorf("ContentBytes = %d, want 9", res.ContentBytes)
	}
}

func TestAnthropicDecodeNonStream_NoUsageIsNotZero(t *testing.T) {
	res, err := NewAnthropicMessages().DecodeNonStream([]byte(
		`{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("DecodeNonStream: %v", err)
	}
	if res.UsageReported {
		t.Error("UsageReported = true, want false — missing usage is never zero")
	}
	if res.Usage.InputTokens != nil {
		t.Errorf("usage = %+v, want nil buckets", res.Usage)
	}
}
