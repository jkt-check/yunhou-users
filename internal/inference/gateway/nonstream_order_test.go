package gateway

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestGateway_NonstreamOrderRejectionDoesNotFailover(t *testing.T) {
	for _, proto := range []domain.Protocol{domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses} {
		t.Run(string(proto), func(t *testing.T) {
			answer := `{"id":"msg_order","type":"message","content":[` +
				`{"type":"text","text":"before"},` +
				`{"type":"tool_use","id":"call_1","name":"lookup","input":{}},` +
				`{"type":"text","text":"after"}],"stop_reason":"tool_use",` +
				`"usage":{"input_tokens":7,"output_tokens":5}}`
			if proto == domain.ProtocolOpenAIResponses {
				answer = `{"id":"resp_order","object":"response","status":"completed","output":[` +
					`{"type":"message","content":[{"type":"output_text","text":"before"}]},` +
					`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},` +
					`{"type":"message","content":[{"type":"output_text","text":"after"}]}],` +
					`"usage":{"input_tokens":7,"output_tokens":5}}`
			}
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, answer)
			})
			f := newFixture(t, up, withNoWindows(), withKeyConcurrencyLimit(1))
			dep := f.snap.Deployments[f.deploymentID]
			dep.Protocol = proto
			f.snap.Deployments[f.deploymentID] = dep
			fallback := newUpstream(t, completionHandler)
			addSecondProvider(t, f, fallback)
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hello"))
			if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
				t.Errorf("unrepresentable output must fail explicitly, got %v", err)
			}
			if out == nil {
				t.Fatal("executed rejection must retain its finalized request identity")
			}
			if len(out.Payload) != 0 || out.Stream != nil {
				t.Errorf("unrepresentable output was served: %+v", out)
			}
			if up.calls.Load() != 1 || fallback.calls.Load() != 0 {
				t.Errorf("upstream calls=%d fallback calls=%d", up.calls.Load(), fallback.calls.Load())
			}
			status, usageStatus, _, amount := f.requestRow(t, out.RequestID)
			if status != "settled" || usageStatus != "reported" || amount != 17 {
				t.Errorf("request=%s/%s amount=%d, want settled/reported/17", status, usageStatus, amount)
			}
			for _, check := range []struct {
				sql  string
				want int
			}{
				{`SELECT COUNT(*) FROM inference_attempts WHERE status='failed' AND error_kind='response_translation'`, 1},
				{`SELECT COUNT(*) FROM inference_attempts`, 1},
				{`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge' AND amount_micros=17`, 1},
				{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 0},
				{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='active'`, 0},
				{`SELECT COUNT(*) FROM inference_reconciliation_jobs`, 0},
				{`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='account' AND state='settled' AND settled_tokens=12`, 1},
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
