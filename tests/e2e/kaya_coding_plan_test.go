package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	infgateway "github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/inference/workers"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

// kaya_coding_plan_test.go — Task 16 全链路验收（控制者决定 2）：
//
//	购买独立套餐 → 发 Key → 调模型 → 三窗口查询 → 配额耗尽 → 正确重置 →
//	续费/退款
//
// mock 渠道（wechat_pay mock 分支）+ mock 上游（httptest SSE stub）+ 真实
// PG。支付/权益/账本事实全部落库断言；客户面只走 HTTP（JWT 管理面 +
// /v1 API Key 面），不直达 repo，保证链路与生产接线一致。

// chainServer bundles the engine + stores the chain test drives.
type chainServer struct {
	engine *gin.Engine
	db     *sqlx.DB
	store  *inferencepostgres.Store
}

// chainPolicy holds the tiny quota limits that make exhaustion reachable in
// a handful of calls (settle ≈ 15 micro/次，见 upstream stub usage 9+3)。
var chainPolicy = struct {
	fiveHour, weekly, monthly int64
	inPrice, outPrice         int64
}{
	fiveHour: 100, weekly: 1_000_000, monthly: 100_000_000,
	inPrice: 1_000_000, outPrice: 2_000_000, // microcredit / Mtok
}

// setupCodingPlanChain wires the FULL production path (支付 → 权益 worker →
// Key 管理 → /v1 网关 → 客户读视图) against the disposable e2e database and
// the stub upstream. Mirrors cmd/server 装配；wechat_pay 走 mock 分支。
func setupCodingPlanChain(t *testing.T, up *chatStubUpstream) *chainServer {
	t.Helper()
	setE2EMode(t)
	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	cleanupDB(t, db) // TRUNCATE inference 表组 CASCADE + 旧表（覆盖 wipeInferenceTables）
	seedTestData(t, db)
	ctx := context.Background()
	store := inferencepostgres.NewStore(db)

	keyDir := t.TempDir()
	privPath := keyDir + "/private.pem"
	pubPath := keyDir + "/public.pem"
	genRSAKeys(t, privPath, pubPath)

	cfg := &config.Config{
		Port:                   "0",
		DatabaseURL:            envOr("E2E_DATABASE_URL", defaultDBURL),
		RSAPrivate:             privPath,
		RSAPublic:              pubPath,
		JWTAccessTTL:           15 * time.Minute,
		JWTRefreshTTL:          168 * time.Hour,
		OrderExpiryDuration:    30 * time.Minute,
		SweeperInterval:        time.Minute,
		OAuthStateSecret:       "e2e-test-oauth-state-secret-padded-to-32-bytes",
		InferenceRecoveryGrace: 15 * time.Minute,
		InferenceAccountRPM:    120,
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	planChangeLogRepo := repo.NewPlanChangeLogRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	orderRepo := repo.NewOrderRepo(db)
	paymentRepo := repo.NewPaymentRepo(db)
	refundRepo := repo.NewRefundRepo(db)
	webhookEventRepo := repo.NewWebhookEventRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	planSvc := service.NewPlanService(planRepo, appRepo, planChangeLogRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)
	paymentSvc := service.NewPaymentService(
		db, orderRepo, paymentRepo, refundRepo, subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo, &stubRefundAPI{},
		&wechat.Client{MockMode: true}, cfg.OrderExpiryDuration)
	paymentSvc.SetBenefitRepo(repo.NewPlanBenefitRepo(db))
	paymentSvc.SetBenefitSync(store)
	subSvc.SetBenefitSync(db, store)

	// --- inference catalog：模型/供应商/部署（公网占位发布 + stub 运行面）/策略/价格。
	const modelID = "glm-4.6"
	if err := store.InsertModel(ctx, &domain.Model{
		ID: modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	prov := &domain.Provider{Code: "glm", DisplayName: "GLM", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("provider: %v", err)
	}
	pol := &inferencepostgres.PolicyVersion{
		Name: "cp-chain", Revision: 1, ModelIDs: []string{modelID},
		FiveHourLimit: e2eMicro(chainPolicy.fiveHour), WeeklyLimit: e2eMicro(chainPolicy.weekly),
		MonthlyLimit: e2eMicro(chainPolicy.monthly), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if err := store.InsertPriceVersion(ctx, &inferencepostgres.PriceVersion{
		ModelID: modelID, Kind: inferencepostgres.PriceKind(accounting.PriceSaleCredit),
		Unit: "microcredit", InputPerMtok: chainPolicy.inPrice, OutputPerMtok: chainPolicy.outPrice,
		Revision: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("price: %v", err)
	}
	catalogSvc := catalog.NewService(store)
	catalogSvc.SetPriceCheck(func(ctx context.Context, id string) (bool, error) {
		_, err := store.LatestPriceVersion(ctx, id, "sale_credit", time.Now())
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	depPub := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-4.6-upstream",
		BaseURL: "https://api.glm.example.com", Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 30 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depPub); err != nil {
		t.Fatalf("publish deployment: %v", err)
	}
	if err := store.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: modelID, DeploymentID: depPub.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("publish route: %v", err)
	}
	if _, err := catalogSvc.Publish(ctx, "e2e"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-4.6-upstream", BaseURL: up.URL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 30 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if err := store.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("route: %v", err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	credSvc := credentials.NewService(vault, store, store)
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "e2e"},
		prov.ID, "main", "api_key", "sk-upstream-chain", "seed", nil)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if err := store.InsertUpstreamAccount(ctx, &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cv.ID, Status: domain.AccountActive, ConcurrencyLimit: 8,
	}); err != nil {
		t.Fatalf("upstream account: %v", err)
	}

	snap := &catalog.Snapshot{
		Models: map[string]domain.Model{modelID: {
			ID: modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		}},
		Providers:   map[string]domain.Provider{prov.ID: *prov},
		Deployments: map[string]domain.Deployment{dep.ID: *dep},
		RoutesByModel: map[string][]domain.ModelRoute{modelID: {{
			ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		}}},
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	adapters := map[domain.Protocol]providers.Adapter{domain.ProtocolOpenAIChat: providers.NewOpenAIChat()}
	routingSvc := routing.NewService(store, adapters, nil)
	accessResolver := access.NewResolver(store, nil)
	gw := infgateway.NewService(staticSnapE2E{snap}, store, access.NewEntitlementResolver(store, nil),
		quota.NewService(store, nil), routingSvc, credSvc, providers.NewHTTPClient(egress), egress, nil)

	rpmCounter := access.NewRPMCounter(nil)
	accessOps := &httpapi.AccessOps{
		UserAPIKeys:       httpapi.NewUserAPIKeysHandler(access.NewKeyService(store, nil)),
		V1Auth:            httpapi.APIKeyAuth(accessResolver, rpmCounter, cfg.InferenceAccountRPM),
		RPMCounter:        rpmCounter,
		V1Models:          httpapi.NewModelsHandler(catalogSvc, accessResolver),
		V1ChatCompletions: httpapi.NewChatCompletionsHandler(gw),
		UserQuotas:        httpapi.NewUserQuotasHandler(management.NewQuotaViewService(store, nil)),
		UserUsage:         httpapi.NewUserUsageHandler(management.NewUsageViewService(store, nil)),
		UserSubscriptions: httpapi.NewUserSubscriptionsHandler(management.NewSubscriptionViewService(store, nil)),
	}

	mv := &middleware.MultiChannelVerifier{
		WeChat: &middleware.WeChatPayV3Verifier{APIv3Key: []byte(e2eWeChatKey), MockMode: true},
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	quoteSvc := service.NewQuoteService(planRepo, appRepo)
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	t.Cleanup(cancelSetup)
	router.Setup(setupCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, mv, []byte(e2eWeChatKey),
		service.NewProviderTokenService(appRepo, nil), quoteSvc,
		service.NewChatService(nil, subRepo, planRepo, repo.NewLLMUsageRepo(db)), nil,
		service.NewGitHubOAuthService(cfg.OAuthStateSecret),
		service.NewWeChatOAuthService(cfg.OAuthStateSecret),
		false, true, /* wechatPayMock */
		service.NewUsageService(repo.NewUsageRepo(db)), nil, nil, accessOps, nil, nil, nil, nil)

	// 在售商品 + 支付/权益配置（029 快照源）。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code, is_active, accepting_new_subscriptions)
		VALUES ('cp_basic', 'Coding Plan Basic', 29.9, 30, '{}', 'CNY', 'coding-plan', true, true)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		VALUES ('cp_basic', $1, '{glm-4.6}', 'subscription')
		ON CONFLICT (plan_id) DO NOTHING`, pol.ID); err != nil {
		t.Fatalf("benefit config: %v", err)
	}

	return &chainServer{engine: engine, db: db, store: store}
}

// wechatMockEvent posts one mock wechat_pay webhook event.
func wechatMockEvent(t *testing.T, engine *gin.Engine, id, eventType, outTradeNo string, total, refund int) {
	t.Helper()
	body := fmt.Sprintf(
		`{"id":%q,"event_type":%q,"resource":{"transaction_id":"wx_%s","out_trade_no":%q,"amount":{"total":%d,"refund":%d}}}`,
		id, eventType, outTradeNo, outTradeNo, total, refund)
	resp := doRequest(t, engine, http.MethodPost, "/webhooks/payment/wechat_pay", body,
		map[string]string{
			"Wechatpay-Signature": "mock-bypass-not-validated",
			"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
			"Wechatpay-Nonce":     "mocknonce",
			"Content-Type":        "application/json",
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook %s: %d — %s", eventType, resp.StatusCode, string(resp.Body))
	}
}

// chainPurchase places one cp_basic order and pays it via the mock channel;
// returns the order ID.
func chainPurchase(t *testing.T, s *chainServer, token string) string {
	t.Helper()
	resp := doRequest(t, s.engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"cp_basic","channel":"wechat_pay"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d — %s", resp.StatusCode, string(resp.Body))
	}
	var created struct {
		Data struct {
			ID             string `json:"id"`
			ProviderIntent struct {
				OutTradeNo string `json:"out_trade_no"`
			} `json:"provider_intent"`
		} `json:"data"`
	}
	resp.JSON(t, &created)
	orderID := created.Data.ID
	wechatMockEvent(t, s.engine, "evt_chain_"+orderID, "TRANSACTION.SUCCESS",
		created.Data.ProviderIntent.OutTradeNo, 2990, 0)
	return orderID
}

// chainChat posts one /v1/chat/completions with the customer API key and
// returns the HTTP status + raw body.
func chainChat(t *testing.T, s *chainServer, apiKey string) (int, string, http.Header) {
	t.Helper()
	resp := doRequest(t, s.engine, http.MethodPost, "/v1/chat/completions",
		`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":true}`,
		map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json"})
	return resp.StatusCode, string(resp.Body), resp.Headers
}

// chainQuotas reads GET /user/model-quotas with the JWT.
func chainQuotas(t *testing.T, s *chainServer, token string) (int, []byte) {
	t.Helper()
	resp := doRequest(t, s.engine, http.MethodGet, "/user/model-quotas", "", authHeader(token))
	return resp.StatusCode, resp.Body
}

func TestKayaCodingPlan_FullChain(t *testing.T) {
	up := newChatStubUpstream(t)
	s := setupCodingPlanChain(t, up)
	ctx := context.Background()
	login := loginAndGetTokens(t, s.engine, "cp-chain-user", "yundian")
	token := login.AccessToken
	userID := login.User.ID
	syncWorker := workers.NewEntitlementSync(s.store, nil, workers.EntitlementSyncConfig{})

	// ---------- 1. 购买独立套餐（mock 渠道；重复回调幂等；Kaya 隔离） ----------
	orderID := chainPurchase(t, s, token)
	var outTradeNo string
	if err := s.db.GetContext(ctx, &outTradeNo,
		`SELECT provider_intent->>'out_trade_no' FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	wechatMockEvent(t, s.engine, "evt_chain_"+orderID, "TRANSACTION.SUCCESS", outTradeNo, 2990, 0) // 重投
	stats, err := syncWorker.RunPass(ctx)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if stats.Granted != 1 {
		t.Fatalf("worker stats = %+v, want granted=1 (重投被支付键去重)", stats)
	}
	var kayaSubs int
	if err := s.db.GetContext(ctx, &kayaSubs,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1 AND product_code = 'kaya-membership'`, userID); err != nil {
		t.Fatal(err)
	}
	if kayaSubs != 0 {
		t.Fatalf("coding-plan purchase touched kaya-membership: %d rows", kayaSubs)
	}
	var firstExpiry time.Time
	if err := s.db.GetContext(ctx, &firstExpiry,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1 AND product_code = 'coding-plan' AND status = 'active'`, userID); err != nil {
		t.Fatalf("active coding-plan sub: %v", err)
	}

	// ---------- 2. 发 Key（明文只返回一次；管理面不存明文） ----------
	resp := doRequest(t, s.engine, http.MethodPost, "/user/api-keys",
		`{"name":"chain-key","model_ids":["glm-4.6"]}`, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create key: %d — %s", resp.StatusCode, string(resp.Body))
	}
	var keyEnv struct {
		Data struct {
			Key    string `json:"key"`
			ID     string `json:"id"`
			Prefix string `json:"prefix"`
		} `json:"data"`
	}
	resp.JSON(t, &keyEnv)
	apiKey := keyEnv.Data.Key
	if !strings.HasPrefix(apiKey, "yk-") || keyEnv.Data.Prefix == "" {
		t.Fatalf("key shape = %+v", keyEnv.Data)
	}
	var hashCount int
	if err := s.db.GetContext(ctx, &hashCount,
		`SELECT COUNT(*) FROM inference_api_keys WHERE id = $1 AND key_hash IS NOT NULL AND key_hash <> $2`,
		keyEnv.Data.ID, apiKey); err != nil {
		t.Fatal(err)
	}
	if hashCount != 1 {
		t.Fatal("key material must not be stored; digest only")
	}

	// ---------- 3. 调模型（/v1 网关 → mock 上游 → 结算落库） ----------
	status, body, _ := chainChat(t, s, apiKey)
	if status != http.StatusOK {
		t.Fatalf("first call: %d — %s", status, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing [DONE]: %s", body)
	}
	// 上游 stub usage = 9 in + 3 out → 9×1 + 3×2 = 15 micro（逐行 RoundUp）。
	settledOf := func() (int, int64) {
		var n int
		var sum int64
		if err := s.db.QueryRowxContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(settled_micros),0) FROM inference_requests
			 WHERE status = 'settled'`).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	nSettled, sumSettled := settledOf()
	if nSettled != 1 || sumSettled != 15 {
		t.Fatalf("settled = %d reqs / %d micro, want 1 / 15", nSettled, sumSettled)
	}
	var chargeRows int
	if err := s.db.GetContext(ctx, &chargeRows,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type = 'charge'`); err != nil {
		t.Fatal(err)
	}
	if chargeRows != 1 {
		t.Fatalf("ledger charge rows = %d, want exactly 1 (一次请求一条 charge)", chargeRows)
	}

	// ---------- 4. 三窗口查询（JWT 读视图；server_time + 三窗口 used/reserved/remaining） ----------
	status, quotasBody := chainQuotas(t, s, token)
	if status != http.StatusOK {
		t.Fatalf("quotas: %d — %s", status, quotasBody)
	}
	var quotas struct {
		Data struct {
			ServerTime string `json:"server_time"`
			Windows    []struct {
				Kind      string  `json:"kind"`
				Used      string  `json:"used"`
				Reserved  string  `json:"reserved"`
				Remaining *string `json:"remaining"`
				ResetsAt  *string `json:"resets_at"`
			} `json:"windows"`
			BlockedBy []any `json:"blocked_by"`
		} `json:"data"`
	}
	if err := respFromBytes(quotasBody, &quotas); err != nil {
		t.Fatal(err)
	}
	if quotas.Data.ServerTime == "" {
		t.Fatal("server_time missing (三窗口卡片必须以服务端时间为准)")
	}
	winUsed := map[string]string{}
	for _, w := range quotas.Data.Windows {
		winUsed[w.Kind] = w.Used
		if w.Reserved != "0" {
			t.Fatalf("window %s reserved = %s after settlement, want 0", w.Kind, w.Reserved)
		}
		if w.Remaining == nil || w.ResetsAt == nil {
			t.Fatalf("window %s missing remaining/resets_at: %+v", w.Kind, w)
		}
	}
	for _, kind := range []string{"five_hour", "weekly", "monthly"} {
		if winUsed[kind] != "15" {
			t.Fatalf("window %s used = %q, want \"15\" (同一消费三窗口同增)", kind, winUsed[kind])
		}
	}

	// ---------- 5. 配额耗尽（五小时窗口封顶 → 429 quota_exceeded，明确 blocked_by） ----------
	blocked := ""
	blockedHeader := http.Header{}
	okCalls := 0
	for i := 0; i < 10 && blocked == ""; i++ {
		st, bd, hdr := chainChat(t, s, apiKey)
		if st == http.StatusOK {
			okCalls++
			continue
		}
		if st != http.StatusTooManyRequests {
			t.Fatalf("call %d: %d — %s (want 200 或 429)", i, st, bd)
		}
		blocked, blockedHeader = bd, hdr
		t.Logf("exhaustion at call %d: %s", i+1, bd)
	}
	if okCalls == 0 || blocked == "" {
		t.Fatalf("exhaustion not reached: ok=%d", okCalls)
	}
	var v1err struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := respFromBytes([]byte(blocked), &v1err); err != nil {
		t.Fatal(err)
	}
	if v1err.Error.Code != "quota_exceeded" || !strings.Contains(v1err.Error.Message, "five_hour") {
		t.Fatalf("429 body = %s, want quota_exceeded + five_hour blocked_by", blocked)
	}
	if blockedHeader.Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After (恢复时间可计算时必须给出)")
	}
	// 被拒的请求不留任何痕迹（无请求行/无预占/无账本）。总请求数 =
	// 第 3 步首次调用(1) + 耗尽循环放行数(okCalls)。
	nSettledAfter, _ := settledOf()
	var totalReqs int
	if err := s.db.GetContext(ctx, &totalReqs, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if wantReqs := okCalls + 1; totalReqs != wantReqs || nSettledAfter != wantReqs {
		t.Fatalf("requests = %d settled = %d want %d (被拒请求留痕!)", totalReqs, nSettledAfter, wantReqs)
	}
	// 视图阻断契约（Task 11：窗口耗尽 remaining=0 才进 blocked_by）：上一步
	// 429 时窗口剩余 25（hold 31 放不进剩余额度即被拒——准入侧阻断），并发
	// 场景下其余 Key 会把剩余额度消费到 0。这里直接把窗口顶到上限模拟该
	// 终态，断言读视图 blocked_by 显式给出 five_hour + resets_at。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inference_quota_windows SET used_micros = limit_micros WHERE kind = 'five_hour'`); err != nil {
		t.Fatal(err)
	}
	status, quotasBody = chainQuotas(t, s, token)
	if status != http.StatusOK {
		t.Fatalf("quotas after exhaustion: %d", status)
	}
	var blockedView struct {
		Data struct {
			BlockedBy []struct {
				Kind      string  `json:"kind"`
				Reason    string  `json:"reason"`
				Remaining *string `json:"remaining"`
				ResetsAt  *string `json:"resets_at"`
			} `json:"blocked_by"`
		} `json:"data"`
	}
	if err := respFromBytes(quotasBody, &blockedView); err != nil {
		t.Fatal(err)
	}
	fiveHourBlocked := false
	for _, b := range blockedView.Data.BlockedBy {
		if b.Kind == "five_hour" && b.ResetsAt != nil && b.Remaining != nil && *b.Remaining == "0" {
			fiveHourBlocked = true
		}
	}
	if !fiveHourBlocked {
		t.Fatalf("blocked_by = %s, want five_hour with resets_at and remaining=0", quotasBody)
	}

	// ---------- 6. 正确重置（窗口期过后新请求激活新窗口；旧窗口行保留不动） ----------
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inference_quota_windows
		    SET window_start = window_start - interval '6 hours',
		        window_end   = window_end   - interval '6 hours'
		  WHERE kind = 'five_hour'`); err != nil {
		t.Fatalf("age five-hour window: %v", err)
	}
	status, body, _ = chainChat(t, s, apiKey)
	if status != http.StatusOK {
		t.Fatalf("call after window reset: %d — %s (旧窗口已过期,新窗口必须放行)", status, body)
	}
	var fiveHourWindows, oldUsed int
	if err := s.db.QueryRowxContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(used_micros) FILTER (WHERE window_end < now()),0)
		   FROM inference_quota_windows WHERE kind = 'five_hour'`).Scan(&fiveHourWindows, &oldUsed); err != nil {
		t.Fatal(err)
	}
	if fiveHourWindows != 2 {
		t.Fatalf("five_hour windows = %d, want 2 (旧窗口保留 + 新窗口激活)", fiveHourWindows)
	}
	status, quotasBody = chainQuotas(t, s, token)
	if status != http.StatusOK {
		t.Fatalf("quotas after reset: %d", status)
	}
	var afterReset struct {
		Data struct {
			Windows []struct {
				Kind string `json:"kind"`
				Used string `json:"used"`
			} `json:"windows"`
			BlockedBy []any `json:"blocked_by"`
		} `json:"data"`
	}
	if err := respFromBytes(quotasBody, &afterReset); err != nil {
		t.Fatal(err)
	}
	for _, w := range afterReset.Data.Windows {
		if w.Kind == "five_hour" && w.Used != "15" {
			t.Fatalf("new five_hour window used = %q, want \"15\" (重置后只计新消费)", w.Used)
		}
	}
	if len(afterReset.Data.BlockedBy) != 0 {
		t.Fatalf("blocked_by after reset = %s, want empty", quotasBody)
	}

	// ---------- 7. 续费（同档再购 → 有效期顺延 + 权益修订不重复发放） ----------
	ledgerBefore := 0
	if err := s.db.GetContext(ctx, &ledgerBefore, `SELECT COUNT(*) FROM inference_ledger_entries`); err != nil {
		t.Fatal(err)
	}
	renewOrder := chainPurchase(t, s, token)
	stats, err = syncWorker.RunPass(ctx)
	if err != nil {
		t.Fatalf("worker after renewal: %v", err)
	}
	if stats.Granted != 1 && stats.Revised != 1 {
		t.Fatalf("renewal sync = %+v, want one grant/revise effect", stats)
	}
	var renewedExpiry time.Time
	if err := s.db.GetContext(ctx, &renewedExpiry,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1 AND product_code = 'coding-plan' AND status = 'active'`, userID); err != nil {
		t.Fatal(err)
	}
	if !renewedExpiry.After(firstExpiry) {
		t.Fatalf("renewal did not extend: first %v renewed %v", firstExpiry, renewedExpiry)
	}
	var entCount int
	if err := s.db.GetContext(ctx, &entCount,
		`SELECT COUNT(*) FROM inference_entitlements e
		   JOIN inference_billing_accounts a ON a.id = e.billing_account_id
		  WHERE a.user_id = $1 AND e.status = 'active'`, userID); err != nil {
		t.Fatal(err)
	}
	if entCount != 1 {
		t.Fatalf("active entitlements = %d, want 1 (续费不重复发放)", entCount)
	}

	// ---------- 8. 退款（全额 → 订阅取消 + 权益吊销 → 调用 403；账本保留） ----------
	var renewOutTradeNo string
	if err := s.db.GetContext(ctx, &renewOutTradeNo,
		`SELECT provider_intent->>'out_trade_no' FROM orders WHERE id = $1`, renewOrder); err != nil {
		t.Fatal(err)
	}
	wechatMockEvent(t, s.engine, "evt_chain_rf_"+renewOrder, "TRANSACTION.REFUND", renewOutTradeNo, 0, 2990)
	if _, err := syncWorker.RunPass(ctx); err != nil {
		t.Fatalf("worker after refund: %v", err)
	}
	var entStatus string
	if err := s.db.GetContext(ctx, &entStatus,
		`SELECT e.status FROM inference_entitlements e
		   JOIN inference_billing_accounts a ON a.id = e.billing_account_id
		  WHERE a.user_id = $1 ORDER BY e.created_at DESC LIMIT 1`, userID); err != nil {
		t.Fatal(err)
	}
	if entStatus != "revoked" {
		t.Fatalf("entitlement after refund = %s, want revoked", entStatus)
	}
	status, body, _ = chainChat(t, s, apiKey)
	if status != http.StatusForbidden {
		t.Fatalf("call after refund: %d — %s, want 403 model_not_allowed", status, body)
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Fatalf("after-refund body = %s, want model_not_allowed", body)
	}
	ledgerAfter := 0
	if err := s.db.GetContext(ctx, &ledgerAfter, `SELECT COUNT(*) FROM inference_ledger_entries`); err != nil {
		t.Fatal(err)
	}
	if ledgerAfter != ledgerBefore {
		t.Fatalf("ledger changed by refund: before %d after %d (财务事实必须保留)", ledgerBefore, ledgerAfter)
	}
	nSettledEnd, _ := settledOf()
	// 首次调用 + 耗尽循环放行 + 重置后调用 = 全部已结算；退款不删历史。
	if wantEnd := okCalls + 2; nSettledEnd != wantEnd {
		t.Fatalf("settled requests = %d, want %d (退款不删历史结算)", nSettledEnd, wantEnd)
	}
}

// respFromBytes unmarshals one JSON body (doRequest bodies are []byte).
func respFromBytes(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
