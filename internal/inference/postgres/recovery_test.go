package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// recovery_test.go — Task 9 持久层：零消费结算落 charge 行、准入上界持久化、
// 滞留扫描、核对任务去重/升级、估算修正（冲正/补差）、账本重建核对、指标。

// settledRequest drives Reserve → attempt → Settle in single-statement
// transactions and returns (requestID, attemptID).
func settledRequest(t *testing.T, s *Store, f fixture, w5, ww, wm string, hold, charge domain.Microcredit) (string, string) {
	t.Helper()
	ctx := context.Background()
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
	return adm.RequestID, attID
}

func TestZeroChargeSettleAppendsChargeRow(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// Task 1 遗留决策点（Task 9 裁定）：零消费正常结算落一条 amount=0 的
	// charge 行 —— "已结算(0)"与"未结算"在账本层可区分，且结算唯一键统一
	// 保护全部投递。
	reqID, _ := settledRequest(t, s, f, w5, ww, wm, 100_000, 0)
	var amount int64
	if err := s.db.Get(&amount,
		`SELECT amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, reqID); err != nil {
		t.Fatalf("zero charge row missing: %v", err)
	}
	if amount != 0 {
		t.Errorf("charge amount = %d, want 0", amount)
	}
	req, err := s.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqSettled || req.SettledMicros == nil || *req.SettledMicros != 0 {
		t.Errorf("request = %s/%v, want settled/0", req.Status, req.SettledMicros)
	}
	// 重复投递：唯一键撞 CodeConflict，账本仍只有一条 charge。
	uow, _ := s.Begin(ctx)
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: uuid.NewString(), Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(0)), OutputTokens: ptr(int64(0))},
		},
		ChargeMicros: 0, SettledAt: time.Now().UTC(),
	})
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate zero settle: err = %v, want CodeConflict", err)
	}
	var charges int
	if err := s.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1 AND entry_type='charge'`, reqID); err != nil {
		t.Fatal(err)
	}
	if charges != 1 {
		t.Errorf("charges = %d, want 1 (重复投递只产生一次结果)", charges)
	}
	// reversal/adjustment 仍必须 > 0（031 CHECK 只放行 charge 的零额）。
	if _, err := s.db.Exec(
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, unit)
		 VALUES ($1,$2,'reversal',0,'microcredit')`, f.accountID, reqID); err == nil {
		t.Error("zero reversal must still violate the CHECK")
	}
}

func TestReservePersistsAdmissionBounds(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	in, out := int64(120), int64(8192)
	cmd.Request.InputBoundTokens = &in
	cmd.Request.OutputCapTokens = &out
	cmd.Request.ExtraBounds = map[string]int64{"tool_call": 2}
	uow, _ := s.Begin(ctx)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.InputBoundTokens == nil || *req.InputBoundTokens != 120 ||
		req.OutputCapTokens == nil || *req.OutputCapTokens != 8192 {
		t.Errorf("bounds = %v/%v, want 120/8192 (恢复估算依据随请求持久化)", req.InputBoundTokens, req.OutputCapTokens)
	}
	if req.ExtraBounds["tool_call"] != 2 {
		t.Errorf("extra bounds = %v, want tool_call=2", req.ExtraBounds)
	}
}

func TestListStaleOpenRequests(t *testing.T) {
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
	now := time.Now().UTC()
	// 新鲜的在途请求不被扫描（grace 保护活请求）。
	stale, err := s.ListStaleOpenRequests(ctx, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale = %d, want 0 (fresh in-flight request is not a crash candidate)", len(stale))
	}
	// 老化到 cutoff 之前 → 被扫描；终态（settled/released/
	// reconciliation_required）不在扫描集。
	if _, err := s.db.Exec(`UPDATE inference_requests SET updated_at = now() - interval '10 minutes' WHERE id = $1`, adm.RequestID); err != nil {
		t.Fatal(err)
	}
	stale, err = s.ListStaleOpenRequests(ctx, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != adm.RequestID {
		t.Fatalf("stale = %+v, want the aged request", stale)
	}

	// 审查修复 Critical 1：updated_at 老旧但存在 grace 窗口内启动的 attempt
	// （活的长请求：单一状态停留可远超任意朴素 grace——部署 RequestTimeout
	// 默认 10 分钟、nginx SSE 700s）→ 不得被扫描为崩溃候选。
	uow2, _ := s.Begin(ctx)
	adm2, err := s.Reserve(ctx, uow2, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	now2 := time.Now().UTC()
	if err := insertAttempt(ctx, mustTx(t, uow2), &domain.Attempt{
		ID: uuid.NewString(), RequestID: adm2.RequestID, AttemptNo: 1,
		StartedAt: &now2, // 网关在发送前落库 attempt 意图并带 started_at
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 请求状态跃迁老化，attempt started_at 保持新鲜（在途）。
	if _, err := s.db.Exec(
		`UPDATE inference_requests SET status='streaming', updated_at = now() - interval '10 minutes' WHERE id = $1`,
		adm2.RequestID); err != nil {
		t.Fatal(err)
	}
	stale, err = s.ListStaleOpenRequests(ctx, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stale {
		if r.ID == adm2.RequestID {
			t.Fatalf("live request %s (recent in-flight attempt) swept as crash candidate", adm2.RequestID)
		}
	}
	// attempt 也老化后（超过最坏活阶段）才允许被扫描。
	if _, err := s.db.Exec(
		`UPDATE inference_attempts SET started_at = now() - interval '10 minutes' WHERE request_id = $1`,
		adm2.RequestID); err != nil {
		t.Fatal(err)
	}
	stale, err = s.ListStaleOpenRequests(ctx, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range stale {
		if r.ID == adm2.RequestID {
			found = true
		}
	}
	if !found {
		t.Fatal("request with all attempts older than grace must become a crash candidate")
	}
}

func TestReconciliationJobUpsertAndEscalation(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	reqID, _ := settledRequest(t, s, f, w5, ww, wm, 100_000, 10_000)

	// 请求级任务：重复入队合并 detail、取更晚 deadline，不产生第二个任务。
	deadline := time.Now().UTC().Add(time.Hour)
	for _, tag := range []string{"first", "second"} {
		uow, _ := s.Begin(ctx)
		err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
			RequestID: &reqID, Reason: "crash_recovery",
			Detail:   json.RawMessage(`{"schema_version":1,"tag":"` + tag + `"}`),
			Deadline: deadline,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var cnt int
	var detail string
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(detail::text),'') FROM inference_reconciliation_jobs WHERE request_id = $1`,
		reqID).Scan(&cnt, &detail); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 || !strings.Contains(detail, "second") {
		t.Errorf("jobs = %d detail = %s, want 1 merged job", cnt, detail)
	}

	// 窗口级任务：request_id NULL，按 window_id 去重。审查修复 Important 2：
	// refresh 只合并 detail、保留原 deadline——漂移不修时每轮推后期限会使
	// 升级永不命中。
	for i, d := range []time.Duration{time.Hour, 48 * time.Hour} {
		uow, _ := s.Begin(ctx)
		err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
			WindowID: w5, Reason: "ledger_mismatch",
			Detail:   json.RawMessage(`{"schema_version":1,"stored_used":9,"rebuilt_used":4}`),
			Deadline: time.Now().UTC().Add(d),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if err := s.db.Get(&cnt,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("window jobs = %d, want 1 (dedup by window_id)", cnt)
	}
	var storedDeadline time.Time
	if err := s.db.Get(&storedDeadline,
		`SELECT deadline_at FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	// 第二次以更晚 deadline 重新入队：期限保持首次值（+1h 级），不被推到 +48h。
	if storedDeadline.After(time.Now().UTC().Add(2 * time.Hour)) {
		t.Errorf("window job deadline = %s, want the ORIGINAL deadline retained (refresh 只合并 detail)", storedDeadline)
	}

	// 期限告警：过期 open 任务升级；绝不自动释放/记零。
	past := time.Now().UTC().Add(2 * time.Hour)
	ids, err := s.EscalateOverdueReconciliationJobs(ctx, past, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("escalated = %d, want 2", len(ids))
	}
	// 升级后不再重复升级。
	ids, err = s.EscalateOverdueReconciliationJobs(ctx, past, 10)
	if err != nil || len(ids) != 0 {
		t.Errorf("re-escalation = %v/%v, want none", ids, err)
	}
	// 升级后的窗口任务不阻挡新差异入队（resolved/escalated 不占去重键）。
	uow, _ := s.Begin(ctx)
	if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
		WindowID: w5, Reason: "ledger_mismatch",
		Detail: json.RawMessage(`{"schema_version":1,"stored_used":9,"rebuilt_used":3}`), Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Get(&cnt, `SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("window jobs after re-report = %d, want 2 (escalated job does not block a fresh finding)", cnt)
	}
}

