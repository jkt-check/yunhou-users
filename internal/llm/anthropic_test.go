package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// decode is a test helper: unmarshal the payload into a generic map.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, body)
	}
	return m
}

func TestBuildAnthropicPayload_BasicAndSystem(t *testing.T) {
	body, err := BuildAnthropicPayload("kimi-k3", 0, []model.ChatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "system", Content: "answer in Chinese"},
		{Role: "user", Content: "hi"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	if p["model"] != "kimi-k3" {
		t.Errorf("model = %v", p["model"])
	}
	if p["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, want default 8192", p["max_tokens"])
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

func TestBuildAnthropicPayload_ToolLoop(t *testing.T) {
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", Content: "file_a\nfile_b", ToolCallID: "call_1"},
		{Role: "tool", Content: "done", ToolCallID: "call_2"},
		{Role: "assistant", Content: "here you go"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	msgs := p["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (two tool results merged into one user msg)", len(msgs))
	}
	// assistant turn: single tool_use block, input parsed from arguments string
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
	// consecutive tool results merged into ONE user message
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

func TestBuildAnthropicPayload_ToolsTranslation(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"run_shell","description":"run cmd","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}`),
		json.RawMessage(`{"name":"list_dir","input_schema":{"type":"object"}}`),
	}
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
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
	// OpenAI envelope keys must not leak through
	if strings.Contains(string(body), `"function"`) {
		t.Errorf("OpenAI tool envelope leaked into Anthropic payload: %s", body)
	}
}

// TestBuildAnthropicPayload_RejectsAnthropicIllegalShapes: two shapes pass
// handler validation and are legal for OpenAI-protocol models, but stock
// Anthropic 400s them — the translation must fail deliberately instead of
// buying an opaque upstream 502 with a paid round-trip.
func TestBuildAnthropicPayload_RejectsAnthropicIllegalShapes(t *testing.T) {
	cases := []struct {
		name     string
		messages []model.ChatMessage
	}{
		{"system-only request (Anthropic requires >= 1 non-system message)", []model.ChatMessage{
			{Role: "system", Content: "be brief"},
		}},
		{"leading assistant with no preceding user (Anthropic requires user-first)", []model.ChatMessage{
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
			body, err := BuildAnthropicPayload("m", 0, tc.messages, nil, nil)
			if err == nil {
				t.Errorf("BuildAnthropicPayload succeeded, want a deliberate error; payload: %s", body)
			}
		})
	}
}

// TestBuildOpenAIPayload_AcceptsShapesAnthropicRejects: the guards are scoped
// to the Anthropic translator — the OpenAI payload builder must keep
// accepting system-only and leading-assistant histories unchanged.
func TestBuildOpenAIPayload_AcceptsShapesAnthropicRejects(t *testing.T) {
	for _, messages := range [][]model.ChatMessage{
		{{Role: "system", Content: "be brief"}},
		{{Role: "assistant", Content: "hello"}, {Role: "user", Content: "hi"}},
	} {
		if _, err := BuildOpenAIPayload("m", messages, nil, nil); err != nil {
			t.Errorf("BuildOpenAIPayload(%v): %v, want success (OpenAI-protocol behavior unchanged)", messages, err)
		}
	}
}

// TestBuildAnthropicPayload_SkipsNamelessTools: a tool without a name would
// translate to "name":null and 400 the whole request upstream — drop it
// instead. When every tool is nameless the tools key is omitted entirely.
func TestBuildAnthropicPayload_SkipsNamelessTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"description":"no name here","parameters":{"type":"object"}}}`),
		json.RawMessage(`{"name":"list_dir","input_schema":{"type":"object"}}`),
	}
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	ts := p["tools"].([]any)
	if len(ts) != 1 || ts[0].(map[string]any)["name"] != "list_dir" {
		t.Errorf("tools = %v, want only the named tool", ts)
	}

	allNameless := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"description":"no name"}}`)}
	body, err = BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, allNameless, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	if _, ok := decode(t, body)["tools"]; ok {
		t.Errorf("all-nameless tools must omit the tools key entirely: %s", body)
	}
}

func TestBuildAnthropicPayload_Thinking(t *testing.T) {
	thinking := true
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	th := p["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(4096) {
		t.Errorf("thinking = %v", th)
	}
	if p["max_tokens"].(float64) <= th["budget_tokens"].(float64) {
		t.Errorf("max_tokens %v must exceed budget_tokens", p["max_tokens"])
	}
}

// TestBuildAnthropicPayload_MergesConsecutiveSameRole: handler validation
// deliberately allows consecutive same-role messages (OpenAI accepts them),
// but Anthropic requires strict user/assistant alternation and 400s — the
// translation must merge such turns into one.
func TestBuildAnthropicPayload_MergesConsecutiveSameRole(t *testing.T) {
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{
		{Role: "user", Content: "one"},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: "a1"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "three"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	msgs := decode(t, body)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (consecutive same-role turns merged)", len(msgs))
	}
	user := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("msgs[0].role = %v", user["role"])
	}
	ub := user["content"].([]any)
	if len(ub) != 2 || ub[0].(map[string]any)["text"] != "one" || ub[1].(map[string]any)["text"] != "two" {
		t.Errorf("merged user blocks = %v, want [one two]", ub)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1].role = %v", asst["role"])
	}
	ab := asst["content"].([]any)
	if len(ab) != 2 || ab[0].(map[string]any)["text"] != "a1" || ab[1].(map[string]any)["text"] != "a2" {
		t.Errorf("merged assistant blocks = %v, want [a1 a2]", ab)
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" || last["content"].([]any)[0].(map[string]any)["text"] != "three" {
		t.Errorf("msgs[2] = %v", last)
	}
}

// TestBuildAnthropicPayload_ToolAfterPlainUserMerges: a tool result following
// a plain user message must not open a second adjacent user turn; the
// tool_result block merges in ahead of the text (tool_results lead the turn).
func TestBuildAnthropicPayload_ToolAfterPlainUserMerges(t *testing.T) {
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "tool", Content: "res", ToolCallID: "c1"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	msgs := decode(t, body)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1 (tool result merged into the user turn)", len(msgs))
	}
	blocks := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("merged blocks = %d, want 2", len(blocks))
	}
	if b := blocks[0].(map[string]any); b["type"] != "tool_result" || b["tool_use_id"] != "c1" {
		t.Errorf("blocks[0] = %v, want the tool_result first", b)
	}
	if b := blocks[1].(map[string]any); b["type"] != "text" || b["text"] != "hi" {
		t.Errorf("blocks[1] = %v, want the text after", b)
	}
}

// TestBuildAnthropicPayload_UserTextAfterToolResult: a plain user text
// following tool results merges into the same user turn, tool_result blocks
// first, then the text (the shape Anthropic accepts).
func TestBuildAnthropicPayload_UserTextAfterToolResult(t *testing.T) {
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "c1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", Content: "file_a", ToolCallID: "c1"},
		{Role: "user", Content: "thanks"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	msgs := decode(t, body)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (user, assistant, merged user)", len(msgs))
	}
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("msgs[2].role = %v", user["role"])
	}
	blocks := user["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("merged blocks = %d, want 2", len(blocks))
	}
	if b := blocks[0].(map[string]any); b["type"] != "tool_result" || b["content"] != "file_a" {
		t.Errorf("blocks[0] = %v, want tool_result first", b)
	}
	if b := blocks[1].(map[string]any); b["type"] != "text" || b["text"] != "thanks" {
		t.Errorf("blocks[1] = %v, want the user text last", b)
	}
}
