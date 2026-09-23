// admin_oauth_test.go — Task 12 管理端 OAuth 端点真库验收：授权流
// HTTP 全链（authorize→callback→accounts→refresh→revoke）、双重身份授权、
// 一次性 state 409、响应永不携带秘密。

package httpapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers/connector"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

const (
	task12AppID     = "task12-test-app"
	task12AppSecret = "task12-app-secret-plaintext"
	task12Admin     = "55555555-5555-4555-8555-555555555555"
	task12PlainUser = "66666666-6666-4666-8666-666666666666"
)

type task12Env struct {
	engine *gin.Engine
	db     *sqlx.DB
	tok    *service.TokenService
}

// oauthHTTPServer is a minimal vendor for the HTTP-layer flow.
func oauthHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "refresh_token" {
			fmt.Fprint(w, `{"access_token":"at-http-refreshed","refresh_token":"rt-http-new","token_type":"bearer","expires_in":3600}`)
			return
		}
		if r.Form.Get("code") != "good-code" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"at-http","refresh_token":"rt-http","token_type":"bearer","expires_in":3600,"account_id":"ext-http-1"}`)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func setupTask12(t *testing.T, vendor *httptest.Server) *task12Env {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := &service.TokenService{
		PrivateKey: key, PublicKey: &key.PublicKey, AccessTTL: time.Minute, RefreshTTL: time.Hour,
	}
	hash, err := util.HashSecret(task12AppSecret)
	if err != nil {
		t.Fatal(err)
	}
	seed := []string{
		fmt.Sprintf(`INSERT INTO apps (app_id, name, is_active, secret_hash)
			VALUES ('%s','task12',true,'%s')
			ON CONFLICT (app_id) DO UPDATE SET secret_hash = EXCLUDED.secret_hash, is_active = true`, task12AppID, hash),
		fmt.Sprintf(`INSERT INTO users (id, status) VALUES ('%s','active'),('%s','active')
			ON CONFLICT (id) DO NOTHING`, task12Admin, task12PlainUser),
		fmt.Sprintf(`INSERT INTO operator_roles (user_id, role, reason) VALUES ('%s','admin','seed')
			ON CONFLICT (user_id, role) DO NOTHING`, task12Admin),
		`INSERT INTO inference_providers (code, display_name, access_type)
			VALUES ('task12http','Task12 HTTP Provider','oauth_connector')
			ON CONFLICT (code) DO NOTHING`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM inference_session_bindings WHERE account_id IN (SELECT id FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code='task12http'))`)
		db.Exec(`DELETE FROM inference_oauth_grants WHERE connector = 'testvendor'`)
		db.Exec(`DELETE FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code='task12http')`)
		db.Exec(`DELETE FROM inference_credentials WHERE provider_id IN (SELECT id FROM inference_providers WHERE code='task12http')`)
		db.Exec(`DELETE FROM inference_audit_log WHERE actor_app_id = 'task12-test-app'`)
		db.Exec(`DELETE FROM inference_providers WHERE code = 'task12http'`)
	})

	vkey := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(vkey); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: vkey}, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db)
	credSvc := credentials.NewService(vault, store, store)
	registry := connector.Registry{"testvendor": connector.Spec{
		Key:          "testvendor",
		AuthorizeURL: vendor.URL + "/authorize",
		TokenURL:     vendor.URL + "/token",
		RevokeURL:    vendor.URL + "/revoke",
		ClientID:     "cid-http",
		RedirectURL:  "https://ops.example/admin/oauth/callback",
	}}
	client := &connector.Client{HTTP: vendor.Client()}
	oauthSvc := credentials.NewOAuthService(vault, store, store, client, registry, credSvc, nil)
	refresher := credentials.NewRefresher(vault, store, store, client, registry, nil)
	oauthHandler := httpapi.NewAdminOAuthHandler(oauthSvc, refresher, store)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	adminGroup := engine.Group("/admin")
	adminGroup.Use(middleware.InternalAppAuth(repo.NewAppRepo(db)))
	ops := adminGroup.Group("")
	ops.Use(middleware.JWTAuth(tok))
	oauthHandler.Register(ops.Group("", httpapi.OperatorAuthz(store, management.PermCredentialsManage)))
	return &task12Env{engine: engine, db: db, tok: tok}
}

func (e *task12Env) headers(t *testing.T, userID string) map[string]string {
	t.Helper()
	token, err := e.tok.SignAccessToken(userID, task12AppID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"Authorization": "Bearer " + token,
		"X-App-ID":      task12AppID,
		"X-App-Secret":  task12AppSecret,
	}
}

