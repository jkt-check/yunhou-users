package workers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/model"
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

// --- 审查修复 Minor-5：良性乐观竞争（racing）立即重排，不吃失败退避 -------

// racingStore 在 worker 的 InsertEntitlementTx 前让"并发收敛者"抢先发放同
// 一来源权益，制造真实的 UNIQUE 冲突 → racing。
type racingStore struct {
	*postgres.Store
	once    sync.Once
	collide func()
}

func (s *racingStore) InsertEntitlementTx(ctx context.Context, w domain.UnitOfWork, e *domain.Entitlement) error {
	s.once.Do(s.collide)
	return s.Store.InsertEntitlementTx(ctx, w, e)
}

func TestEntitlementSync_RacingRescheduledImmediately(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	orderID := f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", futureExpiry(30))
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonPaymentPaid, OrderID: orderID, PaymentID: "pay-race",
	}, nil)

	rs := &racingStore{Store: f.store}
	rs.collide = func() {
		// 并发收敛者用真实路径抢先发放（worker 的插入随后撞唯一键）。
		if _, err := f.worker.SyncUserProduct(ctx, f.userID, model.ProductCodingPlan, ""); err != nil {
			panic(err)
		}
	}
	w := NewEntitlementSync(rs, nil, EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.Racing != 1 || stats.Delivered != 0 {
		t.Fatalf("stats = %+v, want 1 racing 0 delivered", stats)
	}
	// 立即重排：next_retry_at 不得退避到未来（失败路径才 +5s 起）。
	var delayed bool
	if err := f.db.Get(&delayed,
		`SELECT next_retry_at > now() FROM inference_outbox WHERE topic = $1`, access.TopicEntitlementSync); err != nil {
		t.Fatal(err)
	}
	if delayed {
		t.Error("racing message must be rescheduled immediately — 良性竞争不吃失败退避")
	}

	// 下一轮重读已收敛状态 → noop 投递（delivered 标记不被不必要延迟）。
	stats, err = w.RunPass(ctx)
	if err != nil {
		t.Fatalf("converge pass: %v", err)
	}
	if stats.Delivered != 1 || stats.Noops == 0 {
		t.Fatalf("converge stats = %+v, want delivered noop", stats)
	}
}
