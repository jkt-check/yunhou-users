// gateway_test.go — Task 8 验收矩阵:真实 PostgreSQL + 真实 httptest 上游。
//
// 覆盖:拆包 SSE、usage 位于末尾、非流式、工具调用、畸形事件、429/5xx 失
// 败切换、中途 EOF、客户端断开、慢上游(超时)。所有测试在真实库上断言
// 请求/尝试/用量/账本/窗口的持久化事实 —— skip 不算通过(测试环境契约)。

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/migrate"
	"github.com/yunhou/users/internal/model"
)

var testDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		os.Exit(m.Run())
	}
	migs, err := migrate.LoadFiles("../../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		os.Exit(1)
	}
	if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}
	testDSN = dsn
	db.Close()
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// staticSnapshot pins one hand-built catalog snapshot (loopback httptest
// upstreams can never survive the write-path URL validation — by design;
// the publish/validate path is covered by the catalog package tests).
type staticSnapshot struct{ snap *catalog.Snapshot }

func (s *staticSnapshot) Current(ctx context.Context) (*catalog.Snapshot, error) {
	return s.snap, nil
}

// upstream is a programmable httptest server: handler decides the response
// per attempt; calls counts requests.
type upstream struct {
	*httptest.Server
	handler atomic.Value // func(w http.ResponseWriter, r *http.Request, body []byte)
	calls   atomic.Int32
	bodies  chan []byte
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *upstream {
	t.Helper()
	u := &upstream{bodies: make(chan []byte, 16)}
	u.handler.Store(h)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.calls.Add(1)
		select {
		case u.bodies <- body:
		default:
		}
		u.handler.Load().(func(http.ResponseWriter, *http.Request, []byte))(w, r, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) set(h func(w http.ResponseWriter, r *http.Request, body []byte)) {
	u.handler.Store(h)
}

// fixture assembles the full gateway over a real DB with one model, one
// provider/deployment/route/account/credential chain pointing at up.
type fixture struct {
	db           *sqlx.DB
	store        *postgres.Store
	gateway      *Service
	routing      *routing.Service
	snap         *catalog.Snapshot
	up           *upstream
	userID       string
	accountID    string
	keyID        string
	entID        string
	modelID      string
	providerID   string
	deploymentID string
	accountUpID  string
	policyID     string
	priceID      string
}

type fixtureOpt func(f *fixture)

// streamSSE writes an OpenAI SSE stream in fragments (split mid-line across
// flushes — 拆包 SSE), with the usage chunk at the END and [DONE].
func sseHandler(chunks ...string) func(w http.ResponseWriter, r *http.Request, body []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "chatcmpl-1")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			// Split every chunk in the middle to simulate packet splits.
			mid := len(c) / 2
			_, _ = io.WriteString(w, c[:mid])
			if fl != nil {
				fl.Flush()
			}
			_, _ = io.WriteString(w, c[mid:])
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

const (
	chunkA = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"
	chunkB = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n"
	chunkC = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"},\"finish_reason\":\"stop\"}]}\n\n"
	// usage 位于末尾(include_usage 契约), 带 cache/reasoning 明细。
	chunkUsage = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":9}}}\n\n"
	chunkDone  = "data: [DONE]\n\n"
)

func newFixture(t *testing.T, up *upstream, opts ...fixtureOpt) *fixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`TRUNCATE
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

	ctx := context.Background()
	store := postgres.NewStore(db)
	f := &fixture{db: db, store: store, up: up, modelID: "glm-4.6"}

	// user + billing account
	f.userID = uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, f.userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	acct := &domain.BillingAccount{UserID: f.userID}
	if err := store.InsertBillingAccount(ctx, acct); err != nil {
		t.Fatalf("insert billing account: %v", err)
	}
	f.accountID = acct.ID

	// model row (FK target for requests/prices)
	if err := store.InsertModel(ctx, &domain.Model{
		ID: f.modelID, DisplayName: "GLM 4.6",
		ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}

	// provider + credential + upstream account + deployment + route
	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	f.providerID = prov.ID

	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	credSvc := credentials.NewService(vault, store, store)
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov.ID, "main", "api_key", "sk-upstream-test", "seed", nil)
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}

	upAccount := &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cv.ID, DisplayName: "pool-1",
		Status: domain.AccountActive, ConcurrencyLimit: 8,
	}
	if err := store.InsertUpstreamAccount(ctx, upAccount); err != nil {
		t.Fatalf("insert upstream account: %v", err)
	}
	f.accountUpID = upAccount.ID

	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-4.6-upstream",
		BaseURL: up.URL, Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	f.deploymentID = dep.ID

	route := &domain.ModelRoute{
		ModelID: f.modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		PoolStrategy: domain.PoolRoundRobin,
	}
	if err := store.InsertRoute(ctx, route); err != nil {
		t.Fatalf("insert route: %v", err)
	}

	// policy (three windows, generous limits) + entitlement + prices
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(1_000_000_000), WeeklyLimit: micro(10_000_000_000),
		MonthlyLimit: micro(100_000_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	f.policyID = pol.ID

	now := time.Now().Add(-time.Minute)
	ent, err := access.GrantFromPlan(access.PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: "sub-" + uuid.NewString(),
		ModelIDs: []string{f.modelID}, PolicyVersionID: pol.ID,
	}, now, now, nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	ent.BillingAccountID = acct.ID
	if err := store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
	f.entID = ent.ID

	pv := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: now,
	}
	if err := store.InsertPriceVersion(ctx, pv); err != nil {
		t.Fatalf("insert price: %v", err)
	}
	f.priceID = pv.ID
	// 采购价目(每次尝试的成本来源, cost_basis=reported/estimated).
	cost := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceUpstreamCost, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 500_000, OutputPerMtok: 1_000_000,
		Revision: 1, EffectiveFrom: now,
	}
	if err := store.InsertPriceVersion(ctx, cost); err != nil {
		t.Fatalf("insert cost price: %v", err)
	}

	// API key (budget hold target; authentication is the /v1 middleware's job,
	// the gateway receives the verified principal + key).
	key := &domain.APIKey{
		BillingAccountID: acct.ID, Name: "cli",
		Prefix: "yk-test-" + uuid.NewString()[:8],
	}
	if err := store.InsertAPIKey(ctx, key, "sha256:"+uuid.NewString()); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	f.keyID = key.ID

	for _, opt := range opts {
		opt(f)
	}

	// Static catalog snapshot pinned over the same rows.
	f.snap = &catalog.Snapshot{
		Revision: 1,
		Models: map[string]domain.Model{f.modelID: {
			ID: f.modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols:         []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
			InputModalities:   []string{"text"},
			OutputModalities:  []string{"text"},
			SupportsTools:     true,
			SupportsReasoning: true,
		}},
		Providers: map[string]domain.Provider{prov.ID: *prov},
		Deployments: map[string]domain.Deployment{f.deploymentID: {
			ID: f.deploymentID, ProviderID: prov.ID, UpstreamModel: "glm-4.6-upstream",
			BaseURL: up.URL, Protocol: domain.ProtocolOpenAIChat,
			ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
			Status: domain.DeploymentActive,
		}},
		RoutesByModel: map[string][]domain.ModelRoute{f.modelID: {*route}},
	}

	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}
	f.routing = routing.NewService(store, adapters, nil)
	quotaSvc := quota.NewService(store, nil)
	entResolver := access.NewEntitlementResolver(store, nil)
	f.gateway = NewService(&staticSnapshot{f.snap}, store, entResolver, quotaSvc, f.routing,
		credSvc, providers.NewHTTPClient(egress), egress, nil)
	return f
}

func micro(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

// principal is the verified caller the /v1 middleware would produce.
func (f *fixture) principal() (*domain.Principal, *domain.APIKey) {
	return &domain.Principal{
		Kind: domain.PrincipalAPIKey, BillingAccountID: f.accountID, APIKeyID: f.keyID,
	}, &domain.APIKey{ID: f.keyID, BillingAccountID: f.accountID, Status: domain.APIKeyActive}
}

func chatReq(stream bool, content string) *providers.ChatRequest {
	return &providers.ChatRequest{
		Model:  "glm-4.6",
		Stream: stream,
		Messages: []model.ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: content},
		},
	}
}

// drainStream reads the whole stream body and finishes it.
func drainStream(t *testing.T, o *Outcome, end StreamEnd) string {
	t.Helper()
	if o.Stream == nil {
		t.Fatal("outcome is not streaming")
	}
	out, err := io.ReadAll(o.Stream)
	if err != nil && end == EndCompleted {
		t.Fatalf("read stream: %v", err)
	}
	if err := o.Stream.Finish(end); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := o.Stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return string(out)
}

// requestRow loads the request's persisted terminal state.
func (f *fixture) requestRow(t *testing.T, id string) (status, usageStatus string, reserved, settled int64) {
	t.Helper()
	var res, set *int64
	if err := f.db.QueryRow(`SELECT status, usage_status, reserved_micros, settled_micros
		FROM inference_requests WHERE id = $1`, id).Scan(&status, &usageStatus, &res, &set); err != nil {
		t.Fatalf("load request: %v", err)
	}
	if res != nil {
		reserved = *res
	}
	if set != nil {
		settled = *set
	}
	return
}

// ledgerCharge returns the single charge amount for the request (or -1 when
// no charge row exists).
func (f *fixture) ledgerCharge(t *testing.T, requestID string) int64 {
	t.Helper()
	var amount int64
	err := f.db.QueryRow(`SELECT amount_micros FROM inference_ledger_entries
		WHERE request_id = $1 AND entry_type = 'charge'`, requestID).Scan(&amount)
	if err != nil {
		return -1
	}
	return amount
}

// windowTotals sums used/reserved over the entitlement's windows. NOTE:
// the SAME consumption mirrors into every applicable window (设计 §6), so
// summing used across windows multiplies it — use windowUsedEach to assert
// the per-window mirror.
func (f *fixture) windowTotals(t *testing.T) (used, reserved int64) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT COALESCE(SUM(used_micros),0), COALESCE(SUM(reserved_micros),0)
		FROM inference_quota_windows WHERE entitlement_id = $1 AND state = 'active'`, f.entID).
		Scan(&used, &reserved); err != nil {
		t.Fatalf("windows: %v", err)
	}
	return
}

