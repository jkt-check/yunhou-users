package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/workers"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// payment_benefit_db_test.go — Task 10 验收硬项（服务层 + 真实库）：
// 下单快照冻结、按快照兑现（调价/撤售后）、重复/竞争幂等、退款吊销、
// 产品隔离（API 套餐支付不影响 Kaya 会员）、自助接口兜底。

// benefitStack wires the payment service with the benefit repo + outbox +
// the entitlement-sync worker, mirroring cmd/server production wiring.
type benefitStack struct {
	svc    *PaymentService
	store  *inferencepostgres.Store
	worker *workers.EntitlementSync
}

func newBenefitStack(t *testing.T, db *sqlx.DB) *benefitStack {
	t.Helper()
	svc := newTestPaymentService(t, db) // confirmVerifier passthrough installed
	store := inferencepostgres.NewStore(db)
	svc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	svc.SetBenefitSync(store)
	return &benefitStack{
		svc:    svc,
		store:  store,
		worker: workers.NewEntitlementSync(store, nil, workers.EntitlementSyncConfig{}),
	}
}

// seedCodingPlan inserts a sellable coding-plan plan + its benefit config.
// Returns the policy version id.
func seedCodingPlan(t *testing.T, db *sqlx.DB, st *inferencepostgres.Store, planID string, price float64, intervalDays int, policyName string, policyRev int, models []string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code, is_active, accepting_new_subscriptions)
		VALUES ($1, $2, $3, $4, '{}', 'CNY', 'coding-plan', true, true)
	`, planID, planID, price, intervalDays); err != nil {
		t.Fatalf("coding plan %s: %v", planID, err)
	}
	pol := &inferencepostgres.PolicyVersion{
		Name: policyName, Revision: policyRev, ModelIDs: models,
		FiveHourLimit: microcredits(1_000_000), WeeklyLimit: microcredits(10_000_000),
		MonthlyLimit: microcredits(100_000_000), Status: "published",
	}
	if err := st.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy version: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		VALUES ($1, $2, $3, 'subscription')
	`, planID, pol.ID, models); err != nil {
		t.Fatalf("benefit config %s: %v", planID, err)
	}
	return pol.ID
}

