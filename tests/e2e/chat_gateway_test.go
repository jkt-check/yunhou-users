package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
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
// Failure modes are marker-driven: a magic string in the user message content
// selects an upstream error response, so one stub drives both backends through
// their error mappings (legacy reads the HTTP status directly; the facade
// reaches the same bytes via gateway failover).
//
// 上游失败模式由消息内容里的魔数标记选择（单一 stub 同时驱动两种后端的
// 错误映射路径）。
type chatStubUpstream struct {
	*httptest.Server
	lastBody chan []byte
}

// 上行失败标记（出现在消息 content 中即触发）。
const (
	chatStubMarker429 = "STUB_429"     // 上游限流（HTTP 429）
	chatStubMarker500 = "STUB_500"     // 上游故障（HTTP 500）
	chatStubMarker400 = "STUB_400_CTX" // 上游拒绝：超长上下文（HTTP 400）
)

func newChatStubUpstream(t *testing.T) *chatStubUpstream {
	t.Helper()
	u := &chatStubUpstream{lastBody: make(chan []byte, 4)}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case u.lastBody <- body:
		default:
		}
		s := string(body)
		// 失败标记分支：真实上游的错误形状（OpenAI 风格 error 对象）。
		reject := func(status int, payload string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, payload)
		}
		switch {
		case strings.Contains(s, chatStubMarker429):
			reject(http.StatusTooManyRequests, `{"error":{"message":"rate limit reached","code":"rate_limit_exceeded"}}`)
			return
		case strings.Contains(s, chatStubMarker500):
			reject(http.StatusInternalServerError, `{"error":{"message":"internal upstream boom"}}`)
			return
		case strings.Contains(s, chatStubMarker400):
			reject(http.StatusBadRequest, `{"error":{"message":"This model's maximum context length is 8192 tokens","code":"context_length_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		var chunks []string
		// thinking 开启时先出 reasoning 增量（DeepSeek 真实顺序）；
		// 带 tools 时中间出 tool_calls 回合 —— 两路都逐字节透传，
		// 契约套件据此钉住 SSE 形状。
		if strings.Contains(s, `"thinking"`) {
			chunks = append(chunks,
				"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想\"}}]}\n\n")
		}
		chunks = append(chunks,
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n")
		if strings.Contains(s, "run_shell") {
			chunks = append(chunks,
				"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_e2e_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n")
		}
		chunks = append(chunks,
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: {\"id\":\"chatcmpl-e2e\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n",
			"data: [DONE]\n\n")
		for _, c := range chunks {
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
		AppEnv:              "e2e",
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
	chatSvc *service.ChatService, accessOps *httpapi.AccessOps, chatAccessLog *log.Logger) {
	router.Setup(ctx, engine, db,
		repo.NewAppRepo(db), repo.NewAuditLogRepo(db), repo.NewUserRepo(db), repo.NewSocialIdentityRepo(db),
		repo.NewPlanRepo(db), repo.NewSubscriptionRepo(db), repo.NewSessionRepo(db),
		tokenSvc, authSvc, nil,
		// planSvc 真实接线：无关端点存活探针（/apps/:id/plans，R7-N3 step 4）
		// 需要它；nil 会让该端点 panic。
		service.NewPlanService(repo.NewPlanRepo(db), repo.NewAppRepo(db), repo.NewPlanChangeLogRepo(db)), nil,
		&middleware.MultiChannelVerifier{}, nil,
		nil, nil, chatSvc, chatAccessLog, nil, nil, false, false, "e2e",
		service.NewUsageService(repo.NewUsageRepo(db)), nil, nil, accessOps, nil, nil, nil, nil,
		nil) // dashboardAppIDs — dashboard 运营面在本套件不触发
}

// setupChatE2E builds the engine in LEGACY mode: a real ChatService pointed
// at the stub upstream (迁移开关关闭 = 旧行为直通). accessLog 非空时审计
// 日志写入该 logger（R7-N4 契约矩阵要逐字段比对两种模式的审计行）。
func setupChatE2E(t *testing.T, up *chatStubUpstream, accessLog *log.Logger) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	db, tokenSvc, authSvc := chatE2EBase(t)
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	chatSvc := service.NewChatService(llm.LegacyCatalog("sk-legacy-test", up.URL, "deepseek-v4-flash"),
		repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewLLMUsageRepo(db))
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chatRouterSetup(setupCtx, engine, db, tokenSvc, authSvc, chatSvc, nil, accessLog)
	return engine, db
}

// setupChatFacadeE2E builds the facade-mode engine: /chat runs through the
// inference gateway with a static catalog snapshot pointing at the stub
// upstream (迁移开关开启的等价接线). accessLog 同 setupChatE2E。
func setupChatFacadeE2E(t *testing.T, up *chatStubUpstream, accessLog *log.Logger) (*gin.Engine, *sqlx.DB, *postgres.Store) {
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
		KayaChat:       service.NewChatGatewayFacade(gw, resolver, catalogSvc, staticSnapE2E{snap}, modelID),
		KayaChatModels: httpapi.NewKayaModelsHandler(catalogSvc, resolver, staticSnapE2E{snap}, modelID),
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chatRouterSetup(setupCtx, engine, db, tokenSvc, authSvc,
		service.NewChatService(nil, repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewLLMUsageRepo(db)), accessOps, accessLog)
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
	engine, db := setupChatE2E(t, up, nil)

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
// R7-N4：404 是 legacy 独有状态——facade 的等价退化是目录冷启动 503 +
// Retry-After（TestChatFacade_EmptyCatalogDegradesOnlyChatSurface 钉住），
// 属 known-intended 差异（开关打开即代表 chat 启用，未就绪 ≠ 未启用）。
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
	const wantBody = `{"code":404,"data":null,"message":"chat is not enabled"}`
	if w.Body.String() != wantBody {
		t.Errorf("disabled-chat envelope = %s, want byte-exact %s", w.Body.String(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// Facade mode (switch ON — 网关承接)
// ---------------------------------------------------------------------------

func TestChatFacade_GatewayPathWithEntitlement(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up, nil)

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
	engine, db, store := setupChatFacadeE2E(t, up, nil)
	login := loginAndGetTokens(t, engine, "chatmodels", "yundian")

	newReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		req.Header.Set("Authorization", "Bearer "+login.AccessToken)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// N4a：无计费账户（从未购买/获赠）→ 403，envelope 与 legacy
	// ErrChatNoAccess 逐字节一致（不再是 200 空列表）。
	w := newReq()
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /chat/models = %d, want 403: %s", w.Code, w.Body.String())
	}
	const wantNoAcctBody = `{"code":403,"data":null,"message":"active subscription with access to this app is required"}`
	if w.Body.String() != wantNoAcctBody {
		t.Errorf("no-account envelope = %s, want byte-exact legacy shape %s", w.Body.String(), wantNoAcctBody)
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

// N4b 契约用例：facade 模式下带未知 model 字段 → 400 + legacy envelope
// "unknown chat model"（picker 回退默认语义），与无权益/无账户的 403 严格
// 分层。用户需先持有权益（有 billing account），否则 resolver 分支先以
// 403 拦截。
func TestChatFacade_UnknownModelIs400(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up, nil)
	login := loginAndGetTokens(t, engine, "chatunknownmodel", "yundian")
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")

	w := chatPost(t, engine, login.AccessToken, map[string]any{
		"model":    "no-such-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown-model facade /chat = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["code"] != float64(400) || env["message"] != "unknown chat model" {
		t.Errorf("envelope = %v, want the legacy 400 unknown-model shape/message", env)
	}
}

// Facade 模式下无 JWT 仍是 401 envelope(JWT 链不变)。
func TestChatFacade_StillRequiresJWT(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, _, _ := setupChatFacadeE2E(t, up, nil)
	w := chatPost(t, engine, "", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("facade no-JWT = %d, want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// R7-N1/N2 行为级（Task 5 step 4）：空目录冷启动下 facade 进程存活，只有
// /chat 面退化 503 + Retry-After；健康检查与无关端点照常。
// ---------------------------------------------------------------------------

// setupChatFacadeEmptyCatalogE2E builds the facade-mode engine WITHOUT any
// published catalog revision: facade 与 /chat/models 共享真实 SnapshotCache
// （直连真实 store），两者都看到真实的冷启动哨兵形态。gateway 按生产形态
// 接线但永不触达（就绪闸门在它之前短路）。
func setupChatFacadeEmptyCatalogE2E(t *testing.T) *gin.Engine {
	t.Helper()
	db, tokenSvc, authSvc := chatE2EBase(t)
	store := postgres.NewStore(db)
	catalogSvc := catalog.NewService(store)
	// 真实冷启动：本用例不发布任何 revision → cache.Current 携带
	// ErrNoVerifiedSnapshot。
	cache := catalog.NewSnapshotCache(store, func(error) {})
	resolver := access.NewResolver(store, nil)
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}
	gw := infgateway.NewService(cache, store, access.NewEntitlementResolver(store, nil),
		quota.NewService(store, nil), routing.NewService(store, adapters, nil),
		nil, nil, nil, nil)

	accessOps := &httpapi.AccessOps{
		KayaChat:       service.NewChatGatewayFacade(gw, resolver, catalogSvc, cache, "deepseek-chat"),
		KayaChatModels: httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "deepseek-chat"),
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chatRouterSetup(setupCtx, engine, db, tokenSvc, authSvc,
		service.NewChatService(nil, repo.NewSubscriptionRepo(db), repo.NewPlanRepo(db), repo.NewLLMUsageRepo(db)), accessOps, nil)
	return engine
}

func TestChatFacade_EmptyCatalogDegradesOnlyChatSurface(t *testing.T) {
	engine := setupChatFacadeEmptyCatalogE2E(t)
	login := loginAndGetTokens(t, engine, "chatemptycatalog", "yundian")

	// 进程存活：健康检查不受目录冷启动影响。
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200（进程存活）", w.Code)
	}
	// 无关端点照常：套餐列表与 inference 目录无涉。
	req = httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /apps/yundian/plans = %d, want 200（无关端点照常）: %s", w.Code, w.Body.String())
	}

	// POST /chat → 503 + Retry-After（N1：facade 就绪闸门 → ErrChatNotReady
	// → handler 底座映射 503）。
	w = chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold-start facade /chat = %d, want 503: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("POST /chat Retry-After = %q, want %q", got, "5")
	}

	// GET /chat/models → 503 + Retry-After + 冷启动 envelope（N2）。
	req = httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	req.Header.Set("Authorization", "Bearer "+login.AccessToken)
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold-start GET /chat/models = %d, want 503: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("GET /chat/models Retry-After = %q, want %q", got, "5")
	}
	if !strings.Contains(w.Body.String(), `"message":"chat catalog is not ready"`) {
		t.Errorf("GET /chat/models envelope = %s, want the cold-start message", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// R7-N4 双后端契约矩阵（PR2 Task 1 step 2）
//
// 同一组 /chat 用例在 legacy（DeepSeek 直通）与 facade（inference 网关）
// 两种接线各跑一遍，逐字段/逐字节断言对外契约一致——kaya 客户端零改动
// 保证的机器钉。合法差异（facade 上行强制 max_tokens、facade RecordUsage
// 为 no-op 不写 llm_usage_events、facade 无 404 只有冷启动 503）按模式
// 断言正确行为，并在 PR 报告里列为 known-intended difference。
//
// The same /chat cases run once per backend wiring and assert the
// client-visible contract byte-for-byte; legitimate differences are asserted
// per-mode and listed in the PR2 report as known-intended.
// ---------------------------------------------------------------------------

// lockedBuffer is a goroutine-safe sink for the chat access log (审计日志在
// handler 内同步写入；-race 下读取侧与写入侧仍需互斥)。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Lines returns the complete log lines written so far (one JSON entry each).
func (b *lockedBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// chatContractEnv is one backend wiring of the same /chat surface: engine +
// DB + audit-log sink + the mode-specific "grant access" primitive
// (legacy：活跃 kaya-membership 订阅；facade：默认模型的赠送权益)。
type chatContractEnv struct {
	engine *gin.Engine
	db     *sqlx.DB
	store  *postgres.Store // facade only（legacy 为 nil）
	log    *lockedBuffer
	// grantAccess gives userID whatever the mode needs to pass the /chat
	// access gate (legacy: active subscription; facade: gift entitlement).
	grantAccess func(t *testing.T, userID string)
	// defaultModelID is the logical model id the client-visible surface
	// shows for the default (GET /chat/models entry + audit-log model field).
	defaultModelID string
	defaultDisplay string
	// wantMaxTokens pins the upstream-payload difference: facade forces the
	// output cap (设计 §7.2 不允许无限输出), legacy sends no max_tokens.
	// Empty = the key must NOT appear.
	wantMaxTokens string
}

func setupChatContractLegacy(t *testing.T, up *chatStubUpstream) *chatContractEnv {
	t.Helper()
	logBuf := &lockedBuffer{}
	engine, db := setupChatE2E(t, up, log.New(logBuf, "", 0))
	return &chatContractEnv{
		engine: engine, db: db, log: logBuf,
		grantAccess: func(t *testing.T, userID string) {
			seedMembershipSub(t, db, userID)
		},
		defaultModelID: "deepseek-v4-flash", // legacy 目录 id 即上游模型名
		defaultDisplay: "deepseek-v4-flash",
		wantMaxTokens:  "",
	}
}

func setupChatContractFacade(t *testing.T, up *chatStubUpstream) *chatContractEnv {
	t.Helper()
	logBuf := &lockedBuffer{}
	engine, db, store := setupChatFacadeE2E(t, up, log.New(logBuf, "", 0))
	return &chatContractEnv{
		engine: engine, db: db, store: store, log: logBuf,
		grantAccess: func(t *testing.T, userID string) {
			grantKayaModelEntitlement(t, db, store, userID, "deepseek-chat")
		},
		defaultModelID: "deepseek-chat", // facade 公共模型 id（KAYA_CHAT_MODEL 语义）
		defaultDisplay: "DeepSeek Chat",
		wantMaxTokens:  `"max_tokens":8192`,
	}
}

// sseDataPayloads splits an SSE body into its ordered data payloads (the
// terminal [DONE] included verbatim). Anything not shaped `data: <payload>`
// fails the test — the contract is that both backends emit clean SSE.
func sseDataPayloads(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE line in stream: %q (full body: %q)", line, body)
		}
		out = append(out, strings.TrimPrefix(line, "data: "))
	}
	return out
}

// chatStreamChunk is the contract view of one OpenAI chat.completion.chunk:
// delta content / reasoning / tool_calls, finish_reason and the trailing
// usage object.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// auditEntry parses one chat access-log line into a generic map (字段集合
// 比对用 map，值断言按需取).
func auditEntry(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("audit line is not JSON: %q: %v", line, err)
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertUpstreamPayload drains the stub's recorded request and asserts the
// relayed body — the upstream-facing half of the contract (默认模型语义、
// stream_options.include_usage、tools/thinking 透传、以及 facade 独有的
// 强制 max_tokens 差异钉).
func assertUpstreamPayload(t *testing.T, env *chatContractEnv, up *chatStubUpstream) {
	t.Helper()
	select {
	case sent := <-up.lastBody:
		s := string(sent)
		// 不带 model 字段 → 服务端默认模型落上游（KAYA_CHAT_MODEL 语义；
		// 两种模式的上游模型名同为 deepseek-v4-flash）。
		for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`,
			`"stream_options":{"include_usage":true}`} {
			if !strings.Contains(s, want) {
				t.Errorf("upstream payload missing %s: %s", want, s)
			}
		}
		// session_id 是纯审计字段，两模式都不得上行。
		if strings.Contains(s, "session_id") {
			t.Errorf("upstream payload leaks session_id: %s", s)
		}
		if env.wantMaxTokens == "" {
			if strings.Contains(s, `"max_tokens"`) {
				t.Errorf("legacy upstream payload must not carry max_tokens: %s", s)
			}
		} else if !strings.Contains(s, env.wantMaxTokens) {
			t.Errorf("facade upstream payload missing forced cap %s: %s", env.wantMaxTokens, s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the request")
	}
}

// caseChatContractSuccess is the SSE-shape + audit + default-model case.
// It returns the raw SSE body so the wrapper can compare the two backends
// byte-for-byte (同一 stub 流的客户端视图必须完全一致).
func caseChatContractSuccess(t *testing.T, env *chatContractEnv, up *chatStubUpstream) string {
	login := loginAndGetTokens(t, env.engine, "contract-ok", "yundian")
	env.grantAccess(t, login.User.ID)

	before := len(env.log.Lines())
	w := chatPost(t, env.engine, login.AccessToken, map[string]any{
		// 不带 model 字段：钉默认模型语义（KAYA_CHAT_MODEL / 旧默认）。
		"session_id": "sess-contract",
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type": "function", "function": map[string]any{"name": "run_shell"},
		}},
		"thinking_enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("/chat = %d %s", w.Code, w.Body.String())
	}
	// SSE 响应头（两模式共用同一 handler 写头）。
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	// SSE 形状：reasoning 增量 → content 增量 → tool_calls 回合 → finish
	// stop → 末尾 usage chunk（choices:[]，由上游 stream_options.include_usage
	// 产生）→ 终止 [DONE]。
	body := w.Body.String()
	events := sseDataPayloads(t, body)
	if len(events) != 6 {
		t.Fatalf("SSE events = %d, want 6 (reasoning/content/tool_calls/finish/usage/[DONE]): %q", len(events), body)
	}
	parse := func(i int) chatStreamChunk {
		var c chatStreamChunk
		if err := json.Unmarshal([]byte(events[i]), &c); err != nil {
			t.Fatalf("event %d not a chunk: %q: %v", i, events[i], err)
		}
		return c
	}
	if got := parse(0).Choices[0].Delta.ReasoningContent; got != "想" {
		t.Errorf("reasoning delta = %q, want %q（thinking_enabled=true 必须先出 reasoning 增量）", got, "想")
	}
	if c := parse(1); c.Choices[0].Delta.Role != "assistant" || c.Choices[0].Delta.Content != "你" {
		t.Errorf("first content delta = %+v", c.Choices[0].Delta)
	}
	tc := parse(2).Choices[0].Delta.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_e2e_1" || tc[0].Type != "function" ||
		tc[0].Function.Name != "run_shell" || tc[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("tool_calls delta = %+v, want the run_shell turn shape", tc)
	}
	if c := parse(3); c.Choices[0].Delta.Content != "好" || c.Choices[0].FinishReason == nil ||
		*c.Choices[0].FinishReason != "stop" {
		t.Errorf("finish chunk = %+v", c.Choices[0])
	}
	usageChunk := parse(4)
	if len(usageChunk.Choices) != 0 {
		t.Errorf("usage chunk choices = %+v, want []（末尾 usage chunk 不带 choices）", usageChunk.Choices)
	}
	if usageChunk.Usage == nil || usageChunk.Usage.PromptTokens != 9 ||
		usageChunk.Usage.CompletionTokens != 3 || usageChunk.Usage.TotalTokens != 12 {
		t.Errorf("usage chunk = %+v, want {9,3,12}", usageChunk.Usage)
	}
	if events[5] != "[DONE]" {
		t.Errorf("terminal event = %q, want [DONE]", events[5])
	}

	assertUpstreamPayload(t, env, up)

	// 审计日志：恰好一行，字段集合与值逐字段钉住（两模式同一 handler，
	// 这里按同一期望值断言 ⇒ 字段集合一致由构造保证并显式核对）。
	lines := env.log.Lines()
	if len(lines)-before != 1 {
		t.Fatalf("audit lines added = %d, want exactly 1: %q", len(lines)-before, lines[before:])
	}
	entry := auditEntry(t, lines[len(lines)-1])
	wantKeys := []string{"app_id", "duration_ms", "input", "input_bytes", "message_count",
		"model", "output", "output_bytes", "session_id", "status", "thinking_enabled",
		"tools_count", "ts", "user_id"}
	if got := sortedKeys(entry); !equalStrings(got, wantKeys) {
		t.Errorf("audit key set = %v, want %v", got, wantKeys)
	}
	checks := map[string]any{
		"status":           "ok",
		"user_id":          login.User.ID,
		"app_id":           "yundian",
		"session_id":       "sess-contract",
		"model":            env.defaultModelID,
		"message_count":    float64(1),
		"tools_count":      float64(1),
		"thinking_enabled": true,
		"output":           "你好",
		"input_bytes":      float64(2), // len("hi")
		"output_bytes":     float64(6), // len("你好")，CJK 3 字节/字
	}
	for k, want := range checks {
		if entry[k] != want {
			t.Errorf("audit %s = %v, want %v", k, entry[k], want)
		}
	}
	input, ok := entry["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("audit input = %v", entry["input"])
	}
	first := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Errorf("audit input[0] = %v", first)
	}
	return body
}

