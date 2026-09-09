package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// user_api_keys_test.go — /user/api-keys 管理面与 /v1 Key 鉴权的端到端
// 测试（真实库 + 真实 JWT 中间件）。越权矩阵：客户 A/B 各持 JWT，互相
// 读/改/撤对方 Key 一律 404（与不存在无差别）；明文只在创建响应出现；
// 撤销/过期在下一次 /v1 调用生效。

// accessFixture bundles the wired engine + store for one test.
type accessFixture struct {
	db       *sqlx.DB
	store    *postgres.Store
	engine   *gin.Engine
	tokenSvc *service.TokenService
	keySvc   *access.KeyService
	resolver *access.Resolver
}

// newAccessFixture builds a gin engine with:
//   - /user group behind the REAL JWTAuth middleware + the key handlers;
//   - /v1 group behind the REAL APIKeyAuth middleware with a probe route
//     that echoes the resolved principal (Task 8 will replace the probe
//     with the protocol routes; the auth chain is what this task pins).
func newAccessFixture(t *testing.T, accountRPM int) *accessFixture {
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
		inference_audit_log,
		operator_roles,
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
		users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}

	store := postgres.NewStore(db)
	keySvc := access.NewKeyService(store, nil)
	resolver := access.NewResolver(store, nil)
	tokenSvc := newTestTokenService(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	userGroup := engine.Group("/user", middleware.JWTAuth(tokenSvc))
	httpapi.NewUserAPIKeysHandler(keySvc).Register(userGroup)

	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver, access.NewRPMCounter(nil), accountRPM))
	v1.GET("/probe", func(c *gin.Context) {
		p := httpapi.CallerPrincipalOf(c)
		if p == nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"kind":               string(p.Kind),
			"billing_account_id": p.BillingAccountID,
			"api_key_id":         p.APIKeyID,
		})
	})
	return &accessFixture{db: db, store: store, engine: engine, tokenSvc: tokenSvc, keySvc: keySvc, resolver: resolver}
}

