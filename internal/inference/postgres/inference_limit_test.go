package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// inference_limit_test.go — T4 持久速率预占的存储层行为（真实库）。
//
// 覆盖：幂等身份、事务回滚无残留、结算把预占换成实际 token、释放归还预占
// 但保留计数、估算→真实的净差额校正（保持原结算时刻）、过期窗口校正不占
// 当前一分钟额度、保留期清理绝不删未结算预占。

// rateRow is the persisted state of one rate reservation row.
type rateRow struct {
	Scope          string     `db:"scope"`
	State          string     `db:"state"`
	CountedAt      time.Time  `db:"counted_at"`
	ReservedTokens int64      `db:"reserved_tokens"`
	SettledTokens  int64      `db:"settled_tokens"`
	SettledAt      *time.Time `db:"settled_at"`
	ReleasedAt     *time.Time `db:"released_at"`
	APIKeyID       *string    `db:"api_key_id"`
	BillingAccount string     `db:"billing_account_id"`
}

func loadRateRows(t *testing.T, s *Store, requestID string) []rateRow {
	t.Helper()
	var rows []rateRow
	if err := s.db.SelectContext(context.Background(), &rows,
		`SELECT scope, state, counted_at, reserved_tokens, settled_tokens,
		        settled_at, released_at, api_key_id, billing_account_id
		 FROM inference_rate_reservations WHERE request_id = $1 ORDER BY scope`,
		requestID); err != nil {
		t.Fatalf("load rate rows: %v", err)
	}
	return rows
}

func countRateRows(t *testing.T, s *Store, accountID string) int {
	t.Helper()
	var n int
	if err := s.db.Get(&n,
		`SELECT COUNT(*) FROM inference_rate_reservations WHERE billing_account_id = $1`,
		accountID); err != nil {
		t.Fatalf("count rate rows: %v", err)
	}
	return n
}

// admitWithRate reserves the money holds AND the rate reservation of one
// logical request inside a single transaction (the admission shape).
func admitWithRate(t *testing.T, s *Store, f fixture, w5, ww, wm string,
	limits domain.RateLimits, reservedTokens int64) (string, error) {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		_ = uow.Rollback(ctx)
		return "", err
	}
	err = s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID:        adm.RequestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           limits,
		ReservedTokens:   reservedTokens,
	})
	if err != nil {
		_ = uow.Rollback(ctx)
		return "", err
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID, nil
}

// TestReserveRateTx_CountsOneLogicalRequestOnce: 同一个逻辑请求 ID 只能有
// 一份速率预占（幂等身份）—— 重复预占撞唯一键。
func TestReserveRateTx_CountsOneLogicalRequestOnce(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	rows := loadRateRows(t, s, requestID)
	if len(rows) != 2 {
		t.Fatalf("rate rows = %d, want 2 (account + key)", len(rows))
	}
	for _, r := range rows {
		if r.State != "reserved" {
			t.Errorf("scope %s state = %s, want reserved", r.Scope, r.State)
		}
	}
	if rows[0].ReservedTokens != 60 {
		t.Errorf("account reserved tokens = %d, want 60", rows[0].ReservedTokens)
	}
	if rows[1].ReservedTokens != 0 {
		t.Errorf("key reserved tokens = %d, want 0 (the TPM is account-level)", rows[1].ReservedTokens)
	}

	// 同一个逻辑请求再预占一次 → 冲突（幂等），不得产生第二份计数。
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID:        requestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           domain.RateLimits{AccountRPM: 10},
		ReservedTokens:   60,
	})
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate reservation err = %v, want conflict (idempotent identity)", err)
	}
	if got := countRateRows(t, s, f.accountID); got != 2 {
		t.Errorf("rate rows = %d, want 2 (a duplicate must not add a second count)", got)
	}
}

// TestReserveRateTx_RejectionRollsBackEverything: 速率拒绝必须让整次准入
// 回滚 —— 没有请求行、没有金额预占、没有任何速率计数。
func TestReserveRateTx_RejectionRollsBackEverything(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	// 第一个请求成功准入（占掉账户 RPM=1 的额度）。
	if _, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 1}, 60); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// 记录准入后的窗口占用（第一个请求的金额预占仍然持有）。
	reservedBefore := map[string]domain.Microcredit{}
	for _, id := range []string{w5, ww, wm} {
		_, reserved := windowState(t, s, id)
		reservedBefore[id] = reserved
	}

	// 第二个请求：账户 RPM 已满 → 速率拒绝。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID:        adm.RequestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           domain.RateLimits{AccountRPM: 1},
		ReservedTokens:   60,
	})
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("err = %v, want rate_limited (must never look like quota_exceeded)", err)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// 回滚后：第二个请求没有请求行、没有速率行，窗口的 reserved 也归零。
	if got := countRateRows(t, s, f.accountID); got != 2 {
		t.Errorf("rate rows = %d, want 2 (the rejected admission left no count)", got)
	}
	var requests int
	if err := s.db.Get(&requests, `SELECT COUNT(*) FROM inference_requests WHERE id = $1`, adm.RequestID); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Errorf("the rejected admission persisted a request row")
	}
	for _, id := range []string{w5, ww, wm} {
		_, reserved := windowState(t, s, id)
		if reserved != reservedBefore[id] {
			t.Errorf("window %s reserved = %d, want %d (the rejected admission left no partial hold)",
				id, reserved, reservedBefore[id])
		}
	}
}