// windowUsedEach asserts every active window carries exactly `want` used
// micros (the one-consumption-one-mirror rule).
func (f *fixture) windowUsedEach(t *testing.T, want int64) {
	t.Helper()
	rows, err := f.db.Query(`SELECT used_micros, reserved_micros FROM inference_quota_windows
		WHERE entitlement_id = $1 AND state = 'active'`, f.entID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var used, reserved int64
		if err := rows.Scan(&used, &reserved); err != nil {
			t.Fatal(err)
		}
		if used != want || reserved != 0 {
			t.Errorf("window used=%d reserved=%d, want %d/0 (每窗口镜像同一笔消费)", used, reserved, want)
		}
		n++
	}
	if n != 3 {
		t.Errorf("active windows = %d, want 3", n)
	}
}

func (f *fixture) attemptRows(t *testing.T, requestID string) []struct{ Status, Kind, UpstreamID string } {
	t.Helper()
	rows, err := f.db.Query(`SELECT status, error_kind, upstream_request_id FROM inference_attempts
		WHERE request_id = $1 ORDER BY attempt_no`, requestID)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	defer rows.Close()
	var out []struct{ Status, Kind, UpstreamID string }
	for rows.Next() {
		var r struct{ Status, Kind, UpstreamID string }
		if err := rows.Scan(&r.Status, &r.Kind, &r.UpstreamID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// 拆包 SSE + usage 位于末尾 + 完整 [DONE]:reported 结算,账本/窗口/尝试
// 全部落库,成本来源保留。
func TestStreamUsageAtEnd_SettlesReported(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkC, chunkUsage, chunkDone))
	f := newFixture(t, up)

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body := drainStream(t, out, EndCompleted)

	if !strings.Contains(body, `"content":"你好"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client stream missing content/[DONE]:\n%s", body)
	}
	status, usageStatus, reserved, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "reported" {
		t.Errorf("request = %s/%s, want settled/reported", status, usageStatus)
	}
	// 结算:输入 12×1 + 输出 34×2 = 80 micros(缓存读已在输入总价内,归一
	// 化后 input=12-4=8 按 1 价 + cache_read 4×0 + reasoning 不重复加算:
	// 8×1 + 4×0 + 34×2 = 76)。
	if charge := f.ledgerCharge(t, out.RequestID); charge != 76 {
		t.Errorf("charge = %d, want 76 (normalized buckets priced once)", charge)
	}
	if settled != 76 || reserved <= 0 {
		t.Errorf("settled=%d reserved=%d, want 76 and a positive hold", settled, reserved)
	}
	f.windowUsedEach(t, 76)
	attempts := f.attemptRows(t, out.RequestID)
	if len(attempts) != 1 || attempts[0].Status != "completed" || attempts[0].UpstreamID != "chatcmpl-1" {
		t.Errorf("attempts = %+v, want one completed attempt with upstream id", attempts)
	}
	// 尝试成本来源:reported usage → reported cost(采购价目 8×0.5 + 34×1
	// micromoney/token → 4+34=38 USD-micros,RoundDown)。
	var costMicros *int64
	var costBasis, costCurrency *string
	if err := f.db.QueryRow(`SELECT cost_micros, cost_currency, cost_basis FROM inference_attempts
		WHERE request_id = $1`, out.RequestID).Scan(&costMicros, &costCurrency, &costBasis); err != nil {
		t.Fatal(err)
	}
	if costMicros == nil || *costMicros != 38 || costBasis == nil || *costBasis != "reported" ||
		costCurrency == nil || *costCurrency != "USD" {
		t.Errorf("attempt cost = %v %v %v, want 38 USD reported", costMicros, costCurrency, costBasis)
	}
	// 计量事实:规范化桶 + 原始 usage 存档。
	var in, outT, cacheRead, reasoning *int64
	if err := f.db.QueryRow(`SELECT input_tokens, output_tokens, cache_read_tokens, reasoning_tokens
		FROM inference_usage_records WHERE request_id = $1`, out.RequestID).
		Scan(&in, &outT, &cacheRead, &reasoning); err != nil {
		t.Fatal(err)
	}
	if in == nil || *in != 8 || outT == nil || *outT != 34 {
		t.Errorf("usage buckets = %v/%v, want normalized 8/34", in, outT)
	}
	if cacheRead == nil || *cacheRead != 4 || reasoning == nil || *reasoning != 9 {
		t.Errorf("cache/reasoning = %v/%v, want 4/9", cacheRead, reasoning)
	}
}

// 非流式:原生 chat.completion 形状直通,usage 来自响应体。
func TestNonStream_SettlesAndPassesThrough(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"stream":true`) {
			t.Errorf("non-stream request must not ask for a stream: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ns","object":"chat.completion","created":1,"model":"glm-4.6-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	})
	f := newFixture(t, up)

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	if out.Stream != nil {
		t.Fatal("non-stream outcome must not carry a stream")
	}
	if !strings.Contains(string(out.Payload), `"content":"done"`) {
		t.Errorf("payload = %s", out.Payload)
	}
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "reported" {
		t.Errorf("request = %s/%s, want settled/reported", status, usageStatus)
	}
	if charge := f.ledgerCharge(t, out.RequestID); charge != 10*1+5*2 {
		t.Errorf("charge = %d, want 20", charge)
	}
	if settled != 20 {
		t.Errorf("settled = %d, want 20", settled)
	}
}

// 工具调用:工具 schema 原样转发上游;模型声明不支持工具时明确报错且
// 不产生任何上游调用/预占。
func TestTools_ForwardedAndCapabilityGated(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)

	req := chatReq(true, "list files")
	req.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell","parameters":{"type":"object"}}}`)}
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, req)
	if err != nil {
		t.Fatalf("ChatCompletions with tools: %v", err)
	}
	drainStream(t, out, EndCompleted)
	select {
	case body := <-up.bodies:
		if !strings.Contains(string(body), `"run_shell"`) {
			t.Errorf("upstream body missing tools: %s", body)
		}
	default:
		t.Error("upstream body not captured")
	}

	// 能力门:模型不支持工具 → invalid_input,零上游调用。
	f2snap := *f.snap
	m := f2snap.Models[f.modelID]
	m.SupportsTools = false
	f2snap.Models[f.modelID] = m
	f.gateway.snapshots = &staticSnapshot{&f2snap}
	callsBefore := up.calls.Load()
	_, err = f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, req)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("err = %v, want invalid_input (tools unsupported)", err)
	}
	if up.calls.Load() != callsBefore {
		t.Error("no upstream call may happen after a capability rejection")
	}
}

