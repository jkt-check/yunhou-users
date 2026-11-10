package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

// reservation_identity_test.go — 预占的**身份**核对（不只是数量）。
//
// 准入在请求行上钉死了金额预占的**目标身份**：每个金额窗口钉一个
// window_id、Key 预算钉一个 api_key_id（domain.ExpectedMonetaryTargets）。
// held 预占行必须与这些身份**一一对应**：同一条数但绑到别的窗口/别的 Key
// 的预占行不是准入创建的那笔预占 —— 结算会推进错误的窗口、释放会归还
// 错误的窗口，而请求照样终态化。
//
// 数量口径保持既有分类（少了 = missing_reservation，多了 =
// reservation_drift）；身份不符（错类型或错 ID）= typed reservation_drift：
// 恢复 worker 才能把它与"良性竞态"分开（普通 CodeConflict 会被当成竞态
// 静默跳过）。

// reserveWithHolds reserves one request holding exactly the given targets
// and returns its request id.
func reserveWithHolds(t *testing.T, s *Store, f fixture, holds []domain.HoldSpec) string {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.Reserve(ctx, uow, domain.ReserveCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
			PolicyVersionID: f.policyID,
		},
		AdmittedAt: time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC),
		Holds:      holds,
	})
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("reserve: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

// txWindowState reads a window's (used, reserved) inside the open tx — the
// state a refused settlement/release must not have touched.
func txWindowState(t *testing.T, tx *sqlx.Tx, id string) (used, reserved int64) {
	t.Helper()
	var u, r int64
	if err := tx.QueryRowxContext(context.Background(),
		`SELECT used_micros, reserved_micros FROM inference_quota_windows WHERE id = $1`, id).
		Scan(&u, &r); err != nil {
		t.Fatalf("window state: %v", err)
	}
	return u, r
}

// txReservationStates counts the held reservations of a request inside the
// open tx.
func txHeldReservations(t *testing.T, tx *sqlx.Tx, requestID string) int {
	t.Helper()
	var n int
	if err := tx.QueryRowxContext(context.Background(),
		`SELECT COUNT(*) FROM inference_reservations WHERE request_id = $1 AND state = 'held'`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("count held reservations: %v", err)
	}
	return n
}

// txRequestStatus reads the request status inside the open tx.
func txRequestStatus(t *testing.T, tx *sqlx.Tx, requestID string) string {
	t.Helper()
	var status string
	if err := tx.QueryRowxContext(context.Background(),
		`SELECT status FROM inference_requests WHERE id = $1`, requestID).Scan(&status); err != nil {
		t.Fatalf("request status: %v", err)
	}
	return status
}

// txCount counts rows of a query inside the open tx.
func txCount(t *testing.T, tx *sqlx.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRowxContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// TestSettle_ReservationBoundToAnotherWindow_ClassifiesReservationDrift —
// held 预占的**数量**与准入一致，但其中一行绑到了**另一个窗口**（数量口径
// 发现不了）：结算必须拒绝并给出 typed reservation_drift，绝不推进错误的
// 窗口、绝不入账、绝不把请求置为 settled。
func TestSettle_ReservationBoundToAnotherWindow_ClassifiesReservationDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false) // no Key → no Key-budget target
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 请求 A：钉住五小时窗口 + 周窗口。
	reqA := reserveWithHolds(t, s, f, []domain.HoldSpec{
		{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: 100_000},
		{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: 100_000},
	})
	// 请求 B：钉住月窗口（给"错误的窗口"留出可被误推进的预占）。
	reqB := reserveWithHolds(t, s, f, []domain.HoldSpec{
		{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: 100_000},
	})

	// 故障注入：A 的周窗口预占行被改成月窗口（数量不变，仍是 2 条）。
	if _, err := s.db.Exec(
		`UPDATE inference_reservations SET target_kind = 'window_monthly', window_id = $2
		  WHERE request_id = $1 AND target_kind = 'window_weekly'`, reqA, wm); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: reqA, AttemptNo: 1}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	in, out := int64(10), int64(5)
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqA,
		Usage: domain.UsageRecord{
			RequestID: reqA, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000, SettledAt: time.Now().UTC(),
	})
	if err == nil {
		t.Errorf("settle: succeeded — 绑错窗口的预占被照单结算了（必须拒绝并给出 reservation_drift）")
	}
	kind, ok := settleOutcomeKind(err)
	if !ok {
		t.Errorf("settle: err = %v, want a typed settlement outcome (reservation_drift)", err)
	} else if kind != "reservation_drift" {
		t.Errorf("outcome = %q, want reservation_drift (预占身份错绑必须可识别)", kind)
	}

	// 没有任何部分效果：用量事实、账本 charge、请求终态、窗口推进都不得发生。
	tx := mustTx(t, uow)
	if n := txCount(t, tx, `SELECT COUNT(*) FROM inference_usage_records WHERE request_id = $1`, reqA); n != 0 {
		t.Errorf("usage records = %d, want 0 (被拒绝的结算不得留下用量事实)", n)
	}
	if n := txCount(t, tx, `SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1`, reqA); n != 0 {
		t.Errorf("ledger entries = %d, want 0 (被拒绝的结算不得留下账本 charge)", n)
	}
	if st := txRequestStatus(t, tx, reqA); st == string(domain.ReqSettled) {
		t.Errorf("status = %s, want not settled (预占错绑时绝不把请求结算掉)", st)
	}
	used, reserved := txWindowState(t, tx, w5)
	if used != 0 || reserved != 100_000 {
		t.Errorf("five_hour window = used %d / reserved %d, want 0 / 100000 (自己的预占不得被推进)", used, reserved)
	}
	used, reserved = txWindowState(t, tx, ww)
	if used != 0 || reserved != 100_000 {
		t.Errorf("weekly window = used %d / reserved %d, want 0 / 100000 (周窗口不得被推进)", used, reserved)
	}
	used, reserved = txWindowState(t, tx, wm)
	if used != 0 || reserved != 100_000 {
		t.Errorf("monthly window = used %d / reserved %d, want 0 / 100000 (错误的窗口绝不能被推进)", used, reserved)
	}
	if n := txHeldReservations(t, tx, reqB); n != 1 {
		t.Errorf("request B held reservations = %d, want 1 (别的请求的预占不得被牵连)", n)
	}
	_ = uow.Rollback(ctx)
}

