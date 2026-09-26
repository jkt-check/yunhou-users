// admin_quota_policies_test.go — 配额策略管理端点（/adminm/quota-policies，
// models:manage）HTTP 级验收：真实库 + 真实 OperatorAuthz 中间件（复用
// admin_bulk_test.go 的 opsFixture）。覆盖 spec
// 2026-09-26-admin-quota-policies-design.md §5 验收矩阵 A1–A11：鉴权、
// 逐项校验、supersede 语义、幂等、retire 保护规则、同事务审计、列表/详情。

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

func createQPBody(name, modelID string) map[string]any {
	return map[string]any{
		"name":                name,
		"model_ids":           []string{modelID},
		"weekly_limit_micros": 5_000_000,
		"rpm_limit":           60,
		"concurrency_limit":   4,
		"reason":              "出策略",
	}
}

func decodeQPView(t *testing.T, env envelope) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode quota policy view: %v (%s)", err, env.Data)
	}
	return v
}

// createQPPolicy creates a draft policy via the HTTP surface and returns its id.
func createQPPolicy(t *testing.T, f *opsFixture, h map[string]string, name, modelID string) string {
	t.Helper()
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		createQPBody(name, modelID), http.StatusCreated, h)
	v := decodeQPView(t, env)
	id, _ := v["policy_version_id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %s", env.Data)
	}
	return id
}

// publishQP publishes a draft and asserts 200.
func publishQP(t *testing.T, f *opsFixture, h map[string]string, id string) {
	t.Helper()
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/publish",
		map[string]any{"reason": "发布"}, http.StatusOK, h)
}

// seedQPEntitlement plants an active entitlement referencing the policy
// (pin 语义测试与 retire 保护规则的公共种子)。
func seedQPEntitlement(t *testing.T, f *opsFixture, policyID, modelID string) {
	t.Helper()
	ctx := context.Background()
	userID := uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	acct := &domain.BillingAccount{UserID: userID}
	if err := f.store.InsertBillingAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO inference_entitlements
		 (billing_account_id, source_type, source_id, model_ids, policy_version_id,
		  anchor_at, effective_from, status)
		 VALUES ($1, 'grant', $2, $3, $4, now(), now(), 'active')`,
		acct.ID, "seed-"+uuid.NewString(), "{"+modelID+"}", policyID); err != nil {
		t.Fatal(err)
	}
}

func qpAuditCount(t *testing.T, f *opsFixture, action, objectID string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM inference_audit_log WHERE action = $1 AND object_id = $2`,
		action, objectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A1:新建全新 name → 201,revision=1,draft,默认 reject,审计落库。
func TestAdminQuotaPolicies_Create(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		createQPBody("kaya-gift", "deepseek-chat"), http.StatusCreated, h)
	v := decodeQPView(t, env)
	if v["policy_version_id"] == "" || v["name"] != "kaya-gift" {
		t.Fatalf("view = %s", env.Data)
	}
	if v["revision"] != float64(1) || v["status"] != "draft" {
		t.Fatalf("revision/status = %v/%v, want 1/draft", v["revision"], v["status"])
	}
	if v["overage_policy"] != "reject" {
		t.Fatalf("overage_policy = %v, want reject (default)", v["overage_policy"])
	}
	// micros/tpm string 渲染(int64 精度安全);rpm/concurrency 数字。
	if v["weekly_limit_micros"] != "5000000" {
		t.Fatalf("weekly_limit_micros = %v, want string \"5000000\"", v["weekly_limit_micros"])
	}
	if _, present := v["five_hour_limit_micros"]; present && v["five_hour_limit_micros"] != nil {
		t.Fatalf("five_hour_limit_micros = %v, want null/absent", v["five_hour_limit_micros"])
	}
	if v["rpm_limit"] != float64(60) || v["concurrency_limit"] != float64(4) {
		t.Fatalf("rpm/conc = %v/%v", v["rpm_limit"], v["concurrency_limit"])
	}
	if _, present := v["published_at"]; present && v["published_at"] != nil {
		t.Fatalf("draft must have no published_at: %s", env.Data)
	}

	id, _ := v["policy_version_id"].(string)
	if n := qpAuditCount(t, f, "quota_policy.create", id); n != 1 {
		t.Fatalf("create audit rows = %d, want 1 (写+审计同事务)", n)
	}
}

// A2:同 name 再建 → revision = max+1,draft。
func TestAdminQuotaPolicies_CreateNextRevision(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		createQPBody("kaya-gift", "deepseek-chat"), http.StatusCreated, h)
	v := decodeQPView(t, env)
	if v["revision"] != float64(2) || v["status"] != "draft" {
		t.Fatalf("revision/status = %v/%v, want 2/draft", v["revision"], v["status"])
	}
}

