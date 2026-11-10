package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCorrectSettlement_ClosesOnlyAddressedJobs(t *testing.T) {
	for _, status := range []string{"pending", "running", "escalated"} {
		t.Run(status, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				reason    string
				original  domain.Microcredit
				corrected domain.Microcredit
				resolves  bool
			}{
				{"unknown_usage", ReasonUnknownUsage, 60_000, 50_000, true},
				{"crash_recovery", ReasonCrashRecovery, 60_000, 50_000, true},
				{"equal_money_evidence", ReasonCrashRecovery, 60_000, 60_000, true},
				{"overage_within_hold", ReasonSettlementOverage, 120_000, 90_000, true},
				{"overage_exact_hold", ReasonSettlementOverage, 120_000, 100_000, true},
				{"still_over_hold", ReasonSettlementOverage, 120_000, 130_000, false},
				{"manual_mismatch", ReasonSettlementMismatch, 60_000, 50_000, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					db, s := testDB(t)
					ctx := context.Background()
					f := seedFixture(t, s, true)
					w5, ww, wm := makeWindows(t, s, f.entID)
					reqID, attID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, tc.original)
					_, originalAt, _, _ := rateTokens(t, s, reqID)
					deadline := time.Now().UTC().Add(-time.Hour)
					uow, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer uow.Rollback(ctx)
					if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
						RequestID: &reqID, Reason: tc.reason, Deadline: deadline,
						Detail: json.RawMessage(`{"schema_version":1,"audit_marker":"original finding"}`),
					}); err != nil {
						t.Fatal(err)
					}
					if err := uow.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					var jobID string
					if err := db.Get(&jobID, `SELECT id FROM inference_reconciliation_jobs WHERE request_id=$1`, reqID); err != nil {
						t.Fatal(err)
					}
					switch status {
					case "running":
						if _, err := db.Exec(`UPDATE inference_reconciliation_jobs SET status='running' WHERE id=$1`, jobID); err != nil {
							t.Fatal(err)
						}
					case "escalated":
						ids, err := s.EscalateOverdueReconciliationJobs(ctx, time.Now().UTC(), 10)
						if err != nil || len(ids) != 1 || ids[0] != jobID {
							t.Fatalf("escalate = %v, %v; want [%s]", ids, err, jobID)
						}
					}

					tokens := int64(90)
					cmd := correctionFixture(reqID, attID, tc.corrected, &tokens, "addressed-job")
					if err := applyCorrection(t, s, cmd); err != nil {
						t.Fatalf("correction: %v", err)
					}
					var afterStatus, afterReason string
					var detail json.RawMessage
					var resolved sql.NullTime
					var jobCount int
					if err := db.Get(&jobCount, `SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id=$1`, reqID); err != nil {
						t.Fatal(err)
					}
					if err := db.QueryRow(`SELECT status, reason, detail, resolved_at FROM inference_reconciliation_jobs WHERE id=$1`, jobID).
						Scan(&afterStatus, &afterReason, &detail, &resolved); err != nil {
						t.Fatal(err)
					}
					wantStatus := status
					if tc.resolves {
						wantStatus = "resolved"
					}
					if jobCount != 1 || afterStatus != wantStatus || afterReason != tc.reason {
						t.Errorf("job count/status/reason = %d/%s/%s, want 1/%s/%s",
							jobCount, afterStatus, afterReason, wantStatus, tc.reason)
					}
					var doc map[string]json.RawMessage
					if err := json.Unmarshal(detail, &doc); err != nil {
						t.Fatal(err)
					}
					if string(doc["audit_marker"]) != `"original finding"` {
						t.Errorf("lost original audit detail: %s", detail)
					}
					if tc.resolves {
						if !resolved.Valid || !strings.Contains(string(doc["resolution"]), cmd.Reason) {
							t.Errorf("missing correction resolution time/note: %v/%s", resolved, detail)
						}
					} else if resolved.Valid || doc["resolution"] != nil {
						t.Errorf("unaddressed job incorrectly resolved: %v/%s", resolved, detail)
					}
					if tc.name == "still_over_hold" {
						if string(doc["charge_micros"]) != "130000" || string(doc["over_micros"]) != "30000" {
							t.Errorf("lost current overage: %s", detail)
						}
					}

					req, err := s.GetRequest(ctx, reqID)
					if err != nil {
						t.Fatal(err)
					}
					usage, err := s.LoadUsageRecords(ctx, reqID)
					if err != nil {
						t.Fatal(err)
					}
					if req.Status != domain.ReqSettled || req.SettledMicros == nil || *req.SettledMicros != tc.corrected ||
						req.UsageStatus != domain.UsageReported || len(usage) != 2 || usage[1].Source != domain.UsageReported {
						t.Errorf("correction not applied: request=%+v usage=%+v", req, usage)
					}
					gotTokens, afterAt, state, present := rateTokens(t, s, reqID)
					if !present || state != "settled" || gotTokens != 90 ||
						originalAt == nil || afterAt == nil || !originalAt.Equal(*afterAt) {
						t.Errorf("rate correction = %d/%s/%v; original time=%v", gotTokens, state, afterAt, originalAt)
					}
					for _, winID := range []string{w5, ww, wm} {
						if used, _ := windowState(t, s, winID); used != tc.corrected {
							t.Errorf("window used = %d, want %d", used, tc.corrected)
						}
					}
					if used := keyBudgetUsed(t, s, f.keyID); used != tc.corrected {
						t.Errorf("key budget used = %d, want %d", used, tc.corrected)
					}
				})
			}
		})
	}
}
