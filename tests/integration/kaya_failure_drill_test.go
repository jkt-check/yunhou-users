package integration

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

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
)

// kaya_failure_drill_test.go — Task 16 故障演练（控制者决定 4），可重复运
// 行的演练测试（真实 PG + mock 上游；复用 Task 9 恢复 worker 与 Task 13
// 断流语义的既有基建）。场景矩阵：协议 / 并发 / 断流（有/无 usage）/
// 进程重启（恢复 worker）/ 数据库短暂不可用 / 上游额度耗尽。每个场景
// 结束跑统一终态不变量（drillInvariant）：所有请求有终态或明确的待核
// 对状态；终态请求不得残留 held 预占；账本与窗口聚合一枚。
//
// 实测记录见 docs/runbooks/kaya-coding-plan-rollout.md §故障演练。

// drillStack is one isolated drill environment (fresh wipe per scenario).
type drillStack struct {
	db        *sqlx.DB // 直连断言/种子通道（不经过故障代理）
	store     *postgres.Store
	engine    *gin.Engine
	upstream  *drillUpstream
	keyPlain  string
	accountID string
	keyID     string
	entID     string
	modelID   string
	policyID  string
	upAcctID  string
}

// drillUpstream serves a swappable handler (per-scenario script).
type drillUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	handler func(w http.ResponseWriter, body []byte)
}

func (u *drillUpstream) set(h func(w http.ResponseWriter, body []byte)) {
	u.mu.Lock()
	u.handler = h
	u.mu.Unlock()
}