// A3:model_ids 含不存在模型 → 400,消息指明哪个 id。
func TestAdminQuotaPolicies_CreateUnknownModel(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	body := createQPBody("kaya-gift", "deepseek-chat")
	body["model_ids"] = []string{"deepseek-chat", "ghost-model"}
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		body, http.StatusBadRequest, h)
	if !strings.Contains(env.Message, "ghost-model") && !strings.Contains(string(env.Data), "ghost-model") {
		t.Fatalf("error must name the unknown model id: msg=%s data=%s", env.Message, env.Data)
	}
}

// A4:全部 limit 缺省 → 400「策略无任何限制项」。
func TestAdminQuotaPolicies_CreateNoLimits(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		map[string]any{"name": "kaya-gift", "model_ids": []string{"deepseek-chat"}, "reason": "x"},
		http.StatusBadRequest, h)
	if !strings.Contains(string(env.Message), "限制") && !strings.Contains(string(env.Data), "限制") {
		t.Fatalf("want '无任何限制项' style 400: msg=%s data=%s", env.Message, env.Data)
	}
}

// name 格式校验。
func TestAdminQuotaPolicies_CreateNameValidation(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	for _, bad := range []string{"", "Kaya-Gift", "-bad", "bad name", "toolongtoolongtoolongtoolongtoolongtoolongtoolongtoolongtoolongtoolongtoolong"} {
		body := createQPBody(bad, "deepseek-chat")
		doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
			body, http.StatusBadRequest, h)
	}
}

// A5:PATCH published 版本 → 409。
func TestAdminQuotaPolicies_PatchPublished409(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, id)
	doH(t, f.engine, http.MethodPatch, "/adminm/quota-policies/"+id,
		map[string]any{"rpm_limit": 120, "reason": "改"}, http.StatusConflict, h)
}

// draft 可改;空修改 → 400;审计落库。
func TestAdminQuotaPolicies_PatchDraft(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")
	seedPVModel(t, f, "deepseek-flash")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	// 空修改(只有 reason)→ 400。
	doH(t, f.engine, http.MethodPatch, "/adminm/quota-policies/"+id,
		map[string]any{"reason": "空"}, http.StatusBadRequest, h)
	// 真实修改:换 model 集 + 调 rpm + 清掉 concurrency(显式 null)。
	env := doH(t, f.engine, http.MethodPatch, "/adminm/quota-policies/"+id,
		map[string]any{
			"model_ids":         []string{"deepseek-chat", "deepseek-flash"},
			"rpm_limit":         120,
			"concurrency_limit": nil,
			"reason":            "收编 flash,放开并发",
		}, http.StatusOK, h)
	v := decodeQPView(t, env)
	if v["rpm_limit"] != float64(120) {
		t.Fatalf("rpm_limit = %v, want 120", v["rpm_limit"])
	}
	if ids, _ := v["model_ids"].([]any); len(ids) != 2 {
		t.Fatalf("model_ids = %v, want 2 entries", v["model_ids"])
	}
	if cl, present := v["concurrency_limit"]; present && cl != nil {
		t.Fatalf("concurrency_limit = %v, want null after explicit clear", cl)
	}
	// 未提及的字段保留:weekly 仍是 5M。
	if v["weekly_limit_micros"] != "5000000" {
		t.Fatalf("weekly_limit_micros = %v, want unchanged 5000000", v["weekly_limit_micros"])
	}
	if n := qpAuditCount(t, f, "quota_policy.update", id); n != 1 {
		t.Fatalf("update audit rows = %d, want 1", n)
	}
}

// A6:publish 新版 → 同名旧 published 转 superseded;每 name 至多一条 published。
func TestAdminQuotaPolicies_PublishSupersedes(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	r1 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r1)
	r2 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r2)

	var st1, st2 string
	if err := f.db.QueryRow(`SELECT status FROM inference_policy_versions WHERE id = $1`, r1).Scan(&st1); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT status FROM inference_policy_versions WHERE id = $1`, r2).Scan(&st2); err != nil {
		t.Fatal(err)
	}
	if st1 != "superseded" || st2 != "published" {
		t.Fatalf("r1/r2 status = %s/%s, want superseded/published", st1, st2)
	}
	var pub int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM inference_policy_versions WHERE name = 'kaya-gift' AND status = 'published'`).Scan(&pub); err != nil || pub != 1 {
		t.Fatalf("published per name = %d err=%v, want 1", pub, err)
	}
	if n := qpAuditCount(t, f, "quota_policy.publish", r1); n != 1 {
		t.Fatalf("r1 publish audit = %d, want 1", n)
	}
	if n := qpAuditCount(t, f, "quota_policy.publish", r2); n != 1 {
		t.Fatalf("r2 publish audit = %d, want 1", n)
	}
	// published_at 记录。
	env := doH(t, f.engine, http.MethodGet, "/adminm/quota-policies/"+r2, nil, http.StatusOK, h)
	v := decodeQPView(t, env)
	if _, present := v["published_at"]; !present || v["published_at"] == nil {
		t.Fatalf("published policy must carry published_at: %s", env.Data)
	}
}

