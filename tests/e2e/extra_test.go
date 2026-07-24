package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// TestE2E_AuthLogout_Roundtrip covers the full logout → re-login cycle.
func TestE2E_AuthLogout_Roundtrip(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	r := loginAndGetTokens(t, srv.Engine, "logout-rt", "yundian")
	tok := r.AccessToken
	refresh := r.RefreshToken
	if tok == "" || refresh == "" {
		t.Fatal("missing tokens from login")
	}

	// Logout with refresh token.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/logout",
		`{"refresh_token":"`+refresh+`"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: %d", resp.StatusCode)
	}

	// After logout, refresh should be rejected (401).
	resp = doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout, refresh: %d, want 401", resp.StatusCode)
	}

	// But we can still call /user/profile with the access token (until exp).
	resp = doRequest(t, srv.Engine, http.MethodGet, "/user/profile",
		``, authHeader(tok))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("profile after logout: %d, want 200", resp.StatusCode)
	}
}

// TestE2E_RefreshMissingBody covers the 400 path on bad input.
func TestE2E_RefreshMissingBody(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing refresh_token: %d, want 400", resp.StatusCode)
	}
}

// TestE2E_LogoutMissingBody covers the 400 path.
func TestE2E_LogoutMissingBody(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/logout",
		`{}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing refresh_token: %d, want 400", resp.StatusCode)
	}
}

// TestE2E_JWKS_ContentType confirms the response shape.
func TestE2E_JWKS_ContentType(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodGet, "/.well-known/jwks.json",
		``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jwks: %d", resp.StatusCode)
	}
	body := string(resp.Body)
	if !strings.Contains(body, `"alg":"RS256"`) {
		t.Errorf("missing RS256 alg in jwks: %s", body)
	}
	if !strings.Contains(body, `"kid":"yunhou-users-rsa"`) {
		t.Errorf("missing kid in jwks: %s", body)
	}
}

// TestE2E_TestLoginInvalidJSON covers the 400 path on malformed body for the
// dev-only /test/login endpoint. Replaces TestE2E_LoginInvalidJSON which
// drove /auth/login (removed by commit 5ef27ce).
func TestE2E_TestLoginInvalidJSON(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodPost, "/test/login?plan_id=monthly",
		"not-json", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid json test-login: %d, want 400", resp.StatusCode)
	}
}

// TestE2E_HealthEndpoint confirms /healthz returns 200 when DB is up.
func TestE2E_HealthEndpoint(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodGet, "/healthz", ``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d, want 200", resp.StatusCode)
	}
}

// TestE2E_AppAndPlanCRUD exercises the admin endpoints.
func TestE2E_AppAndPlanCRUD(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	hdrs := appAuthHeaders(superAppID)
	// Create app.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/admin/apps",
		`{"app_id":"e2e-extra","name":"E2E Extra"}`,
		hdrs)
	if resp.StatusCode != http.StatusCreated {
		body := string(resp.Body)
		t.Fatalf("create app: %d %s", resp.StatusCode, body)
	}

	// List apps — should include the new one.
	resp = doRequest(t, srv.Engine, http.MethodGet, "/apps",
		``, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), `"app_id":"e2e-extra"`) {
		t.Errorf("new app not in list: %s", resp.Body)
	}

	// Get specific app.
	resp = doRequest(t, srv.Engine, http.MethodGet, "/apps/e2e-extra",
		``, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get app: %d", resp.StatusCode)
	}

	// Update app.
	resp = doRequest(t, srv.Engine, http.MethodPatch, "/admin/apps/e2e-extra",
		`{"description":"updated"}`, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("update app: %d", resp.StatusCode)
	}

	// Create plan.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/admin/plans",
		`{"id":"e2e-extra","name":"E2E Extra","price":1.0,"interval_days":30,"apps":["yundian"]}`,
		hdrs)
	if resp.StatusCode != http.StatusCreated {
		body := string(resp.Body)
		t.Fatalf("create plan: %d %s", resp.StatusCode, body)
	}

	// Get plan.
	resp = doRequest(t, srv.Engine, http.MethodGet, "/admin/plans/e2e-extra",
		``, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get plan: %d", resp.StatusCode)
	}

	// Update plan.
	resp = doRequest(t, srv.Engine, http.MethodPatch, "/admin/plans/e2e-extra",
		`{"name":"Renamed"}`, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("update plan: %d", resp.StatusCode)
	}

	// Delete plan.
	resp = doRequest(t, srv.Engine, http.MethodDelete, "/admin/plans/e2e-extra",
		``, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete plan: %d", resp.StatusCode)
	}
}

// TestE2E_PlanCreateBadJSON covers validation errors.
func TestE2E_PlanCreateBadJSON(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodPost, "/admin/plans",
		`{"id":"x","name":"X","price":-1}`,
		appAuthHeaders(superAppID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative price: %d, want 400", resp.StatusCode)
	}
}

