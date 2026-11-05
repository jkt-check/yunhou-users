package workers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// settlement_recovery_outcomes_test.go — T2 修复轮：终态投递的**分类**必须在
// 恢复 worker 里被正确处置。
//
// 只有"请求已被并发结算、账本一致"（duplicate）与"恢复的保守估算让位于线上
// 已入账的金额"（charge_mismatch）是良性竞态；账本漂移（ledger_drift）、
// 预占漂移（reservation_drift）、缺预占（missing_reservation）与 released
// 后的晚到用量（late_usage_after_release）都是账务异常：必须告警并进入
// 专门核对，绝不能被当成"竞态"吞掉。

// fakeUOW is an inert transaction for the fake store.
type fakeUOW struct{}

func (fakeUOW) Commit(ctx context.Context) error   { return nil }
func (fakeUOW) Rollback(ctx context.Context) error { return nil }

// outcomeFakeStore drives the worker's classification of a typed terminal
// settlement outcome deterministically: the real store only meets a terminal
// request inside a race, so the fake injects the outcome instead.
type outcomeFakeStore struct {
	RecoveryStore
	outcome *domain.SettleError
	// parked records the reasons MarkReconciliationRequired was called with.
	parked []string
	// reported records the reasons ReportTerminalAnomaly was called with.
	reported []string
	// enqueued records the reasons EnqueueReconciliationJobTx was called with.
	enqueued []string
	// settleCalls counts the settlement attempts.
	settleCalls int
}

func (s *outcomeFakeStore) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	return fakeUOW{}, nil
}

func (s *outcomeFakeStore) ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error) {
	reserved := domain.Microcredit(50_000)
	return []domain.Request{{
		ID: "req-outcome", BillingAccountID: "acct-1", ModelID: "glm-4.6",
		Status: domain.ReqDispatching, ReservedMicros: &reserved,
	}}, nil
}

func (s *outcomeFakeStore) ListAttempts(ctx context.Context, requestID string) ([]domain.Attempt, error) {
	now := time.Now().UTC()
	return []domain.Attempt{{ID: "att-1", RequestID: requestID, AttemptNo: 1, Status: "dispatching", StartedAt: &now}}, nil
}

func (s *outcomeFakeStore) AttemptErrorKinds(ctx context.Context, requestID string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *outcomeFakeStore) LatestUsageRevision(ctx context.Context, attemptID string) (int, error) {
	return 0, nil
}

func (s *outcomeFakeStore) LatestPriceVersion(ctx context.Context, modelID, kind string, at time.Time) (*postgres.PriceVersion, error) {
	return nil, domain.NewError(domain.CodeNotFound, "no cost list")
}

func (s *outcomeFakeStore) Settle(ctx context.Context, w domain.UnitOfWork, cmd domain.SettleCommand) error {
	s.settleCalls++
	return s.outcome
}

func (s *outcomeFakeStore) MarkReconciliationRequired(ctx context.Context, w domain.UnitOfWork, requestID, reason string, deadline time.Time) error {
	s.parked = append(s.parked, reason)
	return nil
}

func (s *outcomeFakeStore) ReportTerminalAnomaly(ctx context.Context, w domain.UnitOfWork, requestID, reason string, detail json.RawMessage, deadline time.Time) error {
	s.reported = append(s.reported, reason)
	return nil
}

func (s *outcomeFakeStore) EnqueueReconciliationJobTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.EnqueueReconciliationCommand) error {
	s.enqueued = append(s.enqueued, cmd.Reason)
	return nil
}

func (s *outcomeFakeStore) ListPendingReconciliationJobs(ctx context.Context, limit int) ([]postgres.ReconciliationJob, error) {
	return nil, nil
}

func (s *outcomeFakeStore) EscalateOverdueReconciliationJobs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return nil, nil
}

func (s *outcomeFakeStore) ReconcileWindowAggregates(ctx context.Context) ([]accounting.WindowReconciliation, error) {
	return nil, nil
}

func (s *outcomeFakeStore) RecoveryMetrics(ctx context.Context, staleCutoff time.Time) (postgres.RecoveryMetrics, error) {
	return postgres.RecoveryMetrics{}, nil
}

