package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

// quota_concurrency_test.go — Task 7 验收硬项：真实 PostgreSQL 上的多
// goroutine 与双服务进程并发额度闸门测试。不得以单进程加锁模拟代替跨实
// 例竞争：TestTwoProcessContendLastCredit 通过 re-exec 测试二进制拉起两
// 个真实 OS 进程（各自独立连接池）共享同一可丢弃库竞争最后额度。
//
// 运行（一次性实例）：
//   initdb -D /tmp/kaya-task7-pg -U postgres --auth=trust -E UTF8
//   pg_ctl -D /tmp/kaya-task7-pg -o "-k /tmp/kaya-task7-sock -p 55440" start
//   createdb -h /tmp/kaya-task7-sock -p 55440 -U postgres yunhou_task7
//   DATABASE_URL=postgres://postgres@localhost:55440/yunhou_task7?sslmode=disable \
//     go test -race -count=1 -p 1 ./internal/inference/postgres/ -run Concurrency

// ---------------------------------------------------------------------------
// service-level helpers
// ---------------------------------------------------------------------------

// seedPrice inserts the sale_credit price version the admissions pin.
// Rates: 1 microcredit per input token, output free — so the reservation
// hold equals EstimatedInputTokens micros (output cap contributes 0).
func seedPrice(t *testing.T, s *Store, modelID string) string {
	t.Helper()
	pv := &PriceVersion{
		ModelID: modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, Revision: 1,
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := s.InsertPriceVersion(context.Background(), pv); err != nil {
		t.Fatalf("insert price version: %v", err)
	}
	return pv.ID
}

// admitCmd builds the service admission for the fixture with the given
// hold (micros) and window limits. hold maps 1:1 to EstimatedInputTokens
// via the seeded 1-token=1-micro price.
func admitCmd(f fixture, priceID string, hold int64, five, weekly, monthly domain.Microcredit, conc *int) quota.AdmitCommand {
	policy := quota.Policy{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: &five, WeeklyLimit: &weekly, MonthlyLimit: &monthly,
		ConcurrencyLimit: conc,
	}
	pure, err := (&PriceVersion{
		ID: priceID, ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, Revision: 1,
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}).Pure()
	if err != nil {
		panic(err)
	}
	one := int64(1)
	return quota.AdmitCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
		},
		Entitlement: domain.Entitlement{
			ID: f.entID, BillingAccountID: f.accountID,
			AnchorAt:        time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC),
			PolicyVersionID: f.policyID,
		},
		Policy:               policy,
		CreditPrice:          pure,
		Model:                domain.Model{ID: f.modelID, ContextTokens: 200_000, MaxOutputTokens: 1},
		EstimatedInputTokens: hold,
		ClientMaxTokens:      &one,
	}
}

func newQuotaService(s *Store) *quota.Service {
	return quota.NewService(s, nil)
}

func windowSums(t *testing.T, db *sqlx.DB, entID string) (used, reserved int64) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(used_micros),0), COALESCE(SUM(reserved_micros),0)
		 FROM inference_quota_windows WHERE entitlement_id = $1 AND state = 'active'`, entID).
		Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	return used, reserved
}

func countRequests(t *testing.T, db *sqlx.DB, entID string, status string) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM inference_requests WHERE entitlement_id = $1`
	args := []interface{}{entID}
	if status != "" {
		q += ` AND status = $2`
		args = append(args, status)
	}
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 多 goroutine 竞争最后额度（真实库，单进程内）
// ---------------------------------------------------------------------------

