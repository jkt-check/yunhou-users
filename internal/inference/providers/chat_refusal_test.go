package providers

import (
	"fmt"
	"io"
	"strings"
	"testing"
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
