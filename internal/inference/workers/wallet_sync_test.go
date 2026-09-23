// wallet_sync_test.go — Task 14 钱包同步 worker 验收（真实 PostgreSQL）：
// 充值入账 / 重复回调与 worker 重投幂等（dedup 键 + 业务键双兜底）/ 退款
// 现金原路退 / 赠送不参与退款 / 乱序退款重投不丢失。

package workers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/postgres"
)

// enqueueWalletMsg writes one wallet.sync message through the outbox (the
// payment pipeline's shape: dedup key optional).
func enqueueWalletMsg(t *testing.T, f *workerFixture, msg access.WalletSyncMessage, dedupKey *string) {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnqueueOutbox(ctx, uow, access.TopicWalletSync, payload, dedupKey); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func walletBalance(t *testing.T, f *workerFixture, currency string) accounting.WalletBalance {
	t.Helper()
	acct, err := f.store.GetBillingAccountByUser(context.Background(), fUserID(t, f))
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.store.WalletBalance(context.Background(), acct.ID, currency, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v.Balance
}

// fUserID recovers the fixture's user id (workerFixture keys on account).
func fUserID(t *testing.T, f *workerFixture) string {
	t.Helper()
	var userID string
	if err := f.db.Get(&userID, `SELECT user_id FROM inference_billing_accounts WHERE id = $1`, f.accountID); err != nil {
		t.Fatal(err)
	}
	return userID
}

// TestWalletSync_TopupAndDuplicateReplay: 充值入账一次；dedup 键吸收入队
// 重投，业务键吸收重复消息（重复回调只入账一次）。
func TestWalletSync_TopupAndDuplicateReplay(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)
	paymentID := "pay-" + uuid.NewString()
	msg := access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: paymentID, AmountMicros: 29_990_000, Currency: "CNY",
	}

	dedup := access.WalletTopupDedupKey(paymentID)
	enqueueWalletMsg(t, f, msg, &dedup)
	// 同一 dedup 键再入队 → ON CONFLICT 吸收（行数不增）。
	enqueueWalletMsg(t, f, msg, &dedup)
	// 无 dedup 键的重复投递（补单/重试路径）→ 业务键兜底。
	enqueueWalletMsg(t, f, msg, nil)

	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Replays != 1 || stats.Delivered != 2 {
		t.Fatalf("stats = %+v, want credited 1 replay 1 delivered 2", stats)
	}
	if bal := walletBalance(t, f, "CNY"); bal.CashAvailable != 29_990_000 {
		t.Fatalf("cash = %d, want 29.99 CNY once", bal.CashAvailable)
	}
	// 全部交付后队列清空；再跑一轮空转。
	stats, err = w.RunPass(ctx)
	if err != nil || stats.Fetched != 0 {
		t.Fatalf("second pass = %+v, %v", stats, err)
	}
}

// TestWalletSync_RefundCashOnly: 退款只借记现金来源；只有赠送余额的账户
// 被退款时现金如实转负、赠送一分不动（赠送不得伪装现金退款，裁决 7）。
func TestWalletSync_RefundCashOnly(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)
	acct, _ := f.store.GetBillingAccountByUser(ctx, userID)

	// 先充值 30，再退 5 → 现金 25。
	payID := "pay-" + uuid.NewString()
	enqueueWalletMsg(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: payID, AmountMicros: 30_000_000, Currency: "CNY",
	}, nil)
	refID := "ref-" + uuid.NewString()
	refund := access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: payID, RefundID: refID, AmountMicros: 5_000_000, Currency: "CNY",
	}
	enqueueWalletMsg(t, f, refund, nil)
	enqueueWalletMsg(t, f, refund, nil) // 重复退款事件
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Refunded != 1 || stats.Replays != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if bal := walletBalance(t, f, "CNY"); bal.CashAvailable != 25_000_000 || bal.BonusAvailable != 0 {
		t.Fatalf("balance = %+v, want cash 25 / bonus 0", bal)
	}

	// 纯赠送账户的现金退款：现金转负如实入账，赠送不动。
	uow, _ := f.store.Begin(ctx)
	if _, err := f.store.ApplyWalletAdjustmentTx(ctx, uow, postgres.WalletAdjustmentCommand{
		AccountID: acct.ID, Currency: "CNY",
		Source: accounting.WalletBonus, Direction: accounting.DirCredit,
		AmountMicros: 8_000_000, Reason: "gift",
		OperatorSubject: "user:ops@app:test", IdempotencyKey: "gift-" + uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	enqueueWalletMsg(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: payID, RefundID: "ref-" + uuid.NewString(), AmountMicros: 30_000_000, Currency: "CNY",
	}, nil)
	if _, err := w.RunPass(ctx); err != nil {
		t.Fatal(err)
	}
	bal := walletBalance(t, f, "CNY")
	if bal.CashAvailable != -5_000_000 || bal.BonusAvailable != 8_000_000 {
		t.Fatalf("balance = %+v, want cash -5 (honest overdraft) / bonus 8 untouched", bal)
	}
}

// TestWalletSync_RefundBeforeTopup_RetriesNotDropped: 退款消息先于充值到
// 达（乱序）→ 当轮失败进退避；充值入账后重投成功，退款不丢失。
func TestWalletSync_RefundBeforeTopup_RetriesNotDropped(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	// 真实时钟：失败消息的退避（5s×2^attempts）按真实时间排程；测试用
	// UPDATE next_retry_at 模拟到期（生产由退避自然到期）。
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)
	payID := "pay-" + uuid.NewString()

	enqueueWalletMsg(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: payID, RefundID: "ref-" + uuid.NewString(), AmountMicros: 5_000_000, Currency: "CNY",
	}, nil)
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.Refunded != 0 {
		t.Fatalf("first pass = %+v, want failed 1 refunded 0", stats)
	}

	enqueueWalletMsg(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: userID, OrderID: uuid.NewString(),
		PaymentID: payID, AmountMicros: 30_000_000, Currency: "CNY",
	}, nil)
	stats, err = w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 {
		t.Fatalf("second pass = %+v, want credit 1", stats)
	}
	// 退避中的退款消息到期（测试老化 next_retry_at；生产由退避自然到期）。
	if _, err := f.db.Exec(`UPDATE inference_outbox SET next_retry_at = now() - interval '1 second' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	stats, err = w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Refunded != 1 {
		t.Fatalf("third pass = %+v, want refund 1 (乱序收敛不丢失)", stats)
	}
	if bal := walletBalance(t, f, "CNY"); bal.CashAvailable != 25_000_000 {
		t.Fatalf("cash = %d, want 25 CNY", bal.CashAvailable)
	}
}

// TestWalletSync_MalformedMessage: 畸形/未知种类消息进入退避，不崩溃、
// 不交付、不入账。
func TestWalletSync_MalformedMessage(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)

	enqueueWalletMsg(t, f, access.WalletSyncMessage{
		Kind: "mystery", UserID: userID, PaymentID: "pay-x",
		AmountMicros: 1, Currency: "CNY",
	}, nil)
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.Delivered != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	var entries int
	if err := f.db.Get(&entries, `SELECT COUNT(*) FROM inference_wallet_entries`); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatalf("no entry may exist, got %d", entries)
	}
}
