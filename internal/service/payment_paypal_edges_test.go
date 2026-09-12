package service

import (
	"context"
	"testing"
	"time"
)

// payment_paypal_edges_test.go — Task 16 覆盖率补强：PayPal 续费链路
// （合成续费订单/订阅顺延/重放幂等/缺字段审计）。真实库；wechat 主链
// 路已在 payment_edges_test.go。

func TestPaypalRenewal_ExtendsAndReplaysIdempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	ctx := context.Background()

	// 种子：PayPal 渠道 active 订阅（external_subscription_id 定位商品）。
	oldExpiry := time.Now().UTC().Add(10 * 24 * time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, expires_at, product_code, external_subscription_id)
		VALUES ($1, 'monthly', 'active', $2, 'kaya-membership', 'I-PPSUB-1')`, uid, oldExpiry); err != nil {
		t.Fatal(err)
	}
	fire := func(eventID, txnID string) {
		t.Helper()
		hint := time.Now().UTC().Add(40 * 24 * time.Hour)
		if _, err := svc.OnWebhook(ctx, WebhookEvent{
			Channel: "paypal", EventID: eventID, EventType: "PAYMENT.SALE.COMPLETED",
			TransactionID: txnID, ExternalSubscriptionID: "I-PPSUB-1",
			Amount: 19.9, Currency: "USD", SubExpiresAt: &hint,
			RawPayload: []byte(`{"mock":true}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	fire("evt-pp-1", "pp-txn-1")

	// 订阅顺延（hint 40d 覆盖原 10d）+ 合成订单 paid。
	var newExpiry time.Time
	if err := db.GetContext(ctx, &newExpiry,
		`SELECT expires_at FROM subscriptions WHERE external_subscription_id = 'I-PPSUB-1'`); err != nil {
		t.Fatal(err)
	}
	if !newExpiry.After(oldExpiry.Add(24 * time.Hour)) {
		t.Errorf("renewal did not extend: %v → %v", oldExpiry, newExpiry)
	}
	var paidOrders int
	if err := db.GetContext(ctx, &paidOrders,
		`SELECT COUNT(*) FROM orders WHERE user_id = $1 AND status = 'paid'`, uid); err != nil {
		t.Fatal(err)
	}
	if paidOrders != 1 {
		t.Errorf("synthetic renewal orders = %d, want 1", paidOrders)
	}

	// 重放（换事件 ID 同支付事实）→ 不二次顺延（支付键去重）。
	expBefore := newExpiry
	fire("evt-pp-2", "pp-txn-1")
	if err := db.GetContext(ctx, &newExpiry,
		`SELECT expires_at FROM subscriptions WHERE external_subscription_id = 'I-PPSUB-1'`); err != nil {
		t.Fatal(err)
	}
	if !newExpiry.Equal(expBefore) {
		t.Errorf("replay extended again: %v → %v", expBefore, newExpiry)
	}
	var payments int
	if err := db.GetContext(ctx, &payments,
		`SELECT COUNT(*) FROM payments WHERE channel = 'paypal' AND external_txn_id = 'pp-txn-1'`); err != nil {
		t.Fatal(err)
	}
	if payments != 1 {
		t.Errorf("renewal payments = %d, want 1 (幂等)", payments)
	}
}

func TestPaypalRenewal_MissingExternalSubIDAudited(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	ctx := context.Background()

	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-noid", EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "pp-txn-x", Amount: 19.9, Currency: "USD",
		RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'paypal_renewal_missing_external_sub_id'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("missing-field audit = %d, want 1", n)
	}
	// 未知订阅 → 审计 unknown_subscription，无订阅影响。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-unknown", EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "pp-txn-y", ExternalSubscriptionID: "I-GHOST",
		Amount: 19.9, Currency: "USD", RawPayload: []byte(`{"mock":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'paypal_renewal_unknown_subscription'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("unknown-sub audit = %d, want 1", n)
	}
}
