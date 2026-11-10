package httpapi_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestV1Nonstream_OrderRejectionSettlesWithoutReplay(t *testing.T) {
	for _, proto := range []domain.Protocol{domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses} {
		t.Run(string(proto), func(t *testing.T) {
			for _, surface := range []string{"chat/completions", "messages", "responses"} {
				t.Run(surface, func(t *testing.T) {
					for _, reported := range []bool{true, false} {
						t.Run(fmt.Sprintf("reported=%t", reported), func(t *testing.T) {
							f := newMatrixFixture(t, proto)
							var answer string
							if proto == domain.ProtocolAnthropicMessage {
								answer = `{"id":"msg_order","type":"message","content":[` +
									`{"type":"text","text":"before"},` +
									`{"type":"tool_use","id":"call_1","name":"lookup","input":{}},` +
									`{"type":"text","text":"after"}],"stop_reason":"tool_use"`
							} else {
								answer = `{"id":"resp_order","object":"response","status":"completed","output":[` +
									`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]},` +
									`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},` +
									`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}]`
							}
							if reported {
								answer += `,"usage":{"input_tokens":7,"output_tokens":5}`
							}
							answer += "}"
							f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, _ []byte) {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, answer)
							})
							body := map[string]any{"model": "glm-4.6", "stream": false}
							if surface == "responses" {
								body["input"] = "hello"
								body["max_output_tokens"] = 32
							} else {
								body["messages"] = []map[string]any{{"role": "user", "content": "hello"}}
								body["max_tokens"] = 32
							}
							w := f.call(t, http.MethodPost, "/v1/"+surface, body)
							if w.Code != http.StatusBadGateway {
								t.Errorf("order rejection status=%d body=%s", w.Code, w.Body.String())
							}
							if f.upCalls.Load() != 1 {
								t.Errorf("executed response replayed %d times", f.upCalls.Load())
							}
							var status, usageStatus, attemptStatus, errorKind, source string
							var input, output int64
							if err := f.db.QueryRow(`SELECT r.status, r.usage_status, a.status, a.error_kind,
								u.source, u.input_tokens, u.output_tokens
								FROM inference_requests r JOIN inference_attempts a ON a.request_id=r.id
								JOIN inference_usage_records u ON u.attempt_id=a.id`).
								Scan(&status, &usageStatus, &attemptStatus, &errorKind, &source, &input, &output); err != nil {
								t.Fatal(err)
							}
							wantSource, wantInput, wantOutput := "estimated", int64(22), int64(6)
							if reported {
								wantSource, wantInput, wantOutput = "reported", 7, 5
							}
							if status != "settled" || usageStatus != wantSource || source != wantSource ||
								attemptStatus != "failed" || errorKind != "response_translation" ||
								input != wantInput || output != wantOutput {
								t.Errorf("request=%s/%s attempt=%s/%s usage=%s:%d/%d",
									status, usageStatus, attemptStatus, errorKind, source, input, output)
							}
							for _, check := range []struct {
								sql  string
								want int
							}{
								{`SELECT COUNT(*) FROM inference_attempts`, 1},
								{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`, 1},
								{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
								{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='active'`, 0},
								{`SELECT COUNT(*) FROM inference_response_chains`, 0},
								{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
								{`SELECT COUNT(*) FROM inference_rate_reservations
									WHERE scope='account' AND state='settled'`, 1},
							} {
								var n int
								if err := f.db.QueryRow(check.sql).Scan(&n); err != nil {
									t.Fatal(err)
								}
								if n != check.want {
									t.Errorf("%s = %d, want %d", check.sql, n, check.want)
								}
							}
							var tokens int64
							if err := f.db.QueryRow(`SELECT settled_tokens FROM inference_rate_reservations
								WHERE scope='account'`).Scan(&tokens); err != nil {
								t.Fatal(err)
							}
							if tokens != wantInput+wantOutput {
								t.Errorf("settled TPM=%d, want %d", tokens, wantInput+wantOutput)
							}
						})
					}
				})
			}
		})
	}
}
