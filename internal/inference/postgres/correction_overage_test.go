package postgres

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

type correctedOverageDetail struct {
	AuditMarker       string  `json:"audit_marker"`
	ChargeMicros      int64   `json:"charge_micros"`
	OverMicros        int64   `json:"over_micros"`
	Resolution        *string `json:"resolution"`
	ResolutionHistory []struct {
		Resolution string    `json:"resolution"`
		ResolvedAt time.Time `json:"resolved_at"`
	} `json:"resolution_history"`
}

func correctedOverageJob(t *testing.T, s *Store, requestID string) (ReconciliationJob, correctedOverageDetail) {
	t.Helper()
	jobs, err := s.selectJobs(context.Background(),
		`SELECT id, request_id, reason, status, detail, deadline_at, created_at, resolved_at
		 FROM inference_reconciliation_jobs WHERE request_id=$1`, requestID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("request jobs = %v, %v; want one", jobs, err)
	}
	var detail correctedOverageDetail
	if err := json.Unmarshal(jobs[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	return jobs[0], detail
}

func correctionAccountingSnapshot(t *testing.T, tx *sqlx.Tx, requestID string) string {
	t.Helper()
	accounting := settledAccountingSnapshot(t, tx, requestID)
	var snapshot string
	if err := tx.QueryRowx(`
		SELECT jsonb_build_object(
		    'accounting', $2::jsonb,
		    'receipts', (SELECT jsonb_agg(to_jsonb(c) ORDER BY c.idempotency_key)
		        FROM inference_usage_corrections c WHERE c.request_id=$1),
		    'adjustments', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)
		        FROM inference_adjustments a WHERE a.request_id=$1),
		    'jobs', (SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id)
		        FROM inference_reconciliation_jobs j WHERE j.request_id=$1)
		)::text`, requestID, accounting).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertCorrectedOverageAccounting(t *testing.T, s *Store, requestID, keyID string,
	windows []string, amount domain.Microcredit, tokens int64, originalAt *time.Time, corrections int) {
	t.Helper()
	req, err := s.GetRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqSettled || req.SettledMicros == nil || *req.SettledMicros != amount ||
		req.UsageStatus != domain.UsageReported {
		t.Errorf("corrected request = %+v, want settled/reported/%d", req, amount)
	}
	usage, err := s.LoadUsageRecords(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != corrections+1 || usage[0].Source != domain.UsageEstimated {
		t.Errorf("usage = %+v, want estimate plus %d corrections", usage, corrections)
	}
	var receipts int
	if err := s.db.Get(&receipts, `SELECT COUNT(*) FROM inference_usage_corrections WHERE request_id=$1`, requestID); err != nil {
		t.Fatal(err)
	}
	if receipts != corrections {
		t.Errorf("receipts = %d, want %d", receipts, corrections)
	}
	gotTokens, settledAt, state, present := rateTokens(t, s, requestID)
	if !present || state != "settled" || gotTokens != tokens ||
		settledAt == nil || originalAt == nil || !settledAt.Equal(*originalAt) {
		t.Errorf("rate = %d/%s/%v, want %d/settled/%v", gotTokens, state, settledAt, tokens, originalAt)
	}
	for _, windowID := range windows {
		if used, _ := windowState(t, s, windowID); used != amount {
			t.Errorf("window used = %d, want %d", used, amount)
		}
	}
	if used := keyBudgetUsed(t, s, keyID); used != amount {
		t.Errorf("key budget = %d, want %d", used, amount)
	}
}

func TestCorrectSettlement_RecurringOverage(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	requestID, attemptID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, 60_000)
	_, originalAt, _, _ := rateTokens(t, s, requestID)
	at := time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC)
	correct := func(amount domain.Microcredit, tokens int64, key string, when time.Time) CorrectionCommand {
		t.Helper()
		cmd := correctionFixture(requestID, attemptID, amount, &tokens, key)
		cmd.Reason, cmd.At = key, when
		if err := applyCorrection(t, s, cmd); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return cmd
	}
	escalate := func(when time.Time, wantID string) {
		t.Helper()
		ids, err := s.EscalateOverdueReconciliationJobs(ctx, when, 10)
		if err != nil || len(ids) != 1 || ids[0] != wantID {
			t.Fatalf("escalate = %v, %v; want [%s]", ids, err, wantID)
		}
	}

	// The first deadline is later than the recurrence's: GREATEST must not
	// carry an old closed incident's deadline into the new incident.
	correct(120_000, 120, "first-overage", at.Add(48*time.Hour))
	initial, _ := correctedOverageJob(t, s, requestID)
	escalate(at.Add(72*time.Hour), initial.ID)
	correct(90_000, 90, "first-closure", at)
	closed, closedDetail := correctedOverageJob(t, s, requestID)
	if closed.Status != "resolved" || closed.ResolvedAt == nil || closedDetail.Resolution == nil {
		t.Fatalf("first closure = %+v / %+v", closed, closedDetail)
	}
	if _, err := db.Exec(`UPDATE inference_reconciliation_jobs
		SET detail=detail || '{"audit_marker":"original finding"}'::jsonb WHERE id=$1`, closed.ID); err != nil {
		t.Fatal(err)
	}

	// Ordinary same-reason redelivery is not a new correction.
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(ctx)
	if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
		RequestID: &requestID, Reason: ReasonSettlementOverage, Deadline: initial.DeadlineAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	duplicate, duplicateDetail := correctedOverageJob(t, s, requestID)
	if duplicate.Status != "resolved" || duplicate.ResolvedAt == nil ||
		!duplicate.ResolvedAt.Equal(*closed.ResolvedAt) || len(duplicateDetail.ResolutionHistory) != 0 {
		t.Fatalf("generic duplicate reopened the closed incident: %+v / %+v", duplicate, duplicateDetail)
	}

	recurrence := correct(130_000, 130, "recurring-overage", at)
	reopened, detail := correctedOverageJob(t, s, requestID)
	if reopened.ID != initial.ID || reopened.Reason != ReasonSettlementOverage || reopened.Status != "pending" ||
		reopened.ResolvedAt != nil || !reopened.DeadlineAt.Equal(at.Add(24*time.Hour)) {
		t.Errorf("recurring job = %+v, want original pending job with fresh exact deadline", reopened)
	}
	if detail.Resolution != nil || detail.AuditMarker != "original finding" ||
		detail.ChargeMicros != 130_000 || detail.OverMicros != 30_000 {
		t.Errorf("recurring detail = %s", reopened.Detail)
	}
	if len(detail.ResolutionHistory) != 1 {
		t.Errorf("closure history = %+v, want one archived closure", detail.ResolutionHistory)
	} else if detail.ResolutionHistory[0].Resolution != *closedDetail.Resolution ||
		!detail.ResolutionHistory[0].ResolvedAt.Equal(*closed.ResolvedAt) {
		t.Errorf("archived closure lost its note/time: %+v", detail.ResolutionHistory)
	}
	assertCorrectedOverageAccounting(t, s, requestID, f.keyID, []string{w5, ww, wm}, 130_000, 130, originalAt, 3)
	pending, err := s.ListPendingReconciliationJobs(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != initial.ID {
		t.Errorf("pending = %v, %v; want recurring job %s", pending, err, initial.ID)
	}

	replay, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Rollback(ctx)
	before := correctionAccountingSnapshot(t, mustTx(t, replay), requestID)
	if err := s.CorrectSettlement(ctx, replay, recurrence); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("replay = %v, want conflict", err)
	}
	if after := correctionAccountingSnapshot(t, mustTx(t, replay), requestID); after != before {
		t.Error("replayed correction changed accounting or job history before rollback")
	}
	if err := replay.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.EscalateOverdueReconciliationJobs(ctx, at.Add(24*time.Hour-time.Microsecond), 10); err != nil || len(ids) != 0 {
		t.Errorf("early escalation = %v, %v; want none", ids, err)
	}
	escalate(at.Add(24*time.Hour), initial.ID)
	correct(140_000, 140, "continuing-overage", at)
	continuing, continuingDetail := correctedOverageJob(t, s, requestID)
	if continuing.ID != initial.ID || continuing.Status != "escalated" ||
		!reflect.DeepEqual(continuingDetail.ResolutionHistory, detail.ResolutionHistory) ||
		continuingDetail.ChargeMicros != 140_000 || continuingDetail.OverMicros != 40_000 {
		t.Errorf("ongoing escalated incident reset or lost current detail: %+v / %s", continuing, continuing.Detail)
	}
	correct(95_000, 95, "second-closure", at)
	secondClosed, secondDetail := correctedOverageJob(t, s, requestID)
	if secondClosed.ResolvedAt == nil || secondDetail.Resolution == nil {
		t.Fatalf("second closure = %+v / %s", secondClosed, secondClosed.Detail)
	}
	correct(150_000, 150, "second-recurrence", at.Add(time.Hour))
	second, secondReopenedDetail := correctedOverageJob(t, s, requestID)
	if second.ID != initial.ID || second.Status != "pending" || second.ResolvedAt != nil ||
		!second.DeadlineAt.Equal(at.Add(25*time.Hour)) || secondReopenedDetail.Resolution != nil ||
		secondReopenedDetail.AuditMarker != "original finding" ||
		secondReopenedDetail.ChargeMicros != 150_000 || secondReopenedDetail.OverMicros != 50_000 {
		t.Errorf("second recurrence = %+v / %s", second, second.Detail)
	}
	if len(secondReopenedDetail.ResolutionHistory) != 2 {
		t.Errorf("second closure history = %+v, want two ordered closures", secondReopenedDetail.ResolutionHistory)
	} else {
		first, last := secondReopenedDetail.ResolutionHistory[0], secondReopenedDetail.ResolutionHistory[1]
		if first.Resolution != *closedDetail.Resolution || !first.ResolvedAt.Equal(*closed.ResolvedAt) ||
			last.Resolution != *secondDetail.Resolution || !last.ResolvedAt.Equal(*secondClosed.ResolvedAt) {
			t.Errorf("closure history lost note/time/order: %+v", secondReopenedDetail.ResolutionHistory)
		}
	}
	assertCorrectedOverageAccounting(t, s, requestID, f.keyID, []string{w5, ww, wm}, 150_000, 150, originalAt, 6)
}

func TestCorrectSettlement_OveragePreservesManualResolution(t *testing.T) {
	for _, reason := range []string{
		"missing_reservation", "late_usage_after_release", "settlement_mismatch",
		"ledger_drift", "reservation_drift", "insufficient_evidence",
	} {
		t.Run(reason, func(t *testing.T) {
			_, s := testDB(t)
			ctx := context.Background()
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, 60_000)
			uow, err := s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(ctx)
			if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
				RequestID: &requestID, Reason: reason, Deadline: time.Now().UTC(),
				Detail: json.RawMessage(`{"schema_version":1,"audit_marker":"manual finding"}`),
			}); err != nil {
				t.Fatal(err)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ := correctedOverageJob(t, s, requestID)
			if err := s.ResolveReconciliationJob(ctx, job.ID, "operator verified this finding"); err != nil {
				t.Fatal(err)
			}
			closed, before := correctedOverageJob(t, s, requestID)
			tokens := int64(130)
			if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 130_000, &tokens, "manual-overage")); err != nil {
				t.Fatal(err)
			}
			after, detail := correctedOverageJob(t, s, requestID)
			if after.ID != closed.ID || after.Status != "resolved" || after.Reason != reason ||
				closed.ResolvedAt == nil || after.ResolvedAt == nil || !after.ResolvedAt.Equal(*closed.ResolvedAt) ||
				!reflect.DeepEqual(detail.Resolution, before.Resolution) || detail.AuditMarker != "manual finding" ||
				len(detail.ResolutionHistory) != 0 {
				t.Errorf("overage replaced or reopened manual conclusion: %+v / %s", after, after.Detail)
			}
		})
	}
}

