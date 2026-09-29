package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
)

// admin_entitlements_test.go — 权益增补 admin 面 HTTP 行为
// （entitlement-model-amendment R1–R5；真实库 + 真实权限中间件，复用
// admin_bulk_test.go 的 opsFixture：写面挂 /adminb（billing:adjust），
// 读面挂 /adminu（usage:read））。

// seedEnt 造 customer 用户 + 计费账户 + 策略版本 + 权益行，返回权益 ID。
func seedEnt(t *testing.T, f *opsFixture, models []string) (accountID, entID string) {
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
	lim := domain.Microcredit(1_000_000)
	pol := &inferencepostgres.PolicyVersion{
		Name: "pv-" + uuid.NewString(), Revision: 1, ModelIDs: models,
		FiveHourLimit: &lim, WeeklyLimit: &lim, MonthlyLimit: &lim,
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: models,
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := f.store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	return acct.ID, ent.ID
}

// seedEntOnPlan 同 seedEnt，但权益来源指向真实 subscriptions 行
// （source_plan 选择器的 join 目标）。
func seedEntOnPlan(t *testing.T, f *opsFixture, planID, subStatus, entStatus string, models []string) string {
	t.Helper()
	if _, err := f.db.Exec(
		`INSERT INTO plans (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`, planID); err != nil {
		t.Fatal(err)
	}
	accountID, entID := seedEnt(t, f, models)
	var userID string
	if err := f.db.Get(&userID,
		`SELECT user_id FROM inference_billing_accounts WHERE id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	subID := uuid.NewString()
	if _, err := f.db.Exec(
		`INSERT INTO subscriptions (id, user_id, plan_id, status) VALUES ($1, $2, $3, $4)`,
		subID, userID, planID, subStatus); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(
		`UPDATE inference_entitlements SET source_id = $2, status = $3 WHERE id = $1`,
		entID, subID, entStatus); err != nil {
		t.Fatal(err)
	}
	return entID
}

func entModelIDs(t *testing.T, f *opsFixture, entID string) []string {
	t.Helper()
	var ids pq.StringArray
	if err := f.db.Get(&ids,
		`SELECT model_ids FROM inference_entitlements WHERE id = $1`, entID); err != nil {
		t.Fatal(err)
	}
	return []string(ids)
}

func TestAdminEntitlements_ReviseHTTPFlow(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)

	_, entID := seedEnt(t, f, []string{"m-a"})

	// 正常修订：200 changed=true，集合合并，revision+1。
	env := doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-b"}, "reason": "发布 m-b"}, http.StatusOK, h)
	var resp struct {
		Changed     bool `json:"changed"`
		Entitlement struct {
			ID       string   `json:"id"`
			ModelIDs []string `json:"model_ids"`
			Revision int      `json:"revision"`
			Status   string   `json:"status"`
		} `json:"entitlement"`
	}
	if err := json.Unmarshal(env.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Changed || resp.Entitlement.Revision != 2 ||
		len(resp.Entitlement.ModelIDs) != 2 || resp.Entitlement.ModelIDs[1] != "m-b" {
		t.Fatalf("revise resp = %s", env.Data)
	}

	// 幂等（AC3 单行版）：重复同一增补 → 200 changed=false，revision 不动。
	env = doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-b"}, "reason": "重试"}, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Changed || resp.Entitlement.Revision != 2 {
		t.Fatalf("idempotent re-run = %s, want changed=false revision=2", env.Data)
	}

	// 审计归因（AC5）：一行 entitlement.revise，含前后集合与 revision。
	var n int
	if err := f.db.Get(&n,
		`SELECT COUNT(*) FROM inference_audit_log WHERE action = 'entitlement.revise' AND object_id = $1`, entID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1（幂等重试不落审计）", n)
	}
	var detail string
	if err := f.db.Get(&detail,
		`SELECT detail::text FROM inference_audit_log WHERE action = 'entitlement.revise' AND object_id = $1`, entID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before_model_ids", "after_model_ids", "m-a", "m-b", "before_revision", "after_revision"} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail missing %q: %s", want, detail)
		}
	}

	// 入参形状：缺 reason / add+remove 全空 / 交集 / 未知字段 → 400。
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-c"}}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"reason": "r"}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-x"}, "remove_model_ids": []string{"m-x"}, "reason": "r"},
		http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-c"}, "reason": "r", "actor": "spoof"},
		http.StatusBadRequest, h)

	// 不存在 → 404；非 active → 409（红线 3）。
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+uuid.NewString()+"/revise",
		map[string]any{"add_model_ids": []string{"m-c"}, "reason": "r"}, http.StatusNotFound, h)
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'revoked' WHERE id = $1`, entID); err != nil {
		t.Fatal(err)
	}
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise",
		map[string]any{"add_model_ids": []string{"m-c"}, "reason": "r"}, http.StatusConflict, h)
}

