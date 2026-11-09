// gateway_acceptance_test.go — T10 集成验收（真实一次性 PostgreSQL +
// httptest 模拟上游）。
//
// 验收矩阵：四个客户端入口（/chat、/v1/chat/completions、/v1/messages、
// /v1/responses）× 场景（成功、拒绝、断流、中断、失败切换/重试、未知执行、
// 恢复）。每个场景断言的都是**持久化事实**——inference_requests /
// inference_attempts / inference_usage_records / inference_ledger_entries /
// inference_reservations / inference_rate_reservations /
// inference_concurrency_leases / inference_reconciliation_jobs /
// inference_quota_windows——而不是只有 HTTP 响应体。
//
// fixture 复用既有基建：setupDB（真实库连接，连接失败即 Fatalf，绝不静默
// skip）、writeOpenAIChunks（SSE 写出）、staticSnapshot（目录快照）、
// micro（额度常量）；不另起一套服务器，也不复制既有 drill 的场景。
package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/inference/workers"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// ---------------------------------------------------------------------------
// 验收常量
// ---------------------------------------------------------------------------

const (
	accOpenAIModel    = "acc-openai-model"
	accAnthropicModel = "acc-anthropic-model"
	accUnentitled     = "acc-unentitled-model"
	accPolicyName     = "acceptance"
	accAppID          = "yundian"

	// 两个上游都报这组用量，于是四个入口的结算金额相同：
	// input 7 × 1 micro + output 5 × 2 micro = 17 micro。
	accInputTokens  = 7
	accOutputTokens = 5
	accChargeMicros = int64(accInputTokens*1 + accOutputTokens*2)
)

// accUpstream 是一个可换脚本的模拟上游；每次请求记录一次调用。
type accUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
	calls   int
}

func newAccUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *accUpstream {
	t.Helper()
	u := &accUpstream{handler: h}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls++
		f := u.handler
		u.mu.Unlock()
		f(w, r, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *accUpstream) set(h func(w http.ResponseWriter, r *http.Request, body []byte)) {
	u.mu.Lock()
	u.handler = h
	u.mu.Unlock()
}

func (u *accUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// accSSEOpenAI 是 OpenAI 口径的成功流（usage 7/5 + [DONE]）。
func accSSEOpenAI(w http.ResponseWriter, _ *http.Request, _ []byte) {
	writeOpenAIChunks(w,
		`{"id":"chatcmpl-acc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
		`{"id":"chatcmpl-acc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-acc","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
		"[DONE]")
}

// accJSONOpenAI 是 OpenAI 口径的非流式成功体。
func accJSONOpenAI(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-acc","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
}

// accSSEAnthropic 是 Anthropic 口径的成功流（message_start 报 input=7，
// message_delta 报累计 output=5，message_stop 收尾）。
func accSSEAnthropic(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, ev := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_acc\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"up\",\"content\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		_, _ = io.WriteString(w, ev)
		if fl != nil {
			fl.Flush()
		}
	}
}

// accJSONAnthropic 是 Anthropic 口径的非流式成功体。
func accJSONAnthropic(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_acc","type":"message","role":"assistant","model":"up",
		"content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":7,"output_tokens":5}}`)
}

// ---------------------------------------------------------------------------
// 验收环境
// ---------------------------------------------------------------------------

// acceptanceStack 是一个隔离的验收环境（每次场景全新建库内容）。
type acceptanceStack struct {
	db     *sqlx.DB
	store  *postgres.Store
	engine *gin.Engine

	openaiUp *accUpstream
	anthUp   *accUpstream

	userID    string
	accountID string
	keyPlain  string
	keyID     string
	entID     string
	policyID  string

	openaiModel    string
	anthropicModel string
	openaiDepID    string

	// snap 是目录快照；失败切换场景需要在建环境后追加一条路由。
	snap *catalog.Snapshot
	// credSvc / egress / adapters 是重建网关所需的零件。
	credSvc  *credentials.Service
	egress   *credentials.EgressValidator
	adapters map[domain.Protocol]providers.Adapter
}

// newAcceptanceStack 建环境：两个上游（OpenAI 口径 + Anthropic 口径）、
// 两个模型、一个套餐权益、一把客户 Key、四个客户端入口。
func newAcceptanceStack(t *testing.T) *acceptanceStack {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`TRUNCATE
		inference_wallet_entries, inference_wallet_audits,
		inference_wallet_holds, inference_wallets, inference_payg_config,
		inference_response_chains, inference_bulk_imports,
		inference_session_bindings, inference_oauth_grants,
		inference_audit_log, operator_roles,
		inference_reconciliation_jobs, inference_outbox,
		inference_ledger_entries, inference_adjustments,
		inference_concurrency_leases, inference_reservations,
		inference_quota_windows, inference_usage_records,
		inference_attempts, inference_requests,
		inference_entitlements, inference_policy_versions,
		inference_price_versions,
		inference_api_keys, inference_billing_accounts,
		inference_upstream_accounts, inference_credentials,
		inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models,
		users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	store := postgres.NewStore(db)
	st := &acceptanceStack{
		db: db, store: store,
		openaiModel: accOpenAIModel, anthropicModel: accAnthropicModel,
	}
	st.openaiUp = newAccUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			accSSEOpenAI(w, r, body)
			return
		}
		accJSONOpenAI(w, r, body)
	})
	st.anthUp = newAccUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			accSSEAnthropic(w, r, body)
			return
		}
		accJSONAnthropic(w, r, body)
	})

	protocols := []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses,
		domain.ProtocolAnthropicMessage, domain.ProtocolKayaChat,
	}
	for _, m := range []string{st.openaiModel, st.anthropicModel, accUnentitled} {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m, DisplayName: m, Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192, Protocols: protocols,
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			SupportsTools: true, SupportsReasoning: true,
		}); err != nil {
			t.Fatalf("insert model %s: %v", m, err)
		}
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	credSvc := credentials.NewService(vault, store, store)

	snap := &catalog.Snapshot{
		Models:        map[string]domain.Model{},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{},
		RoutesByModel: map[string][]domain.ModelRoute{},
	}
	addLeg := func(providerCode, modelID, baseURL string, proto domain.Protocol) string {
		prov := &domain.Provider{Code: providerCode, DisplayName: providerCode,
			AccessType: domain.AccessOfficialAPI, Status: "active"}
		if err := store.InsertProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		dep := &domain.Deployment{
			ProviderID: prov.ID, UpstreamModel: "up-model", BaseURL: baseURL,
			Protocol:       proto,
			ConnectTimeout: 2 * time.Second, RequestTimeout: 10 * time.Second,
			Status: domain.DeploymentActive,
		}
		if err := store.InsertDeployment(ctx, dep); err != nil {
			t.Fatal(err)
		}
		cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "acc"},
			prov.ID, "main", "api_key", "sk-"+providerCode, "seed", nil)
		if err != nil {
			t.Fatal(err)
		}
		acct := &domain.UpstreamAccount{
			ProviderID: prov.ID, CredentialID: cv.ID, Status: domain.AccountActive, ConcurrencyLimit: 8,
		}
		if err := store.InsertUpstreamAccount(ctx, acct); err != nil {
			t.Fatal(err)
		}
		if err := store.InsertRoute(ctx, &domain.ModelRoute{
			ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
		mod, err := store.GetModel(ctx, modelID)
		if err != nil {
			t.Fatal(err)
		}
		snap.Models[modelID] = *mod
		snap.Providers[prov.ID] = *prov
		snap.Deployments[dep.ID] = *dep
		snap.RoutesByModel[modelID] = append(snap.RoutesByModel[modelID], domain.ModelRoute{
			ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		})
		return dep.ID
	}
	st.openaiDepID = addLeg("acc-prov-openai", st.openaiModel, st.openaiUp.URL, domain.ProtocolOpenAIChat)
	addLeg("acc-prov-anthropic", st.anthropicModel, st.anthUp.URL, domain.ProtocolAnthropicMessage)
	// 未授权模型：目录里存在、路由可用，但权益不授予它。
	addLeg("acc-prov-unentitled", accUnentitled, st.openaiUp.URL, domain.ProtocolOpenAIChat)

	pol := &postgres.PolicyVersion{
		Name: accPolicyName, Revision: 1, ModelIDs: []string{st.openaiModel, st.anthropicModel},
		FiveHourLimit: microc(1_000_000_000), WeeklyLimit: microc(10_000_000_000),
		MonthlyLimit: microc(100_000_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	st.policyID = pol.ID
	now := time.Now().UTC().Add(-time.Minute)
	for _, m := range []string{st.openaiModel, st.anthropicModel} {
		if err := store.InsertPriceVersion(ctx, &postgres.PriceVersion{
			ModelID: m, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
			InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
			Revision: 1, EffectiveFrom: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	userID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	st.userID = userID
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	st.accountID = acct.ID
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{st.openaiModel, st.anthropicModel},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	st.entID = ent.ID
	created, err := access.NewKeyService(store, nil).CreateKey(ctx, userID, access.CreateParams{Name: "acceptance"})
	if err != nil {
		t.Fatal(err)
	}
	st.keyPlain = created.Plaintext
	st.keyID = created.Key.ID

	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}
	st.credSvc = credSvc
	st.egress = egress
	st.adapters = adapters
	st.snap = snap
	st.rebuildEngine(snap)
	return st
}

// rebuildEngine 用给定目录快照重建网关与四个客户端入口。
func (st *acceptanceStack) rebuildEngine(snap *catalog.Snapshot) {
	gw := gateway.NewService(staticSnapshot{snap}, st.store,
		access.NewEntitlementResolver(st.store, nil), quota.NewService(st.store, nil),
		routing.NewService(st.store, st.adapters, nil), st.credSvc,
		providers.NewHTTPClient(st.egress), st.egress, nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	resolver := access.NewResolver(st.store, nil)
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver))
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	v1.POST("/messages", httpapi.NewMessagesHandler(gw).Create)
	v1.POST("/responses", httpapi.NewResponsesHandler(gw, st.store, nil).Create)

	// /chat（legacy Kaya 面）走同一个网关：facade 把 Kaya 身份解析成统一
	// principal，协议标记为 kaya_chat。JWT 中间件不在本用例范围内（e2e 已
	// 覆盖），这里直接注入服务端已验证的用户身份。
	facade := service.NewChatGatewayFacade(gw, resolver, catalog.NewService(st.store),
		staticSnapshot{snap}, st.openaiModel)
	chatHandler := handler.NewChatHandler(facade, nil)
	engine.POST("/chat", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, st.userID)
		c.Set(middleware.ContextAppID, accAppID)
		c.Next()
	}, chatHandler.StreamChat)
	st.engine = engine
}

// snapshot 返回当前目录快照（失败切换场景在其上追加路由）。
func (st *acceptanceStack) snapshot() *catalog.Snapshot { return st.snap }

// addOpenAILeg 为 openai 模型追加一条候选腿（独立 provider/部署/账号/路由），
// priority 越大越晚试。up 是这条腿的模拟上游。
func (st *acceptanceStack) addOpenAILeg(t *testing.T, code string, priority int,
	h func(w http.ResponseWriter, r *http.Request, body []byte)) (*accUpstream, string) {
	t.Helper()
	ctx := context.Background()
	up := newAccUpstream(t, h)
	prov := &domain.Provider{Code: code, DisplayName: code,
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := st.store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	cv, err := st.credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "acc"},
		prov.ID, "main", "api_key", "sk-"+code, "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{ProviderID: prov.ID, CredentialID: cv.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 4}
	if err := st.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{ProviderID: prov.ID, UpstreamModel: "up-" + code, BaseURL: up.URL,
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: 2 * time.Second,
		RequestTimeout: 10 * time.Second, Status: domain.DeploymentActive}
	if err := st.store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	route := &domain.ModelRoute{ModelID: st.openaiModel, DeploymentID: dep.ID,
		Priority: priority, Weight: 1, Enabled: true}
	if err := st.store.InsertRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	st.snap.Providers[prov.ID] = *prov
	st.snap.Deployments[dep.ID] = *dep
	st.snap.RoutesByModel[st.openaiModel] = append(st.snap.RoutesByModel[st.openaiModel], *route)
	st.rebuildEngine(st.snap)
	return up, dep.ID
}

// setDeploymentTimeout 收紧一条候选腿的请求总超时（模拟上游挂死）。
func (st *acceptanceStack) setDeploymentTimeout(depID string, d time.Duration) {
	dep := st.snap.Deployments[depID]
	dep.RequestTimeout = d
	st.snap.Deployments[depID] = dep
	st.rebuildEngine(st.snap)
}

// ---------------------------------------------------------------------------
// 客户端入口
// ---------------------------------------------------------------------------

// accEntry 描述一个客户端入口：路径、请求体、网关看到的协议。
type accEntry struct {
	name     string
	protocol string
	path     string
	body     string
	// hasKey 表示该入口带客户 Key（/chat 不带 —— Kaya 面按用户身份计费）。
	hasKey bool
}

// entries 返回四个客户端入口（每个入口都走流式 —— 结算/终态化都在流上）。
func (st *acceptanceStack) entries() []accEntry {
	return []accEntry{
		{
			name: "chat", protocol: "kaya_chat", path: "/chat", hasKey: false,
			body: fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, st.openaiModel),
		},
		{
			name: "chat_completions", protocol: "openai_chat", path: "/v1/chat/completions", hasKey: true,
			body: fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, st.openaiModel),
		},
		{
			name: "messages", protocol: "anthropic_messages", path: "/v1/messages", hasKey: true,
			body: fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
				st.anthropicModel),
		},
		{
			name: "responses", protocol: "openai_responses", path: "/v1/responses", hasKey: true,
			body: fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
				st.openaiModel),
		},
	}
}

