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
	"github.com/yunhou/users/internal/inference/routing"
)

// admin_accounts_test.go — 可调度账号管理端点（POST /upstream-accounts、
// POST /upstream-accounts/:id/status、PATCH /upstream-accounts/:id）的 HTTP
// 级验收：真实库 + 真实 OperatorAuthz 中间件（复用 admin_bulk_test.go 的
// opsFixture，账号面挂 /adminc 组）。覆盖需求文档 §5 的鉴权/创建/幂等/
// 负面/启停/变更条目；吊销→恢复全链路在 tests/e2e 覆盖。

// seedProviderCredential inserts one provider + one credential directly via
// the store (handler 测试不经过凭据端点，密文是测试垃圾字节——本条链路不
// 解密).
func seedProviderCredential(t *testing.T, f *opsFixture, authType, credStatus string) (provID, credID string) {
	t.Helper()
	ctx := context.Background()
	prov := &domain.Provider{
		Code: "p-" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""), DisplayName: "Prov",
		AccessType: domain.AccessOfficialAPI,
	}
	if err := f.store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	cred := &domain.Credential{
		ID: uuid.NewString(), ProviderID: prov.ID, Label: "主 key", AuthType: authType,
		Ciphertext: []byte("test-ciphertext"), KeyVersion: 1, Generation: 1,
	}
	if err := f.store.InsertCredential(ctx, cred); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	if credStatus != "" && credStatus != "active" {
		if err := f.store.SetCredentialStatus(ctx, cred.ID, credStatus); err != nil {
			t.Fatalf("set credential status: %v", err)
		}
	}
	return prov.ID, cred.ID
}

func createAccountBody(provID, credID string) map[string]any {
	return map[string]any{
		"provider_id": provID, "credential_id": credID, "reason": "接入静态 key",
	}
}

type accountViewResp struct {
	ID                string `json:"id"`
	ProviderID        string `json:"provider_id"`
	CredentialID      string `json:"credential_id"`
	ExternalAccountID string `json:"external_account_id"`
	DisplayName       string `json:"display_name"`
	Status            string `json:"status"`
	ConcurrencyLimit  int    `json:"concurrency_limit"`
}

func decodeAccountView(t *testing.T, env envelope) accountViewResp {
	t.Helper()
	var v accountViewResp
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode account view: %v (%s)", err, env.Data)
	}
	return v
}

