package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1Responses_RejectedExecutionSettlesOnce(t *testing.T) {
	const refusal = `{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help"}]}`
	for _, surface := range []string{"chat/completions", "messages", "responses"} {
		for _, kind := range []string{"failed", "refusal", "stream_delta", "stream_terminal"} {
			for _, reported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/reported=%t", surface, kind, reported), func(t *testing.T) {
					f := newMatrixFixture(t, domain.ProtocolOpenAIResponses)
					stream := strings.HasPrefix(kind, "stream_")
					output, status := "["+refusal+"]", "completed"
					if kind == "failed" {
						status = "failed"
						output = `[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]`
					}
					answer := fmt.Sprintf(`{"id":"resp_rejected","object":"response","status":%q,"output":%s`, status, output)
					if reported {
						answer += `,"usage":{"input_tokens":10,"output_tokens":3}`
					}
					answer += "}"
					f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
						if !stream {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, answer)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"resp_rejected"}}`+"\n\n")
						if kind == "stream_delta" {
							_, _ = io.WriteString(w, "data: "+`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"I cannot help"}`+"\n\n")
						}
						_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":`+answer+"}\n\n")
					})
					body := map[string]any{"model": "glm-4.6", "stream": stream}
					if surface == "responses" {
						body["input"], body["max_output_tokens"], body["store"] = "hello", 32, true
					} else {
						body["messages"] = []map[string]any{{"role": "user", "content": "hello"}}
						body["max_tokens"] = 32
					}
					w := f.call(t, http.MethodPost, "/v1/"+surface, body)
					wantStatus := http.StatusBadGateway
					if stream {
						wantStatus = http.StatusOK
					}
					if w.Code != wantStatus || !strings.Contains(w.Body.String(), `"error"`) {
						t.Errorf("rejected execution status=%d body=%s", w.Code, w.Body.String())
					}
					for _, terminal := range []string{"[DONE]", "event: message_stop\n", "event: response.completed\n", "event: response.incomplete\n"} {
						if strings.Contains(w.Body.String(), terminal) {
							t.Errorf("rejected execution emitted %q: %s", terminal, w.Body.String())
						}
					}
					if f.upCalls.Load() != 1 {
						t.Errorf("executed response dispatched %d times", f.upCalls.Load())
					}
					var requestStatus, usageStatus, attemptStatus, errorKind, source string
					var upstreamID *string
					var input, out, charge int64
					if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, a.error_kind,
						a.upstream_request_id, u.source, u.input_tokens, u.output_tokens, r.settled_micros
						FROM inference_requests r JOIN inference_attempts a ON a.request_id=r.id
						JOIN inference_usage_records u ON u.attempt_id=a.id`).
						Scan(&requestStatus, &usageStatus, &attemptStatus, &errorKind, &upstreamID,
							&source, &input, &out, &charge); err != nil {
						t.Fatal(err)
					}
					wantSource, wantInput, wantOutput := "estimated", int64(22), int64(6)
					if kind == "failed" {
						wantOutput = 3
					}
					if reported {
						wantInput, wantOutput = 10, 3
						if !stream {
							wantSource = "reported"
						}
					}
					wantKind := "response_translation"
					if stream {
						wantKind = "stream_interrupted"
					}
					if requestStatus != "settled" || usageStatus != wantSource || source != wantSource ||
						attemptStatus != "failed" || errorKind != wantKind || input != wantInput ||
						out != wantOutput || charge != wantInput+2*wantOutput {
						t.Errorf("request=%s/%s attempt=%s/%s usage=%s:%d/%d charge=%d",
							requestStatus, usageStatus, attemptStatus, errorKind, source, input, out, charge)
					}
					if !stream && (upstreamID == nil || *upstreamID != "resp_rejected") {
						t.Errorf("failed execution lost upstream identity: %v", upstreamID)
					}
					for _, check := range []struct {
						sql  string
						want int
					}{
						{`SELECT COUNT(*) FROM inference_attempts`, 1},
						{`SELECT COUNT(*) FROM inference_usage_records`, 1},
						{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
						{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
						{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='active'`, 0},
						{`SELECT COUNT(*) FROM inference_response_chains`, 0},
						{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
						{`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='account' AND state='settled'`, 1},
					} {
						var n int
						if err := f.db.QueryRow(check.sql).Scan(&n); err != nil {
							t.Fatal(err)
						}
						if n != check.want {
							t.Errorf("%s = %d, want %d", check.sql, n, check.want)
						}
					}
					var tokens, ledgerCharge int64
					if err := f.db.QueryRow(`SELECT settled_tokens FROM inference_rate_reservations WHERE scope='account'`).Scan(&tokens); err != nil {
						t.Fatal(err)
					}
					if err := f.db.QueryRow(`SELECT amount_micros FROM inference_ledger_entries WHERE entry_type='charge'`).Scan(&ledgerCharge); err != nil {
						t.Fatal(err)
					}
					if tokens != wantInput+wantOutput || ledgerCharge != wantInput+2*wantOutput {
						t.Errorf("settled tokens=%d charge=%d, want %d/%d", tokens, ledgerCharge, wantInput+wantOutput, wantInput+2*wantOutput)
					}
				})
			}
		}
	}
}

func TestV1Chat_InvalidThinkingRejectedBeforeAdmission(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	for _, thinking := range []string{`{}`, `{"type":""}`, `{"type":"adaptive"}`, `{"type":"sometimes"}`, `{"type":null}`, `"enabled"`, `[]`, `true`} {
		for _, extra := range []string{"", `,"thinking_enabled":false`, `,"thinking_enabled":true`, `,"reasoning_effort":"high"`} {
			t.Run(thinking+extra, func(t *testing.T) {
				var body map[string]any
				if err := json.Unmarshal([]byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"hello"}],"thinking":`+thinking+extra+`}`), &body); err != nil {
					t.Fatal(err)
				}
				w := f.call(t, http.MethodPost, "/v1/chat/completions", body)
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_input"`) {
					t.Errorf("invalid thinking status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
	if calls := f.upCalls.Load(); calls != 0 {
		t.Errorf("invalid thinking dispatched %d upstream requests", calls)
	}
	for _, table := range []string{"inference_requests", "inference_attempts", "inference_usage_records",
		"inference_reservations", "inference_rate_reservations", "inference_concurrency_leases", "inference_ledger_entries"} {
		if n := f.countRows(t, table); n != 0 {
			t.Errorf("invalid thinking created %d rows in %s", n, table)
		}
	}
}
