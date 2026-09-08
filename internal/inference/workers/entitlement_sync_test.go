package workers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/model"
)

// entitlement_sync_test.go — Task 10 验收：支付→权益闭环的消费侧。
// 真实库（专用可丢弃实例）：发放、续费延期、升级改规格、退款吊销、
// 重购复活、乱序/重复消息收敛、无支付证据兜底、捆绑赠送抑制、迁移赠送
// 独立幂等键、dedup 键单入队、双 worker 竞争。

// wipeSyncTables clears BOTH the inference tables and the legacy payment
// tables these tests drive (plans/subscriptions/orders). Tests run under
// -p 1, so the shared wipe is race-free.
func wipeSyncTables(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.Exec(`TRUNCATE
		inference_reconciliation_jobs, inference_outbox,
		inference_ledger_entries, inference_adjustments,
		inference_concurrency_leases, inference_reservations,
		inference_quota_windows, inference_usage_records,
		inference_attempts, inference_requests,
		inference_entitlements, inference_policy_versions,
		inference_price_versions,
		inference_api_keys, inference_billing_accounts,
		inference_upstream_accounts, inference_credentials,
		inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models,
		plan_upgrade_rules, plan_benefit_configs,
		orders, payments, refunds, webhook_events, subscriptions, plans, users
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
}

type syncFixture struct {
	db        *sqlx.DB
	store     *postgres.Store
	worker    *EntitlementSync
	userID    string
	policyID  string
	policy2ID string
	modelID   string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	wipeSyncTables(t, db)

	ctx := context.Background()
	s := postgres.NewStore(db)
	f := &syncFixture{db: db, store: s, modelID: "glm-4.6"}
	f.worker = NewEntitlementSync(s, nil, EntitlementSyncConfig{})

	f.userID = uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, f.userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := s.InsertModel(ctx, &domain.Model{
		ID: f.modelID, DisplayName: "GLM", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := s.InsertModel(ctx, &domain.Model{
		ID: "kimi-k2", DisplayName: "K2", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("model2: %v", err)
	}

	pol := &postgres.PolicyVersion{
		Name: "cp-policy", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: microP(1_000_000), WeeklyLimit: microP(10_000_000),
		MonthlyLimit: microP(100_000_000),
	}
	if err := s.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	f.policyID = pol.ID

	pol2 := &postgres.PolicyVersion{
		Name: "cp-policy", Revision: 2, ModelIDs: []string{f.modelID, "kimi-k2"},
		FiveHourLimit: microP(2_000_000), WeeklyLimit: microP(20_000_000),
		MonthlyLimit: microP(200_000_000),
	}
	if err := s.InsertPolicyVersion(ctx, pol2); err != nil {
		t.Fatalf("policy2: %v", err)
	}
	f.policy2ID = pol2.ID
	return f
}

func (f *syncFixture) seedPlan(t *testing.T, planID, product string, intervalDays int, price float64) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ($1, $2, $3, $4, '{}', 'CNY', $5)
	`, planID, planID, price, intervalDays, product); err != nil {
		t.Fatalf("plan %s: %v", planID, err)
	}
}

func (f *syncFixture) seedBenefitConfig(t *testing.T, planID, policyID string, models []string, grantMode string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		VALUES ($1, $2, $3, $4)
	`, planID, policyID, models, grantMode); err != nil {
		t.Fatalf("benefit config %s: %v", planID, err)
	}
}

// seedPaidOrderFull inserts an order row as the payment pipeline leaves it
// AFTER a successful charge: status='paid' with the frozen 029 snapshot.
// policyID nil simulates a plain (non-benefit) paid order; empty
// grantMode/kind bind SQL NULL (the CHECKs only accept enum literals).
func (f *syncFixture) seedPaidOrderFull(t *testing.T, planID string, policyID *string, models []string, grantMode, kind string) string {
	t.Helper()
	orderID := uuid.NewString()
	var grantModeP, kindP *string
	if grantMode != "" {
		grantModeP = &grantMode
	}
	if kind != "" {
		kindP = &kind
	}
	_, err := f.db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status,
			product_code, plan_interval_days, benefit_policy_version_id,
			benefit_model_ids, benefit_grant_mode, order_kind)
		SELECT $1, $2, p.id, p.price, p.currency, 'paid',
			p.product_code, p.interval_days, $3, $4, $5, $6
		FROM plans p WHERE p.id = $7
	`, orderID, f.userID, policyID, models, grantModeP, kindP, planID)
	if err != nil {
		t.Fatalf("paid order: %v", err)
	}
	return orderID
}

