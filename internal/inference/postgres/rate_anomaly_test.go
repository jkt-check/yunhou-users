package postgres

// rate_anomaly_test.go — 人工停放的账务异常（缺预占 / 预占漂移 / 证据不足）
// 的**速率预占处置**（终审 Finding 1）。
//
// 问题：一个被停放的异常请求，其速率预占行永远停在 state='reserved'，而 TPM
// 的在途求和把**所有** state='reserved' 的行都算进账户额度（与年龄无关），
// 保留期清理又永不删除这类行 —— 一笔异常就会永久吃掉账户的 TPM 额度：账户
// 被永久 429（rate_limited）拦住，而且没有任何自动或人工路径能把它取回来。
//
// 处置语义（migration 054）：
//   - 速率预占行转入 state='anomaly'：**离开在途 TPM 求和**（不再占额度），
//     但**不是** 'released'（那等于宣称"确认零消费"）也不是 'settled'（那
//     等于宣称"已知实际用量"）—— 消费仍是未知，异常证据留在行上；
//   - RPM 事实保留：行还在、counted_at 不变，一次逻辑请求仍然计 1 次；
//   - 金额侧完全不动：窗口/Key 预占仍在，账本没有 charge，钱没有结也没有放；
//   - 保留期清理可以删除 anomaly 行（它不再占额度），与其它终态行同一口径。
import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// rateAnomalyRow is the persisted state of one anomaly-disposed rate row.
type rateAnomalyRow struct {
	Scope         string     `db:"scope"`
	State         string     `db:"state"`
	Reserved      int64      `db:"reserved_tokens"`
	Settled       int64      `db:"settled_tokens"`
	AnomalyAt     *time.Time `db:"anomaly_at"`
	AnomalyReason *string    `db:"anomaly_reason"`
}

func loadRateAnomalyRows(t *testing.T, s *Store, requestID string) []rateAnomalyRow {
	t.Helper()
	var rows []rateAnomalyRow
	if err := s.db.SelectContext(context.Background(), &rows,
		`SELECT scope, state, reserved_tokens, settled_tokens, anomaly_at, anomaly_reason
		 FROM inference_rate_reservations WHERE request_id = $1 ORDER BY scope`,
		requestID); err != nil {
		t.Fatalf("load rate rows: %v", err)
	}
	return rows
}

// TestParkedAnomaly_RateHoldLeavesTheLiveTPM — 人工停放后：速率预占被显式
// 处置为 anomaly（离开在途 TPM 求和），RPM 计数仍然保留 1 次，金额预占与
// 账本完全不动。
func TestParkedAnomaly_RateHoldLeavesTheLiveTPM(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm,
		domain.RateLimits{AccountRPM: 10, PolicyTPM: 100}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if got := inflightTokens(t, s, f.accountID); got != 60 {
		t.Fatalf("in-flight tokens = %d, want 60 (准入先占住 TPM 额度)", got)
	}
	usedBefore, reservedBefore := windowState(t, s, w5)
	keyBefore := keyBudgetUsed(t, s, f.keyID)

	// 停放：人工原因（缺预占）→ 速率预占必须被显式处置。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonMissingReservation,
		time.Now().UTC().Add(time.Hour)); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("park: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rows := loadRateAnomalyRows(t, s, requestID)
	if len(rows) != 2 {
		t.Fatalf("rate rows = %d, want 2 (account + key)", len(rows))
	}
	for _, r := range rows {
		if r.State != "anomaly" {
			t.Errorf("scope %s state = %q, want anomaly (停放必须显式处置速率预占，不得停在 reserved)", r.Scope, r.State)
		}
		if r.AnomalyReason == nil || *r.AnomalyReason != ReasonMissingReservation {
			t.Errorf("scope %s anomaly reason = %v, want %q (异常证据必须留在行上)",
				r.Scope, r.AnomalyReason, ReasonMissingReservation)
		}
		if r.AnomalyAt == nil {
			t.Errorf("scope %s anomaly_at is nil, want the disposition instant", r.Scope)
		}
		if r.Settled != 0 {
			t.Errorf("scope %s settled tokens = %d, want 0 (未知用量不得记成已知)", r.Scope, r.Settled)
		}
	}

	// TPM：停放后不再占额度（这是本回归的核心断言）。
	if got := inflightTokens(t, s, f.accountID); got != 0 {
		t.Errorf("in-flight tokens = %d, want 0 (停放的异常不得永久占用 TPM)", got)
	}
	// RPM：一次逻辑请求仍然计 1 次（速率事实不被处置掉）。
	if got := windowRateCount(t, s, f.accountID); got != 1 {
		t.Errorf("account rate count = %d, want 1 (RPM 事实必须保留)", got)
	}

	// 金额侧完全不动：预占仍在，账本没有 charge，钱既没结也没放。
	usedAfter, reservedAfter := windowState(t, s, w5)
	if usedAfter != usedBefore || reservedAfter != reservedBefore {
		t.Errorf("window = used %d/%d reserved %d/%d, want unchanged (处置速率预占不得动金额预占)",
			usedAfter, usedBefore, reservedAfter, reservedBefore)
	}
	if got := keyBudgetUsed(t, s, f.keyID); got != keyBefore {
		t.Errorf("key budget used = %d, want %d (处置速率预占不得动 Key 预算)", got, keyBefore)
	}
	var charges int
	if err := s.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1 AND entry_type = 'charge'`,
		requestID); err != nil {
		t.Fatal(err)
	}
	if charges != 0 {
		t.Errorf("ledger charge rows = %d, want 0 (处置速率预占不得结算钱)", charges)
	}

	// 新准入不再被那笔停放的预占阻断（账户不会被永久 429 拦住）。
	uow2, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	adm, err := s.Reserve(ctx, uow2, cmd)
	if err != nil {
		_ = uow2.Rollback(ctx)
		t.Fatalf("reserve: %v", err)
	}
	err = s.ReserveRateTx(ctx, uow2, domain.RateReserveCommand{
		RequestID:        adm.RequestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           domain.RateLimits{AccountRPM: 10, PolicyTPM: 100},
		ReservedTokens:   50,
	})
	_ = uow2.Rollback(ctx)
	if err != nil {
		t.Fatalf("a parked anomaly must not block the account TPM: %v", err)
	}
}

// TestParkedAnomaly_IsPurgedByTheRetentionSweep — 处置后的速率行不再占额度，
// 因此保留期清理可以删除它（与其它终态行同一口径）：滞留的行不会无限增长。
func TestParkedAnomaly_IsPurgedByTheRetentionSweep(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonReservationDrift,
		time.Now().UTC().Add(time.Hour)); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("park: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 把计数时刻推到保留期之外（模拟陈旧数据）。
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET counted_at = now() - interval '48 hours'`); err != nil {
		t.Fatal(err)
	}
	removed, err := s.PurgeRateReservations(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 2 {
		t.Errorf("purged = %d, want 2 (处置后的速率行不再占额度，必须可被保留期清理)", removed)
	}
	if got := len(loadRateRows(t, s, requestID)); got != 0 {
		t.Errorf("rate rows = %d, want 0 (处置后的速率行必须可被清理)", got)
	}
}