// A7:重复 publish 同 id → 200 幂等(返回当前状态),审计仍仅一条。
func TestAdminQuotaPolicies_PublishIdempotent(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, id)
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/publish",
		map[string]any{"reason": "重放"}, http.StatusOK, h)
	v := decodeQPView(t, env)
	if v["status"] != "published" {
		t.Fatalf("status = %v, want published (idempotent replay)", v["status"])
	}
	if n := qpAuditCount(t, f, "quota_policy.publish", id); n != 1 {
		t.Fatalf("publish audit rows = %d, want exactly 1 (no duplicate audit on replay)", n)
	}
}

// superseded/retired 版本不可再 publish。
func TestAdminQuotaPolicies_PublishNonDraft409(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	r1 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r1)
	r2 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r2) // r1 → superseded
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+r1+"/publish",
		map[string]any{"reason": "复活 superseded"}, http.StatusConflict, h)
}

// A8:retire 有 active 权益引用(默认)→ 409 + referenced_by;force 放行,
// 存量权益 pin 不变。
func TestAdminQuotaPolicies_RetireReferenced409(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, id)
	seedQPEntitlement(t, f, id, "deepseek-chat")

	// 默认:409 + referenced_by = 1。
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire",
		map[string]any{"reason": "退役"}, http.StatusConflict, h)
	if !strings.Contains(string(env.Data), "referenced_by") {
		t.Fatalf("409 data must carry referenced_by: %s", env.Data)
	}
	var stillPub string
	if err := f.db.QueryRow(`SELECT status FROM inference_policy_versions WHERE id = $1`, id).Scan(&stillPub); err != nil || stillPub != "published" {
		t.Fatalf("status after rejected retire = %s err=%v, want published", stillPub, err)
	}

	// force=true → 200 retired;存量权益仍 pin 该版本(pin 不变性)。
	env = doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire?force=true",
		map[string]any{"reason": "知悉影响,强制退役"}, http.StatusOK, h)
	v := decodeQPView(t, env)
	if v["status"] != "retired" {
		t.Fatalf("status = %v, want retired (forced)", v["status"])
	}
	var pinned string
	if err := f.db.QueryRow(
		`SELECT policy_version_id FROM inference_entitlements WHERE policy_version_id = $1`, id).Scan(&pinned); err != nil || pinned != id {
		t.Fatalf("entitlement pin = %s err=%v, want unchanged %s", pinned, err, id)
	}
	if n := qpAuditCount(t, f, "quota_policy.retire", id); n != 1 {
		t.Fatalf("retire audit rows = %d, want 1 (被拒的 409 不落审计)", n)
	}
}

// A9:retire draft → 200(等同废弃草稿);重复 retire → 200 幂等,审计仅一条。
func TestAdminQuotaPolicies_RetireDraft(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	env := doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire",
		map[string]any{"reason": "废弃草稿"}, http.StatusOK, h)
	if v := decodeQPView(t, env); v["status"] != "retired" {
		t.Fatalf("status = %v, want retired", v["status"])
	}
	// 幂等重放。
	env = doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire",
		map[string]any{"reason": "重放"}, http.StatusOK, h)
	if v := decodeQPView(t, env); v["status"] != "retired" {
		t.Fatalf("status = %v, want retired (replay)", v["status"])
	}
	if n := qpAuditCount(t, f, "quota_policy.retire", id); n != 1 {
		t.Fatalf("retire audit rows = %d, want exactly 1", n)
	}
}

// superseded 版本 retire 同样走引用保护(保守扩展,见 spec §3)。
func TestAdminQuotaPolicies_RetireSupersededGuard(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	r1 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r1)
	seedQPEntitlement(t, f, r1, "deepseek-chat")
	r2 := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, r2) // r1 → superseded,权益仍 pin r1

	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+r1+"/retire",
		map[string]any{"reason": "退役 superseded"}, http.StatusConflict, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+r1+"/retire?force=true",
		map[string]any{"reason": "强制"}, http.StatusOK, h)
}

// A10:缺 reason 的任何写 → 400。
func TestAdminQuotaPolicies_ReasonRequired(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	body := createQPBody("kaya-gift", "deepseek-chat")
	delete(body, "reason")
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies", body, http.StatusBadRequest, h)

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	doH(t, f.engine, http.MethodPatch, "/adminm/quota-policies/"+id,
		map[string]any{"rpm_limit": 90}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/publish",
		map[string]any{}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire",
		map[string]any{}, http.StatusBadRequest, h)
}