func TestConcurrentAdmissionsNeverExceedLimit(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)

	const hold = 100_000
	const capacity = 3
	const demand = 24
	limit := domain.Microcredit(hold * capacity)
	big := domain.Microcredit(1 << 40) // 周/月窗口与 Key 预算不参与阻断

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, quotaExceeded, other := 0, 0, 0
	start := make(chan struct{})
	for i := 0; i < demand; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// 五小时窗口限 300k 是唯一瓶颈；Key 预算 5e6 足够。
			_, err := svc.Admit(context.Background(), admitCmd(f, priceID, hold, limit, big, big, nil))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case domain.CodeOf(err) == domain.CodeQuotaExceeded:
				quotaExceeded++
			default:
				other++
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// 放行量不超过安全预占界限；其余全部 quota_exceeded。
	if admitted != capacity {
		t.Errorf("admitted = %d, want %d (limit/hold)", admitted, capacity)
	}
	if quotaExceeded != demand-capacity || other != 0 {
		t.Errorf("quota_exceeded = %d, other = %d; want %d/0", quotaExceeded, other, demand-capacity)
	}
	// 五小时窗口 reserved 恰为放行量 × 预占上界。
	var fiveReserved int64
	if err := db.QueryRow(
		`SELECT reserved_micros FROM inference_quota_windows
		 WHERE entitlement_id = $1 AND kind = 'five_hour' AND state = 'active'`, f.entID).
		Scan(&fiveReserved); err != nil {
		t.Fatal(err)
	}
	if fiveReserved != hold*capacity {
		t.Errorf("five-hour reserved = %d, want %d", fiveReserved, hold*capacity)
	}
	if n := countRequests(t, db, f.entID, "reserved"); n != capacity {
		t.Errorf("reserved requests = %d, want %d", n, capacity)
	}
}

// TestConcurrentFirstConsumptionActivation: 多 goroutine 同时首消费 —
// 只允许建行一次，其余复用同一五小时窗口行（唯一键/账户锁序列化兜底）。
func TestConcurrentFirstConsumptionActivation(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)

	big := domain.Microcredit(1 << 40)
	const demand = 12
	var wg sync.WaitGroup
	windowIDs := make([]string, 0, demand)
	var mu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < demand; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := svc.Admit(context.Background(), admitCmd(f, priceID, 1000, big, big, big, nil))
			if err != nil {
				t.Errorf("admit: %v", err)
				return
			}
			mu.Lock()
			windowIDs = append(windowIDs, res.WindowIDs[domain.WindowFiveHour])
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	// 全部放行绑定同一五小时窗口行；库里恰好一行。
	if len(windowIDs) != demand {
		t.Fatalf("admissions = %d, want %d", len(windowIDs), demand)
	}
	for _, id := range windowIDs[1:] {
		if id != windowIDs[0] {
			t.Errorf("bound to different five-hour windows: %v", windowIDs)
			break
		}
	}
	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM inference_quota_windows
		 WHERE entitlement_id = $1 AND kind = 'five_hour' AND state = 'active'`, f.entID).
		Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("active five-hour windows = %d, want exactly 1", rows)
	}
}

// TestPartialWindowFailureRollsBack: 月窗口不足 → 整个预占回滚，五小时/周/
// Key 均不留占用（任一失败全部回滚）。
func TestPartialWindowFailureRollsBack(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)

	const hold = 100_000
	big := domain.Microcredit(1 << 40)
	tiny := domain.Microcredit(50_000)
	_, err := svc.Admit(context.Background(), admitCmd(f, priceID, hold, big, big, tiny, nil))
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if len(qe.BlockedBy) != 1 || qe.BlockedBy[0].Kind != domain.WindowMonthly {
		t.Errorf("blocked by %+v, want monthly", qe.BlockedBy)
	}
	if qe.DeficitMicros == nil || *qe.DeficitMicros != hold-50_000 {
		t.Errorf("deficit = %v, want %d （缺口信息）", qe.DeficitMicros, hold-50_000)
	}
	if used, reserved := windowSums(t, db, f.entID); used != 0 || reserved != 0 {
		t.Errorf("windows after failed admit: used=%d reserved=%d, want 0/0", used, reserved)
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget = %d, want 0", got)
	}
	if n := countRequests(t, db, f.entID, ""); n != 0 {
		t.Errorf("requests = %d, want 0 (no partial request row)", n)
	}
	// 窗口行已激活（激活与预占同事务，回滚即消失）—— 不得残留。
	var windowRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM inference_quota_windows WHERE entitlement_id = $1`, f.entID).
		Scan(&windowRows); err != nil {
		t.Fatal(err)
	}
	if windowRows != 0 {
		t.Errorf("window rows = %d, want 0 (activation rolled back with the reserve)", windowRows)
	}
}

