package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/workers"
	"github.com/yunhou/users/internal/repo"
)

// payment_review7_db_test.go — 评审批次7 支付退款级联验收（真实库）：
// Critical-1（Alipay 累计差额早退不得跳过全额退款级联）、Important-2
// （webhook 退款对账以商户退款单号为键 + 退款失败事件翻 failed）、
// Important-3（paid 后 payment_failed 回冲钱包充值）。

// apiRefundRowThenWebhook 走通「POST /refunds 记录行 → 对应 webhook 投递」
// 的公共 setup：下单元月订阅、渠道支付成功、API 全额退款记录 pending 行，
// 返回订单号 / 支付号 / 商户退款单号。
func apiRefundRowThenWebhook(t *testing.T, db *sqlx.DB, svc *PaymentService, uid, channel, payEventType, txnID string, amount float64) (orderID, paymentID, merchantNo string) {
	t.Helper()
	ctx := context.Background()
	order, err := svc.CreateOrder(ctx, uid, "monthly", channel)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: channel, EventID: "evt-r7-pay-" + mustNewUUID()[:8], EventType: payEventType,
		TransactionID: txnID, OrderID: order.ID, Amount: amount, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("paid webhook: %v", err)
	}
	paymentID = paymentIDOf(t, db, order.ID)
	stub := svc.refundAPI.(*stubRefundAPI)
	if _, err := svc.Refund(ctx, RefundInput{
		PaymentID: paymentID, UserID: uid,
		IdempotencyKey: "k-r7-" + mustNewUUID()[:8], Amount: amount,
	}); err != nil {
		t.Fatalf("api refund: %v", err)
	}
	if !stub.called || stub.gotMerchantNo == "" {
		t.Fatalf("channel refund API not called with merchant refund no: %+v", stub)
	}
	return order.ID, paymentID, stub.gotMerchantNo
}

