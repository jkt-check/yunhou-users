package workers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/migrate"
)

// settlement_recovery_test.go — Task 9 验收硬项：五个故障注入点（预占后 /
// 发送后 / 流尾后 / 写账前 / 提交后但响应前）的崩溃恢复实测。每个点用真实
// 库手工构造"进程在该点死掉"的持久化状态，再用一个全新的 worker 实例跑恢
// 复 pass（模拟重启后的 recovery worker），断言：恢复或明确进入待核对，
// 没有静默免费、双扣或无审计释放，且账本重建聚合值与窗口一致。
//
// 测试需要真实 PostgreSQL（专用可丢弃实例）；skip 不算通过。

var testDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		os.Exit(m.Run())
	}
	migs, err := migrate.LoadFiles("../../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		os.Exit(1)
	}
	if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}
	testDSN = dsn
	db.Close()
	os.Exit(m.Run())
}

// workerFixture is the minimal reference chain for recovery tests.
type workerFixture struct {
	db         *sqlx.DB
	store      *postgres.Store
	accountID  string
	keyID      string
	entID      string
	modelID    string
	policyID   string
	w5, ww, wm string
	// clock 固定在两小时后：让"刚写入的行"对 worker 显得足够陈旧（grace
	//  cutoff），无需 UPDATE 老化。
	clock domain.FixedClock
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`TRUNCATE
		inference_reconciliation_jobs, inference_outbox,
		inference_ledger_entries, inference_adjustments,
		inference_concurrency_leases, inference_reservations,
		inference_quota_windows, inference_usage_records,
		inference_attempts, inference_requests,
		inference_entitlements, inference_policy_versions,
		inference_price_versions,
		inference_api_keys, inference_billing_accounts,
		inference_upstream_accounts, inference_credentials,
		inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models,
		users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	ctx := context.Background()
	s := postgres.NewStore(db)
	f := &workerFixture{db: db, store: s, modelID: "glm-4.6",
		clock: domain.FixedClock{T: time.Now().UTC().Add(2 * time.Hour)}}

	userID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	acct := &domain.BillingAccount{UserID: userID}
	if err := s.InsertBillingAccount(ctx, acct); err != nil {
		t.Fatalf("account: %v", err)
	}
	f.accountID = acct.ID
	if err := s.InsertModel(ctx, &domain.Model{
		ID: f.modelID, DisplayName: "GLM", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: microP(1_000_000), WeeklyLimit: microP(10_000_000),
		MonthlyLimit: microP(100_000_000),
	}
	if err := s.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	f.policyID = pol.ID
	now := time.Now().UTC().Add(-time.Minute)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := s.InsertEntitlement(ctx, ent); err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	f.entID = ent.ID
	key := &domain.APIKey{
		BillingAccountID: acct.ID, Name: "cli", Prefix: "yk-t9-" + uuid.NewString()[:8],
		BudgetLimit: microP(5_000_000),
	}
	if err := s.InsertAPIKey(ctx, key, "sha256:"+uuid.NewString()); err != nil {
		t.Fatalf("key: %v", err)
	}
	f.keyID = key.ID

	// 上游成本价目：恢复结算的成本来源（cost_basis=estimated 断言用）。
	cost := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceUpstreamCost, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 500_000, OutputPerMtok: 1_000_000,
		Revision: 1, EffectiveFrom: now,
	}
	if err := s.InsertPriceVersion(ctx, cost); err != nil {
		t.Fatalf("cost price: %v", err)
	}

	start := time.Now().UTC().Add(-time.Minute)
	for _, sp := range []struct {
		kind  domain.WindowKind
		end   time.Time
		limit domain.Microcredit
	}{
		{domain.WindowFiveHour, start.Add(5 * time.Hour), 1_000_000},
		{domain.WindowWeekly, start.Add(7 * 24 * time.Hour), 10_000_000},
		{domain.WindowMonthly, start.AddDate(0, 1, 0), 100_000_000},
	} {
		w := &domain.QuotaWindow{EntitlementID: ent.ID, Kind: sp.kind, Start: start, End: sp.end, Limit: sp.limit}
		if err := s.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatalf("window: %v", err)
		}
		switch sp.kind {
		case domain.WindowFiveHour:
			f.w5 = w.ID
		case domain.WindowWeekly:
			f.ww = w.ID
		case domain.WindowMonthly:
			f.wm = w.ID
		}
	}
	return f
}

func microP(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

// fabricateReserved commits a REAL reservation (hold=50_000, admission
// bounds persisted) — the shared starting point of every fault point.
func (f *workerFixture) fabricateReserved(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	in, out := int64(120), int64(4096)
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := f.store.Reserve(ctx, uow, domain.ReserveCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
			PolicyVersionID:  f.policyID,
			InputBoundTokens: &in, OutputCapTokens: &out,
		},
		AdmittedAt: time.Now().UTC().Add(-time.Minute),
		Holds: []domain.HoldSpec{
			{TargetKind: domain.TargetWindowFiveHour, WindowID: &f.w5, Amount: 50_000},
			{TargetKind: domain.TargetWindowWeekly, WindowID: &f.ww, Amount: 50_000},
			{TargetKind: domain.TargetWindowMonthly, WindowID: &f.wm, Amount: 50_000},
			{TargetKind: domain.TargetKeyBudget, APIKeyID: &f.keyID, Amount: 50_000},
		},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

// fabricateAttempt adds one attempt row in the given status (发送前落库的
// 意图行；崩溃时点由 status/finished_at 表达）。
func (f *workerFixture) fabricateAttempt(t *testing.T, requestID, status, errorKind string, finished bool) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	now := time.Now().UTC().Add(-time.Minute)
	a := &domain.Attempt{
		ID: id, RequestID: requestID, AttemptNo: 1, Status: "dispatching", StartedAt: &now,
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertAttemptTx(ctx, uow, a); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if status != "dispatching" || finished {
		fin := time.Now().UTC().Add(-30 * time.Second)
		var err error
		if finished {
			err = f.store.FinishAttempt(ctx, id, status, errorKind, "upstream-req-1", fin)
		} else {
			err = f.store.FinishAttempt(ctx, id, status, errorKind, "", fin)
		}
		if err != nil {
			t.Fatalf("finish attempt: %v", err)
		}
	}
	return id
}

// newWorker builds the recovery worker as a FRESH instance (重启后的进程):
// fixed clock two hours ahead so every fabricated row is stale past grace.
func (f *workerFixture) newWorker() *SettlementRecovery {
	return NewSettlementRecovery(f.store, f.clock, RecoveryConfig{
		Interval: time.Second, BatchLimit: 50,
		Grace: time.Hour, ReconciliationDeadline: 24 * time.Hour,
	}, nil)
}

func (f *workerFixture) requestState(t *testing.T, id string) (status string, usage string, reserved, settled int64) {
	t.Helper()
	var res, set *int64
	if err := f.db.QueryRow(
		`SELECT status, usage_status, reserved_micros, settled_micros FROM inference_requests WHERE id = $1`, id).
		Scan(&status, &usage, &res, &set); err != nil {
		t.Fatal(err)
	}
	if res != nil {
		reserved = *res
	}
	if set != nil {
		settled = *set
	}
	return
}

func (f *workerFixture) windowUsedReserved(t *testing.T, id string) (used, reserved int64) {
	t.Helper()
	if err := f.db.QueryRow(
		`SELECT used_micros, reserved_micros FROM inference_quota_windows WHERE id = $1`, id).
		Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *workerFixture) ledgerEntries(t *testing.T, requestID string) (charges int, total int64) {
	t.Helper()
	var sum *int64
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FILTER (WHERE entry_type='charge'),
		        SUM(CASE WHEN entry_type='charge' THEN amount_micros
		                 WHEN entry_type='reversal' THEN -amount_micros END)
		 FROM inference_ledger_entries WHERE request_id = $1`, requestID).Scan(&charges, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != nil {
		total = *sum
	}
	return
}