func TestCorrectSettlement_OverageReopenFailureRollsBack(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	requestID, attemptID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, 60_000)
	_, originalAt, _, _ := rateTokens(t, s, requestID)
	tokens := int64(120)
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 120_000, &tokens, "rollback-first-overage")); err != nil {
		t.Fatal(err)
	}
	tokens = 90
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 90_000, &tokens, "rollback-closure")); err != nil {
		t.Fatal(err)
	}
	closed, closedDetail := correctedOverageJob(t, s, requestID)
	if closed.Status != "resolved" || closed.ResolvedAt == nil || closedDetail.Resolution == nil {
		t.Fatalf("closure fixture = %+v / %s", closed, closed.Detail)
	}
	if _, err := db.Exec(`CREATE FUNCTION test_fail_overage_reopen() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected overage reopen failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS test_fail_overage_reopen ON inference_reconciliation_jobs`); err != nil {
			t.Error(err)
		}
		if _, err := db.Exec(`DROP FUNCTION IF EXISTS test_fail_overage_reopen()`); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(`CREATE TRIGGER test_fail_overage_reopen
		BEFORE UPDATE ON inference_reconciliation_jobs FOR EACH ROW
		WHEN (OLD.status='resolved' AND NEW.status='pending' AND OLD.reason='settlement_overage')
		EXECUTE FUNCTION test_fail_overage_reopen()`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		uow, err := s.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer uow.Rollback(ctx)
		return correctionAccountingSnapshot(t, mustTx(t, uow), requestID)
	}
	before := snapshot()
	tokens = 130
	cmd := correctionFixture(requestID, attemptID, 130_000, &tokens, "rollback-recurrence")
	err := applyCorrection(t, s, cmd)
	if err == nil || !strings.Contains(err.Error(), "injected overage reopen failure") {
		t.Errorf("reopen write = %v, want injected failure", err)
	}
	if after := snapshot(); after != before {
		t.Error("failed reopening committed partial accounting or changed the resolved job")
	}
	if _, err := db.Exec(`DROP TRIGGER test_fail_overage_reopen ON inference_reconciliation_jobs`); err != nil {
		t.Fatal(err)
	}
	if err := applyCorrection(t, s, cmd); err != nil {
		t.Fatalf("retry same key after rollback: %v", err)
	}
	reopened, detail := correctedOverageJob(t, s, requestID)
	if reopened.ID != closed.ID || reopened.Status != "pending" || reopened.ResolvedAt != nil ||
		detail.Resolution != nil || len(detail.ResolutionHistory) != 1 {
		t.Errorf("retry recurrence = %+v / %s", reopened, reopened.Detail)
	}
	assertCorrectedOverageAccounting(t, s, requestID, f.keyID, []string{w5, ww, wm}, 130_000, 130, originalAt, 3)
}