func TestCorrectSettlementDebitPreservesHistory(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 原结算 60_000（估算）；真实证据 95_000 → 补差 35_000。
	reqID, attID := settledRequest(t, s, f, w5, ww, wm, 100_000, 60_000)
	uow, _ := s.Begin(ctx)
	in, out := int64(30), int64(12)
	err := s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		CorrectedMicros: 95_000, Reason: "provider invoice arrived",
		OperatorSubject: "system:recovery", IdempotencyKey: "corr-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 账本：charge 60k + adjustment 35k debit（补差 delta 语义；冲正保留给
	// 全额作废）；原 charge 行保留不变。
	type entry struct {
		Type      string `db:"entry_type"`
		Amount    int64  `db:"amount_micros"`
		Reverses  *int64 `db:"reverses_entry_id"`
		Direction string `db:"direction"`
	}
	var entries []entry
	if err := s.db.Select(&entries,
		`SELECT le.entry_type, le.amount_micros, le.reverses_entry_id,
		        COALESCE(adj.direction,'') AS direction
		 FROM inference_ledger_entries le
		 LEFT JOIN inference_adjustments adj ON adj.id = le.adjustment_id
		 WHERE le.request_id = $1 ORDER BY le.id`, reqID); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want charge+adjustment", entries)
	}
	if entries[0].Type != "charge" || entries[0].Amount != 60_000 {
		t.Errorf("original charge rewritten! entries[0] = %+v (历史不可重写)", entries[0])
	}
	if entries[1].Type != "adjustment" || entries[1].Amount != 35_000 || entries[1].Direction != "debit" {
		t.Errorf("adjustment = %+v, want debit 35000 (补差)", entries[1])
	}
	// 窗口/Key 预算按 delta 校正；请求反映修正；usage 原 revision 保留。
	for _, id := range []string{w5, ww, wm} {
		if used, _ := windowState(t, s, id); used != 95_000 {
			t.Errorf("window %s used = %d, want 95000 (60k + 35k delta)", id, used)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 95_000 {
		t.Errorf("key budget = %d, want 95000", got)
	}
	req, err := s.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if *req.SettledMicros != 95_000 || req.UsageStatus != domain.UsageReported {
		t.Errorf("request = %v/%s, want 95000/reported", req.SettledMicros, req.UsageStatus)
	}
	recs, err := s.LoadUsageRecords(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Revision != 1 || recs[1].Revision != 2 ||
		*recs[0].Buckets.InputTokens != 10 || *recs[1].Buckets.InputTokens != 30 {
		t.Errorf("usage revisions = %+v, want rev1(10 in) preserved + rev2(30 in)", recs)
	}

	// 重建核对：窗口 used ≡ charge − reversal + adjustment = 95_000。
	recon, err := s.ReconcileWindowAggregates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recon {
		if r.Diff() != 0 {
			t.Errorf("window %s drift after correction: stored=%d rebuilt=%d", r.WindowID, r.StoredUsed, r.RebuiltUsed)
		}
	}
}

func TestCorrectSettlementCreditAndIdempotency(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	reqID, attID := settledRequest(t, s, f, w5, ww, wm, 100_000, 60_000)
	key := "corr-" + uuid.NewString()
	correct := func() error {
		uow, _ := s.Begin(ctx)
		err := s.CorrectSettlement(ctx, uow, CorrectionCommand{
			RequestID: reqID,
			Corrected: domain.UsageRecord{
				AttemptID: attID, Source: domain.UsageReported,
				Buckets: domain.UsageBuckets{InputTokens: ptr(int64(8)), OutputTokens: ptr(int64(3))},
			},
			CorrectedMicros: 25_000, Reason: "evidence below estimate",
			OperatorSubject: "system:recovery", IdempotencyKey: key,
			At: time.Now().UTC(),
		})
		if err != nil {
			_ = uow.Rollback(ctx)
			return err
		}
		return uow.Commit(ctx)
	}
	if err := correct(); err != nil {
		t.Fatalf("credit correction: %v", err)
	}
	// 退还：窗口/预算下降，不隐藏负差额（客户少收 35k）。
	for _, id := range []string{w5, ww, wm} {
		if used, _ := windowState(t, s, id); used != 25_000 {
			t.Errorf("window %s used = %d, want 25000 after credit", id, used)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 25_000 {
		t.Errorf("key budget = %d, want 25000", got)
	}
	// 幂等键重放：撞唯一键 → CodeConflict，账本不重复生效。
	if err := correct(); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replay: err = %v, want CodeConflict", err)
	}
	var n int
	if err := s.db.Get(&n,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("entries = %d, want 2 (replay applied once)", n)
	}
	// 无操作修正拒绝（相等金额不追加空历史）——delta 以当前已结算额为基准。
	uow, _ := s.Begin(ctx)
	err := s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(8))},
		},
		CorrectedMicros: 25_000, Reason: "noop",
		OperatorSubject: "op", IdempotencyKey: "corr-noop-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("noop correction: err = %v, want invalid_input", err)
	}

	// 链式修正到 0 = 全额作废 → 冲正分录（reversal 当前剩余效应 25k，
	// 指向原 charge）；窗口/预算归零；重建口径 charge − credit − reversal。
	uow, _ = s.Begin(ctx)
	err = s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(0)), OutputTokens: ptr(int64(0))},
		},
		CorrectedMicros: 0, Reason: "provider confirmed zero consumption",
		OperatorSubject: "system:recovery", IdempotencyKey: "corr-void-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("void correction: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var revAmount int64
	if err := s.db.Get(&revAmount,
		`SELECT amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'reversal'`, reqID); err != nil {
		t.Fatalf("reversal missing (全额作废须落冲正分录): %v", err)
	}
	if revAmount != 25_000 {
		t.Errorf("reversal = %d, want 25000 (当前剩余效应)", revAmount)
	}
	for _, id := range []string{w5, ww, wm} {
		if used, _ := windowState(t, s, id); used != 0 {
			t.Errorf("window %s used = %d, want 0 after void", id, used)
		}
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget = %d, want 0 after void", got)
	}
	req, err := s.GetRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if *req.SettledMicros != 0 {
		t.Errorf("settled = %d, want 0 after void", *req.SettledMicros)
	}
	recon, err := s.ReconcileWindowAggregates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recon {
		if r.Diff() != 0 {
			t.Errorf("window %s drift after void: stored=%d rebuilt=%d (60k − 35k credit − 25k reversal = 0)",
				r.WindowID, r.StoredUsed, r.RebuiltUsed)
		}
	}
}

