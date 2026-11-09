package postgres

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCorrectSettlement_RejectsInvalidEvidenceBeforeWriting(t *testing.T) {
	input, output, negative := int64(80), int64(10), int64(-1)
	empty, malformed := "", "not-a-uuid"
	for _, tc := range []struct {
		name      string
		attemptID *string
		buckets   *domain.UsageBuckets
		total     *int64
		unknown   bool
	}{
		{name: "empty_attempt", attemptID: &empty},
		{name: "malformed_attempt", attemptID: &malformed},
		{name: "negative_reasoning_detail", buckets: &domain.UsageBuckets{
			InputTokens: &input, OutputTokens: &output, ReasoningTokens: &negative,
		}},
		{name: "negative_reasoning_only", unknown: true, buckets: &domain.UsageBuckets{
			ReasoningTokens: &negative,
		}},
		{name: "negative_unknown_total", total: &negative, buckets: &domain.UsageBuckets{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testDB(t)
			ctx := context.Background()
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
			_, beforeAt, _, _ := rateTokens(t, s, requestID)
			total := int64(90)
			valid := correctionFixture(requestID, attemptID, 40_000, &total, "invalid-evidence")
			valid.Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}
			invalid := valid
			if tc.attemptID != nil {
				invalid.Corrected.AttemptID = *tc.attemptID
			}
			if tc.buckets != nil {
				invalid.Corrected.Buckets = *tc.buckets
			}
			if tc.total != nil {
				invalid.CorrectedTokens = tc.total
			}
			if tc.unknown {
				invalid.CorrectedTokens = nil
			}

			uow, err := s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(ctx)
			if err := s.CorrectSettlement(ctx, uow, invalid); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("invalid correction = %v, want invalid_input", err)
			}
			assertUnchangedCorrectionState(t, mustTx(t, uow), requestID)
			if t.Failed() {
				return
			}
			if err := s.CorrectSettlement(ctx, uow, valid); err != nil {
				t.Fatalf("valid retry on the same transaction and key: %v", err)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			usage, err := s.LoadUsageRecords(ctx, requestID)
			if err != nil || len(usage) != 2 || usage[0].Source != domain.UsageEstimated ||
				usage[1].Source != domain.UsageReported || usage[1].Revision != 2 ||
				usage[1].Buckets.InputTokens == nil || *usage[1].Buckets.InputTokens != 80 {
				t.Fatalf("corrected usage = %+v, err=%v", usage, err)
			}
			tokens, afterAt, state, present := rateTokens(t, s, requestID)
			if !present || state != "settled" || tokens != 90 ||
				beforeAt == nil || afterAt == nil || !beforeAt.Equal(*afterAt) {
				t.Errorf("corrected TPM = %t/%s/%d/%v, want true/settled/90/%v",
					present, state, tokens, afterAt, beforeAt)
			}
			assertCorrectionReceiptCount(t, s, valid.IdempotencyKey, 1)
			if charge, reversal, adjustment := ledgerTotals(t, s, requestID); charge != 60_000 || reversal != 0 || adjustment != 20_000 {
				t.Errorf("corrected ledger = %d/%d/%d, want 60000/0/20000", charge, reversal, adjustment)
			}
			for _, windowID := range []string{w5, ww, wm} {
				if used, reserved := windowState(t, s, windowID); used != 40_000 || reserved != 0 {
					t.Errorf("corrected window used/reserved = %d/%d, want 40000/0", used, reserved)
				}
			}
			if used := keyBudgetUsed(t, s, f.keyID); used != 40_000 {
				t.Errorf("corrected key budget = %d, want 40000", used)
			}
		})
	}
}

// Inspect before rollback: a database constraint error or late validation
// must not hide an aborted transaction or a partially appended correction.
func assertUnchangedCorrectionState(t *testing.T, tx *sqlx.Tx, requestID string) {
	t.Helper()
	var unchanged bool
	err := tx.QueryRowxContext(context.Background(),
		`SELECT r.status='settled' AND r.settled_micros=60000 AND r.usage_status='estimated'
		        AND (SELECT COUNT(*) FROM inference_usage_records WHERE request_id=$1)=1
		        AND EXISTS (SELECT 1 FROM inference_usage_records
		                    WHERE request_id=$1 AND source='estimated' AND revision=1
		                      AND input_tokens=40 AND output_tokens=10)
		        AND NOT EXISTS (SELECT 1 FROM inference_usage_corrections WHERE request_id=$1)
		        AND NOT EXISTS (SELECT 1 FROM inference_adjustments WHERE request_id=$1)
		        AND (SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id=$1)=1
		        AND EXISTS (SELECT 1 FROM inference_ledger_entries
		                    WHERE request_id=$1 AND entry_type='charge' AND amount_micros=60000)
		        AND EXISTS (SELECT 1 FROM inference_rate_reservations
		                    WHERE request_id=$1 AND scope='account' AND state='settled'
		                      AND settled_tokens=50)
		        AND NOT EXISTS (
		            SELECT 1 FROM inference_reservations h
		            JOIN inference_quota_windows w ON w.id=h.window_id
		            WHERE h.request_id=$1 AND (w.used_micros<>60000 OR w.reserved_micros<>0))
		        AND EXISTS (SELECT 1 FROM inference_api_keys
		                    WHERE id=r.api_key_id AND budget_used_micros=60000)
		 FROM inference_requests r WHERE r.id=$1`, requestID).Scan(&unchanged)
	if err != nil {
		t.Fatalf("rejected correction must leave its transaction usable: %v", err)
	}
	if !unchanged {
		t.Error("rejected correction changed usage, receipts, ledger, TPM or monetary aggregates before rollback")
	}
}