// TestRateSettle_ConvertsTheHoldIntoTheActualTokens: 结算把在途预占换成
// **实际** token（超预占按实际总量计，不截到预占值）。
func TestRateSettle_ConvertsTheHoldIntoTheActualTokens(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 1000}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	// 结算：实际 150 token（超过预占 60）。
	settleRequest(t, s, requestID, domain.UsageReported, 100, 50)
	rows := loadRateRows(t, s, requestID)
	acc := rows[0]
	if acc.State != "settled" {
		t.Errorf("account state = %s, want settled", acc.State)
	}
	if acc.SettledTokens != 150 {
		t.Errorf("settled tokens = %d, want 150 (the actual total, never clamped to the 60 hold)", acc.SettledTokens)
	}
	if acc.ReservedTokens != 60 {
		t.Errorf("reserved tokens = %d, want 60 (the hold is kept for the audit trail)", acc.ReservedTokens)
	}
	if acc.SettledAt == nil {
		t.Error("the settlement instant must be recorded")
	}
	// 在途预占不再计入额度：换一个请求仍可按 TPM=100 之外的额度准入。
	if got := inflightTokens(t, s, f.accountID); got != 0 {
		t.Errorf("in-flight tokens = %d, want 0 after settlement", got)
	}
}

// TestRateRelease_DropsTheHoldButKeepsTheCount: 确认零消费 → 释放 TPM 预占；
// 但 RPM 计数保留（准入成功即计 1 次，上游失败也算）。
func TestRateRelease_DropsTheHoldButKeepsTheCount(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10, PolicyTPM: 100}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, uow, requestID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows := loadRateRows(t, s, requestID)
	for _, r := range rows {
		if r.State != "released" {
			t.Errorf("scope %s state = %s, want released", r.Scope, r.State)
		}
		if r.ReservedTokens != 0 {
			t.Errorf("scope %s reserved tokens = %d, want 0 (the hold is released)", r.Scope, r.ReservedTokens)
		}
	}
	if got := inflightTokens(t, s, f.accountID); got != 0 {
		t.Errorf("in-flight tokens = %d, want 0 after release", got)
	}
	// 计数仍在：账户 RPM 的窗口内计数 = 1。
	if got := windowRateCount(t, s, f.accountID); got != 1 {
		t.Errorf("account rate count = %d, want 1 (a released request still counts once)", got)
	}
}

// TestCorrectRateTokens_NetDeltaKeepsTheOriginalSettlementTime: 估算→真实
// 的多次校正按**同一请求净额**计一次（不累加各 revision），且不改变原结算
// 时刻（60 秒窗口归属不变）。
func TestCorrectRateTokens_NetDeltaKeepsTheOriginalSettlementTime(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 10_000}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	settleRequest(t, s, requestID, domain.UsageEstimated, 10, 10)
	before := loadRateRows(t, s, requestID)[0]
	if before.SettledTokens != 20 {
		t.Fatalf("settled tokens = %d, want 20", before.SettledTokens)
	}

	// 第一次校正：20 → 35。
	if err := correctRateTokens(t, s, requestID, 35); err != nil {
		t.Fatalf("correct 1: %v", err)
	}
	// 第二次校正：35 → 28（净差额，不是累加）。
	if err := correctRateTokens(t, s, requestID, 28); err != nil {
		t.Fatalf("correct 2: %v", err)
	}
	// 幂等：重复同一次校正结果不变。
	if err := correctRateTokens(t, s, requestID, 28); err != nil {
		t.Fatalf("correct 3 (idempotent): %v", err)
	}
	after := loadRateRows(t, s, requestID)[0]
	if after.SettledTokens != 28 {
		t.Errorf("settled tokens = %d, want 28 (the net total, never accumulated)", after.SettledTokens)
	}
	if !after.SettledAt.Equal(*before.SettledAt) {
		t.Errorf("settled_at = %v, want %v (a correction never resets the window attribution)",
			after.SettledAt, before.SettledAt)
	}
}

