package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Exercise both a line crossing many reads and multiple events in one read.
type boundedChunkReader struct {
	io.Reader
	size int
}

func (r boundedChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.Reader.Read(p)
}

func largeDeltaFixture(t *testing.T, kind string, size int) (wire, value string) {
	t.Helper()
	value = strings.Repeat("x", size)
	delta := map[string]any{"content": value}
	finish := "stop"
	if kind == "tool" {
		value = `{"text":"` + value + `"}`
		delta = map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_large", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": value},
		}}}
		finish = "tool_calls"
	}
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
		"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 34},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(payload) + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"` + finish + `"}]}` +
		"\n\ndata: [DONE]\n\n", value
}

func TestLargeSSE_InlineUsageAndContentAreNotDropped(t *testing.T) {
	for _, kind := range []string{"text", "tool"} {
		t.Run(kind, func(t *testing.T) {
			wire, value := largeDeltaFixture(t, kind, 70<<10)
			for _, size := range []int{8191, len(wire)} {
				st := NewOpenAIChat().WrapStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
				out, err := io.ReadAll(st.TeeBody())
				st.Body.Close()
				if err != nil || string(out) != wire {
					t.Fatalf("chunk=%d: passthrough bytes=%d err=%v", size, len(out), err)
				}
				got := st.Tap.Result()
				if got.ContentBytes != int64(len(value)) || !got.Terminal || !got.SawUsage ||
					got.Buckets.InputTokens == nil || *got.Buckets.InputTokens != 12 ||
					got.Buckets.OutputTokens == nil || *got.Buckets.OutputTokens != 34 {
					t.Errorf("chunk=%d: tap=%+v, want %d bytes, 12/34 terminal", size, got, len(value))
				}
			}
			for _, surface := range []string{"messages", "responses"} {
				t.Run(surface, func(t *testing.T) {
					src := io.NopCloser(boundedChunkReader{strings.NewReader(wire), 8191})
					var r io.ReadCloser
					if surface == "messages" {
						r = TranslateOpenAIToAnthropicStream(src, "msg_large", "m", true)
					} else {
						r, _ = TranslateOpenAIToResponsesStream(src, "resp_large", "m", 1, true)
					}
					defer r.Close()
					out, err := io.ReadAll(r)
					if err != nil {
						t.Fatal(err)
					}
					var text, args strings.Builder
					for _, line := range strings.Split(string(out), "\n") {
						data, ok := strings.CutPrefix(line, "data: ")
						if !ok {
							continue
						}
						var ev struct {
							Type  string          `json:"type"`
							Delta json.RawMessage `json:"delta"`
						}
						if err := json.Unmarshal([]byte(data), &ev); err != nil {
							t.Fatal(err)
						}
						switch ev.Type {
						case "content_block_delta":
							var d struct {
								Text string `json:"text"`
								Args string `json:"partial_json"`
							}
							if err := json.Unmarshal(ev.Delta, &d); err != nil {
								t.Fatal(err)
							}
							text.WriteString(d.Text)
							args.WriteString(d.Args)
						case "response.output_text.delta", "response.function_call_arguments.delta":
							var d string
							if err := json.Unmarshal(ev.Delta, &d); err != nil {
								t.Fatal(err)
							}
							if ev.Type == "response.output_text.delta" {
								text.WriteString(d)
							} else {
								args.WriteString(d)
							}
						}
					}
					actual := text.String()
					if kind == "tool" {
						actual = args.String()
					}
					if actual != value {
						t.Errorf("delta bytes=%d, want %d; large event was lost", len(actual), len(value))
					}
					for _, want := range []string{`"output_tokens":34`, "event: message_stop"} {
						if surface == "responses" {
							want = strings.ReplaceAll(want, "event: message_stop", "event: response.completed")
						}
						if !strings.Contains(string(out), want) {
							t.Errorf("missing %s", want)
						}
					}
				})
			}
		})
	}
}

func TestOversizedSSE_FailsWithoutSuccessfulTerminal(t *testing.T) {
	wire, _ := largeDeltaFixture(t, "tool", 2<<20)
	prefix := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n"
	wire = prefix + wire
	for _, size := range []int{8191, len(wire)} {
		st := NewOpenAIChat().WrapStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
		r := st.TeeBody()
		out, err := io.ReadAll(r)
		if err == nil {
			t.Errorf("chunk=%d: oversized passthrough silently accepted", size)
		}
		if strings.Contains(string(out), "[DONE]") {
			t.Errorf("chunk=%d: completion marker escaped after oversized line", size)
		}
		buf := make([]byte, len(wire))
		if n, err := r.Read(buf); n != 0 || err == nil {
			t.Errorf("chunk=%d: read after overflow=%d/%v, want sticky failure", size, n, err)
		}
		st.Body.Close()
		got := st.Tap.Result()
		if got.Terminal || !got.SawUsage || got.Buckets.InputTokens == nil ||
			*got.Buckets.InputTokens != 3 || got.Buckets.OutputTokens == nil || *got.Buckets.OutputTokens != 4 {
			t.Errorf("chunk=%d: tap=%+v, want prior 3/4 usage and nonterminal", size, got)
		}
	}
	for _, surface := range []string{"messages", "responses"} {
		t.Run(surface, func(t *testing.T) {
			src := io.NopCloser(boundedChunkReader{strings.NewReader(wire), 8191})
			var r io.ReadCloser
			var state *ResponsesStreamState
			if surface == "messages" {
				r = TranslateOpenAIToAnthropicStream(src, "msg_over", "m", true)
			} else {
				r, state = TranslateOpenAIToResponsesStream(src, "resp_over", "m", 1, true)
			}
			defer r.Close()
			out, _ := io.ReadAll(r)
			if !strings.Contains(string(out), "event: error\n") ||
				strings.Contains(string(out), "event: message_stop") ||
				strings.Contains(string(out), "event: response.completed") ||
				strings.Contains(string(out), "event: response.incomplete") {
				t.Errorf("oversized %s stream must emit native error, never success", surface)
			}
			if state != nil && state.Completed() {
				t.Error("oversized output must not be persisted as a completed response")
			}
		})
	}
}
