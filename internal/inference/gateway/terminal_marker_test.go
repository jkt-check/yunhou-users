package gateway

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

func TestGateway_NonstreamFinalizerPreservesCompetingTerminal(t *testing.T) {
	for _, zeroPrice := range []bool{false, true} {
		price := "positive"
		var opts []fixtureOpt
		if zeroPrice {
			price = "zero"
			opts = append(opts, withZeroPrice())
		}
		t.Run(price, func(t *testing.T) {
			for _, competing := range []string{"duplicate", "different_tokens", "estimated_source", "released"} {
				t.Run(competing, func(t *testing.T) {
					f := newFixture(t, newUpstream(t, nonStreamUsage(10, 5)), opts...)
					ctx := context.Background()
					var requestID, incomingAttemptID, before string
					var requestBefore *domain.Request
					var alarms []error
					hooks := 0
					f.gateway.OnSettleError = func(id string, err error) {
						if id != requestID {
							t.Errorf("alarm request = %s, want %s", id, requestID)
						}
						alarms = append(alarms, err)
					}
					f.gateway.store = &nonstreamFinalizationStore{
						Store: f.store,
						beforeFinish: func(ctx context.Context, attemptID string) {
							hooks++
							incomingAttemptID = attemptID
							if err := f.db.GetContext(ctx, &requestID,
								`SELECT request_id FROM inference_attempts WHERE id=$1`, attemptID); err != nil {
								t.Fatal(err)
							}
							if competing == "released" {
								uow, err := f.store.Begin(ctx)
								if err != nil {
									t.Fatal(err)
								}
								defer uow.Rollback(ctx)
								if err := f.store.Release(ctx, uow, requestID); err != nil {
									t.Fatal(err)
								}
								if err := uow.Commit(ctx); err != nil {
									t.Fatal(err)
								}
							} else {
								decoded, err := providers.NewOpenAIChat().DecodeNonStream(
									[]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
								if err != nil {
									t.Fatal(err)
								}
								usage := domain.UsageRecord{
									RequestID: requestID, AttemptID: attemptID, Source: domain.UsageReported,
									Buckets:  decoded.Usage,
									RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: decoded.UsageRaw},
								}
								charge := domain.Microcredit(20)
								if competing == "different_tokens" {
									input := int64(20)
									usage.Buckets.InputTokens = &input
									charge = 30
								}
								if competing == "estimated_source" {
									usage.Source = domain.UsageEstimated
								}
								if zeroPrice {
									charge = 0
								}
								if err := f.gateway.settleOnce(ctx, domain.SettleCommand{
									RequestID: requestID, Usage: usage, ChargeMicros: charge,
									SettledAt: time.Now().UTC().Add(-2 * time.Minute),
								}, nil); err != nil {
									t.Fatal(err)
								}
							}
							var err error
							requestBefore, err = f.store.GetRequest(ctx, requestID)
							if err != nil {
								t.Fatal(err)
							}
							before = terminalFinalizerSnapshot(t, f, requestID)
						},
					}
					p, key := f.principal()
					out, err := f.gateway.ChatCompletions(ctx, p, key,
						domain.ProtocolOpenAIChat, chatReq(false, "hi"))
					if err != nil || out == nil || out.RequestID != requestID || hooks != 1 {
						t.Fatalf("finalization = %+v, %v, hooks=%d", out, err, hooks)
					}
					if after := terminalFinalizerSnapshot(t, f, requestID); after != before {
						t.Error("late finalizer rewrote terminal, usage, money or original TPM facts")
					}
					reason, status, found := f.reconciliationJob(t, requestID)
					if competing == "duplicate" {
						if len(alarms) != 0 || found {
							t.Errorf("exact duplicate alarms=%v job=%t/%s", alarms, found, reason)
						}
						after, err := f.store.GetRequest(ctx, requestID)
						if err != nil || !reflect.DeepEqual(after, requestBefore) {
							t.Errorf("duplicate changed request timestamps/error: %+v, %v", after, err)
						}
					} else {
						wantKind, wantReason := domain.SettleOutcomeChargeMismatch, "settlement_mismatch"
						if competing == "released" {
							wantKind, wantReason = domain.SettleOutcomeReleased, "late_usage_after_release"
						}
						if len(alarms) != 1 {
							t.Errorf("alarms = %v, want one %s", alarms, wantKind)
						} else if outcome, ok := domain.AsSettleError(alarms[0]); !ok || outcome.Kind != wantKind ||
							outcome.Status != requestBefore.Status || (competing == "released" && outcome.Trivial) {
							t.Errorf("alarm = %v, want nontrivial %s/%s", alarms[0], wantKind, requestBefore.Status)
						}
						if !found || reason != wantReason || status != "pending" {
							t.Errorf("job = %t/%s/%s, want true/%s/pending", found, reason, status, wantReason)
						}
						after, err := f.store.GetRequest(ctx, requestID)
						if err != nil || !strings.Contains(after.LastError, wantReason) {
							t.Errorf("anomaly detail = %+v, %v", after, err)
						}
						var detail struct {
							Outcome string `json:"outcome"`
							Status  string `json:"status"`
						}
						var raw []byte
						if err := f.db.Get(&raw, `SELECT detail FROM inference_reconciliation_jobs WHERE request_id=$1`, requestID); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(raw, &detail); err != nil || detail.Outcome != string(wantKind) ||
							detail.Status != string(requestBefore.Status) {
							t.Errorf("job detail = %s, %v", raw, err)
						}
						evidence := loadTerminalEvidence(t, f, requestID, 1)
						for _, observation := range evidence {
							wantCharge := int64(20)
							if zeroPrice {
								wantCharge = 0
							}
							if observation.RequestID != requestID || observation.AttemptID != incomingAttemptID ||
								observation.Source != domain.UsageReported || observation.RawUsageSchemaVersion != 1 ||
								observation.Price.ID != f.priceID || observation.Price.ModelID != f.modelID ||
								observation.Price.Kind != "sale_credit" || observation.Price.Revision != 1 ||
								observation.Charge.Source != "plan" || observation.Charge.Unit != "microcredit" ||
								observation.Charge.Currency != "" || observation.Charge.AmountMicros != wantCharge {
								t.Errorf("incoming evidence = %+v", observation)
							}
							assertTerminalBucket(t, observation.Buckets, "input_tokens", int64Ptr(10))
							assertTerminalBucket(t, observation.Buckets, "output_tokens", int64Ptr(5))
							var rawUsage struct {
								Usage struct {
									Input  int64 `json:"prompt_tokens"`
									Output int64 `json:"completion_tokens"`
								} `json:"usage"`
							}
							if err := json.Unmarshal(observation.RawUsage, &rawUsage); err != nil ||
								rawUsage.Usage.Input != 10 || rawUsage.Usage.Output != 5 {
								t.Errorf("incoming raw usage = %s, %v", observation.RawUsage, err)
							}
						}
					}
					if got := heldLeases(t, f); got != 0 {
						t.Errorf("held leases = %d, want 0", got)
					}
				})
			}
		})
	}
}

