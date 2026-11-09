package providers

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

func TestEveryAdapter_PreservesExplicitReasoningToggle(t *testing.T) {
	on, off := true, false
	for protocol, adapter := range RegisteredAdapters() {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				toggle *bool
				want   string
			}{
				{name: "omitted"},
				{name: "disabled", toggle: &off, want: "disabled"},
				{name: "enabled", toggle: &on, want: "enabled"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, tc.name), func(t *testing.T) {
					body, err := adapter.BuildPayload(testCall("m", stream, 8192,
						[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, tc.toggle))
					if err != nil {
						t.Fatal(err)
					}
					payload := decode(t, body)
					key, field, want := "thinking", "type", tc.want
					if protocol == domain.ProtocolOpenAIResponses {
						key, field = "reasoning", "effort"
						switch want {
						case "disabled":
							want = "none"
						case "enabled":
							want = "medium"
						}
					}
					if tc.toggle == nil {
						if _, ok := payload[key]; ok {
							t.Fatalf("omitted toggle must preserve upstream default: %s", body)
						}
						return
					}
					value, ok := payload[key].(map[string]any)
					if !ok || value[field] != want {
						t.Fatalf("explicit %s was lost: %s, want %s.%s=%s",
							tc.name, body, key, field, want)
					}
					if tc.toggle == &off && len(value) != 1 {
						t.Fatalf("disabled reasoning must not carry an enabled budget/summary: %s", body)
					}
				})
			}
		}
	}
}

func TestOpenAIAdapters_ExplicitEffortDoesNotEmitThinkingToggle(t *testing.T) {
	off := false
	for _, adapter := range []Adapter{NewOpenAIChat(), NewOpenAIResponses()} {
		t.Run(string(adapter.Protocol()), func(t *testing.T) {
			call := testCall("m", false, 8192,
				[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, &off)
			call.Request.ReasoningEffort = "high"
			body, err := adapter.BuildPayload(call)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if _, ok := payload["thinking"]; ok {
				t.Fatalf("explicit effort must not emit a conflicting toggle: %s", body)
			}
		})
	}
}