func (f *syncFixture) seedSubscription(t *testing.T, planID, product, status string, expiresAt *time.Time) string {
	t.Helper()
	subID := uuid.NewString()
	_, err := f.db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, $2, $3, $4, now(), $5, $6)
	`, subID, f.userID, planID, status, expiresAt, product)
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}
	return subID
}

func (f *syncFixture) enqueue(t *testing.T, msg access.EntitlementSyncMessage, dedupKey *string) {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := f.db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := f.store.EnqueueOutboxSQLTx(ctx, tx, access.TopicEntitlementSync, payload, dedupKey); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (f *syncFixture) onlyEntitlement(t *testing.T, sourceType domain.EntitlementSource, sourceID string) *domain.Entitlement {
	t.Helper()
	ent, err := f.store.GetLatestEntitlementBySource(context.Background(), sourceType, sourceID)
	if err != nil {
		t.Fatalf("get entitlement %s/%s: %v", sourceType, sourceID, err)
	}
	return ent
}

func (f *syncFixture) countEntitlements(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.GetContext(context.Background(), &n, `SELECT COUNT(*) FROM inference_entitlements`); err != nil {
		t.Fatal(err)
	}
	return n
}

func futureExpiry(days int) *time.Time {
	t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Microsecond)
	return &t
}

// --- 验收：支付成功 → 发放（含计费账户自动建立） ---

func TestEntitlementSync_GrantFromPaidOrder(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	orderID := f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	dedup := access.PaidSyncDedupKey("pay-1")
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonPaymentPaid, OrderID: orderID, PaymentID: "pay-1",
	}, &dedup)

	stats, err := f.worker.RunPass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.Delivered != 1 || stats.Granted != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	ent := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if ent.Status != domain.EntitlementActive {
		t.Fatalf("status = %s", ent.Status)
	}
	if ent.PolicyVersionID != f.policyID || len(ent.ModelIDs) != 1 || ent.ModelIDs[0] != "glm-4.6" {
		t.Fatalf("spec = %v / %v", ent.PolicyVersionID, ent.ModelIDs)
	}
	if ent.EffectiveTo == nil || !ent.EffectiveTo.Equal(*exp) {
		t.Fatalf("effective_to = %v, want %v", ent.EffectiveTo, exp)
	}
	if ent.Stackable {
		t.Fatal("explicit entitlement must be non-stackable in phase 1")
	}
	// 计费账户随首次发放自动建立（每用户一个）。
	if _, err := f.store.GetBillingAccountByUser(context.Background(), f.userID); err != nil {
		t.Fatalf("billing account not ensured: %v", err)
	}

	// 重放：同一消息再跑一遍 pass → 全部 noop，权益行数不变。
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonPaymentPaid, OrderID: orderID, PaymentID: "pay-1",
	}, nil) // 无 dedup 键的重复消息也要收敛为 noop
	stats, err = f.worker.RunPass(context.Background())
	if err != nil {
		t.Fatalf("replay pass: %v", err)
	}
	if stats.Granted != 0 || stats.Noops == 0 {
		t.Fatalf("replay stats = %+v", stats)
	}
	if n := f.countEntitlements(t); n != 1 {
		t.Fatalf("entitlements = %d, want 1", n)
	}
}

// --- 验收：续费延长有效期，同一消费主体不重置 ---

func TestEntitlementSync_RenewalExtendsSameEntitlement(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	before := f.onlyEntitlement(t, domain.SourceSubscription, subID)

	// 续费：订阅到期点 +30d（支付管线 rollover 后的状态），第二笔已支付订单。
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindRenewal)
	newExp := exp.Add(30 * 24 * time.Hour)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET expires_at = $1 WHERE id = $2`, newExp, subID); err != nil {
		t.Fatal(err)
	}
	stats, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, "")
	_ = stats
	if err != nil {
		t.Fatalf("renewal sync: %v", err)
	}
	after := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if after.ID != before.ID {
		t.Fatalf("renewal must keep the same consumption subject: %s → %s", before.ID, after.ID)
	}
	if !after.AnchorAt.Equal(before.AnchorAt) {
		t.Fatalf("anchor moved: %v → %v", before.AnchorAt, after.AnchorAt)
	}
	if after.EffectiveTo == nil || !after.EffectiveTo.Equal(newExp) {
		t.Fatalf("effective_to = %v, want %v", after.EffectiveTo, newExp)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision = %d, want %d", after.Revision, before.Revision+1)
	}

	// 重复续费消息（幂等）：第三次同步 → noop。
	n, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("third sync changed %d rows, want noop", n)
	}
	final := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if final.Revision != after.Revision || !final.EffectiveTo.Equal(*after.EffectiveTo) {
		t.Fatalf("repeat renewal had an effect: %+v", final)
	}
}

