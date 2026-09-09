// oauth_db_test.go — Task 12 OAuth 授权流真库验收：state/PKCE、回调一次
// 性、发起身份绑定、密文存储、撤销。mock 厂商经 httptest；skip 不是通过
// （验收运行必设 DATABASE_URL）。

package credentials_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

const (
	task12User    = "12121212-1212-4212-8212-121212121212"
	task12OtherOp = "34343434-3434-4434-8434-343434343434"
	task12App     = "task12-oauth-app"
)

// mockVendor is a controllable OAuth vendor: authorize/token/revoke plus
// health/quota endpoints for the worker tests in package workers (this
// helper is duplicated there deliberately — tests stay package-local).
type mockVendor struct {
	srv *httptest.Server

	mu               sync.Mutex
	tokenCalls       []url.Values
	revokeCalls      int
	tokenHandler     func(form url.Values) (int, string) // custom override
	refreshResponses []refreshStub                       // consumed in order
}

type refreshStub struct {
	delay  <-chan struct{} // block until closed (or nil)
	status int
	body   string
}

func newMockVendor(t *testing.T) *mockVendor {
	t.Helper()
	v := &mockVendor{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form := r.Form
		v.mu.Lock()
		v.tokenCalls = append(v.tokenCalls, form)
		override := v.tokenHandler
		var stub *refreshStub
		if override == nil && form.Get("grant_type") == "refresh_token" && len(v.refreshResponses) > 0 {
			stub = &v.refreshResponses[0]
			v.refreshResponses = v.refreshResponses[1:]
		}
		v.mu.Unlock()
		if override != nil {
			status, body := override(form)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		if stub != nil {
			if stub.delay != nil {
				<-stub.delay
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stub.status)
			fmt.Fprint(w, stub.body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if form.Get("grant_type") == "refresh_token" {
			fmt.Fprint(w, `{"access_token":"at-refreshed","refresh_token":"rt-new","token_type":"bearer","expires_in":3600}`)
			return
		}
		if form.Get("code") != "good-code" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"at-1","refresh_token":"rt-1","token_type":"bearer","expires_in":3600,"account_id":"vendor-acct-1"}`)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.revokeCalls++
		v.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/quota", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"limit_micros":1000000,"remaining_micros":400000,"reset_at":"2026-09-10T00:00:00Z"}`)
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"vendor-model-1"}]}`)
	})
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func (v *mockVendor) spec() connector.Spec {
	return connector.Spec{
		Key:          "testvendor",
		AuthorizeURL: v.srv.URL + "/authorize",
		TokenURL:     v.srv.URL + "/token",
		RevokeURL:    v.srv.URL + "/revoke",
		ModelsURL:    v.srv.URL + "/models",
		QuotaURL:     v.srv.URL + "/quota",
		HealthURL:    v.srv.URL + "/health",
		ClientID:     "cid-test",
		Scopes:       []string{"inference"},
		RedirectURL:  "https://ops.example/admin/oauth/callback",
	}
}

func (v *mockVendor) registry() connector.Registry {
	return connector.Registry{"testvendor": v.spec()}
}

func (v *mockVendor) lastTokenForm() url.Values {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.tokenCalls) == 0 {
		return nil
	}
	return v.tokenCalls[len(v.tokenCalls)-1]
}

// oauthSetup seeds an oauth_connector provider + operator user and returns
// the wired services.
func oauthSetup(t *testing.T) (*sqlx.DB, *postgres.Store, *credentials.Service, *credentials.OAuthService, *credentials.Vault, *mockVendor, string) {
	t.Helper()
	if atomicDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", atomicDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM inference_session_bindings WHERE account_id IN (SELECT id FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task12prov'))`)
		db.Exec(`DELETE FROM inference_oauth_grants WHERE connector = 'testvendor'`)
		db.Exec(`DELETE FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task12prov')`)
		db.Exec(`DELETE FROM inference_credentials WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task12prov')`)
		db.Exec(`DELETE FROM inference_audit_log WHERE actor_app_id IN ('task12-oauth-app','system:inference-worker')`)
		db.Exec(`DELETE FROM inference_providers WHERE code = 'task12prov'`)
		db.Close()
	})
	seed := []string{
		`INSERT INTO users (id, status) VALUES ('12121212-1212-4212-8212-121212121212','active')
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO inference_providers (code, display_name, access_type)
		 VALUES ('task12prov','Task12 Provider','oauth_connector')
		 ON CONFLICT (code) DO NOTHING`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	var providerID string
	if err := db.Get(&providerID, `SELECT id FROM inference_providers WHERE code = 'task12prov'`); err != nil {
		t.Fatalf("resolve provider: %v", err)
	}
	key := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: key}, 1)
	if err != nil {
		t.Fatal(err)
	}
	vendor := newMockVendor(t)
	store := postgres.NewStore(db)
	credSvc := credentials.NewService(vault, store, store)
	oauthSvc := credentials.NewOAuthService(vault, store, store,
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), credSvc, nil)
	return db, store, credSvc, oauthSvc, vault, vendor, providerID
}

