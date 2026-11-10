package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

// wallet_missing_hold_test.go — 钱包结算路径的金额预占核对（T2 typed 分类）。
//
// 钱包准入创建了**一条**金额预占：inference_reservations 里的 mirrored
// wallet 预占（target_kind='wallet'），以及它镜像的冻结行
// inference_wallet_holds。结算必须**先**核对这笔预占，再写 usage/速率/账本：
//
//   - 少了 → missing_reservation（准入时应当存在的金额预占被删除 = 账务
//     异常，必须可识别）；
//   - 多了 → reservation_drift；
//   - 冻结行不见了 → 同一 typed missing。
//
// 否则钱包路径只会落成普通 NotFound/Conflict，调用方只能重试后以
// unknown_usage 停放，"本应存在的金额预占被删除"就淹没在通用故障里。

// walletAdmission reserves one wallet-funded request: a frozen money hold,
// the mirrored wallet reservation and an in-flight rate hold. It returns
// (requestID, attemptID, apiKeyID).
func walletAdmission(t *testing.T, s *Store) (requestID, attemptID, apiKeyID string) {
	t.Helper()
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 100_000_000, 0)
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 2_000_000))
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("reserve wallet: %v", err)
	}
	attemptID = uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	// 速率预占（准入同事务创建的 TPM 在途预占）：结算会把它推进为 settled，
	// 失败的结算绝不能推进它。
	if _, err := mustTx(t, uow).ExecContext(ctx,
		`INSERT INTO inference_rate_reservations
		 (request_id, billing_account_id, scope, counted_at, reserved_tokens, state)
		 VALUES ($1, $2, 'account', now(), 1000, 'reserved')`, adm.RequestID, f.accountID); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("insert rate reservation: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID, attemptID, f.keyID
}

