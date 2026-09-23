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

// TestWalletTopup_RefundGuardPaidOnly: 钱包退款入队的 payment.status='paid'
// 守卫（Task 14 deferred minor / Task 15 纵深防御）——支付已 refunded 后，
// 另一笔不同 external_refund_id 的退款事件只落退款行，不再入队钱包退款
// （否则钱包现金会被二次扣减；dedup 键挡不住不同 refund_id）。
func TestWalletTopup_RefundGuardPaidOnly(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(inferencepostgres.NewStore(db))
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-60", 60.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-60", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-pay-g", "pi_g1", 60.00)); err != nil {
		t.Fatal(err)
	}

	// 全额退款（支付翻 refunded，入队 1 条钱包退款）。
	fullRefund := WebhookEvent{
		Channel: "stripe", EventID: "evt-g-ref1", EventType: "charge.refunded",
		TransactionID: "pi_g1", OrderID: order.ID, Amount: 60.00, Currency: "CNY",
		RefundAmount: 60.00, ExternalRefundID: "re_g_full",
		RawPayload: json.RawMessage(`{"id":"evt-g-ref1"}`),
	}
	if _, err := svc.OnWebhook(ctx, fullRefund); err != nil {
		t.Fatal(err)
	}
	if n := walletOutboxCount(t, db); n != 2 { // 1 topup + 1 refund
		t.Fatalf("wallet.sync outbox = %d, want 2", n)
	}

	// 第二笔不同 refund_id 的退款事件（支付已 refunded）→ 守卫拦截：
	// 退款行落库（支付域事实）但钱包退款不再入队。
	stray := fullRefund
	stray.EventID = "evt-g-ref2"
	stray.ExternalRefundID = "re_g_stray"
	stray.RawPayload = json.RawMessage(`{"id":"evt-g-ref2"}`)
	if _, err := svc.OnWebhook(ctx, stray); err != nil {
		t.Fatal(err)
	}
	if n := walletOutboxCount(t, db); n != 2 {
		t.Fatalf("wallet.sync outbox = %d after stray refund, want 2 (guard held)", n)
	}
	var refundRows int
	if err := db.GetContext(ctx, &refundRows,
		`SELECT COUNT(*) FROM refunds WHERE payment_id = (SELECT id FROM payments WHERE order_id = $1)`, order.ID); err != nil {
		t.Fatal(err)
	}
	if refundRows != 2 {
		t.Fatalf("refund rows = %d, want 2 (payment-domain fact recorded)", refundRows)
	}
	// 守卫动作留审计痕。
	var audits int
	if err := db.GetContext(ctx, &audits,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'wallet_refund_skipped_payment_not_paid'`); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("guard audit rows = %d, want 1", audits)
	}
}

// TestWalletTopup_RefundArrivesBeforePayment_OutOfOrder: 评审轮1 C2 端到端
// 乱序——退款事件先于支付成功事件到达：第一次投递查无支付行 → 返回错误
// （handler 映射 500 → 渠道重投）；支付成功随后入队钱包充值；渠道重投同一
// 退款事件 → 命中支付行入队钱包退款。worker 消费后钱包先充后退、净额正确。
func TestWalletTopup_RefundArrivesBeforePayment_OutOfOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-ooo", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-ooo", "stripe")
	if err != nil {
		t.Fatal(err)
	}

	// 1. 退款先到：查无支付行 → 错误（非 2xx 语义，渠道将重投）。
	refund := WebhookEvent{
		Channel: "stripe", EventID: "evt-ooo-ref", EventType: "charge.refunded",
		TransactionID: "pi_ooo", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RefundAmount: 50.00, ExternalRefundID: "re_ooo",
		RawPayload: json.RawMessage(`{"id":"evt-ooo-ref"}`),
	}
	if _, err := svc.OnWebhook(ctx, refund); err == nil {
		t.Fatal("refund before payment must return an error so the channel retries")
	}
	if n := walletOutboxCount(t, db); n != 0 {
		t.Fatalf("wallet.sync outbox = %d before payment, want 0", n)
	}

	// 2. 支付成功到达：照常入队钱包充值。
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-ooo-pay", "pi_ooo", 50.00)); err != nil {
		t.Fatalf("webhook paid: %v", err)
	}

	// 3. 渠道重投同一退款事件（同 event_id；processed_at=NULL → dedup 分支
	//    重跑业务动作）→ 这次命中支付行，入队钱包退款。
	if _, err := svc.OnWebhook(ctx, refund); err != nil {
		t.Fatalf("redelivered refund must succeed once the payment row exists: %v", err)
	}
	if n := walletOutboxCount(t, db); n != 2 { // 1 topup + 1 refund
		t.Fatalf("wallet.sync outbox = %d, want 2 (topup + refund)", n)
	}

	// 4. worker 消费：先充 50 后退 50，净额 0，且严格非负拆分/不双花。
	w := workers.NewWalletSync(store, nil, workers.EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Refunded != 1 {
		t.Fatalf("worker stats = %+v, want 1 credited / 1 refunded", stats)
	}
	acct, err := store.GetBillingAccountByUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 0 || view.Balance.BonusAvailable != 0 {
		t.Fatalf("wallet = %+v, want net 0 (先充后退)", view.Balance)
	}
}

// TestWalletTopup_TradeClosedWithRefundAmount_OutOfOrder: 评审轮3 D-1 乱
// 序闭环——Alipay 交易已支付但 TRADE_SUCCESS 还在重投窗口，商户后台退款
// 的 trade_closed（携带退款额）先到：携带退款额 → 不落入未支付关单
// ack，返错让渠道重投；TRADE_SUCCESS 随后到达充值；退款重投成功；钱包
// 先充后退净额 0（双花窗口闭合）。
func TestWalletTopup_TradeClosedWithRefundAmount_OutOfOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-tc", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-tc", "alipay")
	if err != nil {
		t.Fatal(err)
	}

	// 1. 带退款额的 trade_closed 先到（订单 pending、无支付行）→ 返错。
	closeEvent := WebhookEvent{
		Channel: "alipay", EventID: "evt-tc-ref", EventType: "trade_closed",
		TransactionID: "txn_tc_1", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RefundAmount: 50.00, ExternalRefundID: "rf_tc_1",
		RawPayload: json.RawMessage(`{"id":"evt-tc-ref"}`),
	}
	if _, err := svc.OnWebhook(ctx, closeEvent); err == nil {
		t.Fatal("trade_closed carrying a refund amount must return an error so the channel retries (D-1)")
	}
	if n := walletOutboxCount(t, db); n != 0 {
		t.Fatalf("wallet.sync outbox = %d before payment, want 0", n)
	}

	// 2. TRADE_SUCCESS 重投到达：照常入队钱包充值。
	paid := WebhookEvent{
		Channel: "alipay", EventID: "evt-tc-pay", EventType: "TRADE_SUCCESS",
		TransactionID: "txn_tc_1", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RawPayload: json.RawMessage(`{"id":"evt-tc-pay"}`),
	}
	if _, err := svc.OnWebhook(ctx, paid); err != nil {
		t.Fatalf("webhook paid: %v", err)
	}

	// 3. 渠道重投同一 trade_closed（processed_at=NULL → dedup 分支重跑）
	//    → 命中支付行，入队钱包退款。
	if _, err := svc.OnWebhook(ctx, closeEvent); err != nil {
		t.Fatalf("redelivered trade_closed must succeed once the payment row exists: %v", err)
	}
	if n := walletOutboxCount(t, db); n != 2 { // 1 topup + 1 refund
		t.Fatalf("wallet.sync outbox = %d, want 2 (topup + refund)", n)
	}

	// 4. worker 消费：先充 50 后退 50，净额 0。
	w := workers.NewWalletSync(store, nil, workers.EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Refunded != 1 {
		t.Fatalf("worker stats = %+v, want 1 credited / 1 refunded", stats)
	}
	acct, err := store.GetBillingAccountByUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 0 || view.Balance.BonusAvailable != 0 {
		t.Fatalf("wallet = %+v, want net 0 (先充后退)", view.Balance)
	}
}

// TestWalletTopup_AlipayRefund_CumulativeDelta: 评审轮4 B 钱包侧——Alipay
// 累计 refund_fee 的部分退款序列只按增量扣钱包现金；同一累计值重复通知
// 不双退。
func TestWalletTopup_AlipayRefund_CumulativeDelta(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-cum", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-cum", "alipay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "alipay", EventID: "evt-wcum-pay", EventType: "TRADE_SUCCESS",
		TransactionID: "txn_wcum", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RawPayload: json.RawMessage(`{"id":"evt-wcum-pay"}`),
	}); err != nil {
		t.Fatal(err)
	}
	refund := func(eventID, extID string, cumulative float64) {
		_, err := svc.OnWebhook(ctx, WebhookEvent{
			Channel: "alipay", EventID: eventID, EventType: "trade_refund",
			TransactionID: "txn_wcum", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
			RefundAmount: cumulative, ExternalRefundID: extID,
			RawPayload: json.RawMessage(`{"id":"` + eventID + `"}`),
		})
		if err != nil {
			t.Fatalf("refund %s: %v", eventID, err)
		}
	}
	// refund_fee 20 → 钱包退 20；refund_fee 35（累计）→ 只退增量 15；
	// 同累计 35 重复通知 → 不再入队。
	refund("evt-wcum-r1", "alipay-wbiz-1", 20.00)
	refund("evt-wcum-r2", "alipay-wbiz-2", 35.00)
	refund("evt-wcum-r3", "alipay-wbiz-2b", 35.00)
	if n := walletOutboxCount(t, db); n != 3 { // 1 topup + 2 refund（第三笔增量 0 不入队）
		t.Fatalf("wallet.sync outbox = %d, want 3 (topup + 20 + 15)", n)
	}

	w := workers.NewWalletSync(store, nil, workers.EntitlementSyncConfig{})
	stats, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credited != 1 || stats.Refunded != 2 {
		t.Fatalf("worker stats = %+v, want 1 credited / 2 refunded", stats)
	}
	acct, err := store.GetBillingAccountByUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.WalletBalance(ctx, acct.ID, "CNY", order.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// 50 − 20 − 15 = 15（逐笔增量，累计 35）。
	if view.Balance.CashAvailable != 15_000_000 {
		t.Fatalf("cash = %d, want 15 CNY (50−20−15 增量语义)", view.Balance.CashAvailable)
	}
}

// TestWalletTopup_RefundPaymentNeverArrives: 支付成功事件永不到达时，退款
// 事件每次投递都持续返回错误（本测试只断言错误语义；最终一致性依赖渠道的
// 重投窗口——窗口内支付到达则乱序自愈，窗口外审计行
// webhook_refund_unknown_payment 给运营人工介入信号）。
func TestWalletTopup_RefundPaymentNeverArrives(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	for i, eventID := range []string{"evt-never-1", "evt-never-2"} {
		_, err := svc.OnWebhook(context.Background(), WebhookEvent{
			Channel: "stripe", EventID: eventID, EventType: "charge.refunded",
			TransactionID: "pi_never_arrives", RefundAmount: 10.00, ExternalRefundID: "re_never",
			RawPayload: json.RawMessage(`{}`),
		})
		if err == nil {
			t.Fatalf("delivery %d: refund for a payment that never arrives must keep returning an error", i+1)
		}
	}
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'webhook_refund_unknown_payment'`); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("audit rows = %d, want 2 (每次投递都留审计痕)", n)
	}
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
