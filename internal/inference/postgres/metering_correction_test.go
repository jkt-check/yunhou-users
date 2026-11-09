package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCorrectSettlement_EqualMoneyAcceptsChangedEvidence(t *testing.T) {
	for _, change := range []string{"source_only", "buckets_only", "raw_only", "unknown_to_zero"} {
		t.Run(change, func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
			total, input, output, zero := int64(50), int64(40), int64(10), int64(0)
			cmd := correctionFixture(requestID, attemptID, 60_000, &total, "evidence-"+change)
			cmd.Corrected.Source = domain.UsageEstimated
			cmd.Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}
			switch change {
			case "source_only":
				cmd.Corrected.Source = domain.UsageReported
			case "buckets_only":
				input, output = 30, 20
			case "raw_only":
				cmd.Corrected.RawUsage.Raw = json.RawMessage(`{"schema_version":1,"provider_report":"confirmed"}`)
			case "unknown_to_zero":
				cmd.Corrected.Buckets.CacheReadTokens = &zero
			}
			if err := applyCorrection(t, s, cmd); err != nil {
				t.Fatalf("changed %s at equal money: %v", change, err)
			}
			usage, err := s.LoadUsageRecords(context.Background(), requestID)
			if err != nil {
				t.Fatal(err)
			}
			if len(usage) != 2 || usage[1].Source != cmd.Corrected.Source ||
				usage[1].Buckets.InputTokens == nil || *usage[1].Buckets.InputTokens != input ||
				usage[1].Buckets.OutputTokens == nil || *usage[1].Buckets.OutputTokens != output {
				t.Fatalf("corrected usage = %+v, want changed %s evidence", usage, change)
			}
			if change == "unknown_to_zero" && (usage[0].Buckets.CacheReadTokens != nil ||
				usage[1].Buckets.CacheReadTokens == nil || *usage[1].Buckets.CacheReadTokens != 0) {
				t.Error("unknown and reported zero must remain distinct")
			}
			if change == "raw_only" && string(usage[1].RawUsage.Raw) == string(usage[0].RawUsage.Raw) {
				t.Error("new raw evidence was not persisted")
			}
			// A different key cannot make identical evidence into another correction.
			cmd.IdempotencyKey += "-noop"
			if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("unchanged money/evidence: err = %v, want invalid_input", err)
			}
		})
	}
}

func TestCorrectSettlement_MeteringOnlyFailureRollsBackReceiptAndUsage(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
	total := int64(90)
	cmd := correctionFixture(requestID, attemptID, 60_000, &total, "metering-rollback")
	input, output := int64(80), int64(10)
	cmd.Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET state='released' WHERE request_id=$1 AND scope='account'`,
		requestID); err != nil {
		t.Fatal(err)
	}
	if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("malformed rate row: err = %v, want conflict", err)
	}
	assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 0)
	usage, err := s.LoadUsageRecords(context.Background(), requestID)
	if err != nil || len(usage) != 1 {
		t.Fatalf("failed correction usage = %+v, err = %v; want original only", usage, err)
	}
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET state='settled' WHERE request_id=$1 AND scope='account'`,
		requestID); err != nil {
		t.Fatal(err)
	}
	if err := applyCorrection(t, s, cmd); err != nil {
		t.Fatalf("retry after repair must succeed with same key: %v", err)
	}
	assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 1)
	var reason, operator, usageID string
	var correctedTokens, correctedMicros int64
	if err := s.db.QueryRow(
		`SELECT reason, operator_subject, usage_record_id, corrected_tokens, corrected_micros
		 FROM inference_usage_corrections WHERE idempotency_key=$1`, cmd.IdempotencyKey).
		Scan(&reason, &operator, &usageID, &correctedTokens, &correctedMicros); err != nil {
		t.Fatal(err)
	}
	usage, err = s.LoadUsageRecords(context.Background(), requestID)
	if err != nil || len(usage) != 2 {
		t.Fatalf("usage = %+v, err = %v", usage, err)
	}
	if reason != cmd.Reason || operator != cmd.OperatorSubject || usageID != usage[1].ID ||
		correctedTokens != 90 || correctedMicros != 60_000 {
		t.Errorf("correction audit = %q/%q/%q/%d/%d", reason, operator, usageID, correctedTokens, correctedMicros)
	}
}

