package service

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/repo"
)

// payment_edges_test.go — Task 16 覆盖率补强：支付失败级联、全额退款吊销、
// 失败幂等（真实库 + mock 渠道客户端；成功/续费路径已在 payment_db_test）。

func TestPaymentFailed_PendingFlipsOrderFailed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentServiceWith(t, db, &wechat.Client{MockMode: true})
	uid := seedUser(t, db)
	ctx := context.Background()

	order, err := svc.CreateOrder(ctx, uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatal(err)
	}
	// 失败事件（未支付过）→ payment/order 双 failed，无订阅影响。
	res, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-fail-1", EventType: "TRANSACTION.PAY_FAILED",
		TransactionID: "wx-fail-1", OrderID: order.ID, Currency: "CNY", RawPayload: []byte(`{"mock":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	var pStatus, oStatus string
	if err := db.QueryRowxContext(ctx,
		`SELECT p.status, o.status FROM payments p JOIN orders o ON o.id = p.order_id WHERE o.id = $1`,
		order.ID).Scan(&pStatus, &oStatus); err != nil {
		t.Fatal(err)
	}
	if pStatus != "failed" || oStatus != "failed" {
		t.Errorf("payment/order = %s/%s, want failed/failed", pStatus, oStatus)
	}
	// 幂等：同一事件重投不重复处理（终态守卫 no-op）。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-fail-1", EventType: "TRANSACTION.PAY_FAILED",
		TransactionID: "wx-fail-1", OrderID: order.ID, Currency: "CNY", RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.GetContext(ctx, &n, `SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("failed pending payment created a subscription: %d rows", n)
	}
}

func TestPaymentFailed_AfterSuccessCancelsSubscription(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentServiceWith(t, db, &wechat.Client{MockMode: true})
	uid := seedUser(t, db)
	ctx := context.Background()

	order, err := svc.CreateOrder(ctx, uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-ok-1", EventType: "TRANSACTION.SUCCESS",
		TransactionID: "wx-ok-1", OrderID: order.ID, Amount: 19.9, Currency: "CNY", RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	var subStatus string
	if err := db.GetContext(ctx, &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("sub = %s, want active after success", subStatus)
	}
	// 罕见的"成功后失败"竞态：订阅按 (user, plan) 级联取消。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-fail-2", EventType: "TRANSACTION.PAY_FAILED",
		TransactionID: "wx-ok-1", OrderID: order.ID, RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.GetContext(ctx, &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if subStatus != "cancelled" {
		t.Errorf("sub after paid-failure = %s, want cancelled (级联)", subStatus)
	}
	// 审计在册。
	var auditAction string
	if err := db.QueryRowxContext(ctx,
		`SELECT action FROM audit_log WHERE action = 'subscription_deactivated_failed_payment' LIMIT 1`).
		Scan(&auditAction); err != nil {
		t.Fatalf("cascade audit missing: %v", err)
	}
}

func TestRefund_FullAmountRevokesSubscription_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentServiceWith(t, db, &wechat.Client{MockMode: true})
	uid := seedUser(t, db)
	ctx := context.Background()

	order, err := svc.CreateOrder(ctx, uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-ok-2", EventType: "TRANSACTION.SUCCESS",
		TransactionID: "wx-ok-2", OrderID: order.ID, Amount: 19.9, Currency: "CNY", RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	fireRefund := func(eventID string) {
		t.Helper()
		if _, err := svc.OnWebhook(ctx, WebhookEvent{
			Channel: "wechat_pay", EventID: eventID, EventType: "TRANSACTION.REFUND",
			TransactionID: "wx-ok-2", OrderID: order.ID, RefundAmount: 19.9, ExternalRefundID: "re-1", RawPayload: []byte(`{"mock":true}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	fireRefund("evt-rf-1")
	var subStatus, oStatus string
	if err := db.QueryRowxContext(ctx,
		`SELECT s.status, o.status FROM subscriptions s JOIN orders o ON o.user_id = s.user_id
		 WHERE s.user_id = $1 AND o.id = $2`, uid, order.ID).Scan(&subStatus, &oStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != "cancelled" {
		t.Errorf("sub after full refund = %s, want cancelled", subStatus)
	}
	if oStatus != "refunded" {
		t.Errorf("order = %s, want refunded", oStatus)
	}
	var refunds int
	if err := db.GetContext(ctx, &refunds, `SELECT COUNT(*) FROM refunds`); err != nil {
		t.Fatal(err)
	}
	// 重投（不同事件 ID 同一退款事实）→ 幂等（refunds 不重复）。
	fireRefund("evt-rf-2")
	var refunds2 int
	if err := db.GetContext(ctx, &refunds2, `SELECT COUNT(*) FROM refunds`); err != nil {
		t.Fatal(err)
	}
	if refunds2 != refunds {
		t.Errorf("refund replay created rows: %d → %d", refunds, refunds2)
	}
}

// 渠道补单（reconcile）：webhook 未到时 Confirm 走渠道查询——mock 客户端
// 报 NOTPAY 时拒绝确认（订单保持 pending，信任模型见集成测试同名下）。
func TestConfirm_WechatMockNotPayRejected(t *testing.T) {
	db := setupPaymentDB(t)
	// 不装 confirmVerifier 直通替身：真实验证门（mock 渠道报 NOTPAY → 拒绝）。
	svc := NewPaymentService(
		db,
		repo.NewOrderRepo(db), repo.NewPaymentRepo(db), repo.NewRefundRepo(db),
		repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewUserRepo(db),
		repo.NewWebhookEventRepo(db), repo.NewAuditLogRepo(db),
		&stubRefundAPI{}, &wechat.Client{MockMode: true}, 30*time.Minute)
	uid := seedUser(t, db)
	ctx := context.Background()

	order, err := svc.CreateOrder(ctx, uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(ctx, ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "wechat_pay", ExternalTxnID: "wx-notpay-1",
	}); err == nil {
		t.Fatal("confirm without upstream verification must be rejected")
	}
	var status string
	if err := db.GetContext(ctx, &status, `SELECT status FROM orders WHERE id = $1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("order = %s, want pending after rejected confirm", status)
	}
}