// TestRecover_TerminalOutcomeClassification — 每一类终态投递在恢复 worker 里
// 的处置必须不同：良性竞态静默跳过；账务异常必须告警并进入专门核对。
func TestRecover_TerminalOutcomeClassification(t *testing.T) {
	cases := []struct {
		kind string
		// wantRace marks the benign outcomes: the estimate yields to the
		// charge the live path already booked.
		wantRace bool
		// wantReason is the reconciliation reason the anomaly must be
		// filed under (empty for the benign outcomes).
		wantReason string
	}{
		{kind: "duplicate", wantRace: true},
		{kind: "charge_mismatch", wantRace: true},
		{kind: "ledger_drift", wantReason: "ledger_drift"},
		{kind: "reservation_drift", wantReason: "reservation_drift"},
		{kind: "missing_reservation", wantReason: "missing_reservation"},
		{kind: "released", wantReason: "late_usage_after_release"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			store := &outcomeFakeStore{outcome: &domain.SettleError{
				Kind:    domain.SettleOutcomeKind(tc.kind),
				Code:    domain.CodeConflict,
				Message: "injected terminal outcome",
			}}
			w := NewSettlementRecovery(store, nil, RecoveryConfig{}, nil)
			stats, err := w.RunPass(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if store.settleCalls != 1 {
				t.Fatalf("settle calls = %d, want 1", store.settleCalls)
			}
			if tc.wantRace {
				if stats.RacingSkipped != 1 || stats.Errors != 0 {
					t.Errorf("stats = %+v, want one benign skip and no alarm", stats)
				}
				if len(store.parked) != 0 || len(store.reported) != 0 {
					t.Errorf("parked = %v, reported = %v, want none (良性竞态不得入核对)", store.parked, store.reported)
				}
				return
			}
			// 账务异常：必须告警 + 进入专门核对。
			if stats.Errors == 0 {
				t.Errorf("stats = %+v, want an alarm (账务异常必须响亮)", stats)
			}
			if stats.RacingSkipped != 0 {
				t.Errorf("stats = %+v, want no benign skip (账务异常不得当竞态)", stats)
			}
			got := append(append([]string{}, store.parked...), store.reported...)
			if len(got) != 1 || got[0] != tc.wantReason {
				t.Errorf("reconciliation reason = %v, want [%s]", got, tc.wantReason)
			}
		})
	}
}

