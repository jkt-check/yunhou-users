package httpapi_test

import (
	"net/http"
	"testing"
)

// admin_oauth_edges_test.go — Task 16 覆盖率补强：OAuth 管理端点的读取
// 面（账号列表过滤/limit、撤销、手动刷新、回调参数校验）。

func TestAdminOAuth_ListRevokeRefreshEdges(t *testing.T) {
	env := setupTask12(t, oauthHTTPServer(t))
	h := env.headers(t, task12Admin)

	// provider_id 必传（400 分支）+ 正常列表（空形状）+ limit 钳制。
	w := getJSON(t, env.engine, "/admin/upstream-accounts", h)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("list without provider = %d %s, want 400", w.Code, w.Body.String())
	}
	var provID string
	if err := env.db.Get(&provID, `SELECT id FROM inference_providers WHERE code = 'task12http'`); err != nil {
		t.Fatal(err)
	}
	w = getJSON(t, env.engine, "/admin/upstream-accounts?provider_id="+provID, h)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = getJSON(t, env.engine, "/admin/upstream-accounts?provider_id="+provID+"&limit=9999", h)
	if w.Code != http.StatusOK {
		t.Fatalf("list clamp = %d %s", w.Code, w.Body.String())
	}

	// 撤销未知凭据 → 404。
	w = postJSON(t, env.engine, "/admin/oauth/credentials/00000000-0000-0000-0000-000000000000/revoke", h,
		`{"reason":"edge"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("revoke unknown = %d %s, want 404", w.Code, w.Body.String())
	}
	// 手动刷新未知凭据 → 404。
	w = postJSON(t, env.engine, "/admin/oauth/credentials/00000000-0000-0000-0000-000000000000/refresh", h,
		`{"reason":"edge"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("refresh unknown = %d %s, want 404", w.Code, w.Body.String())
	}
	// 回调缺字段 → 400。
	w = postJSON(t, env.engine, "/admin/oauth/callback", h, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("callback empty = %d %s, want 400", w.Code, w.Body.String())
	}
	// 授权请求缺 connector → 400。
	w = postJSON(t, env.engine, "/admin/oauth/authorizations", h, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("authorization empty = %d %s, want 400", w.Code, w.Body.String())
	}
	// 未知 connector → 4xx fail closed。
	w = postJSON(t, env.engine, "/admin/oauth/authorizations", h,
		`{"connector":"ghost","account_label":"x"}`)
	if w.Code == http.StatusOK {
		t.Errorf("unknown connector accepted: %s", w.Body.String())
	}
}