// caseChatContractErrorMatrix pins the {code,data,message} envelope +
// status matrix byte-exact in both modes for the pre-upstream failures.
// 401 在 JWT 中间件层拦截，不进 handler → 审计零行；其余每请求恰好一行
// 审计。上游故障行（429/500/4xx 拒绝）在 caseChatContractUpstreamErrors
// 用隔离环境跑（见那里的冷却说明）。
func caseChatContractErrorMatrix(t *testing.T, env *chatContractEnv) {
	cases := []struct {
		name       string
		token      string // "" = 不带 Authorization 头
		badToken   bool
		grant      bool
		body       map[string]any
		wantStatus int
		wantBody   string // 逐字节 envelope
		wantErr    string // 审计行 error 字段（空 = 无审计行）
	}{
		{
			name:       "no_token",
			token:      "",
			body:       map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"code":401,"message":"missing or invalid authorization header"}`,
		},
		{
			name:       "bad_token",
			token:      "not-a-jwt",
			badToken:   true,
			body:       map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"code":401,"message":"invalid or expired token"}`,
		},
		{
			name:       "no_access",
			token:      "JWT",
			body:       map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}},
			wantStatus: http.StatusForbidden,
			wantBody:   `{"code":403,"data":null,"message":"active subscription with access to this app is required"}`,
			wantErr:    "active subscription with access to this app is required",
		},
		{
			name:  "unknown_model",
			token: "JWT", grant: true,
			body:       map[string]any{"model": "no-such-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}},
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"code":400,"data":null,"message":"unknown chat model"}`,
			wantErr:    "unknown chat model",
		},
		{
			name:  "empty_messages",
			token: "JWT", grant: true,
			body:       map[string]any{"messages": []any{}},
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"code":400,"data":null,"message":"messages is required"}`,
			wantErr:    "messages is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			login := loginAndGetTokens(t, env.engine, "contract-err-"+c.name, "yundian")
			if c.grant {
				env.grantAccess(t, login.User.ID)
			}
			token := ""
			switch {
			case c.badToken:
				token = c.token
			case c.token == "JWT":
				token = login.AccessToken
			}
			before := len(env.log.Lines())
			w := chatPost(t, env.engine, token, c.body)
			if w.Code != c.wantStatus {
				t.Fatalf("/chat = %d, want %d (%s)", w.Code, c.wantStatus, w.Body.String())
			}
			if w.Body.String() != c.wantBody {
				t.Errorf("envelope = %s, want byte-exact %s", w.Body.String(), c.wantBody)
			}
			lines := env.log.Lines()
			wantLines := 1
			if c.wantErr == "" {
				wantLines = 0 // 401 在 JWT 层拦截，不进 handler → 无审计行
			}
			if len(lines)-before != wantLines {
				t.Fatalf("audit lines added = %d, want %d: %q", len(lines)-before, wantLines, lines[before:])
			}
			if wantLines == 1 {
				entry := auditEntry(t, lines[len(lines)-1])
				if entry["status"] != "error" || entry["error"] != c.wantErr {
					t.Errorf("audit line status/error = %v/%v, want error/%q", entry["status"], entry["error"], c.wantErr)
				}
				if entry["user_id"] != login.User.ID {
					t.Errorf("audit user_id = %v, want %s", entry["user_id"], login.User.ID)
				}
			}
		})
	}
}

