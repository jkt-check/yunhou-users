package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

// quota_settlement_test.go — 预占/释放/结算共享事务的行为验证（真实库）。

// reserveCmd builds a full four-target hold (three windows + key budget).
func reserveCmd(f fixture, w5, ww, wm string, amount domain.Microcredit) domain.ReserveCommand {
	admitted := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	return domain.ReserveCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
			PolicyVersionID: f.policyID,
		},
		AdmittedAt: admitted,
		Holds: []domain.HoldSpec{
			{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: amount},
			{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: amount},
			{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: amount},
			{TargetKind: domain.TargetKeyBudget, APIKeyID: &f.keyID, Amount: amount},
		},
	}
}

func windowState(t *testing.T, s *Store, id string) (used, reserved domain.Microcredit) {
	t.Helper()
	var u, r int64
	if err := s.db.QueryRow(
		`SELECT used_micros, reserved_micros FROM inference_quota_windows WHERE id = $1`, id).
		Scan(&u, &r); err != nil {
		t.Fatalf("window state: %v", err)
	}
	return domain.Microcredit(u), domain.Microcredit(r)
}

func keyBudgetUsed(t *testing.T, s *Store, id string) domain.Microcredit {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(
		`SELECT budget_used_micros FROM inference_api_keys WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("key budget: %v", err)
	}
	return domain.Microcredit(v)
}

func TestReserveAndSettleShareOneTransaction(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 预占与结算在同一个 UnitOfWork（同一个 *sqlx.Tx）。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if adm.RequestID != cmd.Request.ID || len(adm.Holds) != 4 {
		t.Fatalf("admission = %+v, want 4 holds", adm)
	}

	attemptID := uuid.NewString()
	att := &domain.Attempt{ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1}
	if err := insertAttempt(ctx, mustTx(t, uow), att); err != nil {
		t.Fatalf("attempt: %v", err)
	}

	in, out := int64(800), int64(200)
	cost, _ := domain.NewMoney(1234, "USD")
	basis := domain.CostReported
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000,
		AttemptID:    &attemptID,
		AttemptCost:  &cost,
		CostBasis:    &basis,
		SettledAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 三窗口 used 同额增加、reserved 归零（设计 §6: 一次消费同时增加三个
	// 适用窗口的 used，客户只结算一次）。
	for _, id := range []string{w5, ww, wm} {
		used, reserved := windowState(t, s, id)
		if used != 60_000 || reserved != 0 {
			t.Errorf("window %s: used=%d reserved=%d, want 60000/0", id, used, reserved)
		}
	}
	// Key 预算按实际消费校正：预占 100_000，结算 60_000，差额归还。
	if got := keyBudgetUsed(t, s, f.keyID); got != 60_000 {
		t.Errorf("key budget used = %d, want 60000 (charge, not hold)", got)
	}
	// 请求终态与账本。
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// 请求级预占额 = 单次消费预占上界（镜像进 4 个目标，只记一次，
	// 不得是 hold 之和 400_000）。
	if req.ReservedMicros == nil || *req.ReservedMicros != 100_000 {
		t.Errorf("reserved_micros = %v, want 100000 (single consumption, not 4× hold sum)", req.ReservedMicros)
	}
	if req.Status != domain.ReqSettled || req.SettledMicros == nil || *req.SettledMicros != 60_000 {
		t.Errorf("request = status %q settled %v, want settled/60000", req.Status, req.SettledMicros)
	}
	if req.UsageStatus != domain.UsageReported {
		t.Errorf("usage_status = %q, want reported", req.UsageStatus)
	}
	if req.WindowFiveHourID == nil || *req.WindowFiveHourID != w5 {
		t.Errorf("window binding lost: %+v", req.WindowFiveHourID)
	}
	var charges int
	if err := s.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if charges != 1 {
		t.Errorf("ledger charges = %d, want 1", charges)
	}
	var costBasis string
	if err := s.db.Get(&costBasis,
		`SELECT cost_basis FROM inference_attempts WHERE id = $1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if costBasis != string(domain.CostReported) {
		t.Errorf("cost_basis = %q, want reported", costBasis)
	}
}

func TestReserveRollbackLeavesNoPartialHolds(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// 预占后回滚：不得遗留任何部分占用（设计 §7.2）。
	if err := uow.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, id := range []string{w5, ww, wm} {
		if used, reserved := windowState(t, s, id); used != 0 || reserved != 0 {
			t.Errorf("window %s after rollback: used=%d reserved=%d, want 0/0", id, used, reserved)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget after rollback = %d, want 0", got)
	}
	var reqCount int
	if err := s.db.Get(&reqCount, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if reqCount != 0 {
		t.Errorf("requests after rollback = %d, want 0", reqCount)
	}
}

func TestReserveFailsWhenAnyWindowLacksQuota(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	// 五小时/周窗口充足；月窗口只剩 50k，预占 100k → 全部拒绝
	// （不能只占五小时/周/Key）。
	start := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	mkWindow := func(kind domain.WindowKind, end time.Time, limit domain.Microcredit) string {
		w := &domain.QuotaWindow{EntitlementID: f.entID, Kind: kind, Start: start, End: end, Limit: limit}
		if err := s.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatalf("insert %s window: %v", kind, err)
		}
		return w.ID
	}
	w5 := mkWindow(domain.WindowFiveHour, start.Add(5*time.Hour), 1_000_000)
	ww := mkWindow(domain.WindowWeekly, start.Add(7*24*time.Hour), 10_000_000)
	tinyID := mkWindow(domain.WindowMonthly, start.AddDate(0, 1, 0), 50_000)

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, uow, reserveCmd(f, w5, ww, tinyID, 100_000))
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if len(qe.BlockedBy) != 1 || qe.BlockedBy[0].Kind != domain.WindowMonthly {
		t.Errorf("blocked by %+v, want monthly window", qe.BlockedBy)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// 失败不遗留部分占用。
	for _, id := range []string{w5, ww, tinyID} {
		if used, reserved := windowState(t, s, id); used != 0 || reserved != 0 {
			t.Errorf("window %s: used=%d reserved=%d, want 0/0", id, used, reserved)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget = %d, want 0", got)
	}
}

func TestReserveFailsOnKeyBudget(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// Key 预算 5_000_000，预占 6_000_000 → KeyBudgetExhausted。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 6_000_000))
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if !qe.KeyBudgetExhausted {
		t.Errorf("KeyBudgetExhausted = false, want true")
	}
	_ = uow.Rollback(ctx)
}

func TestDuplicateSettleIsAConflict(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, _ := s.Begin(ctx)
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(10), int64(5)
	settle := func(uow domain.UnitOfWork) error {
		return s.Settle(ctx, uow, domain.SettleCommand{
			RequestID: adm.RequestID,
			Usage: domain.UsageRecord{
				RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
				Buckets:  domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
				RawUsage: domain.ExtensionConfig{SchemaVersion: 1},
			},
			ChargeMicros: 60_000, SettledAt: time.Now().UTC(),
		})
	}
	if err := settle(uow); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 结算唯一键：重复结算撞 UNIQUE(request_id) WHERE entry_type='charge'。
	uow2, _ := s.Begin(ctx)
	err = settle(uow2)
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second settle: err = %v, want CodeConflict", err)
	}
	// 账本仍只有一条 charge。
	var charges int
	if err := s.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1 AND entry_type='charge'`,
		adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if charges != 1 {
		t.Errorf("charges = %d, want 1 (重复结算不生效)", charges)
	}
}

func TestSettleCorrectsKeyBudgetOverHold(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 预占 100_000，实际结算 25_000：budget_used 须校正为 25_000
	// （差额归还，不永久多计）。
	settleOnce := func(hold, charge domain.Microcredit) string {
		t.Helper()
		uow, err := s.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, hold))
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		attID := uuid.NewString()
		if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
			t.Fatal(err)
		}
		in, out := int64(10), int64(5)
		err = s.Settle(ctx, uow, domain.SettleCommand{
			RequestID: adm.RequestID,
			Usage: domain.UsageRecord{
				RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
				Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
			},
			ChargeMicros: charge, SettledAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return adm.RequestID
	}

	settleOnce(100_000, 25_000)
	if got := keyBudgetUsed(t, s, f.keyID); got != 25_000 {
		t.Errorf("after settle 25k of 100k hold: budget_used = %d, want 25000", got)
	}
	for _, id := range []string{w5, ww, wm} {
		if used, reserved := windowState(t, s, id); used != 25_000 || reserved != 0 {
			t.Errorf("window %s: used=%d reserved=%d, want 25000/0", id, used, reserved)
		}
	}

	// 第二个请求累计实际消费：25_000 + 50_000 = 75_000。
	settleOnce(80_000, 50_000)
	if got := keyBudgetUsed(t, s, f.keyID); got != 75_000 {
		t.Errorf("after second settle: budget_used = %d, want 75000 (累计实际消费)", got)
	}

	// 零消费结算（usage 未知但确认为零的场景）：预占全额归还。
	settleOnce(10_000, 0)
	if got := keyBudgetUsed(t, s, f.keyID); got != 75_000 {
		t.Errorf("after zero-charge settle: budget_used = %d, want 75000 (hold 全额归还)", got)
	}
}

func TestReleaseReturnsAllHolds(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, _ := s.Begin(ctx)
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 确认无上游消费 → 同事务释放全部预占。
	uow2, _ := s.Begin(ctx)
	if err := s.Release(ctx, uow2, adm.RequestID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{w5, ww, wm} {
		if used, reserved := windowState(t, s, id); used != 0 || reserved != 0 {
			t.Errorf("window %s after release: used=%d reserved=%d, want 0/0", id, used, reserved)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget after release = %d, want 0", got)
	}
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqReleased {
		t.Errorf("status = %q, want released", req.Status)
	}
}

func TestMarkReconciliationRequired(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, _ := s.Begin(ctx)
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().UTC().Add(24 * time.Hour)
	uow2, _ := s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow2, adm.RequestID, "unknown_usage", deadline); err != nil {
		t.Fatalf("mark reconciliation: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqReconciliationRequired {
		t.Errorf("status = %q, want reconciliation_required", req.Status)
	}
	var jobs int
	if err := s.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1 AND status = 'pending'`,
		adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("reconciliation jobs = %d, want 1", jobs)
	}
	// 预占保留（禁止仅凭未知状态释放）。
	if _, reserved := windowState(t, s, w5); reserved != 100_000 {
		t.Errorf("five-hour reserved = %d, want 100000 (保留合理预占进入核对)", reserved)
	}
}

// 评审轮1 I2：mark 与并发成功 settle 交错——终态绝不被覆盖。settle 先提交
// 后 MarkReconciliationRequired 必须静默收敛：status 保持 settled、
// usage_status 不被改回 unknown、不入队核对任务。
func TestMarkReconciliationRequired_TerminalGuard(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	reqID, _ := settledRequest(t, s, f, w5, ww, wm, 100_000, 42_000)

	deadline := time.Now().UTC().Add(24 * time.Hour)
	uow, _ := s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow, reqID, "unknown_usage", deadline); err != nil {
		t.Fatalf("mark over a settled request must converge silently: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := s.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqSettled {
		t.Errorf("status = %q, want settled (终态不被覆盖)", req.Status)
	}
	if req.UsageStatus != domain.UsageReported {
		t.Errorf("usage_status = %q, want reported (不被改回 unknown)", req.UsageStatus)
	}
	var jobs int
	if err := s.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Errorf("jobs = %d, want 0 (终态请求不入队任务)", jobs)
	}

	// released 终态同样收敛。
	uow2, _ := s.Begin(ctx)
	adm2, err := s.Reserve(ctx, uow2, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, uow2, adm2.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow2, adm2.RequestID, "unknown_usage", deadline); err != nil {
		t.Fatalf("mark over a released request must converge silently: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	req2, err := s.GetRequest(ctx, adm2.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req2.Status != domain.ReqReleased {
		t.Errorf("status = %q, want released (终态不被覆盖)", req2.Status)
	}

	// 请求不存在仍是调用方 bug → NotFound。
	uow3, _ := s.Begin(ctx)
	err = s.MarkReconciliationRequired(ctx, uow3, uuid.NewString(), "unknown_usage", deadline)
	_ = uow3.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing request: err = %v, want not_found", err)
	}
}

