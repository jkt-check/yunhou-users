package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestChatRefusal_NonstreamMeteringAndNativeConversion(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		bytes         int64
		reject        bool
	}{
		{"refusal", `"content":null,"refusal":"I cannot help"`, 13, true},
		{"empty", `"content":null,"refusal":""`, 0, true},
		{"mixed", `"content":"before","reasoning_content":"why","refusal":"I cannot help","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]`, 24, true},
		{"absent", `"content":"ok"`, 2, false},
		{"null", `"content":"ok","refusal":null`, 2, false},
	} {
		for _, reported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reported=%t", tc.name, reported), func(t *testing.T) {
				body := `{"id":"chatcmpl-r","choices":[{"message":{"role":"assistant",` + tc.message + `},"finish_reason":"stop"}]`
				if reported {
					body += `,"usage":{"prompt_tokens":10,"completion_tokens":3}`
				}
				body += "}"
				res, err := NewOpenAIChat().DecodeNonStream([]byte(body))
				if err != nil || res == nil {
					t.Fatalf("raw Chat decode = %+v, %v", res, err)
				}
				if string(res.Payload) != body || res.ContentBytes != tc.bytes || res.UsageReported != reported {
					t.Errorf("raw Chat result = %+v, want unchanged payload and %d bytes", res, tc.bytes)
				}
				if reported && (res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 ||
					res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 3 ||
					!strings.Contains(string(res.UsageRaw), `"completion_tokens":3`)) {
					t.Errorf("reported metering lost: %+v", res)
				}
				if !reported && (res.Usage.InputTokens != nil || res.Usage.OutputTokens != nil || len(res.UsageRaw) != 0) {
					t.Errorf("absent usage must not become zero: %+v", res)
				}
				msg, msgErr := AnthropicMessageFromCompletion([]byte(body), "m", true)
				response, transcript, respErr := ResponsesFromCompletion([]byte(body), "resp_r", "m", 1, true)
				if tc.reject {
					if msgErr == nil || len(msg) != 0 || respErr == nil || len(response) != 0 || len(transcript) != 0 {
						t.Errorf("native refusal must not become empty success: Messages=%s/%v Responses=%s/%v transcript=%s",
							msg, msgErr, response, respErr, transcript)
					}
				} else if msgErr != nil || respErr != nil {
					t.Errorf("ordinary completion rejected: %v / %v", msgErr, respErr)
				}
			})
		}
	}
}

