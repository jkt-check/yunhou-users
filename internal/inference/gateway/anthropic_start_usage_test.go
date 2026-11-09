package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestAnthropicUpstream_InterruptedInitialOutputSettlesOnce(t *testing.T) {
	for _, ending := range []string{"eof", "error_then_stop"} {
		t.Run(ending, func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":17}}}\n\n"+
						"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
				if ending == "error_then_stop" {
					_, _ = io.WriteString(w,
						"data: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"failed\"}}\n\n"+
							"data: {\"type\":\"message_stop\"}\n\n")
				}
			})
			f := newFixture(t, up, withKeyConcurrencyLimit(1))
			if _, err := f.db.Exec(`UPDATE inference_deployments SET protocol='anthropic_messages' WHERE id=$1`, f.deploymentID); err != nil {
				t.Fatal(err)
			}
			dep := f.snap.Deployments[f.deploymentID]
			dep.Protocol = domain.ProtocolAnthropicMessage
			f.snap.Deployments[f.deploymentID] = dep
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			wire := drainStream(t, out, EndUpstreamBroke)
			if out.Stream.Terminal() || strings.Contains(wire, "[DONE]") || !strings.Contains(wire, `"content":"hi"`) {
				t.Errorf("interrupted stream must retain text without success:\n%s", wire)
			}
			if err := out.Stream.Finish(EndCompleted); err != nil {
				t.Fatalf("duplicate finish: %v", err)
			}
			status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
			if status != "settled" || usageStatus != "estimated" || settled != 59 {
				t.Errorf("request=%s/%s amount=%d, want settled/estimated 59", status, usageStatus, settled)
			}
			var attemptStatus, errorKind, source string
			var input, output, tokens int64
			if err := f.db.QueryRow(`SELECT a.status, a.error_kind, u.source,
				u.input_tokens, u.output_tokens
				FROM inference_attempts a JOIN inference_usage_records u ON u.attempt_id=a.id
				WHERE a.request_id=$1`, out.RequestID).
				Scan(&attemptStatus, &errorKind, &source, &input, &output); err != nil {
				t.Fatal(err)
			}
			if attemptStatus != "failed" || errorKind != "stream_interrupted" || source != "estimated" ||
				input != 25 || output != 17 {
				t.Errorf("attempt=%s/%s usage=%s:%d/%d, want failed/stream_interrupted estimated:25/17",
					attemptStatus, errorKind, source, input, output)
			}
			if err := f.db.QueryRow(`SELECT settled_tokens FROM inference_rate_reservations
				WHERE request_id=$1 AND scope='account' AND state='settled'`, out.RequestID).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if tokens != 42 {
				t.Errorf("settled TPM=%d, want 42", tokens)
			}
			for _, check := range []struct {
				query string
				want  int
			}{
				{`SELECT COUNT(*) FROM inference_attempts WHERE request_id=$1`, 1},
				{`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id=$1 AND entry_type='charge'`, 1},
				{`SELECT COUNT(*) FROM inference_reservations WHERE request_id=$1 AND state='held'`, 0},
				{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE request_id=$1 AND state='held'`, 0},
			} {
				var got int
				if err := f.db.QueryRow(check.query, out.RequestID).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != check.want {
					t.Errorf("%s=%d, want %d", check.query, got, check.want)
				}
			}
			if up.calls.Load() != 1 {
				t.Errorf("upstream calls=%d, want one execution", up.calls.Load())
			}
		})
	}
}
