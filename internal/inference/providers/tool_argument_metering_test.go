package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestToolArguments_MeteredOnceAcrossAdapters(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adapter     Adapter
		nonStream   string
		events      []string
		mixedEvents []string
	}{
		{
			name: "chat", adapter: NewOpenAIChat(),
			nonStream: `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"lookup","arguments":"{\"q\":\"abc\"}"}},{"id":"b","function":{"name":"lookup","arguments":"{\"n\":2}"}}]}}]}`,
			events: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"lookup","arguments":"{\"q\":"}},{"index":1,"id":"b","function":{"name":"lookup","arguments":"{\"n\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"\"abc\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			},
			mixedEvents: []string{`{"choices":[{"delta":{"content":"ok","reasoning_content":"hmm"}}]}`},
		},
		{
			name: "responses", adapter: NewOpenAIResponses(),
			nonStream: `{"status":"completed","output":[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"q\":\"abc\"}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{\"n\":2}"}]}`,
			events: []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"a","name":"lookup","arguments":""}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"b","name":"lookup","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}`,
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"n\":"}`,
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"2}"}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"abc\"}"}`,
				`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"q\":\"abc\"}"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"q\":\"abc\"}"}}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"q\":\"abc\"}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{\"n\":2}"}]}}`,
			},
			mixedEvents: []string{
				`{"type":"response.output_text.delta","output_index":2,"delta":"ok"}`,
				`{"type":"response.reasoning_summary_text.delta","output_index":3,"delta":"hmm"}`,
			},
		},
		{
			name: "anthropic", adapter: NewAnthropicMessages(),
			nonStream: `{"type":"message","content":[{"type":"tool_use","id":"a","name":"lookup","input":{"q":"abc"}},{"type":"tool_use","id":"b","name":"lookup","input":{"n":2}}],"stop_reason":"tool_use"}`,
			events: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"lookup","input":{}}}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"2}"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"abc\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
				`{"type":"message_stop"}`,
			},
			mixedEvents: []string{
				`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"content_block_delta","index":3,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mixed := range []bool{false, true} {
				name, want := "tool_only", int64(18) // {"q":"abc"} (11) + {"n":2} (7).
				if mixed {
					name, want = "mixed", 23 // Plus "ok" (2) and "hmm" (3).
				}
				t.Run(name, func(t *testing.T) {
					t.Run("nonstream", func(t *testing.T) {
						var doc map[string]any
						if err := json.Unmarshal([]byte(tc.nonStream), &doc); err != nil {
							t.Fatal(err)
						}
						// Schema metadata is not generated output.
						doc["tools"] = json.RawMessage(`[{"name":"schema_only","parameters":{"description":"not generated arguments"}}]`)
						if mixed {
							switch tc.name {
							case "chat":
								msg := doc["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
								msg["content"], msg["reasoning_content"] = "ok", "hmm"
							case "responses":
								doc["output"] = append(doc["output"].([]any),
									map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}},
									map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "hmm"}}})
							case "anthropic":
								doc["content"] = append(doc["content"].([]any),
									map[string]any{"type": "text", "text": "ok"},
									map[string]any{"type": "thinking", "thinking": "hmm"})
							}
						}
						body, err := json.Marshal(doc)
						if err != nil {
							t.Fatal(err)
						}
						res, err := tc.adapter.DecodeNonStream(body)
						if err != nil {
							t.Fatal(err)
						}
						if res.UsageReported || res.ContentBytes != want {
							t.Fatalf("reported=%v bytes=%d, want false/%d", res.UsageReported, res.ContentBytes, want)
						}
					})
					t.Run("stream", func(t *testing.T) {
						events := []string{`{"tools":[{"name":"schema_only","parameters":{"description":"not generated arguments"}}]}`}
						if mixed {
							events = append(events, tc.mixedEvents...)
						}
						events = append(events, tc.events...)
						wire := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
						st := tc.adapter.WrapStream(io.NopCloser(strings.NewReader(wire)))
						defer st.Body.Close()
						if _, err := io.ReadAll(st.TeeBody()); err != nil {
							t.Fatal(err)
						}
						res := st.Tap.Result()
						if !res.Terminal || res.SawUsage || res.ContentBytes != want {
							t.Fatalf("terminal=%v reported=%v bytes=%d, want true/false/%d", res.Terminal, res.SawUsage, res.ContentBytes, want)
						}
					})
				})
			}
		})
	}
}