func TestChatRefusal_RawStreamMetersEveryDelta(t *testing.T) {
	for _, tc := range []struct {
		name, deltas string
		bytes        int64
	}{
		{"refusal_only", `{"refusal":"I cannot help"}`, 13},
		{"empty", `{"refusal":""}`, 0},
		{"mixed", `{"content":"before","reasoning_content":"why","refusal":"I cannot help","tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, 24},
		{"null", `{"content":"ok","refusal":null}`, 2},
	} {
		for _, fragment := range []int{1, 7, 4096} {
			t.Run(fmt.Sprintf("%s/fragment=%d", tc.name, fragment), func(t *testing.T) {
				wire := "data: " + `{"choices":[{"delta":` + tc.deltas + `}]}` + "\n\n" +
					"data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n" +
					"data: [DONE]\n\n"
				tap := &OpenAIUsageTracker{}
				for p := []byte(wire); len(p) > 0; {
					n := min(fragment, len(p))
					if err := tap.Feed(p[:n]); err != nil {
						t.Fatal(err)
					}
					p = p[n:]
				}
				res := tap.Result()
				if res.ContentBytes != tc.bytes || !res.Terminal || res.RejectedComplete ||
					!res.SawUsage || res.Buckets.InputTokens == nil || *res.Buckets.InputTokens != 10 ||
					res.Buckets.OutputTokens == nil || *res.Buckets.OutputTokens != 3 {
					t.Errorf("raw Chat tap = %+v, want %d bytes, complete reported 10/3", res, tc.bytes)
				}
			})
		}
	}
}

func TestChatRefusal_NativeStreamRejectsAndDrains(t *testing.T) {
	for _, surface := range []string{"messages", "responses"} {
		for _, refusal := range []string{`"I cannot help"`, `""`} {
			for _, ending := range []string{"done", "eof", "upstream_error"} {
				t.Run(surface+"/"+refusal+"/"+ending, func(t *testing.T) {
					wire := "data: " + `{"choices":[{"delta":{"refusal":` + refusal + `}}]}` + "\n\n" +
						"data: " + `{"choices":[{"delta":{"content":"must not leak"}}]}` + "\n\n"
					if ending == "upstream_error" {
						wire += "data: " + `{"error":{"message":"broken","type":"api_error","code":"broken"}}` + "\n\n"
					}
					wire += "data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n"
					if ending != "eof" {
						wire += "data: [DONE]\n\n"
					}
					st := NewOpenAIChat().WrapStream(io.NopCloser(strings.NewReader(wire)))
					var rendered io.ReadCloser
					var state *ResponsesStreamState
					if surface == "messages" {
						rendered = TranslateOpenAIToAnthropicStream(st.TeeBody(), "msg_r", "m", true)
					} else {
						rendered, state = TranslateOpenAIToResponsesStream(st.TeeBody(), "resp_r", "m", 1, true)
					}
					defer rendered.Close()
					body, err := io.ReadAll(rendered)
					if err != nil {
						t.Fatal(err)
					}
					output := string(body)
					if strings.Count(output, "event: error\n") != 1 {
						t.Errorf("native stream needs exactly one explicit error: %s", output)
					}
					for _, forbidden := range []string{"must not leak", "event: message_stop\n", "event: response.completed\n", "event: response.incomplete\n"} {
						if strings.Contains(output, forbidden) {
							t.Errorf("rejected stream emitted %q: %s", forbidden, output)
						}
					}
					if state != nil && state.Completed() {
						t.Error("rejected Responses output must not be persisted")
					}
					res := st.Tap.Result()
					if !res.SawUsage || res.Buckets.OutputTokens == nil || *res.Buckets.OutputTokens != 3 {
						t.Errorf("native refusal stopped draining before late usage: %+v", res)
					}
				})
			}
		}
	}
}

func TestChatRefusal_DispatchKeepsExecutedNonstreamEvidence(t *testing.T) {
	for _, proto := range []domain.Protocol{"", domain.ProtocolOpenAIChat, domain.ProtocolKayaChat,
		domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses} {
		for _, reported := range []bool{false, true} {
			for _, refusal := range []string{`"I cannot help"`, `""`, `null`} {
				t.Run(fmt.Sprintf("%s/reported=%t/refusal=%s", proto, reported, refusal), func(t *testing.T) {
					body := `{"id":"chatcmpl-r","choices":[{"message":{"content":"before","reasoning_content":"why","refusal":` +
						refusal + `,"tool_calls":[{"function":{"arguments":"{}"}}]}}]`
					wantBytes := int64(11)
					if refusal == `"I cannot help"` {
						wantBytes = 24
					}
					if reported {
						body += `,"usage":{"prompt_tokens":10,"completion_tokens":3}`
					}
					body += "}"
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.WriteString(w, body)
					}))
					defer up.Close()
					call := chatCall(testDeployment(domain.ProtocolOpenAIChat, up.URL), false)
					call.ClientProtocol = proto
					res, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), call)
					reject := refusal != `null` && (proto == domain.ProtocolAnthropicMessage || proto == domain.ProtocolOpenAIResponses)
					if (err != nil) != reject {
						t.Errorf("dispatch err=%v, want rejection=%t", err, reject)
					}
					if res == nil {
						t.Fatal("fully decoded execution lost metering")
					}
					if res.ContentBytes != wantBytes || res.UpstreamRequestID != "chatcmpl-r" || res.UsageReported != reported {
						t.Errorf("execution evidence=%+v, want %d bytes reported=%t", res, wantBytes, reported)
					}
					if reject {
						if len(res.Payload) != 0 {
							t.Errorf("rejected native result retained success payload: %s", res.Payload)
						}
					} else if string(res.Payload) != body {
						t.Errorf("raw/ordinary result payload changed: %s", res.Payload)
					}
					if reported && (res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 ||
						res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 3 ||
						!strings.Contains(string(res.UsageRaw), `"completion_tokens":3`)) {
						t.Errorf("dispatch lost reported evidence: %+v", res)
					}
				})
			}
		}
	}
}

func TestChatRefusal_DispatchStreamCompletionEvidence(t *testing.T) {
	for _, proto := range []domain.Protocol{"", domain.ProtocolOpenAIChat, domain.ProtocolKayaChat,
		domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses} {
		for _, tc := range []struct {
			name, delta, ending string
			bytes               int64
			terminal, rejected  bool
		}{
			{"done", `{"refusal":"I cannot help"}`, "done", 13, true, true},
			{"empty", `{"refusal":""}`, "done", 0, true, true},
			{"null", `{"content":"ok","refusal":null}`, "done", 2, true, false},
			{"absent", `{"content":"ok"}`, "done", 2, true, false},
			{"mixed", `{"content":"before","reasoning_content":"why","tool_calls":[{"function":{"arguments":"{}"}}],"refusal":"I cannot help"}`, "done", 24, true, true},
			{"eof", `{"refusal":"I cannot help"}`, "", 13, false, false},
			{"error_before_done", `{"refusal":"I cannot help"}`, "error_done", 13, false, false},
			{"error_after_done", `{"refusal":"I cannot help"}`, "done_error_done", 13, false, false},
		} {
			for _, fragment := range []int{1, 4096} {
				t.Run(fmt.Sprintf("%s/%s/fragment=%d", proto, tc.name, fragment), func(t *testing.T) {
					wire := "data: " + `{"choices":[{"index":0,"delta":` + tc.delta + `}]}` + "\n\n"
					for _, part := range strings.Split(tc.ending, "_") {
						switch part {
						case "done":
							wire += "data: [DONE]\n\n"
						case "error":
							wire += "data: " + `{"error":{"message":"broken","type":"api_error","code":"broken"}}` + "\n\n"
						}
					}
					wire += "data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n"
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, wire)
					}))
					defer up.Close()
					call := chatCall(testDeployment(domain.ProtocolOpenAIChat, up.URL), true)
					call.ClientProtocol = proto
					res, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), call)
					if err != nil {
						t.Fatal(err)
					}
					defer res.Stream.Body.Close()
					var got strings.Builder
					buf := make([]byte, fragment)
					reader := res.Stream.TeeBody()
					for {
						n, err := reader.Read(buf)
						got.Write(buf[:n])
						if err != nil {
							if err != io.EOF {
								t.Fatal(err)
							}
							break
						}
					}
					native := proto == domain.ProtocolAnthropicMessage || proto == domain.ProtocolOpenAIResponses
					wantTerminal, wantRejected := tc.terminal, false
					if native && tc.rejected {
						wantTerminal, wantRejected = false, true
					}
					tap := res.Stream.Tap.Result()
					if got.String() != wire || tap.Terminal != wantTerminal || tap.RejectedComplete != wantRejected ||
						tap.ContentBytes != tc.bytes || !tap.SawUsage ||
						tap.Buckets.InputTokens == nil || *tap.Buckets.InputTokens != 10 ||
						tap.Buckets.OutputTokens == nil || *tap.Buckets.OutputTokens != 3 {
						t.Errorf("tap=%+v, want bytes=%d terminal=%t rejected=%t and late 10/3 usage; unchanged=%t",
							tap, tc.bytes, wantTerminal, wantRejected, got.String() == wire)
					}
				})
			}
		}
	}
}

func TestChatRefusal_NativeStreamRejectsWholeChunk(t *testing.T) {
	for _, choices := range []string{
		`[{"delta":{"content":"must not leak","refusal":"no"}}]`,
		`[{"index":0,"delta":{"content":"must not leak"}},{"index":1,"delta":{"refusal":""}}]`,
	} {
		for _, surface := range []string{"messages", "responses"} {
			t.Run(surface+"/"+choices, func(t *testing.T) {
				wire := "data: " + `{"choices":` + choices + `}` + "\n\ndata: [DONE]\n\n"
				var rendered io.ReadCloser
				if surface == "messages" {
					rendered = TranslateOpenAIToAnthropicStream(io.NopCloser(strings.NewReader(wire)), "msg_r", "m", true)
				} else {
					rendered, _ = TranslateOpenAIToResponsesStream(io.NopCloser(strings.NewReader(wire)), "resp_r", "m", 1, true)
				}
				defer rendered.Close()
				body, err := io.ReadAll(rendered)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(body), "event: error\n") != 1 || strings.Contains(string(body), "must not leak") ||
					strings.Contains(string(body), "event: message_stop\n") || strings.Contains(string(body), "event: response.completed\n") {
					t.Errorf("mixed refusal chunk became content/success: %s", body)
				}
			})
		}
	}
}
