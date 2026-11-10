package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

type nonstreamOrderFixture struct {
	adapter                   Adapter
	before, after, empty      string
	toolOne, toolTwo          string
	prefix, suffix, usageJSON string
}

func nonstreamOrderFixtures() []nonstreamOrderFixture {
	return []nonstreamOrderFixture{
		{
			adapter:   NewAnthropicMessages(),
			before:    `{"type":"text","text":"before"}`,
			after:     `{"type":"text","text":"after"}`,
			empty:     `{"type":"text","text":""}`,
			toolOne:   `{"type":"tool_use","id":"call_1","name":"one","input":{}}`,
			toolTwo:   `{"type":"tool_use","id":"call_2","name":"two","input":{}}`,
			prefix:    `{"id":"msg_order","type":"message","model":"up","content":[`,
			suffix:    `],"stop_reason":"tool_use"`,
			usageJSON: `,"usage":{"input_tokens":7,"output_tokens":5}`,
		},
		{
			adapter:   NewOpenAIResponses(),
			before:    `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]}`,
			after:     `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}`,
			empty:     `{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`,
			toolOne:   `{"type":"function_call","call_id":"call_1","name":"one","arguments":"{}"}`,
			toolTwo:   `{"type":"function_call","call_id":"call_2","name":"two","arguments":"{}"}`,
			prefix:    `{"id":"resp_order","object":"response","status":"completed","model":"up","output":[`,
			suffix:    `]`,
			usageJSON: `,"usage":{"input_tokens":7,"output_tokens":5}`,
		},
	}
}

func (f nonstreamOrderFixture) body(items []string, usage bool) []byte {
	body := f.prefix + strings.Join(items, ",") + f.suffix
	if usage {
		body += f.usageJSON
	}
	return []byte(body + "}")
}

func TestNativeNonstream_RejectReorderedOutputPreservesMetering(t *testing.T) {
	for _, f := range nonstreamOrderFixtures() {
		t.Run(string(f.adapter.Protocol()), func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				items []string
				bytes int64
			}{
				{"text_tool_text", []string{f.before, f.toolOne, f.after}, 13},
				{"tool_text_tool", []string{f.toolOne, f.after, f.toolTwo}, 9},
			} {
				t.Run(tc.name, func(t *testing.T) {
					for _, reported := range []bool{true, false} {
						name := "estimated"
						if reported {
							name = "reported"
						}
						t.Run(name, func(t *testing.T) {
							res, err := f.adapter.DecodeNonStream(f.body(tc.items, reported))
							if err == nil {
								t.Error("unrepresentable output order must be rejected")
							}
							if res == nil {
								t.Fatal("decoded metering must survive a translation rejection")
							}
							if len(res.Payload) != 0 {
								t.Errorf("reordered payload must not be served: %s", res.Payload)
							}
							if res.ContentBytes != tc.bytes || res.UpstreamRequestID == "" {
								t.Errorf("metering metadata = %+v", res)
							}
							if res.UsageReported != reported {
								t.Errorf("UsageReported=%v, want %v", res.UsageReported, reported)
							}
							if reported {
								if res.Usage.InputTokens == nil || *res.Usage.InputTokens != 7 ||
									res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 5 ||
									len(res.UsageRaw) == 0 {
									t.Errorf("reported usage lost: %+v", res)
								}
							} else if res.Usage.InputTokens != nil || res.Usage.OutputTokens != nil {
								t.Errorf("missing usage must not become zero: %+v", res.Usage)
							}
						})
					}
				})
			}
		})
	}
}

func TestNativeNonstream_SupportedTextThenTools(t *testing.T) {
	for _, f := range nonstreamOrderFixtures() {
		t.Run(string(f.adapter.Protocol()), func(t *testing.T) {
			res, err := f.adapter.DecodeNonStream(f.body(
				[]string{f.before, f.after, f.toolOne, f.empty, f.toolTwo}, true))
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(res.Payload, &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Choices) != 1 {
				t.Fatalf("choices=%+v", out.Choices)
			}
			msg := out.Choices[0].Message
			if msg.Content != "beforeafter" || len(msg.ToolCalls) != 2 ||
				msg.ToolCalls[0].ID != "call_1" || msg.ToolCalls[1].ID != "call_2" {
				t.Errorf("supported output changed: %s", res.Payload)
			}
		})
	}
}

func TestDispatch_NativeOutputRejectionKeepsExecutedResult(t *testing.T) {
	for _, f := range nonstreamOrderFixtures() {
		t.Run(string(f.adapter.Protocol()), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "up-order")
				_, _ = w.Write(f.body([]string{f.before, f.toolOne, f.after}, true))
			}))
			defer srv.Close()
			res, err := Dispatch(context.Background(), NewHTTPClient(nil), f.adapter,
				chatCall(testDeployment(f.adapter.Protocol(), srv.URL), false))
			if err == nil {
				t.Error("translation failure must remain explicit")
			}
			if res == nil || !res.UsageReported || res.Usage.OutputTokens == nil ||
				*res.Usage.OutputTokens != 5 || res.ContentBytes != 13 ||
				res.UpstreamRequestID != "up-order" || len(res.Payload) != 0 {
				t.Errorf("executed result lost or reordered: %+v", res)
			}
		})
	}
}

// Keep the malformed-body control distinct from a fully decoded rejection.
func TestDispatch_MalformedNativeOutputRemainsUnknown(t *testing.T) {
	for _, f := range nonstreamOrderFixtures() {
		t.Run(string(f.adapter.Protocol()), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"usage":`)
			}))
			defer srv.Close()
			res, err := Dispatch(context.Background(), NewHTTPClient(nil), f.adapter,
				chatCall(testDeployment(f.adapter.Protocol(), srv.URL), false))
			de, ok := err.(*DispatchError)
			if res != nil || !ok || !de.ExecutedUnknown || domain.CodeOf(err) == domain.CodeInvalidInput {
				t.Fatalf("malformed 200 must remain unknown-execution: result=%+v err=%v", res, err)
			}
		})
	}
}
