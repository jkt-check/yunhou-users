package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
)

// kaya_models_test.go — Task 16 覆盖率补强：GET /chat/models 的三个分支
// （N4a：无账户→403 对齐 legacy；有权益→条目带 provider/default；未授权
// 模型不出现）。

func TestKayaModels_ListBranches(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()

	// 目录：两个已发布模型（一个授权、一个未授权），各一条活跃部署。
	catalogSvc := catalog.NewService(store)
	for _, m := range []string{"deepseek-chat", "glm-4.6"} {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m, DisplayName: m, Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &domain.Provider{Code: "deepseek", DisplayName: "DeepSeek",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "ds-up", BaseURL: "https://api.deepseek.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"deepseek-chat", "glm-4.6"} {
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: m, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	catalogSvc.SetPriceCheck(func(ctx context.Context, id string) (bool, error) { return true, nil })
	if _, err := catalogSvc.Publish(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	resolver := access.NewResolver(store, nil)
	// 快照源 = 共享 SnapshotCache（评审裁定：不得用 Catalog.LoadSnapshot——
	// 它绕开 cache，冷启动哨兵不可达）。
	cache := catalog.NewSnapshotCache(store, func(error) {})
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "deepseek-chat").List)

	call := func(userID string) (int, []map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		req.Header.Set("X-Test-User", userID)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		var env struct {
			Code int `json:"code"`
			Data struct {
				Models []map[string]any `json:"models"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return w.Code, env.Data.Models
	}

	// 分支 1（N4a，推翻原 200 空数组设计）：无计费账户（从未购买/获赠）=
	// 无有效订阅 → 403，envelope 与 legacy ErrChatNoAccess 逐字节一致——
	// picker 对 403 已有处理（隐藏选择器），而 200 空数组会让 picker 静默
	// 消失且与服务故障无法区分。
	userID, _ := f.addUser(t)
	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-account picker = %d, want 403: %s", w.Code, w.Body.String())
	}
	const wantBody = `{"code":403,"data":null,"message":"active subscription with access to this app is required"}`
	if w.Body.String() != wantBody {
		t.Errorf("no-account envelope = %s, want byte-exact legacy shape %s", w.Body.String(), wantBody)
	}

	// 分支 2：有 deepseek-chat 权益 → 恰一条，provider=deepseek，default=true。
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-gift", Revision: 1, ModelIDs: []string{"deepseek-chat"},
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{"deepseek-chat"},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	code, models := call(userID)
	if code != http.StatusOK || len(models) != 1 {
		t.Fatalf("picker = %d %v, want exactly the entitled model", code, models)
	}
	m := models[0]
	if m["id"] != "deepseek-chat" || m["provider"] != "deepseek" || m["default"] != true {
		t.Errorf("entry = %v", m)
	}
	if _, unentitled := m["display_name"]; !unentitled {
		t.Errorf("display_name missing: %v", m)
	}
}

// R7-N2：目录冷启动（从未发布 → 真实 SnapshotCache 无已验证快照，携带
// ErrNoVerifiedSnapshot 哨兵）→ 503 + Retry-After: 5 + envelope
// {code:503,data:null,message:"chat catalog is not ready"}。就绪检查先于
// ResolveUserSession：无计费账户的用户在冷启动时也必须拿 503（验收 §7.2），
// 而不是「无账户 → 403」——「服务未就绪」与「用户无权限」严格分层。
func TestKayaModels_ColdStartIs503BeforeUserResolution(t *testing.T) {
	f := newAccessFixture(t)
	catalogSvc := catalog.NewService(f.store)
	// 真实 cache 直连真实 store：本用例不发布任何 revision = 真实冷启动形态。
	cache := catalog.NewSnapshotCache(f.store, func(error) {})
	resolver := access.NewResolver(f.store, nil)

	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u-no-account")
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "deepseek-chat").List)

	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold-start /chat/models = %d, want 503: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
	var env struct {
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Code != 503 || string(env.Data) != "null" || env.Message != "chat catalog is not ready" {
		t.Errorf("envelope = %s, want {code:503,data:null,message:\"chat catalog is not ready\"}", w.Body.String())
	}
}

// failingSnapshotSource 手搓桩：注入非哨兵快照错误（真实 SnapshotCache 在冷
// 启动时一律包哨兵，非哨兵错误只可能来自其它实现——防御性口径仍需钉住）。
type failingSnapshotSource struct{ err error }

func (s failingSnapshotSource) Current(context.Context) (*catalog.Snapshot, error) {
	return nil, s.err
}

// 非哨兵快照错误不得伪装成 503 冷启动：走包内 fail() 口径 → 500。
func TestKayaModels_NonSentinelSnapshotErrorIs500(t *testing.T) {
	f := newAccessFixture(t)
	catalogSvc := catalog.NewService(f.store)
	resolver := access.NewResolver(f.store, nil)
	boom := errors.New("snapshot store gone")

	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u1")
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, failingSnapshotSource{err: boom}, "deepseek-chat").List)

	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("non-sentinel snapshot error = %d, want 500: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "" {
		t.Errorf("Retry-After must not be set on non-sentinel errors: %q", w.Header().Get("Retry-After"))
	}
}

// Task 10：GET /chat/models 透出 input_modalities —— kaya picker 据此决定
// 哪些内置模型允许发图。vision 模型（["text","image"]）原样透出。
// 空值归一（缺省放行 ["text","image"]）由包内测试钉住（见
// kaya_models_internal_test.go）。
func TestKayaModels_ListInputModalities(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()

	catalogSvc := catalog.NewService(store)
	if err := store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6v", DisplayName: "GLM 4.6V", Lifecycle: domain.LifecycleActive,
		ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatal(err)
	}
	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-4.6v-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: "glm-4.6v", DeploymentID: dep.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	catalogSvc.SetPriceCheck(func(ctx context.Context, id string) (bool, error) { return true, nil })
	if _, err := catalogSvc.Publish(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	resolver := access.NewResolver(store, nil)
	cache := catalog.NewSnapshotCache(store, func(error) {})
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "glm-4.6v").List)

	// 有 glm-4.6v 权益的用户 → 条目带 input_modalities=["text","image"]。
	userID, _ := f.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-gift", Revision: 1, ModelIDs: []string{"glm-4.6v"},
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{"glm-4.6v"},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("picker = %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []map[string]any `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(env.Data.Models) != 1 {
		t.Fatalf("models = %v, want exactly the entitled model", env.Data.Models)
	}
	mods, ok := env.Data.Models[0]["input_modalities"].([]any)
	if !ok {
		t.Fatalf("input_modalities missing or wrong type: %v", env.Data.Models[0])
	}
	if len(mods) != 2 || mods[0] != "text" || mods[1] != "image" {
		t.Errorf("input_modalities = %v, want [text image]", mods)
	}
}

// T3：只有 disabled provider 的模型不得被当作可用部署展示；草稿停用未
// 发布前不影响当前快照（发布快照是权威事实源）。模型列表的可用性必须与
// 实际调度一致——列表里有的模型必须真的有可用部署。
func TestKayaModels_DisabledProviderHidesModel(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()

	catalogSvc := catalog.NewService(store)
	if err := store.InsertModel(ctx, &domain.Model{
		ID: "deepseek-chat", DisplayName: "DeepSeek Chat", Lifecycle: domain.LifecycleActive,
		ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatal(err)
	}
	prov := &domain.Provider{Code: "deepseek", DisplayName: "DeepSeek",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "ds-up", BaseURL: "https://api.deepseek.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: "deepseek-chat", DeploymentID: dep.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	catalogSvc.SetPriceCheck(func(ctx context.Context, id string) (bool, error) { return true, nil })
	if _, err := catalogSvc.Publish(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	resolver := access.NewResolver(store, nil)
	cache := catalog.NewSnapshotCache(store, func(error) {})
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "deepseek-chat").List)

	call := func(userID string) (int, []map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		req.Header.Set("X-Test-User", userID)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		var env struct {
			Code int `json:"code"`
			Data struct {
				Models []map[string]any `json:"models"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return w.Code, env.Data.Models
	}

	// 有权益的用户。
	userID, _ := f.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-gift", Revision: 1, ModelIDs: []string{"deepseek-chat"},
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{"deepseek-chat"},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}

	// ① 已发布且 provider active → 模型可见。
	code, models := call(userID)
	if code != http.StatusOK || len(models) != 1 {
		t.Fatalf("picker = %d %v, want the entitled model", code, models)
	}

	// ② 草稿停用 provider（未发布）→ 当前已发布快照不变，模型仍然可见。
	prov.Status = "disabled"
	if err := store.UpdateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	code, models = call(userID)
	if code != http.StatusOK || len(models) != 1 {
		t.Fatalf("picker after the draft disable = %d %v, want the model still visible（草稿编辑不影响当前快照）", code, models)
	}

	// ③ 发布后停用生效 → 模型不再被展示（只有 disabled provider 的模型
	// 不得被当作可用部署）。
	if _, err := catalogSvc.Publish(ctx, "test-disable"); err != nil {
		t.Fatal(err)
	}
	code, models = call(userID)
	if code != http.StatusOK {
		t.Fatalf("picker = %d, want 200", code)
	}
	if len(models) != 0 {
		t.Errorf("picker = %v, want no model（provider 停用后模型没有可用部署）", models)
	}
}

// T8: /chat/models 的 input_modalities 必须与**派发**能力一致 —— 声明了
// image 但已发布部署里没有能承载图片的协议时，不得再 advertise image
// （否则 picker 让客户发图，网关在准入前当场 400）。
//
// 判据必须与派发路径共用同一个能力判定（routing 的 CompatibleDeployments），
// 不能另立一条规则。
func TestKayaModels_ImageModalitiesFollowDispatchCapability(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()

	catalogSvc := catalog.NewService(store)
	models := []string{"glm-vision-v", "glm-vision-c", "glm-vision-a"}
	for _, m := range models {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m, DisplayName: m, Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
			InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	// glm-vision-v 只有 openai_responses 部署（本期该协议不承载图片）；
	// glm-vision-c 有 openai_chat 部署、glm-vision-a 有 anthropic_messages
	// 部署（两者都承载图片）。
	depResponses := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "v-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIResponses, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depResponses); err != nil {
		t.Fatal(err)
	}
	depChat := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "c-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depChat); err != nil {
		t.Fatal(err)
	}
	depAnthropic := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "a-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolAnthropicMessage, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depAnthropic); err != nil {
		t.Fatal(err)
	}
	for modelID, depID := range map[string]string{
		"glm-vision-v": depResponses.ID,
		"glm-vision-c": depChat.ID,
		"glm-vision-a": depAnthropic.ID,
	} {
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: modelID, DeploymentID: depID, Weight: 1, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	catalogSvc.SetPriceCheck(func(ctx context.Context, id string) (bool, error) { return true, nil })
	if _, err := catalogSvc.Publish(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	resolver := access.NewResolver(store, nil)
	cache := catalog.NewSnapshotCache(store, func(error) {})
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "glm-vision-c").List)

	userID, _ := f.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-gift", Revision: 1, ModelIDs: models,
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: models,
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("picker = %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Models []map[string]any `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	mods := map[string][]any{}
	for _, m := range env.Data.Models {
		list, ok := m["input_modalities"].([]any)
		if !ok {
			t.Fatalf("input_modalities missing or wrong type: %v", m)
		}
		mods[m["id"].(string)] = list
	}
	// 只有 openai_responses 部署的模型：不得 advertise image（派发会拒）。
	if got := mods["glm-vision-v"]; len(got) != 1 || got[0] != "text" {
		t.Errorf("glm-vision-v input_modalities = %v, want [text] "+
			"(没有能承载图片的部署时不得 advertise image)", got)
	}
	// openai_chat 部署的模型：保留 image。
	if got := mods["glm-vision-c"]; len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Errorf("glm-vision-c input_modalities = %v, want [text image]", got)
	}
	// anthropic_messages 部署的模型：同样保留 image。
	if got := mods["glm-vision-a"]; len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Errorf("glm-vision-a input_modalities = %v, want [text image]", got)
	}
}
