package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestGateway_MissingSettledUsageRequiresReconciliation(t *testing.T) {
	for _, zeroPrice := range []bool{false, true} {
		name, wantCharge := "positive", int64(20)
		var opts []fixtureOpt
		if zeroPrice {
			name, wantCharge = "zero", 0
			opts = append(opts, withZeroPrice())
		}
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, jsonUsageHandler(true))
			f := newFixture(t, up, opts...)
			p, key := f.principal()
			result, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			var completedAt, rateSettledAt time.Time
			var tokens int64
			if err := f.db.QueryRow(`SELECT completed_at FROM inference_requests WHERE id=$1`,
				result.RequestID).Scan(&completedAt); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`
				SELECT settled_tokens, settled_at FROM inference_rate_reservations
				WHERE request_id=$1 AND scope='account'`, result.RequestID).
				Scan(&tokens, &rateSettledAt); err != nil {
				t.Fatal(err)
			}
			if tokens != 15 {
				t.Fatalf("initial TPM = %d, want 15", tokens)
			}
			if _, err := f.db.Exec(`DELETE FROM inference_usage_records WHERE request_id=$1`,
				result.RequestID); err != nil {
				t.Fatal(err)
			}

			var alarms []error
			f.gateway.OnSettleError = func(requestID string, err error) {
				if requestID != result.RequestID {
					t.Errorf("alarm request = %s, want %s", requestID, result.RequestID)
				}
				alarms = append(alarms, err)
			}
			in, out := int64(10), int64(5)
			f.settleDirect(t, result.RequestID, domain.UsageBuckets{InputTokens: &in, OutputTokens: &out})

			if len(alarms) != 1 {
				t.Errorf("settlement alarms = %d, want 1", len(alarms))
			} else if outcome, ok := domain.AsSettleError(alarms[0]); !ok ||
				outcome.Kind != domain.SettleOutcomeChargeMismatch {
				t.Errorf("alarm = %v, want typed charge_mismatch", alarms[0])
			}
			reason, jobStatus, found := f.reconciliationJob(t, result.RequestID)
			if !found || reason != "settlement_mismatch" || jobStatus != "pending" {
				t.Errorf("reconciliation job = %t/%s/%s, want true/settlement_mismatch/pending",
					found, reason, jobStatus)
			}
			if count := f.chargeRows(t, result.RequestID); count != 1 {
				t.Errorf("charge rows = %d, want 1", count)
			}
			if charge := f.ledgerCharge(t, result.RequestID); charge != wantCharge {
				t.Errorf("charge = %d, want %d", charge, wantCharge)
			}
			status, usage, _, settled := f.requestRow(t, result.RequestID)
			if status != "settled" || usage != "reported" || settled != wantCharge {
				t.Errorf("request = %s/%s/%d, want settled/reported/%d", status, usage, settled, wantCharge)
			}
			var completedAfter, rateAfter time.Time
			var tokensAfter int64
			var rateState string
			if err := f.db.QueryRow(`SELECT completed_at FROM inference_requests WHERE id=$1`,
				result.RequestID).Scan(&completedAfter); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`
				SELECT settled_tokens, settled_at, state FROM inference_rate_reservations
				WHERE request_id=$1 AND scope='account'`, result.RequestID).
				Scan(&tokensAfter, &rateAfter, &rateState); err != nil {
				t.Fatal(err)
			}
			if !completedAfter.Equal(completedAt) || !rateAfter.Equal(rateSettledAt) ||
				tokensAfter != 15 || rateState != "settled" {
				t.Errorf("terminal/TPM changed: completed=%v rate=%v/%d/%s",
					completedAfter, rateAfter, tokensAfter, rateState)
			}
			if records := f.usageRecords(t, result.RequestID); len(records) != 0 {
				t.Errorf("replay fabricated %d replacement usage records", len(records))
			}
		})
	}
}
