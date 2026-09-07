package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestBuildOpenAIPayload_Minimal(t *testing.T) {
	body, err := BuildOpenAIPayload("deepseek-chat",
		[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"model":"deepseek-chat"`, `"stream":true`, `"content":"hi"`, `"include_usage":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"tools"`) || strings.Contains(s, `"thinking"`) {
		t.Errorf("payload must not contain tools/thinking: %s", s)
	}
}

func TestBuildOpenAIPayload_ToolsAndThinking(t *testing.T) {
	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	body, err := BuildOpenAIPayload("m", []model.ChatMessage{{Role: "user", Content: "x"}}, tools, &thinking)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
}

func TestExtractStreamUsage(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\n" +
		"data: [DONE]\n\n"
	in, out, ok := ExtractStreamUsage([]byte(raw))
	if !ok || in != 12 || out != 34 {
		t.Errorf("ExtractStreamUsage = (%d, %d, %v), want (12, 34, true)", in, out, ok)
	}
}

func TestExtractStreamUsage_Absent(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	if _, _, ok := ExtractStreamUsage([]byte(raw)); ok {
		t.Error("ok = true, want false when no usage chunk present")
	}
}