// TestCorrectRateTokens_OutOfWindowCorrectionDoesNotConsumeTheCurrentMinute:
// 原结算已在窗口外时，校正只修正请求事实，不重新制造本分钟消费。
func TestCorrectRateTokens_OutOfWindowCorrectionDoesNotConsumeTheCurrentMinute(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 10_000}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	settleRequest(t, s, requestID, domain.UsageEstimated, 10, 10)
	// 把结算时刻推到窗口外（模拟很久之后才拿到真实证据）。
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET settled_at = now() - interval '10 minutes' WHERE request_id = $1`,
		requestID); err != nil {
		t.Fatal(err)
	}
	before := settledTokensInWindow(t, s, f.accountID)
	if before != 0 {
		t.Fatalf("settled tokens in window = %d, want 0 (the settlement is outside the window)", before)
	}
	// 校正到 500 token：窗口归属不变 → 当前分钟的 TPM 消费仍是 0。
	if err := correctRateTokens(t, s, requestID, 500); err != nil {
		t.Fatalf("correct: %v", err)
	}
	after := settledTokensInWindow(t, s, f.accountID)
	if after != 0 {
		t.Errorf("settled tokens in window = %d, want 0 (an out-of-window correction must not create current-minute consumption)", after)
	}
	rows := loadRateRows(t, s, requestID)
	if rows[0].SettledTokens != 500 {
		t.Errorf("settled tokens = %d, want 500 (the request fact is corrected)", rows[0].SettledTokens)
	}
}

// TestPurgeRateReservations_NeverDeletesUnsettledReservations: 保留期清理
// 只删已终结的行；未结算预占永不删除（长流跨窗口仍占额度）。
func TestPurgeRateReservations_NeverDeletesUnsettledReservations(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	settledID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	settleRequest(t, s, settledID, domain.UsageReported, 1, 1)
	// 第二个请求保持在途（未结算）。
	openID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{AccountRPM: 10}, 60)
	if err != nil {
		t.Fatalf("admit 2: %v", err)
	}

	// Both admission and settlement are expired; a recent settlement is live TPM.
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations
		 SET counted_at = now() - interval '48 hours',
		     settled_at = CASE WHEN settled_at IS NOT NULL THEN now() - interval '48 hours' END`); err != nil {
		t.Fatal(err)
	}
	removed, err := s.PurgeRateReservations(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 2 {
		t.Errorf("purged = %d, want 2 (the settled request's two rows)", removed)
	}
	// 未结算的预占必须还在。
	if got := len(loadRateRows(t, s, openID)); got != 2 {
		t.Errorf("open reservation rows = %d, want 2 (an unsettled reservation is never purged)", got)
	}
	if got := inflightTokens(t, s, f.accountID); got != 60 {
		t.Errorf("in-flight tokens = %d, want 60 (the hold survived the purge)", got)
	}
}

func TestPurgeRateReservations_PreservesRecentlySettledOldRequests(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	limits := domain.RateLimits{PolicyTPM: 100}
	requestID, err := admitWithRate(t, s, f, w5, ww, wm, limits, 90)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET counted_at = now() - interval '48 hours'
		 WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}
	settleRequest(t, s, requestID, domain.UsageReported, 80, 10)
	if got := settledTokensInWindow(t, s, f.accountID); got != 90 {
		t.Fatalf("settled tokens before purge = %d, want 90", got)
	}
	if _, err := s.PurgeRateReservations(context.Background(), time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := admitWithRate(t, s, f, w5, ww, wm, limits, 20); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Errorf("90 settled + 20 new tokens: err = %v, want rate_limited after purge", err)
	}
	if got := settledTokensInWindow(t, s, f.accountID); got != 90 {
		t.Errorf("settled tokens after purge = %d, want 90", got)
	}
}

func TestPurgeRateReservations_ShortRetentionPreservesLiveRPM(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	limits := domain.RateLimits{AccountRPM: 1}
	requestID, err := admitWithRate(t, s, f, w5, ww, wm, limits, 10)
	if err != nil {
		t.Fatal(err)
	}
	settleRequest(t, s, requestID, domain.UsageReported, 5, 5)
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET counted_at = now() - interval '30 seconds'
		 WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeRateReservations(context.Background(), time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := admitWithRate(t, s, f, w5, ww, wm, limits, 10); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("second request: err = %v, want rate_limited with live RPM", err)
	}
}