func auditCount(t *testing.T, f *opsFixture, action, objectID string) int {
	t.Helper()
	var n int
	if err := f.db.Get(&n,
		`SELECT COUNT(*) FROM inference_audit_log WHERE action = $1 AND object_id = $2`,
		action, objectID); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdminUpstreamAccountsCreate(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")

	env := doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusCreated, h)
	v := decodeAccountView(t, env)
	if v.ID == "" || v.ProviderID != provID || v.CredentialID != credID {
		t.Fatalf("view = %+v", v)
	}
	if v.Status != "active" || v.ConcurrencyLimit != 1 || v.DisplayName != "主 key" {
		t.Fatalf("view defaults = %+v, want active/1/credential label", v)
	}

	// DB 行可见（GET 列表与调度读面一致）。
	var status string
	if err := f.db.Get(&status,
		`SELECT status FROM inference_upstream_accounts WHERE id = $1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("db status = %s", status)
	}
	list := doH(t, f.engine, http.MethodGet, "/adminc/upstream-accounts?provider_id="+provID, nil, http.StatusOK, h)
	var listed struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(list.Data, &listed); err != nil || len(listed.Accounts) != 1 {
		t.Fatalf("list = %s err=%v", list.Data, err)
	}

	// 创建即入路由候选池（运行时调度状态，无需 publish）。
	active, err := f.store.ListActiveUpstreamAccounts(context.Background(), provID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != v.ID {
		t.Fatalf("schedulable pool = %+v, want the new account", active)
	}

	// 审计行与创建同事务落库。
	if n := auditCount(t, f, "upstream_account.create", v.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

func TestAdminUpstreamAccountsCreateIdempotentConflict(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")

	first := decodeAccountView(t, doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusCreated, h))

	// 重放同一对 → 409 + 已存在账号视图，DB 无重复行，不追加审计。
	env := doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusConflict, h)
	dup := decodeAccountView(t, env)
	if dup.ID != first.ID {
		t.Fatalf("409 view id = %s, want existing %s", dup.ID, first.ID)
	}
	if !strings.Contains(env.Message, "already exists") {
		t.Fatalf("message = %q", env.Message)
	}
	var n int
	if err := f.db.Get(&n,
		`SELECT COUNT(*) FROM inference_upstream_accounts WHERE provider_id = $1 AND credential_id = $2`,
		provID, credID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if n := auditCount(t, f, "upstream_account.create", first.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (no audit on conflict replay)", n)
	}
}

func TestAdminUpstreamAccountsCreateNegatives(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")

	t.Run("credential missing -> 404", func(t *testing.T) {
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(provID, uuid.NewString()), http.StatusNotFound, h)
	})

	t.Run("provider missing -> 404", func(t *testing.T) {
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(uuid.NewString(), credID), http.StatusNotFound, h)
	})

	t.Run("provider mismatch -> 400", func(t *testing.T) {
		otherProv, _ := seedProviderCredential(t, f, "api_key", "")
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(otherProv, credID), http.StatusBadRequest, h)
	})

	t.Run("oauth credential -> 400", func(t *testing.T) {
		p, c := seedProviderCredential(t, f, "oauth", "")
		env := doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(p, c), http.StatusBadRequest, h)
		if !strings.Contains(env.Message, "oauth") {
			t.Fatalf("message = %q, want oauth hint", env.Message)
		}
	})

	t.Run("revoked credential -> 409", func(t *testing.T) {
		p, c := seedProviderCredential(t, f, "api_key", "revoked")
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(p, c), http.StatusConflict, h)
	})

	t.Run("disabled provider -> 409", func(t *testing.T) {
		p, c := seedProviderCredential(t, f, "api_key", "")
		if _, err := f.db.Exec(`UPDATE inference_providers SET status = 'disabled' WHERE id = $1`, p); err != nil {
			t.Fatal(err)
		}
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			createAccountBody(p, c), http.StatusConflict, h)
	})

	t.Run("unknown field -> 400", func(t *testing.T) {
		body := createAccountBody(provID, credID)
		body["actor"] = "mallory"
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			body, http.StatusBadRequest, h)
	})

	t.Run("missing reason -> 400", func(t *testing.T) {
		body := createAccountBody(provID, credID)
		delete(body, "reason")
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			body, http.StatusBadRequest, h)
	})

	t.Run("negative concurrency -> 400", func(t *testing.T) {
		body := createAccountBody(provID, credID)
		body["concurrency_limit"] = -1
		doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			body, http.StatusBadRequest, h)
	})

	t.Run("explicit zero concurrency is legal", func(t *testing.T) {
		p, c := seedProviderCredential(t, f, "api_key", "")
		body := createAccountBody(p, c)
		body["concurrency_limit"] = 0
		env := doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
			body, http.StatusCreated, h)
		v := decodeAccountView(t, env)
		if v.ConcurrencyLimit != 0 {
			t.Fatalf("concurrency = %d, want explicit 0 (备而不用)", v.ConcurrencyLimit)
		}
		// 0 并发 = 备而不用：行落库 active（运营读面可见），但调度健康面
		// 判为不可调度；候选序排除的断言在 routing/account_pool_test.go
		// (TestAccountPool_ZeroConcurrencySkipped)。
		row, err := f.store.GetUpstreamAccount(context.Background(), v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != domain.AccountActive || row.ConcurrencyLimit != 0 {
			t.Fatalf("stored = %+v, want active/0", row)
		}
		if routing.NewService(f.store, nil, nil).HealthOf(*row).Schedulable {
			t.Fatal("concurrency 0 account must not be schedulable")
		}
	})
}

func TestAdminUpstreamAccountsAuthChain(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	provID, credID := seedProviderCredential(t, f, "api_key", "")

	// 无身份双腿 → 401。
	doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusUnauthorized, nil)

	// 无 credentials:manage 权限（auditor 只有 usage:read）→ 403。
	auditor := uuid.NewString()
	f.grantOp(t, auditor, management.RoleAuditor)
	doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusForbidden, f.headers(auditor))

	// 非运营用户（无角色）→ 403。
	plain := uuid.NewString()
	f.grantOp(t, plain) // 只建用户，无角色
	doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusForbidden, f.headers(plain))
}

func TestAdminUpstreamAccountsStatus(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")
	created := decodeAccountView(t, doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusCreated, h))

	// active → disabled：立即退出调度读面。
	dis := decodeAccountView(t, doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "disabled", "reason": "厂商限流"}, http.StatusOK, h))
	if dis.Status != "disabled" {
		t.Fatalf("status = %s", dis.Status)
	}
	active, err := f.store.ListActiveUpstreamAccounts(context.Background(), provID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("disabled account still schedulable: %+v", active)
	}
	if n := auditCount(t, f, "upstream_account.status", created.ID); n != 1 {
		t.Fatalf("status audit rows = %d, want 1", n)
	}

	// disabled → disabled 幂等：200 + 当前视图，仍记审计（noop）。
	noop := decodeAccountView(t, doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "disabled", "reason": "重复停用"}, http.StatusOK, h))
	if noop.Status != "disabled" {
		t.Fatalf("noop status = %s", noop.Status)
	}
	if n := auditCount(t, f, "upstream_account.status", created.ID); n != 2 {
		t.Fatalf("status audit rows = %d, want 2 (noop audited)", n)
	}

	// disabled → active：恢复调度。
	act := decodeAccountView(t, doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "active", "reason": "恢复"}, http.StatusOK, h))
	if act.Status != "active" {
		t.Fatalf("status = %s", act.Status)
	}
	active, err = f.store.ListActiveUpstreamAccounts(context.Background(), provID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("reactivated account not schedulable: %+v", active)
	}

	// 内部运行时态不可写。
	doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "cooldown", "reason": "x"}, http.StatusBadRequest, h)

	// 未知账号 404。
	doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+uuid.NewString()+"/status",
		map[string]any{"status": "disabled", "reason": "x"}, http.StatusNotFound, h)
}

func TestAdminUpstreamAccountsActivateWithRevokedCredential(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")
	created := decodeAccountView(t, doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusCreated, h))

	// 凭据 revoked：级联停用账号（既有行为），随后激活账号必须 409。
	if err := f.store.SetCredentialStatus(context.Background(), credID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(
		`UPDATE inference_upstream_accounts SET status = 'disabled' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "active", "reason": "restore"}, http.StatusConflict, h)

	// 凭据恢复后激活成功。
	if err := f.store.SetCredentialStatus(context.Background(), credID, "active"); err != nil {
		t.Fatal(err)
	}
	doH(t, f.engine, http.MethodPost,
		"/adminc/upstream-accounts/"+created.ID+"/status",
		map[string]any{"status": "active", "reason": "restore"}, http.StatusOK, h)
}

