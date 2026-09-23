// v1_protocol_test.go — Task 8 标准协议面测试:/v1/models 与
// /v1/chat/completions 走完整鉴权链(真实库 + 真实 httptest 上游),
// 断言原生 OpenAI 响应形状(无管理 envelope)、权益可见性、能力校验与
// 错误映射。skip 不算通过。
package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
)

// v1Fixture is the full standard-protocol stack over the disposable DB.
type v1Fixture struct {
	db       *sqlx.DB
	store    *postgres.Store
	engine   *gin.Engine
	upstream *httptest.Server
	modelID  string
	policyID string
	account  *domain.BillingAccount
	keyPlain string
	keyID    string
	// bodies records upstream request bodies（Task 16: 转发断言;非阻塞写入）。
	bodies chan []byte
}

// lastUpstreamBody returns the most recent upstream request body.
func (f *v1Fixture) lastUpstreamBody(t *testing.T) string {
	t.Helper()
	select {
	case b := <-f.bodies:
		return string(b)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream saw no request")
		return ""
	}
}

func newV1Fixture(t *testing.T) *v1Fixture {
	t.Helper()
	fx := newAccessFixture(t, 0) // reuses the wipe + token service harness
	db := fx.db
	store := fx.store
	ctx := context.Background()

	f := &v1Fixture{db: db, store: store, modelID: "glm-4.6", bodies: make(chan []byte, 16)}

	// --- catalog: published model with a public (never dialed) deployment ---
	if err := store.InsertModel(ctx, &domain.Model{
		ID: f.modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		SupportsTools: true, SupportsReasoning: true,
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	// A second model exists but is NOT in the caller's entitlement set.
	if err := store.InsertModel(ctx, &domain.Model{
		ID: "kimi-k2", DisplayName: "Kimi K2", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		SupportsTools: true,
	}); err != nil {
		t.Fatalf("insert model 2: %v", err)
	}
	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	depPub := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-upstream",
		BaseURL: "https://upstream.example.com", Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 30 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depPub); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	for _, m := range []string{f.modelID, "kimi-k2"} {
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: m, DeploymentID: depPub.ID, Weight: 1, Enabled: true,
		}); err != nil {
			t.Fatalf("insert route: %v", err)
		}
	}
	catalogSvc := catalog.NewService(store)
	catalogSvc.SetPriceCheck(func(ctx context.Context, modelID string) (bool, error) {
		_, err := store.LatestPriceVersion(ctx, modelID, string(accounting.PriceSaleCredit), time.Now())
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if _, err := catalogSvc.Publish(ctx, "test"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// --- policy / prices / entitlement / key ---
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: microP(1_000_000_000), WeeklyLimit: microP(10_000_000_000),
		MonthlyLimit: microP(100_000_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	f.policyID = pol.ID
	now := time.Now().Add(-time.Minute)
	for _, kind := range []string{postgres.PriceSaleCredit, postgres.PriceUpstreamCost} {
		pv := &postgres.PriceVersion{
			ModelID: f.modelID, Kind: kind,
			InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
			Revision: 1, EffectiveFrom: now,
		}
		if kind == postgres.PriceUpstreamCost {
			pv.Unit = "micromoney"
			pv.Currency = "USD"
		} else {
			pv.Unit = "microcredit"
		}
		if err := store.InsertPriceVersion(ctx, pv); err != nil {
			t.Fatalf("insert price %s: %v", kind, err)
		}
	}

	userID, _ := fx.addUser(t)
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	f.account = acct
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
	created, err := fx.keySvc.CreateKey(ctx, userID, access.CreateParams{Name: "cli"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	f.keyPlain = created.Plaintext
	f.keyID = created.Key.ID

	// --- gateway over a loopback httptest upstream (静态快照;发布校验拒绝
	// loopback 是刻意的写路径行为,见 gateway 包测试说明) ---
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case f.bodies <- body:
		default:
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for _, c := range []string{
				"data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n",
				"data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n",
				"data: [DONE]\n\n",
			} {
				_, _ = io.WriteString(w, c)
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-v1ns","object":"chat.completion","created":1,"model":"glm-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	}))
	t.Cleanup(f.upstream.Close)

	// Deployment + account rows for the attempt FKs (repo insert bypasses the
	// write-path URL validation deliberately).
	depRT := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-upstream", BaseURL: f.upstream.URL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, depRT); err != nil {
		t.Fatalf("insert runtime deployment: %v", err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	credSvc := credentials.NewService(vault, store, store)
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov.ID, "main", "api_key", "sk-up", "seed", nil)
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
		Models: map[string]domain.Model{
			f.modelID: {
				ID: f.modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
				ContextTokens: 200000, MaxOutputTokens: 8192,
				Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
				InputModalities: []string{"text"}, OutputModalities: []string{"text"},
				SupportsTools: true, SupportsReasoning: true,
			},
			// kimi-k2 在网关中存在但 (a) 默认不在调用者权益集合内,
			// (b) 声明不支持工具 — 两个门分别由错误矩阵/能力测试钉牢。
			"kimi-k2": {
				ID: "kimi-k2", DisplayName: "Kimi K2", Lifecycle: domain.LifecycleActive,
				ContextTokens: 200000, MaxOutputTokens: 8192,
				Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
				InputModalities: []string{"text"}, OutputModalities: []string{"text"},
				SupportsTools: false,
			},
		},
		Providers:   map[string]domain.Provider{prov.ID: *prov},
		Deployments: map[string]domain.Deployment{depRT.ID: *depRT},
		RoutesByModel: map[string][]domain.ModelRoute{
			f.modelID: {{ModelID: f.modelID, DeploymentID: depRT.ID, Weight: 1, Enabled: true}},
			"kimi-k2": {{ModelID: "kimi-k2", DeploymentID: depRT.ID, Weight: 1, Enabled: true}},
		},
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}
	routingSvc := routing.NewService(store, adapters, nil)
	gw := gateway.NewService(staticSnap{snap}, store, access.NewEntitlementResolver(store, nil),
		quota.NewService(store, nil), routingSvc, credSvc, providers.NewHTTPClient(egress), egress, nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(fx.resolver, access.NewRPMCounter(nil), 0))
	v1.GET("/models", httpapi.NewModelsHandler(catalogSvc, fx.resolver).List)
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	f.engine = engine
	return f
}

type staticSnap struct{ snap *catalog.Snapshot }

func (s staticSnap) Current(ctx context.Context) (*catalog.Snapshot, error) { return s.snap, nil }

func microP(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

func (f *v1Fixture) call(t *testing.T, method, path, auth string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rdr)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// --- /v1/models ---------------------------------------------------------------

func TestV1Models_NativeShapeAndEntitlementScope(t *testing.T) {
	f := newV1Fixture(t)

	// Unauthenticated → 401 native shape (no management envelope).
	w := f.call(t, http.MethodGet, "/v1/models", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	var errBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Errorf("missing native error key: %s", w.Body.String())
	}
	if _, ok := errBody["code"]; ok {
		t.Errorf("management envelope leaked into /v1: %s", w.Body.String())
	}

	// Authenticated → standard model list shape, entitlement-scoped:
	// glm-4.6 visible, kimi-k2 (published but not entitled) absent.
	w = f.call(t, http.MethodGet, "/v1/models", f.keyPlain, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "glm-4.6" || list.Data[0].Object != "model" {
		t.Errorf("data = %+v, want exactly the entitled model", list.Data)
	}
}

// --- /v1/chat/completions ------------------------------------------------------

// 标准客户端形状消费:非流式 JSON 与流式 SSE 都用通用解码方式验证。
func TestV1ChatCompletions_NonStreamNativeShape(t *testing.T) {
	f := newV1Fixture(t)
	w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model": "glm-4.6",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	// A standard OpenAI client decodes this directly.
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("standard client decode: %v (%s)", err, w.Body.String())
	}
	if resp.Object != "chat.completion" || resp.Choices[0].Message.Content != "Hello" ||
		resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("resp = %+v", resp)
	}
	// 持久化:已结算,客户收费 7×1 + 5×2 = 17 micros。
	var status string
	var settled int64
	if err := f.db.QueryRow(`SELECT status, settled_micros FROM inference_requests ORDER BY created_at DESC LIMIT 1`).
		Scan(&status, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || settled != 17 {
		t.Errorf("request = %s/%d, want settled/17", status, settled)
	}
}

func TestV1ChatCompletions_StreamSSE(t *testing.T) {
	f := newV1Fixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+f.keyPlain)
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	// 标准 SSE 客户端消费:按事件块解析,data: [DONE] 终止。
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var content strings.Builder
	sawDone, sawUsage := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("client parse chunk %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
		}
		if chunk.Usage != nil {
			sawUsage = true
			if chunk.Usage.PromptTokens != 7 || chunk.Usage.CompletionTokens != 5 {
				t.Errorf("usage = %+v", chunk.Usage)
			}
		}
	}
	if content.String() != "Hello" || !sawDone || !sawUsage {
		t.Errorf("content=%q done=%v usage=%v", content.String(), sawDone, sawUsage)
	}
}

func TestV1ChatCompletions_ErrorMatrix(t *testing.T) {
	f := newV1Fixture(t)

	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
		wantErr  string
	}{
		{"unknown model → 404", map[string]any{
			"model": "no-such", "messages": []map[string]any{{"role": "user", "content": "x"}},
		}, http.StatusNotFound, "model_not_found"},
		{"unentitled model → 403", map[string]any{
			"model": "kimi-k2", "messages": []map[string]any{{"role": "user", "content": "x"}},
		}, http.StatusForbidden, "model_not_allowed"},
		{"multimodal content → 400 (不静默丢字段)", map[string]any{
			"model": "glm-4.6",
			"messages": []map[string]any{{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": "x"}},
			}},
		}, http.StatusBadRequest, "invalid_input"},
		{"modalities field → 400", map[string]any{
			"model": "glm-4.6", "modalities": []string{"text", "image"},
			"messages": []map[string]any{{"role": "user", "content": "x"}},
		}, http.StatusBadRequest, "invalid_input"},
		{"n>1 → 400", map[string]any{
			"model": "glm-4.6", "n": 2,
			"messages": []map[string]any{{"role": "user", "content": "x"}},
		}, http.StatusBadRequest, "invalid_input"},
		{"empty messages → 400", map[string]any{
			"model": "glm-4.6",
		}, http.StatusBadRequest, "invalid_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d; body = %s", w.Code, tc.wantCode, w.Body.String())
			}
			var errBody struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != tc.wantErr {
				t.Errorf("error body = %s, want code %s", w.Body.String(), tc.wantErr)
			}
		})
	}
}

