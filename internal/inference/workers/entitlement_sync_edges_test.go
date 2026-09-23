package workers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
)

// entitlement_sync_edges_test.go — Task 16 覆盖率补强：entitlement-sync
// processMessage 的失败/退避分支（畸形消息、plan 失败、消息永不丢弃）。
// 发放/收敛主路径已在 entitlement_sync_test.go。

func TestEntitlementSync_MalformedMessageRescheduledNotDropped(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	w := NewEntitlementSync(f.store, nil, EntitlementSyncConfig{})

	// 不可解码 payload（合法 JSON 但不是消息形状）→ Failed + 退避
	// （不投递、不丢弃；jsonb 列保证"非 JSON"在入队即被拒）。
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnqueueOutbox(ctx, uow, access.TopicEntitlementSync,
		json.RawMessage(`{"user_id":123,"product_code":5}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.Delivered != 0 {
		t.Fatalf("stats = %+v, want 1 failed 0 delivered", stats)
	}
	// 消息仍在 pending（退避到未来 —— 立即可见的下一批取不到）。
	var nextRetrySet bool
	if err := f.db.Get(&nextRetrySet,
		`SELECT next_retry_at > now() FROM inference_outbox WHERE topic = $1`, access.TopicEntitlementSync); err != nil {
		t.Fatal(err)
	}
	if !nextRetrySet {
		t.Error("malformed message must be rescheduled with backoff (永不丢弃)")
	}
}

// 结构合法但目标不存在 → 收敛为空计划 noop 投递（不重试风暴）。
func TestEntitlementSync_UnknownUserNoopsDelivered(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	w := NewEntitlementSync(f.store, nil, EntitlementSyncConfig{})

	payload, err := json.Marshal(access.EntitlementSyncMessage{
		UserID: uuid.NewString(), ProductCode: "coding-plan", Reason: "payment.paid",
	})
	if err != nil {
		t.Fatal(err)
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnqueueOutbox(ctx, uow, access.TopicEntitlementSync, payload, nil); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Delivered != 1 || stats.Noops != 1 || stats.Granted != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want delivered/noop（零效果收敛）", stats)
	}
}