// --- 验收：升级沿用同一消费主体，规格就地更新 ---

func TestEntitlementSync_UpgradeRevisesInPlace(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedPlan(t, "cp_pro", model.ProductCodingPlan, 30, 99.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	f.seedBenefitConfig(t, "cp_pro", f.policy2ID, []string{"glm-4.6", "kimi-k2"}, model.BenefitGrantModeSubscription)
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	before := f.onlyEntitlement(t, domain.SourceSubscription, subID)

	// 升级：订阅切到 cp_pro（激活已写新套餐），新的已支付订单带新快照。
	f.seedPaidOrderFull(t, "cp_pro", &f.policy2ID, []string{"glm-4.6", "kimi-k2"}, model.BenefitGrantModeSubscription, model.OrderKindUpgrade)
	newExp := futureExpiry(30)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET plan_id = 'cp_pro', expires_at = $1 WHERE id = $2`, newExp, subID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("upgrade sync: %v", err)
	}
	after := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if after.ID != before.ID || !after.AnchorAt.Equal(before.AnchorAt) {
		t.Fatalf("upgrade must keep ID+anchor: %+v", after)
	}
	if after.PolicyVersionID != f.policy2ID || len(after.ModelIDs) != 2 {
		t.Fatalf("spec after upgrade = %v / %v", after.PolicyVersionID, after.ModelIDs)
	}
	if after.EffectiveTo == nil || !after.EffectiveTo.Equal(*newExp) {
		t.Fatalf("effective_to = %v, want %v", after.EffectiveTo, newExp)
	}
}

// --- 验收：退款吊销 + 重购复活；已消费账本不动 ---

func TestEntitlementSync_RefundRevokesAndRepurchaseRevives(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	granted := f.onlyEntitlement(t, domain.SourceSubscription, subID)

	// 全额退款：支付域把订阅翻 cancelled、订单翻 refunded。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'refunded' WHERE user_id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonRefundFull, SubscriptionID: subID,
	}, nil)
	stats, err := f.worker.RunPass(context.Background())
	if err != nil {
		t.Fatalf("refund pass: %v", err)
	}
	if stats.Retired != 1 {
		t.Fatalf("refund stats = %+v", stats)
	}
	revoked := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if revoked.Status != domain.EntitlementRevoked {
		t.Fatalf("status = %s, want revoked", revoked.Status)
	}
	// 账本/窗口一行不动：本测试未产生账本，但权益行本身保留（不删除）。
	if f.countEntitlements(t) != 1 {
		t.Fatal("refund must not delete the entitlement row")
	}

	// 重复退款消息：revoked 已是终态 → noop。
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonRefundFull, SubscriptionID: subID,
	}, nil)
	stats, err = f.worker.RunPass(context.Background())
	if err != nil {
		t.Fatalf("refund replay pass: %v", err)
	}
	if stats.Retired != 0 {
		t.Fatalf("duplicate refund had an effect: %+v", stats)
	}

	// 重新购买：订阅行原地复活（支付管线 UPSERT 语义），新订单已支付。
	newExp := futureExpiry(30)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET status = 'active', expires_at = $1 WHERE id = $2`, newExp, subID); err != nil {
		t.Fatal(err)
	}
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("repurchase sync: %v", err)
	}
	revived := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if revived.Status != domain.EntitlementActive || revived.ID != granted.ID {
		t.Fatalf("revive = id %s status %s", revived.ID, revived.Status)
	}
	if !revived.AnchorAt.Equal(granted.AnchorAt) {
		t.Fatal("revive must keep the original anchor")
	}
}