// A11:鉴权 —— 无身份 401;只有 usage:read 的运营 403。
func TestAdminQuotaPolicies_Authz(t *testing.T) {
	f := newOpsFixture(t)
	seedPVModel(t, f, "deepseek-chat")

	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		createQPBody("kaya-gift", "deepseek-chat"), http.StatusUnauthorized, map[string]string{})

	usageOnly := uuid.NewString()
	f.grantOp(t, usageOnly, management.RoleAuditor)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies",
		createQPBody("kaya-gift", "deepseek-chat"), http.StatusForbidden, f.headers(usageOnly))
}

// 列表/详情:过滤、排序、分页、referenced_by。
func TestAdminQuotaPolicies_ListAndGet(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	a1 := createQPPolicy(t, f, h, "alpha", "deepseek-chat")
	_ = createQPPolicy(t, f, h, "alpha", "deepseek-chat") // alpha r2
	b1 := createQPPolicy(t, f, h, "beta", "deepseek-chat")
	publishQP(t, f, h, b1)
	seedQPEntitlement(t, f, b1, "deepseek-chat")

	// 全量列表:排序 name ASC, revision DESC。
	env := doH(t, f.engine, http.MethodGet, "/adminm/quota-policies", nil, http.StatusOK, h)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("list decode: %v (%s)", err, env.Data)
	}
	if len(list.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(list.Items))
	}
	got := [][2]any{{list.Items[0]["name"], list.Items[0]["revision"]}, {list.Items[1]["name"], list.Items[1]["revision"]}, {list.Items[2]["name"], list.Items[2]["revision"]}}
	if got[0][0] != "alpha" || got[0][1] != float64(2) || got[1][0] != "alpha" || got[1][1] != float64(1) || got[2][0] != "beta" {
		t.Fatalf("sort order wrong (want alpha r2, alpha r1, beta): %v", got)
	}

	// name 精确过滤 + status 过滤。
	env = doH(t, f.engine, http.MethodGet, "/adminm/quota-policies?name=beta&status=published", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0]["policy_version_id"] != b1 {
		t.Fatalf("filtered items = %v, want just beta r1", list.Items)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/quota-policies?status=published", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("published-only items = %d, want 1", len(list.Items))
	}

	// 详情带 referenced_by。
	env = doH(t, f.engine, http.MethodGet, "/adminm/quota-policies/"+b1, nil, http.StatusOK, h)
	v := decodeQPView(t, env)
	if v["referenced_by"] != float64(1) {
		t.Fatalf("referenced_by = %v, want 1", v["referenced_by"])
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/quota-policies/"+a1, nil, http.StatusOK, h)
	v = decodeQPView(t, env)
	if v["referenced_by"] != float64(0) {
		t.Fatalf("referenced_by = %v, want 0", v["referenced_by"])
	}

	// 分页窗口。
	env = doH(t, f.engine, http.MethodGet, "/adminm/quota-policies?limit=2&offset=2", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("paged items = %d, want 1", len(list.Items))
	}
	doH(t, f.engine, http.MethodGet, "/adminm/quota-policies?offset=-1", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminm/quota-policies?status=bogus", nil, http.StatusBadRequest, h)
}

// 非 UUID 的 :id 不得漏成 500(22P02 已映射 invalid_input;评审轮1)。
func TestAdminQuotaPolicies_MalformedID400(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)

	doH(t, f.engine, http.MethodGet, "/adminm/quota-policies/not-a-uuid", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/not-a-uuid/publish",
		map[string]any{"reason": "x"}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/not-a-uuid/retire",
		map[string]any{"reason": "x"}, http.StatusBadRequest, h)
}

// 配置级引用(评审轮3 finding 2):PAYG 配置指向该版本 → 详情带
// referenced_by_configs;retire 默认 409(data 双计数);force 放行。
func TestAdminQuotaPolicies_ConfigRefsGuard(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	id := createQPPolicy(t, f, h, "kaya-gift", "deepseek-chat")
	publishQP(t, f, h, id)
	if _, err := f.store.PutPAYGConfig(context.Background(), id, []string{"deepseek-chat"}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}

	env := doH(t, f.engine, http.MethodGet, "/adminm/quota-policies/"+id, nil, http.StatusOK, h)
	v := decodeQPView(t, env)
	if v["referenced_by_configs"] != float64(1) || v["referenced_by"] != float64(0) {
		t.Fatalf("detail refs = %v/%v, want configs=1 ents=0", v["referenced_by_configs"], v["referenced_by"])
	}

	env = doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire",
		map[string]any{"reason": "退役"}, http.StatusConflict, h)
	if !strings.Contains(string(env.Data), "referenced_by_configs") {
		t.Fatalf("409 data must carry referenced_by_configs: %s", env.Data)
	}
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/"+id+"/retire?force=true",
		map[string]any{"reason": "先退役,配置稍后轮换"}, http.StatusOK, h)
}
