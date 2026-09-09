package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/workers"
	"github.com/yunhou/users/internal/repo"
)

// payment_wallet_db_test.go — Task 14 验收（支付域 × 真实库）：余额充值商
// 品独立定义（不把充值金额当订阅有效期）；已支付回调按订单快照金额入队
// 钱包充值（同事务 outbox + dedup 幂等）；重复回调/退款只入账一次；退款
// 现金原路退。

// seedTopupPlan inserts a sellable wallet-topup plan (无权益配置要求).
func seedTopupPlan(t *testing.T, db *sqlx.DB, planID string, price float64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code, is_active, accepting_new_subscriptions)
		VALUES ($1, $2, $3, 0, '{}', 'CNY', 'wallet-topup', true, true)
	`, planID, planID, price); err != nil {
		t.Fatalf("topup plan %s: %v", planID, err)
	}
}

func walletOutboxCount(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = 'wallet.sync'`); err != nil {
		t.Fatal(err)
	}
	return n
}

func stripePaidEvent(orderID, eventID, txnID string, amount float64) WebhookEvent {
	return WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: orderID, Amount: amount, Currency: "CNY",
		RawPayload: json.RawMessage(`{"id":"` + eventID + `"}`),
	}
}

// TestWalletTopup_PaidCreditsWalletOnce: 充值订单支付成功——不动订阅、不
// 发权益、按订单快照金额入队钱包充值；重复回调（webhook 重投）幂等；
// worker 消费后现金入账恰好一次。
func TestWalletTopup_PaidCreditsWalletOnce(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-30", 29.99)
	uid := seedUser(t, db)

	order, err := svc.CreateOrder(ctx, uid, "topup-30", "stripe")
	if err != nil {
		t.Fatalf("create topup order: %v", err)
	}
	if order.SnapshotProductCode() != "wallet-topup" {
		t.Fatalf("product snapshot = %q", order.SnapshotProductCode())
	}
	if order.BenefitPolicyVersionID != nil || order.BenefitGrantMode != nil {
		t.Fatalf("topup order must carry no benefit snapshot: %+v", order)
	}

	// 支付成功 webhook（两次不同 event id 模拟渠道重投同一支付）。
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-1", "pi_topup_1", 29.99)); err != nil {
		t.Fatalf("webhook paid: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-1b", "pi_topup_1", 29.99)); err != nil {
		t.Fatalf("webhook paid replay: %v", err)
	}

	// 不激活订阅、不入队权益同步。
	if s := activeSubForMaybe(t, db, uid, "wallet-topup"); s != nil {
		t.Fatalf("topup must not create a subscription: %+v", s)
	}
	if n := outboxCount(t, db); n != 0 {
		t.Fatalf("entitlement.sync outbox = %d, want 0", n)
	}
	if n := walletOutboxCount(t, db); n != 1 {
		t.Fatalf("wallet.sync outbox = %d, want 1 (dedup 吸收重复回调)", n)
	}

	// worker 消费：现金入账 29.99 CNY = 29_990_000 微。
	w := workers.NewWalletSync(store, nil, workers.EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 {
		t.Fatalf("worker stats = %+v", stats)
	}
	acct, err := store.GetBillingAccountByUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 29_990_000 || view.Balance.BonusAvailable != 0 {
		t.Fatalf("wallet = %+v, want cash 29_990_000 bonus 0", view.Balance)
	}

	// 再重投同一消息（补单路径无 dedup 键）→ 业务键兜底，不重复入账。
	payload, _ := json.Marshal(access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: uid, OrderID: order.ID,
		PaymentID: paymentIDOf(t, db, order.ID), AmountMicros: 29_990_000, Currency: "CNY",
	})
	uow, _ := store.Begin(ctx)
	if _, err := store.EnqueueOutbox(ctx, uow, access.TopicWalletSync, payload, nil); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err = w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 0 || stats.Replays != 1 {
		t.Fatalf("replay pass = %+v, want 0 credited / 1 replay", stats)
	}
	view, _ = store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if view.Balance.CashAvailable != 29_990_000 {
		t.Fatalf("cash = %d after replay, want unchanged", view.Balance.CashAvailable)
	}
}

// TestWalletTopup_RefundCashBack: 充值订单退款——现金原路退（全额+部分
// 都入队）；重复退款事件幂等；不触碰订阅/权益。
func TestWalletTopup_RefundCashBack(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-50", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-50", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-pay", "pi_r1", 50.00)); err != nil {
		t.Fatal(err)
	}
	payID := paymentIDOf(t, db, order.ID)

	// 部分退款 20（渠道确认）→ 钱包退款 20。
	partRefund := WebhookEvent{
		Channel: "stripe", EventID: "evt-ref-1", EventType: "charge.refunded",
		TransactionID: "pi_r1", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RefundAmount: 20.00, ExternalRefundID: "re_part",
		RawPayload: json.RawMessage(`{"id":"evt-ref-1"}`),
	}
	if _, err := svc.OnWebhook(ctx, partRefund); err != nil {
		t.Fatal(err)
	}
	// 重复退款事件（同 external_refund_id）→ 不重复入队。
	if _, err := svc.OnWebhook(ctx, partRefund); err != nil {
		t.Fatal(err)
	}
	// 全额退款（渠道金额=支付额）→ 再退 50（渠道口径；钱包按退款行逐笔原路退）。
	fullRefund := partRefund
	fullRefund.EventID = "evt-ref-2"
	fullRefund.RefundAmount = 50.00
	fullRefund.ExternalRefundID = "re_full"
	fullRefund.RawPayload = json.RawMessage(`{"id":"evt-ref-2"}`)
	if _, err := svc.OnWebhook(ctx, fullRefund); err != nil {
		t.Fatal(err)
	}
	if n := walletOutboxCount(t, db); n != 3 { // 1 topup + 2 refund
		t.Fatalf("wallet.sync outbox = %d, want 3", n)
	}

	w := workers.NewWalletSync(store, nil, workers.EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Refunded != 2 {
		t.Fatalf("worker stats = %+v", stats)
	}
	acct, _ := store.GetBillingAccountByUser(ctx, uid)
	view, err := store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// 50 − 20 − 50 = −20：退款总额超过充值（渠道层不变量另行保证）时现金
	// 如实转负（不隐藏负差额），赠送不参与。
	if view.Balance.CashAvailable != -20_000_000 {
		t.Fatalf("cash = %d, want -20 CNY (honest ledger)", view.Balance.CashAvailable)
	}
	_ = payID
}

// activeSubForMaybe is the nil-tolerant variant of activeSubFor.
func activeSubForMaybe(t *testing.T, db *sqlx.DB, userID, product string) *struct{} {
	t.Helper()
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1 AND product_code = $2 AND status = 'active'`, userID, product); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return nil
	}
	return &struct{}{}
}

func paymentIDOf(t *testing.T, db *sqlx.DB, orderID string) string {
	t.Helper()
	var id string
	if err := db.GetContext(context.Background(), &id,
		`SELECT id FROM payments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`, orderID); err != nil {
		t.Fatal(err)
	}
	return id
}