// walletSettleCmd is one settlement delivery of a wallet request.
func walletSettleCmd(t *testing.T, requestID, attemptID string, micros int64) domain.SettleCommand {
	t.Helper()
	in, out := int64(1000), int64(500)
	money, err := domain.NewMoney(micros, "CNY")
	if err != nil {
		t.Fatalf("money: %v", err)
	}
	return domain.SettleCommand{
		RequestID: requestID,
		Usage: domain.UsageRecord{
			RequestID: requestID, AttemptID: attemptID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: domain.Microcredit(micros),
		WalletCharge: &money,
		SettledAt:    time.Now().UTC(),
	}
}

// assertNoPartialSettlement inspects the STILL OPEN transaction: a refused
// settlement must not have written anything — no usage fact, no ledger
// charge, no wallet consume entry — and must not have advanced the money
// hold or the rate hold.
func assertNoPartialSettlement(t *testing.T, tx *sqlx.Tx, requestID string) {
	t.Helper()
	ctx := context.Background()
	count := func(query string) int {
		t.Helper()
		var n int
		if err := tx.QueryRowxContext(ctx, query, requestID).Scan(&n); err != nil {
			t.Fatalf("count (%s): %v", query, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM inference_usage_records WHERE request_id = $1`); n != 0 {
		t.Errorf("usage records = %d, want 0 (失败的结算不得留下用量事实)", n)
	}
	if n := count(`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1`); n != 0 {
		t.Errorf("ledger entries = %d, want 0 (失败的结算不得留下账本 charge)", n)
	}
	if n := count(`SELECT COUNT(*) FROM inference_wallet_entries WHERE request_id = $1`); n != 0 {
		t.Errorf("wallet entries = %d, want 0 (失败的结算不得留下钱包消费分录)", n)
	}
	var rateState string
	var reserved int64
	if err := tx.QueryRowxContext(ctx,
		`SELECT state, reserved_tokens FROM inference_rate_reservations
		  WHERE request_id = $1 AND scope = 'account'`, requestID).
		Scan(&rateState, &reserved); err != nil {
		t.Fatalf("read rate reservation: %v", err)
	}
	if rateState != "reserved" || reserved != 1000 {
		t.Errorf("rate reservation = %s/%d, want reserved/1000 (失败的结算不得推进速率预占)", rateState, reserved)
	}
}

// assertWalletHoldUnchanged asserts the money freeze still holds what it
// held (the hold row itself may have been deleted by the fault injection —
// then there is nothing left to advance).
func assertWalletHoldUnchanged(t *testing.T, tx *sqlx.Tx, requestID string, wantAmount int64) {
	t.Helper()
	ctx := context.Background()
	var state string
	var amount int64
	err := tx.QueryRowxContext(ctx,
		`SELECT state, amount_micros FROM inference_wallet_holds WHERE request_id = $1`, requestID).
		Scan(&state, &amount)
	if err != nil {
		return // the hold row is gone — the fault injection removed it
	}
	if state != "held" || amount != wantAmount {
		t.Errorf("wallet hold = %s/%d, want held/%d (失败的结算不得推进金额预占)", state, amount, wantAmount)
	}
}

// TestSettleWallet_MissingWalletHold_ClassifiesMissingReservation — 钱包
// 冻结行不见了 = 准入时应当存在的金额预占被删除：结算必须返回可识别的
// typed missing_reservation，绝不是普通 NotFound（调用方会把它当"没找到"
// 吞掉），也绝不留下任何部分效果。
func TestSettleWallet_MissingWalletHold_ClassifiesMissingReservation(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	requestID, attemptID, _ := walletAdmission(t, s)

	// 故障注入：冻结行被删除（mirrored 预占还在）。
	if _, err := s.db.Exec(
		`DELETE FROM inference_wallet_holds WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Settle(ctx, uow, walletSettleCmd(t, requestID, attemptID, 1_500))
	if err == nil {
		_ = uow.Rollback(ctx)
		t.Fatal("settle: want a refusal (the wallet hold is missing)")
	}
	se, ok := domain.AsSettleError(err)
	if !ok {
		_ = uow.Rollback(ctx)
		t.Fatalf("settle: err = %v, want a typed settlement outcome (missing_reservation)", err)
	}
	if se.Kind != domain.SettleOutcomeMissingReservation {
		t.Errorf("outcome = %q, want missing_reservation (缺预占必须可识别)", se.Kind)
	}
	assertNoPartialSettlement(t, mustTx(t, uow), requestID)
	_ = uow.Rollback(ctx)
}

// TestSettleWallet_MissingMirroredReservation_ClassifiesMissingReservation
// — mirrored wallet 预占不见了（准入时应当存在的金额预占被删除）：同样是可
// 识别的 typed missing_reservation，且事务里没有任何部分效果（用量事实、
// 账本 charge、钱包消费分录都没有写，金额与速率预占也没有被推进）。
func TestSettleWallet_MissingMirroredReservation_ClassifiesMissingReservation(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	requestID, attemptID, _ := walletAdmission(t, s)

	// 故障注入：mirrored wallet 预占被删除（冻结行还在）。
	if _, err := s.db.Exec(
		`DELETE FROM inference_reservations WHERE request_id = $1 AND target_kind = 'wallet'`,
		requestID); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Settle(ctx, uow, walletSettleCmd(t, requestID, attemptID, 1_500))
	if err == nil {
		_ = uow.Rollback(ctx)
		t.Fatal("settle: want a refusal (the mirrored wallet reservation is missing)")
	}
	se, ok := domain.AsSettleError(err)
	if !ok {
		_ = uow.Rollback(ctx)
		t.Fatalf("settle: err = %v, want a typed settlement outcome (missing_reservation)", err)
	}
	if se.Kind != domain.SettleOutcomeMissingReservation {
		t.Errorf("outcome = %q, want missing_reservation (缺预占必须可识别)", se.Kind)
	}
	assertNoPartialSettlement(t, mustTx(t, uow), requestID)
	assertWalletHoldUnchanged(t, mustTx(t, uow), requestID, 2_000_000)
	_ = uow.Rollback(ctx)
}

// TestSettleWallet_ExtraHeldReservation_ClassifiesReservationDrift — 钱包
// 请求的 held 预占多于准入创建的那一条 = 预占漂移：同样必须可识别
// (reservation_drift)，且不得留下任何部分效果。
func TestSettleWallet_ExtraHeldReservation_ClassifiesReservationDrift(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	requestID, attemptID, keyID := walletAdmission(t, s)

	// 故障注入：多出一条不属于该准入的 held 预占（准入只创建一条 wallet）。
	if _, err := s.db.Exec(
		`INSERT INTO inference_reservations (id, request_id, target_kind, api_key_id, amount_micros, state)
		 VALUES ($1, $2, 'key_budget', $3, 1000, 'held')`, uuid.NewString(), requestID, keyID); err != nil {
		t.Fatal(err)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Settle(ctx, uow, walletSettleCmd(t, requestID, attemptID, 1_500))
	if err == nil {
		_ = uow.Rollback(ctx)
		t.Fatal("settle: want a refusal (more held reservations than the admission created)")
	}
	se, ok := domain.AsSettleError(err)
	if !ok {
		_ = uow.Rollback(ctx)
		t.Fatalf("settle: err = %v, want a typed settlement outcome (reservation_drift)", err)
	}
	if se.Kind != domain.SettleOutcomeReservationDrift {
		t.Errorf("outcome = %q, want reservation_drift (预占漂移必须可识别)", se.Kind)
	}
	assertNoPartialSettlement(t, mustTx(t, uow), requestID)
	assertWalletHoldUnchanged(t, mustTx(t, uow), requestID, 2_000_000)
	_ = uow.Rollback(ctx)
}