// --- 验收：乱序消息收敛到当前状态 ---

func TestEntitlementSync_OutOfOrderMessagesConverge(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	orderID := f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	// 先发放。
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatal(err)
	}

	// 乱序：一条"旧的 paid"消息与一条"新的 refund"消息同批到达；
	// 订阅已是 cancelled。paid 先消费（id 小）也必须读到当前状态。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonPaymentPaid, OrderID: orderID, PaymentID: "pay-old",
	}, nil)
	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonRefundFull, SubscriptionID: subID,
	}, nil)
	if _, err := f.worker.RunPass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	final := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if final.Status != domain.EntitlementRevoked {
		t.Fatalf("out-of-order final status = %s, want revoked", final.Status)
	}
}

// --- 验收：无支付证据兜底（自助免费订阅不能变出额度） ---

func TestEntitlementSync_NoPaidOrderEvidence(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_free_draft", model.ProductCodingPlan, 30, 0)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_free_draft", model.ProductCodingPlan, "active", exp)

	// 自助创建的订阅：无任何已支付订单 → 不发放。
	n, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || f.countEntitlements(t) != 0 {
		t.Fatalf("backstop granted without payment evidence (n=%d)", n)
	}

	// 防御：即使某处错误地发过了，同步也会吊销无证据的权益。
	acct, err := f.store.EnsureBillingAccount(context.Background(), f.userID)
	if err != nil {
		t.Fatal(err)
	}
	f.seedBenefitConfig(t, "cp_free_draft", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	bogus, err := access.GrantFromPlan(access.PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: subID,
		ModelIDs: []string{"glm-4.6"}, PolicyVersionID: f.policyID,
	}, time.Now().UTC(), time.Now().UTC(), exp)
	if err != nil {
		t.Fatal(err)
	}
	bogus.BillingAccountID = acct.ID
	if err := f.store.InsertEntitlement(context.Background(), bogus); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatal(err)
	}
	after := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if after.Status != domain.EntitlementRevoked {
		t.Fatalf("evidence-less entitlement status = %s, want revoked", after.Status)
	}
}

// --- 验收：Bundle 显式发 grant、默认不叠加 ---