// TestDuplicateReserveIsAConflict: 同一内部请求 ID 重复预占撞请求主键唯一
// 键 → CodeConflict，额度只计一次。
func TestDuplicateReserveIsAConflict(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	ctx := context.Background()
	big := domain.Microcredit(1 << 40)

	cmd := admitCmd(f, priceID, 100_000, big, big, big, nil)
	if _, err := svc.Admit(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	// 同一 request_id 重复投递：激活可重入，但插入请求撞主键 → conflict，
	// 整个事务回滚，不产生第二份预占。
	_, err := svc.Admit(ctx, cmd)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate admit: err = %v, want CodeConflict", err)
	}
	// 同一 request_id 重复投递：激活可重入，但插入请求撞主键 → conflict，
	// 整个事务回滚，不产生第二份预占。windowSums 是三窗口镜像之和：
	// 3 × 100_000。
	if _, reserved := windowSums(t, db, f.entID); reserved != 300_000 {
		t.Errorf("reserved = %d, want 300000 (duplicate reserve did not double-count)", reserved)
	}
	if n := countRequests(t, db, f.entID, ""); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestCrossWindowCompletion: admitted_at 绑定窗口；请求跨五小时重置时刻
// 结束后仍结算到原窗口，下一次消费激活新窗口（结算不改绑新周期）。
func TestCrossWindowCompletion(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	ctx := context.Background()
	big := domain.Microcredit(1 << 40)

	t0 := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	cmd := admitCmd(f, priceID, 100_000, big, big, big, nil)
	cmd.At = t0
	res, err := svc.Admit(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	origWindow := res.WindowIDs[domain.WindowFiveHour]

	// 窗口结束后结算（t0+6h > t0+5h）：仍落入原窗口。
	attID := uuid.NewString()
	uow, _ := s.Begin(ctx)
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: res.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(60_000), int64(1)
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: res.RequestID,
		Usage: domain.UsageRecord{
			RequestID: res.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000,
		SettledAt:    t0.Add(6 * time.Hour),
	})
	if err != nil {
		t.Fatalf("settle after window reset: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	used, reserved := windowState(t, s, origWindow)
	// 结算把整条 held 预占转出：used += 实际 60k，reserved 全额清零（多占
	// 的 40k 归还，不悬挂）。
	if used != 60_000 || reserved != 0 {
		t.Errorf("original window after cross-reset settle: used=%d reserved=%d, want 60000/0", used, reserved)
	}

	// 下一次消费（t0+6h）激活全新五小时窗口，不复用已过期窗口。
	cmd2 := admitCmd(f, priceID, 10_000, big, big, big, nil)
	cmd2.At = t0.Add(6 * time.Hour)
	res2, err := svc.Admit(ctx, cmd2)
	if err != nil {
		t.Fatal(err)
	}
	newWindow := res2.WindowIDs[domain.WindowFiveHour]
	if newWindow == origWindow {
		t.Error("consumption after window end must open a NEW five-hour window")
	}
	var start time.Time
	if err := db.QueryRow(`SELECT window_start FROM inference_quota_windows WHERE id = $1`, newWindow).
		Scan(&start); err != nil {
		t.Fatal(err)
	}
	if !start.Equal(t0.Add(6 * time.Hour)) {
		t.Errorf("new window start = %s, want %s", start, t0.Add(6*time.Hour))
	}
}

// TestKeySubBudgetConcurrency: Key 子预算为瓶颈时放行量不超预算（累计上
// 限语义，不按周期清零 — 控制者裁决）。
func TestKeySubBudgetConcurrency(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)

	// Key 预算收紧到 2 个 hold；窗口近乎无限。
	const hold = 100_000
	if _, err := db.Exec(`UPDATE inference_api_keys SET budget_limit_micros = $1 WHERE id = $2`,
		2*hold, f.keyID); err != nil {
		t.Fatal(err)
	}
	big := domain.Microcredit(1 << 40)
	const demand = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, keyBlocked := 0, 0
	start := make(chan struct{})
	for i := 0; i < demand; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Admit(ctx, admitCmd(f, priceID, hold, big, big, big, nil))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				admitted++
				return
			}
			var qe *domain.QuotaExceededError
			if errors.As(err, &qe) && qe.KeyBudgetExhausted {
				keyBlocked++
				return
			}
			t.Errorf("unexpected error: %v", err)
		}()
	}
	close(start)
	wg.Wait()
	if admitted != 2 {
		t.Errorf("admitted = %d, want 2 (key budget / hold)", admitted)
	}
	if keyBlocked != demand-2 {
		t.Errorf("key-blocked = %d, want %d", keyBlocked, demand-2)
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 2*hold {
		t.Errorf("budget_used = %d, want %d", got, 2*hold)
	}
}