// assertLedgerMatchesWindows 验收：由账本重建聚合值与窗口核对一致。
func (f *workerFixture) assertLedgerMatchesWindows(t *testing.T) {
	t.Helper()
	recon, err := f.store.ReconcileWindowAggregates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recon {
		if r.Diff() != 0 {
			t.Errorf("window %s(%s): stored=%d rebuilt=%d — 账本/窗口不一致",
				r.WindowID, r.Kind, r.StoredUsed, r.RebuiltUsed)
		}
	}
}

func (f *workerFixture) openJob(t *testing.T, requestID string) (reason, status string, found bool) {
	t.Helper()
	err := f.db.QueryRow(
		`SELECT reason, status FROM inference_reconciliation_jobs WHERE request_id = $1`, requestID).
		Scan(&reason, &status)
	if err != nil {
		return "", "", false
	}
	return reason, status, true
}

// ---------------------------------------------------------------------------
// 故障注入点 1：预占后（reservation committed, no attempt intent）
// ---------------------------------------------------------------------------

func TestRecover_AfterReservation_NotSent_Releases(t *testing.T) {
	f := newWorkerFixture(t)
	reqID := f.fabricateReserved(t)
	// 进程死于预占提交之后、attempt 落库之前：绝无上游发送（attempt 意图
	// 先于发送持久化）→ 确认零消费 → 释放（审计在册：reservation 行
	// released、请求 released、五小时窗口若无他用则撤销）。

	stats, err := f.newWorker().RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReleasedNotSent != 1 || stats.SettledConservative != 0 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want one released-not-sent", stats)
	}
	status, usage, _, _ := f.requestState(t, reqID)
	if status != "released" || usage == "unknown" {
		t.Errorf("request = %s/%s, want released (非 unknown)", status, usage)
	}
	for _, w := range []string{f.w5, f.ww, f.wm} {
		used, reserved := f.windowUsedReserved(t, w)
		if used != 0 || reserved != 0 {
			t.Errorf("window %s = %d/%d, want 0/0 (无消费不占额度)", w, used, reserved)
		}
	}
	charges, _ := f.ledgerEntries(t, reqID)
	if charges != 0 {
		t.Errorf("charges = %d, want 0 (确认零消费不入账)", charges)
	}
	// 全部预占释放且窗口零消费 → 五小时窗口事务内撤销。
	var w5state string
	if err := f.db.Get(&w5state, `SELECT state FROM inference_quota_windows WHERE id = $1`, f.w5); err != nil {
		t.Fatal(err)
	}
	if w5state != "voided" {
		t.Errorf("five-hour window = %s, want voided (未使用窗口安全撤销)", w5state)
	}
	var holds int
	if err := f.db.Get(&holds,
		`SELECT COUNT(*) FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Errorf("held reservations = %d, want 0 (释放有审计：行在、状态 released)", holds)
	}
	f.assertLedgerMatchesWindows(t)
}

// ---------------------------------------------------------------------------
// 故障注入点 2：发送后（attempt intent committed, dispatch outcome unknown）
// ---------------------------------------------------------------------------

func TestRecover_AfterDispatch_PossiblySent_ConservativeSettle(t *testing.T) {
	f := newWorkerFixture(t)
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "dispatching", "", false)
	// 网关发送前会把请求标记 dispatching（Task 8）。
	if err := f.store.UpdateRequestStatus(context.Background(), reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}
	// 进程死于此处：请求可能已发出、结果未知 → 禁止释放/记零/重试；
	// 保守估算 = 预占额全额入账 + 核对任务保留证据窗口。

	stats, err := f.newWorker().RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want one conservative settle", stats)
	}
	status, usage, reserved, settled := f.requestState(t, reqID)
	if status != "settled" || usage != "estimated" {
		t.Errorf("request = %s/%s, want settled/estimated", status, usage)
	}
	if reserved != 50_000 || settled != 50_000 {
		t.Errorf("reserved/settled = %d/%d, want 50000/50000 (保守估算=预占额，不静默免费)", reserved, settled)
	}
	for _, w := range []string{f.w5, f.ww, f.wm} {
		used, res := f.windowUsedReserved(t, w)
		if used != 50_000 || res != 0 {
			t.Errorf("window %s = %d/%d, want 50000/0 (reserved→used 转换)", w, used, res)
		}
	}
	charges, total := f.ledgerEntries(t, reqID)
	if charges != 1 || total != 50_000 {
		t.Errorf("ledger = %d charges/%d, want 1/50000", charges, total)
	}
	// usage fact: estimated with the admission bounds, audit basis in raw.
	recs, err := f.store.LoadUsageRecords(context.Background(), reqID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("usage records: %v/%d, want 1", err, len(recs))
	}
	if recs[0].Source != domain.UsageEstimated || recs[0].AttemptID != attID ||
		recs[0].Buckets.InputTokens == nil || *recs[0].Buckets.InputTokens != 120 {
		t.Errorf("usage = %+v, want estimated on bounds (input 120)", recs[0])
	}
	// 核对任务保留（crash_recovery，证据窗口 24h）——可冲正，不静默结案。
	reason, jstatus, found := f.openJob(t, reqID)
	if !found || reason != "crash_recovery" || jstatus != "pending" {
		t.Errorf("job = %s/%s/%v, want crash_recovery/pending", reason, jstatus, found)
	}
	// 成本可追溯：存在 upstream_cost 价目 → estimated 成本入账。
	var basis *string
	if err := f.db.QueryRow(`SELECT cost_basis FROM inference_attempts WHERE id = $1`, attID).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if basis == nil || *basis != "estimated" {
		t.Errorf("cost basis = %v, want estimated", basis)
	}
	f.assertLedgerMatchesWindows(t)

	// 幂等：第二个 pass（重复投递）不再产生任何效果。
	stats, err = f.newWorker().RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 0 || stats.ReleasedNotSent != 0 {
		t.Errorf("second pass = %+v, want no effect (幂等)", stats)
	}
	charges, _ = f.ledgerEntries(t, reqID)
	if charges != 1 {
		t.Errorf("charges after second pass = %d, want 1 (无双扣)", charges)
	}
}

// ---------------------------------------------------------------------------
// 故障注入点 3：流尾后（stream finished upstream, usage lost with process）
// ---------------------------------------------------------------------------

func TestRecover_AfterStreamEnd_KnownResultUnknownCharge(t *testing.T) {
	f := newWorkerFixture(t)
	reqID := f.fabricateReserved(t)
	f.fabricateAttempt(t, reqID, "completed", "", true)
	if err := f.store.UpdateRequestStatus(context.Background(), reqID, domain.ReqStreaming, ""); err != nil {
		t.Fatal(err)
	}
	// 已知结果（attempt completed）但用量随进程丢失 = 未知费用 → 保守估算。

	stats, err := f.newWorker().RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want one conservative settle", stats)
	}
	status, usage, _, settled := f.requestState(t, reqID)
	if status != "settled" || usage != "estimated" || settled != 50_000 {
		t.Errorf("request = %s/%s/%d, want settled/estimated/50000", status, usage, settled)
	}
	f.assertLedgerMatchesWindows(t)
}

// ---------------------------------------------------------------------------
// 故障注入点 4：写账前（settlement tx 原子 — 崩溃即整体回滚，无半账）
// ---------------------------------------------------------------------------

func TestRecover_BeforeLedgerWrite_TxAtomicityThenRecover(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "completed", "", true)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqNonStreaming, ""); err != nil {
		t.Fatal(err)
	}

	// 模拟"写账中崩溃"：开启结算事务、执行 Settle、然后回滚（进程死亡 =
	// 事务整体消失）。库中不得留有任何部分效果。
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	err = f.store.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 30_000, SettledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("settle in doomed tx: %v", err)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	charges, _ := f.ledgerEntries(t, reqID)
	if charges != 0 {
		t.Fatalf("rolled-back settlement left %d charges (结算事务必须原子)", charges)
	}
	status, _, _, _ := f.requestState(t, reqID)
	if status != "non_streaming" {
		t.Fatalf("request = %s after rollback, want non_streaming (无半账)", status)
	}

	// 重启恢复：已读用量随进程丢失 → 保守估算。
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 {
		t.Errorf("stats = %+v, want one conservative settle", stats)
	}
	_, usage, _, settled := f.requestState(t, reqID)
	if usage != "estimated" || settled != 50_000 {
		t.Errorf("recovered = %s/%d, want estimated/50000", usage, settled)
	}
	f.assertLedgerMatchesWindows(t)
}

// ---------------------------------------------------------------------------
// 故障注入点 5：提交后但响应前（settlement committed — 恢复不得双扣）
// ---------------------------------------------------------------------------

func TestRecover_AfterCommitBeforeResponse_NoDoubleCharge(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "completed", "", true)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqNonStreaming, ""); err != nil {
		t.Fatal(err)
	}

	// 结算已提交（真实用量 30_000），响应未送达客户端进程即死。
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	err = f.store.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 30_000, SettledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 0 || stats.ReleasedNotSent != 0 || stats.RacingSkipped != 0 {
		t.Errorf("stats = %+v, want untouched (终态不在扫描集)", stats)
	}
	charges, total := f.ledgerEntries(t, reqID)
	if charges != 1 || total != 30_000 {
		t.Errorf("ledger = %d/%d, want 1/30000 (无双扣)", charges, total)
	}
	status, usage, _, settled := f.requestState(t, reqID)
	if status != "settled" || usage != "reported" || settled != 30_000 {
		t.Errorf("request = %s/%s/%d, want settled/reported/30000 (原结算不被改写)", status, usage, settled)
	}
	for _, w := range []string{f.w5, f.ww, f.wm} {
		used, res := f.windowUsedReserved(t, w)
		if used != 30_000 || res != 0 {
			t.Errorf("window %s = %d/%d, want 30000/0", w, used, res)
		}
	}
	f.assertLedgerMatchesWindows(t)
}

// ---------------------------------------------------------------------------
// 停放请求（gateway 已 MarkReconciliationRequired）由任务队列驱动恢复
// ---------------------------------------------------------------------------

func TestRecover_ParkedUnknownUsage_SettlesViaJob(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	f.fabricateAttempt(t, reqID, "failed", "stream_interrupted", true)
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkReconciliationRequired(ctx, uow, reqID, "unknown_usage",
		f.clock.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 滞留扫描不管它（reconciliation_required 不在 open 集）——任务队列驱动。
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.JobsSettled != 1 || stats.SettledConservative != 1 {
		t.Errorf("stats = %+v, want job-driven conservative settle", stats)
	}
	status, usage, _, settled := f.requestState(t, reqID)
	if status != "settled" || usage != "estimated" || settled != 50_000 {
		t.Errorf("request = %s/%s/%d, want settled/estimated/50000", status, usage, settled)
	}
	reason, jstatus, found := f.openJob(t, reqID)
	if !found || jstatus != "pending" {
		t.Errorf("job = %s/%s/%v, want still pending (证据窗口保留)", reason, jstatus, found)
	}
	f.assertLedgerMatchesWindows(t)
}

// 停放期间拿到零消费证明（全部尝试确认未执行）→ 证据驱动释放 + 任务
// 同事务 resolve —— 不是 TTL 释放。
func TestRecover_ParkedAllAttemptsFailedZero_ReleasesWithEvidence(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	f.fabricateAttempt(t, reqID, "failed", "http_400", true)
	f.fabricateAttempt2(t, reqID, "failed", "transport", true)
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkReconciliationRequired(ctx, uow, reqID, "unknown_usage",
		f.clock.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReleasedNotSent != 1 || stats.SettledConservative != 0 {
		t.Errorf("stats = %+v, want evidence-driven release", stats)
	}
	status, _, _, _ := f.requestState(t, reqID)
	if status != "released" {
		t.Errorf("status = %s, want released", status)
	}
	_, jstatus, found := f.openJob(t, reqID)
	if !found || jstatus != "resolved" {
		t.Errorf("job = %s/%v, want resolved", jstatus, found)
	}
	charges, _ := f.ledgerEntries(t, reqID)
	if charges != 0 {
		t.Errorf("charges = %d, want 0", charges)
	}
	f.assertLedgerMatchesWindows(t)
}

// fabricateAttempt2 adds a second attempt row (attempt_no=2).
func (f *workerFixture) fabricateAttempt2(t *testing.T, requestID, status, errorKind string, finished bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	a := &domain.Attempt{ID: uuid.NewString(), RequestID: requestID, AttemptNo: 2, Status: "dispatching", StartedAt: &now}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertAttemptTx(ctx, uow, a); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.FinishAttempt(ctx, a.ID, status, errorKind, "", time.Now().UTC().Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 期限告警：证据窗口已过仍无证据 → 升级叫人，绝不自动记零/释放
// ---------------------------------------------------------------------------

func TestDeadlineEscalationNeverZeroes(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "unknown", "transport", true)
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}

	// Pass 1：保守估算结算 + crash_recovery 任务（证据窗口 24h）。
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 {
		t.Fatalf("pass 1 = %+v, want conservative settle", stats)
	}
	charges, total := f.ledgerEntries(t, reqID)
	if charges != 1 || total != 50_000 {
		t.Fatalf("ledger = %d/%d, want 1/50000", charges, total)
	}

	// 证据窗口到期仍无真实证据（直接把任务期限改到过去，模拟 24h 后）。
	if _, err := f.db.Exec(
		`UPDATE inference_reconciliation_jobs SET deadline_at = $1 WHERE request_id = $2`,
		f.clock.Now().Add(-time.Minute), reqID); err != nil {
		t.Fatal(err)
	}
	stats, err = f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Escalated != 1 {
		t.Errorf("escalated = %d, want 1 (期限告警)", stats.Escalated)
	}
	_, jstatus, found := f.openJob(t, reqID)
	if !found || jstatus != "escalated" {
		t.Errorf("job = %s/%v, want escalated", jstatus, found)
	}
	// 关键不变式：升级只叫人 — 已结算的保守估算分文不动，无双扣、
	// 无静默免费、无 TTL 释放。
	charges, total = f.ledgerEntries(t, reqID)
	if charges != 1 || total != 50_000 {
		t.Errorf("ledger after escalation = %d/%d, want 1/50000 (禁止 TTL 到期视为零消费)", charges, total)
	}
	for _, w := range []string{f.w5, f.ww, f.wm} {
		used, res := f.windowUsedReserved(t, w)
		if used != 50_000 || res != 0 {
			t.Errorf("window %s = %d/%d after escalation, want 50000/0", w, used, res)
		}
	}
	_ = attID
	f.assertLedgerMatchesWindows(t)
}

// ---------------------------------------------------------------------------
// 供应商能力例外：逐笔上游核对成功 → 按真实证据结算（不是预占额）
// ---------------------------------------------------------------------------

type fakeVerifier struct {
	record domain.UsageRecord
	charge domain.Microcredit
	ok     bool
	calls  int
}

func (v *fakeVerifier) LookupUsage(ctx context.Context, attempt domain.Attempt) (domain.UsageRecord, domain.Microcredit, bool, error) {
	v.calls++
	return v.record, v.charge, v.ok, nil
}

func TestRecover_VerifierExceptionSettlesRealEvidence(t *testing.T) {
	f := newWorkerFixture(t)
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "unknown", "transport", true)
	// 核对需要上游请求 ID。
	if _, err := f.db.Exec(`UPDATE inference_attempts SET upstream_request_id = 'req-up-1' WHERE id = $1`, attID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateRequestStatus(context.Background(), reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}
	in, out := int64(700), int64(100)
	verifier := &fakeVerifier{
		record: domain.UsageRecord{
			AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		charge: 12_345, ok: true,
	}
	w := NewSettlementRecovery(f.store, f.clock, RecoveryConfig{
		BatchLimit: 50, Grace: time.Hour, ReconciliationDeadline: 24 * time.Hour,
	}, verifier)
	stats, err := w.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledVerified != 1 || verifier.calls != 1 {
		t.Errorf("stats = %+v calls = %d, want verified settle", stats, verifier.calls)
	}
	_, usage, _, settled := f.requestState(t, reqID)
	if usage != "reported" || settled != 12_345 {
		t.Errorf("request = %s/%d, want reported/12345 (真实证据而非预占额)", usage, settled)
	}
	// 核对成功 = 证据已落地，不再开 crash_recovery 证据窗口任务。
	if _, _, found := f.openJob(t, reqID); found {
		t.Error("verified settle must not enqueue a crash_recovery job")
	}
	// 成本来源随证据：reported。
	var basis *string
	if err := f.db.QueryRow(`SELECT cost_basis FROM inference_attempts WHERE id = $1`, attID).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if basis == nil || *basis != "reported" {
		t.Errorf("cost basis = %v, want reported", basis)
	}
	f.assertLedgerMatchesWindows(t)
}

// 供应商不支持按请求查询（ok=false）→ 调用后回落保守估算（主流上游现状）。
func TestRecover_VerifierUnsupportedFallsBack(t *testing.T) {
	f := newWorkerFixture(t)
	reqID := f.fabricateReserved(t)
	f.fabricateAttempt(t, reqID, "unknown", "transport", true)
	if err := f.store.UpdateRequestStatus(context.Background(), reqID, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}
	verifier := &fakeVerifier{ok: false}
	w := NewSettlementRecovery(f.store, f.clock, RecoveryConfig{
		BatchLimit: 50, Grace: time.Hour, ReconciliationDeadline: 24 * time.Hour,
	}, verifier)
	stats, err := w.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 {
		t.Errorf("stats = %+v, want conservative fallback", stats)
	}
	// 供应商回答了"查不了"→ 例外路径尝试过一次后回落。
	if verifier.calls != 1 {
		t.Errorf("verifier calls = %d, want 1 (尝试过例外路径)", verifier.calls)
	}
	_, _, _, settled := f.requestState(t, reqID)
	if settled != 50_000 {
		t.Errorf("settled = %d, want 50000", settled)
	}
}

// ---------------------------------------------------------------------------
// 窗口级账本差异 → ledger_mismatch 任务（不自动修复账本）
// ---------------------------------------------------------------------------

func TestWindowMismatchEnqueuesJob(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "completed", "", true)
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	if err := f.store.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 30_000, SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 注入漂移（绕过账本的非法写）。
	if _, err := f.db.Exec(`UPDATE inference_quota_windows SET used_micros = used_micros + 7 WHERE id = $1`, f.w5); err != nil {
		t.Fatal(err)
	}
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowMismatches != 1 {
		t.Errorf("mismatches = %d, want 1 (只有被注入的五小时窗口)", stats.WindowMismatches)
	}
	var reason, status, detail string
	if err := f.db.QueryRow(
		`SELECT reason, status, detail::text FROM inference_reconciliation_jobs WHERE request_id IS NULL`).
		Scan(&reason, &status, &detail); err != nil {
		t.Fatalf("window mismatch job missing: %v", err)
	}
	if reason != "ledger_mismatch" || status != "pending" {
		t.Errorf("job = %s/%s, want ledger_mismatch/pending", reason, status)
	}
	// 差异只入队告警，账本/窗口都不自动改写。
	used, _ := f.windowUsedReserved(t, f.w5)
	if used != 30_007 {
		t.Errorf("window used = %d, want 30007 (差异不自动修复，待人工)", used)
	}
	// 第二个 pass：去重键挡住重复入队。
	stats, err = f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := f.db.Get(&cnt, `SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("window jobs = %d after second pass, want 1 (去重)", cnt)
	}
	if stats.WindowMismatches != 1 {
		t.Errorf("second pass mismatches = %d, want still detected (1)", stats.WindowMismatches)
	}
}