// caseChatContractUpstreamErrors pins the upstream-failure rows of the
// status matrix (429/500/结构化 4xx 拒绝). 每个用例用全新的接线 + stub：
// facade 的可重试失败会给唯一上游账号记 60s 进程内冷却
// （routing.cooldownAfterRetryable），共享环境会污染后续用例；隔离环境
// 也让「恰好一行审计 + 上行确已触达」的断言无交叉。
func caseChatContractUpstreamErrors(t *testing.T, setup func(t *testing.T, up *chatStubUpstream) *chatContractEnv) {
	cases := []struct {
		name       string
		marker     string
		wantStatus int
		wantBody   string
		wantErr    string
		wantRej    bool // 审计行携带 upstream_status/upstream_code（结构化拒绝）
	}{
		{
			// 上游 429：legacy 直连读出 429；facade 候选全部限流、failover
			// 耗尽后由 R7-N4 修复映射回同一 envelope。
			name:       "upstream_rate_limited",
			marker:     chatStubMarker429,
			wantStatus: http.StatusTooManyRequests,
			wantBody:   `{"code":429,"data":null,"message":"chat upstream rate limit exceeded"}`,
			wantErr:    "chat upstream rate limit exceeded",
		},
		{
			name:       "upstream_500",
			marker:     chatStubMarker500,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"code":502,"data":null,"message":"chat upstream error"}`,
			wantErr:    "chat upstream error",
		},
		{
			// 上游 4xx 拒绝：legacy 分类为 ChatUpstreamRejection（502 +
			// data{upstream_status,upstream_code,upstream_message}）；facade
			// 经 R7-N4 修复复用同一 classifyUpstreamRejection → 逐字节一致。
			name:       "upstream_rejected_400",
			marker:     chatStubMarker400,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"code":502,"data":{"upstream_code":"context_length_exceeded","upstream_message":"This model's maximum context length is 8192 tokens","upstream_status":400},"message":"chat request rejected by upstream"}`,
			wantErr:    "chat request rejected by upstream",
			wantRej:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newChatStubUpstream(t)
			env := setup(t, up)
			login := loginAndGetTokens(t, env.engine, "contract-err-"+c.name, "yundian")
			env.grantAccess(t, login.User.ID)
			w := chatPost(t, env.engine, login.AccessToken, map[string]any{
				"messages": []map[string]any{{"role": "user", "content": c.marker}},
			})
			if w.Code != c.wantStatus {
				t.Fatalf("/chat = %d, want %d (%s)", w.Code, c.wantStatus, w.Body.String())
			}
			if w.Body.String() != c.wantBody {
				t.Errorf("envelope = %s, want byte-exact %s", w.Body.String(), c.wantBody)
			}
			// 上行确已触达（错误来自上游，不是本地闸门）。
			select {
			case <-up.lastBody:
			case <-time.After(2 * time.Second):
				t.Error("upstream never saw the request")
			}
			lines := env.log.Lines()
			if len(lines) != 1 {
				t.Fatalf("audit lines = %d, want exactly 1: %q", len(lines), lines)
			}
			entry := auditEntry(t, lines[0])
			if entry["status"] != "error" || entry["error"] != c.wantErr {
				t.Errorf("audit line status/error = %v/%v, want error/%q", entry["status"], entry["error"], c.wantErr)
			}
			if c.wantRej {
				// 结构化拒绝同样进审计行（两模式一致）。
				if entry["upstream_status"] != float64(400) || entry["upstream_code"] != "context_length_exceeded" {
					t.Errorf("audit upstream_* = %v/%v, want 400/context_length_exceeded",
						entry["upstream_status"], entry["upstream_code"])
				}
			}
		})
	}
}

