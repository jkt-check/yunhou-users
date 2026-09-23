package workers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
)

// wallet_sync_edges_test.go — Task 16 覆盖率补强：wallet-sync
// processMessage 的分支（未知 kind / 缺 refund_id / 业务键重放后标记交
// 付 / 畸形载荷退避不丢弃）。主路径已在 wallet_sync_test.go。

func enqueueWalletEdge(t *testing.T, f *workerFixture, payload any) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnqueueOutbox(ctx, uow, access.TopicWalletSync, raw, nil); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWalletSync_UnknownKindAndMissingRefundID(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)

	// 未知 kind → Failed + 退避（不丢弃）。
	enqueueWalletEdge(t, f, access.WalletSyncMessage{
		Kind: "lottery", UserID: userID, PaymentID: "pay-x", Currency: "CNY", AmountMicros: 100,
	})
	// refund 缺 refund_id → Failed + 退避。
	enqueueWalletEdge(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: userID, PaymentID: "pay-y", Currency: "CNY", AmountMicros: 100,
	})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 2 || stats.Delivered != 0 {
		t.Fatalf("stats = %+v, want 2 failed 0 delivered", stats)
	}
	var pending int
	if err := f.db.Get(&pending,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = $1 AND status = 'pending'`, access.TopicWalletSync); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Errorf("failed messages dropped: pending = %d, want 2 (慢车道)", pending)
	}
}

func TestWalletSync_BusinessKeyReplayMarksDelivered(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)

	// 先入队一条充值并消费（业务键生效）。
	msg := access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: userID, PaymentID: "pay-rp",
		OrderID: uuid.NewString(), Currency: "CNY", AmountMicros: 5000,
	}
	enqueueWalletEdge(t, f, msg)
	stats, err := w.RunPass(ctx)
	if err != nil || stats.Credited != 1 {
		t.Fatalf("first pass = %+v/%v", stats, err)
	}
	// 同一 payment 的第二条消息（不同 outbox 行）→ 业务键冲突 →
	// Replays + Delivered（单独事务标记交付，不叠加金额）。
	enqueueWalletEdge(t, f, msg)
	stats, err = w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Replays != 1 || stats.Delivered != 1 || stats.Credited != 0 {
		t.Fatalf("replay pass = %+v, want replays=1 delivered=1 credited=0", stats)
	}
	bal := walletBalance(t, f, "CNY")
	if bal.CashAvailable != 5000 {
		t.Errorf("cash = %d, want 5000 (重放不叠加)", bal.CashAvailable)
	}
	// 两条 outbox 都已交付。
	var pending int
	if err := f.db.Get(&pending,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = $1 AND status = 'pending'`, access.TopicWalletSync); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("replayed message not marked delivered: pending = %d", pending)
	}
}

func TestWalletSync_MalformedPayloadBranches(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{})
	userID := fUserID(t, f)

	// 不可解码（合法 JSON 非消息形状）。
	enqueueWalletEdge(t, f, map[string]any{"kind": 42})
	// 可解码但缺字段（AmountMicros=0）。
	enqueueWalletEdge(t, f, access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: userID, PaymentID: "pay-z", Currency: "CNY",
	})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 2 {
		t.Fatalf("stats = %+v, want 2 failed", stats)
	}
}
