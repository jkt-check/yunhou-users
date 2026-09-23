package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/workers"
)

// coding_plan_purchase_test.go — Task 10 端到端验收：HTTP 层下单 + mock 渠道
// 支付回调 → 同事务 outbox → entitlement-sync worker → 权益解析 → 配额准入
// （"从下单到 Key 调用有额度"）。另钉住：产品隔离（不动 Kaya 会员）、重复
// 回调幂等、全额退款吊销。

// seedCodingPlanCatalog inserts the inference catalog + a sellable
// coding-plan plan with its payment/benefit configuration (029).
func seedCodingPlanCatalog(t *testing.T, srv *E2EServer) (policyID string) {
	t.Helper()
	ctx := context.Background()
	store := inferencepostgres.NewStore(srv.DB)

	if err := store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	pol := &inferencepostgres.PolicyVersion{
		Name: "cp-policy-e2e", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: e2eMicro(1_000_000), WeeklyLimit: e2eMicro(10_000_000),
		MonthlyLimit: e2eMicro(100_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if err := store.InsertPriceVersion(ctx, &inferencepostgres.PriceVersion{
		ModelID: "glm-4.6", Kind: inferencepostgres.PriceKind(accounting.PriceSaleCredit),
		Unit: "microcredit", InputPerMtok: 100, OutputPerMtok: 200,
		Revision: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("price: %v", err)
	}
	if _, err := srv.DB.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code, is_active, accepting_new_subscriptions)
		VALUES ('cp_basic', 'Coding Plan Basic', 29.9, 30, '{}', 'CNY', 'coding-plan', true, true)
	`); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := srv.DB.ExecContext(ctx, `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		VALUES ('cp_basic', $1, '{glm-4.6}', 'subscription')
	`, pol.ID); err != nil {
		t.Fatalf("benefit config: %v", err)
	}
	return pol.ID
}

func e2eMicro(v int64) *domain.Microcredit { m := domain.Microcredit(v); return &m }

func TestCodingPlan_PurchaseToQuotaEndToEnd(t *testing.T) {
	srv := setupE2EServerWithMockWeChatPay(t)
	policyID := seedCodingPlanCatalog(t, srv)
	ctx := context.Background()

	token := loginAndGetTokens(t, srv.Engine, "cp-e2e-user", "yundian").AccessToken

	// 1. 下单（mock 渠道）——订单必须冻结权益快照。
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"cp_basic","channel":"wechat_pay"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d — %s", resp.StatusCode, string(resp.Body))
	}
	var created struct {
		Data struct {
			ID             string  `json:"id"`
			ProductCode    string  `json:"product_code"`
			Amount         float64 `json:"amount"`
			ProviderIntent struct {
				OutTradeNo string `json:"out_trade_no"`
			} `json:"provider_intent"`
		} `json:"data"`
	}
	resp.JSON(t, &created)
	orderID := created.Data.ID
	if created.Data.ProductCode != "coding-plan" {
		t.Fatalf("order product_code = %q", created.Data.ProductCode)
	}
	var snapPolicy string
	if err := srv.DB.GetContext(ctx, &snapPolicy,
		`SELECT benefit_policy_version_id::text FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if snapPolicy != policyID {
		t.Fatalf("order snapshot policy = %s, want %s", snapPolicy, policyID)
	}
	outTradeNo := created.Data.ProviderIntent.OutTradeNo

	// 2. mock 渠道支付回调（HTTP 层）。
	body := []byte(fmt.Sprintf(
		`{"id":"evt_cp_%s","event_type":"TRANSACTION.SUCCESS","resource":{"transaction_id":"wx_cp_%s","out_trade_no":"%s","amount":{"total":2990}}}`,
		orderID, outTradeNo, outTradeNo,
	))
	ts := time.Now().Unix()
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/wechat_pay", string(body),
		map[string]string{
			"Wechatpay-Signature": "mock-bypass-not-validated",
			"Wechatpay-Timestamp": strconv.FormatInt(ts, 10),
			"Wechatpay-Nonce":     "mocknonce",
			"Content-Type":        "application/json",
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pay webhook: %d — %s", resp.StatusCode, string(resp.Body))
	}

	// 重复投递同一事件 → 200 且幂等。
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/wechat_pay", string(body),
		map[string]string{
			"Wechatpay-Signature": "mock-bypass-not-validated",
			"Wechatpay-Timestamp": strconv.FormatInt(ts, 10),
			"Wechatpay-Nonce":     "mocknonce",
			"Content-Type":        "application/json",
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pay webhook replay: %d — %s", resp.StatusCode, string(resp.Body))
	}

	var userID string
	if err := srv.DB.GetContext(ctx, &userID, `SELECT user_id::text FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}

	// 订阅激活在 coding-plan 产品下；Kaya 会员无任何订阅行（产品隔离）。
	var subID, subProduct string
	var subExpiry time.Time
	if err := srv.DB.QueryRowxContext(ctx,
		`SELECT id::text, product_code, expires_at FROM subscriptions WHERE user_id = $1 AND status = 'active'`, userID).
		Scan(&subID, &subProduct, &subExpiry); err != nil {
		t.Fatalf("active sub: %v", err)
	}
	if subProduct != "coding-plan" {
		t.Fatalf("sub product = %q", subProduct)
	}
	var kayaSubs int
	if err := srv.DB.GetContext(ctx, &kayaSubs,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1 AND product_code = 'kaya-membership'`, userID); err != nil {
		t.Fatal(err)
	}
	if kayaSubs != 0 {
		t.Fatalf("API 套餐支付创建了 Kaya 订阅: %d rows", kayaSubs)
	}

	// 3. 同事务 outbox → worker 消费 → 权益发放。
	var pending int
	if err := srv.DB.GetContext(ctx, &pending,
		`SELECT COUNT(*) FROM inference_outbox WHERE topic = 'entitlement.sync' AND status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending outbox = %d, want 1 (duplicate webhook deduped by payment key)", pending)
	}
	syncWorker := workers.NewEntitlementSync(inferencepostgres.NewStore(srv.DB), nil, workers.EntitlementSyncConfig{})
	stats, err := syncWorker.RunPass(ctx)
	if err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	if stats.Granted != 1 {
		t.Fatalf("worker stats = %+v", stats)
	}
	// 再来一轮（重放安全网）：全部 noop。
	stats, err = syncWorker.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Granted != 0 || stats.Revised != 0 {
		t.Fatalf("second pass had effects: %+v", stats)
	}

	// 4. 权益断言：来源订阅、快照规格、有效期 = 订阅到期点。
	store := inferencepostgres.NewStore(srv.DB)
	ent, err := store.GetLatestEntitlementBySource(ctx, domain.SourceSubscription, subID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if ent.Status != domain.EntitlementActive || ent.PolicyVersionID != policyID {
		t.Fatalf("entitlement = %+v", ent)
	}
	if ent.EffectiveTo == nil || !ent.EffectiveTo.Equal(subExpiry) {
		t.Fatalf("effective_to = %v, want %v", ent.EffectiveTo, subExpiry)
	}

	// 5. "Key 调用有额度"：resolver 选中权益 → 配额准入放行。
	acct, err := store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		t.Fatalf("billing account: %v", err)
	}
	resolver := access.NewEntitlementResolver(store, nil)
	picked, err := resolver.Resolve(ctx, acct.ID, "glm-4.6", time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if picked.ID != ent.ID {
		t.Fatalf("picked %s, want %s", picked.ID, ent.ID)
	}
	polRow, err := store.GetPolicyVersion(ctx, ent.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	priceRow, err := store.LatestPriceVersion(ctx, "glm-4.6", string(accounting.PriceSaleCredit), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	creditPrice, err := priceRow.Pure()
	if err != nil {
		t.Fatal(err)
	}
	mdl, err := store.GetModel(ctx, "glm-4.6")
	if err != nil {
		t.Fatal(err)
	}
	quotaSvc := quota.NewService(store, nil)
	outCap := int64(1000)
	adm, err := quotaSvc.Admit(ctx, quota.AdmitCommand{
		Request: domain.Request{
			BillingAccountID: acct.ID, EntitlementID: ent.ID,
			ModelID: "glm-4.6", Protocol: domain.ProtocolOpenAIChat,
		},
		Entitlement: *picked, Policy: polRow.Pure(), CreditPrice: creditPrice, Model: *mdl,
		EstimatedInputTokens: 100, ClientMaxTokens: &outCap,
	})
	if err != nil {
		t.Fatalf("admit after purchase: %v (从下单到 Key 调用必须有额度)", err)
	}
	if adm.HoldMicros <= 0 {
		t.Fatalf("hold = %v, want a positive reservation", adm.HoldMicros)
	}
	// 释放准入，保持账本干净（测试库共享）。
	uow, err := store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, uow, adm.RequestID); err != nil {
		t.Fatalf("release admission: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 6. 全额退款（mock 渠道）→ 订阅取消 → worker 吊销权益 → 再调用被拒。
	refundBody := []byte(fmt.Sprintf(
		`{"id":"evt_cprf_%s","event_type":"TRANSACTION.REFUND","resource":{"transaction_id":"wx_cp_%s","out_trade_no":"%s","amount":{"refund":2990}}}`,
		orderID, outTradeNo, outTradeNo,
	))
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/wechat_pay", string(refundBody),
		map[string]string{
			"Wechatpay-Signature": "mock-bypass-not-validated",
			"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
			"Wechatpay-Nonce":     "mocknonce",
			"Content-Type":        "application/json",
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refund webhook: %d — %s", resp.StatusCode, string(resp.Body))
	}
	if _, err := syncWorker.RunPass(ctx); err != nil {
		t.Fatal(err)
	}
	entAfter, err := store.GetLatestEntitlementBySource(ctx, domain.SourceSubscription, subID)
	if err != nil {
		t.Fatal(err)
	}
	if entAfter.Status != domain.EntitlementRevoked {
		t.Fatalf("after refund: status = %s, want revoked (后续调用必须失去授权)", entAfter.Status)
	}
	if _, err := resolver.Resolve(ctx, acct.ID, "glm-4.6", time.Now()); err == nil {
		t.Fatal("refunded account must not resolve any entitlement")
	}
}
