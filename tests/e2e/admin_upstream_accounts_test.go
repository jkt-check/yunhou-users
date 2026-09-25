package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	infgateway "github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

// admin_upstream_accounts_test.go — 「可调度账号」管理端点的端到端验收
// （需求文档 §5.3/§5.8）：静态 key 路径下凭据创建后不建账号流量跑不起来；
// 经 POST /admin/upstream-accounts 绑定后立即可调度（账号是运行时调度状
// 态，无需 publish）；凭据吊销级联停用账号、凭据恢复后账号不自动恢复，
// 经 POST /admin/upstream-accounts/:id/status 恢复后流量恢复。

// adminAccountStubAuth mirrors the opsFixture stub: identity legs come from
// test headers, OperatorAuthz does the real role/permission check.
func adminAccountStubAuth(c *gin.Context) {
	if u := c.GetHeader("X-Test-User"); u != "" {
		c.Set(middleware.ContextUserID, u)
	}
	if a := c.GetHeader("X-Test-App"); a != "" {
		c.Set(middleware.ContextApp, &model.App{AppID: a})
		c.Set(middleware.ContextAppID, a)
	}
	c.Next()
}

type accountE2EEnv struct {
	engine   *gin.Engine
	db       *sqlx.DB
	store    *postgres.Store
	provID   string
	credID   string
	opHeader map[string]string
}

// setupAccountE2E builds the facade-mode gateway stack (同
// setupChatFacadeE2E）但刻意不走最后一步：凭据已建、可调度账号未建 —— 即
// 静态 key 路径的真实缺口。admin 面（凭据 + 账号端点）挂在同一引擎上。
func setupAccountE2E(t *testing.T, up *chatStubUpstream) *accountE2EEnv {
	t.Helper()
	db, tokenSvc, authSvc := chatE2EBase(t)
	ctx := context.Background()
	store := postgres.NewStore(db)

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
	pv := &postgres.PriceVersion{
		ModelID: modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: time.Now().Add(-time.Minute),
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
	// 发布面用公网占位部署（loopback 过不了发布校验，刻意行为）；运行面
	// 静态快照指向 stub 上游。
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
	// 静态 key 路径：凭据建好，账号未建（新 provider 接入的最后一步缺失态）。
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "e2e"},
		prov.ID, "main", "api_key", "sk-upstream-e2e", "seed", nil)
	if err != nil {
		t.Fatalf("credential: %v", err)
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

	// admin 面：凭据 + 可调度账号端点（credentials:manage；身份双腿打桩，
	// 权限检查走真实 OperatorAuthz）。
	opUser := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO users (id) VALUES ($1) ON CONFLICT DO NOTHING`, opUser); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantRole(ctx, opUser, management.RoleOperator, nil, "test"); err != nil {
		t.Fatal(err)
	}
	adminG := engine.Group("/admin", adminAccountStubAuth,
		httpapi.OperatorAuthz(store, management.PermCredentialsManage))
	httpapi.NewAdminCredentialsHandler(credSvc).Register(adminG)
	httpapi.NewAdminAccountsHandler(credentials.NewAccountService(store, store)).Register(adminG)

	return &accountE2EEnv{
		engine: engine, db: db, store: store, provID: prov.ID, credID: cv.ID,
		opHeader: map[string]string{"X-Test-User": opUser, "X-Test-App": "ops-console"},
	}
}

func adminDo(t *testing.T, engine *gin.Engine, method, path string, body map[string]any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(raw))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestAdminUpstreamAccounts_FullLifecycleE2E 验收 §5.3 + §5.8：
// 创建账号前调用失败 → 创建后立即可调度 → 凭据吊销后失败 → 凭据恢复但账号
// 不自动恢复（仍失败）→ 账号激活后恢复。
func TestAdminUpstreamAccounts_FullLifecycleE2E(t *testing.T) {
	up := newChatStubUpstream(t)
	env := setupAccountE2E(t, up)

	login := loginAndGetTokens(t, env.engine, "accte2e", "yundian")
	grantKayaModelEntitlement(t, env.db, env.store, login.User.ID, "deepseek-chat")
	chatBody := map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}}

	chatOK := func() (int, string) {
		w := chatPost(t, env.engine, login.AccessToken, chatBody)
		return w.Code, w.Body.String()
	}

	// 1) 缺口复现：凭据在、账号不在 → 调用不可达上游。
	if code, body := chatOK(); code == http.StatusOK {
		t.Fatalf("chat without upstream account = 200 %s, want failure", body)
	}

	// 2) 绑定可调度账号（新 provider 接入的最后一步）。
	w := adminDo(t, env.engine, http.MethodPost, "/admin/upstream-accounts", map[string]any{
		"provider_id": env.provID, "credential_id": env.credID,
		"concurrency_limit": 4, "reason": "接入 deepseek 静态 key",
	}, env.opHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("create account = %d %s", w.Code, w.Body.String())
	}
	var createEnv struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createEnv); err != nil || createEnv.Data.ID == "" {
		t.Fatalf("create response = %s err=%v", w.Body.String(), err)
	}
	accountID := createEnv.Data.ID

	// 3) 创建即入路由池（无需 publish）：同一 provider 的模型经网关调用成功。
	if code, body := chatOK(); code != http.StatusOK {
		t.Fatalf("chat after account create = %d %s, want 200", code, body)
	} else if !strings.Contains(body, `"content":"你"`) {
		t.Fatalf("chat body = %s", body)
	}

	// 4) 凭据吊销：级联停用账号（同事务），调用失败。
	w = adminDo(t, env.engine, http.MethodPost, "/admin/credentials/"+env.credID+"/status",
		map[string]any{"status": "revoked", "reason": "key 泄露应急吊销"}, env.opHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke credential = %d %s", w.Code, w.Body.String())
	}
	var acctStatus string
	if err := env.db.Get(&acctStatus,
		`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	if acctStatus != "disabled" {
		t.Fatalf("account after revoke = %s, want disabled (级联停用)", acctStatus)
	}
	if code, _ := chatOK(); code == http.StatusOK {
		t.Fatal("chat after revoke = 200, want failure")
	}

	// 5) 凭据恢复 ≠ 账号恢复：账号仍 disabled，调用仍失败。
	w = adminDo(t, env.engine, http.MethodPost, "/admin/credentials/"+env.credID+"/status",
		map[string]any{"status": "active", "reason": "换发新 key 完成"}, env.opHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("restore credential = %d %s", w.Code, w.Body.String())
	}
	if code, _ := chatOK(); code == http.StatusOK {
		t.Fatal("chat after credential restore without account reactivation = 200, want failure")
	}

	// 6) 账号激活（唯一恢复入口）→ 端到端调用恢复。
	w = adminDo(t, env.engine, http.MethodPost, "/admin/upstream-accounts/"+accountID+"/status",
		map[string]any{"status": "active", "reason": "凭据已恢复，账号复投"}, env.opHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("reactivate account = %d %s", w.Code, w.Body.String())
	}
	if code, body := chatOK(); code != http.StatusOK {
		t.Fatalf("chat after account reactivation = %d %s, want 200", code, body)
	} else if !strings.Contains(body, `"content":"你"`) {
		t.Fatalf("chat body = %s", body)
	}
}