// ---------------------------------------------------------------------------
// 审查修复 Critical 1：活的长请求绝不被扫描误结算
// ---------------------------------------------------------------------------

// 一次活请求可在单一状态停留远超朴素 grace（部署 RequestTimeout 默认 10 分
// 钟、nginx SSE 700s）：status=streaming + updated_at 新鲜 + 在途 attempt。
// 多个 pass 后：不结算、不入账、预占原样 —— 流尾活路径的真实用量结算不会
// 被"撞 CodeConflict 吞掉"（多扣）。
func TestRecovery_LiveStreamingRequestNeverSwept(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	f.fabricateAttempt(t, reqID, "dispatching", "", false) // started_at = 近期
	if err := f.store.UpdateRequestStatus(ctx, reqID, domain.ReqStreaming, ""); err != nil {
		t.Fatal(err)
	}

	// 真实时钟 + 默认级 grace（15min 地板语义；测试用 15min 表达生产口径）。
	live := NewSettlementRecovery(f.store, nil, RecoveryConfig{
		BatchLimit: 50, Grace: 15 * time.Minute, ReconciliationDeadline: 24 * time.Hour,
	}, nil)
	for pass := 0; pass < 3; pass++ {
		stats, err := live.RunPass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SettledConservative != 0 || stats.ReleasedNotSent != 0 || stats.Scanned != 0 {
			t.Fatalf("pass %d swept a live request: %+v", pass, stats)
		}
	}
	status, usage, reserved, settled := f.requestState(t, reqID)
	if status != "streaming" || usage != "pending" || settled != 0 {
		t.Errorf("live request = %s/%s/%d, want streaming/pending/unsettled", status, usage, settled)
	}
	if reserved != 50_000 {
		t.Errorf("reserved = %d, want 50000 (预占原样保留)", reserved)
	}
	charges, _ := f.ledgerEntries(t, reqID)
	if charges != 0 {
		t.Errorf("charges = %d, want 0 (活请求绝不被保守结算)", charges)
	}

	// 即使 updated_at 被人为老化（例如某路径少了一次状态跃迁），只要存在
	// grace 窗口内启动的在途 attempt，扫描守卫依然跳过它。
	if _, err := f.db.Exec(
		`UPDATE inference_requests SET updated_at = now() - interval '30 minutes' WHERE id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	stats, err := live.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 0 || stats.SettledConservative != 0 {
		t.Errorf("aged-updated_at live request swept: %+v (attempt 存活守卫失效)", stats)
	}
	// 全部 attempt 也超过 grace（真正卡死）后才允许恢复介入。
	if _, err := f.db.Exec(
		`UPDATE inference_attempts SET started_at = now() - interval '30 minutes' WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	stats, err = live.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SettledConservative != 1 {
		t.Errorf("stats = %+v, want recovery once the request is provably stuck", stats)
	}
}

// ---------------------------------------------------------------------------
// 审查修复 Important 2：窗口漂移持续期间，任务按原 deadline 升级
// ---------------------------------------------------------------------------

func TestWindowMismatchEscalatesAtOriginalDeadline(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	attID := f.fabricateAttempt(t, reqID, "completed", "", true)
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	if err := f.store.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 30_000, SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE inference_quota_windows SET used_micros = used_micros + 7 WHERE id = $1`, f.w5); err != nil {
		t.Fatal(err)
	}

	// Pass 1 @ T：入队，deadline = T+24h。
	t0 := time.Now().UTC()
	w1 := NewSettlementRecovery(f.store, domain.FixedClock{T: t0}, RecoveryConfig{
		BatchLimit: 50, Grace: 15 * time.Minute, ReconciliationDeadline: 24 * time.Hour,
	}, nil)
	stats, err := w1.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowMismatches != 1 || stats.Escalated != 0 {
		t.Fatalf("pass1 = %+v, want 1 mismatch, 0 escalations", stats)
	}
	var deadline time.Time
	if err := f.db.Get(&deadline,
		`SELECT deadline_at FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if deadline.Before(t0.Add(23*time.Hour)) || deadline.After(t0.Add(25*time.Hour)) {
		t.Fatalf("deadline = %s, want ≈ T+24h", deadline)
	}

	// Pass 2..4 @ T+1h…T+23h：漂移持续存在，每轮 refresh —— 期限不得被推后。
	for _, offset := range []time.Duration{time.Hour, 12 * time.Hour, 23 * time.Hour} {
		w := NewSettlementRecovery(f.store, domain.FixedClock{T: t0.Add(offset)}, RecoveryConfig{
			BatchLimit: 50, Grace: 15 * time.Minute, ReconciliationDeadline: 24 * time.Hour,
		}, nil)
		stats, err := w.RunPass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Escalated != 0 {
			t.Fatalf("pass @+%v escalated early: %+v", offset, stats)
		}
		var d2 time.Time
		if err := f.db.Get(&d2,
			`SELECT deadline_at FROM inference_reconciliation_jobs WHERE request_id IS NULL`); err != nil {
			t.Fatal(err)
		}
		if !d2.Equal(deadline) {
			t.Fatalf("pass @+%v moved deadline %s → %s (期限告警失效)", offset, deadline, d2)
		}
	}

	// Pass @ T+25h（超过原 deadline）：升级命中。
	wLate := NewSettlementRecovery(f.store, domain.FixedClock{T: t0.Add(25 * time.Hour)}, RecoveryConfig{
		BatchLimit: 50, Grace: 15 * time.Minute, ReconciliationDeadline: 24 * time.Hour,
	}, nil)
	stats, err = wLate.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Escalated != 1 {
		t.Errorf("pass @+25h escalated = %d, want 1 (漂移持续 24h 后必须升级告警)", stats.Escalated)
	}
}