// caseChatContractToolTurn pins the tools passthrough + tool_calls turn
// shape: 带 tool_calls 的 assistant 回合与 tool 结果回合在两模式下都逐
// 字段透传上游；被拒请求的审计行也保留 tool_calls（truncateChatInput 只
// 截 content，不动回合形状）。
func caseChatContractToolTurn(t *testing.T, env *chatContractEnv, up *chatStubUpstream) {
	toolHistory := []map[string]any{
		{"role": "user", "content": "list files"},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "run_shell", "arguments": `{"cmd":"ls"}`},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	}

	// 成功路径：完整工具回合历史透传上游。
	login := loginAndGetTokens(t, env.engine, "contract-tools", "yundian")
	env.grantAccess(t, login.User.ID)
	w := chatPost(t, env.engine, login.AccessToken, map[string]any{"messages": toolHistory})
	if w.Code != http.StatusOK {
		t.Fatalf("tool-turn /chat = %d %s", w.Code, w.Body.String())
	}
	select {
	case sent := <-up.lastBody:
		s := string(sent)
		for _, want := range []string{`"tool_calls"`, `"id":"call_1"`, `"tool_call_id":"call_1"`, `"run_shell"`} {
			if !strings.Contains(s, want) {
				t.Errorf("upstream payload missing %s: %s", want, s)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the tool-turn request")
	}

	// 拒绝路径（无访问前提）：审计错误行保留 tool_calls 回合形状。
	noAccess := loginAndGetTokens(t, env.engine, "contract-tools-denied", "yundian")
	before := len(env.log.Lines())
	w = chatPost(t, env.engine, noAccess.AccessToken, map[string]any{"messages": toolHistory})
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied tool-turn /chat = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	lines := env.log.Lines()
	if len(lines)-before != 1 {
		t.Fatalf("audit lines added = %d, want 1", len(lines)-before)
	}
	entry := auditEntry(t, lines[len(lines)-1])
	input, ok := entry["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("audit input = %v, want the 3-turn tool history", entry["input"])
	}
	assistant := input[1].(map[string]any)
	tcs, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("audit error line lost tool_calls: %v", assistant)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "run_shell" {
		t.Errorf("audit tool_calls function.name = %v", fn["name"])
	}
	if input[2].(map[string]any)["tool_call_id"] != "call_1" {
		t.Errorf("audit tool turn lost tool_call_id: %v", input[2])
	}
}

// caseChatContractModels pins GET /chat/models: 无访问前提 → 403 逐字节一
// 致；有访问前提 → {code:0,data:{models:[{id,display_name,provider,default}]}}
// 且恰好默认模型带 default:true（客户端预选择语义）。
func caseChatContractModels(t *testing.T, env *chatContractEnv) {
	login := loginAndGetTokens(t, env.engine, "contract-models", "yundian")
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		req.Header.Set("Authorization", "Bearer "+login.AccessToken)
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		return w
	}

	w := get()
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /chat/models (no access) = %d, want 403: %s", w.Code, w.Body.String())
	}
	const want403 = `{"code":403,"data":null,"message":"active subscription with access to this app is required"}`
	if w.Body.String() != want403 {
		t.Errorf("no-access /chat/models envelope = %s, want byte-exact %s", w.Body.String(), want403)
	}

	env.grantAccess(t, login.User.ID)
	w = get()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /chat/models = %d: %s", w.Code, w.Body.String())
	}
	want := fmt.Sprintf(`{"code":0,"data":{"models":[{"id":%q,"display_name":%q,"provider":"deepseek","default":true}]}}`,
		env.defaultModelID, env.defaultDisplay)
	if w.Body.String() != want {
		t.Errorf("/chat/models body = %s, want byte-exact %s", w.Body.String(), want)
	}
}