// 畸形事件:坏 JSON 行混入流中,客户端流与计量都不受影响。
func TestMalformedSSEEvents_Tolerated(t *testing.T) {
	up := newUpstream(t, sseHandler(
		chunkA,
		"data: {this is not json\n\n",
		": comment line\n\n",
		chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	if !strings.Contains(body, `"content":"你好"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream = %s", body)
	}
	if charge := f.ledgerCharge(t, out.RequestID); charge != 76 {
		t.Errorf("charge = %d, want 76 (usage parsed despite malformed events)", charge)
	}
}

// 429 失败切换:候选 1 被 429,候选 2 供流;两次尝试都落库,客户只结算一次;
// 不用重试绕过账号容量(失败切换走下一个候选)。
func TestFailoverOn429_SettlesOnce(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	up2 := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up1)
	// 第二个 provider/deployment/account/route,同模型,priority 更高(数字大者后试)。
	ctx := context.Background()
	prov2 := &domain.Provider{Code: "moonshot", DisplayName: "Moonshot", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := f.store.InsertProvider(ctx, prov2); err != nil {
		t.Fatal(err)
	}
	vault, _ := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	credSvc := credentials.NewService(vault, f.store, f.store)
	cv2, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"}, prov2.ID, "main", "api_key", "sk-2", "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct2 := &domain.UpstreamAccount{ProviderID: prov2.ID, CredentialID: cv2.ID, Status: domain.AccountActive, ConcurrencyLimit: 4}
	if err := f.store.InsertUpstreamAccount(ctx, acct2); err != nil {
		t.Fatal(err)
	}
	dep2 := &domain.Deployment{
		ProviderID: prov2.ID, UpstreamModel: "kimi-k2", BaseURL: up2.URL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second, Status: domain.DeploymentActive,
	}
	if err := f.store.InsertDeployment(ctx, dep2); err != nil {
		t.Fatal(err)
	}
	route2 := &domain.ModelRoute{ModelID: f.modelID, DeploymentID: dep2.ID, Priority: 2, Weight: 1, Enabled: true}
	if err := f.store.InsertRoute(ctx, route2); err != nil {
		t.Fatal(err)
	}
	// 既有 route priority 默认 0 → 先打 up1(429)。
	f.snap.Providers[prov2.ID] = *prov2
	f.snap.Deployments[dep2.ID] = *dep2
	f.snap.RoutesByModel[f.modelID] = append(f.snap.RoutesByModel[f.modelID], *route2)
	// gateway 持有的凭据 resolver 是第一个 fixture 的 credSvc — 两者底层同库,
	// 解析按 ID 读取,无需替换。

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream = %s", body)
	}
	if up1.calls.Load() != 1 || up2.calls.Load() != 1 {
		t.Errorf("calls = %d/%d, want 1/1 (one 429 then failover)", up1.calls.Load(), up2.calls.Load())
	}
	attempts := f.attemptRows(t, out.RequestID)
	if len(attempts) != 2 || attempts[0].Status != "failed" || attempts[0].Kind != "http_429" || attempts[1].Status != "completed" {
		t.Errorf("attempts = %+v, want [failed http_429, completed]", attempts)
	}
	if charge := f.ledgerCharge(t, out.RequestID); charge != 76 {
		t.Errorf("charge = %d, want exactly one settlement of 76", charge)
	}
	f.windowUsedEach(t, 76)
}

// 全部上游 5xx:有界重试后 upstream_unavailable;确认零消费 → 预占释放,
// 五小时窗口被安全撤销,无账本记录。
func TestAllUpstream5xx_ReleasesReservation(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	f := newFixture(t, up)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("err = %v, want upstream_unavailable", err)
	}
	if up.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (single candidate, bounded)", up.calls.Load())
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_requests WHERE status = 'released'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("released requests = %d err=%v, want 1", cnt, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_reservations WHERE state = 'released'`).Scan(&cnt); err != nil || cnt != 4 {
		t.Errorf("released reservations = %d, want 4 (3 windows + key budget)", cnt)
	}
	// 未使用的五小时窗口在事务内安全撤销(设计 §6)。
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_quota_windows WHERE state = 'voided'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Errorf("voided windows = %d, want 1 (unused five-hour window)", cnt)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_ledger_entries`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("ledger entries = %d, want 0", cnt)
	}
	attempts := f.attemptRows(t, mustRequestID(t, f.db))
	if len(attempts) != 1 || attempts[0].Status != "failed" || attempts[0].Kind != "http_500" {
		t.Errorf("attempts = %+v", attempts)
	}
}

func mustRequestID(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM inference_requests ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// 中途 EOF(无 [DONE])但 usage 已读:中断语义 —— 已读用量保留,
// estimated 结算,不重复扣、不记零。
func TestMidStreamEOF_EstimatedSettlementKeepsReadUsage(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage)) // 无 [DONE]
	f := newFixture(t, up)
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body, err := io.ReadAll(out.Stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Stream.Terminal() {
		t.Fatal("Terminal = true without [DONE], want false (interrupted)")
	}
	if !strings.Contains(string(body), chunkUsage[:20]) {
		t.Errorf("stream must pass the usage chunk through: %s", body)
	}
	if err := out.Stream.Finish(EndUpstreamBroke); err != nil {
		t.Fatal(err)
	}
	out.Stream.Close()
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "estimated" {
		t.Errorf("request = %s/%s, want settled/estimated (已读用量保留)", status, usageStatus)
	}
	if settled != 76 {
		t.Errorf("settled = %d, want 76 (已读用量按已知实际量结算)", settled)
	}
	attempts := f.attemptRows(t, out.RequestID)
	if len(attempts) != 1 || attempts[0].Status != "failed" || attempts[0].Kind != "stream_interrupted" {
		t.Errorf("attempts = %+v, want failed/stream_interrupted", attempts)
	}
	// estimated 成本来源。
	var basis *string
	if err := f.db.QueryRow(`SELECT cost_basis FROM inference_attempts WHERE request_id = $1`, out.RequestID).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if basis == nil || *basis != "estimated" {
		t.Errorf("cost basis = %v, want estimated", basis)
	}
}

// 中途 EOF 且毫无 usage:不记零、不静默释放 —— 预占保留,进入核对队列
// (恢复闭环归 Task 9)。
func TestMidStreamEOFNoUsage_ReconciliationHoldsReservation(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB)) // 无 usage 无 [DONE]
	f := newFixture(t, up)
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	_, _ = io.ReadAll(out.Stream)
	if err := out.Stream.Finish(EndUpstreamBroke); err != nil {
		t.Fatal(err)
	}
	out.Stream.Close()
	status, usageStatus, reserved, _ := f.requestRow(t, out.RequestID)
	if status != "reconciliation_required" {
		t.Errorf("status = %s, want reconciliation_required", status)
	}
	if usageStatus != "pending" {
		t.Errorf("usage_status = %s, want pending (never zeroed)", usageStatus)
	}
	if reserved <= 0 {
		t.Errorf("reserved = %d, want the hold retained", reserved)
	}
	_, res := f.windowTotals(t)
	if res <= 0 {
		t.Error("window reserved must stay held (禁止静默释放)")
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE status = 'pending'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Errorf("reconciliation jobs = %d, want 1", cnt)
	}
	if charge := f.ledgerCharge(t, out.RequestID); charge != -1 {
		t.Errorf("charge = %d, want none (未知不计费)", charge)
	}
}

// 客户端断开:停止上游连接,已读用量保留并结算(设计 §7.2)。
func TestClientDisconnect_SettlesReadUsage(t *testing.T) {
	// 上游:先发一条带 usage 的 chunk(部分供应商流式累计上报),然后卡住
	// 直到请求 ctx 取消。
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, chunkB)
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done() // 卡住直到客户端断开/超时
	})
	f := newFixture(t, up)

	ctx, cancel := context.WithCancel(context.Background())
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := out.Stream.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("first read = %d, %v", n, err)
	}
	cancel() // 客户端断开
	// 传输层可能还有缓冲字节;读到出错为止(取消后必然快速失败)。
	var readErr error
	for i := 0; i < 16; i++ {
		if _, readErr = out.Stream.Read(buf); readErr != nil {
			break
		}
	}
	if readErr == nil {
		t.Fatal("read after client cancel must fail")
	}
	if err := out.Stream.Finish(EndClientGone); err != nil {
		t.Fatal(err)
	}
	out.Stream.Close()
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "estimated" {
		t.Errorf("request = %s/%s, want settled/estimated (已读用量保留并结算)", status, usageStatus)
	}
	// 已读用量 5×1 + 2×2 = 9。
	if settled != 9 {
		t.Errorf("settled = %d, want 9", settled)
	}
	attempts := f.attemptRows(t, out.RequestID)
	if len(attempts) != 1 || attempts[0].Status != "cancelled" || attempts[0].Kind != "client_gone" {
		t.Errorf("attempts = %+v, want cancelled/client_gone", attempts)
	}
}

// 慢上游(超过部署 RequestTimeout):执行状态未知 → 预占保留进核对,
// 不静默释放、不计费。
func TestSlowUpstream_TimeoutHoldsForReconciliation(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
	})
	f := newFixture(t, up)
	// 收紧部署超时。
	dep := f.snap.Deployments[f.deploymentID]
	dep.RequestTimeout = 300 * time.Millisecond
	f.snap.Deployments[f.deploymentID] = dep

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("err = %v, want upstream_unavailable", err)
	}
	status, _, reserved, _ := f.requestRow(t, mustRequestID(t, f.db))
	if status != "reconciliation_required" || reserved <= 0 {
		t.Errorf("status=%s reserved=%d, want reconciliation_required with the hold retained", status, reserved)
	}
	attempts := f.attemptRows(t, mustRequestID(t, f.db))
	if len(attempts) != 1 || attempts[0].Status != "unknown" || attempts[0].Kind != "transport" {
		t.Errorf("attempts = %+v, want unknown/transport", attempts)
	}
}

// 额度不足:Admit 阻断,429 语义由 API 层映射;无尝试、无上游调用。
func TestQuotaExceeded_BlocksBeforeDispatch(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkUsage, chunkDone))
	f := newFixture(t, up)
	// 把五小时窗口限额压到远小于预占上界。
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 10 WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	var qe *domain.QuotaExceededError
	if !errorsAsQuota(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if qe.DeficitMicros == nil || *qe.DeficitMicros <= 0 {
		t.Errorf("deficit = %v, want a positive 缺口信息", qe.DeficitMicros)
	}
	if up.calls.Load() != 0 {
		t.Error("no upstream call may happen when admission is rejected")
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_requests`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("requests = %d, want 0 (admission failed before persistence)", cnt)
	}
}

// 无权益:显式模型集合之外 → model_not_allowed(拒绝继承 NULL 全放行)。
func TestNoEntitlement_ModelNotAllowed(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkUsage, chunkDone))
	f := newFixture(t, up)
	if _, err := f.db.Exec(`DELETE FROM inference_entitlements`); err != nil {
		t.Fatal(err)
	}
	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("err = %v, want model_not_allowed", err)
	}
	if up.calls.Load() != 0 {
		t.Error("no upstream call without entitlement")
	}
}