func task12Op() credentials.Operator {
	return credentials.Operator{UserID: task12User, AppID: task12App, Roles: []string{"admin"}}
}

func TestOAuthAuthorizeCallbackHappyPath(t *testing.T) {
	db, _, _, oauthSvc, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	start, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "pool-acct-1", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	// Authorize URL carries state + S256 challenge, never the verifier.
	u, err := url.Parse(start.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != start.State {
		t.Fatalf("authorize url state mismatch")
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("missing PKCE challenge in authorize url: %s", start.AuthorizeURL)
	}
	if strings.Contains(start.AuthorizeURL, "code_verifier") {
		t.Fatal("verifier leaked into authorize url")
	}

	res, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "complete")
	if err != nil {
		t.Fatal(err)
	}
	if res.Credential.AuthType != "oauth" || res.Credential.Status != "active" {
		t.Fatalf("unexpected credential view: %+v", res.Credential)
	}
	if res.Credential.ExpiresAt == nil {
		t.Fatal("expires_at not set from token response")
	}

	// PKCE: the verifier sent to the token endpoint must satisfy
	// S256(verifier) == challenge from the authorize URL.
	form := vendor.lastTokenForm()
	verifier := form.Get("code_verifier")
	if verifier == "" {
		t.Fatal("token exchange did not send code_verifier")
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Fatal("PKCE verifier does not match challenge")
	}

	// Sealed storage: ciphertext decrypts to the token bundle; the account
	// carries the vendor account id; connector key persisted for refresh.
	cred, err := postgres.NewStore(db).GetCredential(ctx, res.Credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Connector != "testvendor" {
		t.Fatalf("connector not persisted: %q", cred.Connector)
	}
	plain, err := vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := credentials.UnmarshalBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.AccessToken != "at-1" || bundle.RefreshToken != "rt-1" {
		t.Fatalf("bundle mismatch: %+v", bundle)
	}
	var acctStatus, extID string
	if err := db.QueryRow(`SELECT status, external_account_id FROM inference_upstream_accounts WHERE id = $1`,
		res.AccountID).Scan(&acctStatus, &extID); err != nil {
		t.Fatal(err)
	}
	if acctStatus != "active" || extID != "vendor-acct-1" {
		t.Fatalf("account mismatch: %s %s", acctStatus, extID)
	}
	// Audit: begin + complete both recorded, detail never carries tokens.
	var n int
	db.Get(&n, `SELECT count(*) FROM inference_audit_log
		WHERE action IN ('oauth.authorize.begin','oauth.authorize.complete') AND object_id IN ($1,$2)`,
		res.Credential.ID, res.Credential.ID)
	var grantID string
	db.Get(&grantID, `SELECT id FROM inference_oauth_grants WHERE state = $1`, start.State)
	db.Get(&n, `SELECT count(*) FROM inference_audit_log
		WHERE action = 'oauth.authorize.begin' AND object_id = $1`, grantID)
	if n != 1 {
		t.Fatalf("missing begin audit: %d", n)
	}
	db.Get(&n, `SELECT count(*) FROM inference_audit_log
		WHERE action = 'oauth.authorize.complete' AND object_id = $1
		  AND detail::text NOT LIKE '%at-1%' AND detail::text NOT LIKE '%rt-1%'`, res.Credential.ID)
	if n != 1 {
		t.Fatalf("missing/sensitive complete audit: %d", n)
	}
}

func TestOAuthCallbackOneTimeState(t *testing.T) {
	_, _, _, oauthSvc, _, _, providerID := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	start, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "acct", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "complete"); err != nil {
		t.Fatal(err)
	}
	// 重放同一 state：一次性校验拒绝。
	if _, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "replay"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replayed state must be conflict, got %v", err)
	}
	// 未知 state 同样拒绝。
	if _, err := oauthSvc.HandleCallback(ctx, op, "no-such-state", "good-code", "x"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unknown state must be conflict, got %v", err)
	}
}

