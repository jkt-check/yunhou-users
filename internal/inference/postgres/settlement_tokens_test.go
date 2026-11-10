package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestSettle_RejectsInvalidTokenTotalsBeforeWriting(t *testing.T) {
	max, three, negative, two := int64(math.MaxInt64), int64(3), int64(-1), int64(2)
	for _, source := range []string{"plan", "wallet"} {
		t.Run(source, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				buckets domain.UsageBuckets
			}{
				{"positive_wrapped_total", domain.UsageBuckets{InputTokens: &max, CacheReadTokens: &max, OutputTokens: &three}},
				{"negative_bucket", domain.UsageBuckets{InputTokens: &negative, OutputTokens: &two}},
				{"negative_reasoning_detail", domain.UsageBuckets{OutputTokens: &two, ReasoningTokens: &negative}},
			} {
				t.Run(tc.name, func(t *testing.T) {
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
					// A zero-priced execution can carry huge token evidence without
					// overflowing money. Settlement must validate tokens independently.
					valid := walletSettleCmd(t, requestID, attemptID, 0)
					if source == "plan" {
						valid.WalletCharge = nil
					}
					invalid := valid
					invalid.Usage.Buckets = tc.buckets
					uow, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer uow.Rollback(ctx)
					if err := s.Settle(ctx, uow, invalid); domain.CodeOf(err) != domain.CodeInvalidInput {
						t.Errorf("invalid token settlement = %v, want invalid_input", err)
					}
					assertUnsettledIdentityState(t, mustTx(t, uow), requestID)
					if source == "wallet" {
						assertWalletHoldUnchanged(t, mustTx(t, uow), requestID, 2_000_000)
					}
					if t.Failed() {
						return
					}
					// An exact-Max total is valid; reasoning is already in output.
					valid.Usage.Buckets = domain.UsageBuckets{OutputTokens: &max, ReasoningTokens: &three}
					if err := s.Settle(ctx, uow, valid); err != nil {
						t.Fatalf("valid retry on the same transaction: %v", err)
					}
					if err := uow.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					tokens, _, state, present := rateTokens(t, s, requestID)
					if !present || state != "settled" || tokens != math.MaxInt64 {
						t.Errorf("settled TPM = %t/%s/%d, want true/settled/MaxInt64", present, state, tokens)
					}
					if charge, reversal, adjustment := ledgerTotals(t, s, requestID); charge != 0 || reversal != 0 || adjustment != 0 {
						t.Errorf("zero-priced settlement = %d/%d/%d", charge, reversal, adjustment)
					}
					replay, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer replay.Rollback(ctx)
					if err := s.Settle(ctx, replay, invalid); domain.CodeOf(err) != domain.CodeInvalidInput {
						t.Errorf("invalid duplicate = %v, want invalid_input", err)
					}
				})
			}
		})
	}
}
