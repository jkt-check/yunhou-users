package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// settlement_recovery_edges_test.go — Task 16 覆盖率补强：恢复 worker 的
// 人工处理分支（无上界请求只告警不动手；窗口/账本差异入队不自动修复）。
// 五个故障注入点的主路径已在 settlement_recovery_test.go。

// 031 前存量行（无准入上界）：可能已发送且无上界——保守估算无从谈起，
// 只告警、不结算、不释放（未发送确认零消费仍走证据释放，与本路径无关）。
func TestRecover_NoReservedAmount_AlarmOnly(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	// 可能已发送（dispatching attempt + dispatching 请求）→ 走估算路径。
	_ = f.fabricateAttempt(t, reqID, "dispatching", "", false)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}
	// 模拟 031 前存量行：无持久化上界。
	if _, err := f.db.ExecContext(ctx,
		`UPDATE inference_requests SET reserved_micros = NULL WHERE id = $1`, reqID); err != nil {
		t.Fatal(err)
	}

	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors == 0 || stats.SettledConservative != 0 || stats.ReleasedNotSent != 0 {
		t.Fatalf("stats = %+v, want alarm-only (errors>0, no settle, no release)", stats)
	}
	status, usage, _, _ := f.requestState(t, reqID)
	if status == "settled" || status == "released" || usage == "estimated" {
		t.Errorf("request = %s/%s, want untouched (人工处理)", status, usage)
	}
	// 预占保留（不静默释放）。
	var holds int
	if err := f.db.Get(&holds,
		`SELECT COUNT(*) FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
		t.Fatal(err)
	}
	if holds == 0 {
		t.Error("reservation must stay held for manual handling")
	}
}

// 窗口聚合与账本重建不一致 → 窗口级 ledger_mismatch 任务入队（不自动修
// 复，人工核对）。
func TestRecover_WindowMismatchEnqueuedNotAutoFixed(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	// 正常保守结算一次（账本/窗口一致）。
	_ = f.fabricateAttempt(t, reqID, "dispatching", "", false)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 {
		t.Fatalf("setup settle = %+v", stats)
	}
	f.assertLedgerMatchesWindows(t)

	// 人为制造差异：窗口 used 被多扣（与账本不符）。
	if _, err := f.db.ExecContext(ctx,
		`UPDATE inference_quota_windows SET used_micros = used_micros + 1234 WHERE id = $1`, f.w5); err != nil {
		t.Fatal(err)
	}
	stats, err = f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowMismatches != 1 {
		t.Fatalf("stats = %+v, want 1 window mismatch", stats)
	}
	// 窗口级任务：request_id IS NULL + detail 带 window_id + reason
	// ledger_mismatch；差异未被自动修复。
	var reason, detail string
	if err := f.db.QueryRow(
		`SELECT reason, detail::text FROM inference_reconciliation_jobs WHERE request_id IS NULL`).
		Scan(&reason, &detail); err != nil {
		t.Fatalf("window-level job missing: %v", err)
	}
	if reason != "ledger_mismatch" || detail == "" {
		t.Errorf("job = %s %s", reason, detail)
	}
	used, _ := f.windowUsedReserved(t, f.w5)
	if used != 50_000+1234 {
		t.Errorf("window used = %d, want unchanged (不自动修复)", used)
	}
	// 入队去重（部分唯一索引）：再来一轮不重复建任务。
	stats, err = f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := f.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("window jobs = %d, want 1 (去重)", jobs)
	}
}

// --- 审查修复 Minor-6：恢复 pass 各阶段独立执行、错误聚合 -----------------
//
// stageFakeStore 按阶段注入失败并记录调用（嵌入 nil 接口，仅覆盖 RunPass
// 触达的五个阶段入口）。无需 DB。

type stageFakeStore struct {
	RecoveryStore
	fail   map[string]error
	called map[string]bool
}

func (s *stageFakeStore) mark(stage string) error {
	s.called[stage] = true
	return s.fail[stage]
}

func (s *stageFakeStore) ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error) {
	return nil, s.mark("scan")
}

func (s *stageFakeStore) ListPendingReconciliationJobs(ctx context.Context, limit int) ([]postgres.ReconciliationJob, error) {
	return nil, s.mark("jobs")
}

func (s *stageFakeStore) EscalateOverdueReconciliationJobs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return nil, s.mark("escalate")
}

func (s *stageFakeStore) ReconcileWindowAggregates(ctx context.Context) ([]accounting.WindowReconciliation, error) {
	return nil, s.mark("reconcile")
}

func (s *stageFakeStore) RecoveryMetrics(ctx context.Context, staleCutoff time.Time) (postgres.RecoveryMetrics, error) {
	return postgres.RecoveryMetrics{}, s.mark("metrics")
}

// 阶段 2 失败不得连带跳过阶段 3（到期升级——卡死资金的安全网）与阶段 4；
// 失败聚合后统一返回。
func TestRunPass_StagesIndependentOnFailure(t *testing.T) {
	scanErr := errors.New("scan boom")
	jobsErr := errors.New("jobs boom")
	store := &stageFakeStore{
		fail:   map[string]error{"scan": scanErr, "jobs": jobsErr},
		called: map[string]bool{},
	}
	w := NewSettlementRecovery(store, nil, RecoveryConfig{}, nil)
	stats, err := w.RunPass(context.Background())
	if err == nil || !errors.Is(err, scanErr) || !errors.Is(err, jobsErr) {
		t.Fatalf("err = %v, want aggregated scan+jobs errors", err)
	}
	for _, stage := range []string{"scan", "jobs", "escalate", "reconcile", "metrics"} {
		if !store.called[stage] {
			t.Errorf("stage %s skipped — 阶段失败不得连带跳过后续阶段", stage)
		}
	}
	if stats.Errors != 2 {
		t.Errorf("stats.Errors = %d, want 2 (每失败阶段计一次)", stats.Errors)
	}
}

// 全部阶段健康时返回 nil 错误（errors.Join 空集语义）。
func TestRunPass_NoStageFailureReturnsNil(t *testing.T) {
	store := &stageFakeStore{fail: map[string]error{}, called: map[string]bool{}}
	w := NewSettlementRecovery(store, nil, RecoveryConfig{}, nil)
	if _, err := w.RunPass(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
