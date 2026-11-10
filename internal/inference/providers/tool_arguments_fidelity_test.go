package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

var toolArgumentCases = []struct {
	name, args string
	object     bool
	valid      bool
}{
	{"missing", "", false, false},
	{"whitespace", " \n", false, false},
	{"truncated", `{"n":`, false, false},
	{"malformed", `{"n":x}`, false, false},
	{"empty_object", `{}`, true, true},
	{"large_integer", `{"n":9007199254740993}`, true, true},
	{"null", `null`, false, true},
	{"array", `[1]`, false, true},
	{"scalar", `42`, false, true},
	{"string", `"value"`, false, true},
}

func toolArgumentCompletion(args string) []byte {
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-args","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_args","type":"function","function":{"name":"lookup","arguments":%q}}]},"finish_reason":"tool_calls"}]}`, args))
}

func TestToolArguments_ResponsesPreservesOutputHistoryAndReplay(t *testing.T) {
	for _, tc := range toolArgumentCases {
		t.Run(tc.name, func(t *testing.T) {
			payload := toolArgumentCompletion(tc.args)
			body, transcript, err := ResponsesFromCompletion(payload, "resp_args", "m", 1, true)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Output []responsesItemView `json:"output"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Output) != 1 || response.Output[0].Arguments != tc.args {
				t.Errorf("Responses output changed arguments: %s, want %q", body, tc.args)
			}
			_, replayErr := ResponsesReplayToChat(transcript)
			if (replayErr == nil) != tc.valid {
				t.Errorf("replay error=%v, want valid=%t; transcript=%s", replayErr, tc.valid, transcript)
			}
			call := chatCall(testDeployment(domain.ProtocolOpenAIResponses, "http://unused"), false)
			call.Request.Messages = []model.ChatMessage{{Role: "user", Content: "hello"}, {
				Role: "assistant", ToolCalls: []model.ToolCall{{
					ID: "call_args", Type: "function", Function: model.ToolCallFunction{Name: "lookup", Arguments: tc.args},
				}},
			}}
			history, err := NewOpenAIResponses().BuildPayload(call)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Input []responsesItemView `json:"input"`
			}
			if err := json.Unmarshal(history, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Input) != 2 || request.Input[1].Arguments != tc.args {
				t.Errorf("Responses history changed arguments: %s, want %q", history, tc.args)
			}
			upstream := fmt.Sprintf(`{"id":"resp_args","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call_args","name":"lookup","arguments":%q}]}`, tc.args)
			ns, err := NewOpenAIResponses().DecodeNonStream([]byte(upstream))
			if err != nil {
				t.Fatal(err)
			}
			v, err := parseCompletion(ns.Payload)
			if err != nil || len(v.Choices) != 1 || len(v.Choices[0].Message.ToolCalls) != 1 {
				t.Fatalf("decode=%s, %v", ns.Payload, err)
			}
			if got := v.Choices[0].Message.ToolCalls[0].Function.Arguments; got != tc.args {
				t.Errorf("Responses decode changed arguments to %q, want %q", got, tc.args)
			}
		})
	}
}

func TestToolArguments_MessagesObjectConversion(t *testing.T) {
	for _, tc := range toolArgumentCases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := AnthropicMessageFromCompletion(toolArgumentCompletion(tc.args), "m", true)
			if (err == nil) != tc.object {
				t.Errorf("Messages conversion err=%v, want object=%t; body=%s", err, tc.object, body)
			}
			if !tc.object && len(body) != 0 {
				t.Errorf("rejected output must be absent: %s", body)
			}
			call := chatCall(testDeployment(domain.ProtocolAnthropicMessage, "http://unused"), false)
			call.Request.Messages = []model.ChatMessage{{Role: "user", Content: "hello"}, {
				Role: "assistant", ToolCalls: []model.ToolCall{{
					ID: "call_args", Type: "function", Function: model.ToolCallFunction{Name: "lookup", Arguments: tc.args},
				}},
			}}
			history, historyErr := NewAnthropicMessages().BuildPayload(call)
			if (historyErr == nil) != tc.object {
				t.Errorf("Messages history err=%v, want object=%t; body=%s", historyErr, tc.object, history)
			}
			if tc.object {
				for _, rendered := range [][]byte{body, history} {
					if !strings.Contains(string(rendered), `"input":`+tc.args) {
						t.Errorf("valid object precision lost: %s, want %s", rendered, tc.args)
					}
				}
			} else if domain.CodeOf(historyErr) != domain.CodeInvalidInput {
				t.Errorf("invalid history needs a pre-dispatch input error, got %v", historyErr)
			}
		})
	}
}

func TestToolArguments_MessagesIngressObject(t *testing.T) {
	for _, tc := range toolArgumentCases {
		if !tc.valid && tc.args != "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			input := ""
			if tc.args != "" {
				input = `,"input":` + tc.args
			}
			body := `{"model":"m","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_args","name":"lookup"` + input + `}]}]}`
			req, err := ParseAnthropicMessagesRequest([]byte(body))
			if (err == nil) != tc.object {
				t.Fatalf("ingress err=%v, want object=%t", err, tc.object)
			}
			if !tc.object {
				if domain.CodeOf(err) != domain.CodeInvalidInput {
					t.Errorf("invalid object needs input error: %v", err)
				}
				return
			}
			if got := req.Messages[0].ToolCalls[0].Function.Arguments; got != tc.args {
				t.Errorf("ingress object changed to %q, want %q", got, tc.args)
			}
		})
	}
}

func TestToolArguments_MessagesDispatchNonstreamEvidence(t *testing.T) {
	for _, adapter := range []Adapter{NewOpenAIChat(), NewOpenAIResponses(), NewAnthropicMessages()} {
		for _, reported := range []bool{false, true} {
			for _, tc := range toolArgumentCases {
				// Anthropic's input is a JSON value, not a string; malformed
				// JSON would test a malformed envelope instead of conversion.
				if adapter.Protocol() == domain.ProtocolAnthropicMessage && !tc.valid && tc.args != "" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/reported=%t", adapter.Protocol(), tc.name, reported), func(t *testing.T) {
					body := toolArgumentUpstreamBody(adapter.Protocol(), tc.args, reported)
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						_, _ = io.WriteString(w, body)
					}))
					defer up.Close()
					call := chatCall(testDeployment(adapter.Protocol(), up.URL), false)
					call.ClientProtocol = domain.ProtocolAnthropicMessage
					res, err := Dispatch(context.Background(), NewHTTPClient(nil), adapter, call)
					if (err == nil) != tc.object {
						t.Errorf("Dispatch err=%v, want object=%t", err, tc.object)
					}
					if res == nil {
						t.Fatalf("complete execution must retain evidence: %v", err)
					}
					if !tc.object && len(res.Payload) != 0 {
						t.Errorf("invalid Messages output reached successful settlement: %s", res.Payload)
					}
					if res.ContentBytes != int64(len(tc.args)) || res.UsageReported != reported || res.UpstreamRequestID == "" {
						t.Errorf("execution evidence lost: %+v; want %d bytes reported=%t", res, len(tc.args), reported)
					}
					if reported && (res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 ||
						res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 3 || len(res.UsageRaw) == 0) {
						t.Errorf("reported evidence lost: %+v", res)
					}
				})
			}
		}
	}
}

func toolArgumentUpstreamBody(proto domain.Protocol, args string, reported bool) string {
	if proto == domain.ProtocolOpenAIChat {
		body := strings.TrimSuffix(string(toolArgumentCompletion(args)), "}")
		if reported {
			body += `,"usage":{"prompt_tokens":10,"completion_tokens":3}`
		}
		return body + "}"
	}
	var body string
	if proto == domain.ProtocolAnthropicMessage {
		input := ""
		if args != "" {
			input = `,"input":` + args
		}
		body = `{"id":"msg_args","type":"message","content":[{"type":"tool_use","id":"call_args","name":"lookup"` + input + `}],"stop_reason":"tool_use"`
	} else {
		body = fmt.Sprintf(`{"id":"resp_args","status":"completed","output":[{"type":"function_call","call_id":"call_args","name":"lookup","arguments":%q}]`, args)
	}
	if reported {
		body += `,"usage":{"input_tokens":10,"output_tokens":3}`
	}
	return body + "}"
}

func toolArgumentStream(proto domain.Protocol, args, ending string, reported bool) string {
	var events []string
	// A valid second call interleaves with the first.
	switch proto {
	case domain.ProtocolAnthropicMessage:
		start := `{"type":"message_start","message":{"id":"msg_args"}}`
		if reported {
			start = `{"type":"message_start","message":{"id":"msg_args","usage":{"input_tokens":10,"output_tokens":0}}}`
		}
		events = append(events,
			start,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_args","name":"lookup","input":{}}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_valid","name":"lookup","input":{}}}`,
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, args[:len(args)/2]),
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, args[len(args)/2:]),
			`{"type":"content_block_stop","index":0}`, `{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
		if ending == "done_error_done" {
			events = append(events, `{"type":"message_stop"}`)
		}
		if strings.Contains(ending, "error") {
			events = append(events, `{"type":"error","error":{"message":"broken"}}`)
		}
		if reported {
			events = append(events, `{"type":"message_delta","usage":{"output_tokens":3}}`)
		}
		if ending != "eof" {
			events = append(events, `{"type":"message_stop"}`)
		}
	case domain.ProtocolOpenAIResponses:
		events = append(events,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_args","name":"lookup","arguments":""}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_valid","name":"lookup","arguments":""}}`,
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, args[:len(args)/2]),
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
			fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, args[len(args)/2:]))
		usage := ""
		if reported {
			usage = `,"usage":{"input_tokens":10,"output_tokens":3}`
		}
		if ending == "done_error_done" {
			events = append(events, `{"type":"response.completed","response":{"id":"resp_args","status":"completed"}}`)
		}
		if strings.Contains(ending, "error") {
			events = append(events, `{"type":"error","error":{"message":"broken"}}`)
		}
		if ending != "eof" {
			events = append(events, `{"type":"response.completed","response":{"id":"resp_args","status":"completed"`+usage+`}}`)
		}
	default:
		events = append(events,
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_args","function":{"name":"lookup","arguments":%q}}]}}]}`, args[:len(args)/2]),
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_valid","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, args[len(args)/2:]))
		if ending == "done_error_done" {
			events = append(events, `[DONE]`)
		}
		if strings.Contains(ending, "error") {
			events = append(events, `{"error":{"message":"broken","type":"api_error"}}`)
		}
		if reported {
			events = append(events, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
		}
		if ending != "eof" {
			events = append(events, `[DONE]`)
		}
	}
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func TestToolArguments_StreamConversionAndEvidence(t *testing.T) {
	for _, adapter := range []Adapter{NewOpenAIChat(), NewOpenAIResponses(), NewAnthropicMessages()} {
		for _, tc := range toolArgumentCases {
			for _, ending := range []string{"done", "eof", "error_done", "done_error_done"} {
				if tc.object && ending == "done_error_done" {
					continue // A valid success already emitted cannot be retracted.
				}
				for _, fragment := range []int{1, 4096} {
					t.Run(fmt.Sprintf("%s/%s/%s/fragment=%d", adapter.Protocol(), tc.name, ending, fragment), func(t *testing.T) {
						wire := toolArgumentStream(adapter.Protocol(), tc.args, ending, true)
						up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							_, _ = io.WriteString(w, wire)
						}))
						defer up.Close()
						call := chatCall(testDeployment(adapter.Protocol(), up.URL), true)
						call.ClientProtocol = domain.ProtocolAnthropicMessage
						res, err := Dispatch(context.Background(), NewHTTPClient(nil), adapter, call)
						if err != nil {
							t.Fatal(err)
						}
						source := &fragmentReader{src: res.Stream.TeeBody(), n: fragment}
						rendered := TranslateOpenAIToAnthropicStream(source, "msg_args", "m", true)
						defer rendered.Close()
						body, err := io.ReadAll(rendered)
						if err != nil {
							t.Fatal(err)
						}
						complete := ending == "done"
						success := tc.object && complete
						output := string(body)
						if strings.Contains(output, "event: message_stop\n") != success {
							t.Errorf("success=%t, want %t: %s", strings.Contains(output, "event: message_stop\n"), success, output)
						}
						if !success && strings.Count(output, "event: error\n") != 1 {
							t.Errorf("rejected/interrupted Messages needs exactly one error: %s", output)
						}
						tap := res.Stream.Tap.Result()
						wantUsage := adapter.Protocol() != domain.ProtocolOpenAIResponses || ending != "eof"
						if tap.Terminal != success || tap.RejectedComplete != (complete && !tc.object) ||
							tap.ContentBytes != int64(len(tc.args)+2) || tap.SawUsage != wantUsage ||
							(wantUsage && (tap.Buckets.OutputTokens == nil || *tap.Buckets.OutputTokens != 3)) {
							t.Errorf("tap=%+v, want terminal=%t rejected=%t bytes=%d and late 10/3",
								tap, success, complete && !tc.object, len(tc.args)+2)
						}
					})
				}
			}
		}
	}
}

