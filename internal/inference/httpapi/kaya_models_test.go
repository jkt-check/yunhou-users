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
	f := newAccessFixture(t, 0)
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
	f := newAccessFixture(t, 0)
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
	f := newAccessFixture(t, 0)
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