// TestParkedAnomaly_LaterSettlementBooksTheActualTokens — 异常被人工处置后
// 请求真的结算了：速率行从 anomaly 转为 settled，并把**实际**归一化 token
// 记入 TPM（不是 hold、也不是 0），异常痕迹保留。
func TestParkedAnomaly_LaterSettlementBooksTheActualTokens(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonMissingReservation,
		time.Now().UTC().Add(time.Hour)); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("park: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 人工处置后请求真的结算了（金额预占仍在，因此结算可以成功）。
	settleRequest(t, s, requestID, domain.UsageReported, 10, 20)

	rows := loadRateAnomalyRows(t, s, requestID)
	if len(rows) != 2 {
		t.Fatalf("rate rows = %d, want 2", len(rows))
	}
	if rows[0].State != "settled" {
		t.Errorf("account state = %q, want settled (处置后真的结算必须落实际 token)", rows[0].State)
	}
	if rows[0].Settled != 30 {
		t.Errorf("account settled tokens = %d, want 30 (实际归一化 token)", rows[0].Settled)
	}
	if rows[0].AnomalyReason == nil {
		t.Error("account anomaly reason was erased — the audit trace must survive the promotion")
	}
	if got := settledTokensInWindow(t, s, f.accountID); got != 30 {
		t.Errorf("settled tokens in window = %d, want 30", got)
	}
}

// TestParkedAnomaly_CorrectionIsExplicit — 对 anomaly 行的修正请求必须得到
// **明确**的拒绝（不是静默的 no-op、也不是把未知记成 0）：该行没有已结算的
// token 总量可改。
func TestParkedAnomaly_CorrectionIsExplicit(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonMissingReservation,
		time.Now().UTC().Add(time.Hour)); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("park: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	err = correctRateTokens(t, s, requestID, 123)
	if err == nil {
		t.Fatal("correcting an anomaly-held rate reservation must fail loudly, not silently no-op")
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("err = %v, want a conflict (anomaly 行没有已结算 token 总量可改)", err)
	}
	// 拒绝之后行仍然是 anomaly，token 总量没有被改写（未知 ≠ 0）。
	rows := loadRateAnomalyRows(t, s, requestID)
	if rows[0].State != "anomaly" || rows[0].Settled != 0 {
		t.Errorf("account row = %s/%d, want anomaly/0 (拒绝后不得改写行)", rows[0].State, rows[0].Settled)
	}
}

// TestEvidenceDrivenPark_KeepsTheRateHold — 证据驱动的停放（unknown_usage）
// 必须**保留**速率预占：恢复 worker 会按保守估算结算它，额度不能提前归还。
func TestEvidenceDrivenPark_KeepsTheRateHold(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonUnknownUsage,
		time.Now().UTC().Add(time.Hour)); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("park: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inflightTokens(t, s, f.accountID); got != 60 {
		t.Errorf("in-flight tokens = %d, want 60 (证据驱动的停放必须保留预占，等待保守估算结算)", got)
	}
	rows := loadRateAnomalyRows(t, s, requestID)
	for _, r := range rows {
		if r.State != "reserved" {
			t.Errorf("scope %s state = %q, want reserved (证据驱动的停放不得处置预占)", r.Scope, r.State)
		}
	}
}