func TestCorrectSettlement_ConcurrentMoneyAndMeteringShareIdempotency(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	total, input, output := int64(90), int64(80), int64(10)
	commands := make([]CorrectionCommand, 2)
	for i, amount := range []domain.Microcredit{40_000, 60_000} {
		requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
		commands[i] = correctionFixture(requestID, attemptID, amount, &total, "mixed-shared-key")
		commands[i].Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}
	}
	type result struct {
		index int
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i, cmd := range commands {
		go func(index int, cmd CorrectionCommand) {
			<-start
			ctx := context.Background()
			uow, err := s.Begin(ctx)
			if err == nil {
				err = s.CorrectSettlement(ctx, uow, cmd)
				if err == nil {
					err = uow.Commit(ctx)
				} else {
					_ = uow.Rollback(ctx)
				}
			}
			results <- result{index, err}
		}(i, cmd)
	}
	close(start)
	successes, conflicts := 0, 0
	for range commands {
		result := <-results
		wantTokens, wantRevisions := int64(50), 1
		switch domain.CodeOf(result.err) {
		case "":
			successes++
			wantTokens, wantRevisions = 90, 2
		case domain.CodeConflict:
			conflicts++
		default:
			t.Errorf("correction %d: unexpected err = %v", result.index, result.err)
		}
		cmd := commands[result.index]
		tokens, _, _, _ := rateTokens(t, s, cmd.RequestID)
		usage, err := s.LoadUsageRecords(context.Background(), cmd.RequestID)
		if err != nil || tokens != wantTokens || len(usage) != wantRevisions {
			t.Errorf("correction %d: tokens/revisions = %d/%d, err=%v; want %d/%d",
				result.index, tokens, len(usage), err, wantTokens, wantRevisions)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Errorf("success/conflict = %d/%d, want 1/1", successes, conflicts)
	}
	assertCorrectionReceiptCount(t, s, "mixed-shared-key", 1)
}

func TestCorrectSettlement_ConcurrentMeteringReplayReturnsConflict(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 40, 10)
	total := int64(1000)
	cmd := correctionFixture(requestID, attemptID, 60_000, &total, "concurrent-metering-replay")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := mustTx(t, blocker).ExecContext(ctx,
		`SELECT id FROM inference_requests WHERE id=$1 FOR UPDATE`, requestID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var pids [2]int
	for i := range pids {
		uow, err := s.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer uow.Rollback(context.Background())
		if err := mustTx(t, uow).GetContext(ctx, &pids[i], `SELECT pg_backend_pid()`); err != nil {
			t.Fatal(err)
		}
		go func(uow domain.UnitOfWork) {
			err := s.CorrectSettlement(ctx, uow, cmd)
			if err == nil {
				err = uow.Commit(ctx)
			} else {
				_ = uow.Rollback(context.Background())
			}
			results <- err
		}(uow)
	}
	// Both calls must pass any pre-lock checks before releasing the request.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked int
		if err := s.db.GetContext(ctx, &blocked,
			`SELECT COUNT(*) FROM pg_stat_activity WHERE pid IN ($1,$2) AND wait_event_type='Lock'`,
			pids[0], pids[1]); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("corrections did not both wait on the locked request")
		case <-ticker.C:
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	successes, conflicts := 0, 0
	for range pids {
		err := <-results
		switch {
		case err == nil:
			successes++
		case domain.CodeOf(err) == domain.CodeConflict:
			conflicts++
		default:
			t.Errorf("concurrent replay = %v, want success or conflict", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Errorf("success/conflict = %d/%d, want 1/1", successes, conflicts)
	}
	assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 1)
}

func assertCorrectionReceiptCount(t *testing.T, s *Store, key string, want int) {
	t.Helper()
	var count int
	if err := s.db.Get(&count, `SELECT COUNT(*) FROM inference_usage_corrections WHERE idempotency_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Errorf("correction receipts for %q = %d, want %d", key, count, want)
	}
}
