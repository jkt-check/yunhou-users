package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1ExplicitReasoningDisable_ReachesEveryUpstream(t *testing.T) {
	for _, protocol := range []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
	} {
		for _, stream := range []bool{false, true} {
			for _, entry := range []string{"chat_toggle", "chat_object", "messages", "responses"} {
				for _, disable := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/disable=%t", protocol, entry, stream, disable), func(t *testing.T) {
						f := newMatrixFixture(t, protocol)
						path := "/v1/chat/completions"
						request := map[string]any{
							"model": "glm-4.6", "stream": stream,
							"messages": []any{map[string]any{"role": "user", "content": "hi"}},
						}
						switch entry {
						case "chat_toggle":
							if disable {
								request["thinking_enabled"] = false
							}
						case "chat_object":
							if disable {
								request["thinking"] = map[string]any{"type": "disabled"}
							}
						case "messages":
							path = "/v1/messages"
							request["max_tokens"] = 1024
							if disable {
								request["thinking"] = map[string]any{"type": "disabled"}
							}
						case "responses":
							path = "/v1/responses"
							delete(request, "messages")
							request["input"] = "hi"
							if disable {
								request["reasoning"] = map[string]any{"effort": "none"}
							}
						}
						w := f.call(t, http.MethodPost, path, request)
						if w.Code != http.StatusOK {
							t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal([]byte(f.lastUpstreamBody(t)), &payload); err != nil {
							t.Fatal(err)
						}
						key, field, want := "thinking", "type", "disabled"
						if protocol == domain.ProtocolOpenAIResponses {
							key, field, want = "reasoning", "effort", "none"
						}
						if !disable {
							if value, ok := payload[key]; ok {
								t.Fatalf("omitted toggle must preserve upstream default: %s", value)
							}
						} else {
							var value map[string]any
							if err := json.Unmarshal(payload[key], &value); err != nil {
								t.Fatalf("explicit disable missing from %s: %s (%v)", key, payload[key], err)
							}
							if len(value) != 1 || value[field] != want {
								t.Fatalf("disable = %v, want only %s=%s", value, field, want)
							}
						}
						if n := f.upCalls.Load(); n != 1 {
							t.Fatalf("upstream calls = %d, want 1", n)
						}
						if n := f.countRows(t, "inference_usage_records"); n != 1 {
							t.Fatalf("usage rows = %d, want 1", n)
						}
					})
				}
			}
		}
	}
}