func TestEntitlementSync_BundleGiftNonStacking(t *testing.T) {
	f := newSyncFixture(t)
	// kaya 会员套餐挂捆绑赠送配置（gift 模式）。
	f.seedPlan(t, "monthly", model.ProductKayaMembership, 30, 19.9)
	f.seedBenefitConfig(t, "monthly", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeGift)
	f.seedPaidOrderFull(t, "monthly", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeGift, "")
	exp := futureExpiry(30)
	kayaSubID := f.seedSubscription(t, "monthly", model.ProductKayaMembership, "active", exp)

	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductKayaMembership, ""); err != nil {
		t.Fatalf("bundle sync: %v", err)
	}
	gift := f.onlyEntitlement(t, domain.SourceGrant, access.BundleGiftSourceID(kayaSubID))
	if gift.Status != domain.EntitlementActive || gift.Stackable {
		t.Fatalf("gift = %+v", gift)
	}
	if gift.EffectiveTo == nil || !gift.EffectiveTo.Equal(*exp) {
		t.Fatalf("gift effective_to = %v, want %v", gift.EffectiveTo, exp)
	}

	// 默认不叠加：账户存在显式（subscription）权益时，赠送被整体抑制。
	acct, err := f.store.EnsureBillingAccount(context.Background(), f.userID)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := access.GrantFromPlan(access.PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: "sub-explicit-" + uuid.NewString(),
		ModelIDs: []string{"glm-4.6"}, PolicyVersionID: f.policy2ID,
	}, time.Now().UTC(), time.Now().UTC(), exp)
	if err != nil {
		t.Fatal(err)
	}
	explicit.BillingAccountID = acct.ID
	if err := f.store.InsertEntitlement(context.Background(), explicit); err != nil {
		t.Fatal(err)
	}
	picked, err := access.SelectEntitlement(
		[]domain.Entitlement{*gift, *explicit}, "glm-4.6", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != explicit.ID {
		t.Fatal("explicit entitlement must win over the bundle gift (赠送默认不叠加)")
	}

	// kaya 续费：同一赠送权益延期，不发第二份。
	newExp := exp.Add(30 * 24 * time.Hour)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET expires_at = $1 WHERE id = $2`, newExp, kayaSubID); err != nil {
		t.Fatal(err)
	}
	f.seedPaidOrderFull(t, "monthly", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeGift, "")
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductKayaMembership, ""); err != nil {
		t.Fatal(err)
	}
	giftAfter := f.onlyEntitlement(t, domain.SourceGrant, access.BundleGiftSourceID(kayaSubID))
	if giftAfter.ID != gift.ID || !giftAfter.EffectiveTo.Equal(newExp) {
		t.Fatalf("bundle renewal must extend the same gift: %+v", giftAfter)
	}
	if f.countEntitlements(t) != 2 {
		t.Fatalf("entitlements = %d, want 2 (gift + explicit)", f.countEntitlements(t))
	}

	// kaya 取消 → 赠送吊销；显式权益不受影响。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, kayaSubID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductKayaMembership, ""); err != nil {
		t.Fatal(err)
	}
	giftRevoked := f.onlyEntitlement(t, domain.SourceGrant, access.BundleGiftSourceID(kayaSubID))
	if giftRevoked.Status != domain.EntitlementRevoked {
		t.Fatalf("gift status = %s", giftRevoked.Status)
	}
	explicitAfter := f.onlyEntitlement(t, domain.SourceSubscription, explicit.SourceID)
	if explicitAfter.Status != domain.EntitlementActive {
		t.Fatal("kaya cancel must not touch the explicit coding-plan entitlement")
	}
}

// --- 验收：迁移赠送独立幂等来源键 ---

func TestEntitlementSync_MigrationGiftIndependentKey(t *testing.T) {
	f := newSyncFixture(t)
	exp := futureExpiry(90)

	granted, err := f.worker.IssueMigrationGift(context.Background(), f.userID, "kaya-legacy-2026-09",
		[]string{"glm-4.6"}, f.policyID, exp)
	if err != nil {
		t.Fatal(err)
	}
	if !granted {
		t.Fatal("first issuance must grant")
	}
	// 重放：同一 (rule, user) 不再发。
	again, err := f.worker.IssueMigrationGift(context.Background(), f.userID, "kaya-legacy-2026-09",
		[]string{"glm-4.6"}, f.policyID, exp)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("re-issuance must be a no-op (独立幂等来源键)")
	}
	// 不同规则 = 另一来源键，可独立发放。
	other, err := f.worker.IssueMigrationGift(context.Background(), f.userID, "kaya-legacy-2026-10",
		[]string{"glm-4.6"}, f.policyID, exp)
	if err != nil || !other {
		t.Fatal("a different rule must grant independently")
	}
	if n := f.countEntitlements(t); n != 2 {
		t.Fatalf("entitlements = %d, want 2", n)
	}
}

// --- 验收：dedup 键单入队 + 双 worker 竞争只发一次 ---

func TestEntitlementSync_DedupKeySingleEnqueue(t *testing.T) {
	f := newSyncFixture(t)
	dedup := access.PaidSyncDedupKey("pay-dedup")
	msg := access.EntitlementSyncMessage{UserID: f.userID, ProductCode: model.ProductCodingPlan, Reason: access.SyncReasonPaymentPaid, PaymentID: "pay-dedup"}
	f.enqueue(t, msg, &dedup)
	f.enqueue(t, msg, &dedup) // 撞键 → 不入队

	var n int
	if err := f.db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = $1`, access.TopicEntitlementSync); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