func TestAdminUpstreamAccountsPatch(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	provID, credID := seedProviderCredential(t, f, "api_key", "")
	created := decodeAccountView(t, doH(t, f.engine, http.MethodPost, "/adminc/upstream-accounts",
		createAccountBody(provID, credID), http.StatusCreated, h))

	// 调并发 + 改名。
	env := doH(t, f.engine, http.MethodPatch, "/adminc/upstream-accounts/"+created.ID,
		map[string]any{"display_name": "deepseek 主账号", "concurrency_limit": 4, "reason": "主 key 调并发"},
		http.StatusOK, h)
	v := decodeAccountView(t, env)
	if v.DisplayName != "deepseek 主账号" || v.ConcurrencyLimit != 4 {
		t.Fatalf("patched view = %+v", v)
	}
	var conc int
	if err := f.db.Get(&conc,
		`SELECT concurrency_limit FROM inference_upstream_accounts WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if conc != 4 {
		t.Fatalf("db concurrency = %d, want 4", conc)
	}
	if n := auditCount(t, f, "upstream_account.update", created.ID); n != 1 {
		t.Fatalf("update audit rows = %d, want 1", n)
	}

	// 绑定不变（不重建）。
	if v.ProviderID != provID || v.CredentialID != credID {
		t.Fatalf("binding changed: %+v", v)
	}

	// 空变更 / 负并发 / 缺 reason → 400；未知账号 → 404。
	doH(t, f.engine, http.MethodPatch, "/adminc/upstream-accounts/"+created.ID,
		map[string]any{"reason": "x"}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPatch, "/adminc/upstream-accounts/"+created.ID,
		map[string]any{"concurrency_limit": -1, "reason": "x"}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPatch, "/adminc/upstream-accounts/"+created.ID,
		map[string]any{"concurrency_limit": 2}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPatch, "/adminc/upstream-accounts/"+uuid.NewString(),
		map[string]any{"concurrency_limit": 2, "reason": "x"}, http.StatusNotFound, h)
}
