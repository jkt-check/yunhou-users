package httpapi_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1ChatRefusal_CompletedExecutionSettlesOnce(t *testing.T) {
	for _, surface := range []string{"chat/completions", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, reported := range []bool{false, true} {
				for _, refusal := range []string{"I cannot help", ""} {
					t.Run(fmt.Sprintf("%s/stream=%t/reported=%t/refusal=%q", surface, stream, reported, refusal), func(t *testing.T) {
						f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
						answer := fmt.Sprintf(`{"id":"chatcmpl-r","choices":[{"message":{"role":"assistant","content":null,"refusal":%q},"finish_reason":"stop"}]`, refusal)
						if reported {
							answer += `,"usage":{"prompt_tokens":10,"completion_tokens":3}`
						}
						answer += "}"
						f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
							if !stream {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, answer)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"refusal\":%q}}]}\n\n", refusal)
							if reported {
								_, _ = io.WriteString(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`+"\n\n")
							}
							_, _ = io.WriteString(w, "data: [DONE]\n\n")
						})
						w := f.call(t, http.MethodPost, "/v1/"+surface, chatRefusalRequest(surface, stream))
						native := surface != "chat/completions"
						wantStatus := http.StatusOK
						if native && !stream {
							wantStatus = http.StatusBadGateway
						}
						if w.Code != wantStatus {
							t.Errorf("status=%d, want %d: %s", w.Code, wantStatus, w.Body.String())
						}
						if native {
							if !strings.Contains(w.Body.String(), `"error"`) {
								t.Errorf("native refusal became success: %s", w.Body.String())
							}
							if stream && strings.Count(w.Body.String(), "event: error\n") != 1 {
								t.Errorf("expected exactly one native error: %s", w.Body.String())
							}
							for _, terminal := range []string{"[DONE]", "event: message_stop\n", "event: response.completed\n", "event: response.incomplete\n"} {
								if strings.Contains(w.Body.String(), terminal) {
									t.Errorf("native refusal emitted %q: %s", terminal, w.Body.String())
								}
							}
						} else if !strings.Contains(w.Body.String(), fmt.Sprintf(`"refusal":%q`, refusal)) {
							t.Errorf("raw Chat lost refusal: %s", w.Body.String())
						}
						var requestStatus, usageStatus, attemptStatus, kind, source string
						var input, output, charge, tokens, ledgerCharge int64
						if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, COALESCE(a.error_kind,''),
							u.source, u.input_tokens, u.output_tokens, r.settled_micros
							FROM inference_requests r JOIN inference_attempts a ON a.request_id=r.id
							JOIN inference_usage_records u ON u.attempt_id=a.id`).
							Scan(&requestStatus, &usageStatus, &attemptStatus, &kind, &source, &input, &output, &charge); err != nil {
							t.Fatal(err)
						}
						wantSource, wantInput, wantOutput := "estimated", int64(22), int64(6)
						if refusal == "" {
							wantOutput = 0
						}
						if reported {
							wantInput, wantOutput = 10, 3
							if !stream || !native {
								wantSource = "reported"
							}
						}
						wantAttempt, wantKind := "completed", ""
						if native {
							wantAttempt, wantKind = "failed", "response_translation"
							if stream {
								wantKind = "stream_interrupted"
							}
						}
						if requestStatus != "settled" || usageStatus != wantSource || source != wantSource ||
							attemptStatus != wantAttempt || kind != wantKind || input != wantInput ||
							output != wantOutput || charge != wantInput+2*wantOutput {
							t.Errorf("request=%s/%s attempt=%s/%s usage=%s:%d/%d charge=%d; want %s:%d/%d %s/%s",
								requestStatus, usageStatus, attemptStatus, kind, source, input, output, charge,
								wantSource, wantInput, wantOutput, wantAttempt, wantKind)
						}
						if err := f.db.QueryRow(`SELECT settled_tokens FROM inference_rate_reservations WHERE scope='account' AND state='settled'`).Scan(&tokens); err != nil {
							t.Fatal(err)
						}
						if err := f.db.QueryRow(`SELECT amount_micros FROM inference_ledger_entries WHERE entry_type='charge'`).Scan(&ledgerCharge); err != nil {
							t.Fatal(err)
						}
						if tokens != wantInput+wantOutput || ledgerCharge != wantInput+2*wantOutput {
							t.Errorf("TPM=%d charge=%d, want %d/%d", tokens, ledgerCharge, wantInput+wantOutput, wantInput+2*wantOutput)
						}
						for _, check := range []struct {
							sql  string
							want int
						}{
							{`SELECT COUNT(*) FROM inference_attempts`, 1},
							{`SELECT COUNT(*) FROM inference_usage_records`, 1},
							{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
							{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
							{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, 0},
							{`SELECT COUNT(*) FROM inference_response_chains`, 0},
							{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
						} {
							var n int
							if err := f.db.QueryRow(check.sql).Scan(&n); err != nil {
								t.Fatal(err)
							}
							if n != check.want {
								t.Errorf("%s = %d, want %d", check.sql, n, check.want)
							}
						}
						if f.upCalls.Load() != 1 {
							t.Errorf("refused execution dispatched %d times", f.upCalls.Load())
						}
					})
				}
			}
		}
	}
}

func TestV1ChatRefusal_InterruptedStreamRetainsEvidence(t *testing.T) {
	for _, surface := range []string{"chat/completions", "messages", "responses"} {
		for _, ending := range []string{"eof", "error_after_done"} {
			for _, reported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/reported=%t", surface, ending, reported), func(t *testing.T) {
					f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
					f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+`{"choices":[{"delta":{"refusal":"I cannot help"}}]}`+"\n\n")
						if ending == "error_after_done" {
							_, _ = io.WriteString(w, "data: [DONE]\n\n"+
								"data: "+`{"error":{"message":"broken","type":"api_error","code":"broken"}}`+"\n\n")
						}
						if reported {
							_, _ = io.WriteString(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`+"\n\n")
						}
					})
					w := f.call(t, http.MethodPost, "/v1/"+surface, chatRefusalRequest(surface, true))
					if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error"`) {
						t.Errorf("interrupted stream status=%d body=%s", w.Code, w.Body.String())
					}
					if surface != "chat/completions" {
						if strings.Count(w.Body.String(), "event: error\n") != 1 ||
							strings.Contains(w.Body.String(), "event: response.completed\n") ||
							strings.Contains(w.Body.String(), "event: message_stop\n") {
							t.Errorf("native interrupted refusal needs exactly one error and no completion: %s", w.Body.String())
						}
					}
					var status, usageStatus, attemptStatus, kind string
					if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, a.error_kind
						FROM inference_requests r JOIN inference_attempts a ON a.request_id=r.id`).
						Scan(&status, &usageStatus, &attemptStatus, &kind); err != nil {
						t.Fatal(err)
					}
					wantStatus, wantUsage := "reconciliation_required", "unknown"
					holds, jobs, usages := 4, 1, 0
					if reported {
						wantStatus, wantUsage = "settled", "estimated"
						holds, jobs, usages = 0, 0, 1
						var input, output, charge, tokens int64
						if err := f.db.QueryRow(`SELECT u.input_tokens, u.output_tokens, r.settled_micros, rr.settled_tokens
							FROM inference_requests r JOIN inference_usage_records u ON u.request_id=r.id
							JOIN inference_rate_reservations rr ON rr.request_id=r.id AND rr.scope='account'`).
							Scan(&input, &output, &charge, &tokens); err != nil {
							t.Fatal(err)
						}
						if input != 10 || output != 3 || charge != 16 || tokens != 13 {
							t.Errorf("late usage=%d/%d charge=%d TPM=%d, want 10/3,16,13", input, output, charge, tokens)
						}
					}
					if status != wantStatus || usageStatus != wantUsage || attemptStatus != "failed" ||
						kind != "stream_interrupted" || f.upCalls.Load() != 1 {
						t.Errorf("request=%s/%s attempt=%s/%s calls=%d", status, usageStatus, attemptStatus, kind, f.upCalls.Load())
					}
					for _, check := range []struct {
						sql  string
						want int
					}{
						{`SELECT COUNT(*) FROM inference_attempts`, 1},
						{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, holds},
						{`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE status='pending'`, jobs},
						{`SELECT COUNT(*) FROM inference_usage_records`, usages},
						{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, usages},
						{`SELECT COUNT(*) FROM inference_response_chains`, 0},
						{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, 0},
					} {
						var n int
						if err := f.db.QueryRow(check.sql).Scan(&n); err != nil {
							t.Fatal(err)
						}
						if n != check.want {
							t.Errorf("%s = %d, want %d", check.sql, n, check.want)
						}
					}
				})
			}
		}
	}
}

func chatRefusalRequest(surface string, stream bool) map[string]any {
	body := map[string]any{"model": "glm-4.6", "stream": stream}
	if surface == "responses" {
		body["input"], body["max_output_tokens"], body["store"] = "hello", 32, true
	} else {
		body["messages"] = []map[string]any{{"role": "user", "content": "hello"}}
		body["max_tokens"] = 32
	}
	return body
}