func TestEntitlementSync_ConcurrentWorkersGrantOnce(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	f.enqueue(t, access.EntitlementSyncMessage{
		UserID: f.userID, ProductCode: model.ProductCodingPlan,
		Reason: access.SyncReasonPaymentPaid, PaymentID: "pay-race",
	}, nil)

	// 两个 worker 实例（模拟双实例部署）同时消费同一批消息。
	// EnsureBillingAccount 的 ON CONFLICT 读回 + 权益来源唯一键 +
	// 乐观 revision 守卫保证只发一份；racing 的一方回滚后重试 noop。
	for _, w := range []*EntitlementSync{
		NewEntitlementSync(postgres.NewStore(f.db), nil, EntitlementSyncConfig{}),
		NewEntitlementSync(postgres.NewStore(f.db), nil, EntitlementSyncConfig{}),
	} {
		if _, err := w.RunPass(context.Background()); err != nil {
			t.Fatalf("concurrent pass: %v", err)
		}
	}
	if n := f.countEntitlements(t); n != 1 {
		t.Fatalf("entitlements = %d, want 1", n)
	}
	ent := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if ent.Status != domain.EntitlementActive {
		t.Fatalf("status = %s", ent.Status)
	}
}

// --- 自然到期状态翻转（sweeper 钩子的存储层） ---

func TestMarkExpiredEntitlements(t *testing.T) {
	f := newSyncFixture(t)
	acct, err := f.store.EnsureBillingAccount(context.Background(), f.userID)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	expired, err := access.GrantFromPlan(access.PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: "sub-expired",
		ModelIDs: []string{"glm-4.6"}, PolicyVersionID: f.policyID,
	}, past.Add(-30*24*time.Hour), past.Add(-30*24*time.Hour), &past)
	if err != nil {
		t.Fatal(err)
	}
	expired.BillingAccountID = acct.ID
	if err := f.store.InsertEntitlement(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	live, err := access.GrantFromPlan(access.PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: "sub-live",
		ModelIDs: []string{"glm-4.6"}, PolicyVersionID: f.policyID,
	}, time.Now().UTC(), time.Now().UTC(), futureExpiry(30))
	if err != nil {
		t.Fatal(err)
	}
	live.BillingAccountID = acct.ID
	if err := f.store.InsertEntitlement(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	n, err := f.store.MarkExpiredEntitlements(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	got := f.onlyEntitlement(t, domain.SourceSubscription, "sub-expired")
	if got.Status != domain.EntitlementExpired {
		t.Fatalf("status = %s", got.Status)
	}
	gotLive := f.onlyEntitlement(t, domain.SourceSubscription, "sub-live")
	if gotLive.Status != domain.EntitlementActive {
		t.Fatalf("live status = %s", gotLive.Status)
	}
	// 幂等：重跑 0 行。
	n, err = f.store.MarkExpiredEntitlements(context.Background(), time.Now().UTC())
	if err != nil || n != 0 {
		t.Fatalf("rerun marked = %d, err = %v", n, err)
	}
}

// --- 审查修复：开放型订阅（expires_at NULL）的有限→开放转型 + panic 兜底 ---

// TestEntitlementSync_OpenEndedTransition: 订阅从有限期改为开放型
// （expires_at = NULL，如升级终身档）后，同步必须把现存有限权益的
// effective_to 显式置 NULL —— 不 panic、不空转。
func TestEntitlementSync_OpenEndedTransition(t *testing.T) {
	f := newSyncFixture(t)
	f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
	f.seedBenefitConfig(t, "cp_basic", f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription)
	f.seedPaidOrderFull(t, "cp_basic", &f.policyID, []string{"glm-4.6"}, model.BenefitGrantModeSubscription, model.OrderKindNew)
	exp := futureExpiry(30)
	subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", exp)

	if _, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, ""); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	before := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if before.EffectiveTo == nil {
		t.Fatal("precondition: finite effective_to")
	}

	// 订阅转为开放型（expires_at := NULL）。
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE subscriptions SET expires_at = NULL WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	n, err := f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, "")
	if err != nil {
		t.Fatalf("open-ended sync must not fail/panic: %v", err)
	}
	if n != 1 {
		t.Fatalf("sync changed %d rows, want 1", n)
	}
	after := f.onlyEntitlement(t, domain.SourceSubscription, subID)
	if after.EffectiveTo != nil {
		t.Fatalf("effective_to = %v, want NULL (有限→开放)", *after.EffectiveTo)
	}
	if after.Status != domain.EntitlementActive || after.Revision != before.Revision+1 {
		t.Fatalf("after = status %s revision %d", after.Status, after.Revision)
	}
	// 重复同步 → noop（置 NULL 后收敛稳定）。
	n, err = f.worker.SyncUserProduct(context.Background(), f.userID, model.ProductCodingPlan, "")
	if err != nil || n != 0 {
		t.Fatalf("re-sync = %d, err %v, want noop", n, err)
	}
}