// newTestTokenService mints a real TokenService over throwaway RSA keys.
func newTestTokenService(t *testing.T) *service.TokenService {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	dir := t.TempDir()
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("pkix: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("write pub: %v", err)
	}
	svc, err := service.NewTokenService(&config.Config{
		RSAPrivate: privPath, RSAPublic: pubPath, JWTAccessTTL: time.Hour,
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return svc
}

// addUser inserts a users row (FK target for the billing account) and
// returns the user id + a signed access token.
func (f *accessFixture) addUser(t *testing.T) (userID, token string) {
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

// grantEntitlement seeds a published model + policy + active entitlement
// for the user's account so model scope checks have something to allow.
func (f *accessFixture) grantEntitlement(t *testing.T, userID string, modelIDs ...string) {
	t.Helper()
	ctx := context.Background()
	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	for _, m := range modelIDs {
		if err := f.store.InsertModel(ctx, &domain.Model{
			ID: m, DisplayName: m, ContextTokens: 200000, MaxOutputTokens: 8192,
		}); err != nil {
			t.Fatalf("insert model: %v", err)
		}
	}
	pol := &postgres.PolicyVersion{Name: "coding-plan", Revision: 1, ModelIDs: modelIDs}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	now := time.Now().UTC()
	if err := f.store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: modelIDs,
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
}

func (f *accessFixture) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// createKey POSTs /user/api-keys and returns the decoded data map.
func (f *accessFixture) createKey(t *testing.T, token string, body map[string]any) map[string]any {
	t.Helper()
	w := f.do(t, http.MethodPost, "/user/api-keys", token, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create key: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

func TestUserAPIKeys_LifecycleAndCrossCustomerMatrix(t *testing.T) {
	f := newAccessFixture(t, 0)
	userA, tokA := f.addUser(t)
	_, tokB := f.addUser(t)
	_ = userA

	// Create → plaintext shown exactly once, inside the envelope.
	created := f.createKey(t, tokA, map[string]any{"name": "cli", "budget_micros": "5000000", "rpm_limit": 100})
	plaintext, _ := created["key"].(string)
	if !strings.HasPrefix(plaintext, "yk-") {
		t.Fatalf("plaintext missing yk- prefix: %q", created["key"])
	}
	keyID, _ := created["id"].(string)
	if created["budget_micros"] != "5000000" {
		t.Errorf("budget_micros = %v, want string \"5000000\"", created["budget_micros"])
	}

	// At rest: digest + prefix only — the plaintext is unrecoverable.
	var hash, prefix string
	if err := f.db.QueryRow(`SELECT key_hash, key_prefix FROM inference_api_keys WHERE id = $1`, keyID).Scan(&hash, &prefix); err != nil {
		t.Fatalf("db read: %v", err)
	}
	if hash == plaintext || !strings.HasPrefix(hash, "sha256:") {
		t.Errorf("plaintext persisted: %q", hash)
	}
	if !strings.HasPrefix(plaintext, prefix) {
		t.Errorf("prefix %q is not a prefix of the key", prefix)
	}

	// Second read cannot recover the plaintext: neither list nor detail
	// carry a "key" field, and the raw bodies never contain the secret.
	for _, path := range []string{"/user/api-keys", "/user/api-keys/" + keyID} {
		w := f.do(t, http.MethodGet, path, tokA, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), plaintext) {
			t.Errorf("GET %s leaked the plaintext", path)
		}
		if strings.Contains(w.Body.String(), `"key"`) {
			t.Errorf("GET %s carries a key field", path)
		}
	}

	// Cross-customer matrix: B cannot read, update or revoke A's key —
	// every attempt is 404, indistinguishable from a missing key.
	nonexistentID := uuid.NewString()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/user/api-keys/" + keyID},
		{http.MethodPatch, "/user/api-keys/" + keyID},
		{http.MethodDelete, "/user/api-keys/" + keyID},
	} {
		w := f.do(t, tc.method, tc.path, tokB, map[string]any{"name": "pwned"})
		if w.Code != http.StatusNotFound {
			t.Errorf("B %s %s: got %d, want 404 (%s)", tc.method, tc.path, w.Code, w.Body.String())
		}
		// Existence oracle guard: someone else's key ID and a never-created
		// ID must produce byte-identical responses for the same caller —
		// same status, same body (no "get api key by id: not found" vs
		// "api key not found" split, no internal operation names).
		wMissing := f.do(t, tc.method, "/user/api-keys/"+nonexistentID, tokB, map[string]any{"name": "pwned"})
		if wMissing.Code != w.Code || wMissing.Body.String() != w.Body.String() {
			t.Errorf("B %s: foreign-id and missing-id responses differ:\nforeign: %d %s\nmissing: %d %s",
				tc.method, w.Code, w.Body.String(), wMissing.Code, wMissing.Body.String())
		}
		// The same indistinguishability holds for the owner querying a
		// missing ID (owner has an account, exercising the third branch).
		if tc.method == http.MethodGet {
			wOwner := f.do(t, tc.method, "/user/api-keys/"+nonexistentID, tokA, nil)
			if wOwner.Code != w.Code || wOwner.Body.String() != w.Body.String() {
				t.Errorf("owner missing-id response differs from cross-customer:\nowner: %d %s\nforeign: %d %s",
					wOwner.Code, wOwner.Body.String(), w.Code, w.Body.String())
			}
		}
	}
	// B's list does not contain A's key.
	w := f.do(t, http.MethodGet, "/user/api-keys", tokB, nil)
	if strings.Contains(w.Body.String(), keyID) {
		t.Error("B's list contains A's key")
	}

	// Owner update path.
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"name": "cli-2", "budget_micros": nil})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if env.Data["name"] != "cli-2" || env.Data["budget_micros"] != nil {
		t.Errorf("patch not applied: %v", env.Data)
	}

	// Idempotent revoke.
	w = f.do(t, http.MethodDelete, "/user/api-keys/"+keyID, tokA, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked":true`) {
		t.Fatalf("revoke #1: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodDelete, "/user/api-keys/"+keyID, tokA, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revoked":false`) {
		t.Fatalf("revoke #2 (idempotent): %d %s", w.Code, w.Body.String())
	}

	// Unauthenticated management calls are rejected by JWTAuth itself.
	if w := f.do(t, http.MethodGet, "/user/api-keys", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no-JWT list: got %d, want 401", w.Code)
	}
	// Unknown body fields are rejected loudly (no smuggled fields).
	if w := f.do(t, http.MethodPost, "/user/api-keys", tokA, map[string]any{"billing_account_id": "x"}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field: got %d, want 400", w.Code)
	}
}

func TestUserAPIKeys_ModelScopeCannotExceedEntitlement(t *testing.T) {
	f := newAccessFixture(t, 0)
	userA, tokA := f.addUser(t)
	f.grantEntitlement(t, userA, "glm-4.6", "deepseek-v4")

	// Within the entitlement: allowed.
	created := f.createKey(t, tokA, map[string]any{"model_ids": []string{"glm-4.6"}})
	if created["model_ids"].([]any)[0] != "glm-4.6" {
		t.Errorf("model_ids not stored: %v", created["model_ids"])
	}
	// Beyond the entitlement: 403 model_not_allowed.
	w := f.do(t, http.MethodPost, "/user/api-keys", tokA, map[string]any{"model_ids": []string{"gpt-x"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("widening on create: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
	// Widening via update is equally rejected.
	keyID := created["id"].(string)
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"model_ids": []string{"gpt-x"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("widening on update: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
	// ...and the stored narrowing is unchanged.
	w = f.do(t, http.MethodGet, "/user/api-keys/"+keyID, tokA, nil)
	if !strings.Contains(w.Body.String(), "glm-4.6") || strings.Contains(w.Body.String(), "gpt-x") {
		t.Errorf("stored narrowing changed by rejected update: %s", w.Body.String())
	}
}

func TestUserAPIKeys_MultipleKeysShareOneAccountPool(t *testing.T) {
	f := newAccessFixture(t, 0)
	userA, tokA := f.addUser(t)
	f.grantEntitlement(t, userA, "glm-4.6")

	k1 := f.createKey(t, tokA, map[string]any{"budget_micros": "1000000"})
	k2 := f.createKey(t, tokA, map[string]any{"budget_micros": "2000000"})
	k3 := f.createKey(t, tokA, map[string]any{})

	// One account, one entitlement — creating keys never grows the pool.
	var accounts, ents int
	if err := f.db.Get(&accounts, `SELECT count(*) FROM inference_billing_accounts`); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&ents, `SELECT count(*) FROM inference_entitlements`); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || ents != 1 {
		t.Errorf("accounts=%d entitlements=%d, want 1/1", accounts, ents)
	}
	// Per-key sub-budgets start at zero and are independent.
	for _, k := range []map[string]any{k1, k2, k3} {
		if k["budget_used_micros"] != "0" {
			t.Errorf("budget_used = %v, want \"0\"", k["budget_used_micros"])
		}
	}
	w := f.do(t, http.MethodGet, "/user/api-keys?limit=2&offset=0", tokA, nil)
	if !strings.Contains(w.Body.String(), `"total":3`) {
		t.Errorf("list total: %s", w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/user/api-keys?limit=2&offset=2", tokA, nil)
	if !strings.Contains(w.Body.String(), k1["id"].(string)) {
		t.Errorf("page 2 missing oldest key: %s", w.Body.String())
	}
}

func TestV1_KeyAuth_RevokeAndExpiryTakeEffect(t *testing.T) {
	f := newAccessFixture(t, 0)
	_, tokA := f.addUser(t)

	probe := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		f.engine.ServeHTTP(w, req)
		return w
	}

	// Valid key → 200, principal kind api_key, /v1 native shape (no
	// management envelope on errors).
	created := f.createKey(t, tokA, map[string]any{})
	plaintext := created["key"].(string)
	keyID := created["id"].(string)
	w := probe(plaintext)
	if w.Code != http.StatusOK {
		t.Fatalf("valid key: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"kind":"api_key"`) || !strings.Contains(w.Body.String(), keyID) {
		t.Errorf("probe principal wrong: %s", w.Body.String())
	}

	// Revoked → the very next call is 401 with the native error shape.
	if w := f.do(t, http.MethodDelete, "/user/api-keys/"+keyID, tokA, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	w = probe(plaintext)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"invalid_api_key"`) {
		t.Fatalf("revoked key: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"data"`) {
		t.Errorf("/v1 error leaked the management envelope: %s", w.Body.String())
	}

	// Expired → 401, and the lazy transition flips the stored status.
	full, _, digest, err := access.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	past := time.Now().Add(-time.Hour).UTC()
	acct, err := f.store.GetBillingAccountByUser(context.Background(), createdUserID(t, f, tokA))
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	expKey := &domain.APIKey{BillingAccountID: acct.ID, Prefix: access.LookupPrefix(full), ExpiresAt: &past}
	if err := f.store.InsertAPIKey(context.Background(), expKey, digest); err != nil {
		t.Fatalf("insert expiring key: %v", err)
	}
	if w := probe(full); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired key: %d", w.Code)
	}
	var status string
	if err := f.db.Get(&status, `SELECT status FROM inference_api_keys WHERE id = $1`, expKey.ID); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Errorf("lazy expiry status = %s, want expired", status)
	}

	// Tampered / missing credentials.
	if w := probe(plaintext[:len(plaintext)-1] + "A"); w.Code != http.StatusUnauthorized {
		t.Errorf("tampered key: %d", w.Code)
	}
	if w := probe(""); w.Code != http.StatusUnauthorized {
		t.Errorf("missing key: %d", w.Code)
	}
}

