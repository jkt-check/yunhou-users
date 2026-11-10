package gateway

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
)

func (f *fixture) replaySettlement(t *testing.T, usage domain.UsageRecord) {
	t.Helper()
	ctx := context.Background()
	request, err := f.store.GetRequest(ctx, usage.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	priceRow, err := f.store.GetPriceVersion(ctx, *request.PriceVersionID)
	if err != nil {
		t.Fatal(err)
	}
	price, err := priceRow.Pure()
	if err != nil {
		t.Fatal(err)
	}
	hold, ok := domain.HoldUpperBound(request)
	if !ok {
		t.Fatal("settlement replay has no persisted monetary upper bound")
	}
	f.gateway.settle(ctx, usage.RequestID, usage.AttemptID, usage.Buckets,
		usage.Source, usage.RawUsage.Raw, price, providers.NewOpenAIChat(),
		&quota.AdmissionResult{RequestID: usage.RequestID, HoldMicros: hold,
			ChargeSource: request.ChargeSource})
}

func TestGateway_ReplayedEvidenceMismatchRequiresReconciliation(t *testing.T) {
	for _, zeroPrice := range []bool{false, true} {
		name := "positive"
		var opts []fixtureOpt
		if zeroPrice {
			name = "zero"
			opts = append(opts, withZeroPrice())
		}
		t.Run(name, func(t *testing.T) {
			for _, change := range []string{"estimated_to_reported", "reported_to_estimated", "raw_changed", "raw_missing"} {
				t.Run(change, func(t *testing.T) {
					reported := change != "estimated_to_reported"
					f := newFixture(t, newUpstream(t, jsonUsageHandler(reported)), opts...)
					ctx := context.Background()
					p, key := f.principal()
					result, err := f.gateway.ChatCompletions(ctx, p, key,
						domain.ProtocolOpenAIChat, chatReq(false, "hi"))
					if err != nil {
						t.Fatal(err)
					}
					records, err := f.store.LoadUsageRecords(ctx, result.RequestID)
					if err != nil || len(records) != 1 {
						t.Fatalf("original usage = %v, %v", records, err)
					}
					wantCharge := int64(20)
					if !reported {
						wantCharge = 33
					}
					if zeroPrice {
						wantCharge = 0
					}
					if got := f.ledgerCharge(t, result.RequestID); got != wantCharge {
						t.Fatalf("original charge = %d, want %d", got, wantCharge)
					}
					requestBefore, err := f.store.GetRequest(ctx, result.RequestID)
					if err != nil {
						t.Fatal(err)
					}
					var rateBefore string
					if err := f.db.Get(&rateBefore, `SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id)::text
						FROM inference_rate_reservations r WHERE request_id=$1`, result.RequestID); err != nil {
						t.Fatal(err)
					}
					delivery := records[0]
					switch change {
					case "estimated_to_reported":
						delivery.Source = domain.UsageReported
					case "reported_to_estimated":
						delivery.Source = domain.UsageEstimated
					case "raw_changed":
						delivery.RawUsage.Raw = json.RawMessage(`{"schema_version":1,"usage":{"prompt_tokens":9,"completion_tokens":6}}`)
					case "raw_missing":
						delivery.RawUsage.Raw = nil
					}
					var alarms []error
					f.gateway.OnSettleError = func(requestID string, err error) {
						if requestID != result.RequestID {
							t.Errorf("alarm request = %s, want %s", requestID, result.RequestID)
						}
						alarms = append(alarms, err)
					}
					f.replaySettlement(t, delivery)
					if len(alarms) != 1 {
						t.Errorf("settlement alarms = %d, want 1", len(alarms))
					} else if outcome, ok := domain.AsSettleError(alarms[0]); !ok ||
						outcome.Kind != domain.SettleOutcomeChargeMismatch ||
						outcome.ExistingChargeMicros == nil ||
						*outcome.ExistingChargeMicros != domain.Microcredit(wantCharge) ||
						outcome.RequestedChargeMicros != domain.Microcredit(wantCharge) {
						t.Errorf("alarm = %v, want equal-money charge_mismatch", alarms[0])
					}
					reason, status, found := f.reconciliationJob(t, result.RequestID)
					if !found || reason != "settlement_mismatch" || status != "pending" {
						t.Errorf("job = %t/%s/%s, want true/settlement_mismatch/pending", found, reason, status)
					}
					after, err := f.store.LoadUsageRecords(ctx, result.RequestID)
					if err != nil || !reflect.DeepEqual(after, records) {
						t.Errorf("replay changed persisted usage: %v, %v", after, err)
					}
					requestAfter, err := f.store.GetRequest(ctx, result.RequestID)
					if err != nil {
						t.Fatal(err)
					}
					if requestAfter.Status != domain.ReqSettled ||
						requestAfter.UsageStatus != requestBefore.UsageStatus ||
						requestAfter.SettledMicros == nil || int64(*requestAfter.SettledMicros) != wantCharge ||
						requestAfter.CompletedAt == nil || requestBefore.CompletedAt == nil ||
						!requestAfter.CompletedAt.Equal(*requestBefore.CompletedAt) {
						t.Errorf("replay changed terminal facts: %+v", requestAfter)
					}
					var rateAfter string
					if err := f.db.Get(&rateAfter, `SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id)::text
						FROM inference_rate_reservations r WHERE request_id=$1`, result.RequestID); err != nil {
						t.Fatal(err)
					}
					if rateAfter != rateBefore || f.chargeRows(t, result.RequestID) != 1 ||
						f.ledgerCharge(t, result.RequestID) != wantCharge {
						t.Error("replay changed rate accounting or charged again")
					}
				})
			}
		})
	}
}

func TestGateway_ExactEstimatedReplayIsIdempotent(t *testing.T) {
	f := newFixture(t, newUpstream(t, jsonUsageHandler(false)))
	ctx := context.Background()
	p, key := f.principal()
	result, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.store.LoadUsageRecords(ctx, result.RequestID)
	if err != nil || len(records) != 1 || records[0].Source != domain.UsageEstimated {
		t.Fatalf("original usage = %v, %v", records, err)
	}
	before, err := f.store.GetRequest(ctx, result.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.OnSettleError = func(_ string, err error) { t.Errorf("exact replay alarm: %v", err) }
	f.replaySettlement(t, records[0])
	after, err := f.store.GetRequest(ctx, result.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || f.chargeRows(t, result.RequestID) != 1 {
		t.Error("exact replay changed settled request or charged twice")
	}
	if reason, _, found := f.reconciliationJob(t, result.RequestID); found {
		t.Errorf("exact replay queued %s", reason)
	}
	if before.CompletedAt == nil || before.CompletedAt.After(time.Now()) {
		t.Errorf("missing original completion time: %v", before.CompletedAt)
	}
}