// TestConcurrentReleaseConsistency: 并发释放全部一致 — 重复释放恰一次成
// 功；最后一个释放者撤销未使用的五小时窗口；下一次消费激活新窗口。
func TestConcurrentReleaseConsistency(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)

	// 三个请求预占在同一五小时窗口上。
	reqIDs := make([]string, 3)
	var fiveWindow string
	for i := range reqIDs {
		res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
		if err != nil {
			t.Fatal(err)
		}
		reqIDs[i] = res.RequestID
		fiveWindow = res.WindowIDs[domain.WindowFiveHour]
	}

	// 并发释放，其中 reqIDs[0] 被两个 goroutine 同时释放（重复释放）。
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i, reqID := range reqIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i] = svc.ReleaseAdmission(ctx, id)
		}(i, reqID)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[3] = svc.ReleaseAdmission(ctx, reqIDs[0])
	}()
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case domain.CodeOf(err) == domain.CodeConflict:
			conflicts++
		default:
			t.Errorf("unexpected release error: %v", err)
		}
	}
	if successes != 3 || conflicts != 1 {
		t.Errorf("release successes=%d conflicts=%d, want 3/1 （重复释放恰一次成功）", successes, conflicts)
	}
	if used, reserved := windowSums(t, db, f.entID); used != 0 || reserved != 0 {
		t.Errorf("windows after releases: used=%d reserved=%d, want 0/0", used, reserved)
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget = %d, want 0", got)
	}
	// 全部请求确认无消费且无其他有效预占 → 五小时窗口事务内撤销。
	var state string
	if err := db.QueryRow(`SELECT state FROM inference_quota_windows WHERE id = $1`, fiveWindow).
		Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "voided" {
		t.Errorf("five-hour window state = %q, want voided", state)
	}
	// 下一次消费激活新窗口（voided 行不参与重叠排他）。
	res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.WindowIDs[domain.WindowFiveHour] == fiveWindow {
		t.Error("new consumption must activate a fresh window after void")
	}
}

// TestReleaseRefusesFinalizedOrReconciled: 已结算/核对中的请求不得释放
// （禁止仅凭未知状态释放预占）。
func TestReleaseRefusesFinalizedOrReconciled(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)

	res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
	if err != nil {
		t.Fatal(err)
	}
	uow, _ := s.Begin(ctx)
	if err := s.MarkReconciliationRequired(ctx, uow, res.RequestID, "unknown_usage", time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseAdmission(ctx, res.RequestID); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("release of reconciling request: err = %v, want CodeConflict", err)
	}
}