type fragmentReader struct {
	src io.ReadCloser
	n   int
}

func (r *fragmentReader) Read(p []byte) (int, error) { return r.src.Read(p[:min(len(p), r.n)]) }
func (r *fragmentReader) Close() error               { return r.src.Close() }

func TestToolArguments_ResponsesStreamVerbatim(t *testing.T) {
	for _, tc := range toolArgumentCases {
		t.Run(tc.name, func(t *testing.T) {
			wire := toolArgumentStream(domain.ProtocolOpenAIChat, tc.args, "done", false)
			rendered, state := TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire)), "resp_args", "m", 1, true)
			defer rendered.Close()
			body, err := io.ReadAll(rendered)
			if err != nil || !state.Completed() {
				t.Fatalf("Responses stream err=%v: %s", err, body)
			}
			items := state.FinalOutputItems()
			if len(items) != 2 {
				t.Fatalf("items=%s", items)
			}
			var item responsesItemView
			if err := json.Unmarshal(items[0], &item); err != nil {
				t.Fatal(err)
			}
			if item.Arguments != tc.args {
				t.Errorf("stream changed %q to %q", tc.args, item.Arguments)
			}
			_, err = ResponsesReplayToChat(items)
			if (err == nil) != tc.valid {
				t.Errorf("stream transcript replay err=%v, want valid=%t", err, tc.valid)
			}
		})
	}
}