// caseChatContractMetering pins the per-mode metering facts:
//   - legacy：每次完成的上游调用落一行 llm_usage_events（tokens 来自流末
//     usage chunk）；
//   - facade：RecordUsage 是 no-op，llm_usage_events 零新增（spec §7.11
//     不双写），计量事实在 inference_requests 结算行（known-intended 差异）。
//
// 顺带钉住纯聊天（无 tools/thinking）审计行的 omitempty 行为。
func caseChatContractMetering(t *testing.T, env *chatContractEnv, up *chatStubUpstream) {
	login := loginAndGetTokens(t, env.engine, "contract-meter", "yundian")
	env.grantAccess(t, login.User.ID)
	before := len(env.log.Lines())
	w := chatPost(t, env.engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("/chat = %d %s", w.Code, w.Body.String())
	}
	// 排空 stub 记录的上行 payload（本用例不断言它，避免 channel 堆积干扰
	// 其它用例 —— lastBody 容量 4，但排空更稳）。
	select {
	case <-up.lastBody:
	default:
	}

	lines := env.log.Lines()
	if len(lines)-before != 1 {
		t.Fatalf("audit lines added = %d, want 1", len(lines)-before)
	}
	entry := auditEntry(t, lines[len(lines)-1])
	if _, ok := entry["tools_count"]; ok {
		t.Errorf("plain chat audit line must omit tools_count: %v", sortedKeys(entry))
	}
	if _, ok := entry["thinking_enabled"]; ok {
		t.Errorf("plain chat audit line must omit thinking_enabled: %v", sortedKeys(entry))
	}
	if entry["model"] != env.defaultModelID || entry["status"] != "ok" {
		t.Errorf("audit model/status = %v/%v", entry["model"], entry["status"])
	}

	if env.store == nil { // legacy
		var model, provider, upstreamModel, status string
		var inTok, outTok int
		err := env.db.QueryRow(`SELECT model, provider, upstream_model, status, input_tokens, output_tokens
			FROM llm_usage_events WHERE user_id = $1`, login.User.ID).
			Scan(&model, &provider, &upstreamModel, &status, &inTok, &outTok)
		if err != nil {
			t.Fatalf("legacy llm_usage_events row: %v（每次完成的上游调用必须落一行）", err)
		}
		if model != "deepseek-v4-flash" || provider != "deepseek" || upstreamModel != "deepseek-v4-flash" ||
			status != "ok" || inTok != 9 || outTok != 3 {
			t.Errorf("usage event = %s/%s/%s/%s/%d/%d, want deepseek-v4-flash/deepseek/deepseek-v4-flash/ok/9/3",
				model, provider, upstreamModel, status, inTok, outTok)
		}
		return
	}
	// facade：llm_usage_events 零新增（不双写），网关结算行存在且按
	// reported 用量结算（9×1 + 3×2 = 15 µ¢，同既有套件口径）。
	var n int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM llm_usage_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("facade llm_usage_events = %d rows, want 0（RecordUsage no-op，计费归网关结算）", n)
	}
	var reqStatus, proto string
	var settled int64
	if err := env.db.QueryRow(`SELECT status, protocol, settled_micros FROM inference_requests
		ORDER BY created_at DESC LIMIT 1`).Scan(&reqStatus, &proto, &settled); err != nil {
		t.Fatalf("facade inference_requests: %v", err)
	}
	if reqStatus != "settled" || proto != "kaya_chat" || settled != 15 {
		t.Errorf("facade request = %s/%s/%d, want settled/kaya_chat/15", reqStatus, proto, settled)
	}
}

