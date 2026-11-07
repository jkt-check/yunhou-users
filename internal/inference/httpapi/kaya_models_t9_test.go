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
		// 声明层：这两个条目都声明支持工具与推理 —— 于是「派发能力 ∧
		// 声明」仍为 true，下面的断言不被误收窄（防误收窄的对照）。
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m.id, DisplayName: m.id, Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols:       []domain.Protocol{m.protocol, domain.ProtocolKayaChat},
			InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
			SupportsTools: true, SupportsReasoning: true,
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

// TestKayaModels_UndeclaredCapabilitiesAreNotAdvertised pins the OTHER half
// of T9's contract: the listing must not advertise tools/thinking the model
// does not DECLARE, even when the route + adapter could carry them.
//
// The capability oracle answers "can this published route + adapter serve
// tools/reasoning at all" — it deliberately says nothing about the model's
// own declaration. The dispatcher does look at the declaration
// (gateway/service.go: a tools call on a model declaring SupportsTools=false
// is refused), so a listing that trusts the oracle alone advertises a
// capability the dispatcher refuses → the picker lets the user ask for tools
// and the gateway rejects the call.
//
// Both models sit behind the SAME route + adapter (openai_chat carries
// tools and reasoning), so the oracle answers true for both — only the
// declaration differs.
func TestKayaModels_UndeclaredCapabilitiesAreNotAdvertised(t *testing.T) {
	f := newAccessFixture(t)
	store := f.store
	ctx := context.Background()
	catalogSvc := catalog.NewService(store)

	for _, m := range []struct {
		id             string
		supportsTools  bool
		supportsReason bool
	}{
		{"plain-model", false, false},
		{"capable-model", true, true},
	} {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m.id, DisplayName: m.id, Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			SupportsTools: m.supportsTools, SupportsReasoning: m.supportsReason,
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-up", BaseURL: "https://api.zhipu.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"plain-model", "capable-model"} {
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: id, DeploymentID: dep.ID, Weight: 1, Enabled: true,
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
	}, httpapi.NewKayaModelsHandler(catalogSvc, resolver, cache, "plain-model").List)

	userID, _ := f.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "kaya-cap", Revision: 1, ModelIDs: []string{"plain-model", "capable-model"},
		FiveHourLimit: microP(1_000), WeeklyLimit: microP(2_000), MonthlyLimit: microP(3_000),
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant,
		SourceID: "gift-" + uuid.NewString(), ModelIDs: []string{"plain-model", "capable-model"},
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
	plain, ok := byID["plain-model"]
	if !ok {
		t.Fatalf("plain-model missing from %v", env.Data.Models)
	}
	// 模型声明 tools/reasoning=false → 列表不得声明 true（route+adapter
	// 能承载不算数：派发路径按模型声明拒绝）。
	if plain["supports_tools"] != false {
		t.Errorf("plain-model supports_tools = %v, want false (模型未声明支持工具)", plain["supports_tools"])
	}
	if plain["supports_thinking"] != false {
		t.Errorf("plain-model supports_thinking = %v, want false (模型未声明支持推理)", plain["supports_thinking"])
	}
	capable, ok := byID["capable-model"]
	if !ok {
		t.Fatalf("capable-model missing from %v", env.Data.Models)
	}
	// 对照组：声明 + 派发都支持 → 仍然是 true（不得误收窄）。
	if capable["supports_tools"] != true {
		t.Errorf("capable-model supports_tools = %v, want true", capable["supports_tools"])
	}
	if capable["supports_thinking"] != true {
		t.Errorf("capable-model supports_thinking = %v, want true", capable["supports_thinking"])
	}
}
