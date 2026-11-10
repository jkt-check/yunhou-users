package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var unsupportedIncompleteDetails = []struct{ name, field string }{
	{"missing", ""},
	{"null", `,"incomplete_details":null`},
	{"empty_object", `,"incomplete_details":{}`},
	{"empty_reason", `,"incomplete_details":{"reason":""}`},
	{"unknown", `,"incomplete_details":{"reason":"provider_specific"}`},
}

func TestResponsesIncomplete_NonstreamRetainsRejectedEvidence(t *testing.T) {
	for _, tc := range unsupportedIncompleteDetails {
		for _, tool := range []bool{false, true} {
			for _, reported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/tool=%t/reported=%t", tc.name, tool, reported), func(t *testing.T) {
					output := `[{"type":"message","content":[{"type":"output_text","text":"partial"}]}`
					wantBytes := int64(7)
					if tool {
						output += `,{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`
						wantBytes = 9
					}
					body := `{"id":"resp_incomplete","object":"response","status":"incomplete","output":` +
						output + "]" + tc.field
					if reported {
						body += `,"usage":{"input_tokens":10,"output_tokens":3}`
					}
					body += "}"
					res, err := NewOpenAIResponses().DecodeNonStream([]byte(body))
					if err == nil {
						t.Error("unrepresentable incomplete reason became successful completion")
					}
					if res == nil || len(res.Payload) != 0 || res.UpstreamRequestID != "resp_incomplete" ||
						res.ContentBytes != wantBytes || res.UsageReported != reported {
						t.Fatalf("rejected execution lost evidence: %+v", res)
					}
					if reported && (res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 ||
						res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 3 ||
						!strings.Contains(string(res.UsageRaw), `"output_tokens":3`)) {
						t.Errorf("reported evidence lost: %+v", res)
					}
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						_, _ = io.WriteString(w, body)
					}))
					defer up.Close()
					adapter := NewOpenAIResponses()
					result, err := Dispatch(context.Background(), NewHTTPClient(nil), adapter,
						chatCall(testDeployment(adapter.Protocol(), up.URL), false))
					if err == nil || result == nil || len(result.Payload) != 0 ||
						result.UpstreamRequestID != "resp_incomplete" || result.ContentBytes != wantBytes ||
						result.UsageReported != reported {
						t.Errorf("dispatch lost rejected execution evidence: %+v, err=%v", result, err)
					}
				})
			}
		}
	}
}

func TestResponsesIncomplete_StreamRejectsWithoutFalseTerminal(t *testing.T) {
	for _, tc := range unsupportedIncompleteDetails {
		for _, reported := range []bool{false, true} {
			for _, status := range []string{"", `,"status":"incomplete"`, `,"status":"completed"`} {
				for _, size := range []int{1, 4096} {
					t.Run(fmt.Sprintf("%s/reported=%t/status=%s/chunk=%d", tc.name, reported, status, size), func(t *testing.T) {
						terminal := `data: {"type":"response.incomplete","response":{"id":"resp_i"` + status + tc.field
						if reported {
							terminal += `,"usage":{"input_tokens":10,"output_tokens":3}`
						}
						terminal += "}}\n\n"
						wire := "data: " + `{"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n" + terminal
						st := TranslateResponsesStream(io.NopCloser(boundedChunkReader{strings.NewReader(wire), size}))
						defer st.Body.Close()
						body, err := io.ReadAll(st.TeeBody())
						if err != nil {
							t.Fatal(err)
						}
						if strings.Count(string(body), `"error"`) != 1 || strings.Contains(string(body), "[DONE]") ||
							strings.Contains(string(body), `"finish_reason"`) {
							t.Errorf("incomplete emitted a successful terminal or no error: %s", body)
						}
						tap := st.Tap.Result()
						if tap.Terminal || !tap.RejectedComplete || tap.ContentBytes != 7 || tap.SawUsage != reported {
							t.Errorf("rejected terminal evidence = %+v", tap)
						}
						if reported && (tap.Buckets.InputTokens == nil || *tap.Buckets.InputTokens != 10 ||
							tap.Buckets.OutputTokens == nil || *tap.Buckets.OutputTokens != 3) {
							t.Errorf("usage lost: %+v", tap)
						}
					})
				}
			}
		}
	}
}

func TestResponsesIncomplete_ActualFailureDominatesCompleteRejection(t *testing.T) {
	const terminal = "data: " + `{"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n"
	const late = "data: " + `{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":10,"output_tokens":3}}}` + "\n\n"
	for _, failure := range []struct{ name, wire string }{
		{"error", "data: " + `{"type":"error","message":"broken"}` + "\n\n"},
		{"failed", "data: " + `{"type":"response.failed","response":{"status":"failed"}}` + "\n\n"},
		{"read_error", ""},
	} {
		for _, position := range []string{"before", "after"} {
			for _, size := range []int{1, 4096} {
				t.Run(fmt.Sprintf("%s/%s/chunk=%d", failure.name, position, size), func(t *testing.T) {
					wire := terminal + failure.wire + late
					if position == "before" {
						wire = failure.wire + terminal + late
					}
					var src io.Reader = boundedChunkReader{strings.NewReader(wire), size}
					if failure.name == "read_error" {
						src = io.MultiReader(src, &errAfterStringReader{read: true})
					}
					st := TranslateResponsesStream(io.NopCloser(src))
					defer st.Body.Close()
					body, err := io.ReadAll(st.TeeBody())
					if (err != nil) != (failure.name == "read_error") {
						t.Fatalf("read error=%v", err)
					}
					if strings.Contains(string(body), "[DONE]") {
						t.Errorf("failed incomplete stream emitted success: %s", body)
					}
					tap := st.Tap.Result()
					if tap.Terminal || tap.RejectedComplete || !tap.SawUsage ||
						tap.Buckets.InputTokens == nil || *tap.Buckets.InputTokens != 10 ||
						tap.Buckets.OutputTokens == nil || *tap.Buckets.OutputTokens != 3 {
						t.Errorf("failure must dominate late terminals without losing usage: %+v", tap)
					}
				})
			}
		}
	}
}
