package providers

import (
	"io"
	"strings"
	"testing"
)

func TestNativeSSE_OptionalDataSpace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter Adapter
		events  []string
	}{
		{
			name: "anthropic", adapter: NewAnthropicMessages(),
			events: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "responses", adapter: NewOpenAIResponses(),
			events: []string{
				`{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_text.delta","output_index":0,"delta":"ok"}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
				`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, spacing := range []struct{ name, value string }{
				{"none", ""}, {"one", " "}, {"multiple", "   "},
			} {
				for _, newline := range []struct{ name, value string }{
					{"LF", "\n"}, {"CRLF", "\r\n"},
				} {
					t.Run(spacing.name+"/"+newline.name, func(t *testing.T) {
						prefix := "data:" + spacing.value
						// Metadata, comments and malformed data must not prevent later events.
						wire := ": keepalive\nid: event_1\nevent: ignored\n" + prefix + "not-json\n\n"
						wire += prefix + strings.Join(tc.events, "\n\n"+prefix) + "\n\n"
						wire = strings.ReplaceAll(wire, "\n", newline.value)
						st := tc.adapter.WrapStream(io.NopCloser(strings.NewReader(wire)))
						defer st.Body.Close()
						out, err := io.ReadAll(st.TeeBody())
						if err != nil {
							t.Fatal(err)
						}
						for _, want := range []string{`"content":"ok"`, `"id":"call_1"`, `"arguments":"{}"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
							if !strings.Contains(string(out), want) {
								t.Errorf("missing %s in translated stream: %s", want, out)
							}
						}
						got := st.Tap.Result()
						if !got.Terminal || !got.SawUsage || got.ContentBytes != 4 ||
							got.Buckets.InputTokens == nil || *got.Buckets.InputTokens != 1 ||
							got.Buckets.OutputTokens == nil || *got.Buckets.OutputTokens != 2 {
							t.Errorf("tap=%+v, want terminal/usage, 4 bytes, tokens 1/2", got)
						}
					})
				}
			}
		})
	}
}
