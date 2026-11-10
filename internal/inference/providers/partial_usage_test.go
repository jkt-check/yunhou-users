package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

type partialUsageCase struct {
	name, native, chat, messages, responses string
}

func partialUsageCases(proto domain.Protocol) []partialUsageCase {
	switch proto {
	case domain.ProtocolOpenAIChat:
		return []partialUsageCase{
			{"input_only", `{"prompt_tokens":2}`, `{"prompt_tokens":2}`, `{"input_tokens":2}`, `{"input_tokens":2}`},
			{"output_only", `{"completion_tokens":7}`, `{"completion_tokens":7}`, `{"output_tokens":7}`, `{"output_tokens":7}`},
			{"detail_only", `{"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}`,
				`{"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}`,
				`{"cache_read_input_tokens":3}`, `{"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`},
			{"total_only", `{"total_tokens":9}`, `{"total_tokens":9}`, `{}`, `{"total_tokens":9}`},
			{"explicit_zero", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"cache_creation_input_tokens":0,"completion_tokens_details":{"reasoning_tokens":0}}`,
				`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"cache_creation_input_tokens":0,"completion_tokens_details":{"reasoning_tokens":0}}`,
				`{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				`{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`},
			{"complete", `{"prompt_tokens":2,"completion_tokens":7}`, `{"prompt_tokens":2,"completion_tokens":7}`,
				`{"input_tokens":2,"output_tokens":7}`, `{"input_tokens":2,"output_tokens":7,"total_tokens":9}`},
			{"reported_total", `{"prompt_tokens":2,"completion_tokens":7,"total_tokens":15}`, `{"prompt_tokens":2,"completion_tokens":7,"total_tokens":15}`,
				`{"input_tokens":2,"output_tokens":7}`, `{"input_tokens":2,"output_tokens":7,"total_tokens":15}`},
		}
	case domain.ProtocolOpenAIResponses:
		return []partialUsageCase{
			{"input_only", `{"input_tokens":2}`, `{"prompt_tokens":2}`, `{"input_tokens":2}`, `{"input_tokens":2}`},
			{"output_only", `{"output_tokens":7}`, `{"completion_tokens":7}`, `{"output_tokens":7}`, `{"output_tokens":7}`},
			{"detail_only", `{"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`,
				`{"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}`,
				`{"cache_read_input_tokens":3}`, `{"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`},
			{"total_only", `{"total_tokens":9}`, `{"total_tokens":9}`, `{}`, `{"total_tokens":9}`},
			{"explicit_zero", `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`,
				`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}`,
				`{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}`,
				`{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`},
			{"complete", `{"input_tokens":2,"output_tokens":7}`, `{"prompt_tokens":2,"completion_tokens":7,"total_tokens":9}`,
				`{"input_tokens":2,"output_tokens":7}`, `{"input_tokens":2,"output_tokens":7,"total_tokens":9}`},
			{"reported_total", `{"input_tokens":2,"output_tokens":7,"total_tokens":15}`, `{"prompt_tokens":2,"completion_tokens":7,"total_tokens":15}`,
				`{"input_tokens":2,"output_tokens":7}`, `{"input_tokens":2,"output_tokens":7,"total_tokens":15}`},
		}
	default:
		return []partialUsageCase{
			{"input_only", `{"input_tokens":2}`, `{"prompt_tokens":2}`, `{"input_tokens":2}`, `{"input_tokens":2}`},
			{"output_only", `{"output_tokens":7}`, `{"completion_tokens":7}`, `{"output_tokens":7}`, `{"output_tokens":7}`},
			{"detail_only", `{"cache_read_input_tokens":3,"cache_creation_input_tokens":5}`,
				`{"prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":5}`,
				`{"cache_read_input_tokens":3,"cache_creation_input_tokens":5}`, `{"input_tokens_details":{"cached_tokens":3}}`},
			{"explicit_zero", `{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"cache_creation_input_tokens":0}`,
				`{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				`{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0}}`},
			{"complete", `{"input_tokens":2,"output_tokens":7}`, `{"prompt_tokens":2,"completion_tokens":7,"total_tokens":9}`,
				`{"input_tokens":2,"output_tokens":7}`, `{"input_tokens":2,"output_tokens":7,"total_tokens":9}`},
			{"complete_cache", `{"input_tokens":2,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}`,
				`{"prompt_tokens":2,"completion_tokens":7,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":3},"cache_creation_input_tokens":5}`,
				`{"input_tokens":2,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}`,
				`{"input_tokens":10,"output_tokens":7,"total_tokens":17,"input_tokens_details":{"cached_tokens":3}}`},
		}
	}
}

