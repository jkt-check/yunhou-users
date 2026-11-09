package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCorrectSettlement_RejectsOverflowingTokenEvidence(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
	total, large, output := int64(1), int64(math.MaxInt64), int64(3)
	cmd := correctionFixture(requestID, attemptID, 40_000, &total, "overflow-total")
	// These nonnegative buckets wrap to 1 with unchecked int64 addition.
	cmd.Corrected.Buckets = domain.UsageBuckets{
		InputTokens: &large, CacheReadTokens: &large, OutputTokens: &output,
	}
	if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("overflowing normalized evidence = %v, want invalid_input", err)
	}
	assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 0)
	if tokens, _, _, _ := rateTokens(t, s, requestID); tokens != 50 {
		t.Errorf("rejected overflow rate total = %d, want 50", tokens)
	}
}

func TestCorrectSettlement_RejectsTokenTotalDisagreeingWithEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		original  domain.Microcredit
		corrected domain.Microcredit
	}{
		{"money_and_tokens", 60_000, 40_000},
		{"free_model", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, tc.original)
			// Normalized buckets total 90. Reasoning is already included in output.
			input, read, write, output, reasoning := int64(40), int64(20), int64(10), int64(20), int64(15)
			total := int64(0)
			cmd := correctionFixture(requestID, attemptID, tc.corrected, &total, "inconsistent-total")
			cmd.Corrected.Buckets = domain.UsageBuckets{
				InputTokens: &input, CacheReadTokens: &read, CacheWriteTokens: &write,
				OutputTokens: &output, ReasoningTokens: &reasoning,
			}
			if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("90-token evidence with a zero total = %v, want invalid_input", err)
			}
			assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 0)
			usage, err := s.LoadUsageRecords(context.Background(), requestID)
			if err != nil || len(usage) != 1 || usage[0].Source != domain.UsageEstimated {
				t.Errorf("rejected correction changed usage: %+v, err=%v", usage, err)
			}
			if tokens, _, _, present := rateTokens(t, s, requestID); !present || tokens != 50 {
				t.Errorf("rejected correction rate total = %d (present=%t), want 50", tokens, present)
			}
			if charge, reversal, adjustment := ledgerTotals(t, s, requestID); charge != int64(tc.original) || reversal != 0 || adjustment != 0 {
				t.Errorf("ledger = %d/%d/%d, want %d/0/0", charge, reversal, adjustment, tc.original)
			}
			for _, windowID := range []string{w5, ww, wm} {
				if used, reserved := windowState(t, s, windowID); used != tc.original || reserved != 0 {
					t.Errorf("window used/reserved = %d/%d, want %d/0", used, reserved, tc.original)
				}
			}
			if used := keyBudgetUsed(t, s, f.keyID); used != tc.original {
				t.Errorf("key used = %d, want %d", used, tc.original)
			}
			if t.Failed() {
				return
			}
			total = 90
			if err := applyCorrection(t, s, cmd); err != nil {
				t.Fatalf("matching total must succeed with the same key: %v", err)
			}
			if tokens, _, _, _ := rateTokens(t, s, requestID); tokens != 90 {
				t.Errorf("corrected tokens = %d, want 90 without double-counting reasoning", tokens)
			}
			if _, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 100}, 20); domain.CodeOf(err) != domain.CodeRateLimited {
				t.Errorf("corrected 90 + new 20 = %v, want rate_limited", err)
			}
		})
	}
}