// Attempt finalization and lease cleanup are expected; terminal accounting is not.
func terminalFinalizerSnapshot(t *testing.T, f *fixture, requestID string) string {
	t.Helper()
	var snapshot string
	err := f.db.Get(&snapshot, `
		SELECT jsonb_build_object(
		    'request', to_jsonb(r) - 'last_error' - 'updated_at',
		    'usage', (SELECT jsonb_agg(to_jsonb(u) ORDER BY u.id)
		        FROM inference_usage_records u WHERE u.request_id=r.id),
		    'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id)
		        FROM inference_ledger_entries l WHERE l.request_id=r.id),
		    'rate', (SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id)
		        FROM inference_rate_reservations s WHERE s.request_id=r.id),
		    'reservations', (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id)
		        FROM inference_reservations h WHERE h.request_id=r.id),
		    'windows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id)
		        FROM inference_quota_windows w
		        WHERE w.id IN (r.window_five_hour_id, r.window_weekly_id, r.window_monthly_id)),
		    'wallets', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id)
		        FROM inference_wallets w WHERE w.billing_account_id=r.billing_account_id),
		    'wallet_holds', (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id)
		        FROM inference_wallet_holds h WHERE h.request_id=r.id),
		    'wallet_entries', (SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id)
		        FROM inference_wallet_entries e JOIN inference_wallets w ON w.id=e.wallet_id
		        WHERE w.billing_account_id=r.billing_account_id),
		    'key', (SELECT to_jsonb(k) FROM inference_api_keys k WHERE k.id=r.api_key_id)
		)::text FROM inference_requests r WHERE r.id=$1`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