func TestCorrectSettlementOverageAndJobResolution(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 请求先被保守恢复结算（= 预占额 100k），留下 crash_recovery 任务。
	reqID, attID := settledRequest(t, s, f, w5, ww, wm, 100_000, 100_000)
	deadline := time.Now().UTC().Add(24 * time.Hour)
	uow, _ := s.Begin(ctx)
	if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
		RequestID: &reqID, Reason: "crash_recovery",
		Detail: json.RawMessage(`{"schema_version":1}`), Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 真实证据超出预占：120k > 100k → 补差 20k + settlement_overage 异常
	// （不隐藏负差额）。原 crash_recovery 任务先被证据 resolve，随后被新
	// reason 的异常重开为 pending（detail 保留全部历史）。
	uow, _ = s.Begin(ctx)
	err := s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(50)), OutputTokens: ptr(int64(50))},
		},
		CorrectedMicros: 120_000, Reason: "invoice exceeds hold",
		OperatorSubject: "system:recovery", IdempotencyKey: "corr-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var jobReason, jobStatus, jobDetail string
	if err := s.db.QueryRow(
		`SELECT reason, status, detail::text FROM inference_reconciliation_jobs WHERE request_id = $1`,
		reqID).Scan(&jobReason, &jobStatus, &jobDetail); err != nil {
		t.Fatal(err)
	}
	if jobReason != "settlement_overage" || jobStatus != "pending" {
		t.Errorf("job = %s/%s, want settlement_overage/pending (新异常重开)", jobReason, jobStatus)
	}
	if !strings.Contains(jobDetail, "resolution") || !strings.Contains(jobDetail, "over_micros") {
		t.Errorf("detail = %s, want merged history (resolution + overage)", jobDetail)
	}
	for _, id := range []string{w5, ww, wm} {
		if used, _ := windowState(t, s, id); used != 120_000 {
			t.Errorf("window %s used = %d, want 120000 (真实超占入账，不钳制)", id, used)
		}
	}
}