// TestReserveWindowHoldFailClosedOnVanishedRow: 窗口行在条件 UPDATE 未命
// 中且明细重读前已消失（同事务内删除）时，Reserve 必须阻断（quota_exceeded
// 带 detail-less block），绝不放行、不产生任何预占/请求行（审查修复：
// 修复前此路径静默 continue，其他 hold 成功时请求会在缺少该窗口预占的情
// 况下被放行 —— fail-open）。
func TestReserveWindowHoldFailClosedOnVanishedRow(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 同事务内删除五小时窗口行：条件 UPDATE 与明细重读都查不到它。
	if _, err := mustTx(t, uow).ExecContext(ctx,
		`DELETE FROM inference_quota_windows WHERE id = $1`, w5); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError (fail-closed)", err)
	}
	if len(qe.BlockedBy) != 1 || qe.BlockedBy[0].Kind != domain.WindowFiveHour {
		t.Errorf("blocked by %+v, want one five_hour block", qe.BlockedBy)
	}
	if qe.BlockedBy[0].ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil（恢复时刻未知不得编造）", qe.BlockedBy[0].ResetsAt)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// 零预占、零请求行、Key 预算未动。
	if n := countRequests(t, db, f.entID, ""); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
	var reservations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Errorf("reservations = %d, want 0", reservations)
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != 0 {
		t.Errorf("key budget = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 数据库租约：账户及上游并发协调（续租、所有权、fencing、超时回收）
// ---------------------------------------------------------------------------

func TestConcurrencyLeaseProtocol(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	now := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)

	// 租约挂在真实请求上（FK）。
	mkReq := func() string {
		res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
		if err != nil {
			t.Fatal(err)
		}
		return res.RequestID
	}
	scope := domain.LeaseScopeUpstreamAccount
	scopeID := uuid.NewString()
	ownerA, ownerB := "instance-a", "instance-b"

	// 上限 2：两个实例各获一个槽位，第三个获取被拒（附恢复时刻）。
	l1, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: ownerA, Limit: 2, TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: ownerB, Limit: 2, TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if l1.FencingToken != 1 || l2.FencingToken != 2 {
		t.Errorf("fencing tokens = %d,%d, want 1,2 (per-scope monotonic)", l1.FencingToken, l2.FencingToken)
	}
	_, err = s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: ownerA, Limit: 2, TTL: time.Minute, Now: now,
	})
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("third acquire: err = %v, want insufficient_capacity", err)
	}

	// 续租：所有权不匹配（错误 owner/fencing）→ conflict；正确 → 成功。
	if err := s.RenewLease(ctx, l1.ID, ownerB, l1.FencingToken, now.Add(2*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("renew with wrong owner: err = %v, want conflict", err)
	}
	if err := s.RenewLease(ctx, l1.ID, ownerA, l1.FencingToken+99, now.Add(2*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("renew with wrong fencing: err = %v, want conflict", err)
	}
	if err := s.RenewLease(ctx, l1.ID, ownerA, l1.FencingToken, now.Add(2*time.Minute)); err != nil {
		t.Errorf("renew with correct ownership: %v", err)
	}

	// 使用点检查：持有者可过；过期后检查失败。
	if err := s.CheckLease(ctx, l1.ID, ownerA, l1.FencingToken, now.Add(time.Minute)); err != nil {
		t.Errorf("check live lease: %v", err)
	}
	if err := s.CheckLease(ctx, l1.ID, ownerA, l1.FencingToken, now.Add(3*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("check expired lease: err = %v, want conflict", err)
	}

	// 超时回收：l1/l2 到期（now+10min），新获取回收旧槽位并拿到更大的
	// fencing token —— 旧持有者无法再续租/释放/通过使用点检查，回收不会
	// 与仍活跃请求产生重叠授权。
	l3, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: ownerA, Limit: 2, TTL: time.Minute, Now: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if l3.FencingToken <= l2.FencingToken {
		t.Errorf("post-reclaim fencing = %d, want > %d (单调递增，跨回收不复用)", l3.FencingToken, l2.FencingToken)
	}
	if err := s.RenewLease(ctx, l1.ID, ownerA, l1.FencingToken, now.Add(11*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("renew reclaimed lease: err = %v, want conflict （旧持有者失去授权）", err)
	}
	if err := s.ReleaseLease(ctx, l1.ID, ownerA, l1.FencingToken); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("release reclaimed lease: err = %v, want conflict", err)
	}
	if err := s.CheckLease(ctx, l1.ID, ownerA, l1.FencingToken, now.Add(10*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("check reclaimed lease: err = %v, want conflict (fenced)", err)
	}
	if err := s.CheckLease(ctx, l3.ID, ownerA, l3.FencingToken, now.Add(10*time.Minute)); err != nil {
		t.Errorf("check new lease after reclaim: %v", err)
	}

	// 正常释放后槽位立即归还。
	if err := s.ReleaseLease(ctx, l3.ID, ownerA, l3.FencingToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: ownerB, Limit: 2, TTL: time.Minute, Now: now.Add(10 * time.Minute),
	}); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}

// TestAdmissionLeaseFailureRollsBackEverything: 策略并发上限满 → 准入失
// 败且预占一并回滚（租约与预占同事务，不留半状态）。
func TestAdmissionLeaseFailureRollsBackEverything(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	svc.AccountLeaseTTL = time.Minute
	big := domain.Microcredit(1 << 40)
	conc := 1

	res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, &conc))
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountLease == nil {
		t.Fatal("want account concurrency lease on admission")
	}
	// 第二个并发准入：额度充足但并发槽满 → insufficient_capacity，且预占
	// 全部回滚。
	_, err = svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, &conc))
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("second admit: err = %v, want insufficient_capacity", err)
	}
	// 第二个并发准入：额度充足但并发槽满 → insufficient_capacity，且预占
	// 全部回滚。三窗口镜像：1 次放行 = 3 × 10_000。
	if _, reserved := windowSums(t, db, f.entID); reserved != 30_000 {
		t.Errorf("reserved = %d, want 30000 (second admission left no holds)", reserved)
	}
	if n := countRequests(t, db, f.entID, ""); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	// 释放租约后准入恢复。
	if err := s.ReleaseLease(ctx, res.AccountLease.ID, svc.OwnerToken, res.AccountLease.FencingToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, &conc)); err != nil {
		t.Errorf("admit after lease release: %v", err)
	}
}