// TestChatContract_DualBackend runs the full R7-N4 contract matrix once per
// backend wiring, then compares the success SSE stream byte-for-byte across
// backends (kaya 零改动保证的最强钉法：同一 stub 流的客户端视图完全一致).
func TestChatContract_DualBackend(t *testing.T) {
	modes := []struct {
		name  string
		setup func(t *testing.T, up *chatStubUpstream) *chatContractEnv
	}{
		{"legacy", setupChatContractLegacy},
		{"facade", setupChatContractFacade},
	}
	bodies := map[string]string{}
	for _, mode := range modes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			up := newChatStubUpstream(t)
			env := mode.setup(t, up)
			t.Run("success_stream", func(t *testing.T) {
				bodies[mode.name] = caseChatContractSuccess(t, env, up)
			})
			t.Run("error_matrix", func(t *testing.T) { caseChatContractErrorMatrix(t, env) })
			t.Run("tool_turn", func(t *testing.T) { caseChatContractToolTurn(t, env, up) })
			t.Run("models", func(t *testing.T) { caseChatContractModels(t, env) })
			t.Run("metering", func(t *testing.T) { caseChatContractMetering(t, env, up) })
			// 上游故障行放最后且用隔离环境（facade 冷却会污染共享环境）。
			t.Run("upstream_errors", func(t *testing.T) {
				caseChatContractUpstreamErrors(t, mode.setup)
			})
		})
	}
	if bodies["legacy"] != "" && bodies["facade"] != "" && bodies["legacy"] != bodies["facade"] {
		t.Errorf("SSE stream differs across backends:\nlegacy: %q\nfacade: %q", bodies["legacy"], bodies["facade"])
	}
}