func TestAdminEntitlements_AmendHTTPFlow(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)

	planA := "plan-amend-" + uuid.NewString()[:8]
	entMissing := seedEntOnPlan(t, f, planA, "active", "active", []string{"m-a"})
	seedEntOnPlan(t, f, planA, "active", "active", []string{"m-a", "m-new"})  // 已含 → skipped
	entRevoked := seedEntOnPlan(t, f, planA, "expired", "revoked", []string{"m-a"}) // 非 active → skipped（AC6）
	seedEnt(t, f, []string{"m-a"})                                              // 别的来源，仅 all_active 命中

	// dry_run 默认 true：预演不落库。
	env := doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m-new", "action": "add", "selector": "all_active", "reason": "发布 m-new"},
		http.StatusOK, h)
	var rep struct {
		DryRun        bool `json:"dry_run"`
		Scanned       int  `json:"scanned"`
		Amended       int  `json:"amended"`
		Skipped       int  `json:"skipped"`
		SkippedDetail []struct {
			EntitlementID string `json:"entitlement_id"`
			Reason        string `json:"reason"`
		} `json:"skipped_detail"`
	}
	if err := json.Unmarshal(env.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Scanned != 3 || rep.Amended != 2 || rep.Skipped != 1 {
		t.Fatalf("dry-run report = %s", env.Data)
	}
	if got := entModelIDs(t, f, entMissing); len(got) != 1 {
		t.Fatalf("dry-run must not write: %v", got)
	}

	// 正式执行：amended=2（缺模型的两行 active），revoked 行不动。
	env = doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m-new", "action": "add", "selector": "all_active",
			"reason": "发布 m-new", "dry_run": false},
		http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.DryRun || rep.Amended != 2 || rep.Skipped != 1 {
		t.Fatalf("commit report = %s", env.Data)
	}
	if got := entModelIDs(t, f, entMissing); len(got) != 2 {
		t.Fatalf("amended row = %v, want m-new added", got)
	}
	if got := entModelIDs(t, f, entRevoked); len(got) != 1 {
		t.Fatalf("revoked row must be untouched: %v", got)
	}

	// AC3：重跑同一增补 → amended=0、skipped 全量、无报错。
	env = doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m-new", "action": "add", "selector": "all_active",
			"reason": "重跑", "dry_run": false},
		http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Amended != 0 || rep.Skipped != 3 {
		t.Fatalf("rerun report = %s, want amended=0 skipped=3", env.Data)
	}

	// AC6：source_plan 选择器命中 revoked 行 → 出现在 skipped 明细、不被修改。
	env = doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m-zzz", "action": "add", "selector": "source_plan:" + planA,
			"reason": "预演", "dry_run": true},
		http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 { // 该 plan 的三行（含 revoked），不含其它来源
		t.Fatalf("source_plan scanned = %d, want 3 (%s)", rep.Scanned, env.Data)
	}
	foundRevoked := false
	for _, d := range rep.SkippedDetail {
		if d.EntitlementID == entRevoked && d.Reason == "not active: revoked" {
			foundRevoked = true
		}
	}
	if !foundRevoked {
		t.Errorf("revoked row missing from skipped_detail: %s", env.Data)
	}

	// 入参校验：非法 action / 非法 selector / 缺 model_id → 400。
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m", "action": "set", "selector": "all_active", "reason": "r"},
		http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"model_id": "m", "action": "add", "selector": "everything", "reason": "r"},
		http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models",
		map[string]any{"action": "add", "selector": "all_active", "reason": "r"},
		http.StatusBadRequest, h)
}

func TestAdminEntitlements_ReadSurface(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)

	accountID, entID := seedEnt(t, f, []string{"m-a"})
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'expired' WHERE id = $1`, entID); err != nil {
		t.Fatal(err)
	}
	_, activeID := seedEnt(t, f, []string{"m-b"})

	// 单读：200 + 全字段视图；不存在 → 404。
	env := doH(t, f.engine, http.MethodGet, "/adminu/entitlements/"+entID, nil, http.StatusOK, h)
	var got struct {
		Entitlement struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"entitlement"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Entitlement.ID != entID || got.Entitlement.Status != "expired" {
		t.Fatalf("get = %s", env.Data)
	}
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements/"+uuid.NewString(), nil, http.StatusNotFound, h)

	// 列表：billing_account_id 过滤、status 过滤、非法 status → 400。
	env = doH(t, f.engine, http.MethodGet, "/adminu/entitlements?billing_account_id="+accountID, nil, http.StatusOK, h)
	var list struct {
		Entitlements []struct {
			ID string `json:"id"`
		} `json:"entitlements"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Entitlements) != 1 || list.Entitlements[0].ID != entID {
		t.Fatalf("account filter = %s", env.Data)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminu/entitlements?status=active", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Entitlements) != 1 || list.Entitlements[0].ID != activeID {
		t.Fatalf("status filter = %s", env.Data)
	}
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements?status=bogus", nil, http.StatusBadRequest, h)
}

// TestAdminEntitlements_AuthMatrix（AC7）：写面 billing:adjust（admin 独有）、
// 读面 usage:read（admin/operator/auditor 均可）；缺身份 401。
func TestAdminEntitlements_AuthMatrix(t *testing.T) {
	f := newOpsFixture(t)
	_, entID := seedEnt(t, f, []string{"m-a"})

	reviseBody := map[string]any{"add_model_ids": []string{"m-x"}, "reason": "r"}
	amendBody := map[string]any{"model_id": "m-x", "action": "add", "selector": "all_active", "reason": "r"}

	// 缺身份 → 401（OperatorAuthz 缺双腿即拒）。
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise", reviseBody, http.StatusUnauthorized, nil)
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements", nil, http.StatusUnauthorized, nil)

	// auditor：读 200、写 403。
	auditor := uuid.NewString()
	f.grantOp(t, auditor, management.RoleAuditor)
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements", nil, http.StatusOK, f.headers(auditor))
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements/"+entID, nil, http.StatusOK, f.headers(auditor))
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise", reviseBody, http.StatusForbidden, f.headers(auditor))
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/amend-models", amendBody, http.StatusForbidden, f.headers(auditor))

	// operator（models:manage/credentials:manage，无 billing:adjust）：写 403、读 200。
	opr := uuid.NewString()
	f.grantOp(t, opr, management.RoleOperator)
	doH(t, f.engine, http.MethodPost, "/adminb/entitlements/"+entID+"/revise", reviseBody, http.StatusForbidden, f.headers(opr))
	doH(t, f.engine, http.MethodGet, "/adminu/entitlements", nil, http.StatusOK, f.headers(opr))
}