// call 发起一次入口调用（带 Key 的入口带 Authorization 头）。
func (st *acceptanceStack) call(t *testing.T, e accEntry) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, e.path, strings.NewReader(e.body))
	if e.hasKey {
		req.Header.Set("Authorization", "Bearer "+st.keyPlain)
	}
	w := httptest.NewRecorder()
	st.engine.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// 数据库事实
// ---------------------------------------------------------------------------

// accFacts 是一个逻辑请求的全部持久化事实。
type accFacts struct {
	Found        bool
	RequestID    string
	Status       string
	UsageStatus  string
	Settled      int64
	Protocol     string
	HasKey       bool
	Attempts     int
	AttemptState []string
	AttemptKind  []string
	UsageRows    int
	UsageSource  string
	InputTokens  sql.NullInt64
	OutputTokens sql.NullInt64
	ChargeRows   int
	ChargeAmount int64
	ResTotal     int
	ResHeld      int
	ResSettled   int
	ResReleased  int
	RateRows     int
	RateReserved int
	RateSettled  int
	RateReleased int
	LeaseTotal   int
	LeaseHeld    int
	Jobs         int
	JobReason    string
	JobStatus    string
	WindowUsed   []int64
}

// readAccFacts 读取某个请求行的全部事实（request/attempt/usage/ledger/
// holds/rate reservations/leases/核对任务/窗口聚合）。
func readAccFacts(t *testing.T, db *sqlx.DB, requestID string) accFacts {
	t.Helper()
	f := accFacts{}
	err := db.QueryRow(`SELECT id, status, usage_status, COALESCE(settled_micros,0), protocol,
	                       api_key_id IS NOT NULL
	                    FROM inference_requests WHERE id = $1`, requestID).
		Scan(&f.RequestID, &f.Status, &f.UsageStatus, &f.Settled, &f.Protocol, &f.HasKey)
	if errors.Is(err, sql.ErrNoRows) {
		return f
	}
	if err != nil {
		t.Fatalf("read request %s: %v", requestID, err)
	}
	f.Found = true

	rows, err := db.Query(`SELECT status, error_kind FROM inference_attempts
	                       WHERE request_id = $1 ORDER BY attempt_no`, requestID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	for rows.Next() {
		var s, k string
		if err := rows.Scan(&s, &k); err != nil {
			t.Fatal(err)
		}
		f.Attempts++
		f.AttemptState = append(f.AttemptState, s)
		f.AttemptKind = append(f.AttemptKind, k)
	}
	rows.Close()

	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(source),'') FROM inference_usage_records
	                       WHERE request_id = $1`, requestID).Scan(&f.UsageRows, &f.UsageSource); err != nil {
		t.Fatal(err)
	}
	if f.UsageRows > 0 {
		if err := db.QueryRow(`SELECT input_tokens, output_tokens FROM inference_usage_records
		                       WHERE request_id = $1 ORDER BY recorded_at DESC LIMIT 1`, requestID).
			Scan(&f.InputTokens, &f.OutputTokens); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(amount_micros),0) FROM inference_ledger_entries
	                       WHERE request_id = $1 AND entry_type = 'charge'`, requestID).
		Scan(&f.ChargeRows, &f.ChargeAmount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),
	                          COUNT(*) FILTER (WHERE state='held'),
	                          COUNT(*) FILTER (WHERE state='settled'),
	                          COUNT(*) FILTER (WHERE state='released')
	                       FROM inference_reservations WHERE request_id = $1`, requestID).
		Scan(&f.ResTotal, &f.ResHeld, &f.ResSettled, &f.ResReleased); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),
	                          COUNT(*) FILTER (WHERE state='reserved'),
	                          COUNT(*) FILTER (WHERE state='settled'),
	                          COUNT(*) FILTER (WHERE state='released')
	                       FROM inference_rate_reservations WHERE request_id = $1`, requestID).
		Scan(&f.RateRows, &f.RateReserved, &f.RateSettled, &f.RateReleased); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE state='held')
	                       FROM inference_concurrency_leases WHERE request_id = $1`, requestID).
		Scan(&f.LeaseTotal, &f.LeaseHeld); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1`,
		requestID).Scan(&f.Jobs); err != nil {
		t.Fatal(err)
	}
	if f.Jobs > 0 {
		if err := db.QueryRow(`SELECT reason, status FROM inference_reconciliation_jobs
		                       WHERE request_id = $1 ORDER BY created_at DESC LIMIT 1`, requestID).
			Scan(&f.JobReason, &f.JobStatus); err != nil {
			t.Fatal(err)
		}
	}
	used, err := db.Query(`SELECT used_micros FROM inference_quota_windows
	                       WHERE entitlement_id = $1 ORDER BY kind`, accEntitlement(t, db, requestID))
	if err != nil {
		t.Fatal(err)
	}
	for used.Next() {
		var v int64
		if err := used.Scan(&v); err != nil {
			t.Fatal(err)
		}
		f.WindowUsed = append(f.WindowUsed, v)
	}
	used.Close()
	return f
}