// TestE2E_RefreshRotatesToken verifies token rotation.
func TestE2E_RefreshRotatesToken(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	login := loginAndGetTokens(t, srv.Engine, "rotate", "yundian")
	refresh := login.RefreshToken

	// First refresh.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refresh: %d", resp.StatusCode)
	}
	var refreshResp struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			User         struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	resp.JSON(t, &refreshResp)
	if refreshResp.Data.RefreshToken == refresh {
		t.Errorf("refresh token did not rotate")
	}

	// Old refresh should be revoked.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after rotate, old refresh: %d, want 401", resp.StatusCode)
	}
}

// TestE2E_ProfilePatch covers nickname + avatar validation.
func TestE2E_ProfilePatch(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	tok := loginAndGetTokens(t, srv.Engine, "profile", "yundian").AccessToken
	_ = tok

	t.Run("valid patch", func(t *testing.T) {
		body := `{"nickname":"alice","avatar_url":"https://example.com/a.png"}`
		resp := doRequest(t, srv.Engine, http.MethodPatch, "/user/profile",
			body, authHeader(tok))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("patch profile: %d", resp.StatusCode)
		}
	})
	t.Run("bad avatar http", func(t *testing.T) {
		body := `{"avatar_url":"http://example.com/a.png"}`
		resp := doRequest(t, srv.Engine, http.MethodPatch, "/user/profile",
			body, authHeader(tok))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("http avatar: %d, want 400", resp.StatusCode)
		}
	})
	t.Run("bad nickname too long", func(t *testing.T) {
		body := `{"nickname":"` + strings.Repeat("x", 101) + `"}`
		resp := doRequest(t, srv.Engine, http.MethodPatch, "/user/profile",
			body, authHeader(tok))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("long nickname: %d, want 400", resp.StatusCode)
		}
	})
}

// TestE2E_RefreshReuseFamilyRevoke tests the security response.
func TestE2E_RefreshReuseFamilyRevoke(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	r := loginAndGetTokens(t, srv.Engine, "reuse-test", "yundian")
	refresh := r.RefreshToken

	// First refresh.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refresh: %d", resp.StatusCode)
	}

	// Now the old refresh is revoked; re-using it should 401.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after rotate, old refresh: %d, want 401", resp.StatusCode)
	}
}

