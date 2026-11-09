package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestSettle_RejectsForeignEvidenceBeforeWriting(t *testing.T) {
	for _, source := range []string{"plan", "wallet"} {
		t.Run(source, func(t *testing.T) {
			for _, mismatch := range []string{"usage_request", "usage_attempt", "missing_usage_attempt", "cost_attempt", "missing_cost_attempt"} {
				t.Run(mismatch, func(t *testing.T) {
					_, s := testDB(t)
					ctx := context.Background()
					var requestID, attemptID string
					if source == "wallet" {
						requestID, attemptID, _ = walletAdmission(t, s)
					} else {
						f := seedFixture(t, s, true)
						w5, ww, wm := makeWindows(t, s, f.entID)
						var err error
						requestID, err = admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 100_000}, 1000)
						if err != nil {
							t.Fatal(err)
						}
						attemptID = uuid.NewString()
						if err := s.InsertAttempt(ctx, &domain.Attempt{ID: attemptID, RequestID: requestID, AttemptNo: 1}); err != nil {
							t.Fatal(err)
						}
					}
					other := seedFixtureModel(t, s, true, "other-settlement-model")
					w5, ww, wm := makeWindows(t, s, other.entID)
					otherRequestID, err := admitWithRate(t, s, other, w5, ww, wm, domain.RateLimits{PolicyTPM: 100_000}, 1000)
					if err != nil {
						t.Fatal(err)
					}
					otherAttemptID := uuid.NewString()
					oldCost, _ := domain.NewMoney(777, "USD")
					oldBasis := domain.CostEstimated
					if err := s.InsertAttempt(ctx, &domain.Attempt{
						ID: otherAttemptID, RequestID: otherRequestID, AttemptNo: 1, Cost: &oldCost, Basis: &oldBasis,
					}); err != nil {
						t.Fatal(err)
					}
					valid := walletSettleCmd(t, requestID, attemptID, 60_000)
					if source == "plan" {
						valid.WalletCharge = nil
					}
					cost, _ := domain.NewMoney(13, "USD")
					basis := domain.CostReported
					valid.AttemptID, valid.AttemptCost, valid.CostBasis = &attemptID, &cost, &basis
					invalid := valid
					missingID := uuid.NewString()
					switch mismatch {
					case "usage_request":
						invalid.Usage.RequestID = otherRequestID
						invalid.Usage.AttemptID = otherAttemptID
					case "usage_attempt":
						invalid.Usage.AttemptID = otherAttemptID
					case "missing_usage_attempt":
						invalid.Usage.AttemptID = missingID
					case "cost_attempt":
						invalid.AttemptID = &otherAttemptID
					case "missing_cost_attempt":
						invalid.AttemptID = &missingID
					}

					uow, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer uow.Rollback(ctx)
					if err := s.Settle(ctx, uow, invalid); domain.CodeOf(err) != domain.CodeInvalidInput {
						t.Errorf("mismatched settlement = %v, want invalid_input", err)
					}
					tx := mustTx(t, uow)
					assertUnsettledIdentityState(t, tx, requestID)
					assertUnsettledIdentityState(t, tx, otherRequestID)
					var storedCost int64
					var storedBasis string
					if err := tx.QueryRowxContext(ctx,
						`SELECT cost_micros, cost_basis FROM inference_attempts WHERE id=$1`, otherAttemptID).
						Scan(&storedCost, &storedBasis); err != nil {
						t.Fatal(err)
					}
					if storedCost != 777 || storedBasis != "estimated" {
						t.Errorf("foreign attempt cost = %d/%s, want 777/estimated", storedCost, storedBasis)
					}
					if t.Failed() {
						return
					}
					if err := s.Settle(ctx, uow, valid); err != nil {
						t.Fatalf("valid retry on the same transaction: %v", err)
					}
					if err := uow.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					usage, err := s.LoadUsageRecords(ctx, requestID)
					if err != nil || len(usage) != 1 || usage[0].AttemptID != attemptID {
						t.Fatalf("settled usage = %+v, err=%v", usage, err)
					}
					tokens, _, state, present := rateTokens(t, s, requestID)
					if !present || state != "settled" || tokens != 1500 {
						t.Errorf("settled rate = %t/%s/%d, want true/settled/1500", present, state, tokens)
					}
					req, err := s.GetRequest(ctx, requestID)
					if err != nil || req.Status != domain.ReqSettled || req.SettledMicros == nil || *req.SettledMicros != 60_000 {
						t.Fatalf("settled request = %+v, err=%v", req, err)
					}
					if charge, reversal, adjustment := ledgerTotals(t, s, requestID); charge != 60_000 || reversal != 0 || adjustment != 0 {
						t.Errorf("ledger = %d/%d/%d, want 60000/0/0", charge, reversal, adjustment)
					}
					replay, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer replay.Rollback(ctx)
					if err := s.Settle(ctx, replay, invalid); domain.CodeOf(err) != domain.CodeInvalidInput {
						t.Errorf("foreign duplicate = %v, want invalid_input", err)
					}
					if err := s.Settle(ctx, replay, valid); err == nil {
						t.Error("valid replay must be a typed duplicate")
					} else if outcome, ok := domain.AsSettleError(err); !ok || outcome.Kind != domain.SettleOutcomeDuplicate {
						t.Errorf("valid replay = %v, want typed duplicate", err)
					}
					assertUnsettledIdentityState(t, mustTx(t, replay), otherRequestID)
				})
			}
		})
	}
}

// Inspect the transaction before rollback so a late refusal cannot hide writes.
func assertUnsettledIdentityState(t *testing.T, tx *sqlx.Tx, requestID string) {
	t.Helper()
	assertNoPartialSettlement(t, tx, requestID)
	var unchanged bool
	err := tx.QueryRowxContext(context.Background(),
		`SELECT status='reserved' AND settled_micros IS NULL
		        AND NOT EXISTS (
		            SELECT 1 FROM inference_reservations
		            WHERE request_id=$1 AND state <> 'held')
		        AND NOT EXISTS (
		            SELECT 1 FROM inference_wallet_holds
		            WHERE request_id=$1 AND state <> 'held')
		        AND NOT EXISTS (
		            SELECT 1 FROM inference_reservations r
		            JOIN inference_quota_windows w ON w.id=r.window_id
		            WHERE r.request_id=$1 AND (w.used_micros<>0 OR w.reserved_micros<>r.amount_micros))
		        AND NOT EXISTS (
		            SELECT 1 FROM inference_reservations r
		            JOIN inference_api_keys k ON k.id=r.api_key_id
		            WHERE r.request_id=$1 AND k.budget_used_micros<>r.amount_micros)
		 FROM inference_requests WHERE id=$1`, requestID).Scan(&unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged {
		t.Errorf("request %s or its monetary holds/aggregates changed before refusal", requestID)
	}
}
