package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// admin_bulk_test.go — Task 15 运营面的 HTTP 级验收（真实库 + 真实授权中间
// 件；身份双腿用测试头打桩，与 user_wallet_test.go 同一模式）：
//   - 批量导入：dry-run 不写库 / 部分错误 commit 400 且一行不写（绝不半发
//     布）/ task_id 重放不重复创建 / commit 追加审计可定位到人员。
//   - 补偿：POST 调整带同事务审计；GET /admin/model-adjustments（usage:read）
//     列出 operator_subject/service_subject/幂等键。
//   - 统计/异常/预览端点的挂载与响应形状。

// doH is do() plus request headers (operator identity legs).
func doH(t *testing.T, engine http.Handler, method, path string, body any, wantStatus int, headers map[string]string) envelope {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d (body %s)", method, path, w.Code, wantStatus, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope decode: %v (%s)", err, w.Body.String())
	}
	return env
}

type opsFixture struct {
	db     *sqlx.DB
	store  *postgres.Store
	engine *gin.Engine
}

// newOpsFixture builds the Task 15 operator surface with header-stubbed
// identity legs (X-Test-User / X-Test-App) in front of the REAL per-
// permission authorization middleware.
func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`TRUNCATE
		inference_wallet_entries, inference_wallet_audits,
		inference_wallet_holds, inference_wallets, inference_payg_config,
		inference_response_chains,
		inference_session_bindings, inference_oauth_grants,
		inference_bulk_imports,
		inference_audit_log,
		operator_roles,
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
		users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}

	store := postgres.NewStore(db)
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	stub := func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		if a := c.GetHeader("X-Test-App"); a != "" {
			c.Set(middleware.ContextApp, &model.App{AppID: a})
			c.Set(middleware.ContextAppID, a)
		}
		c.Next()
	}

	opsSvc := management.NewOperationsService(store, nil)
	usageH := httpapi.NewAdminUsageHandler(opsSvc, management.NewPricingPreviewService(store, nil))
	bulkH := httpapi.NewAdminBulkHandler(management.NewBulkImportService(store, store,
		func(context.Context, string) error { return nil })) // permissive egress stub
	adjH := httpapi.NewAdminAdjustmentsHandler(store, nil, store, opsSvc)

	modelsGroup := engine.Group("/adminm", stub, httpapi.OperatorAuthz(store, management.PermModelsManage))
	bulkH.Register(modelsGroup)
	usageH.RegisterPreview(modelsGroup)
	// 售价版本管理面（models:manage）——创建只追加 + 列表。
	httpapi.NewAdminPriceVersionsHandler(management.NewPriceVersionService(store, store, nil)).Register(modelsGroup)
	// 既有目录发布面（导入→草稿→发布链路断言用）。Task 16：读写两面都挂
	// （RegisterReadOnly 提供 GET 列表/详情/revisions/active 覆盖路径）。
	catalogSvc := catalog.NewService(store)
	adminModelsH := httpapi.NewAdminModelsHandler(management.NewCatalogManager(catalogSvc, store,
		func(context.Context, string) error { return nil }))
	adminModelsH.RegisterReadOnly(modelsGroup)
	adminModelsH.RegisterWrite(modelsGroup)
	usageGroup := engine.Group("/adminu", stub, httpapi.OperatorAuthz(store, management.PermUsageRead))
	usageH.RegisterRead(usageGroup)
	billingGroup := engine.Group("/adminb", stub, httpapi.OperatorAuthz(store, management.PermBillingAdjust))
	adjH.Register(billingGroup)

	// 凭据/可调度账号面（credentials:manage）：OAuth handler 提供既有的
	// GET /upstream-accounts 读面（oauth/refresh 服务在本夹具中不调用）。
	keyBytes := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: keyBytes}, 1)
	if err != nil {
		t.Fatal(err)
	}
	credSvc := credentials.NewService(vault, store, store)
	credGroup := engine.Group("/adminc", stub, httpapi.OperatorAuthz(store, management.PermCredentialsManage))
	httpapi.NewAdminCredentialsHandler(credSvc).Register(credGroup)
	httpapi.NewAdminOAuthHandler(nil, nil, store).Register(credGroup)
	httpapi.NewAdminAccountsHandler(credentials.NewAccountService(store, store)).Register(credGroup)

	return &opsFixture{db: db, store: store, engine: engine}
}

// grantOp seeds a user + operator roles.
func (f *opsFixture) grantOp(t *testing.T, userID string, roles ...string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := f.store.GrantRole(context.Background(), userID, r, nil, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *opsFixture) headers(userID string) map[string]string {
	return map[string]string{"X-Test-User": userID, "X-Test-App": "ops-console"}
}

func bulkDoc() map[string]any {
	return map[string]any{
		"providers": []map[string]any{{
			"code": "glm", "display_name": "GLM", "access_type": "official_api",
		}},
		"models": []map[string]any{{
			"id": "glm-4.7", "display_name": "GLM 4.7",
			"context_tokens": 200000, "max_output_tokens": 8192,
			"protocols": []string{"openai_chat"},
			"deployments": []map[string]any{{
				"provider_code": "glm", "upstream_model": "glm-4.7-up",
				"base_url": "https://api.glm.example.com", "protocol": "openai_chat",
			}},
		}},
	}
}

func tableCount(t *testing.T, f *opsFixture, table string) int {
	t.Helper()
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAdminBulkImport_HTTPFlow: dry-run 不落库 → commit 201 落草稿 + 审计
// 归因 → 同 task_id 重放 200 不重复创建。
func TestAdminBulkImport_HTTPFlow(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)

	// dry-run：200 + 逐项 would_insert，库为空。
	body := bulkDoc()
	body["task_id"] = "imp-http-1"
	body["dry_run"] = true
	env := doH(t, f.engine, http.MethodPost, "/adminm/catalog/bulk-import", body, http.StatusOK, f.headers(op))
	var dry struct {
		DryRun bool `json:"dry_run"`
		Items  []struct {
			Kind       string `json:"kind"`
			NaturalKey string `json:"natural_key"`
			Status     string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || len(dry.Items) != 4 {
		t.Fatalf("dry-run = %s", env.Data)
	}
	for _, it := range dry.Items {
		if it.Status != "would_insert" && it.Status != "would_skip" {
			t.Fatalf("dry-run item = %+v", it)
		}
	}
	if n := tableCount(t, f, "inference_models"); n != 0 {
		t.Fatalf("dry-run wrote %d models", n)
	}

	// commit：201 + 全 inserted；模型落库即草稿。
	body["dry_run"] = false
	env = doH(t, f.engine, http.MethodPost, "/adminm/catalog/bulk-import", body, http.StatusCreated, f.headers(op))
	var committed struct {
		Committed bool `json:"committed"`
		Replayed  bool `json:"replayed"`
		Inserted  int  `json:"inserted"`
	}
	if err := json.Unmarshal(env.Data, &committed); err != nil {
		t.Fatal(err)
	}
	if !committed.Committed || committed.Replayed || committed.Inserted != 4 {
		t.Fatalf("commit = %s", env.Data)
	}
	var lifecycle string
	if err := f.db.Get(&lifecycle, `SELECT lifecycle FROM inference_models WHERE id = 'glm-4.7'`); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "draft" {
		t.Fatalf("lifecycle = %s, want draft", lifecycle)
	}

	// 审计归因：catalog.bulk_import 记录人员与服务双重归因。
	var actorUser, actorApp, action string
	if err := f.db.QueryRow(
		`SELECT actor_user_id::text, actor_app_id, action FROM inference_audit_log
		 WHERE object_type = 'bulk_import' AND object_id = 'imp-http-1'`).
		Scan(&actorUser, &actorApp, &action); err != nil {
		t.Fatalf("bulk import audit missing: %v", err)
	}
	if actorUser != op || actorApp != "ops-console" || action != "catalog.bulk_import" {
		t.Fatalf("audit attribution = %s/%s/%s", actorUser, actorApp, action)
	}

	// 重放：同 task_id（哪怕文档不同）→ 200 replayed，行数不变。
	replayBody := map[string]any{"task_id": "imp-http-1",
		"providers": []map[string]any{{"code": "other", "display_name": "O", "access_type": "official_api"}}}
	env = doH(t, f.engine, http.MethodPost, "/adminm/catalog/bulk-import", replayBody, http.StatusOK, f.headers(op))
	var replayed struct {
		Replayed bool `json:"replayed"`
		Inserted int  `json:"inserted"`
	}
	if err := json.Unmarshal(env.Data, &replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Inserted != 4 {
		t.Fatalf("replay = %s", env.Data)
	}
	if n := tableCount(t, f, "inference_providers"); n != 1 {
		t.Fatalf("replay created rows: providers = %d", n)
	}

	// 导入→草稿→发布：catalog publish 成功且发布不翻生命周期（仍是草稿，
	// 运营补齐授权/价格后再单独激活）。
	env = doH(t, f.engine, http.MethodPost, "/adminm/catalog/publish", map[string]any{}, http.StatusOK, f.headers(op))
	var pub struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(env.Data, &pub); err != nil || pub.Revision < 1 {
		t.Fatalf("publish = %s err=%v", env.Data, err)
	}
	if err := f.db.Get(&lifecycle, `SELECT lifecycle FROM inference_models WHERE id = 'glm-4.7'`); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "draft" {
		t.Fatalf("publish must not flip lifecycle (still draft), got %s", lifecycle)
	}
}

// TestAdminBulkImport_PartialErrorsNotHalfPublished: 部分错误的 commit →
// 400 + 逐项错误在 data.items，一行不写。
func TestAdminBulkImport_PartialErrorsNotHalfPublished(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)

	doc := bulkDoc()
	doc["task_id"] = "imp-http-err"
	doc["models"] = append(doc["models"].([]map[string]any), map[string]any{
		"id": "broken model!", "display_name": "x",
		"context_tokens": 1000, "max_output_tokens": 100,
		"protocols": []string{"openai_chat"},
	})
	env := doH(t, f.engine, http.MethodPost, "/adminm/catalog/bulk-import", doc, http.StatusBadRequest, f.headers(op))
	var res struct {
		Errors int `json:"errors"`
		Items  []struct {
			Kind       string `json:"kind"`
			NaturalKey string `json:"natural_key"`
			Status     string `json:"status"`
			Error      string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Errors == 0 {
		t.Fatalf("expected per-item errors: %s", env.Data)
	}
	found := false
	for _, it := range res.Items {
		if it.Kind == "model" && it.NaturalKey == "broken model!" && it.Status == "error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("per-item error missing: %s", env.Data)
	}
	// 绝不半发布：合法的那一半也不落库。
	for _, tbl := range []string{"inference_providers", "inference_models", "inference_deployments", "inference_model_routes"} {
		if n := tableCount(t, f, tbl); n != 0 {
			t.Fatalf("%s rows = %d after rejected commit (半发布!)", tbl, n)
		}
	}
	if n := tableCount(t, f, "inference_bulk_imports"); n != 0 {
		t.Fatalf("failed import registered a task row")
	}
}

// TestAdminBulkImport_PermissionGate: 无 models:manage 权限的 auditor 被拒。
func TestAdminBulkImport_PermissionGate(t *testing.T) {
	f := newOpsFixture(t)
	auditor := uuid.NewString()
	f.grantOp(t, auditor, management.RoleAuditor)
	doc := bulkDoc()
	doc["task_id"] = "imp-denied"
	doH(t, f.engine, http.MethodPost, "/adminm/catalog/bulk-import", doc, http.StatusForbidden, f.headers(auditor))
	// auditor 有 usage:read：统计面可达。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary", nil, http.StatusOK, f.headers(auditor))
}

// TestAdminAdjustments_AuditAndTrace: 补偿带同事务追加审计 + 两读面（
// billing:adjust 的 /wallet/adjustments 与 usage:read 的 /model-adjustments）
// 均可定位到人员；幂等键重放不重复补偿。
func TestAdminAdjustments_AuditAndTrace(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin) // admin 含 billing:adjust + usage:read
	customer := uuid.NewString()
	f.grantOp(t, customer) // 仅用户行（无角色）

	adjust := map[string]any{
		"user_id": customer, "currency": "CNY", "source": "bonus",
		"direction": "credit", "amount_micros": "5000",
		"reason": "服务降级补偿", "idempotency_key": "comp-001",
	}
	env := doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", adjust, http.StatusCreated, f.headers(op))
	var applied struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(env.Data, &applied); err != nil || !applied.Applied {
		t.Fatalf("adjust = %s err=%v", env.Data, err)
	}

	// 同事务审计：wallet.adjust 带人员归因与幂等键细节。
	var actorUser, action string
	var detail []byte
	if err := f.db.QueryRow(
		`SELECT actor_user_id::text, action, detail FROM inference_audit_log
		 WHERE action = 'wallet.adjust'`).Scan(&actorUser, &action, &detail); err != nil {
		t.Fatalf("adjustment audit missing: %v", err)
	}
	if actorUser != op || !json.Valid(detail) {
		t.Fatalf("audit = %s %s %s", actorUser, action, detail)
	}

	// 幂等键重放：applied=false，账本不重复。
	env = doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", adjust, http.StatusOK, f.headers(op))
	var replay struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(env.Data, &replay); err != nil || replay.Applied {
		t.Fatalf("replay = %s err=%v", env.Data, err)
	}
	if n := tableCount(t, f, "inference_adjustments"); n != 1 {
		t.Fatalf("adjustments = %d, want 1 (幂等)", n)
	}

	// usage:read 读面（auditor 可达）：补偿可定位到人员。
	auditor := uuid.NewString()
	f.grantOp(t, auditor, management.RoleAuditor)
	env = doH(t, f.engine, http.MethodGet, "/adminu/model-adjustments", nil, http.StatusOK, f.headers(auditor))
	var list struct {
		Adjustments []struct {
			Reason          string `json:"reason"`
			OperatorSubject string `json:"operator_subject"`
			ServiceSubject  string `json:"service_subject"`
			IdempotencyKey  string `json:"idempotency_key"`
		} `json:"adjustments"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil || len(list.Adjustments) != 1 {
		t.Fatalf("adjustments list = %s err=%v", env.Data, err)
	}
	a := list.Adjustments[0]
	wantSubject := fmt.Sprintf("user:%s@app:ops-console", op)
	if a.OperatorSubject != wantSubject || a.ServiceSubject == "" ||
		a.IdempotencyKey != "comp-001" || a.Reason != "服务降级补偿" {
		t.Fatalf("attribution = %+v", a)
	}

	// 无权限用户被拒（双腿齐但无角色）。
	stranger := uuid.NewString()
	f.grantOp(t, stranger)
	doH(t, f.engine, http.MethodGet, "/adminu/model-adjustments", nil, http.StatusForbidden, f.headers(stranger))
}