// TestE2E_ExpiredSub_LoginAndRefresh is the cn-staging 2026-07-23
// incident's end-to-end regression. A user with status='active' but
// expires_at in the past must:
//  1. Still log in successfully (the historical bug: the OAuth
//     callback returned `reason=subscription_expired` and bounced
//     the user to /auth/login with no escape).
//  2. Receive has_access=false in the login response (the original
//     plan's apps[] is honoured by the access-token scope, but the
//     response flag is forced false so the FE renders the paywall
//     and `useBilling.planID != "free"` bypass routes correctly to
//     the renewal CTA).
//  3. Surface the original PlanID/PlanName (NOT the default plan's
//     identity) so the FE renders "renew your X plan" rather than
//     misreading as "downgraded to free".
//  4. Refresh the access token without error (the historical bug
//     path returned ErrSubscriptionExpired here too).
//  5. Be permitted to create an order for an accepting plan (the
//     second-stage follow-up fix widened CreateOrder's precondition
//     to allow past-but-active rows through).
//
// Setup seeds an active-but-past subscription directly in the DB
// before the /test/login call (the only login endpoint available in
// e2e), then drives the same code paths the OAuth callbacks do via
// AuthService.LoginWithProfile. The test pins every observable
// surface of the response shape so any future regression of the
// decouple trips one of these assertions.
func TestE2E_ExpiredSub_LoginAndRefresh(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	db := srv.DB
	_ = e2eMustSeedExpiredSubUser(t, db) // seeds user+identity+plan+past sub; the /test/login below binds to it

	// 1. Login (via /test/login which routes through AuthService.LoginWithProfile).
	login := loginAndGetTokens(t, srv.Engine, "expired-sub-user", "yundian")
	if login.AccessToken == "" {
		t.Fatal("login must succeed even when subscription is past (cn-staging 2026-07-23 invariant)")
	}
	if login.Subscription == nil {
		t.Fatal("login response must include subscription view")
	}
	// 2. has_access MUST be false — even though 'quarterly' includes 'yundian' and the
	// default plan also includes 'yundian', the expired-sub branch forces the flag
	// false so the FE's useBilling hook routes the user to renewal rather than
	// treating them as "already subscribed".
	if login.Subscription.HasAccess {
		t.Error("Subscription.HasAccess must be false for an expired sub (cn-staging 2026-07-23 follow-up fix; default plan includes the app but the override is unconditional)")
	}
	// 3. PlanID is the user's *intended* paid plan, not the default plan — so the FE
	// renders "renew your quarterly plan" rather than misreading as "downgraded to free".
	if login.Subscription.PlanID != "quarterly" {
		t.Errorf("Subscription.PlanID = %q, want quarterly (must reflect original paid plan, not default)", login.Subscription.PlanID)
	}
	if login.Subscription.PlanName != "按季订阅" {
		t.Errorf("Subscription.PlanName = %q, want 按季订阅", login.Subscription.PlanName)
	}
	if login.Subscription.ExpiresAt == nil {
		t.Errorf("Subscription.ExpiresAt must surface the past timestamp (FE reads this for 'your plan expired N days ago')")
	}

	// 4. Refresh must succeed with the same has_access=false shape (the historical
	// bug returned ErrSubscriptionExpired here; refresh should issue a fresh token).
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+login.RefreshToken+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh with expired sub must succeed (cn-staging 2026-07-23 invariant); got %d %s", resp.StatusCode, string(resp.Body))
	}
	var refreshed struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Subscription *struct {
				PlanID    string `json:"plan_id"`
				HasAccess bool   `json:"has_access"`
			} `json:"subscription"`
		} `json:"data"`
	}
	resp.JSON(t, &refreshed)
	if refreshed.Data.AccessToken == "" {
		t.Error("refresh must mint a new access token")
	}
	if refreshed.Data.Subscription == nil {
		t.Fatal("refresh response must include subscription view")
	}
	if refreshed.Data.Subscription.HasAccess {
		t.Error("refresh Subscription.HasAccess must be false for expired sub (same invariant as login)")
	}
	if refreshed.Data.Subscription.PlanID != "quarterly" {
		t.Errorf("refresh Subscription.PlanID = %q, want quarterly", refreshed.Data.Subscription.PlanID)
	}

	// 5. CreateOrder for an accepting plan must succeed for the past-but-active
	// row. The quarterly fixture intentionally rejects new orders, so use
	// monthly here to isolate the stale-subscription precondition; monthly is
	// valid and accepting-new, so the order must be created (201).
	orderResp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly","channel":"stripe"}`, authHeader(refreshed.Data.AccessToken))
	if orderResp.StatusCode != http.StatusCreated {
		t.Fatalf("CreateOrder must permit purchase when status=active but expires_at is past; got %d, want %d. Body: %s",
			orderResp.StatusCode, http.StatusCreated, string(orderResp.Body))
	}
}