// drillSSEOpenAI writes a normal OpenAI SSE stream (usage 7/5) — the
// "protocol" scenario's happy path.
func drillSSEOpenAI(w http.ResponseWriter, _ []byte) {
	writeOpenAIChunks(w,
		`{"id":"chatcmpl-d1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"id":"chatcmpl-d1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-d1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
		"[DONE]")
}

func newDrillStack(t *testing.T, fiveHourLimit int64) *drillStack {
	t.Helper()
	return newDrillStackDSN(t, fiveHourLimit, "")
}

// newDrillStackDSN builds the drill stack; dsn == "" 时网关/存储与断言共用
// 直连池，非空时网关/存储走 dsn 指定的池（故障演练经 TCP 代理），断言
// 通道始终直连。
func newDrillStackDSN(t *testing.T, fiveHourLimit int64, dsn string) *drillStack {
	t.Helper()
	db := setupDB(t)
	gwDB := db
	if dsn != "" {
		var err error
		gwDB, err = sqlx.Connect("postgres", dsn)
		if err != nil {
			t.Fatalf("gateway pool connect: %v", err)
		}
		t.Cleanup(func() { gwDB.Close() })
	}
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
	store := postgres.NewStore(gwDB)
	st := &drillStack{db: db, store: store, modelID: "drill-model"}

	up := &drillUpstream{}
	up.handler = drillSSEOpenAI
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		h := up.handler
		up.mu.Unlock()
		h(w, body)
	}))
	t.Cleanup(up.Close)
	st.upstream = up

	if err := store.InsertModel(ctx, &domain.Model{
		ID: st.modelID, DisplayName: "Drill", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatal(err)
	}
	prov := &domain.Provider{Code: "drill-prov", DisplayName: "drill",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "drill-up", BaseURL: up.URL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 10 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: st.modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	credSvc := credentials.NewService(vault, store, store)
	cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "drill"},
		prov.ID, "main", "api_key", "sk-drill", "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	upAcct := &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cv.ID, Status: domain.AccountActive, ConcurrencyLimit: 64,
	}
	if err := store.InsertUpstreamAccount(ctx, upAcct); err != nil {
		t.Fatal(err)
	}
	st.upAcctID = upAcct.ID

	pol := &postgres.PolicyVersion{
		Name: "drill", Revision: 1, ModelIDs: []string{st.modelID},
		FiveHourLimit: microc(fiveHourLimit), WeeklyLimit: microc(100 * fiveHourLimit),
		MonthlyLimit: microc(1000 * fiveHourLimit), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	st.policyID = pol.ID
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.InsertPriceVersion(ctx, &postgres.PriceVersion{
		ModelID: st.modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPriceVersion(ctx, &postgres.PriceVersion{
		ModelID: st.modelID, Kind: postgres.PriceUpstreamCost, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 500_000, OutputPerMtok: 1_000_000,
		Revision: 1, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	st.accountID = acct.ID
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{st.modelID},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	st.entID = ent.ID
	created, err := access.NewKeyService(store, nil).CreateKey(ctx, userID, access.CreateParams{Name: "drill"})
	if err != nil {
		t.Fatal(err)
	}
	st.keyPlain = created.Plaintext
	st.keyID = created.Key.ID

	snap := &catalog.Snapshot{
		Models:      map[string]domain.Model{st.modelID: mustGetModel(t, store, st.modelID)},
		Providers:   map[string]domain.Provider{prov.ID: *prov},
		Deployments: map[string]domain.Deployment{dep.ID: *dep},
		RoutesByModel: map[string][]domain.ModelRoute{st.modelID: {{
			ModelID: st.modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		}}},
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[domain.Protocol]providers.Adapter{domain.ProtocolOpenAIChat: providers.NewOpenAIChat()}
	routingSvc := routing.NewService(store, adapters, nil)
	gw := gateway.NewService(staticSnapshot{snap}, store,
		access.NewEntitlementResolver(store, nil), quota.NewService(store, nil),
		routingSvc, credSvc, providers.NewHTTPClient(egress), egress, nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	resolver := access.NewResolver(store, nil)
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver, access.NewRPMCounter(nil), 0))
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	st.engine = engine
	return st
}

func mustGetModel(t *testing.T, store *postgres.Store, id string) domain.Model {
	t.Helper()
	m, err := store.GetModel(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// drillCall fires one /v1/chat/completions (max_tokens=5 → hold 31 micro,
// settle 17 micro on the happy path) and returns status + body.
func (st *drillStack) drillCall(t *testing.T) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"drill-model","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+st.keyPlain)
	w := httptest.NewRecorder()
	st.engine.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// drillInvariant is the drill-wide acceptance sweep（每个场景结束都跑）：
//  1. 非终态请求必须有打开的核对任务（明确待核对，不得悬空）；
//  2. 终态请求不得残留 held 预占；
//  3. 账本重建聚合与窗口一枚（Task 9 不变量）。
func drillInvariant(t *testing.T, st *drillStack) {
	t.Helper()
	var orphans int
	if err := st.db.Get(&orphans, `
		SELECT COUNT(*) FROM inference_requests r
		 WHERE r.status NOT IN ('settled','released','failed')
		   AND NOT EXISTS (SELECT 1 FROM inference_reconciliation_jobs j
		                    WHERE j.request_id = r.id
		                      AND j.status IN ('pending','running','escalated'))`); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("non-terminal requests without an open reconciliation job: %d (悬空请求!)", orphans)
	}
	var stuckHeld int
	if err := st.db.Get(&stuckHeld, `
		SELECT COUNT(*) FROM inference_reservations res
		  JOIN inference_requests r ON r.id = res.request_id
		 WHERE res.state = 'held' AND r.status IN ('settled','released','failed')`); err != nil {
		t.Fatal(err)
	}
	if stuckHeld != 0 {
		t.Errorf("held reservations on terminal requests: %d (预占泄漏!)", stuckHeld)
	}
	recon, err := st.store.ReconcileWindowAggregates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recon {
		if r.Diff() != 0 {
			t.Errorf("window %s(%s): stored=%d rebuilt=%d — 账本/窗口不一致", r.WindowID, r.Kind, r.StoredUsed, r.RebuiltUsed)
		}
	}
}

// ---------------------------------------------------------------------------
// 场景 1 协议：正常 SSE 完成 → reported 结算（Task 13 深矩阵的演练抽样）
// ---------------------------------------------------------------------------

func TestDrill_ProtocolCleanSettle(t *testing.T) {
	st := newDrillStack(t, 1_000_000)
	code, body := st.drillCall(t)
	if code != http.StatusOK || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("clean call = %d %s", code, body)
	}
	var status, usage string
	var settled int64
	if err := st.db.QueryRow(
		`SELECT status, usage_status, settled_micros FROM inference_requests`).Scan(&status, &usage, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || usage != "reported" || settled != 17 {
		t.Errorf("request = %s/%s/%d, want settled/reported/17", status, usage, settled)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 2 并发：8 路并发抢小五小时窗口 → 账户共同封顶，无部分预占
// ---------------------------------------------------------------------------

func TestDrill_ConcurrencyAccountCapped(t *testing.T) {
	const limit = 100 // hold 31/次：约 3 个并发预占后封顶；settle 17/次
	st := newDrillStack(t, limit)

	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := st.drillCall(t)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	ok, rejected := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Fatalf("unexpected status %d under race", c)
		}
	}
	if ok == 0 || rejected == 0 || ok+rejected != 8 {
		t.Fatalf("race outcome: ok=%d rejected=%d (want 混合且合计 8)", ok, rejected)
	}
	// 账户共同封顶：五小时窗口 used+reserved ≤ limit；无部分预占。
	var used, reserved int64
	if err := st.db.QueryRow(
		`SELECT used_micros, reserved_micros FROM inference_quota_windows WHERE kind = 'five_hour'`).
		Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || used > limit {
		t.Errorf("five-hour window = used %d / reserved %d (cap %d) — 超封顶或部分预占", used, reserved, limit)
	}
	// 每个放行的请求恰结算一次；被拒请求零痕迹。
	var totalReqs, settledReqs int
	if err := st.db.QueryRow(
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'settled') FROM inference_requests`).
		Scan(&totalReqs, &settledReqs); err != nil {
		t.Fatal(err)
	}
	if totalReqs != ok || settledReqs != ok {
		t.Errorf("requests = %d settled = %d ok = %d (留痕不一致)", totalReqs, settledReqs, ok)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 3 断流（有 usage）：流中 EOF 但已读 usage → estimated 按已读实际量结算
// ---------------------------------------------------------------------------

func TestDrill_StreamAbortWithUsage(t *testing.T) {
	st := newDrillStack(t, 1_000_000)
	st.upstream.set(func(w http.ResponseWriter, _ []byte) {
		// 内容 + usage 之后无 [DONE] 直接 EOF（上游断流）。
		writeOpenAIChunks(w,
			`{"id":"chatcmpl-d2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"id":"chatcmpl-d2","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	})
	code, _ := st.drillCall(t)
	if code != http.StatusOK {
		t.Fatalf("abort-with-usage = %d (流已开始,客户端收中断事件)", code)
	}
	var status, usage string
	var settled int64
	if err := st.db.QueryRow(
		`SELECT status, usage_status, settled_micros FROM inference_requests`).Scan(&status, &usage, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || usage != "estimated" || settled != 17 {
		t.Errorf("request = %s/%s/%d, want settled/estimated/17 (已读实际量,不漏记)", status, usage, settled)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 4 断流（无 usage）：未读任何 usage 即 EOF → 预占保留 + 明确待核对
// （不记零、不静默释放）
// ---------------------------------------------------------------------------

func TestDrill_StreamAbortWithoutUsage(t *testing.T) {
	st := newDrillStack(t, 1_000_000)
	st.upstream.set(func(w http.ResponseWriter, _ []byte) {
		writeOpenAIChunks(w,
			`{"id":"chatcmpl-d3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"H"}}]}`)
	})
	code, _ := st.drillCall(t)
	if code != http.StatusOK {
		t.Fatalf("abort-without-usage = %d", code)
	}
	var status string
	if err := st.db.QueryRow(`SELECT status FROM inference_requests`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "settled" || status == "released" {
		t.Fatalf("request = %s, want 非终态（待核对）", status)
	}
	var reason, jstatus string
	if err := st.db.QueryRow(
		`SELECT reason, status FROM inference_reconciliation_jobs WHERE request_id = (SELECT id FROM inference_requests)`).
		Scan(&reason, &jstatus); err != nil {
		t.Fatalf("reconciliation job missing: %v (断流无 usage 必须进核对)", err)
	}
	if reason != "unknown_usage" || jstatus != "pending" {
		t.Errorf("job = %s/%s, want unknown_usage/pending", reason, jstatus)
	}
	var holds int
	if err := st.db.Get(&holds,
		`SELECT COUNT(*) FROM inference_reservations WHERE state = 'held'`); err != nil {
		t.Fatal(err)
	}
	if holds == 0 {
		t.Error("reservation must stay held for reconciliation (不静默释放)")
	}
	var charges int
	if err := st.db.Get(&charges,
		`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type = 'charge'`); err != nil {
		t.Fatal(err)
	}
	if charges != 0 {
		t.Errorf("charges = %d, want 0 (未知消费不记零也不编造)", charges)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 5 进程重启：预占后崩溃 → 新进程（新 worker 实例）恢复——
// 未发送的释放、可能已发送的保守估算入账 + 核对任务保留证据
// ---------------------------------------------------------------------------

func TestDrill_ProcessRestartRecovery(t *testing.T) {
	st := newDrillStack(t, 1_000_000)
	ctx := context.Background()

	// 三窗口一次创建（崩溃前准入已激活），两次预占共用——与真实准入同形。
	var w5, ww, wm string
	start := time.Now().UTC().Add(-time.Minute)
	for _, sp := range []struct {
		kind  domain.WindowKind
		end   time.Time
		limit domain.Microcredit
	}{
		{domain.WindowFiveHour, start.Add(5 * time.Hour), 1_000_000},
		{domain.WindowWeekly, start.Add(7 * 24 * time.Hour), 100_000_000},
		{domain.WindowMonthly, start.AddDate(0, 1, 0), 1_000_000_000},
	} {
		w := &domain.QuotaWindow{EntitlementID: st.entID, Kind: sp.kind, Start: start, End: sp.end, Limit: sp.limit}
		if err := st.store.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatal(err)
		}
		switch sp.kind {
		case domain.WindowFiveHour:
			w5 = w.ID
		case domain.WindowWeekly:
			ww = w.ID
		case domain.WindowMonthly:
			wm = w.ID
		}
	}
	fabricate := func() string {
		in, out := int64(120), int64(4096)
		uow, err := st.store.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		adm, err := st.store.Reserve(ctx, uow, domain.ReserveCommand{
			Request: domain.Request{
				ID: uuid.NewString(), BillingAccountID: st.accountID, APIKeyID: &st.keyID,
				EntitlementID: st.entID, ModelID: st.modelID,
				Protocol: domain.ProtocolOpenAIChat, Stream: true,
				PolicyVersionID: st.policyID, InputBoundTokens: &in, OutputCapTokens: &out,
			},
			AdmittedAt: time.Now().UTC().Add(-time.Minute),
			Holds: []domain.HoldSpec{
				{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: 50_000},
				{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: 50_000},
				{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: 50_000},
				{TargetKind: domain.TargetKeyBudget, APIKeyID: &st.keyID, Amount: 50_000},
			},
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return adm.RequestID
	}

	// 崩溃时点 A：预占提交后、attempt 意图落库前（绝无上游发送）。
	reqNotSent := fabricate()
	// 崩溃时点 B：attempt 意图已提交、请求 dispatching（可能已发送）。
	reqDispatched := fabricate()
	attID := uuid.NewString()
	startedAt := time.Now().UTC().Add(-time.Minute)
	uow, err := st.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.store.InsertAttemptTx(ctx, uow, &domain.Attempt{
		ID: attID, RequestID: reqDispatched, AttemptNo: 1, Status: "dispatching", StartedAt: &startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.store.UpdateRequestStatus(ctx, reqDispatched, domain.ReqDispatching, ""); err != nil {
		t.Fatal(err)
	}

	// “新进程”：新的 worker 实例，时钟 +2h（所有行越过 grace）。
	worker := workers.NewSettlementRecovery(st.store, domain.FixedClock{T: time.Now().UTC().Add(2 * time.Hour)},
		workers.RecoveryConfig{Interval: time.Second, BatchLimit: 50, Grace: time.Hour, ReconciliationDeadline: 24 * time.Hour}, nil)
	stats, err := worker.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReleasedNotSent != 1 || stats.SettledConservative != 1 || stats.Errors != 0 {
		t.Fatalf("recovery stats = %+v, want 1 released + 1 conservative-settled", stats)
	}
	var s1 string
	if err := st.db.Get(&s1, `SELECT status FROM inference_requests WHERE id = $1`, reqNotSent); err != nil {
		t.Fatal(err)
	}
	if s1 != "released" {
		t.Errorf("not-sent request = %s, want released (确认零消费)", s1)
	}
	var s2, u2 string
	var settled2 int64
	if err := st.db.QueryRow(
		`SELECT status, usage_status, settled_micros FROM inference_requests WHERE id = $1`, reqDispatched).
		Scan(&s2, &u2, &settled2); err != nil {
		t.Fatal(err)
	}
	if s2 != "settled" || u2 != "estimated" || settled2 != 50_000 {
		t.Errorf("dispatched request = %s/%s/%d, want settled/estimated/50000 (保守估算=预占额,不静默免费)", s2, u2, settled2)
	}
	var reason string
	if err := st.db.Get(&reason,
		`SELECT reason FROM inference_reconciliation_jobs WHERE request_id = $1`, reqDispatched); err != nil {
		t.Fatalf("crash_recovery job missing: %v", err)
	}
	if reason != "crash_recovery" {
		t.Errorf("job reason = %s, want crash_recovery (证据窗口保留)", reason)
	}
	// 恢复幂等：第二个 pass 零效果。
	stats, err = worker.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReleasedNotSent != 0 || stats.SettledConservative != 0 {
		t.Errorf("second pass = %+v, want no effect (幂等)", stats)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 6 数据库短暂不可用：连接被管理员终止 → 新调用 fail-closed（500，
// 零痕迹）；连接池自愈后调用恢复
// ---------------------------------------------------------------------------

// pgTCPProxy 是测试内 L4 中继：网关池拨代理地址，代理转发到真实 PG。
// cut() 关监听 + 断全部已建连接（池侧立即 ECONNREFUSED/EOF——与网络层
// 短暂宕机同效，确定性强，不依赖实例控制权限）；heal() 同地址重听。
type pgTCPProxy struct {
	addr   string
	target string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	down   bool
}

func newPGTCPProxy(t *testing.T, target string) *pgTCPProxy {
	t.Helper()
	p := &pgTCPProxy{target: target, conns: map[net.Conn]struct{}{}}
	p.listen(t)
	t.Cleanup(p.cut)
	return p
}

func (p *pgTCPProxy) listen(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.ln = ln
	p.addr = ln.Addr().String()
	p.down = false
	p.mu.Unlock()
	go p.serve()
}

func (p *pgTCPProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return // listener closed (cut/heal/cleanup)
		}
		up, err := net.DialTimeout("tcp", p.target, 2*time.Second)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		go func() {
			defer func() {
				p.mu.Lock()
				delete(p.conns, c)
				p.mu.Unlock()
				c.Close()
				up.Close()
			}()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}

// cut drops every established proxied connection and stops accepting — the
// pool sees ECONNREFUSED on its next dial, exactly like a brief outage.
func (p *pgTCPProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return
	}
	p.down = true
	_ = p.ln.Close()
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *pgTCPProxy) heal(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr) // 同地址重听：池无需改 DSN
	if err != nil {
		t.Fatalf("proxy re-listen: %v", err)
	}
	p.mu.Lock()
	p.ln = ln
	p.down = false
	p.mu.Unlock()
	go p.serve()
}

func TestDrill_DatabaseBriefOutage(t *testing.T) {
	// 网关/存储池经 TCP 代理连真实 PG；断言通道直连。cut() 后池拨号
	// ECONNREFUSED——网络层短暂宕机的确定性注入（不依赖 superuser/实例控制）。
	u, err := url.Parse(dbURL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newPGTCPProxy(t, u.Host)
	u.Host = proxy.addr
	st := newDrillStackDSN(t, 1_000_000, u.String()) // 种子全经代理（代理健康在此已证）

	// 故障窗口内：新调用 fail-closed（500）。
	proxy.cut()
	code, _ := st.drillCall(t)
	if code != http.StatusInternalServerError {
		t.Fatalf("call during outage = %d, want 500 (fail-closed)", code)
	}

	// 恢复：同地址重听 → 池重连；故障期调用零痕迹。
	proxy.heal(t)
	var reqs int
	if err := st.db.Get(&reqs, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if reqs != 0 {
		t.Fatalf("request rows after failed call = %d, want 0 (准入失败零痕迹)", reqs)
	}

	// 调用恢复并成功结算。
	code, body := st.drillCall(t)
	if code != http.StatusOK {
		t.Fatalf("call after recovery = %d — %s", code, body)
	}
	var status string
	if err := st.db.Get(&status, `SELECT status FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if status != "settled" {
		t.Errorf("post-recovery request = %s, want settled", status)
	}
	drillInvariant(t, st)
}

// ---------------------------------------------------------------------------
// 场景 7 上游耗尽：唯一上游账号观测额度归零 → 调度跳过 → 503 +
// 预占释放（零扣费、零残留）；reset 已过恢复后调用恢复
// ---------------------------------------------------------------------------

func TestDrill_UpstreamQuotaExhausted(t *testing.T) {
	st := newDrillStack(t, 1_000_000)
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx, `
		UPDATE inference_upstream_accounts
		   SET quota_limit_micros = 1000, quota_remaining_micros = 0,
		       quota_observed_at = now(), quota_source = 'reported',
		       quota_reset_at = now() + interval '1 hour'
		 WHERE id = $1`, st.upAcctID); err != nil {
		t.Fatal(err)
	}
	code, body := st.drillCall(t)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted upstream call = %d — %s, want 503 upstream_unavailable", code, body)
	}
	if !strings.Contains(body, "upstream_unavailable") {
		t.Errorf("body = %s, want upstream_unavailable", body)
	}
	var status string
	var settled *int64
	if err := st.db.QueryRow(
		`SELECT status, settled_micros FROM inference_requests`).Scan(&status, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "released" || settled != nil {
		t.Errorf("request = %s/%v, want released/NULL (无消费不入账)", status, settled)
	}
	var charges int
	if err := st.db.Get(&charges, `SELECT COUNT(*) FROM inference_ledger_entries`); err != nil {
		t.Fatal(err)
	}
	if charges != 0 {
		t.Errorf("charges = %d, want 0 (未触上游零扣费)", charges)
	}
	drillInvariant(t, st)

	// reset 已过（健康 worker 复测恢复语义的演练抽样）→ 调用恢复。
	if _, err := st.db.ExecContext(ctx, `
		UPDATE inference_upstream_accounts SET quota_reset_at = now() - interval '1 minute' WHERE id = $1`, st.upAcctID); err != nil {
		t.Fatal(err)
	}
	code, body = st.drillCall(t)
	if code != http.StatusOK {
		t.Fatalf("call after quota reset = %d — %s", code, body)
	}
	drillInvariant(t, st)
}