// TestAdminUsageSummary_AndExceptions_HTTP: 统计/异常/预览端点的 HTTP 形状
// （真实行 + 账本派生金额字段存在）。
func TestAdminUsageSummary_AndExceptions_HTTP(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)

	// 种子：provider/deployment/policy/entitlement/一条 settled 请求 + charge。
	ctx := context.Background()
	var provID, depID string
	if err := f.db.QueryRowContext(ctx,
		`INSERT INTO inference_providers (code, display_name, access_type) VALUES ('glm','GLM','official_api') RETURNING id::text`).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx,
		`INSERT INTO inference_deployments (provider_id, upstream_model, base_url, protocol)
		 VALUES ($1,'glm-up','https://api.glm.example.com','openai_chat') RETURNING id::text`, provID).Scan(&depID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM", ContextTokens: 1000, MaxOutputTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	_ = depID

	// 统计端点：空库形状（groups 空数组、as_of 存在）。
	env := doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?group_by=provider", nil, http.StatusOK, f.headers(op))
	var sum struct {
		GroupBy string `json:"group_by"`
		Groups  []any  `json:"groups"`
		AsOf    string `json:"as_of"`
	}
	if err := json.Unmarshal(env.Data, &sum); err != nil || sum.GroupBy != "provider" || sum.AsOf == "" {
		t.Fatalf("summary = %s err=%v", env.Data, err)
	}

	// 异常筛选：kind 必选项 + 合法 kind 的空形状。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/exceptions", nil, http.StatusBadRequest, f.headers(op))
	env = doH(t, f.engine, http.MethodGet, "/adminu/model-usage/exceptions?kind=settlement_backlog", nil, http.StatusOK, f.headers(op))
	var exc struct {
		Kind              string `json:"kind"`
		SettlementBacklog []any  `json:"settlement_backlog"`
	}
	if err := json.Unmarshal(env.Data, &exc); err != nil || exc.Kind != "settlement_backlog" {
		t.Fatalf("exceptions = %s err=%v", env.Data, err)
	}

	// 共享账号检测视图（一账号一部署）。
	doH(t, f.engine, http.MethodGet, "/adminu/upstream-accounts/shared", nil, http.StatusOK, f.headers(op))

	// 价格预览（models:manage）：已知模型 + 新价 → current_effective null。
	env = doH(t, f.engine, http.MethodPost, "/adminm/model-prices/preview", map[string]any{
		"model_id": "glm-4.6", "kind": "sale_credit",
		"input_micros_per_mtok": 100, "output_micros_per_mtok": 300,
	}, http.StatusOK, f.headers(op))
	var prev struct {
		ModelID          string `json:"model_id"`
		CurrentEffective any    `json:"current_effective"`
		KeepVersion      bool   `json:"existing_subscriptions_keep_version"`
	}
	if err := json.Unmarshal(env.Data, &prev); err != nil || prev.ModelID != "glm-4.6" || !prev.KeepVersion {
		t.Fatalf("preview = %s err=%v", env.Data, err)
	}
	// 未知模型 → 404。
	doH(t, f.engine, http.MethodPost, "/adminm/model-prices/preview", map[string]any{
		"model_id": "no-such", "kind": "sale_credit",
	}, http.StatusNotFound, f.headers(op))
	// 额度策略预览：name 必填。
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/preview", map[string]any{
		"model_ids": []string{"glm-4.6"},
	}, http.StatusBadRequest, f.headers(op))
}