func TestReleaseFromReconciliationResolvesJob(t *testing.T) {
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
	uow, _ = s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow, adm.RequestID, "unknown_usage", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 普通 Release 对停放请求依然拒绝（禁止仅凭未知状态释放）。
	uow, _ = s.Begin(ctx)
	err = s.Release(ctx, uow, adm.RequestID)
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("plain release of parked request: err = %v, want conflict", err)
	}
	// 证据驱动的恢复释放：放行 + 任务同事务 resolve。
	uow, _ = s.Begin(ctx)
	if err := s.ReleaseFromReconciliation(ctx, uow, adm.RequestID); err != nil {
		t.Fatalf("release from reconciliation: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != domain.ReqReleased {
		t.Errorf("status = %s, want released", req.Status)
	}
	var jobStatus string
	if err := s.db.Get(&jobStatus,
		`SELECT status FROM inference_reconciliation_jobs WHERE request_id = $1`, adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "resolved" {
		t.Errorf("job = %s, want resolved (同一事务)", jobStatus)
	}
	if _, reserved := windowState(t, s, w5); reserved != 0 {
		t.Errorf("five-hour reserved = %d, want 0", reserved)
	}
}

func TestReconcileWindowAggregatesFindsDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	settledRequest(t, s, f, w5, ww, wm, 100_000, 60_000)
	recon, err := s.ReconcileWindowAggregates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recon) != 3 {
		t.Fatalf("recon windows = %d, want 3", len(recon))
	}
	for _, r := range recon {
		if r.StoredUsed != 60_000 || r.RebuiltUsed != 60_000 || r.Diff() != 0 {
			t.Errorf("window %s = %+v, want stored=rebuilt=60000 (由账本重建与窗口一致)", r.WindowID, r)
		}
	}
	// 注入漂移（绕过账本的非法写）→ 核对必须发现。
	if _, err := s.db.Exec(`UPDATE inference_quota_windows SET used_micros = used_micros - 1 WHERE id = $1`, w5); err != nil {
		t.Fatal(err)
	}
	recon, err = s.ReconcileWindowAggregates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var drift *int64
	for _, r := range recon {
		if r.WindowID == w5 {
			d := r.Diff()
			drift = &d
		} else if r.Diff() != 0 {
			t.Errorf("untouched window %s drifted: %+v", r.WindowID, r)
		}
	}
	if drift == nil || *drift != 1 {
		t.Errorf("drift = %v, want +1 (rebuilt − stored)", drift)
	}
}

