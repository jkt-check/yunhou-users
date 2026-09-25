package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRequest builds a headered request for non-POST verbs.
func newRequest(method, path string, headers map[string]string) *http.Request {
	req, _ := http.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// doRaw serves one request on the engine.
func doRaw(engine http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// jsonField extracts one top-level data field from an envelope body.
func jsonField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	v, _ := env.Data[field].(string)
	return v
}

// admin_credential_edges_test.go — Task 16 覆盖率补强：凭据与运营角色端
// 点的读取/状态路径（真实库 + 真实双腿认证；主生命周期已在
// admin_credentials_test.go）。

func TestAdminCredential_ReadAndStatusPaths(t *testing.T) {
	env := setupTask4(t)
	adminH := env.headers(t, task4Admin)

	// provider_id 取自种子行。
	var provID string
	if err := env.db.Get(&provID, `SELECT id::text FROM inference_providers WHERE code = 'task4prov'`); err != nil {
		t.Fatal(err)
	}
	// strictBindJSON 只拒未知字段（binding required 不在其职责）——带齐
	// reason 的正规路径：
	w := postJSON(t, env.engine, "/admin/credentials", adminH,
		`{"provider_id":"`+provID+`","label":"k1","auth_type":"api_key","secret":"sk-1","reason":"seed"}`)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	credID := jsonField(t, w.Body.Bytes(), "id")
	if credID == "" {
		t.Fatalf("create response = %s", w.Body.String())
	}

	// List + Get。
	w = getJSON(t, env.engine, "/admin/credentials?provider_code=task4prov", adminH)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), credID) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = getJSON(t, env.engine, "/admin/credentials/"+credID, adminH)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), credID) {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	// 未知凭据（合法但不存在的 uuid）→ 404。
	w = getJSON(t, env.engine, "/admin/credentials/00000000-0000-0000-0000-000000000000", adminH)
	if w.Code != http.StatusNotFound {
		t.Errorf("get unknown = %d %s, want 404", w.Code, w.Body.String())
	}

	// Test（结构化占位拨测）→ 200/4xx 但非 404。
	w = postJSON(t, env.engine, "/admin/credentials/"+credID+"/test", adminH, `{"reason":"probe"}`)
	if w.Code == http.StatusNotFound {
		t.Errorf("test endpoint missing: %d", w.Code)
	}

	// Rotate（含审计）→ 200。
	w = postJSON(t, env.engine, "/admin/credentials/"+credID+"/rotate", adminH,
		`{"secret":"sk-2","reason":"scheduled rotation"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", w.Code, w.Body.String())
	}

	// SetStatus：disable → 200；非法状态 → 400。
	w = postJSON(t, env.engine, "/admin/credentials/"+credID+"/status", adminH,
		`{"status":"revoked","reason":"compromised"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set status = %d %s", w.Code, w.Body.String())
	}
	w = postJSON(t, env.engine, "/admin/credentials/"+credID+"/status", adminH,
		`{"status":"limbo","reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", w.Code)
	}
}

func TestAdminOperators_RoleAdminPaths(t *testing.T) {
	env := setupTask4(t)
	adminH := env.headers(t, task4Admin)

	// List。
	w := getJSON(t, env.engine, "/admin/operators", adminH)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "operator") {
		t.Fatalf("list operators = %d %s", w.Code, w.Body.String())
	}
	// Grant：正常授予 → 200（幂等 granted=false）。
	w = postJSON(t, env.engine, "/admin/operators", adminH,
		`{"user_id":"`+task4PlainUser+`","role":"auditor","reason":"edge test"}`)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("grant = %d %s", w.Code, w.Body.String())
	}
	// 非法角色 → 400。
	w = postJSON(t, env.engine, "/admin/operators", adminH,
		`{"user_id":"`+task4PlainUser+`","role":"superuser","reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad role = %d, want 400", w.Code)
	}
	// Revoke：存在 → 200；不存在 → 200 revoked=false（幂等）。reason 必填（M-6）。
	req := newRequest(http.MethodDelete, "/admin/operators/"+task4PlainUser+"/roles/auditor?reason=edge+test", adminH)
	w = doRaw(env.engine, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	w = doRaw(env.engine, newRequest(http.MethodDelete, "/admin/operators/"+task4PlainUser+"/roles/auditor?reason=edge+test", adminH))
	if w.Code != http.StatusOK {
		t.Fatalf("re-revoke = %d %s", w.Code, w.Body.String())
	}
	// 缺 reason → 400（M-6）。
	w = doRaw(env.engine, newRequest(http.MethodDelete, "/admin/operators/"+task4PlainUser+"/roles/auditor", adminH))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("revoke without reason = %d, want 400", w.Code)
	}
}
