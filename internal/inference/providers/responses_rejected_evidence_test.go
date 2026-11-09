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
)

func TestResponsesNonstream_RejectedExecutionKeepsMetering(t *testing.T) {
	for _, tc := range []struct {
		name, status, output string
		bytes                int64
	}{
		{"failed", "failed", `[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]`, 7},
		{"refusal", "completed", `[{"type":"message","content":[{"type":"refusal","refusal":"I cannot help"}]}]`, 13},
		{"empty_refusal", "completed", `[{"type":"message","content":[{"type":"refusal","refusal":""}]}]`, 0},
		{"mixed_refusal", "completed", `[{"type":"message","content":[{"type":"output_text","text":"before"},{"type":"refusal","refusal":"I cannot help"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]`, 21},
	} {
		for _, reported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reported=%t", tc.name, reported), func(t *testing.T) {
				body := fmt.Sprintf(`{"object":"response","id":"resp_rejected","status":%q,"output":%s`, tc.status, tc.output)
				if reported {
					body += `,"usage":{"input_tokens":10,"output_tokens":3}`
				}
				body += "}"
				adapter := NewOpenAIResponses()
				res, err := adapter.DecodeNonStream([]byte(body))
				if err == nil {
					t.Error("failed or unrepresentable output must not become a successful completion")
				}
				if res == nil {
					t.Fatal("fully decoded execution lost its metering result")
				}
				if len(res.Payload) != 0 || res.UpstreamRequestID != "resp_rejected" ||
					res.ContentBytes != tc.bytes || res.UsageReported != reported {
					t.Errorf("rejected execution = %+v", res)
				}
				if reported {
					if res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 ||
						res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 3 ||
						!strings.Contains(string(res.UsageRaw), `"output_tokens":3`) {
						t.Errorf("reported evidence lost: %+v", res)
					}
				} else if res.Usage.InputTokens != nil || res.Usage.OutputTokens != nil || len(res.UsageRaw) != 0 {
					t.Errorf("absent usage became reported zero: %+v", res)
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, body)
				}))
				defer up.Close()
				dispatched, dispatchErr := Dispatch(context.Background(), NewHTTPClient(nil), adapter,
					chatCall(testDeployment(adapter.Protocol(), up.URL), false))
				if dispatchErr == nil || dispatched == nil || len(dispatched.Payload) != 0 ||
					dispatched.UpstreamRequestID != "resp_rejected" || dispatched.ContentBytes != tc.bytes ||
					dispatched.UsageReported != reported {
					t.Errorf("dispatch must retain executed evidence without replay eligibility: result=%+v err=%v",
						dispatched, dispatchErr)
				}
			})
		}
	}
}

func TestResponsesStream_RefusalRejectsWithoutLosingEvidence(t *testing.T) {
	const part = `{"type":"refusal","refusal":"I cannot help"}`
	const item = `{"type":"message","id":"msg_1","role":"assistant","content":[` + part + `]}`
	event := func(s string) string { return "data: " + s + "\n\n" }
	deltas := event(`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"I cannot "}`) +
		event(`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"help"}`)
	for _, tc := range []struct {
		name, events, output string
		bytes                int64
	}{
		{"delta", deltas, "[]", 13},
		{"done", event(`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"I cannot help"}`), "[]", 13},
		{"part_added", event(`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":` + part + `}`), "[]", 13},
		{"part_done", event(`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + part + `}`), "[]", 13},
		{"item_added", event(`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`), "[]", 13},
		{"item_done", event(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`), "[]", 13},
		{"terminal_only", "", "[" + item + "]", 13},
		{"empty_refusal", event(`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`), "[]", 0},
		{"cumulative_not_additive", deltas +
			event(`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"I cannot help"}`) +
			event(`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":`+part+`}`) +
			event(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`), "[" + item + "]", 13},
		{"distinct_parts", deltas +
			event(`{"type":"response.refusal.delta","output_index":0,"content_index":1,"delta":"no"}`) +
			event(`{"type":"response.refusal.delta","output_index":1,"content_index":0,"delta":"stop"}`), "[]", 19},
	} {
		for _, reported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reported=%t", tc.name, reported), func(t *testing.T) {
				terminal := `{"type":"response.completed","response":{"status":"completed","output":` + tc.output
				if reported {
					terminal += `,"usage":{"input_tokens":10,"output_tokens":3}`
				}
				terminal += "}}"
				fixture := event(`{"type":"response.created","response":{"id":"resp_refusal"}}`) + tc.events + event(terminal)
				st := TranslateResponsesStream(io.NopCloser(strings.NewReader(fixture)))
				defer st.Body.Close()
				body, err := io.ReadAll(st.TeeBody())
				if err != nil {
					t.Fatal(err)
				}
				wire := string(body)
				if !strings.Contains(wire, `"error"`) {
					t.Errorf("unrepresentable refusal needs an explicit error: %s", wire)
				}
				if strings.Contains(wire, "[DONE]") || strings.Contains(wire, `"finish_reason":"stop"`) {
					t.Errorf("refusal was converted into a successful terminal: %s", wire)
				}
				res := st.Tap.Result()
				if res.Terminal || res.SawUsage != reported || res.ContentBytes != tc.bytes {
					t.Errorf("refusal metering = %+v, want failed, reported=%t, bytes=%d", res, reported, tc.bytes)
				}
				if reported {
					if res.Buckets.InputTokens == nil || *res.Buckets.InputTokens != 10 ||
						res.Buckets.OutputTokens == nil || *res.Buckets.OutputTokens != 3 ||
						!json.Valid(res.Raw) || !strings.Contains(wire, `"completion_tokens":3`) {
						t.Errorf("late reported usage must survive rejection: tap=%+v wire=%s", res, wire)
					}
				}
			})
		}
	}
}
