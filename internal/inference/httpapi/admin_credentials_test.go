package httpapi_test

import (
	"crypto/rand"
	"encoding/hex"
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

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

// Task 4 acceptance suite: the operator write surface behind the verified
// dual identity (user JWT + app secret) with role permissions, AEAD
// ciphertext binding, emergency-disable propagation, and audit attribution.

const (
	task4AppID     = "task4-test-app"
	task4AppSecret = "task4-app-secret-plaintext"
	task4User      = "11111111-1111-4111-8111-111111111111"
	task4Admin     = "22222222-2222-4222-8222-222222222222"
	task4Auditor   = "33333333-3333-4333-8333-333333333333"
	task4PlainUser = "44444444-4444-4444-8444-444444444444"
)

var task4ModelID = func() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "task4-model-" + hex.EncodeToString(b)
}()

type task4Env struct {
	engine *gin.Engine
	db     *sqlx.DB
	tok    *service.TokenService
}

func setupTask4(t *testing.T) *task4Env {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Same shared-DB wipe as newTestServer: earlier packages (inference/catalog
	// repo tests, Task 3 catalog tests) leave models/providers behind, and
	// publish validates the whole draft — leftovers would fail this suite.
	if _, err := db.Exec(`TRUNCATE
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

	// TokenService over a throwaway RSA key.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := &service.TokenService{
		PrivateKey: key, PublicKey: &key.PublicKey, AccessTTL: time.Minute, RefreshTTL: time.Hour,
	}

	// Seed verified identities: the internal app (with bcrypt secret hash)
	// and four users with different operator roles.
	hash, err := util.HashSecret(task4AppSecret)
	if err != nil {
		t.Fatal(err)
	}
	seed := []string{
		fmt.Sprintf(`INSERT INTO apps (app_id, name, is_active, secret_hash)
			VALUES ('%s','task4',true,'%s')
			ON CONFLICT (app_id) DO UPDATE SET secret_hash = EXCLUDED.secret_hash, is_active = true`, task4AppID, hash),
		fmt.Sprintf(`INSERT INTO users (id, status) VALUES
			('%s','active'),('%s','active'),('%s','active'),('%s','active')
			ON CONFLICT (id) DO NOTHING`, task4User, task4Admin, task4Auditor, task4PlainUser),
		fmt.Sprintf(`INSERT INTO operator_roles (user_id, role, reason) VALUES
			('%s','operator','seed'),('%s','admin','seed'),('%s','auditor','seed')
			ON CONFLICT (user_id, role) DO NOTHING`, task4User, task4Admin, task4Auditor),
		`INSERT INTO inference_providers (code, display_name, access_type)
			VALUES ('task4prov','Task4 Provider','official_api')
			ON CONFLICT (code) DO NOTHING`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// Vault with a throwaway test key (never a real key).
	keyBytes := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: keyBytes}, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db)
	credSvc := credentials.NewService(vault, store, store)
	catalogSvc := catalog.NewService(store)
	catalogMgr := management.NewCatalogManager(catalogSvc, store, nil) // egress not under test here
	modelsHandler := httpapi.NewAdminModelsHandler(catalogMgr)
	adminOps := &httpapi.AdminOps{
		RequireModels:      httpapi.OperatorAuthz(store, management.PermModelsManage),
		RequireCredentials: httpapi.OperatorAuthz(store, management.PermCredentialsManage),
		RequireAdmin:       httpapi.OperatorRequireRole(store, management.RoleAdmin),
		Models:             modelsHandler,
		Credentials:        httpapi.NewAdminCredentialsHandler(credSvc),
		Auth:               httpapi.NewAdminAuthHandler(store, store),
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	adminGroup := engine.Group("/admin")
	adminGroup.Use(middleware.InternalAppAuth(repo.NewAppRepo(db)))
	{
		modelsHandler.RegisterReadOnly(adminGroup)
		ops := adminGroup.Group("")
		ops.Use(middleware.JWTAuth(tok))
		adminOps.Mount(ops)
	}
	return &task4Env{engine: engine, db: db, tok: tok}
}

func (e *task4Env) headers(t *testing.T, userID string) map[string]string {
	t.Helper()
	token, err := e.tok.SignAccessToken(userID, task4AppID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"Authorization": "Bearer " + token,
		"X-App-ID":      task4AppID,
		"X-App-Secret":  task4AppSecret,
	}
}

func postJSON(t *testing.T, engine *gin.Engine, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestCredentialLifecycleAuthorized(t *testing.T) {
	env := setupTask4(t)
	hdrs := env.headers(t, task4Admin)

	// Create: the secret is accepted but never echoed back.
	secret := "sk-task4-live-secret"
	w := postJSON(t, env.engine, "/admin/credentials", hdrs, fmt.Sprintf(
		`{"provider_id":%q,"label":"prod","auth_type":"api_key","secret":%q,"reason":"onboard"}`,
		task4ProviderID(t, env.db), secret))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("create response leaks the plaintext secret")
	}
	var created struct {
		Data credentials.View `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.KeyVersion != 1 || created.Data.Generation != 1 || created.Data.Status != "active" {
		t.Fatalf("unexpected view: %+v", created.Data)
	}

	// DB row holds ciphertext, not plaintext.
	var ct []byte
	if err := env.db.Get(&ct, `SELECT ciphertext FROM inference_credentials WHERE id = $1`, created.Data.ID); err != nil {
		t.Fatal(err)
	}
	if len(ct) == 0 || strings.Contains(string(ct), secret) {
		t.Fatal("stored row must hold AEAD ciphertext only")
	}

	// Audit row: dual attribution + object + reason, no secret material.
	var audit struct {
		ActorUserID *string         `db:"actor_user_id"`
		ActorAppID  string          `db:"actor_app_id"`
		Action      string          `db:"action"`
		ObjectID    string          `db:"object_id"`
		Reason      string          `db:"reason"`
		Detail      json.RawMessage `db:"detail"`
	}
	err := env.db.Get(&audit,
		`SELECT actor_user_id, actor_app_id, action, object_id, reason, detail
		   FROM inference_audit_log WHERE action = 'credential.create' AND object_id = $1`, created.Data.ID)
	if err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if audit.ActorUserID == nil || *audit.ActorUserID != task4Admin || audit.ActorAppID != task4AppID {
		t.Fatalf("dual attribution missing: %+v", audit)
	}
	if audit.Reason != "onboard" || strings.Contains(string(audit.Detail), secret) {
		t.Fatalf("audit reason/secret: %+v", audit)
	}

	// Test (decrypt check) → ok.
	w = postJSON(t, env.engine, "/admin/credentials/"+created.Data.ID+"/test", hdrs, `{"reason":"check"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}

	// Rotate → generation 2.
	w = postJSON(t, env.engine, "/admin/credentials/"+created.Data.ID+"/rotate", hdrs,
		fmt.Sprintf(`{"secret":%q,"reason":"scheduled"}`, secret+"-rotated"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"generation":2`) {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}

	// Disable → upstream account propagation + bounded in-flight story.
	env.db.MustExec(`INSERT INTO inference_upstream_accounts (provider_id, credential_id, display_name)
		VALUES ($1, $2, 'acct-1') ON CONFLICT (provider_id, credential_id) DO NOTHING`,
		task4ProviderID(t, env.db), created.Data.ID)
	w = postJSON(t, env.engine, "/admin/credentials/"+created.Data.ID+"/status", hdrs,
		`{"status":"revoked","reason":"compromised"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	var acctStatus string
	env.db.Get(&acctStatus, `SELECT status FROM inference_upstream_accounts WHERE credential_id = $1`, created.Data.ID)
	if acctStatus != "disabled" {
		t.Fatalf("emergency disable did not propagate: %q", acctStatus)
	}
	var auditDetail string
	env.db.Get(&auditDetail,
		`SELECT detail::text FROM inference_audit_log WHERE action = 'credential.disable' AND object_id = $1`, created.Data.ID)
	if !strings.Contains(auditDetail, "upstream_accounts_disabled") {
		t.Fatalf("disable audit missing propagation detail: %s", auditDetail)
	}

	// Resolve-after-disable is refused (gateway hot path would fail closed).
	var postStatus string
	env.db.Get(&postStatus, `SELECT status FROM inference_credentials WHERE id = $1`, created.Data.ID)
	if postStatus != "revoked" {
		t.Fatalf("credential status = %q", postStatus)
	}
}

func TestCredentialEndpointsRejectUnauthorized(t *testing.T) {
	env := setupTask4(t)
	body := fmt.Sprintf(`{"provider_id":%q,"label":"x","auth_type":"api_key","secret":"s","reason":"r"}`,
		task4ProviderID(t, env.db))

	// Plain app credentials (no user JWT) → 401.
	h := map[string]string{"X-App-ID": task4AppID, "X-App-Secret": task4AppSecret}
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("app-only: %d", w.Code)
	}
	// Plain user JWT (no app secret) → 401 at InternalAppAuth.
	h = env.headers(t, task4Admin)
	delete(h, "X-App-Secret")
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("jwt-only: %d", w.Code)
	}
	// Authenticated user without operator roles → 403.
	h = env.headers(t, task4PlainUser)
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusForbidden {
		t.Fatalf("plain user: %d", w.Code)
	}
	// Auditor (usage:read only) → 403 on the secret write surface.
	h = env.headers(t, task4Auditor)
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusForbidden {
		t.Fatalf("auditor write: %d", w.Code)
	}
	// Wrong app secret → 401.
	h = env.headers(t, task4Admin)
	h["X-App-Secret"] = "wrong"
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong app secret: %d", w.Code)
	}
}

func TestCredentialForgedActorRejected(t *testing.T) {
	env := setupTask4(t)
	hdrs := env.headers(t, task4Admin)
	body := fmt.Sprintf(`{"provider_id":%q,"label":"x","auth_type":"api_key","secret":"s","reason":"r","actor":"mallory","role":"admin"}`,
		task4ProviderID(t, env.db))
	if w := postJSON(t, env.engine, "/admin/credentials", hdrs, body); w.Code != http.StatusBadRequest {
		t.Fatalf("forged actor/role must be rejected: %d %s", w.Code, w.Body.String())
	}
	// The real attribution in any successful create is the middleware's, not
	// the body's — verified by TestCredentialLifecycleAuthorized.
}

func TestCatalogWriteMountedWithAuthz(t *testing.T) {
	env := setupTask4(t)

	// Operator with models:manage can write; actor lands in revision history.
	hdrs := env.headers(t, task4User)
	body := fmt.Sprintf(`{"id":%q,"display_name":"M","protocols":["openai_chat"],"lifecycle":"draft","context_tokens":8000,"max_output_tokens":1000}`, task4ModelID)
	w := postJSON(t, env.engine, "/admin/models", hdrs, body)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("operator model create: %d %s", w.Code, w.Body.String())
	}
	// created_by is only set on publish; check the model audit instead.
	var auditActor *string
	if err := env.db.Get(&auditActor,
		`SELECT actor_user_id FROM inference_audit_log WHERE action = 'model.create' AND object_id = $1`, task4ModelID); err != nil {
		t.Fatalf("model.create audit: %v", err)
	}
	if auditActor == nil || *auditActor != task4User {
		t.Fatalf("model.create attribution: %v", auditActor)
	}

	// Publish works and attributes the combined subject.
	w = postJSON(t, env.engine, "/admin/catalog/publish", hdrs, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	var createdBy string
	env.db.Get(&createdBy, `SELECT created_by FROM inference_config_revisions ORDER BY revision DESC LIMIT 1`)
	if !strings.HasPrefix(createdBy, "user:"+task4User+"@app:") {
		t.Fatalf("publish attribution: %q", createdBy)
	}

	// Plain app credentials cannot write the catalog (普通 App 凭据无法管理).
	h := map[string]string{"X-App-ID": task4AppID, "X-App-Secret": task4AppSecret}
	if w := postJSON(t, env.engine, "/admin/models", h, `{"id":"x2","display_name":"X"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("app-only catalog write: %d", w.Code)
	}
	// Plain user cannot write (普通用户无法管理).
	h = env.headers(t, task4PlainUser)
	if w := postJSON(t, env.engine, "/admin/models", h, `{"id":"x3","display_name":"X"}`); w.Code != http.StatusForbidden {
		t.Fatalf("plain user catalog write: %d", w.Code)
	}
	// Auditor cannot write the catalog.
	h = env.headers(t, task4Auditor)
	if w := postJSON(t, env.engine, "/admin/models", h, `{"id":"x4","display_name":"X"}`); w.Code != http.StatusForbidden {
		t.Fatalf("auditor catalog write: %d", w.Code)
	}
}

func TestCredentialCiphertextSwapAndKeyVersion(t *testing.T) {
	env := setupTask4(t)
	hdrs := env.headers(t, task4Admin)
	prov := task4ProviderID(t, env.db)

	mk := func(label, secret string) string {
		w := postJSON(t, env.engine, "/admin/credentials", hdrs, fmt.Sprintf(
			`{"provider_id":%q,"label":%q,"auth_type":"api_key","secret":%q,"reason":"r"}`, prov, label, secret))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", label, w.Code, w.Body.String())
		}
		var v struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &v)
		return v.Data.ID
	}
	idA := mk("a", "secret-aaa")
	idB := mk("b", "secret-bbb")

	// 密文串换: move A's ciphertext onto B's row → B's decrypt check fails
	// because AAD binds ciphertext to the credential ID.
	var ctA []byte
	env.db.Get(&ctA, `SELECT ciphertext FROM inference_credentials WHERE id = $1`, idA)
	env.db.MustExec(`UPDATE inference_credentials SET ciphertext = $2 WHERE id = $1`, idB, ctA)
	w := postJSON(t, env.engine, "/admin/credentials/"+idB+"/test", hdrs, `{"reason":"swap check"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("ciphertext swap must fail the test endpoint: %d %s", w.Code, w.Body.String())
	}

	// 错误 key version: point B at a version the vault doesn't hold.
	env.db.MustExec(`UPDATE inference_credentials SET ciphertext = $2, key_version = 99 WHERE id = $1`, idB, ctA)
	w = postJSON(t, env.engine, "/admin/credentials/"+idB+"/test", hdrs, `{"reason":"kv check"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unknown key version must fail the test endpoint: %d", w.Code)
	}

	// Neither failure may leak secret material in the response.
	if strings.Contains(w.Body.String(), "secret-") {
		t.Fatalf("error response leaks secret material: %s", w.Body.String())
	}
}

func task4ProviderID(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	var id string
	if err := db.Get(&id, `SELECT id FROM inference_providers WHERE code = 'task4prov'`); err != nil {
		t.Fatalf("provider seed: %v", err)
	}
	return id
}


// TestOperatorJWTBoundToVerifiedApp: the two identity legs must BIND. A
// stolen operator JWT issued for another app presented together with a valid
// secret for the real app must not authorize (Task 4 review fix #1) — and
// must not launder audit attribution through the wrong app.
func TestOperatorJWTBoundToVerifiedApp(t *testing.T) {
	env := setupTask4(t)

	token, err := env.tok.SignAccessToken(task4Admin, "task4-other-app", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{
		"Authorization": "Bearer " + token,
		"X-App-ID":      task4AppID,
		"X-App-Secret":  task4AppSecret,
	}
	body := fmt.Sprintf(`{"provider_id":%q,"label":"x","auth_type":"api_key","secret":"s","reason":"r"}`,
		task4ProviderID(t, env.db))
	if w := postJSON(t, env.engine, "/admin/credentials", h, body); w.Code != http.StatusForbidden {
		t.Fatalf("cross-app JWT must be rejected: %d %s", w.Code, w.Body.String())
	}

	// The rejected attempt must not leave an audit row attributing to the
	// real app either.
	var n int
	if err := env.db.Get(&n,
		`SELECT count(*) FROM inference_audit_log WHERE actor_user_id = $1`, task4Admin); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected cross-app attempt left audit rows: %d", n)
	}
}