func microcredits(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

func codingWebhookEvent(orderID, eventID, txnID string, amount float64) WebhookEvent {
	return WebhookEvent{
		Channel: "wechat_pay", EventID: eventID, EventType: "TRANSACTION.SUCCESS",
		TransactionID: txnID, OrderID: orderID, Amount: amount, Currency: "CNY",
		RawPayload: json.RawMessage(`{"id":"` + eventID + `"}`),
	}
}

func entitlementForSub(t *testing.T, st *inferencepostgres.Store, subID string) *domain.Entitlement {
	t.Helper()
	ent, err := st.GetLatestEntitlementBySource(context.Background(), domain.SourceSubscription, subID)
	if err != nil {
		t.Fatalf("entitlement for sub %s: %v", subID, err)
	}
	return ent
}

func activeSubFor(t *testing.T, db *sqlx.DB, userID, product string) *model.Subscription {
	t.Helper()
	s, err := repo.NewSubscriptionRepo(db).FindActiveByUserAndProduct(context.Background(), userID, product)
	if err != nil {
		t.Fatalf("active sub %s/%s: %v", userID, product, err)
	}
	return s
}

func outboxCount(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = 'entitlement.sync'`); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- 下单快照冻结 + 没有支付配置不可购买 ---

func TestCreateOrder_CodingPlanSnapshotAndConfigGate(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)

	// 无配置 → 不可购买。
	if _, err := db.Exec(`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ('cp_naked', 'Naked', 9.9, 30, '{}', 'CNY', 'coding-plan')`); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.CreateOrder(context.Background(), uid, "cp_naked", "stripe"); !errors.Is(err, ErrPlanNotPurchasable) {
		t.Fatalf("no-config coding plan: err = %v, want ErrPlanNotPurchasable", err)
	}

	// 有配置 → 订单冻结完整快照。
	polID := seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})
	order, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.SnapshotProductCode() != "coding-plan" || order.SnapshotIntervalDays() != 30 {
		t.Fatalf("snapshot product/interval = %q/%d", order.SnapshotProductCode(), order.SnapshotIntervalDays())
	}
	if order.BenefitPolicyVersionID == nil || *order.BenefitPolicyVersionID != polID {
		t.Fatalf("snapshot policy = %v", order.BenefitPolicyVersionID)
	}
	if order.SnapshotGrantMode() != "subscription" || order.SnapshotOrderKind() != "new" {
		t.Fatalf("snapshot mode/kind = %q/%q", order.SnapshotGrantMode(), order.SnapshotOrderKind())
	}
	if len(order.BenefitModelIDs) != 1 || order.BenefitModelIDs[0] != "glm-4.6" {
		t.Fatalf("snapshot models = %v", order.BenefitModelIDs)
	}

	// Kaya 套餐保持原样：无快照权益、无 kind。
	kayaOrder, err := stack.svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("kaya CreateOrder: %v", err)
	}
	if kayaOrder.BenefitPolicyVersionID != nil || kayaOrder.SnapshotOrderKind() != "" {
		t.Fatalf("kaya order must not carry a benefit snapshot: %+v", kayaOrder)
	}
	if kayaOrder.SnapshotProductCode() != "kaya-membership" || kayaOrder.SnapshotIntervalDays() != 30 {
		t.Fatalf("kaya snapshot = %q/%d", kayaOrder.SnapshotProductCode(), kayaOrder.SnapshotIntervalDays())
	}
}

// --- Coding Plan 档位规则：续费/升级规则行；旧"周期更长"逻辑不适用 ---

func TestCreateOrder_CodingPlanUpgradeRules(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})
	// cp_pro 与 cp_basic 同为 30 天周期 —— 旧逻辑会认为"不构成升级"（周期相同），
	// 新规则只看显式规则行。
	seedCodingPlan(t, db, stack.store, "cp_pro", 99.9, 30, "cp-policy", 2, []string{"glm-4.6", "kimi-k2"})

	// 先买 basic 并支付激活。
	order1, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order1.ID, "evt-b1", "txn-b1", 29.9)); err != nil {
		t.Fatal(err)
	}

	// 同套餐续费：允许，kind=renewal。
	renew, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatalf("renewal order: %v", err)
	}
	if renew.SnapshotOrderKind() != "renewal" {
		t.Fatalf("kind = %q, want renewal", renew.SnapshotOrderKind())
	}

	// 跨档无规则 → 409（即使周期相同/更长都不推导）。
	if _, err := stack.svc.CreateOrder(context.Background(), uid, "cp_pro", "stripe"); !errors.Is(err, ErrPlanUpgradeNotConfigured) {
		t.Fatalf("cross-tier without rule: err = %v", err)
	}

	// 配置规则 → 允许，kind=upgrade + from 钉住 basic。
	if _, err := db.Exec(`INSERT INTO plan_upgrade_rules (from_plan_id, to_plan_id) VALUES ('cp_basic', 'cp_pro')`); err != nil {
		t.Fatal(err)
	}
	up, err := stack.svc.CreateOrder(context.Background(), uid, "cp_pro", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if up.SnapshotOrderKind() != "upgrade" || up.UpgradeFromPlanID == nil || *up.UpgradeFromPlanID != "cp_basic" {
		t.Fatalf("upgrade snapshot = %q/%v", up.SnapshotOrderKind(), up.UpgradeFromPlanID)
	}

	// Kaya 商品仍用旧周期比较规则（保留原会员购买规则）：yearly 活跃订阅
	// 买 monthly = 降级拒绝；再买 yearly = 允许（续费 rollover）。
	uid2 := seedUser(t, db)
	yOrder, err := stack.svc.CreateOrder(context.Background(), uid2, "yearly", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-y1", EventType: "payment_intent.succeeded",
		TransactionID: "txn-y1", OrderID: yOrder.ID, Amount: 199.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{"id":"evt-y1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.CreateOrder(context.Background(), uid2, "monthly", "stripe"); !errors.Is(err, ErrPlanDowngrade) {
		t.Fatalf("legacy downgrade rule broken: err = %v, want ErrPlanDowngrade", err)
	}
	if _, err := stack.svc.CreateOrder(context.Background(), uid2, "yearly", "stripe"); err != nil {
		t.Fatalf("legacy same-cycle renewal must stay allowed: %v", err)
	}
}

// --- 端到端（服务层构造 + mock 渠道回调）：下单 → 支付成功 → 权益 → 有额度 ---

func TestCodingPlan_OrderToEntitlementEndToEnd(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	polID := seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})

	// 用户有一个活跃的 Kaya 会员订阅 —— API 套餐支付不得触碰它。
	kayaSub := &model.Subscription{ID: mustNewUUID(), UserID: uid, PlanID: "monthly", ProductCode: model.ProductKayaMembership, Status: "active"}
	if err := repo.NewSubscriptionRepo(db).Create(context.Background(), kayaSub); err != nil {
		t.Fatal(err)
	}

	order, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// 支付回调（mock 渠道事件）。
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-e2e-1", "txn-e2e-1", 29.9)); err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}

	// 订阅激活：产品隔离 + 到期点 = 快照周期。
	sub := activeSubFor(t, db, uid, "coding-plan")
	if sub.PlanID != "cp_basic" || sub.ExpiresAt == nil {
		t.Fatalf("coding sub = %+v", sub)
	}
	kayaAfter := activeSubFor(t, db, uid, "kaya-membership")
	if kayaAfter.PlanID != "monthly" {
		t.Fatalf("API 套餐支付改动了 Kaya 订阅: %+v", kayaAfter)
	}

	// 同事务 outbox：恰好一条待投递消息。
	if n := outboxCount(t, db); n != 1 {
		t.Fatalf("outbox = %d, want 1", n)
	}

	// worker 消费 → 权益发放。
	stats, err := stack.worker.RunPass(context.Background())
	if err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	if stats.Granted != 1 || stats.Delivered != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	ent := entitlementForSub(t, stack.store, sub.ID)
	if ent.Status != domain.EntitlementActive || ent.PolicyVersionID != polID {
		t.Fatalf("entitlement = %+v", ent)
	}
	if ent.EffectiveTo == nil || !ent.EffectiveTo.Equal(*sub.ExpiresAt) {
		t.Fatalf("entitlement effective_to = %v, want sub expiry %v", ent.EffectiveTo, sub.ExpiresAt)
	}

	// Key 调用有额度：resolver 选中权益 + 配额准入放行。
	acct, err := stack.store.GetBillingAccountByUser(context.Background(), uid)
	if err != nil {
		t.Fatalf("billing account: %v", err)
	}
	resolver := access.NewEntitlementResolver(stack.store, nil)
	picked, err := resolver.Resolve(context.Background(), acct.ID, "glm-4.6", time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if picked.ID != ent.ID {
		t.Fatalf("resolver picked %s, want %s", picked.ID, ent.ID)
	}

	// 重复 webhook（同事件 ID + 同交易号不同事件 ID 各一次）→ 不产生第二份权益、
	// 到期点不移动。
	firstExpiry := *sub.ExpiresAt
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-e2e-1", "txn-e2e-1", 29.9)); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-e2e-1b", "txn-e2e-1", 29.9)); err != nil {
		t.Fatal(err)
	}
	stats, err = stack.worker.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Granted != 0 {
		t.Fatalf("duplicate webhook granted again: %+v", stats)
	}
	subAfter := activeSubFor(t, db, uid, "coding-plan")
	if !subAfter.ExpiresAt.Equal(firstExpiry) {
		t.Fatalf("duplicate webhook moved expiry: %v → %v", firstExpiry, *subAfter.ExpiresAt)
	}
	if n := entitlementCountFor(t, db, acct.ID); n != 1 {
		t.Fatalf("entitlements = %d, want 1", n)
	}
}

func entitlementCountFor(t *testing.T, db *sqlx.DB, accountID string) int {
	t.Helper()
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM inference_entitlements WHERE billing_account_id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- 调价/撤售后已支付订单按快照兑现 ---

func TestCodingPlan_SnapshotHonoredAfterPlanEdit(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	polID := seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})
	// 运营发布的新版本（更大配额 + 更多模型）——下单后才挂到配置上的。
	pol2 := &inferencepostgres.PolicyVersion{
		Name: "cp-policy", Revision: 2, ModelIDs: []string{"glm-4.6", "kimi-k2"},
		FiveHourLimit: microcredits(9_000_000), Status: "published",
	}
	if err := stack.store.InsertPolicyVersion(context.Background(), pol2); err != nil {
		t.Fatal(err)
	}

	order, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatal(err)
	}

	// 运营改写：调价 + 缩短周期 + 撤售 + 权益配置切到新版本。
	if _, err := db.Exec(`UPDATE plans SET price = 99.9, interval_days = 7, is_active = false WHERE id = 'cp_basic'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE plan_benefit_configs SET policy_version_id = $1, model_ids = '{glm-4.6,kimi-k2}' WHERE plan_id = 'cp_basic'`, pol2.ID); err != nil {
		t.Fatal(err)
	}

	// 支付回调：金额仍按订单快照校验（29.9），到期点按快照 30 天。
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-snap-1", "txn-snap-1", 29.9)); err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	sub := activeSubFor(t, db, uid, "coding-plan")
	if sub.ExpiresAt == nil {
		t.Fatal("no expiry")
	}
	want := time.Now().Add(30 * 24 * time.Hour)
	if d := sub.ExpiresAt.Sub(want); d > time.Minute || d < -time.Minute {
		t.Fatalf("expiry = %v, want ≈ %v (snapshot 30d, not the edited 7d)", *sub.ExpiresAt, want)
	}

	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	ent := entitlementForSub(t, stack.store, sub.ID)
	if ent.PolicyVersionID != polID {
		t.Fatalf("entitlement policy = %s, want the ORDER-TIME snapshot %s (not the rewritten config)", ent.PolicyVersionID, polID)
	}
	if len(ent.ModelIDs) != 1 || ent.ModelIDs[0] != "glm-4.6" {
		t.Fatalf("entitlement models = %v, want snapshot [glm-4.6]", ent.ModelIDs)
	}
}

// --- 重复退款/续费不产生重复效果 ---

func TestCodingPlan_FullRefundRevokes_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})

	order, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-r1", "txn-r1", 29.9)); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := activeSubFor(t, db, uid, "coding-plan")
	ent := entitlementForSub(t, stack.store, sub.ID)

	// 全额退款事件（渠道重投两次：同事件 ID 重放 + 不同事件 ID 同退款号）。
	refundEvt := func(eventID string) WebhookEvent {
		return WebhookEvent{
			Channel: "wechat_pay", EventID: eventID, EventType: "TRANSACTION.REFUND",
			TransactionID: "txn-r1", RefundAmount: 29.9, ExternalRefundID: "rf-r1",
			RawPayload: json.RawMessage(`{"id":"` + eventID + `"}`),
		}
	}
	if _, err := stack.svc.OnWebhook(context.Background(), refundEvt("evt-rf-1")); err != nil {
		t.Fatalf("refund webhook: %v", err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), refundEvt("evt-rf-1")); err != nil {
		t.Fatalf("refund replay: %v", err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), refundEvt("evt-rf-1b")); err != nil {
		t.Fatalf("refund redelivery: %v", err)
	}

	stats, err := stack.worker.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retired != 1 {
		t.Fatalf("retire stats = %+v (want exactly 1 retire)", stats)
	}
	after := entitlementForSub(t, stack.store, sub.ID)
	if after.Status != domain.EntitlementRevoked || after.ID != ent.ID {
		t.Fatalf("after refund = %+v", after)
	}
	// 权益行不删除；revision 只前进一次（revoke）——重放在 worker 侧 noop。
	if after.Revision != ent.Revision+1 {
		t.Fatalf("revision = %d, want %d", after.Revision, ent.Revision+1)
	}
	// 再跑一遍 worker（漏网重投）→ 全部 noop。
	stats, err = stack.worker.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retired != 0 || stats.Granted != 0 {
		t.Fatalf("second pass had effects: %+v", stats)
	}
	// 订阅已取消且未被复活。
	var subStatus string
	if err := db.GetContext(context.Background(), &subStatus, `SELECT status FROM subscriptions WHERE id = $1`, sub.ID); err != nil {
		t.Fatal(err)
	}
	if subStatus != "cancelled" {
		t.Fatalf("sub status = %s", subStatus)
	}
}

// --- 续费幂等：同一笔续费重复投递不重复延长 ---

func TestCodingPlan_RenewalIdempotentNoDoubleExtend(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})

	order1, _ := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order1.ID, "evt-n1", "txn-n1", 29.9)); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := activeSubFor(t, db, uid, "coding-plan")
	exp1 := *sub.ExpiresAt
	ent1 := entitlementForSub(t, stack.store, sub.ID)

	// 续费订单（rollover：从当前到期点 +30d）。
	order2, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if order2.SnapshotOrderKind() != "renewal" {
		t.Fatalf("kind = %q", order2.SnapshotOrderKind())
	}
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order2.ID, "evt-n2", "txn-n2", 29.9)); err != nil {
		t.Fatal(err)
	}
	sub2 := activeSubFor(t, db, uid, "coding-plan")
	if want := exp1.Add(30 * 24 * time.Hour); !sub2.ExpiresAt.Equal(want) {
		t.Fatalf("renewal expiry = %v, want %v", *sub2.ExpiresAt, want)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	ent2 := entitlementForSub(t, stack.store, sub.ID)
	if ent2.ID != ent1.ID || ent2.EffectiveTo == nil || !ent2.EffectiveTo.Equal(*sub2.ExpiresAt) {
		t.Fatalf("renewal converge = %+v", ent2)
	}

	// 同一笔续费重复投递（webhook 重投 + Confirm 竞争）→ 到期点不动、权益不再修订。
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order2.ID, "evt-n2-dup", "txn-n2", 29.9)); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order2.ID, UserID: uid, Channel: "wechat_pay", ExternalTxnID: "txn-n2",
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := stack.worker.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sub3 := activeSubFor(t, db, uid, "coding-plan")
	if !sub3.ExpiresAt.Equal(*sub2.ExpiresAt) {
		t.Fatalf("duplicate renewal moved expiry: %v → %v", *sub2.ExpiresAt, *sub3.ExpiresAt)
	}
	ent3 := entitlementForSub(t, stack.store, sub.ID)
	if ent3.Revision != ent2.Revision {
		t.Fatalf("duplicate renewal revised again: %d → %d (stats %+v)", ent2.Revision, ent3.Revision, stats)
	}
}

// --- Confirm 与 webhook 竞争同一笔支付 → 只发一份 ---

func TestCodingPlan_ConfirmWebhookRaceSingleGrant(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})

	order, err := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	// Confirm 与 webhook 各自驱动同一 (channel, txn)。
	if _, err := stack.svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "wechat_pay", ExternalTxnID: "txn-race",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-race", "txn-race", 29.9)); err != nil {
		t.Fatal(err)
	}
	// dedup 键钉在 payment 上：两路只入队一条。
	if n := outboxCount(t, db); n != 1 {
		t.Fatalf("outbox = %d, want 1 (dedup on payment)", n)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := activeSubFor(t, db, uid, "coding-plan")
	acct, err := stack.store.GetBillingAccountByUser(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if n := entitlementCountFor(t, db, acct.ID); n != 1 {
		t.Fatalf("entitlements = %d, want 1", n)
	}
	_ = sub
}

// --- 自助免费订阅接口兜底 + 取消联动吊销 ---

func TestSubscription_SelfServiceRejectsCodingPlan(t *testing.T) {
	db := setupPaymentDB(t)
	planSvc := NewPlanService(repo.NewPlanRepo(db), repo.NewAppRepo(db), repo.NewPlanChangeLogRepo(db))
	subSvc := NewSubscriptionService(repo.NewSubscriptionRepo(db), planSvc)
	uid := seedUser(t, db)

	if _, err := db.Exec(`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ('cp_free', 'CP Free Draft', 0, 30, '{}', 'CNY', 'coding-plan')`); err != nil {
		t.Fatal(err)
	}
	// 即使 price=0 的草稿套餐，自助接口也不能创建 coding-plan 订阅。
	if _, err := subSvc.Create(context.Background(), uid, "cp_free", nil); !errors.Is(err, ErrSelfServiceProductForbidden) {
		t.Fatalf("self-serve coding plan: err = %v", err)
	}
	// kaya 免费套餐自助订阅保留（本夹具中 free 为 active）：成功且落在
	// kaya-membership 产品下。
	freeSub, err := subSvc.Create(context.Background(), uid, "free", nil)
	if err != nil {
		t.Fatalf("kaya free self-subscribe (legacy path) broke: %v", err)
	}
	if freeSub.ProductCode != model.ProductKayaMembership {
		t.Fatalf("free sub product = %q", freeSub.ProductCode)
	}
}

func TestSubscription_CancelEnqueuesBenefitSync(t *testing.T) {
	db := setupPaymentDB(t)
	stack := newBenefitStack(t, db)
	uid := seedUser(t, db)
	seedCodingPlan(t, db, stack.store, "cp_basic", 29.9, 30, "cp-policy", 1, []string{"glm-4.6"})

	order, _ := stack.svc.CreateOrder(context.Background(), uid, "cp_basic", "stripe")
	if _, err := stack.svc.OnWebhook(context.Background(), codingWebhookEvent(order.ID, "evt-c1", "txn-c1", 29.9)); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sub := activeSubFor(t, db, uid, "coding-plan")
	ent := entitlementForSub(t, stack.store, sub.ID)
	if ent.Status != domain.EntitlementActive {
		t.Fatalf("precondition: entitlement %s", ent.Status)
	}

	// 生产接线的 Cancel：状态翻转 + outbox 同事务。
	subSvc := NewSubscriptionService(repo.NewSubscriptionRepo(db), NewPlanService(repo.NewPlanRepo(db), repo.NewAppRepo(db), repo.NewPlanChangeLogRepo(db)))
	subSvc.SetBenefitSync(db, stack.store)
	if err := subSvc.Cancel(context.Background(), sub.ID, uid); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := stack.worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := entitlementForSub(t, stack.store, sub.ID)
	if after.Status != domain.EntitlementRevoked {
		t.Fatalf("after cancel = %s, want revoked", after.Status)
	}
	// 重复取消 → ErrAlreadyCancelled，不产生第二条效果。
	if err := subSvc.Cancel(context.Background(), sub.ID, uid); !errors.Is(err, ErrAlreadyCancelled) {
		t.Fatalf("double cancel: %v", err)
	}
}

// --- 报价侧兜底：无支付配置的 coding-plan 商品不可报价 ---

func TestQuote_CodingPlanRequiresBenefitConfig(t *testing.T) {
	db := setupPaymentDB(t)
	uid := seedUser(t, db)
	if _, err := db.Exec(`INSERT INTO apps (app_id, name, is_active) VALUES ('kayacode', 'Kaya Code', true)`); err != nil {
		t.Fatal(err)
	}
	// 挂在 app 上的 coding-plan 套餐：无配置 → 报价拒绝。
	if _, err := db.Exec(`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ('cp_naked', 'Naked', 9.9, 30, '{kayacode}', 'CNY', 'coding-plan')`); err != nil {
		t.Fatal(err)
	}
	quoteSvc := NewQuoteService(repo.NewPlanRepo(db), repo.NewAppRepo(db))
	quoteSvc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	if _, err := quoteSvc.Get(context.Background(), "kayacode", "cp_naked", uid); !errors.Is(err, ErrPlanNotPurchasable) {
		t.Fatalf("quote without config: err = %v, want ErrPlanNotPurchasable", err)
	}
	// kaya 套餐报价不受新门槛影响（monthly 挂在 yundian 上，无权益配置 → 正常报价）。
	if _, err := quoteSvc.Get(context.Background(), "yundian", "monthly", uid); err != nil {
		t.Fatalf("kaya quote must stay unaffected: %v", err)
	}
}