// grantTinyQuotaEntitlement issues an entitlement whose windows are 1 µ¢ —
// any admission estimate exceeds it, so /chat fails with quota_exceeded
// BEFORE any upstream call (facade 原生 429 来源；legacy 无配额概念，其
// 429 仅来自上游限流——同一 client envelope，不同触发源，见报告差异清单).
func grantTinyQuotaEntitlement(t *testing.T, db *sqlx.DB, store *postgres.Store, userID, modelID string) {
	t.Helper()
	ctx := context.Background()
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-tiny-" + uuid.NewString(), Revision: 1, ModelIDs: []string{modelID},
		FiveHourLimit: microE2E(1), WeeklyLimit: microE2E(1), MonthlyLimit: microE2E(1),
		Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("tiny policy: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "tiny-" + uuid.NewString(), ModelIDs: []string{modelID},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("tiny entitlement: %v", err)
	}
}

// TestChatContract_FacadeQuotaExceededIs429 pins the facade-native 429
// source: 配额耗尽 → ErrChatRateLimited → 与 legacy 上游限流完全相同的
// client envelope，且预占失败发生在任何上游调用之前（stub 不应被触达）。
func TestChatContract_FacadeQuotaExceededIs429(t *testing.T) {
	up := newChatStubUpstream(t)
	env := setupChatContractFacade(t, up)
	login := loginAndGetTokens(t, env.engine, "contract-quota", "yundian")
	grantTinyQuotaEntitlement(t, env.db, env.store, login.User.ID, "deepseek-chat")

	w := chatPost(t, env.engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-exceeded facade /chat = %d, want 429 (%s)", w.Code, w.Body.String())
	}
	const wantBody = `{"code":429,"data":null,"message":"chat upstream rate limit exceeded"}`
	if w.Body.String() != wantBody {
		t.Errorf("quota envelope = %s, want byte-exact legacy 429 shape %s", w.Body.String(), wantBody)
	}
	select {
	case sent := <-up.lastBody:
		t.Errorf("upstream was hit despite quota exhaustion: %s", sent)
	default:
	}
}
