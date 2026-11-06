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

// kaya_models_t9_test.go — T9：GET /chat/models 在既有字段之上**追加**准确
// 的能力描述（图片 / 工具 / 思考 / 上下文与输出上限）。
//
// 硬约束：追加的字段**不得**声明超过当前入口（已发布目录里的模型+路由）
// 与 adapter 的能力 —— 一个只会 openai_responses 的模型不能被声明成能收
// 图片（该协议的适配层显式不支持图片）。
//
// 断言落在响应 JSON 上（线上契约），因此在未改动的生产代码上是行为断言
// 失败（RED），不是编译错误。

func TestKayaModels_CapabilityDescriptionIsAccurate(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()
	catalogSvc := catalog.NewService(store)

	models := []struct {
		id       string
		protocol domain.Protocol
	}{
		{"vision-chat", domain.ProtocolOpenAIChat},
		{"text-responses", domain.ProtocolOpenAIResponses},
	}
	for _, m := range models {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m.id, DisplayName: m.id, Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols:       []domain.Protocol{m.protocol, domain.ProtocolKayaChat},
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
	depChat := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depChat); err != nil {
		t.Fatal(err)
	}
	depResp := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-up-responses", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIResponses, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depResp); err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		dep := depChat
		if m.protocol == domain.ProtocolOpenAIResponses {
			dep = depResp
		}
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: m.id, DeploymentID: dep.ID, Weight: 1, Enabled: true,
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
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		c.Next()
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "vision-chat").List)

	userID, _ := f.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-cap", Revision: 1, ModelIDs: []string{"vision-chat", "text-responses"},
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{"vision-chat", "text-responses"},
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
	byID := map[string]map[string]any{}
	for _, m := range env.Data.Models {
		byID[m["id"].(string)] = m
	}
	vision, ok := byID["vision-chat"]
	if !ok {
		t.Fatalf("vision-chat missing from %v", env.Data.Models)
	}
	// openai_chat 适配层支持图片/工具/推理 → 三项都为 true。
	if vision["supports_images"] != true {
		t.Errorf("vision-chat supports_images = %v, want true (openai_chat 承载图片)", vision["supports_images"])
	}
	if vision["supports_tools"] != true {
		t.Errorf("vision-chat supports_tools = %v, want true", vision["supports_tools"])
	}
	if vision["supports_thinking"] != true {
		t.Errorf("vision-chat supports_thinking = %v, want true", vision["supports_thinking"])
	}
	if vision["context_tokens"] != float64(200000) {
		t.Errorf("vision-chat context_tokens = %v, want 200000", vision["context_tokens"])
	}
	if vision["max_output_tokens"] != float64(8192) {
		t.Errorf("vision-chat max_output_tokens = %v, want 8192", vision["max_output_tokens"])
	}
	if _, ok := vision["capability_description"]; !ok {
		t.Errorf("capability_description missing: %v", vision)
	}

	text, ok := byID["text-responses"]
	if !ok {
		t.Fatalf("text-responses missing from %v", env.Data.Models)
	}
	// openai_responses 适配层**不支持图片** → 绝不声明能收图片。
	if text["supports_images"] != false {
		t.Errorf("text-responses supports_images = %v, want false (该协议不承载图片)", text["supports_images"])
	}
	if text["supports_tools"] != true {
		t.Errorf("text-responses supports_tools = %v, want true", text["supports_tools"])
	}
}