// 评审轮1 I2（任务 CASE 守卫）：已 resolved 的任务不被同理由投递无条件
// 重开；新理由（新证据）才重开为 pending。
func TestMarkReconciliationRequired_JobReopenGuard(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, _ := s.Begin(ctx)
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(24 * time.Hour)
	if err := s.MarkReconciliationRequired(ctx, uow, adm.RequestID, "unknown_usage", deadline); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := s.db.Get(&jobID,
		`SELECT id FROM inference_reconciliation_jobs WHERE request_id = $1`, adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveReconciliationJob(ctx, jobID, "manually verified"); err != nil {
		t.Fatal(err)
	}

	// 同理由重投：请求重新入核对状态，但任务保持 resolved（不无条件重开）。
	uow2, _ := s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow2, adm.RequestID, "unknown_usage", deadline); err != nil {
		t.Fatal(err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.Get(&status,
		`SELECT status FROM inference_reconciliation_jobs WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" {
		t.Errorf("job status = %q after same-reason re-mark, want resolved (不无条件重开)", status)
	}

	// 新理由（新证据）→ 重开为 pending 并换理由。
	uow3, _ := s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow3, adm.RequestID, "crash_recovery", deadline); err != nil {
		t.Fatal(err)
	}
	if err := uow3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := s.db.QueryRow(
		`SELECT status, reason FROM inference_reconciliation_jobs WHERE id = $1`, jobID).
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || reason != "crash_recovery" {
		t.Errorf("job = %s/%s after new-reason mark, want pending/crash_recovery (新证据重开)", status, reason)
	}
}

// mustTx unwraps the shared tx for test-only direct writes.
func mustTx(t *testing.T, w domain.UnitOfWork) *sqlx.Tx {
	t.Helper()
	tx, err := sqlTx(w)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}
