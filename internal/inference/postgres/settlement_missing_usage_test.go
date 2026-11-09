package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestSettle_MissingUsageCannotBeDuplicate(t *testing.T) {
	for _, source := range []string{"plan", "wallet"} {
		t.Run(source, func(t *testing.T) {
			for _, charge := range []int64{0, 60_000} {
				name := "positive"
				if charge == 0 {
					name = "zero"
				}
				t.Run(name, func(t *testing.T) {
					for _, missing := range []string{"usage", "revision", "same_request_attempt"} {
						t.Run(missing, func(t *testing.T) {
							_, s := testDB(t)
							ctx := context.Background()
							var requestID, attemptID string
							if source == "wallet" {
								requestID, attemptID, _ = walletAdmission(t, s)
							} else {
								f := seedFixture(t, s, true)
								w5, ww, wm := makeWindows(t, s, f.entID)
								var err error
								requestID, err = admitWithRate(t, s, f, w5, ww, wm,
									domain.RateLimits{PolicyTPM: 100_000}, 1000)
								if err != nil {
									t.Fatal(err)
								}
								attemptID = uuid.NewString()
								if err := s.InsertAttempt(ctx, &domain.Attempt{
									ID: attemptID, RequestID: requestID, AttemptNo: 1,
								}); err != nil {
									t.Fatal(err)
								}
							}
							cmd := walletSettleCmd(t, requestID, attemptID, charge)
							if source == "plan" {
								cmd.WalletCharge = nil
							}
							uow, err := s.Begin(ctx)
							if err != nil {
								t.Fatal(err)
							}
							defer uow.Rollback(ctx)
							if err := s.Settle(ctx, uow, cmd); err != nil {
								t.Fatal(err)
							}
							if err := uow.Commit(ctx); err != nil {
								t.Fatal(err)
							}

							otherAttemptID := uuid.NewString()
							if err := s.InsertAttempt(ctx, &domain.Attempt{
								ID: otherAttemptID, RequestID: requestID, AttemptNo: 2,
							}); err != nil {
								t.Fatal(err)
							}
							replay, err := s.Begin(ctx)
							if err != nil {
								t.Fatal(err)
							}
							defer replay.Rollback(ctx)
							tx := mustTx(t, replay)
							before := settledAccountingSnapshot(t, tx, requestID)
							for _, revision := range []int{0, 1} {
								cmd.Usage.Revision = revision
								err := s.Settle(ctx, replay, cmd)
								outcome, ok := domain.AsSettleError(err)
								if !ok || outcome.Kind != domain.SettleOutcomeDuplicate {
									t.Fatalf("original replay revision %d = %v, want duplicate", revision, err)
								}
							}
							if after := settledAccountingSnapshot(t, tx, requestID); after != before {
								t.Fatal("original duplicate rewrote settled accounting")
							}

							switch missing {
							case "usage":
								if _, err := tx.ExecContext(ctx,
									`DELETE FROM inference_usage_records WHERE request_id=$1`, requestID); err != nil {
									t.Fatal(err)
								}
							case "revision":
								cmd.Usage.Revision = 2
							case "same_request_attempt":
								cmd.Usage.AttemptID = otherAttemptID
							}
							before = settledAccountingSnapshot(t, tx, requestID)
							for _, input := range []int64{1000, 2000} {
								cmd.Usage.Buckets.InputTokens = &input
								err := s.Settle(ctx, replay, cmd)
								outcome, ok := domain.AsSettleError(err)
								if !ok || outcome.Kind != domain.SettleOutcomeChargeMismatch {
									t.Errorf("missing %s, input %d = %v, want charge_mismatch", missing, input, err)
								} else if outcome.ExistingChargeMicros == nil ||
									*outcome.ExistingChargeMicros != domain.Microcredit(charge) ||
									outcome.RequestedChargeMicros != domain.Microcredit(charge) ||
									outcome.Status != domain.ReqSettled {
									t.Errorf("mismatch lost the unchanged charge/terminal facts: %+v", outcome)
								}
								if after := settledAccountingSnapshot(t, tx, requestID); after != before {
									t.Error("missing usage replay changed settled accounting before refusal")
								}
							}
						})
					}
				})
			}
		})
	}
}

// Compare persisted rows inside the open transaction so rollback cannot hide writes.
func settledAccountingSnapshot(t *testing.T, tx *sqlx.Tx, requestID string) string {
	t.Helper()
	var snapshot string
	err := tx.QueryRowxContext(context.Background(), `
		SELECT jsonb_build_object(
		    'request', to_jsonb(r),
		    'usage', (SELECT jsonb_agg(to_jsonb(u) ORDER BY u.id)
		        FROM inference_usage_records u WHERE u.request_id=r.id),
		    'attempts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)
		        FROM inference_attempts a WHERE a.request_id=r.id),
		    'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id)
		        FROM inference_ledger_entries l WHERE l.request_id=r.id),
		    'rate', (SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id)
		        FROM inference_rate_reservations s WHERE s.request_id=r.id),
		    'reservations', (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id)
		        FROM inference_reservations h WHERE h.request_id=r.id),
		    'windows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id)
		        FROM inference_quota_windows w
		        WHERE w.id IN (r.window_five_hour_id, r.window_weekly_id, r.window_monthly_id)),
		    'key', (SELECT to_jsonb(k) FROM inference_api_keys k WHERE k.id=r.api_key_id),
		    'wallet', (SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id)
		        FROM inference_wallets w WHERE w.billing_account_id=r.billing_account_id),
		    'wallet_holds', (SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id)
		        FROM inference_wallet_holds h WHERE h.request_id=r.id),
		    'wallet_entries', (SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id)
		        FROM inference_wallet_entries e WHERE e.request_id=r.id)
		)::text FROM inference_requests r WHERE r.id=$1`, requestID).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
