package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestUsageRaw_OnlyUsageAcrossAdapters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		adapter   Adapter
		nonStream string
		stream    string
		inputKey  string
		outputKey string
	}{
		{
			name: "chat", adapter: NewOpenAIChat(), inputKey: "prompt_tokens", outputKey: "completion_tokens",
			nonStream: `{"id":"private-id","choices":[{"message":{"content":"private-answer","reasoning_content":"private-thought","tool_calls":[{"function":{"name":"private-tool","arguments":"private-args"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
			stream:    "data: " + `{"id":"private-id","choices":[{"delta":{"content":"private-answer","reasoning_content":"private-thought","tool_calls":[{"function":{"name":"private-tool","arguments":"private-args"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}` + "\n\ndata: [DONE]\n\n",
		},
		{
			name: "responses", adapter: NewOpenAIResponses(), inputKey: "input_tokens", outputKey: "output_tokens",
			nonStream: `{"id":"private-id","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-answer"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"private-thought"}]},{"type":"function_call","call_id":"private-call","name":"private-tool","arguments":"private-args"}],"usage":{"input_tokens":3,"output_tokens":5}}`,
			stream:    "data: " + `{"type":"response.completed","response":{"id":"private-id","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-answer"}]},{"type":"function_call","name":"private-tool","arguments":"private-args"}],"usage":{"input_tokens":3,"output_tokens":5}}}` + "\n\n",
		},
		{
			name: "anthropic", adapter: NewAnthropicMessages(), inputKey: "input_tokens", outputKey: "output_tokens",
			nonStream: `{"id":"private-id","type":"message","content":[{"type":"text","text":"private-answer"},{"type":"thinking","thinking":"private-thought"},{"type":"tool_use","id":"private-call","name":"private-tool","input":{"q":"private-args"}}],"usage":{"input_tokens":3,"output_tokens":5}}`,
			stream: "data: " + `{"type":"message_start","message":{"id":"private-id","content":[{"type":"text","text":"private-answer"},{"type":"tool_use","name":"private-tool","input":{"q":"private-args"}}],"usage":{"input_tokens":3}}}` + "\n\n" +
				"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("nonstream", func(t *testing.T) {
				res, err := tc.adapter.DecodeNonStream([]byte(tc.nonStream))
				if err != nil {
					t.Fatal(err)
				}
				if !res.UsageReported {
					t.Fatal("usage not reported")
				}
				assertUsageOnly(t, res.UsageRaw, tc.inputKey, tc.outputKey)
			})
			t.Run("stream", func(t *testing.T) {
				st := tc.adapter.WrapStream(io.NopCloser(strings.NewReader(tc.stream)))
				defer st.Body.Close()
				if _, err := io.ReadAll(st.TeeBody()); err != nil {
					t.Fatal(err)
				}
				res := st.Tap.Result()
				if !res.SawUsage || !res.Terminal {
					t.Fatalf("tap = %+v", res)
				}
				assertUsageOnly(t, res.Raw, tc.inputKey, tc.outputKey)
			})
		})
	}
}

func assertUsageOnly(t *testing.T, raw json.RawMessage, inputKey, outputKey string) {
	t.Helper()
	if strings.Contains(string(raw), "private-") {
		t.Errorf("conversation content leaked into raw usage: %s", raw)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 2 || string(doc["schema_version"]) != "1" {
		t.Errorf("want schema_version + usage only, got %s", raw)
	}
	var usage map[string]int64
	if err := json.Unmarshal(doc["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	if usage[inputKey] != 3 || usage[outputKey] != 5 {
		t.Errorf("usage counters lost: %s", raw)
	}
}
