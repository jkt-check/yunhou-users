package httpapi

import (
	"fmt"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestChatCompletions_RejectInvalidThinkingType(t *testing.T) {
	for _, thinking := range []string{`{}`, `{"type":""}`, `{"type":"adaptive"}`, `{"type":"sometimes"}`, `{"type":null}`, `"enabled"`, `[]`, `true`} {
		for _, extra := range []string{"", `,"thinking_enabled":false`, `,"thinking_enabled":true`, `,"reasoning_effort":"high"`} {
			t.Run(thinking+extra, func(t *testing.T) {
				req, err := parseChatCompletions([]byte(fmt.Sprintf(
					`{"model":"m","messages":[{"role":"user","content":"q"}],"thinking":%s%s}`, thinking, extra)))
				if err == nil || req != nil || domain.CodeOf(err) != domain.CodeInvalidInput {
					t.Fatalf("invalid thinking must be rejected before selecting a toggle: req=%+v err=%v", req, err)
				}
			})
		}
	}
}

func TestChatCompletions_ValidThinkingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		want        *bool
		effort      string
	}{
		{"omitted", "", nil, ""},
		{"null", `,"thinking":null`, nil, ""},
		{"enabled", `,"thinking":{"type":"enabled"}`, boolPtr(true), ""},
		{"disabled", `,"thinking":{"type":"disabled"}`, boolPtr(false), ""},
		{"boolean_wins", `,"thinking":{"type":"enabled"},"thinking_enabled":false`, boolPtr(false), ""},
		{"effort_preserved", `,"thinking":{"type":"disabled"},"reasoning_effort":"high"`, boolPtr(false), "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseChatCompletions([]byte(`{"model":"m","messages":[{"role":"user","content":"q"}]` + tc.extra + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if (req.ThinkingEnabled == nil) != (tc.want == nil) ||
				tc.want != nil && *req.ThinkingEnabled != *tc.want || req.ReasoningEffort != tc.effort {
				t.Errorf("valid precedence changed: %+v", req)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
