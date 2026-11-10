package postgres

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCorrectSettlement_RejectsAnotherRequestsAttempt(t *testing.T) {
	for _, amount := range []domain.Microcredit{40_000, 60_000} {
		name := "money_and_tokens"
		if amount == 60_000 {
			name = "tokens_only"
		}
		t.Run(name, func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
			other := seedFixtureModel(t, s, true, "other-model")
			o5, ow, om := makeWindows(t, s, other.entID)
			otherRequestID, otherAttemptID := correctedFixture(t, s, other, o5, ow, om, 80, 20)
			total, input, output := int64(90), int64(80), int64(10)
			cmd := correctionFixture(requestID, otherAttemptID, amount, &total, "foreign-attempt")
			cmd.Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}

			if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("correction using another account's attempt = %v, want invalid_input", err)
			}
			assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 0)
			for _, original := range []struct {
				requestID string
				attemptID string
				tokens    int64
			}{{requestID, attemptID, 50}, {otherRequestID, otherAttemptID, 100}} {
				usage, err := s.LoadUsageRecords(context.Background(), original.requestID)
				if err != nil || len(usage) != 1 || usage[0].AttemptID != original.attemptID ||
					usage[0].Source != domain.UsageEstimated || usage[0].Revision != 1 {
					t.Errorf("original usage changed: %+v, err=%v", usage, err)
				}
				if tokens, _, _, present := rateTokens(t, s, original.requestID); !present || tokens != original.tokens {
					t.Errorf("request %s rate total = %d (present=%t), want %d",
						original.requestID, tokens, present, original.tokens)
				}
				req, err := s.GetRequest(context.Background(), original.requestID)
				if err != nil {
					t.Fatal(err)
				}
				if req.SettledMicros == nil || *req.SettledMicros != 60_000 ||
					req.UsageStatus != domain.UsageEstimated {
					t.Errorf("request settlement changed: %+v", req)
				}
				if charge, reversal, adjustment := ledgerTotals(t, s, original.requestID); charge != 60_000 || reversal != 0 || adjustment != 0 {
					t.Errorf("ledger = %d/%d/%d, want 60000/0/0", charge, reversal, adjustment)
				}
			}
			for _, windowID := range []string{w5, ww, wm, o5, ow, om} {
				if used, reserved := windowState(t, s, windowID); used != 60_000 || reserved != 0 {
					t.Errorf("window used/reserved = %d/%d, want 60000/0", used, reserved)
				}
			}
			for _, keyID := range []string{f.keyID, other.keyID} {
				if used := keyBudgetUsed(t, s, keyID); used != 60_000 {
					t.Errorf("key used = %d, want 60000", used)
				}
			}
			if t.Failed() {
				return
			}
			cmd.Corrected.AttemptID = attemptID
			if err := applyCorrection(t, s, cmd); err != nil {
				t.Fatalf("correct attempt must remain correctable with the same key: %v", err)
			}
		})
	}
}