// settleRequest settles a request with the given token buckets (the
// attempt row is inserted first — the usage record references it).
func settleRequest(t *testing.T, s *Store, requestID string, source domain.UsageSource,
	in, out int64) {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	att := &domain.Attempt{ID: attemptID, RequestID: requestID, AttemptNo: 1}
	if err := insertAttempt(ctx, mustTx(t, uow), att); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: requestID,
		Usage: domain.UsageRecord{
			RequestID: requestID, AttemptID: attemptID, Source: source,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 1_000,
		SettledAt:    time.Now().UTC(),
	}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// inflightTokens sums the account's in-flight TPM reservations.
func inflightTokens(t *testing.T, s *Store, accountID string) int64 {
	t.Helper()
	var v int64
	if err := s.db.Get(&v,
		`SELECT COALESCE(SUM(reserved_tokens),0) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account' AND state = 'reserved'`,
		accountID); err != nil {
		t.Fatalf("inflight tokens: %v", err)
	}
	return v
}

// settledTokensInWindow sums the account's settled TPM tokens inside the
// rolling window.
func settledTokensInWindow(t *testing.T, s *Store, accountID string) int64 {
	t.Helper()
	var v int64
	if err := s.db.Get(&v,
		`SELECT COALESCE(SUM(settled_tokens),0) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account' AND state = 'settled'
		   AND settled_at > now() - interval '60 seconds'`,
		accountID); err != nil {
		t.Fatalf("settled tokens: %v", err)
	}
	return v
}

// windowRateCount counts the account's rate reservations in the window.
func windowRateCount(t *testing.T, s *Store, accountID string) int {
	t.Helper()
	var n int
	if err := s.db.Get(&n,
		`SELECT COUNT(*) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account'
		   AND counted_at > now() - interval '60 seconds'`,
		accountID); err != nil {
		t.Fatalf("rate count: %v", err)
	}
	return n
}

// correctRateTokens applies one settlement correction to the request's
// rate reservation.
func correctRateTokens(t *testing.T, s *Store, requestID string, tokens int64) error {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CorrectRateTokensTx(ctx, uow, requestID, tokens); err != nil {
		_ = uow.Rollback(ctx)
		return err
	}
	return uow.Commit(ctx)
}

// TestRateTPM_InFlightHoldSurvivesTheWindow: 长流跨过 60 秒滚动窗口后，
// 在途预占**仍然**占着 TPM 额度（不能靠 TTL/窗口释放）；窗口滚动后
// RPM 计数归零，但 TPM 依旧被在途预占阻断。
func TestRateTPM_InFlightHoldSurvivesTheWindow(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 第一个请求在途，预占 60 token（账户 TPM=100）。
	if _, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 100}, 60); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// 把计数时刻推到窗口之外（模拟长流跑了一分钟以上）。
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET counted_at = now() - interval '90 seconds'`); err != nil {
		t.Fatal(err)
	}
	if got := windowRateCount(t, s, f.accountID); got != 0 {
		t.Errorf("account rate count = %d, want 0 (the RPM window rolled over)", got)
	}
	if got := inflightTokens(t, s, f.accountID); got != 60 {
		t.Errorf("in-flight tokens = %d, want 60 (an in-flight hold never expires with the window)", got)
	}
	// 在途 60 + 新请求 50 = 110 > 100 → 仍然被拒。
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 100_000)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID:        adm.RequestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           domain.RateLimits{PolicyTPM: 100},
		ReservedTokens:   50,
	})
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("err = %v, want rate_limited (the in-flight hold survives the window)", err)
	}
}

// TestRateSettle_DuplicateSettlementDoesNotDoubleCountTheTokens: 重复结算
// 是幂等的（唯一键），速率侧同样不得双记（不漏限、不双校正）。
func TestRateSettle_DuplicateSettlementDoesNotDoubleCountTheTokens(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	ctx := context.Background()

	requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 10_000}, 60)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	settleRequest(t, s, requestID, domain.UsageReported, 10, 10)
	usage, err := s.LoadUsageRecords(ctx, requestID)
	if err != nil || len(usage) != 1 {
		t.Fatalf("settled usage = %+v, err = %v; want one record", usage, err)
	}

	// 第二次结算：同一请求 → 已终态的冲突（typed），不得双记 token。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID:    requestID,
		Usage:        usage[0],
		ChargeMicros: 1_000, SettledAt: time.Now().UTC(),
	})
	_ = uow.Rollback(ctx)
	if err == nil {
		t.Fatal("a duplicate settlement must be refused")
	}
	if se, ok := domain.AsSettleError(err); !ok || se.Kind != domain.SettleOutcomeDuplicate {
		t.Fatalf("err = %v, want a typed duplicate outcome", err)
	}
	row := loadRateRows(t, s, requestID)[0]
	if row.SettledTokens != 20 {
		t.Errorf("settled tokens = %d, want 20 (a duplicate settlement must not double count)", row.SettledTokens)
	}
}