// st_Entitlement 取出请求绑定的权益（窗口聚合按权益分组）。
func accEntitlement(t *testing.T, db *sqlx.DB, requestID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT entitlement_id FROM inference_requests WHERE id = $1`, requestID).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// onlyRequestID 返回库里唯一的请求 ID（验收场景每次只有一个逻辑请求）。
func onlyRequestID(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM inference_requests ORDER BY created_at DESC LIMIT 1`).
		Scan(&id); err != nil {
		t.Fatalf("no request row: %v", err)
	}
	return id
}

// countRows 统计某表行数（断言"零痕迹"用）。
func countRows(t *testing.T, db *sqlx.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// assertNoTrace 断言一次被拒绝的请求没有留下任何持久化痕迹。
func assertNoTrace(t *testing.T, db *sqlx.DB, what string) {
	t.Helper()
	for _, q := range []string{
		`SELECT COUNT(*) FROM inference_requests`,
		`SELECT COUNT(*) FROM inference_attempts`,
		`SELECT COUNT(*) FROM inference_usage_records`,
		`SELECT COUNT(*) FROM inference_ledger_entries`,
		`SELECT COUNT(*) FROM inference_reservations`,
		`SELECT COUNT(*) FROM inference_rate_reservations`,
		`SELECT COUNT(*) FROM inference_concurrency_leases`,
		`SELECT COUNT(*) FROM inference_reconciliation_jobs`,
	} {
		if n := countRows(t, db, q); n != 0 {
			t.Errorf("%s left %d row(s) in %s (a rejected request must leave no trace)", what, n, q)
		}
	}
}

// ---------------------------------------------------------------------------
// 场景 1：成功 —— 四个入口各自结算一次，事实自洽
// ---------------------------------------------------------------------------

func TestAcceptance_SuccessSettlesExactlyOncePerEntrypoint(t *testing.T) {
	for _, e := range newAcceptanceStack(t).entries() {
		e := e
		t.Run(e.name, func(t *testing.T) {
			st := newAcceptanceStack(t)
			w := st.call(t, e)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
			}
			id := onlyRequestID(t, st.db)
			f := readAccFacts(t, st.db, id)

			if !f.Found {
				t.Fatal("no request row")
			}
			if f.Protocol != e.protocol {
				t.Errorf("protocol = %q, want %q", f.Protocol, e.protocol)
			}
			if f.Status != "settled" {
				t.Errorf("status = %q, want settled", f.Status)
			}
			if f.UsageStatus != "reported" {
				t.Errorf("usage_status = %q, want reported", f.UsageStatus)
			}
			if f.Settled != accChargeMicros {
				t.Errorf("settled_micros = %d, want %d", f.Settled, accChargeMicros)
			}
			if f.Attempts != 1 || f.AttemptState[0] != "completed" {
				t.Errorf("attempts = %d %v, want 1 completed", f.Attempts, f.AttemptState)
			}
			if f.UsageRows != 1 || f.UsageSource != "reported" {
				t.Errorf("usage records = %d (%q), want 1 reported", f.UsageRows, f.UsageSource)
			}
			if !f.InputTokens.Valid || f.InputTokens.Int64 != accInputTokens ||
				!f.OutputTokens.Valid || f.OutputTokens.Int64 != accOutputTokens {
				t.Errorf("usage tokens = in %v out %v, want %d/%d",
					f.InputTokens, f.OutputTokens, accInputTokens, accOutputTokens)
			}
			if f.ChargeRows != 1 || f.ChargeAmount != accChargeMicros {
				t.Errorf("ledger charges = %d (%d micro), want exactly 1 × %d",
					f.ChargeRows, f.ChargeAmount, accChargeMicros)
			}
			if f.Settled != f.ChargeAmount {
				t.Errorf("settled_micros %d != ledger charge %d", f.Settled, f.ChargeAmount)
			}
			// 预占：三个金额窗口（+ 客户 Key 预算，仅带 Key 的入口）。
			wantRes := 3
			if e.hasKey {
				wantRes = 4
			}
			if f.ResTotal != wantRes {
				t.Errorf("reservations = %d, want %d", f.ResTotal, wantRes)
			}
			if f.ResHeld != 0 {
				t.Errorf("held reservations = %d, want 0 (a settled request holds nothing)", f.ResHeld)
			}
			if f.ResSettled != wantRes {
				t.Errorf("settled reservations = %d, want %d", f.ResSettled, wantRes)
			}
			// 速率预占：账户级 + （带 Key 的入口）Key 级，都必须已终结。
			wantRate := 1
			if e.hasKey {
				wantRate = 2
			}
			if f.RateRows != wantRate {
				t.Errorf("rate reservations = %d, want %d", f.RateRows, wantRate)
			}
			if f.RateReserved != 0 {
				t.Errorf("reserved rate rows = %d, want 0", f.RateReserved)
			}
			if f.RateSettled != wantRate {
				t.Errorf("settled rate rows = %d, want %d", f.RateSettled, wantRate)
			}
			// 并发租约：请求结束即释放。
			if f.LeaseHeld != 0 {
				t.Errorf("held leases = %d, want 0", f.LeaseHeld)
			}
			if f.Jobs != 0 {
				t.Errorf("reconciliation jobs = %d, want 0", f.Jobs)
			}
			// 窗口聚合：一次消费同时记入三个窗口，且与账本一致。
			if len(f.WindowUsed) != 3 {
				t.Errorf("windows = %d, want 3", len(f.WindowUsed))
			}
			for i, v := range f.WindowUsed {
				if v != accChargeMicros {
					t.Errorf("window %d used = %d, want %d", i, v, accChargeMicros)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 场景 2：拒绝 —— 准入前拒绝，零预占、零出站、零账本
// ---------------------------------------------------------------------------

func TestAcceptance_RejectionLeavesNoTrace(t *testing.T) {
	for _, e := range newAcceptanceStack(t).entries() {
		e := e
		t.Run(e.name, func(t *testing.T) {
			st := newAcceptanceStack(t)
			// 换一个权益不授予的模型 → 准入前拒绝。
			bad := e
			bad.body = strings.Replace(e.body, e.model(), accUnentitled, 1)
			w := st.call(t, bad)
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 body = %s", w.Code, w.Body.String())
			}
			assertNoTrace(t, st.db, "an unentitled model")
			if st.openaiUp.count() != 0 || st.anthUp.count() != 0 {
				t.Errorf("upstream calls = %d/%d, want 0/0 (a pre-admission rejection must not dispatch)",
					st.openaiUp.count(), st.anthUp.count())
			}
		})
	}
}

// model 从入口请求体里取出模型 id（用于替换成未授权模型）。
func (e accEntry) model() string {
	i := strings.Index(e.body, `"model":"`)
	if i < 0 {
		return ""
	}
	rest := e.body[i+len(`"model":"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// ---------------------------------------------------------------------------
// 场景 3：断流 —— 上游半截结束（无终止标记）
// ---------------------------------------------------------------------------

func TestAcceptance_BrokenStreamParksWithoutSilentZero(t *testing.T) {
	for _, e := range newAcceptanceStack(t).entries() {
		e := e
		t.Run(e.name, func(t *testing.T) {
			st := newAcceptanceStack(t)
			// OpenAI 口径：发半截内容后直接结束（无 [DONE]、无用量块）。
			st.openaiUp.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
				if strings.Contains(string(body), `"stream":true`) {
					writeOpenAIChunks(w,
						`{"id":"chatcmpl-brk","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"par"}}]}`)
					return
				}
				accJSONOpenAI(w, r, body)
			})
			// Anthropic 口径：message_start（已报 input）后直接结束（无
			// message_stop、无 output 桶）——已读到的部分按 estimated 结算，
			// 未报到的桶保持 NULL（NULL ≠ 0）。
			st.anthUp.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
				if !strings.Contains(string(body), `"stream":true`) {
					accJSONAnthropic(w, r, body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_brk\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"up\",\"content\":[],\"usage\":{\"input_tokens\":7}}}\n\n")
			})
			w := st.call(t, e)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
			}
			id := onlyRequestID(t, st.db)
			f := readAccFacts(t, st.db, id)

			if e.protocol == "anthropic_messages" {
				// 已读到用量：按已读量结算（estimated），未报到的桶为 NULL。
				if f.Status != "settled" {
					t.Errorf("status = %q, want settled (usage was reported before the break)", f.Status)
				}
				if f.UsageStatus != "estimated" {
					t.Errorf("usage_status = %q, want estimated", f.UsageStatus)
				}
				if !f.InputTokens.Valid || f.InputTokens.Int64 != accInputTokens {
					t.Errorf("input tokens = %v, want %d", f.InputTokens, accInputTokens)
				}
				if f.OutputTokens.Valid {
					t.Errorf("output tokens = %v, want NULL (未报到的桶不得记零)", f.OutputTokens)
				}
				if f.ChargeRows != 1 {
					t.Errorf("ledger charges = %d, want 1", f.ChargeRows)
				}
				if f.ResHeld != 0 {
					t.Errorf("held reservations = %d, want 0 (a settled request holds nothing)", f.ResHeld)
				}
				if f.Jobs != 0 {
					t.Errorf("reconciliation jobs = %d, want 0", f.Jobs)
				}
			} else {
				// 未读到任何用量：停放对账（不记零、不静默释放）。
				if f.Status != "reconciliation_required" {
					t.Errorf("status = %q, want reconciliation_required (a broken stream is not a completed answer)", f.Status)
				}
				if f.UsageStatus != "unknown" {
					t.Errorf("usage_status = %q, want unknown (未知用量不得静默记零)", f.UsageStatus)
				}
				if f.ChargeRows != 0 {
					t.Errorf("ledger charges = %d, want 0 (a broken stream has no settled charge yet)", f.ChargeRows)
				}
				if f.Jobs == 0 || f.JobReason != "unknown_usage" {
					t.Errorf("reconciliation jobs = %d (%q), want ≥1 unknown_usage", f.Jobs, f.JobReason)
				}
				// 预占保留（不得静默释放成免费请求）。
				if f.ResHeld == 0 {
					t.Errorf("held reservations = %d, want >0 (the hold must survive for reconciliation)", f.ResHeld)
				}
			}
			if f.LeaseHeld != 0 {
				t.Errorf("held leases = %d, want 0 (the request ended: its slots must be back)", f.LeaseHeld)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 场景 4：中断 —— 客户端在流中途断开
// ---------------------------------------------------------------------------

// accFailWriter 在写满 limit 字节后失败，模拟客户端断开。
type accFailWriter struct {
	header http.Header
	buf    strings.Builder
	limit  int
	failed bool
}

func (w *accFailWriter) Header() http.Header { return w.header }
func (w *accFailWriter) WriteHeader(int)     {}
func (w *accFailWriter) Write(p []byte) (int, error) {
	if w.failed || w.buf.Len()+len(p) > w.limit {
		w.failed = true
		return 0, errors.New("client gone")
	}
	return w.buf.Write(p)
}
func (w *accFailWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *accFailWriter) Flush()                            {}

func TestAcceptance_ClientInterruptionSettlesAndReleases(t *testing.T) {
	for _, e := range newAcceptanceStack(t).entries() {
		e := e
		t.Run(e.name, func(t *testing.T) {
			st := newAcceptanceStack(t)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			// OpenAI 口径上游：发一段内容后保持连接打开（客户端此时断开）。
			st.openaiUp.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
				if strings.Contains(string(body), `"stream":true`) {
					writeOpenAIChunks(w,
						`{"id":"chatcmpl-cut","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`)
					select {
					case <-release:
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
					return
				}
				accJSONOpenAI(w, r, body)
			})
			// Anthropic 口径上游：message_start（已报 input/output）+ 一段内容后保持
			// 连接打开 —— 已读到的用量按 estimated 结算。
			st.anthUp.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
				if !strings.Contains(string(body), `"stream":true`) {
					accJSONAnthropic(w, r, body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl, _ := w.(http.Flusher)
				for _, ev := range []string{
					"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cut\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"up\",\"content\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n",
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
				} {
					_, _ = io.WriteString(w, ev)
					if fl != nil {
						fl.Flush()
					}
				}
				select {
				case <-release:
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
			})

			req := httptest.NewRequest(http.MethodPost, e.path, strings.NewReader(e.body))
			if e.hasKey {
				req.Header.Set("Authorization", "Bearer "+st.keyPlain)
			}
			w := &accFailWriter{header: http.Header{}, limit: 8}
			st.engine.ServeHTTP(w, req)

			id := onlyRequestID(t, st.db)
			f := readAccFacts(t, st.db, id)
			if e.protocol == "anthropic_messages" {
				// 已读到 input/output：按已读量结算（estimated），不得丢弃初始 output。
				if f.Status != "settled" {
					t.Errorf("status = %q, want settled (usage was read before the client left)", f.Status)
				}
				if f.UsageStatus != "estimated" {
					t.Errorf("usage_status = %q, want estimated", f.UsageStatus)
				}
				if !f.InputTokens.Valid || f.InputTokens.Int64 != accInputTokens {
					t.Errorf("input tokens = %v, want %d", f.InputTokens, accInputTokens)
				}
				if !f.OutputTokens.Valid || f.OutputTokens.Int64 != 1 {
					t.Errorf("output tokens = %v, want 1 from message_start", f.OutputTokens)
				}
				if f.ChargeRows != 1 {
					t.Errorf("ledger charges = %d, want 1", f.ChargeRows)
				}
				if f.ResHeld != 0 {
					t.Errorf("held reservations = %d, want 0 (a settled request holds nothing)", f.ResHeld)
				}
				if f.Jobs != 0 {
					t.Errorf("reconciliation jobs = %d, want 0", f.Jobs)
				}
			} else {
				// 未读到任何用量：停放对账（不记零、不静默释放）。
				if f.Status != "reconciliation_required" {
					t.Errorf("status = %q, want reconciliation_required", f.Status)
				}
				if f.UsageStatus != "unknown" {
					t.Errorf("usage_status = %q, want unknown", f.UsageStatus)
				}
				if f.ChargeRows != 0 {
					t.Errorf("ledger charges = %d, want 0", f.ChargeRows)
				}
				if f.Jobs == 0 {
					t.Errorf("reconciliation jobs = %d, want ≥1", f.Jobs)
				}
				if f.ResHeld == 0 {
					t.Errorf("held reservations = %d, want >0", f.ResHeld)
				}
			}
			if f.LeaseHeld != 0 {
				t.Errorf("held leases = %d, want 0 (an interrupted request must give its slots back)", f.LeaseHeld)
			}
			if f.Attempts != 1 || f.AttemptState[0] != "cancelled" {
				t.Errorf("attempts = %d %v, want 1 cancelled", f.Attempts, f.AttemptState)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 场景 5：重试 / 失败切换 —— 上游 429 后换候选，只结算一次
// ---------------------------------------------------------------------------

func TestAcceptance_FailoverAfterRetrySettlesOnce(t *testing.T) {
	st := newAcceptanceStack(t)
	// 主上游一律 429（可重试 → 冷却并切换到下一个候选）。
	st.openaiUp.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	// 备份腿（priority 更高 → 后试）健康。
	backup, _ := st.addOpenAILeg(t, "acc-prov-backup", 2, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			accSSEOpenAI(w, r, body)
			return
		}
		accJSONOpenAI(w, r, body)
	})

	e := st.entries()[1] // /v1/chat/completions
	w := st.call(t, e)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if st.openaiUp.count() != 1 || backup.count() != 1 {
		t.Errorf("upstream calls = %d/%d, want 1/1 (one 429 then failover)",
			st.openaiUp.count(), backup.count())
	}
	id := onlyRequestID(t, st.db)
	f := readAccFacts(t, st.db, id)
	if f.Attempts != 2 {
		t.Errorf("attempts = %d %v, want 2 (one 429 + one success)", f.Attempts, f.AttemptState)
	}
	if f.AttemptState[0] != "failed" || f.AttemptState[1] != "completed" {
		t.Errorf("attempt states = %v, want [failed completed]", f.AttemptState)
	}
	if f.ChargeRows != 1 || f.ChargeAmount != accChargeMicros {
		t.Errorf("ledger charges = %d (%d), want exactly 1 × %d — 一次逻辑请求只结算一次",
			f.ChargeRows, f.ChargeAmount, accChargeMicros)
	}
	if f.UsageRows != 1 {
		t.Errorf("usage records = %d, want 1 (the failed attempt carries no usage)", f.UsageRows)
	}
	if f.Settled != accChargeMicros {
		t.Errorf("settled_micros = %d, want %d", f.Settled, accChargeMicros)
	}
	if f.ResHeld != 0 || f.LeaseHeld != 0 {
		t.Errorf("held reservations = %d, held leases = %d, want 0/0", f.ResHeld, f.LeaseHeld)
	}
	if f.Jobs != 0 {
		t.Errorf("reconciliation jobs = %d, want 0 (a 429 failover is a definite zero-consumption failure)", f.Jobs)
	}
}

// ---------------------------------------------------------------------------
// 场景 6：未知执行 —— 上游 200 但响应体损坏 → 停放对账，不重放
// ---------------------------------------------------------------------------

func TestAcceptance_UnknownExecutionParksWithoutReplay(t *testing.T) {
	st := newAcceptanceStack(t)
	// 主上游挂死（超过部署的请求总超时）→ 传输超时 = 上游是否执行未知。
	st.openaiUp.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-time.After(30 * time.Second):
		case <-r.Context().Done():
		}
	})
	// 备份腿健康：若网关盲目重放，这里会被调用（双倍消耗）。
	backup, _ := st.addOpenAILeg(t, "acc-prov-backup", 2, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			accSSEOpenAI(w, r, body)
			return
		}
		accJSONOpenAI(w, r, body)
	})
	st.setDeploymentTimeout(st.openaiDepID, 300*time.Millisecond)

	e := st.entries()[1]
	w := st.call(t, e)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 body = %s", w.Code, w.Body.String())
	}
	if st.openaiUp.count() != 1 {
		t.Errorf("primary upstream calls = %d, want 1", st.openaiUp.count())
	}
	if backup.count() != 0 {
		t.Errorf("backup upstream calls = %d, want 0 — 未知执行绝不盲目重放（双倍消耗）", backup.count())
	}
	id := onlyRequestID(t, st.db)
	f := readAccFacts(t, st.db, id)
	if f.Status != "reconciliation_required" {
		t.Errorf("status = %q, want reconciliation_required", f.Status)
	}
	if f.UsageStatus != "unknown" {
		t.Errorf("usage_status = %q, want unknown", f.UsageStatus)
	}
	if f.Attempts != 1 || f.AttemptState[0] != "unknown" || f.AttemptKind[0] != "transport" {
		t.Errorf("attempts = %d %v/%v, want a single unknown/transport attempt", f.Attempts, f.AttemptState, f.AttemptKind)
	}
	if f.ChargeRows != 0 {
		t.Errorf("ledger charges = %d, want 0", f.ChargeRows)
	}
	if f.ResHeld == 0 {
		t.Errorf("held reservations = %d, want >0 (the hold must survive)", f.ResHeld)
	}
	if f.Jobs == 0 {
		t.Errorf("reconciliation jobs = %d, want ≥1", f.Jobs)
	}
	if f.LeaseHeld != 0 {
		t.Errorf("held leases = %d, want 0", f.LeaseHeld)
	}
}

// ---------------------------------------------------------------------------
// 场景 7：恢复 —— 停放的请求被恢复 worker 保守结算，事实自洽
// ---------------------------------------------------------------------------

func TestAcceptance_RecoverySettlesParkedRequestOnce(t *testing.T) {
	st := newAcceptanceStack(t)
	// 主上游挂死 → 未知执行 → 停放对账。
	st.openaiUp.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-time.After(30 * time.Second):
		case <-r.Context().Done():
		}
	})
	backup, _ := st.addOpenAILeg(t, "acc-prov-backup", 2, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			accSSEOpenAI(w, r, body)
			return
		}
		accJSONOpenAI(w, r, body)
	})
	st.setDeploymentTimeout(st.openaiDepID, 300*time.Millisecond)

	e := st.entries()[1]
	if w := st.call(t, e); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 body = %s", w.Code, w.Body.String())
	}
	id := onlyRequestID(t, st.db)
	before := readAccFacts(t, st.db, id)
	if before.Status != "reconciliation_required" {
		t.Fatalf("status = %q, want reconciliation_required before recovery", before.Status)
	}
	hold := before.ResHeld
	if hold == 0 {
		t.Fatal("the parked request must keep its hold before recovery")
	}

	// “新进程”：恢复 worker，时钟 +2h（越过 grace）。
	worker := workers.NewSettlementRecovery(st.store, domain.FixedClock{T: time.Now().UTC().Add(2 * time.Hour)},
		workers.RecoveryConfig{Interval: time.Second, BatchLimit: 50, Grace: time.Hour,
			ReconciliationDeadline: 24 * time.Hour}, nil)
	stats, err := worker.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors != 0 {
		t.Errorf("recovery errors = %d, want 0", stats.Errors)
	}
	after := readAccFacts(t, st.db, id)
	if after.Status != "settled" {
		t.Errorf("status = %q, want settled after recovery", after.Status)
	}
	if after.UsageStatus != "estimated" {
		t.Errorf("usage_status = %q, want estimated (the real usage is unknown)", after.UsageStatus)
	}
	if after.ChargeRows != 1 {
		t.Errorf("ledger charges = %d, want exactly 1 after recovery", after.ChargeRows)
	}
	if after.ChargeAmount <= 0 {
		t.Errorf("recovery charge = %d, want > 0 (绝不静默免费)", after.ChargeAmount)
	}
	if after.Settled != after.ChargeAmount {
		t.Errorf("settled_micros %d != ledger charge %d", after.Settled, after.ChargeAmount)
	}
	if after.ResHeld != 0 {
		t.Errorf("held reservations = %d, want 0 after recovery", after.ResHeld)
	}
	if after.LeaseHeld != 0 {
		t.Errorf("held leases = %d, want 0 after recovery", after.LeaseHeld)
	}
	if backup.count() != 0 {
		t.Errorf("backup upstream calls = %d, want 0 (recovery never replays the upstream)", backup.count())
	}
	// 恢复幂等：第二个 pass 零效果（不会二次扣费）。
	if _, err := worker.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := readAccFacts(t, st.db, id)
	if again.ChargeRows != 1 {
		t.Errorf("ledger charges after the second pass = %d, want 1 (recovery must be idempotent)", again.ChargeRows)
	}
	if again.Settled != after.Settled {
		t.Errorf("settled_micros changed on the second pass: %d → %d", after.Settled, again.Settled)
	}
}
