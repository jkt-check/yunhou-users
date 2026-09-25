package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/management"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
)

// admin_surface_coverage_test.go — Task 16 覆盖率补强：运营目录写面
// （models/providers/deployments/routes CRUD + publish/rollback/读面）与
// 钱包运营面（Adjust/Reverse/List/Get/PAYG）的 HTTP 级行为（真实库 +
// 真实权限中间件；复用 admin_bulk_test.go 的 opsFixture）。PATCH 全部走
// "先读 updated_at 版本令牌再写"的乐观锁流程——与生产客户端一致。

// versionToken GETs one admin object and returns its updated_at token.
func versionToken(t *testing.T, f *opsFixture, path string, h map[string]string) string {
	t.Helper()
	env := doH(t, f.engine, http.MethodGet, path, nil, http.StatusOK, h)
	var doc struct {
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(env.Data, &doc); err != nil || doc.UpdatedAt == "" {
		t.Fatalf("version token from %s = %s err=%v", path, env.Data, err)
	}
	return doc.UpdatedAt
}

func TestAdminCatalogWriteSurface_FullLifecycle(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)

	// Provider CRUD（乐观锁 PATCH）。
	env := doH(t, f.engine, http.MethodPost, "/adminm/providers", map[string]any{
		"code": "glm-x", "display_name": "GLM X", "access_type": "official_api", "reason": "seed",
	}, http.StatusOK, h)
	var prov struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(env.Data, &prov); err != nil || prov.ID == "" || prov.UpdatedAt == "" {
		t.Fatalf("create provider = %s err=%v", env.Data, err)
	}
	// provider 无列表/单读端点：版本令牌来自创建响应（乐观锁写约定）。
	doH(t, f.engine, http.MethodPatch, "/adminm/providers/"+prov.ID, map[string]any{
		"code": "glm-x", "display_name": "GLM X2", "access_type": "official_api", "status": "active",
		"updated_at": prov.UpdatedAt, "reason": "rename",
	}, http.StatusOK, h)

	// Model CRUD + 生命周期 + 读面。
	env = doH(t, f.engine, http.MethodPost, "/adminm/models", map[string]any{
		"id": "glm-x-1", "display_name": "GLM X 1", "context_tokens": 1000, "max_output_tokens": 100,
		"protocols": []string{"openai_chat"}, "reason": "seed",
	}, http.StatusOK, h)
	tok := versionToken(t, f, "/adminm/models/glm-x-1", h)
	doH(t, f.engine, http.MethodPatch, "/adminm/models/glm-x-1", map[string]any{
		"display_name": "GLM X 1b", "updated_at": tok, "reason": "rename",
	}, http.StatusOK, h)
	doH(t, f.engine, http.MethodGet, "/adminm/models", nil, http.StatusOK, h)
	tok = versionToken(t, f, "/adminm/models/glm-x-1", h)
	doH(t, f.engine, http.MethodPost, "/adminm/models/glm-x-1/lifecycle", map[string]any{
		"lifecycle": "active", "reason": "activate",
	}, http.StatusOK, h)

	// Deployment CRUD。
	env = doH(t, f.engine, http.MethodPost, "/adminm/deployments", map[string]any{
		"provider_id": prov.ID, "upstream_model": "glm-x-up", "base_url": "https://api.glmx.example.com",
		"protocol": "openai_chat", "reason": "seed",
	}, http.StatusOK, h)
	var dep struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &dep); err != nil || dep.ID == "" {
		t.Fatalf("create deployment = %s err=%v", env.Data, err)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/deployments", nil, http.StatusOK, h)
	var deps struct {
		Deployments []struct {
			ID            string `json:"id"`
			ConfigVersion int    `json:"config_version"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(env.Data, &deps); err != nil || len(deps.Deployments) == 0 {
		t.Fatalf("deployments list = %s err=%v", env.Data, err)
	}
	doH(t, f.engine, http.MethodPatch, "/adminm/deployments/"+dep.ID, map[string]any{
		"provider_id": prov.ID, "upstream_model": "glm-x-up", "base_url": "https://api.glmx.example.com",
		"protocol": "openai_chat", "status": "active",
		"connect_timeout_ms": 5000, "request_timeout_ms": 600000,
		"config_version": deps.Deployments[0].ConfigVersion, "reason": "activate",
	}, http.StatusOK, h)

	// Route CRUD。
	env = doH(t, f.engine, http.MethodPost, "/adminm/models/glm-x-1/routes", map[string]any{
		"deployment_id": dep.ID, "weight": 1, "reason": "seed",
	}, http.StatusOK, h)
	var route struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &route); err != nil || route.ID == "" {
		t.Fatalf("create route = %s err=%v", env.Data, err)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/models/glm-x-1/routes", nil, http.StatusOK, h)
	var routes struct {
		Routes []struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(env.Data, &routes); err != nil || len(routes.Routes) == 0 {
		t.Fatalf("routes list = %s err=%v", env.Data, err)
	}
	// PATCH 先读后写：model/deployment 由服务端带出，客户端只给可变字段
	// + 版本令牌（Task 16 修复前任何 PATCH 都 400）。
	doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+route.ID, map[string]any{
		"weight": 1, "priority": 0,
		"enabled": false, "updated_at": routes.Routes[0].UpdatedAt, "reason": "disable",
	}, http.StatusOK, h)
	doH(t, f.engine, http.MethodDelete, "/adminm/routes/"+route.ID+"?reason=cleanup", nil, http.StatusOK, h)

	// Publish → revisions/active → rollback。
	doH(t, f.engine, http.MethodPost, "/adminm/catalog/publish?reason=ship", map[string]any{}, http.StatusOK, h)
	doH(t, f.engine, http.MethodGet, "/adminm/catalog/revisions", nil, http.StatusOK, h)
	doH(t, f.engine, http.MethodGet, "/adminm/catalog/active", nil, http.StatusOK, h)
	doH(t, f.engine, http.MethodPost, "/adminm/catalog/rollback", map[string]any{
		"to_revision": 1, "reason": "rollback drill",
	}, http.StatusOK, h)

	// 删除路径。
	doH(t, f.engine, http.MethodDelete, "/adminm/deployments/"+dep.ID+"?reason=cleanup", nil, http.StatusOK, h)
	doH(t, f.engine, http.MethodDelete, "/adminm/models/glm-x-1?reason=cleanup", nil, http.StatusOK, h)
	doH(t, f.engine, http.MethodDelete, "/adminm/providers/"+prov.ID+"?reason=cleanup", nil, http.StatusOK, h)

	// 错误映射抽样：未知模型 404。
	doH(t, f.engine, http.MethodGet, "/adminm/models/no-such", nil, http.StatusNotFound, h)
}

func TestAdminWalletSurface_AdjustReverseGetPAYG(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)
	customer := uuid.NewString()
	f.grantOp(t, customer)

	// Adjust（credit）→ 幂等重放 → Get 钱包 → List。
	adjust := map[string]any{
		"user_id": customer, "currency": "CNY", "source": "bonus",
		"direction": "credit", "amount_micros": "8000",
		"reason": "覆盖测试补偿", "idempotency_key": "cov-adj-1",
	}
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", adjust, http.StatusCreated, h)
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", adjust, http.StatusOK, h) // 重放
	doH(t, f.engine, http.MethodGet, "/adminb/wallet/adjustments", nil, http.StatusOK, h)
	env := doH(t, f.engine, http.MethodGet, "/adminb/wallet?user_id="+customer+"&currency=CNY", nil, http.StatusOK, h)
	var wallet struct {
		Wallet struct {
			Currency string `json:"currency"`
		} `json:"wallet"`
	}
	if err := json.Unmarshal(env.Data, &wallet); err != nil || wallet.Wallet.Currency != "CNY" {
		t.Fatalf("wallet get = %s err=%v", env.Data, err)
	}
	// 未知用户 → 400（ensure 账户外键违规如实映射 invalid_input）。
	doH(t, f.engine, http.MethodGet, "/adminb/wallet?user_id="+uuid.NewString()+"&currency=CNY", nil, http.StatusBadRequest, h)

	// Reverse：对刚创建的钱包分录冲正（分录 ID 由账本直读；一条分录至多
	// 一条冲正）。entry_id 走 DecimalInt64 契约（十进制整数字符串）。
	var entryID int64
	if err := f.db.Get(&entryID,
		`SELECT id FROM inference_wallet_entries ORDER BY id DESC LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	reverse := map[string]any{
		"user_id": customer, "entry_id": strconv.FormatInt(entryID, 10),
	}
	env = doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", reverse, http.StatusOK, h)
	var rev struct {
		Reversed bool `json:"reversed"`
	}
	if err := json.Unmarshal(env.Data, &rev); err != nil || !rev.Reversed {
		t.Fatalf("reverse = %s err=%v", env.Data, err)
	}
	env = doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", reverse, http.StatusOK, h) // 重放
	if err := json.Unmarshal(env.Data, &rev); err != nil || rev.Reversed {
		t.Fatalf("reverse replay = %s err=%v (一条分录至多一条冲正)", env.Data, err)
	}

	// PAYG：未发布 → 404；PUT 发布（需既有策略版本）→ GET 200。
	doH(t, f.engine, http.MethodGet, "/adminb/payg-config", nil, http.StatusNotFound, h)
	pol := &inferencepostgres.PolicyVersion{
		Name: "payg-cov", Revision: 1, ModelIDs: []string{"glm-payg"},
		FiveHourLimit: microP(1000), WeeklyLimit: microP(10000), MonthlyLimit: microP(100000),
		Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	doH(t, f.engine, http.MethodPut, "/adminb/payg-config", map[string]any{
		"policy_version_id": pol.ID, "model_ids": []string{"glm-payg"},
	}, http.StatusOK, h)
	env = doH(t, f.engine, http.MethodGet, "/adminb/payg-config", nil, http.StatusOK, h)
	var payg struct {
		ModelIDs []string `json:"model_ids"`
	}
	if err := json.Unmarshal(env.Data, &payg); err != nil {
		t.Fatalf("payg get = %s err=%v", env.Data, err)
	}

	// 校验错误：缺 reason → 400；未知用户 adjust → 404。
	bad := map[string]any{
		"user_id": customer, "currency": "CNY", "source": "bonus",
		"direction": "credit", "amount_micros": "1", "idempotency_key": "cov-adj-bad",
	}
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", bad, http.StatusBadRequest, h)
	ghost := map[string]any{
		"user_id": uuid.NewString(), "currency": "CNY", "source": "bonus",
		"direction": "credit", "amount_micros": "1", "reason": "x", "idempotency_key": "cov-adj-ghost",
	}
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", ghost, http.StatusBadRequest, h)
}

// Task 16 补：Reverse/Get 的参数与冲突分支。
func TestAdminWalletSurface_ReverseGetBranches(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)
	customer := uuid.NewString()
	f.grantOp(t, customer)

	// 先建一笔可冲正分录。
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/adjustments", map[string]any{
		"user_id": customer, "currency": "CNY", "source": "bonus",
		"direction": "credit", "amount_micros": "1000",
		"reason": "branch", "idempotency_key": "cov-branch-adj",
	}, http.StatusCreated, h)
	var entryID int64
	if err := f.db.Get(&entryID, `SELECT id FROM inference_wallet_entries ORDER BY id DESC LIMIT 1`); err != nil {
		t.Fatal(err)
	}

	// entry_id 非十进制 → 400。
	doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", map[string]any{
		"user_id": customer, "entry_id": "12x",
	}, http.StatusBadRequest, h)
	// entry_id 不存在 → 错误（非 200）。
	w := doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", map[string]any{
		"user_id": customer, "entry_id": "99999999",
	}, http.StatusNotFound, h)
	_ = w
	// billing_account_id 形态寻址 + 冲正成功。
	var acctID string
	if err := f.db.Get(&acctID,
		`SELECT id::text FROM inference_billing_accounts WHERE user_id = $1`, customer); err != nil {
		t.Fatal(err)
	}
	env := doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", map[string]any{
		"billing_account_id": acctID, "entry_id": "1000",
	}, http.StatusNotFound, h) // 该 entry 不属于此账户时按不存在处理
	_ = env
	// 正确冲正。
	env = doH(t, f.engine, http.MethodPost, "/adminb/wallet/reversals", map[string]any{
		"billing_account_id": acctID, "entry_id": strconv.FormatInt(entryID, 10),
	}, http.StatusOK, h)
	var rev struct {
		Reversed bool `json:"reversed"`
	}
	if err := json.Unmarshal(env.Data, &rev); err != nil || !rev.Reversed {
		t.Fatalf("reverse by account = %s err=%v", env.Data, err)
	}

	// Get：billing_account_id 寻址 + 缺 currency → 400。
	doH(t, f.engine, http.MethodGet, "/adminb/wallet?billing_account_id="+acctID, nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminb/wallet?billing_account_id="+acctID+"&currency=CNY", nil, http.StatusOK, h)
	// 两参都不给 → 400。
	doH(t, f.engine, http.MethodGet, "/adminb/wallet", nil, http.StatusBadRequest, h)
}