// panicOnSubStore is a fake EntitlementSyncStore whose subscription read
// panics — the poisoned-message fixture for the recover test. All other
// methods are inert stubs.
type panicOnSubStore struct {
	msg        postgres.OutboxMessage
	failMarks  int
	failLastID int64
}

func (p *panicOnSubStore) Begin(context.Context) (domain.UnitOfWork, error) {
	return nil, domain.NewError(domain.CodeInternal, "panic store: begin not used")
}
func (p *panicOnSubStore) EnsureBillingAccount(context.Context, string) (*domain.BillingAccount, error) {
	return nil, domain.NewError(domain.CodeInternal, "panic store: not used")
}
func (p *panicOnSubStore) FetchPendingOutboxByTopic(context.Context, string, int) ([]postgres.OutboxMessage, error) {
	return []postgres.OutboxMessage{p.msg}, nil
}
func (p *panicOnSubStore) MarkOutboxFailed(_ context.Context, id int64, _ time.Time) error {
	p.failMarks++
	p.failLastID = id
	return nil
}
func (p *panicOnSubStore) MarkOutboxDeliveredTx(context.Context, domain.UnitOfWork, int64) error {
	return nil
}
func (p *panicOnSubStore) GetSyncSubscription(context.Context, string, string) (*access.SubscriptionState, error) {
	panic("poisoned store: subscription read panics")
}
func (p *panicOnSubStore) GetLatestPaidBenefitOrder(context.Context, string, string) (*access.OrderBenefitSnapshot, error) {
	return nil, domain.NewError(domain.CodeNotFound, "panic store: not used")
}
func (p *panicOnSubStore) GetLatestEntitlementBySource(context.Context, domain.EntitlementSource, string) (*domain.Entitlement, error) {
	return nil, domain.NewError(domain.CodeNotFound, "panic store: not used")
}
func (p *panicOnSubStore) InsertEntitlementTx(context.Context, domain.UnitOfWork, *domain.Entitlement) error {
	return nil
}
func (p *panicOnSubStore) ReviseEntitlementTx(context.Context, domain.UnitOfWork, string, domain.EntitlementPatch) (*domain.Entitlement, error) {
	return nil, nil
}
func (p *panicOnSubStore) ReviveEntitlementTx(context.Context, domain.UnitOfWork, string, domain.EntitlementPatch) (*domain.Entitlement, error) {
	return nil, nil
}
func (p *panicOnSubStore) RetireEntitlementTx(context.Context, domain.UnitOfWork, string, int, domain.EntitlementStatus) error {
	return nil
}

// TestEntitlementSync_PanicInMessageNeverCrashes pins the worker recover
// 兜底: a message whose processing panics is rescheduled into bounded
// backoff (MarkOutboxFailed), counted Failed, and the worker loop keeps
// running — no process crash, no crash loop across passes.
func TestEntitlementSync_PanicInMessageNeverCrashes(t *testing.T) {
	payload, _ := json.Marshal(access.EntitlementSyncMessage{
		UserID: "u-1", ProductCode: "coding-plan", Reason: access.SyncReasonPaymentPaid,
	})
	store := &panicOnSubStore{msg: postgres.OutboxMessage{ID: 42, Topic: access.TopicEntitlementSync, Payload: payload}}
	w := NewEntitlementSync(store, nil, EntitlementSyncConfig{})

	// 两个 pass 都不崩溃；消息每轮被标记重试（attempts 前进、进入退避）。
	for pass := 0; pass < 2; pass++ {
		stats, err := w.RunPass(context.Background())
		if err != nil {
			t.Fatalf("pass %d returned error: %v", pass, err)
		}
		if stats.Failed != 1 || stats.Delivered != 0 {
			t.Fatalf("pass %d stats = %+v, want exactly 1 failed", pass, stats)
		}
	}
	if store.failMarks != 2 || store.failLastID != 42 {
		t.Fatalf("fail marks = %d (last id %d), want 2 reschedules of message 42", store.failMarks, store.failLastID)
	}
}