// 额度耗尽 → 429 + Retry-After(可计算恢复时刻) + 原生形状。
func TestV1ChatCompletions_QuotaExceeded429(t *testing.T) {
	f := newV1Fixture(t)
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 1 WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model": "glm-4.6", "messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "quota_exceeded" || !strings.Contains(errBody.Error.Message, "five_hour") {
		t.Errorf("error = %+v, want quota_exceeded naming the window", errBody.Error)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set when every blocking window has a computable reset")
	}
}

// 工具能力拒绝经由网关:模型不支持工具时 400,且没有任何上游/账本痕迹。
func TestV1ChatCompletions_ToolsCapabilityRejected(t *testing.T) {
	f := newV1Fixture(t)
	// 静态快照中的模型关掉工具能力。
	// (这里通过改快照不可行 —— 直接用另一个无 tools 能力的模型:把 kimi-k2
	// 加进权益并让它的快照模型不支持工具。)
	ctx := context.Background()
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 2, ModelIDs: []string{f.modelID, "kimi-k2"},
		FiveHourLimit: microP(1_000_000_000), WeeklyLimit: microP(10_000_000_000),
		MonthlyLimit: microP(100_000_000_000), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	if err := f.store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: f.account.ID, SourceType: domain.SourceOrder,
		SourceID: "ord-" + uuid.NewString(), ModelIDs: []string{"kimi-k2"},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	w := f.call(t, http.MethodPost, "/v1/chat/completions", f.keyPlain, map[string]any{
		"model":    "kimi-k2",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "run_shell"}}},
	})
	// 模型声明不支持工具 → 明确的 400(不能静默丢字段);零上游痕迹。
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != "invalid_input" {
		t.Errorf("error = %s, want invalid_input", w.Body.String())
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_attempts`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("attempts = %d, want 0 (capability rejection precedes any spend)", cnt)
	}
}
