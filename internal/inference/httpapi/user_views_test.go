package httpapi_test

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

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// user_views_test.go — Task 11 客户读端点的端到端测试（真实库 + 真实 JWT
// 中间件）。越权模型：端点不接收任何 ID 参数，所有权由 JWT 身份决定，跨客
// 户读取在结构上不可能 —— 测试断言 B 的任何响应都不含 A 的数据标识。

type viewsFixture struct {
	db       *sqlx.DB
	store    *postgres.Store
	engine   *gin.Engine
	tokenSvc *service.TokenService
}

func newViewsFixture(t *testing.T) *viewsFixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	wipeViews(t, db)

	store := postgres.NewStore(db)
	tokenSvc := newTestTokenService(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	userGroup := engine.Group("/user", middleware.JWTAuth(tokenSvc))
	httpapi.NewUserQuotasHandler(management.NewQuotaViewService(store, nil)).Register(userGroup)
	httpapi.NewUserUsageHandler(management.NewUsageViewService(store, nil)).Register(userGroup)
	httpapi.NewUserSubscriptionsHandler(management.NewSubscriptionViewService(store, nil)).Register(userGroup)
	return &viewsFixture{db: db, store: store, engine: engine, tokenSvc: tokenSvc}
}

func wipeViews(t *testing.T, db *sqlx.DB) {
	t.Helper()
	_, err := db.Exec(`TRUNCATE
		inference_audit_log, operator_roles,
		inference_wallet_entries, inference_wallet_audits,
		inference_wallet_holds, inference_wallets, inference_payg_config,
		inference_response_chains,
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
		usage_events, subscriptions, plans, apps,
		users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
}

func (f *viewsFixture) addUser(t *testing.T) (userID, token string) {
	t.Helper()
	userID = uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tok, err := f.tokenSvc.SignAccessToken(userID, "kaya", nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return userID, tok
}

func (f *viewsFixture) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// decodeData unwraps the {code,data,message} envelope.
func decodeData(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	if env.Code != 0 {
		t.Fatalf("code = %d (%s)", env.Code, w.Body.String())
	}
	return env.Data
}

func mustMicro(v int64) *domain.Microcredit { m := domain.Microcredit(v); return &m }

// seedViewGraph seeds a full customer graph: policy + entitlement + three
// window rows + one settled request with usage + one parked request.
type viewGraph struct {
	accountID  string
	entID      string
	policyID   string
	windows    map[domain.WindowKind]string
	reqSettled string
	reqParked  string
}

func (f *viewsFixture) seedViewGraph(t *testing.T, userID string) viewGraph {
	t.Helper()
	ctx := context.Background()
	var g viewGraph

	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	g.accountID = acct.ID
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: mustMicro(1_000_000), WeeklyLimit: mustMicro(10_000_000),
		MonthlyLimit: mustMicro(100_000_000), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	g.policyID = pol.ID
	anchor := time.Now().UTC().Add(-48 * time.Hour)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{"glm-4.6"},
		PolicyVersionID: pol.ID, AnchorAt: anchor, EffectiveFrom: anchor,
	}
	if err := f.store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	g.entID = ent.ID

	// 窗口行每权益每类恰一行（EXCLUDE 不重叠约束）——整图共享。
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	g.windows = map[domain.WindowKind]string{}
	for _, spec := range []struct {
		kind  domain.WindowKind
		end   time.Time
		limit domain.Microcredit
	}{
		{domain.WindowFiveHour, start.Add(5 * time.Hour), 1_000_000},
		{domain.WindowWeekly, start.Add(7 * 24 * time.Hour), 10_000_000},
		{domain.WindowMonthly, start.AddDate(0, 1, 0), 100_000_000},
	} {
		w := &domain.QuotaWindow{
			EntitlementID: g.entID, Kind: spec.kind, Start: start, End: spec.end, Limit: spec.limit,
		}
		if err := f.store.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatalf("window %s: %v", spec.kind, err)
		}
		g.windows[spec.kind] = w.ID
	}

	// 一次真实预占+结算（reported 用量）。
	g.reqSettled = f.admitAndSettle(t, g, 60_000)
	// 一次待核对（预占保留、无计量事实）。
	parked := &domain.Request{
		ID: uuid.NewString(), BillingAccountID: acct.ID, EntitlementID: ent.ID,
		ModelID: "glm-4.6", Protocol: domain.ProtocolOpenAIChat,
		Status: domain.ReqReconciliationRequired, PolicyVersionID: pol.ID,
		ReservedMicros: mustMicro(9_999), UsageStatus: domain.UsageUnknown,
		LastError: "stream interrupted",
	}
	if err := f.store.InsertRequest(ctx, parked); err != nil {
		t.Fatal(err)
	}
	g.reqParked = parked.ID
	return g
}

// admitAndSettle runs the real reservation + settlement transaction chain
// against the graph's shared windows.
func (f *viewsFixture) admitAndSettle(t *testing.T, g viewGraph, charge domain.Microcredit) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	wins := g.windows

	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reqID := uuid.NewString()
	hold := charge + 10_000
	w5 := wins[domain.WindowFiveHour]
	ww := wins[domain.WindowWeekly]
	wm := wins[domain.WindowMonthly]
	adm, err := f.store.Reserve(ctx, uow, domain.ReserveCommand{
		Request: domain.Request{
			ID: reqID, BillingAccountID: g.accountID, EntitlementID: g.entID,
			ModelID: "glm-4.6", Protocol: domain.ProtocolOpenAIChat,
			PolicyVersionID: g.policyID,
		},
		Holds: []domain.HoldSpec{
			{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: hold},
			{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: hold},
			{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: hold},
		},
		AdmittedAt: now,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	attemptID := uuid.NewString()
	// 尝试意图必须落在同一事务内（设计 §7.2）：用池连接直插会等未提交的
	// 请求行 FK，形成自死锁。
	if err := f.store.InsertAttemptTx(ctx, uow, &domain.Attempt{
		ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1, Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	if err := f.store.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: charge, SettledAt: now,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

func TestUserViews_OwnershipAndIsolation(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	_, tokB := f.addUser(t)
	g := f.seedViewGraph(t, userA)

	// A 看到自己的配额/用量；B 的任何响应不得含 A 的标识。
	w := f.get(t, "/user/model-quotas", tokA)
	if w.Code != http.StatusOK {
		t.Fatalf("A quotas: %d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	if data["entitlement_id"] != g.entID {
		t.Errorf("entitlement_id = %v", data["entitlement_id"])
	}
	if data["unit"] != "microcredit" {
		t.Errorf("unit = %v", data["unit"])
	}
	windows := data["windows"].([]any)
	if len(windows) != 3 {
		t.Fatalf("windows = %d", len(windows))
	}

	for _, path := range []string{
		"/user/model-quotas", "/user/model-usage/summary",
		"/user/model-usage/requests", "/user/model-subscriptions",
	} {
		w := f.get(t, path, tokB)
		if w.Code != http.StatusOK {
			t.Fatalf("B %s: %d %s", path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, leaked := range []string{g.entID, g.accountID, g.reqSettled, g.reqParked, g.policyID} {
			if strings.Contains(body, leaked) {
				t.Errorf("B %s leaked A's identifier %s", path, leaked)
			}
		}
	}
	// B 的配额视图：无权益空视图 + no_active_entitlement。
	dataB := decodeData(t, f.get(t, "/user/model-quotas", tokB))
	if dataB["entitlement_id"] != nil || dataB["entitlement"] != nil {
		t.Errorf("B entitlement = %v", dataB["entitlement_id"])
	}
	blocks := dataB["blocked_by"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["reason"] != "no_active_entitlement" {
		t.Errorf("B blocked_by = %v", blocks)
	}
	// B 的用量与订阅为空集合（非错误）。
	sumB := decodeData(t, f.get(t, "/user/model-usage/summary", tokB))
	if len(sumB["groups"].([]any)) != 0 {
		t.Errorf("B groups = %v", sumB["groups"])
	}
	subB := decodeData(t, f.get(t, "/user/model-subscriptions", tokB))
	if len(subB["subscriptions"].([]any)) != 0 || subB["kaya_membership"] != nil {
		t.Errorf("B subscriptions = %v", subB)
	}

	// 未认证一律 401（JWT 中间件）。
	for _, path := range []string{
		"/user/model-quotas", "/user/model-usage/summary",
		"/user/model-usage/requests", "/user/model-subscriptions",
	} {
		if w := f.get(t, path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("no-JWT %s: %d, want 401", path, w.Code)
		}
	}
}

func TestUserViews_UsageValidation(t *testing.T) {
	f := newViewsFixture(t)
	_, tokA := f.addUser(t)
	bad := []string{
		"/user/model-usage/summary?group_by=day",
		"/user/model-usage/requests?limit=0",
		"/user/model-usage/requests?limit=101",
		"/user/model-usage/requests?cursor=abc",
		"/user/model-usage/summary?from=2026-09-09T00:00:00Z&to=2026-09-08T00:00:00Z",
		"/user/model-usage/summary?from=2026-06-01T00:00:00Z&to=2026-09-09T00:00:00Z", // > 92 天
		"/user/model-usage/requests?from=not-a-time",
	}
	// I1：伪造游标（合法 base64+JSON、id 非 UUID）必须 400 invalid_input，
	// 不得漏到 SQL 的 ::uuid 转换（500）。
	forged := management.EncodeRequestCursor(management.RequestCursor{
		CreatedAt: time.Now().UTC(), ID: "not-a-uuid"})
	bad = append(bad, "/user/model-usage/requests?cursor="+forged)
	for _, path := range bad {
		w := f.get(t, path, tokA)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid_input") {
			t.Errorf("%s: body missing invalid_input (%s)", path, w.Body.String())
		}
	}
	// 默认范围（30 天）生效。
	w := f.get(t, "/user/model-usage/summary", tokA)
	data := decodeData(t, w)
	rng := data["range"].(map[string]any)
	from, _ := time.Parse(time.RFC3339, rng["from"].(string))
	to, _ := time.Parse(time.RFC3339, rng["to"].(string))
	if d := to.Sub(from); d != 30*24*time.Hour {
		t.Errorf("default range = %v, want 30d", d)
	}
	if data["complete_through"] == nil || data["as_of"] == nil {
		t.Error("as_of/complete_through missing")
	}
}

func TestUserViews_UsageKeysetPaginationOverHTTP(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	g := f.seedViewGraph(t, userA)
	// 再加两个 settled 请求 → 共 4 行（含 parked）。
	f.admitAndSettle(t, g, 20_000)
	f.admitAndSettle(t, g, 30_000)

	var ids []string
	cursor := ""
	for page := 0; page < 6; page++ {
		path := "/user/model-usage/requests?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		w := f.get(t, path, tokA)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, w.Code, w.Body.String())
		}
		data := decodeData(t, w)
		items := data["items"].([]any)
		for _, it := range items {
			ids = append(ids, it.(map[string]any)["request_id"].(string))
		}
		if data["next_cursor"] == nil {
			cursor = ""
			break
		}
		cursor = data["next_cursor"].(string)
	}
	if len(ids) != 4 {
		t.Fatalf("paginated ids = %v, want 4 rows", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate row across pages: %s", id)
		}
		seen[id] = true
	}
	if cursor != "" {
		t.Error("last page must carry null next_cursor")
	}
	// 明细分页行的计量完整性字段。
	w := f.get(t, "/user/model-usage/requests?limit=100", tokA)
	data := decodeData(t, w)
	var parkedRow, settledRow map[string]any
	for _, it := range data["items"].([]any) {
		m := it.(map[string]any)
		switch m["request_id"] {
		case g.reqParked:
			parkedRow = m
		case g.reqSettled:
			settledRow = m
		}
	}
	if parkedRow == nil || parkedRow["usage_status"] != "unknown" || parkedRow["status"] != "reconciliation_required" {
		t.Errorf("parked row = %v", parkedRow)
	}
	if parkedRow["tokens"] != nil || parkedRow["charge_micros"] != nil {
		t.Errorf("parked row must have null tokens/charge: %v", parkedRow)
	}
	if parkedRow["reserved_micros"] != "9999" {
		t.Errorf("parked reserved = %v (预占保留可见)", parkedRow["reserved_micros"])
	}
	if settledRow == nil || settledRow["charge_micros"] != "60000" || settledRow["net_micros"] != "60000" {
		t.Errorf("settled row = %v", settledRow)
	}
	tokens := settledRow["tokens"].(map[string]any)
	if tokens["input_tokens"] != float64(800) || tokens["cache_read_tokens"] != nil {
		t.Errorf("tokens = %v (null = 未知)", tokens)
	}

	// summary 计量完整性计数。
	data = decodeData(t, f.get(t, "/user/model-usage/summary?group_by=model", tokA))
	groups := data["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	grp := groups[0].(map[string]any)
	if grp["reported"] != float64(3) || grp["unknown"] != float64(1) || grp["reconciliation_pending"] != float64(1) {
		t.Errorf("integrity counts = %v", grp)
	}
	if grp["charge_micros"] != "110000" {
		t.Errorf("charge = %v, want 110000 (60k+20k+30k)", grp["charge_micros"])
	}
}

func TestUserViews_MultiWindowBlock(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	ctx := context.Background()

	acct, err := f.store.EnsureBillingAccount(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "g", ContextTokens: 1000, MaxOutputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "p", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: mustMicro(100), WeeklyLimit: mustMicro(500), MonthlyLimit: mustMicro(9000),
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	anchor := now.Add(-48 * time.Hour)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{"glm-4.6"},
		PolicyVersionID: pol.ID, AnchorAt: anchor, EffectiveFrom: anchor,
	}
	if err := f.store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	// five_hour 活跃窗口耗尽；weekly 当前周期耗尽；monthly 有剩余。
	for _, w := range []*domain.QuotaWindow{
		{EntitlementID: ent.ID, Kind: domain.WindowFiveHour,
			Start: now.Add(-time.Hour), End: now.Add(4 * time.Hour), Limit: 100, Used: 100},
		{EntitlementID: ent.ID, Kind: domain.WindowWeekly,
			Start: anchor, End: anchor.Add(7 * 24 * time.Hour), Limit: 500, Used: 500},
		{EntitlementID: ent.ID, Kind: domain.WindowMonthly,
			Start: anchor, End: anchor.AddDate(0, 1, 0), Limit: 9000, Used: 10},
	} {
		if err := f.store.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	blocks := data["blocked_by"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocked_by = %v, want five_hour+weekly", blocks)
	}
	got := map[string]map[string]any{}
	for _, b := range blocks {
		m := b.(map[string]any)
		got[m["kind"].(string)] = m
	}
	fh, wk := got["five_hour"], got["weekly"]
	if fh == nil || wk == nil {
		t.Fatalf("blocked kinds = %v", got)
	}
	// 准确的阻断原因与恢复时刻；剩余 0。
	if fh["reason"] != "quota_exhausted" || fh["resets_at"] == nil || fh["remaining"] != "0" {
		t.Errorf("five_hour block = %v", fh)
	}
	if wk["reason"] != "quota_exhausted" || wk["resets_at"] == nil || wk["used"] != "500" {
		t.Errorf("weekly block = %v", wk)
	}
	// 前端无需计算窗口：weekly/monthly 有边界，five_hour 活跃行有边界。
	for _, wv := range data["windows"].([]any) {
		m := wv.(map[string]any)
		if m["window_start"] == nil || m["resets_at"] == nil {
			t.Errorf("active window missing bounds: %v", m)
		}
		if m["remaining"] == nil {
			t.Errorf("window missing remaining: %v", m)
		}
	}
}

func TestUserViews_SubscriptionsSeparateFromLegacyMembership(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	ctx := context.Background()
	g := f.seedViewGraph(t, userA)

	// coding-plan 订阅 + kaya 会员 + 捆绑赠送权益。
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO apps (app_id, name, is_active) VALUES ('kaya', 'Kaya', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		 VALUES ('cp_basic', 'Coding Plan Basic', 29.9, 30, '{}', 'CNY', 'coding-plan'),
		        ('monthly', 'Kaya Monthly', 9.9, 30, '{}', 'CNY', 'kaya-membership')`); err != nil {
		t.Fatal(err)
	}
	cpSubID := uuid.NewString()
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO subscriptions (id, user_id, plan_id, product_code, status) VALUES ($1,$2,'cp_basic','coding-plan','active')`,
		cpSubID, userA); err != nil {
		t.Fatal(err)
	}
	kayaSubID := uuid.NewString()
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO subscriptions (id, user_id, plan_id, product_code, status) VALUES ($1,$2,'monthly','kaya-membership','active')`,
		kayaSubID, userA); err != nil {
		t.Fatal(err)
	}
	// 权益来源指向 coding-plan 订阅（替换 seed 的假 source）。
	if _, err := f.db.ExecContext(ctx,
		`UPDATE inference_entitlements SET source_id = $1 WHERE id = $2`, cpSubID, g.entID); err != nil {
		t.Fatal(err)
	}
	// kaya 会员的捆绑赠送权益（bundle:<sub>）。
	anchor := time.Now().UTC().Add(-96 * time.Hour)
	gift := &domain.Entitlement{
		BillingAccountID: g.accountID, SourceType: domain.SourceGrant,
		SourceID: "bundle:" + kayaSubID, ModelIDs: []string{"glm-4.6"},
		PolicyVersionID: g.policyID, AnchorAt: anchor, EffectiveFrom: anchor,
	}
	if err := f.store.InsertEntitlement(ctx, gift); err != nil {
		t.Fatal(err)
	}

	w := f.get(t, "/user/model-subscriptions", tokA)
	data := decodeData(t, w)
	subs := data["subscriptions"].([]any)
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %v, want exactly the coding-plan row", subs)
	}
	sub := subs[0].(map[string]any)
	if sub["product_code"] != "coding-plan" || sub["plan_name"] != "Coding Plan Basic" {
		t.Errorf("sub = %v", sub)
	}
	kaya := data["kaya_membership"].(map[string]any)
	if kaya["plan_id"] != "monthly" || kaya["product_code"] != "kaya-membership" {
		t.Errorf("kaya_membership = %v", kaya)
	}
	ents := data["entitlements"].([]any)
	if len(ents) != 2 {
		t.Fatalf("entitlements = %v", ents)
	}
	var explicit, bundle map[string]any
	for _, e := range ents {
		m := e.(map[string]any)
		switch m["source"].(map[string]any)["type"] {
		case "subscription":
			explicit = m
		case "grant":
			bundle = m
		}
	}
	if explicit == nil || explicit["source"].(map[string]any)["subscription_id"] != cpSubID {
		t.Errorf("explicit entitlement source = %v", explicit)
	}
	if bundle == nil {
		t.Fatal("bundle gift entitlement missing")
	}
	bsrc := bundle["source"].(map[string]any)
	if bsrc["kind"] != "bundle_gift" || bsrc["subscription_id"] != kayaSubID || bsrc["plan_id"] != "monthly" {
		t.Errorf("bundle gift source = %v", bsrc)
	}
	// 不暴露上游账号：响应不存在 upstream 字样字段。
	if strings.Contains(strings.ToLower(w.Body.String()), "upstream") {
		t.Errorf("response leaks upstream references: %s", w.Body.String())
	}
}