func TestToolArguments_MessagesRejectsMultipleChoices(t *testing.T) {
	wire := "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_zero","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_one","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, wire)
	}))
	defer up.Close()
	call := chatCall(testDeployment(domain.ProtocolOpenAIChat, up.URL), true)
	call.ClientProtocol = domain.ProtocolAnthropicMessage
	res, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), call)
	if err != nil {
		t.Fatal(err)
	}
	rendered := TranslateOpenAIToAnthropicStream(res.Stream.TeeBody(), "msg_args", "m", true)
	defer rendered.Close()
	body, err := io.ReadAll(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "event: error\n") != 1 || strings.Contains(string(body), "event: message_stop\n") {
		t.Errorf("multiple choices merged into one Messages response: %s", body)
	}
	tap := res.Stream.Tap.Result()
	if tap.Terminal || !tap.RejectedComplete || tap.ContentBytes != 4 {
		t.Errorf("multiple choices lost rejection evidence: %+v", tap)
	}
	payload := []byte(`{"choices":[{"index":0,"message":{"content":"zero"}},{"index":1,"message":{"content":"one"}}]}`)
	if body, err := AnthropicMessageFromCompletion(payload, "m", true); err == nil || len(body) != 0 {
		t.Errorf("multiple nonstream choices silently dropped: %s, %v", body, err)
	}
}
