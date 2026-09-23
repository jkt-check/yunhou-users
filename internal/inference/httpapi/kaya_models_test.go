package httpapi_test

import (
	"context"
	"encoding/json"
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
// （无账户→空列表；有权益→条目带 provider/default；未授权模型不出现）。

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
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, "deepseek-chat").List)

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

	// 分支 1：无计费账户（从未购买/获赠）→ 200 空列表（非错误）。
	userID, _ := f.addUser(t)
	code, models := call(userID)
	if code != http.StatusOK || len(models) != 0 {
		t.Fatalf("no-account picker = %d %v, want 200 []", code, models)
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
	code, models = call(userID)
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