// TestStorageUnavailableFailsClosed: 存储不可用时新售卖调用拒绝放行 —
// 对已关闭连接池的 Admit 必须返回错误而不是放行。
func TestStorageUnavailableFailsClosed(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	db.Close() // 存储不可用

	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	if _, err := svc.Admit(context.Background(), admitCmd(f, priceID, 10_000, big, big, big, nil)); err == nil {
		t.Fatal("admission against unavailable storage must fail closed")
	}
}

// ---------------------------------------------------------------------------
// 双进程跨实例竞争（两个真实服务进程共享同一可丢弃库）
// ---------------------------------------------------------------------------

// workerResult is the machine-readable worker summary (last stdout line).
type workerResult struct {
	Admitted      int `json:"admitted"`
	QuotaExceeded int `json:"quota_exceeded"`
	Other         int `json:"other"`
}

// TestQuotaWorkerProcess is the child-process entry point — it runs ONLY
// when re-executed with KAYA_QUOTA_WORKER=1. Each invocation is a real,
// independent service process: own binary, own connection pool, same
// shared disposable database.
func TestQuotaWorkerProcess(t *testing.T) {
	if os.Getenv("KAYA_QUOTA_WORKER") != "1" {
		t.Skip("worker process entry point")
	}
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker connect: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()

	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			fmt.Fprintf(os.Stderr, "worker: missing env %s\n", k)
			os.Exit(2)
		}
		return v
	}
	demand, _ := strconv.Atoi(env("KAYA_WORKER_DEMAND"))
	hold, _ := strconv.ParseInt(env("KAYA_WORKER_HOLD"), 10, 64)
	limit, _ := strconv.ParseInt(env("KAYA_WORKER_LIMIT"), 10, 64)
	anchor, err := time.Parse(time.RFC3339, env("KAYA_WORKER_ANCHOR"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker anchor: %v\n", err)
		os.Exit(2)
	}
	f := fixture{
		accountID: env("KAYA_WORKER_ACCOUNT"), keyID: env("KAYA_WORKER_KEY"),
		entID: env("KAYA_WORKER_ENT"), policyID: env("KAYA_WORKER_POLICY"),
		modelID: env("KAYA_WORKER_MODEL"),
	}
	priceID := env("KAYA_WORKER_PRICE")

	pure, err := (&PriceVersion{
		ID: priceID, ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, Revision: 1,
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}).Pure()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker price: %v\n", err)
		os.Exit(2)
	}
	big := domain.Microcredit(1 << 40)
	lim := domain.Microcredit(limit)
	one := int64(1)
	svc := quota.NewService(NewStore(db), nil)

	var res workerResult
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	const goroutines = 4
	per := demand / goroutines
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < per; i++ {
				cmd := quota.AdmitCommand{
					Request: domain.Request{
						ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
						EntitlementID: f.entID, ModelID: f.modelID,
						Protocol: domain.ProtocolOpenAIChat,
					},
					Entitlement: domain.Entitlement{
						ID: f.entID, BillingAccountID: f.accountID,
						AnchorAt: anchor, PolicyVersionID: f.policyID,
					},
					Policy: quota.Policy{
						Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
						FiveHourLimit: &lim, WeeklyLimit: &big, MonthlyLimit: &big,
					},
					CreditPrice:          pure,
					Model:                domain.Model{ID: f.modelID, ContextTokens: 200_000, MaxOutputTokens: 1},
					EstimatedInputTokens: hold,
					ClientMaxTokens:      &one,
				}
				_, err := svc.Admit(ctx, cmd)
				mu.Lock()
				switch {
				case err == nil:
					res.Admitted++
				case domain.CodeOf(err) == domain.CodeQuotaExceeded:
					res.QuotaExceeded++
				default:
					res.Other++
					fmt.Fprintf(os.Stderr, "worker error: %v\n", err)
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	out, _ := json.Marshal(res)
	fmt.Printf("RESULT %s\n", out)
}

// TestTwoProcessContendLastCredit orchestrates TWO real service processes
// (re-executed test binary, independent connection pools) racing for the
// last credits of one account on the shared disposable database — the
// cross-instance counterpart of TestConcurrentAdmissionsNeverExceedLimit
// (任务书: 不得以单进程加锁模拟代替).
func TestTwoProcessContendLastCredit(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)

	const hold = 100_000
	const capacity = 3
	const demandPerProc = 8 // 每进程 4 goroutine × 2 次
	limit := int64(hold * capacity)

	anchor := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC).Format(time.RFC3339)
	mkWorker := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestQuotaWorkerProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"KAYA_QUOTA_WORKER=1",
			"DATABASE_URL="+testDSN,
			"KAYA_WORKER_DEMAND="+strconv.Itoa(demandPerProc),
			"KAYA_WORKER_HOLD="+strconv.FormatInt(hold, 10),
			"KAYA_WORKER_LIMIT="+strconv.FormatInt(limit, 10),
			"KAYA_WORKER_ANCHOR="+anchor,
			"KAYA_WORKER_ACCOUNT="+f.accountID,
			"KAYA_WORKER_KEY="+f.keyID,
			"KAYA_WORKER_ENT="+f.entID,
			"KAYA_WORKER_POLICY="+f.policyID,
			"KAYA_WORKER_MODEL="+f.modelID,
			"KAYA_WORKER_PRICE="+priceID,
		)
		return cmd
	}

	run := func() (workerResult, []byte, error) {
		out, err := mkWorker().CombinedOutput()
		var r workerResult
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "RESULT ") {
				if jerr := json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT ")), &r); jerr != nil {
					return r, out, fmt.Errorf("parse worker result: %w", jerr)
				}
			}
		}
		return r, out, err
	}

	type procOut struct {
		res workerResult
		out []byte
		err error
	}
	ch := make(chan procOut, 2)
	go func() {
		r, out, err := run()
		ch <- procOut{r, out, err}
	}()
	go func() {
		r, out, err := run()
		ch <- procOut{r, out, err}
	}()

	var total workerResult
	perProc := make([]workerResult, 0, 2)
	for i := 0; i < 2; i++ {
		po := <-ch
		if po.err != nil {
			t.Fatalf("worker process failed: %v\n%s", po.err, po.out)
		}
		perProc = append(perProc, po.res)
		total.Admitted += po.res.Admitted
		total.QuotaExceeded += po.res.QuotaExceeded
		total.Other += po.res.Other
	}
	t.Logf("two-process contention: proc A %+v, proc B %+v", perProc[0], perProc[1])

	// 跨实例放行量不超过安全预占界限；其余全部 quota_exceeded。
	if total.Admitted != capacity {
		t.Errorf("total admitted across 2 processes = %d, want %d", total.Admitted, capacity)
	}
	if total.QuotaExceeded != 2*demandPerProc-capacity || total.Other != 0 {
		t.Errorf("quota_exceeded=%d other=%d, want %d/0", total.QuotaExceeded, total.Other, 2*demandPerProc-capacity)
	}
	// 跨进程预占在数据库中恰好一致：三窗口镜像之和 = 3 窗口 × 放行量 × 上界。
	if _, reserved := windowSums(t, db, f.entID); reserved != 3*hold*capacity {
		t.Errorf("window reserved = %d, want %d", reserved, 3*hold*capacity)
	}
	if n := countRequests(t, db, f.entID, "reserved"); n != capacity {
		t.Errorf("reserved requests = %d, want %d", n, capacity)
	}
}