func partialUsageWire(proto domain.Protocol, usage string) (string, string) {
	switch proto {
	case domain.ProtocolOpenAIChat:
		return `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":` + usage + `}`,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
	case domain.ProtocolOpenAIResponses:
		obj := `{"id":"resp_partial","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":` + usage + `}`
		return obj, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":" + obj + "}\n\n"
	default:
		return `{"id":"msg_partial","type":"message","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":` + usage + `}`,
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":" + usage + "}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
	}
}

func TestPartialUsage_PublicSurfaces(t *testing.T) {
	for _, adapter := range []Adapter{NewOpenAIChat(), NewOpenAIResponses(), NewAnthropicMessages()} {
		for _, tc := range partialUsageCases(adapter.Protocol()) {
			t.Run(string(adapter.Protocol())+"/"+tc.name, func(t *testing.T) {
				native, wire := partialUsageWire(adapter.Protocol(), tc.native)
				for _, surface := range []struct{ name, want string }{
					{"chat", tc.chat}, {"messages", tc.messages}, {"responses", tc.responses},
				} {
					t.Run(surface.name+"/nonstream", func(t *testing.T) {
						result, err := adapter.DecodeNonStream([]byte(native))
						if err != nil {
							t.Fatal(err)
						}
						payload := result.Payload
						switch surface.name {
						case "messages":
							payload, err = AnthropicMessageFromCompletion(payload, "m", adapter.Inclusion().CacheReadInInput)
						case "responses":
							payload, _, err = ResponsesFromCompletion(payload, "resp_public", "m", 1, adapter.Inclusion().CacheReadInInput)
						}
						if err != nil {
							t.Fatal(err)
						}
						assertPartialUsage(t, partialUsageObject(t, payload), surface.want)
					})
					for _, size := range []int{1, len(wire)} {
						t.Run(fmt.Sprintf("%s/stream/chunk=%d", surface.name, size), func(t *testing.T) {
							up := adapter.WrapStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
							defer up.Body.Close()
							body := up.TeeBody()
							switch surface.name {
							case "messages":
								body = TranslateOpenAIToAnthropicStream(body, "msg_public", "m", adapter.Inclusion().CacheReadInInput)
							case "responses":
								body, _ = TranslateOpenAIToResponsesStream(body, "resp_public", "m", 1, adapter.Inclusion().CacheReadInInput)
							}
							defer body.Close()
							out, err := io.ReadAll(body)
							if err != nil {
								t.Fatal(err)
							}
							if !up.Tap.Result().Terminal {
								t.Fatalf("missing native terminal: %s", out)
							}
							assertPartialUsage(t, partialStreamUsage(t, out, surface.name), surface.want)
						})
					}
				}
			})
		}
	}
}