// 策略并发上限:账户级租约在预占同事务获取,请求结束后释放(Task 7 消费点)。
func TestConcurrencyLease_AcquiredAndReleased(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkUsage, chunkDone))
	f := newFixture(t, up)
	conc := 1
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET concurrency_limit = $1 WHERE id = $2`, conc, f.policyID); err != nil {
		t.Fatal(err)
	}
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	drainStream(t, out, EndCompleted)
	// 两个租约:计费账户(策略并发) + 上游账号(每次尝试),都应已释放。
	var scope string
	var cnt int
	rows, err := f.db.Query(`SELECT scope, state FROM inference_concurrency_leases`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]int{}
	for rows.Next() {
		var st string
		if err := rows.Scan(&scope, &st); err != nil {
			t.Fatal(err)
		}
		states[scope+":"+st]++
		cnt++
	}
	if cnt != 2 {
		t.Errorf("leases = %d, want 2 (billing_account + upstream_account)", cnt)
	}
	if states["billing_account:released"] != 1 || states["upstream_account:released"] != 1 {
		t.Errorf("lease states = %v, want both released after the request", states)
	}
}

// Anthropic 上游:流式翻译成 OpenAI 形状,usage 来自 message_delta,正常结算。
func TestAnthropicUpstream_StreamTranslatedAndSettled(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(string(body), `"max_tokens"`) || strings.Contains(string(body), `"messages":[{"role":"system"`) {
			t.Errorf("anthropic payload malformed: %s", body)
		}
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic auth headers missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, ev := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":25}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":17}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = io.WriteString(w, ev)
			if fl != nil {
				fl.Flush()
			}
		}
	})
	f := newFixture(t, up)
	// 部署协议切换为 anthropic_messages(DB 行与快照同步)。
	if _, err := f.db.Exec(`UPDATE inference_deployments SET protocol = 'anthropic_messages' WHERE id = $1`, f.deploymentID); err != nil {
		t.Fatal(err)
	}
	dep := f.snap.Deployments[f.deploymentID]
	dep.Protocol = domain.ProtocolAnthropicMessage
	f.snap.Deployments[f.deploymentID] = dep

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body := drainStream(t, out, EndCompleted)
	// 客户端看到的是 OpenAI chunk 形状 + [DONE]。
	if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("translated stream = %s", body)
	}
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "reported" {
		t.Errorf("request = %s/%s, want settled/reported", status, usageStatus)
	}
	// Anthropic input 不含 cache 桶(Inclusion 全 false):25×1 + 17×2 = 59。
	if settled != 59 {
		t.Errorf("settled = %d, want 59", settled)
	}
}

// Anthropic 非流式:响应翻译成 OpenAI chat.completion 形状。
func TestAnthropicUpstream_NonStreamTranslated(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_9","type":"message","role":"assistant","model":"kimi-k2",
			"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":7,"output_tokens":3}}`)
	})
	f := newFixture(t, up)
	if _, err := f.db.Exec(`UPDATE inference_deployments SET protocol = 'anthropic_messages' WHERE id = $1`, f.deploymentID); err != nil {
		t.Fatal(err)
	}
	dep := f.snap.Deployments[f.deploymentID]
	dep.Protocol = domain.ProtocolAnthropicMessage
	f.snap.Deployments[f.deploymentID] = dep

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	if !strings.Contains(string(out.Payload), `"object":"chat.completion"`) ||
		!strings.Contains(string(out.Payload), `"content":"done"`) ||
		!strings.Contains(string(out.Payload), `"prompt_tokens":7`) {
		t.Errorf("payload = %s", out.Payload)
	}
	_, _, _, settled := f.requestRow(t, out.RequestID)
	if settled != 7*1+3*2 {
		t.Errorf("settled = %d, want 13", settled)
	}
}

// 完整结束但上游确实不提供 usage(忽略 include_usage):estimated 路径,
// 不记零。
func TestStreamWithoutUsageChunk_EstimatedNotZero(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkDone)) // 完整结束,无 usage
	f := newFixture(t, up)
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	drainStream(t, out, EndCompleted)
	status, usageStatus, _, settled := f.requestRow(t, out.RequestID)
	if status != "settled" || usageStatus != "estimated" {
		t.Errorf("request = %s/%s, want settled/estimated", status, usageStatus)
	}
	if settled <= 0 {
		t.Errorf("settled = %d, want a positive estimate (缺失 usage 不记零)", settled)
	}
	var src string
	if err := f.db.QueryRow(`SELECT source FROM inference_usage_records WHERE request_id = $1`, out.RequestID).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "estimated" {
		t.Errorf("usage source = %s, want estimated", src)
	}
}

// errorsAsQuota 提取 QuotaExceededError(避免在测试里引入 errors 的噪音)。
func errorsAsQuota(err error, target **domain.QuotaExceededError) bool {
	for err != nil {
		if qe, ok := err.(*domain.QuotaExceededError); ok {
			*target = qe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