func TestV1_RateLimit_PerKeyAndPerAccount(t *testing.T) {
	f := newAccessFixture(t, 0)
	_, tokA := f.addUser(t)

	probe := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		f.engine.ServeHTTP(w, req)
		return w
	}

	// Per-Key bucket: rpm_limit=2 → the third request is 429 with a
	// computable Retry-After.
	limited := f.createKey(t, tokA, map[string]any{"rpm_limit": 2})
	lk := limited["key"].(string)
	for i := 0; i < 2; i++ {
		if w := probe(lk); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, w.Code)
		}
	}
	w := probe(lk)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if !strings.Contains(w.Body.String(), `"rate_limited"`) {
		t.Errorf("429 body: %s", w.Body.String())
	}
}

func TestV1_RateLimit_AccountBucketSharedAcrossKeys(t *testing.T) {
	f := newAccessFixture(t, 1) // account RPM = 1
	_, tokA := f.addUser(t)
	k1 := f.createKey(t, tokA, map[string]any{})["key"].(string)
	k2 := f.createKey(t, tokA, map[string]any{})["key"].(string)

	probe := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		f.engine.ServeHTTP(w, req)
		return w
	}
	if w := probe(k1); w.Code != http.StatusOK {
		t.Fatalf("first key first request: %d", w.Code)
	}
	// The second key draws from the SAME account bucket.
	if w := probe(k2); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second key should hit the shared account bucket: %d", w.Code)
	}
}

// createdUserID extracts the user id from a signed token (test helper —
// the claim was minted by this test process).
func createdUserID(t *testing.T, f *accessFixture, token string) string {
	t.Helper()
	claims, err := f.tokenSvc.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return claims.Subject
}