// TestRelease_ReservationBoundToAnotherWindow_ClassifiesReservationDrift
// — 同样的身份错绑在释放路径上同样必须被识别：释放绝不能归还**错误的
// 窗口**（那会让另一个请求的预占凭空消失）、绝不能把请求终态化。
func TestRelease_ReservationBoundToAnotherWindow_ClassifiesReservationDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	w5, ww, wm := makeWindows(t, s, f.entID)

	reqA := reserveWithHolds(t, s, f, []domain.HoldSpec{
		{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: 100_000},
		{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: 100_000},
	})
	reqB := reserveWithHolds(t, s, f, []domain.HoldSpec{
		{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: 100_000},
	})

	// 故障注入：A 的五小时预占行被改成月窗口（数量不变，仍是 2 条）。
	if _, err := s.db.Exec(
		`UPDATE inference_reservations SET target_kind = 'window_monthly', window_id = $2
		  WHERE request_id = $1 AND target_kind = 'window_five_hour'`, reqA, wm); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Release(ctx, uow, reqA)
	if err == nil {
		t.Errorf("release: succeeded — 绑错窗口的预占被照单释放了（必须拒绝并给出 reservation_drift）")
	}
	kind, ok := settleOutcomeKind(err)
	if !ok {
		t.Errorf("release: err = %v, want a typed settlement outcome (reservation_drift)", err)
	} else if kind != "reservation_drift" {
		t.Errorf("outcome = %q, want reservation_drift (预占身份错绑必须可识别)", kind)
	}

	tx := mustTx(t, uow)
	if st := txRequestStatus(t, tx, reqA); st == string(domain.ReqReleased) {
		t.Errorf("status = %s, want not released (预占错绑时绝不能终态化)", st)
	}
	if n := txHeldReservations(t, tx, reqA); n != 2 {
		t.Errorf("held reservations = %d, want 2 (被拒绝的释放不得释放任何预占)", n)
	}
	used, reserved := txWindowState(t, tx, w5)
	if used != 0 || reserved != 100_000 {
		t.Errorf("five_hour window = used %d / reserved %d, want 0 / 100000 (自己的预占不得被释放)", used, reserved)
	}
	used, reserved = txWindowState(t, tx, ww)
	if used != 0 || reserved != 100_000 {
		t.Errorf("weekly window = used %d / reserved %d, want 0 / 100000 (周窗口不得被释放)", used, reserved)
	}
	used, reserved = txWindowState(t, tx, wm)
	if used != 0 || reserved != 100_000 {
		t.Errorf("monthly window = used %d / reserved %d, want 0 / 100000 (错误的窗口绝不能被释放)", used, reserved)
	}
	if n := txHeldReservations(t, tx, reqB); n != 1 {
		t.Errorf("request B held reservations = %d, want 1 (别的请求的预占不得被牵连)", n)
	}
	_ = uow.Rollback(ctx)
}