func TestPartialUsage_AbnormalNativeFlush(t *testing.T) {
	for _, adapter := range []Adapter{NewOpenAIResponses(), NewAnthropicMessages()} {
		for _, tc := range partialUsageCases(adapter.Protocol()) {
			if tc.name != "output_only" && tc.name != "total_only" && tc.name != "explicit_zero" {
				continue
			}
			for _, ending := range []string{"eof", "error"} {
				t.Run(string(adapter.Protocol())+"/"+tc.name+"/"+ending, func(t *testing.T) {
					var wire string
					if adapter.Protocol() == domain.ProtocolOpenAIResponses {
						wire = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":" + tc.native + "}}\n\n"
					} else {
						wire = "data: {\"type\":\"message_start\",\"message\":{\"usage\":" + tc.native + "}}\n\n"
					}
					if ending == "error" {
						wire += "data: {\"type\":\"error\",\"message\":\"broken\"}\n\n"
					}
					up := adapter.WrapStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), 1}))
					defer up.Body.Close()
					out, err := io.ReadAll(up.TeeBody())
					if err != nil {
						t.Fatal(err)
					}
					if up.Tap.Result().Terminal || strings.Contains(string(out), "[DONE]") {
						t.Fatalf("abnormal stream became complete: %s", out)
					}
					assertPartialUsage(t, partialStreamUsage(t, out, "chat"), tc.chat)
				})
			}
		}
	}
}

func TestPartialUsage_ArithmeticDoesNotInventCounters(t *testing.T) {
	for _, tc := range []struct {
		name, usage, messages, responses string
		inclusive                        bool
	}{
		{"total_overflow", `{"prompt_tokens":9223372036854775807,"completion_tokens":1}`,
			`{"input_tokens":9223372036854775807,"output_tokens":1}`, `{"input_tokens":9223372036854775807,"output_tokens":1}`, true},
		{"cache_add_overflow", `{"prompt_tokens":9223372036854775807,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":1}}`,
			`{"input_tokens":9223372036854775807,"output_tokens":1,"cache_read_input_tokens":1}`,
			`{"output_tokens":1,"input_tokens_details":{"cached_tokens":1}}`, false},
		{"cache_sub_underflow", `{"prompt_tokens":2,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}`,
			`{"output_tokens":7,"cache_read_input_tokens":3}`,
			`{"input_tokens":2,"output_tokens":7,"total_tokens":9,"input_tokens_details":{"cached_tokens":3}}`, true},
		{"cache_creation_only", `{"completion_tokens":7,"cache_creation_input_tokens":5}`,
			`{"output_tokens":7,"cache_creation_input_tokens":5}`, `{"output_tokens":7}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`)
			msg, err := AnthropicMessageFromCompletion(payload, "m", tc.inclusive)
			if err != nil {
				t.Fatal(err)
			}
			assertPartialUsage(t, partialUsageObject(t, msg), tc.messages)
			resp, _, err := ResponsesFromCompletion(payload, "resp_public", "m", 1, tc.inclusive)
			if err != nil {
				t.Fatal(err)
			}
			assertPartialUsage(t, partialUsageObject(t, resp), tc.responses)
		})
	}
}

func partialUsageObject(t *testing.T, payload []byte) json.RawMessage {
	t.Helper()
	var obj struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Usage) == 0 || string(obj.Usage) == "null" {
		t.Fatalf("missing reported usage: %s", payload)
	}
	return obj.Usage
}

func partialStreamUsage(t *testing.T, wire []byte, surface string) json.RawMessage {
	t.Helper()
	if surface == "responses" {
		return partialUsageObject(t, responsesCacheTerminal(t, wire))
	}
	var usage json.RawMessage
	for _, line := range strings.Split(string(wire), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var obj struct {
			Type  string          `json:"type"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatal(err)
		}
		if len(obj.Usage) != 0 && (surface == "chat" || obj.Type == "message_delta") {
			if usage != nil {
				t.Fatalf("duplicate usage: %s", wire)
			}
			usage = obj.Usage
		}
	}
	if usage == nil {
		t.Fatalf("missing terminal usage: %s", wire)
	}
	return usage
}

func assertPartialUsage(t *testing.T, got []byte, want string) {
	t.Helper()
	decode := func(raw []byte) any {
		t.Helper()
		var v any
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !reflect.DeepEqual(decode(got), decode([]byte(want))) {
		t.Errorf("usage=%s, want %s (missing is not zero)", got, want)
	}
}