func TestRecoveryMetricsSnapshot(t *testing.T) {
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
	// 老化预占与请求。
	if _, err := s.db.Exec(`UPDATE inference_reservations SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inference_requests SET updated_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	uow, _ = s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow, adm.RequestID, "unknown_usage", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	m, err := s.RecoveryMetrics(ctx, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if m.ReconciliationBacklog != 1 || m.DanglingHolds != 4 {
		t.Errorf("metrics = %+v, want backlog=1 dangling_holds=4 (三窗口+Key)", m)
	}
	if m.UnknownUsageRequests != 1 {
		t.Errorf("unknown usage = %d, want 1 (parked 请求如实标记 unknown)", m.UnknownUsageRequests)
	}
	if m.OldestOpenUpdatedAt != nil {
		// reconciliation_required 不在 open 扫描集（由任务驱动）。
		t.Errorf("oldest open = %v, want nil (parked 请求不算 open)", m.OldestOpenUpdatedAt)
	}
}

func ptr(v int64) *int64 { return &v }

// 审查修复 Important 1：修正链上超占异常必须持续可见。
// 链：网关超占结算落 settlement_overage(pending) → CorrectSettlement 仍超占
// → 步骤 8 不得误 resolve 超占任务（只 resolve 证据类任务）；修正回到预占
// 之内时才允许 resolve 超占任务。
func TestCorrectSettlementOverageStaysVisible(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 超占结算：hold 100k，实际 120k（网关路径同事务落超占任务——此处直接
	// 复现该终态）。
	reqID, attID := settledRequest(t, s, f, w5, ww, wm, 100_000, 120_000)
	deadline := time.Now().UTC().Add(24 * time.Hour)
	uow, _ := s.Begin(ctx)
	if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
		RequestID: &reqID, Reason: "settlement_overage",
		Detail:   json.RawMessage(`{"schema_version":1,"over_micros":20000}`),
		Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 修正后仍超占（130k > 100k）：超占任务必须仍然 pending 且 detail 合并
	// （修复前：步骤 8 误 resolve + 同 reason 不重开 → 异常静默消失）。
	uow, _ = s.Begin(ctx)
	err := s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(60)), OutputTokens: ptr(int64(60))},
		},
		CorrectedMicros: 130_000, Reason: "invoice higher still",
		OperatorSubject: "system:recovery", IdempotencyKey: "corr-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var status, detail string
	if err := s.db.QueryRow(
		`SELECT status, detail::text FROM inference_reconciliation_jobs WHERE request_id = $1`,
		reqID).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("overage job = %s, want pending (修正后仍超占,异常必须可见)", status)
	}
	if !strings.Contains(detail, "130000") {
		t.Errorf("detail = %s, want merged latest charge", detail)
	}

	// 修正回到预占之内（90k ≤ 100k）：异常被本次修正处置 → resolve 带说明。
	uow, _ = s.Begin(ctx)
	err = s.CorrectSettlement(ctx, uow, CorrectionCommand{
		RequestID: reqID,
		Corrected: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: ptr(int64(40)), OutputTokens: ptr(int64(40))},
		},
		CorrectedMicros: 90_000, Reason: "final invoice within hold",
		OperatorSubject: "system:recovery", IdempotencyKey: "corr-" + uuid.NewString(),
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("correct within hold: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(
		`SELECT status, detail::text FROM inference_reconciliation_jobs WHERE request_id = $1`,
		reqID).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" || !strings.Contains(detail, "within the hold") {
		t.Errorf("overage job = %s detail=%s, want resolved with note", status, detail)
	}
}
