package workers

// settlement_recovery_rate_anomaly_test.go — 终审 Finding 1：恢复 worker 把
// 一个账务异常请求停放之后，
//
//   - 该请求的在途速率预占必须被**显式处置**（离开账户的 TPM 在途求和）——
//     否则一笔异常就永久吃掉账户的 TPM 额度（账户被永久 429 拦住，且没有
//     任何路径能把它取回来）；
//   - 后续每一轮扫描都不得**重复驱动、重复告警**（人工原因不得被自动驱动），
//     也不得重复处置。
import (
	"context"
	"testing"
)

// fabricateRateReservation inserts the rate reservation rows an admission
// would have created (account + key scope) for the given request.
func (f *workerFixture) fabricateRateReservation(t *testing.T, requestID string, reservedTokens int64) {
	t.Helper()
	if _, err := f.db.Exec(
		`INSERT INTO inference_rate_reservations
		   (request_id, billing_account_id, api_key_id, scope, counted_at, reserved_tokens, state)
		 VALUES ($1, $2, NULL, 'account', now(), $4, 'reserved'),
		        ($1, $2, $3,  'key',      now(), 0,    'reserved')`,
		requestID, f.accountID, f.keyID, reservedTokens); err != nil {
		t.Fatalf("insert rate reservation: %v", err)
	}
}

// rateReservationStates reads the state of the request's rate rows.
func (f *workerFixture) rateReservationStates(t *testing.T, requestID string) []string {
	t.Helper()
	var states []string
	if err := f.db.SelectContext(context.Background(), &states,
		`SELECT state FROM inference_rate_reservations WHERE request_id = $1 ORDER BY scope`,
		requestID); err != nil {
		t.Fatalf("read rate reservations: %v", err)
	}
	return states
}

// accountInflightTokens sums the account's in-flight TPM reservations.
func (f *workerFixture) accountInflightTokens(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := f.db.Get(&v,
		`SELECT COALESCE(SUM(reserved_tokens),0) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account' AND state = 'reserved'`,
		f.accountID); err != nil {
		t.Fatalf("read in-flight tokens: %v", err)
	}
	return v
}

// TestRecover_ParkedAnomaly_DisposesTheRateHoldAndDoesNotReDrive — 缺预占的
// 请求被恢复 worker 停放时：速率预占被处置为 anomaly（离开 TPM 在途求和），
// 金额预占仍在、账本没有 charge；下一轮扫描不得重复驱动、不得重复告警。
func TestRecover_ParkedAnomaly_DisposesTheRateHoldAndDoesNotReDrive(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	f.fabricateRateReservation(t, reqID, 60)
	// 本应存在的金额预占被删除 → 恢复走释放路径 → 缺预占异常 → 停放。
	if _, err := f.db.Exec(
		`DELETE FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
		t.Fatal(err)
	}
	if got := f.accountInflightTokens(t); got != 60 {
		t.Fatalf("in-flight tokens = %d, want 60 (准入先占住 TPM 额度)", got)
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
	status, _, _, settled := f.requestState(t, reqID)
	if status != "reconciliation_required" {
		t.Errorf("request status = %q, want reconciliation_required (停放进专门核对)", status)
	}
	if settled != 0 {
		t.Errorf("settled = %d, want 0 (缺预占时不得入账)", settled)
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

	// 速率预占被显式处置：离开 TPM 在途求和，但保留 RPM 计数事实。
	for _, st := range f.rateReservationStates(t, reqID) {
		if st != "anomaly" {
			t.Errorf("rate reservation state = %q, want anomaly (停放的异常不得永久占用 TPM)", st)
		}
	}
	if got := f.accountInflightTokens(t); got != 0 {
		t.Errorf("in-flight tokens = %d, want 0 (停放的异常不得永久占用 TPM)", got)
	}
	var counted int
	if err := f.db.Get(&counted,
		`SELECT COUNT(*) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account'
		   AND counted_at > now() - interval '60 seconds'`, f.accountID); err != nil {
		t.Fatal(err)
	}
	if counted != 1 {
		t.Errorf("account rate count = %d, want 1 (RPM 事实必须保留)", counted)
	}

	// 第二轮：不得重复驱动、不得重复告警、不得重复处置。
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
	if stats2.SettledConservative != 0 || stats2.SettledVerified != 0 {
		t.Errorf("second pass stats = %+v, want no automatic settlement (人工原因不得被驱动)", stats2)
	}
	var jobs int
	if err := f.db.Get(&jobs,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("jobs = %d, want 1 (不得重复入队)", jobs)
	}
	if got := f.accountInflightTokens(t); got != 0 {
		t.Errorf("second pass in-flight tokens = %d, want 0 (处置是幂等的)", got)
	}
}

// TestRecover_ParkedAnomaly_KeepsTheMonetaryHoldsAndTheLedgerEmpty — 处置
// 速率预占**不动金额**：窗口预占仍在、账本没有 charge —— 处置的是额度，不是
// 证据，更不是钱。
func TestRecover_ParkedAnomaly_KeepsTheMonetaryHoldsAndTheLedgerEmpty(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	reqID := f.fabricateReserved(t)
	f.fabricateRateReservation(t, reqID, 60)
	usedBefore, reservedBefore := f.windowUsedReserved(t, f.w5)
	if _, err := f.db.Exec(
		`DELETE FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.newWorker().RunPass(ctx); err != nil {
		t.Fatal(err)
	}

	usedAfter, reservedAfter := f.windowUsedReserved(t, f.w5)
	if usedAfter != usedBefore || reservedAfter != reservedBefore {
		t.Errorf("window = used %d/%d reserved %d/%d, want unchanged (处置不得动金额预占)",
			usedAfter, usedBefore, reservedAfter, reservedBefore)
	}
	var charges int
	if err := f.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1 AND entry_type = 'charge'`,
		reqID); err != nil {
		t.Fatal(err)
	}
	if charges != 0 {
		t.Errorf("ledger charge rows = %d, want 0 (处置不得结算钱)", charges)
	}
	status, usage, _, settled := f.requestState(t, reqID)
	if status != "reconciliation_required" {
		t.Errorf("request status = %q, want reconciliation_required", status)
	}
	if usage != "unknown" {
		t.Errorf("usage status = %q, want unknown (未知用量不得记成 0)", usage)
	}
	if settled != 0 {
		t.Errorf("settled = %d, want 0", settled)
	}
	// 停放的请求不会在保留期之外被 TTL 释放（钱与证据都必须等人）。
	if _, err := f.db.Exec(
		`UPDATE inference_requests SET updated_at = now() - interval '72 hours'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.newWorker().RunPass(ctx); err != nil {
		t.Fatal(err)
	}
	status2, _, _, settled2 := f.requestState(t, reqID)
	if status2 != "reconciliation_required" {
		t.Errorf("request status = %q, want reconciliation_required (不得按 TTL 释放)", status2)
	}
	if settled2 != 0 {
		t.Errorf("settled = %d, want 0 (不得按 TTL 记零)", settled2)
	}
}
