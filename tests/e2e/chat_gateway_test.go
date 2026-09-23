package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	infgateway "github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

// chat_gateway_test.go — 旧 Kaya /chat e2e 回归(Task 8 验收硬项)。
//
// 同一 handler 契约(JWT 鉴权、SSE relay、{code,data,message} 错误
// envelope、无 model 默认、工具/思考开关)在两种模式下各跑一遍:
//   - legacy:INFERENCE_KAYA_CHAT_GATEWAY 关闭(默认),DeepSeek 直通;
//   - facade:开关打开,/chat 经 inference 网关(权益 → 预占 → 路由 →
//     上游 → 结算),对外行为不变。

// chatStubUpstream serves a canned OpenAI SSE stream and records payloads.
type chatStubUpstream struct {
	*httptest.Server
	lastBody chan []byte
}

func newChatStubUpstream(t *testing.T) *chatStubUpstream {
	t.Helper()
	u := &chatStubUpstream{lastBody: make(chan []byte, 4)}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case u.lastBody <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range []string{
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n",
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, c)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func setE2EMode(t *testing.T) {
	t.Helper()
	if err := os.Setenv("PAYPAL_L3_E2E_MODE", "1"); err != nil {
		t.Fatalf("set PAYPAL_L3_E2E_MODE: %v", err)
	}
}

func wipeInferenceTables(t *testing.T, db *sqlx.DB) {
	t.Helper()
	// The shared e2e database may carry inference rows from earlier suites;
	// the facade assertions need a clean inference ledger.
	// Child tables first: requests/attempts/usage/leases/reservations
	// reference windows, accounts reference users — delete in FK-safe order.
	tables := []string{
		"inference_audit_log", "operator_roles",
		"inference_reconciliation_jobs", "inference_outbox",
		"inference_ledger_entries", "inference_adjustments",
		"inference_concurrency_leases", "inference_reservations",
		"inference_usage_records",
		"inference_attempts", "inference_requests",
		"inference_quota_windows",
		"inference_entitlements", "inference_policy_versions",
		"inference_price_versions",
		"inference_api_keys", "inference_billing_accounts",
		"inference_upstream_accounts", "inference_credentials",
		"inference_config_revisions", "inference_model_routes",
		"inference_deployments", "inference_providers", "inference_models",
	}
	for _, tbl := range tables {
		if _, err := db.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("wipe %s: %v", tbl, err)
		}
	}
}