// TestSettle_KeyBudgetReservationBoundToAnotherKey_ClassifiesReservationDrift
// — 同一账户的**另一把 Key** 出现在 Key 预算预占行上（数量对得上、种类也
// 对得上，只有身份错了）：结算必须按 typed reservation_drift 拒绝 —— 绝不
// 去修正别的 Key 的预算（那会把预占记到别人的账上），也绝不让请求终态化。
func TestSettle_KeyBudgetReservationBoundToAnotherKey_ClassifiesReservationDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 同一账户的另一把 Key（预占行被错绑到它身上）。
	otherKey := &domain.APIKey{
		BillingAccountID: f.accountID, Name: "other", Prefix: "yk-other-" + uuid.NewString()[:8],
		BudgetLimit: micro(5_000_000), BudgetUsed: 200_000,
	}
	if err := s.InsertAPIKey(ctx, otherKey, "sha256:"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 故障注入：Key 预算预占行绑到了另一把 Key（数量/种类都不变）。
	if _, err := s.db.Exec(
		`UPDATE inference_reservations SET api_key_id = $2
		  WHERE request_id = $1 AND target_kind = 'key_budget'`, adm.RequestID, otherKey.ID); err != nil {
		t.Fatal(err)
	}

	uow2, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(10), int64(5)
	err = s.Settle(ctx, uow2, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000, SettledAt: time.Now().UTC(),
	})
	if err == nil {
		t.Errorf("settle: succeeded — 绑到别的 Key 的预占被照单结算了（必须拒绝并给出 reservation_drift）")
	}
	kind, ok := settleOutcomeKind(err)
	if !ok {
		t.Errorf("settle: err = %v, want a typed settlement outcome (reservation_drift)", err)
	} else if kind != "reservation_drift" {
		t.Errorf("outcome = %q, want reservation_drift (Key 预算预占身份错绑必须可识别)", kind)
	}

	tx := mustTx(t, uow2)
	if n := txCount(t, tx, `SELECT COUNT(*) FROM inference_usage_records WHERE request_id = $1`, adm.RequestID); n != 0 {
		t.Errorf("usage records = %d, want 0 (被拒绝的结算不得留下用量事实)", n)
	}
	if n := txCount(t, tx, `SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1`, adm.RequestID); n != 0 {
		t.Errorf("ledger entries = %d, want 0 (被拒绝的结算不得留下账本 charge)", n)
	}
	if st := txRequestStatus(t, tx, adm.RequestID); st == string(domain.ReqSettled) {
		t.Errorf("status = %s, want not settled (预占错绑时绝不把请求结算掉)", st)
	}
	var otherUsed int64
	if err := tx.QueryRowxContext(ctx,
		`SELECT budget_used_micros FROM inference_api_keys WHERE id = $1`, otherKey.ID).Scan(&otherUsed); err != nil {
		t.Fatal(err)
	}
	if otherUsed != 200_000 {
		t.Errorf("other key budget_used = %d, want 200000 (绝不去修正别的 Key 的预算)", otherUsed)
	}
	_ = uow2.Rollback(ctx)
}

// TestSettleWallet_NonHeldFreeze_ClassifiesReservationDrift — 钱包冻结行
// 存在但**不是 held**（已被释放/结算）：那不是"准入时应当存在的预占不见
// 了"（missing），而是预占状态漂移 —— 必须是 typed reservation_drift，且
// 在任何写入之前拒绝（不得留下用量事实/账本 charge/钱包消费分录）。
func TestSettleWallet_NonHeldFreeze_ClassifiesReservationDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	requestID, attemptID, _ := walletAdmission(t, s)

	// 故障注入：冻结行不再是 held（已释放），mirrored 预占仍在。
	if _, err := s.db.Exec(
		`UPDATE inference_wallet_holds SET state = 'released', released_at = now() WHERE request_id = $1`,
		requestID); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Settle(ctx, uow, walletSettleCmd(t, requestID, attemptID, 1_500))
	if err == nil {
		t.Errorf("settle wallet: succeeded — 非 held 的冻结被照单结算了（必须拒绝并给出 reservation_drift）")
	}
	se, ok := domain.AsSettleError(err)
	if !ok {
		t.Errorf("settle wallet: err = %v, want a typed settlement outcome (reservation_drift)", err)
	} else if se.Kind != domain.SettleOutcomeReservationDrift {
		t.Errorf("outcome = %q, want reservation_drift (非 held 的冻结 = 预占状态漂移)", se.Kind)
	}
	assertNoPartialSettlement(t, mustTx(t, uow), requestID)
	_ = uow.Rollback(ctx)
}