func TestOAuthCallbackExpiredAndWrongOperator(t *testing.T) {
	db, _, _, oauthSvc, _, _, providerID := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	start, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "acct", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	// 直接回拨 expires_at 制造过期 state。
	if _, err := db.Exec(`UPDATE inference_oauth_grants SET expires_at = now() - interval '1 minute' WHERE state = $1`, start.State); err != nil {
		t.Fatal(err)
	}
	if _, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "late"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired state must be conflict, got %v", err)
	}

	// 换运营人员：state 绑定发起身份，拒绝且不可重放。
	start2, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "acct", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	stranger := credentials.Operator{UserID: task12OtherOp, AppID: task12App, Roles: []string{"admin"}}
	if _, err := oauthSvc.HandleCallback(ctx, stranger, start2.State, "good-code", "stolen"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("foreign operator must be conflict, got %v", err)
	}
	// state 已被消费（先消费后校验身份），原操作员也无法再用。
	if _, err := oauthSvc.HandleCallback(ctx, op, start2.State, "good-code", "retry"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("consumed state must stay consumed, got %v", err)
	}
}

func TestOAuthCallbackExchangeFailureConsumesState(t *testing.T) {
	_, _, _, oauthSvc, _, _, providerID := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	start, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "acct", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	_, err = oauthSvc.HandleCallback(ctx, op, start.State, "bad-code", "vendor rejected")
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("exchange failure must map upstream_unavailable, got %v", err)
	}
	// 失败的回调已消费 state：不能拿同一 state 重试换码。
	if _, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "retry"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("state must be consumed after failed exchange, got %v", err)
	}
}

func TestOAuthBeginRejectsNonConnectorProvider(t *testing.T) {
	db, _, _, oauthSvc, _, _, _ := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	if _, err := db.Exec(`INSERT INTO inference_providers (code, display_name, access_type)
		VALUES ('task12official','Official','official_api') ON CONFLICT (code) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DELETE FROM inference_providers WHERE code = 'task12official'`)
	var officialID string
	if err := db.Get(&officialID, `SELECT id FROM inference_providers WHERE code = 'task12official'`); err != nil {
		t.Fatal(err)
	}
	if _, err := oauthSvc.BeginAuthorization(ctx, op, officialID, "testvendor", "x", "onboard"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("official_api provider must reject oauth flow, got %v", err)
	}
	if _, err := oauthSvc.BeginAuthorization(ctx, op, officialID, "no-such-connector", "x", "onboard"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("unknown connector must be invalid_input, got %v", err)
	}
}

func TestOAuthRevokeDisablesAccountsAndCallsVendor(t *testing.T) {
	db, _, _, oauthSvc, _, vendor, providerID := oauthSetup(t)
	ctx := context.Background()
	op := task12Op()

	start, err := oauthSvc.BeginAuthorization(ctx, op, providerID, "testvendor", "acct", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	res, err := oauthSvc.HandleCallback(ctx, op, start.State, "good-code", "complete")
	if err != nil {
		t.Fatal(err)
	}
	view, err := oauthSvc.Revoke(ctx, op, res.Credential.ID, "operator revoke")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "revoked" {
		t.Fatalf("credential not revoked: %s", view.Status)
	}
	var acctStatus string
	if err := db.Get(&acctStatus, `SELECT status FROM inference_upstream_accounts WHERE id = $1`, res.AccountID); err != nil {
		t.Fatal(err)
	}
	if acctStatus != "disabled" {
		t.Fatalf("revoke must disable bound accounts, got %s", acctStatus)
	}
	vendor.mu.Lock()
	revoked := vendor.revokeCalls
	vendor.mu.Unlock()
	if revoked != 1 {
		t.Fatalf("vendor revoke endpoint not called: %d", revoked)
	}
	// 调度面立即不可见（ListActiveUpstreamAccounts 只认 active）。
	acts, err := postgres.NewStore(db).ListActiveUpstreamAccounts(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("revoked account still schedulable: %d", len(acts))
	}
}

// TestOAuthRegistryParse pins the config-load contract.
func TestOAuthRegistryParse(t *testing.T) {
	reg, err := connector.ParseRegistry(`[{"key":"k","authorize_url":"https://v.example/a","token_url":"https://v.example/t","client_id":"c","redirect_url":"https://ops.example/cb"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !reg["k"].SupportsPKCE() {
		t.Fatal("PKCE must default on")
	}
	if _, err := connector.ParseRegistry(`[{"key":"k","authorize_url":"https://v.example/a","token_url":"https://v.example/t","redirect_url":"x"}]`); err == nil {
		t.Fatal("missing client_id must fail")
	}
	if _, err := connector.ParseRegistry(`not-json`); err == nil {
		t.Fatal("malformed JSON must fail")
	}
	if _, err := connector.ParseRegistry(`[{"key":"k","authorize_url":"https://v/a","token_url":"https://v/t","client_id":"c","redirect_url":"r"},{"key":"k","authorize_url":"https://v/a","token_url":"https://v/t","client_id":"c","redirect_url":"r"}]`); err == nil {
		t.Fatal("duplicate key must fail")
	}
}

var _ = json.Marshal // keep encoding/json imported for fixture shaping
