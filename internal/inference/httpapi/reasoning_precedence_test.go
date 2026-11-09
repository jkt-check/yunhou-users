package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

func TestChatCompletions_ConflictingReasoningUsesExplicitEffort(t *testing.T) {
	req, err := parseChatCompletions([]byte(`{"model":"m",
		"messages":[{"role":"user","content":"q"}],
		"thinking_enabled":false,"reasoning_effort":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []providers.Adapter{providers.NewOpenAIChat(), providers.NewOpenAIResponses()} {
		body, err := adapter.BuildPayload(&providers.Call{
			Request: req, OutputCap: 2048,
			Deployment: &domain.Deployment{UpstreamModel: "upstream"},
		})
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Effort    string `json:"reasoning_effort"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		got := payload.Effort
		if adapter.Protocol() == domain.ProtocolOpenAIResponses {
			got = payload.Reasoning.Effort
		}
		if got != "high" {
			t.Errorf("%s: effort=%q, want high", adapter.Protocol(), got)
		}
	}
}