func getJSON(t *testing.T, engine *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestAdminOAuthFlowEndToEnd(t *testing.T) {
	vendor := oauthHTTPServer(t)
	env := setupTask12(t, vendor)
	hdrs := env.headers(t, task12Admin)

	var providerID string
	if err := env.db.Get(&providerID, `SELECT id FROM inference_providers WHERE code = 'task12http'`); err != nil {
		t.Fatal(err)
	}

	// 1. 授权开始。
	w := postJSON(t, env.engine, "/admin/oauth/authorizations", hdrs, fmt.Sprintf(
		`{"provider_id":%q,"connector":"testvendor","account_label":"pool-1","reason":"onboard"}`, providerID))
	if w.Code != http.StatusCreated {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "code_verifier") || strings.Contains(w.Body.String(), "client_secret") {
		t.Fatalf("response leaks secret material: %s", w.Body.String())
	}
	var start struct {
		Data struct {
			AuthorizeURL string `json:"authorize_url"`
			State        string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(start.Data.AuthorizeURL, "code_challenge=") {
		t.Fatalf("authorize url missing PKCE challenge: %s", start.Data.AuthorizeURL)
	}

	// 2. 回调（一次性）。
	w = postJSON(t, env.engine, "/admin/oauth/callback", hdrs, fmt.Sprintf(
		`{"state":%q,"code":"good-code","reason":"complete"}`, start.Data.State))
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "at-http") || strings.Contains(w.Body.String(), "rt-http") {
		t.Fatalf("callback response leaks tokens: %s", w.Body.String())
	}
	var cb struct {
		Data struct {
			Credential credentials.View `json:"credential"`
			AccountID  string           `json:"account_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cb); err != nil {
		t.Fatal(err)
	}
	credID := cb.Data.Credential.ID
	if cb.Data.Credential.AuthType != "oauth" || cb.Data.AccountID == "" {
		t.Fatalf("unexpected callback result: %+v", cb.Data)
	}

	// 回调重放 → 409。
	w = postJSON(t, env.engine, "/admin/oauth/callback", hdrs, fmt.Sprintf(
		`{"state":%q,"code":"good-code","reason":"replay"}`, start.Data.State))
	if w.Code != http.StatusConflict {
		t.Fatalf("callback replay must 409: %d %s", w.Code, w.Body.String())
	}

	// 3. 账号池视图（运营可见上游额度缓存字段；未知 = 字段缺席）。
	w = getJSON(t, env.engine, "/admin/upstream-accounts?provider_id="+providerID, hdrs)
	if w.Code != http.StatusOK {
		t.Fatalf("list accounts: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), cb.Data.AccountID) {
		t.Fatalf("account missing from pool view: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ciphertext") || strings.Contains(w.Body.String(), "at-http") {
		t.Fatalf("pool view leaks secrets: %s", w.Body.String())
	}

	// 4. 手工刷新（同一锁+CAS 协议）。
	w = postJSON(t, env.engine, "/admin/oauth/credentials/"+credID+"/refresh", hdrs, `{"reason":"manual"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	var ro struct {
		Data credentials.RefreshOutcome `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ro); err != nil {
		t.Fatal(err)
	}
	if !ro.Data.Rotated || ro.Data.Generation != 2 {
		t.Fatalf("manual refresh must rotate to gen 2: %+v", ro.Data)
	}

	// 5. 撤销：凭据 revoked + 账号 disabled。
	w = postJSON(t, env.engine, "/admin/oauth/credentials/"+credID+"/revoke", hdrs, `{"reason":"offboard"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	var acctStatus, credStatus string
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, cb.Data.AccountID).Scan(&acctStatus)
	env.db.QueryRow(`SELECT status FROM inference_credentials WHERE id = $1`, credID).Scan(&credStatus)
	if credStatus != "revoked" || acctStatus != "disabled" {
		t.Fatalf("revoke propagation failed: cred=%s acct=%s", credStatus, acctStatus)
	}
}

func TestAdminOAuthAuthorizationGate(t *testing.T) {
	vendor := oauthHTTPServer(t)
	env := setupTask12(t, vendor)

	var providerID string
	env.db.Get(&providerID, `SELECT id FROM inference_providers WHERE code = 'task12http'`)

	// 无 JWT → 401。
	w := postJSON(t, env.engine, "/admin/oauth/authorizations",
		map[string]string{"X-App-ID": task12AppID, "X-App-Secret": task12AppSecret},
		fmt.Sprintf(`{"provider_id":%q,"connector":"testvendor","reason":"x"}`, providerID))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no JWT must 401: %d", w.Code)
	}
	// 无角色用户 → 403。
	w = postJSON(t, env.engine, "/admin/oauth/authorizations", env.headers(t, task12PlainUser),
		fmt.Sprintf(`{"provider_id":%q,"connector":"testvendor","reason":"x"}`, providerID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("plain user must 403: %d %s", w.Code, w.Body.String())
	}
	// 未知连接器 → 400。
	w = postJSON(t, env.engine, "/admin/oauth/authorizations", env.headers(t, task12Admin),
		fmt.Sprintf(`{"provider_id":%q,"connector":"nope","reason":"x"}`, providerID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown connector must 400: %d", w.Code)
	}
	// 请求体走私未知字段 → 400（strict decode）。
	w = postJSON(t, env.engine, "/admin/oauth/authorizations", env.headers(t, task12Admin),
		fmt.Sprintf(`{"provider_id":%q,"connector":"testvendor","reason":"x","actor":"spoof"}`, providerID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field must 400: %d", w.Code)
	}
}