// TestRecover_MissingReservationRelease_ParksWithoutReplay — 本应存在的金额
// 预占被删除后，确认零消费的**释放**路径不能"成功"（请求不得变成 released），
// 必须响亮地进入专门核对，并且下一轮扫描不得重复盲放。
func TestRecover_MissingReservationRelease_ParksWithoutReplay(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	// 无 attempt 意图行 → 分类为"未发送"→ 走释放路径。
	if _, err := f.db.Exec(
		`DELETE FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
		t.Fatal(err)
	}

	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors == 0 {
		t.Errorf("stats = %+v, want an alarm (缺预占必须响亮)", stats)
	}
	if stats.RacingSkipped != 0 {
		t.Errorf("stats = %+v, want no benign skip (缺预占不得当竞态)", stats)
	}
	if stats.ReleasedNotSent != 0 {
		t.Errorf("stats = %+v, want no release (缺预占时释放绝不能静默成功)", stats)
	}
	status, _, _, _ := f.requestState(t, reqID)
	if status == "released" {
		t.Error("request = released — 缺预占时不得伪造成功终态")
	}
	if status != "reconciliation_required" {
		t.Errorf("request status = %q, want reconciliation_required (停放进专门核对)", status)
	}
	reason, jobStatus, found := f.openJob(t, reqID)
	if !found {
		t.Fatal("no reconciliation job: 缺预占必须进入专门核对")
	}
	if reason != "missing_reservation" {
		t.Errorf("job reason = %q, want missing_reservation", reason)
	}
	if jobStatus != "pending" {
		t.Errorf("job status = %q, want pending", jobStatus)
	}

	// 第二轮：不得重复盲放、不得重复告警。
	stats2, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Errors != 0 {
		t.Errorf("second pass stats = %+v, want no repeated alarm", stats2)
	}
	if stats2.ReleasedNotSent != 0 {
		t.Errorf("second pass stats = %+v, want no repeated release", stats2)
	}
	var jobs int
	if err := f.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("jobs = %d, want 1 (不得重复入队)", jobs)
	}
}

// TestRecover_ManualOnlyJobIsNotDriven — 只能人工处理的核对原因
// （missing_reservation / late_usage_after_release / settlement_mismatch /
// insufficient_evidence）绝不能被恢复 worker 自动驱动：请求必须保持停放，
// 不得被自动结算，也不得重复告警。
func TestRecover_ManualOnlyJobIsNotDriven(t *testing.T) {
	for _, reason := range []string{
		"missing_reservation", "late_usage_after_release",
		"settlement_mismatch", "insufficient_evidence",
	} {
		t.Run(reason, func(t *testing.T) {
			f := newWorkerFixture(t)
			ctx := context.Background()
			reqID := f.fabricateReserved(t)
			f.fabricateAttempt(t, reqID, "dispatching", "", false)
			if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqDispatching, ""); err != nil {
				t.Fatal(err)
			}
			// 停放该请求，任务原因 = 只能人工处理的原因。
			uow, err := f.store.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.MarkReconciliationRequired(ctx, uow, reqID, reason,
				time.Now().UTC().Add(time.Hour)); err != nil {
				_ = uow.Rollback(ctx)
				t.Fatal(err)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			stats, err := f.newWorker().RunPass(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.SettledConservative != 0 || stats.SettledVerified != 0 {
				t.Errorf("stats = %+v, want no automatic settlement (人工原因不得被驱动)", stats)
			}
			if stats.Errors != 0 {
				t.Errorf("stats = %+v, want no repeated alarm", stats)
			}
			status, _, _, settled := f.requestState(t, reqID)
			if status != "reconciliation_required" {
				t.Errorf("request status = %q, want reconciliation_required (保持停放)", status)
			}
			if settled != 0 {
				t.Errorf("settled = %d, want 0 (人工原因不得自动入账)", settled)
			}
			var jobs int
			if err := f.db.Get(&jobs,
				`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`, reqID); err != nil {
				t.Fatal(err)
			}
			if jobs != 1 {
				t.Errorf("jobs = %d, want 1 (不得重复入队)", jobs)
			}
		})
	}
}

// TestRecover_InsufficientEvidence_DoesNotReDriveEveryPass — 证据不足的老行
// 停放进核对后，后续每一轮扫描都不得重复驱动、也不得重复告警。
func TestRecover_InsufficientEvidence_DoesNotReDriveEveryPass(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()

	// 老行形状：没有金额预占行，也没有任何可恢复估算证据。
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := f.store.Reserve(ctx, uow, domain.ReserveCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
			PolicyVersionID: f.policyID,
		},
		AdmittedAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	reqID := adm.RequestID
	f.fabricateAttempt(t, reqID, "dispatching", "", false)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}

	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 0 {
		t.Errorf("first pass stats = %+v, want no settlement (证据不足不得结算)", stats)
	}
	reason, _, found := f.openJob(t, reqID)
	if !found || reason != "insufficient_evidence" {
		t.Fatalf("job = %q (found=%v), want insufficient_evidence", reason, found)
	}

	// 后续每一轮：既不重新驱动，也不重复告警。
	for pass := 2; pass <= 3; pass++ {
		stats, err := f.newWorker().RunPass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stats.ParkedNoEvidence != 0 {
			t.Errorf("pass %d stats = %+v, want no re-drive (已停放的任务不得每轮重放)", pass, stats)
		}
		if stats.Errors != 0 {
			t.Errorf("pass %d stats = %+v, want no repeated alarm", pass, stats)
		}
		if stats.SettledConservative != 0 {
			t.Errorf("pass %d stats = %+v, want no settlement", pass, stats)
		}
	}
	var jobs int
	if err := f.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("jobs = %d, want 1", jobs)
	}
}