// chatE2EBase wires the shared plumbing (DB wipe + seed, RSA keys,
// token/auth services) both chat setups need.
func chatE2EBase(t *testing.T) (*sqlx.DB, *service.TokenService, *service.AuthService) {
	t.Helper()
	setE2EMode(t)
	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	// Wipe inference tables FIRST: inference_billing_accounts references
	// users without cascade (去标识化账本边界), so the shared cleanup's
	// users delete would violate the FK otherwise.
	wipeInferenceTables(t, db)
	cleanupDB(t, db)
	seedTestData(t, db)

	keyDir := t.TempDir()
	privPath := keyDir + "/private.pem"
	pubPath := keyDir + "/public.pem"
	genRSAKeys(t, privPath, pubPath)

	cfg := &config.Config{
		DatabaseURL:         envOr("E2E_DATABASE_URL", defaultDBURL),
		RSAPrivate:          privPath,
		RSAPublic:           pubPath,
		JWTAccessTTL:        15 * time.Minute,
		JWTRefreshTTL:       168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     time.Minute,
		OAuthStateSecret:    "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}
	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	appRepo := repo.NewAppRepo(db)
	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	return db, tokenSvc, authSvc
}

func chatRouterSetup(ctx context.Context, engine *gin.Engine, db *sqlx.DB,
	tokenSvc *service.TokenService, authSvc *service.AuthService,
	chatSvc *service.ChatService, accessOps *httpapi.AccessOps) {
	router.Setup(ctx, engine, db,
		repo.NewAppRepo(db), repo.NewUserRepo(db), repo.NewSocialIdentityRepo(db),
		repo.NewPlanRepo(db), repo.NewSubscriptionRepo(db), repo.NewSessionRepo(db),
		tokenSvc, authSvc, nil, nil, nil,
		&middleware.MultiChannelVerifier{}, nil,
		nil, nil, chatSvc, nil, nil, nil, false, false,
		service.NewUsageService(repo.NewUsageRepo(db)), nil, nil, accessOps, nil, nil)
}

// setupChatE2E builds the engine in LEGACY mode: a real ChatService pointed
// at the stub upstream (迁移开关关闭 = 旧行为直通).
func setupChatE2E(t *testing.T, up *chatStubUpstream) *gin.Engine {
	t.Helper()
	db, tokenSvc, authSvc := chatE2EBase(t)
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	chatSvc := service.NewChatService(llm.LegacyCatalog("sk-legacy-test", up.URL, "deepseek-v4-flash"),
		repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewLLMUsageRepo(db))
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chatRouterSetup(setupCtx, engine, db, tokenSvc, authSvc, chatSvc, nil)
	return engine
}

// setupChatFacadeE2E builds the facade-mode engine: /chat runs through the
// inference gateway with a static catalog snapshot pointing at the stub
// upstream (迁移开关开启的等价接线).
func setupChatFacadeE2E(t *testing.T, up *chatStubUpstream) (*gin.Engine, *sqlx.DB, *postgres.Store) {
	t.Helper()
	db, tokenSvc, authSvc := chatE2EBase(t)
	ctx := context.Background()
	store := postgres.NewStore(db)

	// Catalog rows: model (active, kaya_chat+openai_chat), provider,
	// loopback deployment (repo insert bypasses write-path URL validation
	// deliberately — publish validation is covered by the catalog tests),
	// route, policy, price, credential, upstream account.
	const modelID = "deepseek-chat"
	if err := store.InsertModel(ctx, &domain.Model{
		ID: modelID, DisplayName: "DeepSeek Chat", Lifecycle: domain.LifecycleActive,
		ContextTokens: 128000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		SupportsTools: true, SupportsReasoning: true,
	}); err != nil {
		t.Fatalf("model: %v", err)
	}
	prov := &domain.Provider{Code: "deepseek", DisplayName: "DeepSeek", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("provider: %v", err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-gift", Revision: 1, ModelIDs: []string{modelID},
		FiveHourLimit: microE2E(1_000_000_000), WeeklyLimit: microE2E(10_000_000_000),
		MonthlyLimit: microE2E(100_000_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	pv := &postgres.PriceVersion{
		ModelID: modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: now,
	}
	if err := store.InsertPriceVersion(ctx, pv); err != nil {
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
	// /chat/models 读 DB 发布快照(生产真相);发布校验拒绝 loopback 上游是
	// 刻意的写路径行为,所以发布面用公网占位部署(永不拨号),网关运行面
	// 用静态快照指向 stub 上游。
	depPub := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "deepseek-v4-flash",
		BaseURL: "https://api.deepseek.example.com", Protocol: domain.ProtocolOpenAIChat,
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
		ProviderID: prov.ID, UpstreamModel: "deepseek-v4-flash", BaseURL: up.URL,
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
		prov.ID, "main", "api_key", "sk-upstream-e2e", "seed", nil)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	upAcct := &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cv.ID, Status: domain.AccountActive, ConcurrencyLimit: 8,
	}
	if err := store.InsertUpstreamAccount(ctx, upAcct); err != nil {
		t.Fatalf("upstream account: %v", err)
	}

	snap := &catalog.Snapshot{
		Models: map[string]domain.Model{modelID: {
			ID: modelID, DisplayName: "DeepSeek Chat", Lifecycle: domain.LifecycleActive,
			ContextTokens: 128000, MaxOutputTokens: 8192,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			SupportsTools: true, SupportsReasoning: true,
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
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}
	routingSvc := routing.NewService(store, adapters, nil)
	resolver := access.NewResolver(store, nil)
	gw := infgateway.NewService(staticSnapE2E{snap}, store, access.NewEntitlementResolver(store, nil),
		quota.NewService(store, nil), routingSvc, credSvc, providers.NewHTTPClient(egress), egress, nil)

	accessOps := &httpapi.AccessOps{
		KayaChat:       service.NewChatGatewayFacade(gw, resolver, catalogSvc, modelID),
		KayaChatModels: httpapi.NewKayaModelsHandler(catalogSvc, resolver, modelID),
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chatRouterSetup(setupCtx, engine, db, tokenSvc, authSvc,
		service.NewChatService(nil, repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewLLMUsageRepo(db)), accessOps)
	return engine, db, store
}

type staticSnapE2E struct{ snap *catalog.Snapshot }

func (s staticSnapE2E) Current(context.Context) (*catalog.Snapshot, error) { return s.snap, nil }

func microE2E(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

// grantKayaModelEntitlement gives the user's billing account an active gift
// entitlement over the facade's default model (kaya 赠送权益语义).
func grantKayaModelEntitlement(t *testing.T, db *sqlx.DB, store *postgres.Store, userID, modelID string) {
	t.Helper()
	ctx := context.Background()
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	var polID string
	if err := db.QueryRow(`SELECT id FROM inference_policy_versions WHERE name = 'kaya-gift'`).Scan(&polID); err != nil {
		t.Fatalf("policy: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{modelID},
		PolicyVersionID: polID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("entitlement: %v", err)
	}
}

// chatPost issues one POST /chat and returns the raw recorder.
func chatPost(t *testing.T, engine *gin.Engine, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(string(raw)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// Legacy mode (switch OFF — 旧行为直通)
// ---------------------------------------------------------------------------

// seedMembershipSub inserts an active kaya-membership subscription (the
// /chat 订阅闸门的数据面;/test/login 不落订阅行,只按请求套餐计算
// has_access 视图).
func seedMembershipSub(t *testing.T, db *sqlx.DB, userID string) {
	t.Helper()
	exp := time.Now().Add(30 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now(), $2, 'kaya-membership')
	`, userID, exp); err != nil {
		t.Fatalf("seed membership sub: %v", err)
	}
}

func TestChatLegacy_RelayAndErrorContract(t *testing.T) {
	up := newChatStubUpstream(t)
	engine := setupChatE2E(t, up)
	db := connectDB(t)
	t.Cleanup(func() { db.Close() })

	login := loginAndGetTokens(t, engine, "chatlegacy", "yundian")
	seedMembershipSub(t, db, login.User.ID)

	// 无 model 字段 → 服务端默认模型;tools + thinking 开关原样转发。
	w := chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type": "function", "function": map[string]any{"name": "run_shell"},
		}},
		"thinking_enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("legacy /chat = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("legacy relay missing content/[DONE]: %s", body)
	}
	select {
	case sent := <-up.lastBody:
		s := string(sent)
		for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`, `"run_shell"`, `"thinking":{"type":"enabled"}`} {
			if !strings.Contains(s, want) {
				t.Errorf("upstream payload missing %s: %s", want, s)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the request")
	}

	// 无活跃订阅 → 403 + 旧错误 envelope(message 逐字兼容)。
	noSub := loginAndGetTokens(t, engine, "chatnosub", "yundian")
	w = chatPost(t, engine, noSub.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-subscription /chat = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["code"] != float64(403) || env["message"] != "active subscription with access to this app is required" {
		t.Errorf("envelope = %v, want the legacy 403 shape/message", env)
	}

	// 无 JWT → 401 envelope。
	w = chatPost(t, engine, "", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-JWT /chat = %d, want 401", w.Code)
	}

	// 校验错误(空 messages)→ 400 envelope {code,data,message}。
	w = chatPost(t, engine, login.AccessToken, map[string]any{"messages": []any{}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty messages = %d, want 400", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["message"]; !ok {
		t.Errorf("legacy error envelope missing message: %s", w.Body.String())
	}
}

// Chat disabled (empty server key) → 404, mirroring the old convention.
func TestChatLegacy_DisabledIs404(t *testing.T) {
	engine, _, db := setupE2EServer(t) // chatSvc with empty key in the shared helper
	login := loginAndGetTokens(t, engine, "chatdisabled", "yundian")
	seedMembershipSub(t, db, login.User.ID)
	w := chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled chat = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Facade mode (switch ON — 网关承接)
// ---------------------------------------------------------------------------

func TestChatFacade_GatewayPathWithEntitlement(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up)

	login := loginAndGetTokens(t, engine, "chatfacade", "yundian")

	// 无权益 → 403(旧"无访问"语义,错误 shape 不变)。
	w := chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-entitlement facade /chat = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["code"] != float64(403) {
		t.Errorf("envelope = %v, want code 403", env)
	}

	// 发放赠送权益后:同一请求经网关成功,客户端看到同样的 SSE 形状。
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")

	w = chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type": "function", "function": map[string]any{"name": "run_shell"},
		}},
		"thinking_enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("facade /chat = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("facade relay = %s", body)
	}
	select {
	case sent := <-up.lastBody:
		s := string(sent)
		// 经网关:强制输出上限 + 显式流式 usage 请求 + 工具/思考转发。
		for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`, `"run_shell"`,
			`"thinking":{"type":"enabled"}`, `"max_tokens":8192`, `"stream_options":{"include_usage":true}`} {
			if !strings.Contains(s, want) {
				t.Errorf("gateway upstream payload missing %s: %s", want, s)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the request")
	}

	// 账本事实:kaya_chat 协议的逻辑请求已按 reported 结算(9×1 + 3×2 = 15)。
	var status, proto string
	var settled int64
	if err := db.QueryRow(`SELECT status, protocol, settled_micros FROM inference_requests
		ORDER BY created_at DESC LIMIT 1`).Scan(&status, &proto, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || proto != "kaya_chat" || settled != 15 {
		t.Errorf("request = %s/%s/%d, want settled/kaya_chat/15", status, proto, settled)
	}
}

func TestChatFacade_ModelsPickerContract(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up)
	login := loginAndGetTokens(t, engine, "chatmodels", "yundian")

	newReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		req.Header.Set("Authorization", "Bearer "+login.AccessToken)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// 无权益 → 空列表(不是错误)。
	w := newReq()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /chat/models = %d", w.Code)
	}
	var emptyEnv struct {
		Code int `json:"code"`
		Data struct {
			Models []any `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &emptyEnv); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if emptyEnv.Code != 0 || len(emptyEnv.Data.Models) != 0 {
		t.Errorf("models = %+v, want empty list", emptyEnv)
	}

	// 有权益 → 候选分支对齐形状 {id, display_name, provider, default}。
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")
	w = newReq()
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				Provider    string `json:"provider"`
				Default     bool   `json:"default"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Code != 0 || len(env.Data.Models) != 1 {
		t.Fatalf("models = %+v", env)
	}
	m := env.Data.Models[0]
	if m.ID != "deepseek-chat" || m.DisplayName != "DeepSeek Chat" || m.Provider != "deepseek" || !m.Default {
		t.Errorf("model entry = %+v", m)
	}
}

// Facade 模式下无 JWT 仍是 401 envelope(JWT 链不变)。
func TestChatFacade_StillRequiresJWT(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, _, _ := setupChatFacadeE2E(t, up)
	w := chatPost(t, engine, "", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("facade no-JWT = %d, want 401", w.Code)
	}
}