// TestAlipay_APIRefundRowThenWebhook_FullCascadeRuns — Critical-1：API 记录
// 的 100% 退款行让 webhook（累计 refund_fee=全额）算出 delta=0。修复前直
// 接 tx.Commit() 早退：payment 保持 paid、订阅保持 active。修复后级联必
// 须执行（全部幂等），且不插入第二条退款行（Important-2：两侧键统一为
// 商户退款单号，webhook 解析层的 "alipay-" 前缀在服务层剥掉）。
func TestAlipay_APIRefundRowThenWebhook_FullCascadeRuns(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	ctx := context.Background()

	orderID, paymentID, merchantNo := apiRefundRowThenWebhook(t, db, svc, uid, "alipay", "TRADE_SUCCESS", "txn-r7-a1", 19.9)

	// 渠道退款 webhook：累计 refund_fee=全额 → delta=0（API 行已计入
	// prior）。webhook 解析层给键加 "alipay-" 前缀（handler/webhook.go）。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "alipay", EventID: "evt-r7-a1-rf-" + mustNewUUID()[:8], EventType: "trade_refund",
		TransactionID: "txn-r7-a1", OrderID: orderID, Amount: 19.9, Currency: "CNY",
		RefundAmount: 19.9, ExternalRefundID: "alipay-" + merchantNo,
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("refund webhook: %v", err)
	}

	var payStatus, orderStatus, subStatus string
	if err := db.GetContext(ctx, &payStatus, `SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "refunded" {
		t.Errorf("payment = %s, want refunded（delta=0 不得跳过全额级联）", payStatus)
	}
	if err := db.GetContext(ctx, &orderStatus, `SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if orderStatus != "refunded" {
		t.Errorf("order = %s, want refunded", orderStatus)
	}
	if err := db.GetContext(ctx, &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1 AND plan_id = 'monthly'`, uid); err != nil {
		t.Fatal(err)
	}
	if subStatus != "cancelled" {
		t.Errorf("sub = %s, want cancelled（全额退款吊销订阅）", subStatus)
	}
	// 对账命中 API 行：不插入第二条退款行，且 API 行翻 paid。
	var rows []string
	if err := db.SelectContext(ctx, &rows,
		`SELECT status FROM refunds WHERE payment_id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != "paid" {
		t.Errorf("refund rows = %v, want 单行 paid（webhook 对账命中 API 行）", rows)
	}
	// 键统一为裸商户退款单号（解析层前缀已剥）。
	var extID string
	if err := db.GetContext(ctx, &extID,
		`SELECT external_refund_id FROM refunds WHERE payment_id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if extID != merchantNo {
		t.Errorf("external_refund_id = %q, want 裸商户单号 %q", extID, merchantNo)
	}
}

// TestWeChat_APIRefundRowThenWebhook_Reconciled — Important-2：微信
// REFUND.SUCCESS 以 out_refund_no（商户退款单号）对账，ON CONFLICT 命中
// API 行翻 paid，不为同一笔钱插入第二条退款行；全额级联照常。
func TestWeChat_APIRefundRowThenWebhook_Reconciled(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	ctx := context.Background()

	orderID, paymentID, merchantNo := apiRefundRowThenWebhook(t, db, svc, uid, "wechat_pay", "TRANSACTION.SUCCESS", "wx-r7-w1", 19.9)

	res, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-r7-w1-rf-" + mustNewUUID()[:8], EventType: "REFUND.SUCCESS",
		TransactionID: "wx-r7-w1", OrderID: orderID, Amount: 19.9, Currency: "CNY",
		RefundAmount: 19.9, ExternalRefundID: merchantNo,
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("refund webhook: %v", err)
	}
	if res.DomainAction != "refund_paid" {
		t.Errorf("DomainAction = %q, want refund_paid", res.DomainAction)
	}
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT count(*) FROM refunds WHERE payment_id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("refund rows = %d, want 1（对账命中，不双插——账务漂移）", n)
	}
	var refundStatus string
	if err := db.GetContext(ctx, &refundStatus,
		`SELECT status FROM refunds WHERE payment_id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if refundStatus != "paid" {
		t.Errorf("refund = %s, want paid（API 行不再卡 pending，合计不变量解除）", refundStatus)
	}
	var payStatus string
	if err := db.GetContext(ctx, &payStatus, `SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "refunded" {
		t.Errorf("payment = %s, want refunded", payStatus)
	}
}

// TestAlipay_APIRefundRowThenWebhook_WalletRefundEnqueued — Critical-1 钱包
// 场景：API 全额退款行让 webhook 算出 delta=0，钱包退款仍必须入队（按行
// 金额），支付翻 refunded。
func TestAlipay_APIRefundRowThenWebhook_WalletRefundEnqueued(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-r7", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-r7", "alipay")
	if err != nil {
		t.Fatalf("create topup order: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "alipay", EventID: "evt-r7-wt-pay", EventType: "TRADE_SUCCESS",
		TransactionID: "txn-r7-wt", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("paid webhook: %v", err)
	}
	paymentID := paymentIDOf(t, db, order.ID)
	stub := svc.refundAPI.(*stubRefundAPI)
	if _, err := svc.Refund(ctx, RefundInput{
		PaymentID: paymentID, UserID: uid,
		IdempotencyKey: "k-r7-wt-" + mustNewUUID()[:8], Amount: 50.00,
	}); err != nil {
		t.Fatalf("api refund: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "alipay", EventID: "evt-r7-wt-rf", EventType: "trade_refund",
		TransactionID: "txn-r7-wt", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RefundAmount: 50.00, ExternalRefundID: "alipay-" + stub.gotMerchantNo,
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("refund webhook: %v", err)
	}

	var payStatus string
	if err := db.GetContext(ctx, &payStatus, `SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "refunded" {
		t.Errorf("payment = %s, want refunded", payStatus)
	}
	// 1 topup + 1 refund（delta=0 不吞钱包退款入队）。
	if n := walletOutboxCount(t, db); n != 2 {
		t.Fatalf("wallet.sync outbox = %d, want 2 (topup + refund)", n)
	}
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
	if view.Balance.CashAvailable != 0 {
		t.Fatalf("cash = %d, want 0（先充 50 后退 50）", view.Balance.CashAvailable)
	}
}

// TestPaymentFailed_AfterPaid_WalletTopupDebitsWallet — Important-3：钱包充
// 值支付 paid 之后来 payment_failed，wasPaid 级联除权益吊销外还必须入队
// 钱包冲正——否则已入账现金仍可花而支付行是 failed。
func TestPaymentFailed_AfterPaid_WalletTopupDebitsWallet(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-fail", 30.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-fail", "stripe")
	if err != nil {
		t.Fatalf("create topup order: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-r7-f-pay", "pi_r7_fail", 30.00)); err != nil {
		t.Fatalf("paid webhook: %v", err)
	}
	paymentID := paymentIDOf(t, db, order.ID)

	fail := func(eventID string) {
		t.Helper()
		if _, err := svc.OnWebhook(ctx, WebhookEvent{
			Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
			TransactionID: "pi_r7_fail", OrderID: order.ID, Amount: 30.00, Currency: "CNY",
			RawPayload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("payment_failed webhook: %v", err)
		}
	}
	fail("evt-r7-f-fail")

	var payStatus string
	if err := db.GetContext(ctx, &payStatus, `SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "failed" {
		t.Errorf("payment = %s, want failed", payStatus)
	}
	// 1 topup + 1 冲正（dedup 钉 payment id）。
	if n := walletOutboxCount(t, db); n != 2 {
		t.Fatalf("wallet.sync outbox = %d, want 2 (topup + 冲正)", n)
	}
	// 重投 payment_failed（不同事件 id）：终态守卫 no-op，不重复入队。
	fail("evt-r7-f-fail-2")
	if n := walletOutboxCount(t, db); n != 2 {
		t.Fatalf("wallet.sync outbox after replay = %d, want 2（幂等）", n)
	}

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
	if view.Balance.CashAvailable != 0 {
		t.Fatalf("cash = %d, want 0（paid 后失败全额冲正）", view.Balance.CashAvailable)
	}
}

// TestOnWebhook_RefundFailedEvent_FlipsPendingRow — Important-2：微信
// REFUND.ABNORMAL / REFUND.CLOSED 终态失败事件路由为翻 failed（此前落
// audit-only 默认分支，失败退款永远卡 pending、堵住合计不变量）。
func TestOnWebhook_RefundFailedEvent_FlipsPendingRow(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	ctx := context.Background()

	order, err := svc.CreateOrder(ctx, uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-r7-rf-pay", EventType: "TRANSACTION.SUCCESS",
		TransactionID: "wx-r7-rf", OrderID: order.ID, Amount: 19.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("paid webhook: %v", err)
	}
	paymentID := paymentIDOf(t, db, order.ID)
	stub := svc.refundAPI.(*stubRefundAPI)

	// 第一次全额退款 → REFUND.ABNORMAL（终态失败）。
	if _, err := svc.Refund(ctx, RefundInput{
		PaymentID: paymentID, UserID: uid, IdempotencyKey: "k-r7-rf-1", Amount: 19.9,
	}); err != nil {
		t.Fatalf("api refund: %v", err)
	}
	firstMerchantNo := stub.gotMerchantNo
	res, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-r7-rf-f1-" + mustNewUUID()[:8],
		EventType: "REFUND.ABNORMAL", TransactionID: "wx-r7-rf", OrderID: order.ID,
		ExternalRefundID: firstMerchantNo,
		RawPayload:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("REFUND.ABNORMAL webhook: %v", err)
	}
	if res.DomainAction != "refund_failed" {
		t.Errorf("DomainAction = %q, want refund_failed", res.DomainAction)
	}

	// 失败后用户重试同一逻辑全额退款：failed 行不计入合计不变量的预留
	// （若计入，19.9 + 19.9 > 19.9 必然被拒）。
	if _, err := svc.Refund(ctx, RefundInput{
		PaymentID: paymentID, UserID: uid, IdempotencyKey: "k-r7-rf-2", Amount: 19.9,
	}); err != nil {
		t.Fatalf("retry refund after failure: %v（failed 行不得计入合计不变量）", err)
	}
	secondMerchantNo := stub.gotMerchantNo
	res, err = svc.OnWebhook(ctx, WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-r7-rf-f2-" + mustNewUUID()[:8],
		EventType: "REFUND.CLOSED", TransactionID: "wx-r7-rf", OrderID: order.ID,
		ExternalRefundID: secondMerchantNo,
		RawPayload:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("REFUND.CLOSED webhook: %v", err)
	}
	if res.DomainAction != "refund_failed" {
		t.Errorf("DomainAction = %q, want refund_failed", res.DomainAction)
	}
	var statuses []string
	if err := db.SelectContext(ctx, &statuses,
		`SELECT status FROM refunds WHERE payment_id = $1 ORDER BY created_at`, paymentID); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != "failed" || statuses[1] != "failed" {
		t.Errorf("refund statuses = %v, want [failed failed]", statuses)
	}
	// 钱没退出去：支付保持 paid、订阅保持 active。
	var payStatus, subStatus string
	if err := db.GetContext(ctx, &payStatus, `SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "paid" {
		t.Errorf("payment = %s, want paid（退款失败不动支付）", payStatus)
	}
	if err := db.GetContext(ctx, &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1 AND plan_id = 'monthly'`, uid); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Errorf("sub = %s, want active（退款失败不动订阅）", subStatus)
	}
	// 审计留痕。
	var auditAction string
	if err := db.GetContext(ctx, &auditAction,
		`SELECT action FROM audit_log WHERE action = 'refund_marked_failed' LIMIT 1`); err != nil {
		t.Fatalf("refund_marked_failed audit missing: %v", err)
	}
}

// TestOnWebhook_RefundFailedEvent_UnknownPayment — 乱序：退款失败事件先于
// 支付成功事件到达时返错（渠道按其重投计划再投递），不 ack 丢事实。
func TestOnWebhook_RefundFailedEvent_UnknownPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "wechat_pay", EventID: "evt-r7-rf-unk-" + mustNewUUID()[:8], EventType: "REFUND.ABNORMAL",
		TransactionID: "wx-r7-never", ExternalRefundID: "mrn-unk",
		RawPayload: json.RawMessage(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown payment") {
		t.Fatalf("err = %v, want unknown-payment error（返错让渠道重投）", err)
	}
}

// TestPaymentFailed_AfterPaid_PartialRefund_DebitsOnlyDifference — 评审轮2
// N3：充值 50 → 部分退款 20（钱包已借记 20）→ payment_failed 乱序到达，
// 钱包只冲正差额 30；若仍按订单全额冲正，用户被追 70。
func TestPaymentFailed_AfterPaid_PartialRefund_DebitsOnlyDifference(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	ctx := context.Background()

	seedTopupPlan(t, db, "topup-partial-fail", 50.00)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(ctx, uid, "topup-partial-fail", "stripe")
	if err != nil {
		t.Fatalf("create topup order: %v", err)
	}
	if _, err := svc.OnWebhook(ctx, stripePaidEvent(order.ID, "evt-n3-pay", "pi_n3_partial", 50.00)); err != nil {
		t.Fatalf("paid webhook: %v", err)
	}
	paymentID := paymentIDOf(t, db, order.ID)

	// 部分退款 20：支付保持 paid，钱包已借记 20。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "stripe", EventID: "evt-n3-refund", EventType: "charge.refunded",
		TransactionID: "pi_n3_partial", OrderID: order.ID, Currency: "CNY",
		RefundAmount: 20.00, ExternalRefundID: "re_n3_partial",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("partial refund webhook: %v", err)
	}

	// payment_failed 乱序到达：只冲正差额 30（50 − 已 paid 退款 20）。
	if _, err := svc.OnWebhook(ctx, WebhookEvent{
		Channel: "stripe", EventID: "evt-n3-fail", EventType: "payment_intent.payment_failed",
		TransactionID: "pi_n3_partial", OrderID: order.ID, Amount: 50.00, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("payment_failed webhook: %v", err)
	}

	// outbox：topup + 部分退款 + 差额冲正 = 3 条。
	if n := walletOutboxCount(t, db); n != 3 {
		t.Fatalf("wallet.sync outbox = %d, want 3 (topup + 部分退款 + 差额冲正)", n)
	}
	// 冲正消息金额必须是差额 30（30_000_000 微），不是订单全额 50。
	var payload []byte
	if err := db.GetContext(ctx, &payload,
		`SELECT payload FROM inference_outbox WHERE topic = 'wallet.sync' AND dedup_key = $1`, "wallet:failed:"+paymentID); err != nil {
		t.Fatalf("failed-debit outbox row: %v", err)
	}
	var msg access.WalletSyncMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("unmarshal failed-debit payload: %v", err)
	}
	if msg.AmountMicros != 30_000_000 {
		t.Errorf("failed-debit amount = %d micros, want 30_000_000（差额 30，不得按全额 50 冲正）", msg.AmountMicros)
	}

	// worker 消费：50 入账 − 20 退款 − 30 差额冲正 = 0。
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
	if view.Balance.CashAvailable != 0 {
		t.Fatalf("cash = %d, want 0（50 − 20 退款 − 30 差额冲正）", view.Balance.CashAvailable)
	}
}