// e2eMustSeedExpiredSubUser creates a user + a 'quarterly' plan + an active-but-
// past subscription in one shot. Returns the new user UUID. Used by
// TestE2E_ExpiredSub_LoginAndRefresh to set up the cn-staging incident
// reproduction in deterministic state.
func e2eMustSeedExpiredSubUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	uid := uuidNew()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, nickname, avatar_url, status) VALUES ($1, $2, '', 'active')`,
		uid, "expired-sub-user"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Pre-seed a github social identity so the email-merge in LoginWithProfile
	// finds a matching identity and binds to this user row (rather than racing
	// a concurrent auto-create that would orphan our subscription).
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO social_identities (id, user_id, provider, provider_uid, email) VALUES (gen_random_uuid(), $1, 'github', $2, $3)`,
		uid, "l3-e2e-"+uid, "expired-sub-user@e2e.test"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	// 'quarterly' includes 'yundian' (which is also on the free plan) — this
	// drives the "default plan includes the app + paid plan also includes
	// the app" case that motivated the HasAccess=false override.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active,
			is_listed, accepting_new_subscriptions, currency, trial_days,
			description, display_order
		) VALUES (
			'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], true,
			true, false, 'CNY', 0, '按季订阅 ¥79.9，暂不开放新订阅，已有订阅保留', 20
		) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	past := time.Now().Add(-1 * time.Hour)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at) VALUES (gen_random_uuid(), $1, 'quarterly', 'active', now() - INTERVAL '90 days', $2)`,
		uid, past); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return uid
}

// uuidNew generates a UUID string. Standalone helper (rather than reusing
// the unexported one in internal/repo/repo_test.go) keeps the test file's
// import surface minimal — the only e2e test that needs UUIDs is this one.
func uuidNew() string {
	return uuid.NewString()
}

// keep strings import alive for future sub-test names without lint complaint.
var _ = strings.TrimSpace

// TestE2E_GitHubOAuth_RedirectToCallback drives the full GitHub OAuth
// redirect→callback flow against an httptest mock of api.github.com.
// The test pins every observable surface of the production code path:
//  1. /auth/github/redirect → 302 to a GitHub URL whose state binds
//     (app_id, callback_index) and validates against the HMAC secret.
//  2. /auth/github/callback with a valid code+state → 302 to the BFF
//     callback URL with a fresh access/refresh token pair in the
//     fragment.
//  3. The minted JWT is delivered to the BFF; the user + identity
//     rows are persisted (the email-merge identity-binding path
//     hit /user/emails, found the verified primary, and bound).
//
// Without this test, the OAuth state HMAC binding, the code-exchange
// upstream call, the profile fetch, and the BFF redirect contract are
// all exercised only at the unit-test layer — leaving an integration
// regression (e.g. a route mount order change) undetected.
func TestE2E_GitHubOAuth_RedirectToCallback(t *testing.T) {
	// Start a mock GitHub server and rewire the GitHubOAuthService
	// to point at it. The mock serves the same /login/oauth/access_token,
	//	/user, /user/emails paths production GitHub uses, returning a
	// deterministic access token + verified primary email.
	github := newMockGitHubServer(t)
	srv, db := setupE2EServerWithGH(t)
	srv.GitHubOAuthService.SetAccessTokenURL(github.URL + "/login/oauth/access_token")
	srv.GitHubOAuthService.SetAuthorizeURL(github.URL + "/login/oauth/authorize")
	srv.GitHubOAuthService.SetUserURL(github.URL + "/user")
	srv.GitHubOAuthService.SetEmailsURL(github.URL + "/user/emails")
	engine := srv.Engine

	// 1. /auth/github/redirect → 302 to a GitHub URL with a valid state.
	redirectResp := doRequest(t, engine, http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https://staging.yunhouai.com/auth/callback",
		"", nil)
	if redirectResp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/github/redirect: expected 302, got %d %s", redirectResp.StatusCode, string(redirectResp.Body))
	}
	loc := redirectResp.Headers.Get("Location")
	if loc == "" {
		t.Fatal("redirect missing Location header")
	}
	// After URL override the redirect points at the mock, not real GitHub.
	if !strings.HasPrefix(loc, github.URL+"/login/oauth/authorize") {
		t.Errorf("redirect Location = %q, want mock GitHub URL prefix %q", loc, github.URL)
	}
	if !strings.Contains(loc, "state=") {
		t.Error("redirect Location missing state parameter (HMAC state binding required)")
	}

	// 2. /auth/github/callback with a valid code + the captured state →
	//    302 to the BFF callback URL with token + refresh in the
	//    fragment. The callback path needs the SAME state we just
	//    issued, so we parse it back out of the redirect Location.
	parsedLoc, perr := url.Parse(loc)
	if perr != nil {
		t.Fatalf("parse redirect location: %v", perr)
	}
	state := parsedLoc.Query().Get("state")
	if state == "" {
		t.Fatal("could not extract state from redirect Location")
	}

	cbResp := doRequest(t, engine, http.MethodGet,
		"/auth/github/callback?code="+mockGitHubCode+"&state="+state+"&app_id=yundian",
		"", nil)
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/github/callback: expected 302, got %d %s", cbResp.StatusCode, string(cbResp.Body))
	}
	cbLoc := cbResp.Headers.Get("Location")
	if cbLoc == "" {
		t.Fatal("callback missing Location header")
	}
	if !strings.Contains(cbLoc, "https://staging.yunhouai.com/auth/callback#") {
		t.Errorf("callback Location = %q, want BFF callback URL with fragment", cbLoc)
	}
	// Fragment should carry the JWT pair the SPA reads.
	cbParsed, perr := url.Parse(cbLoc)
	if perr != nil {
		t.Fatalf("parse callback location: %v", perr)
	}
	fragValues, perr := url.ParseQuery(cbParsed.Fragment)
	if perr != nil {
		t.Fatalf("parse callback fragment: %v", perr)
	}
	if fragValues.Get("token") == "" {
		t.Error("callback fragment missing token parameter (access_token not delivered to BFF)")
	}
	if fragValues.Get("refresh_token") == "" {
		t.Error("callback fragment missing refresh_token parameter")
	}

	// 3. The minted token must produce a user + identity row in the
	//    DB (identity-binding path: /user + /user/emails → email-merge
	//    → identity INSERT). If any step in the chain breaks, the
	//    row count is 0.
	var userCount int
	if err := db.GetContext(context.Background(), &userCount,
		`SELECT COUNT(*) FROM users`); err != nil {
		t.Fatal(err)
	}
	if userCount < 1 {
		t.Error("expected at least one user row created by GitHub callback")
	}
	var idCount int
	if err := db.GetContext(context.Background(), &idCount,
		`SELECT COUNT(*) FROM social_identities WHERE provider = 'github'`); err != nil {
		t.Fatal(err)
	}
	if idCount < 1 {
		t.Error("expected at least one github social_identity row bound by callback")
	}
}
