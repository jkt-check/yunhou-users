package providers

import (
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestParseAnthropicMessagesRequest_RejectsTextAfterToolUse(t *testing.T) {
	for _, blocks := range []string{
		`{"type":"text","text":"before"},{"type":"tool_use","id":"call_a","name":"lookup","input":{}},{"type":"text","text":"after"}`,
		`{"type":"tool_use","id":"call_a","name":"lookup","input":{}},{"type":"text","text":"between"},{"type":"tool_use","id":"call_b","name":"lookup","input":{}}`,
	} {
		_, err := ParseAnthropicMessagesRequest([]byte(`{"model":"m","max_tokens":20,"messages":[
			{"role":"user","content":"q"},{"role":"assistant","content":[` + blocks + `]}]}`))
		if domain.CodeOf(err) != domain.CodeInvalidInput || !strings.Contains(err.Error(), "order") {
			t.Fatalf("err=%v, want explicit invalid_input rather than reordered history", err)
		}
	}
}

func TestParseAnthropicMessagesRequest_TextBeforeParallelTools(t *testing.T) {
	req, err := ParseAnthropicMessagesRequest([]byte(`{"model":"m","max_tokens":20,"messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"text","text":"before"},
			{"type":"tool_use","id":"call_a","name":"first","input":{}},
			{"type":"text","text":""},
			{"type":"tool_use","id":"call_b","name":"second","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"a"},
			{"type":"tool_result","tool_use_id":"call_b","content":"b"},
			{"type":"text","text":"after"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages=%+v, want user, assistant, tool, tool, user", req.Messages)
	}
	assistant := req.Messages[1]
	if assistant.Content != "before" || len(assistant.ToolCalls) != 2 ||
		assistant.ToolCalls[0].ID != "call_a" || assistant.ToolCalls[1].ID != "call_b" ||
		req.Messages[2].ToolCallID != "call_a" || req.Messages[3].ToolCallID != "call_b" ||
		req.Messages[4].Content != "after" {
		t.Fatalf("history changed order: %+v", req.Messages)
	}
}
