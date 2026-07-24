package e2e

import (
	"net/http"
	"testing"
)

// TestLoginFlow exercises the complete login flow:
// login → user profile → refresh → logout.
func TestLoginFlow(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login with test endpoint (PAYPAL_L3_E2E_MODE=1, see setupE2EServer).
	t.Run("login", func(t *testing.T) {
		r := loginAndGetTokens(t, engine, "test-user-token", "yundian")
		access := r.AccessToken
		refresh := r.RefreshToken
		sub := r.Subscription
		if access == "" {
			t.Fatal("access token is empty")
		}
		if refresh == "" {
			t.Fatal("refresh token is empty")
		}
		if sub.PlanID != "monthly" {
			t.Fatalf("expected monthly plan, got %s", sub.PlanID)
		}
		if !sub.HasAccess {
			t.Error("expected has_access=true for yundian on monthly plan")
		}
	})

	// Login to another app included in the requested monthly plan.
	t.Run("login_paid_app_with_requested_plan", func(t *testing.T) {
		r := loginAndGetTokens(t, engine, "another-user-token", "yundash")
		sub := r.Subscription
		if !sub.HasAccess {
			t.Error("expected has_access=true for yundash on monthly plan")
		}
	})
}

// TestAuthRefresh exercises token refresh with rotation.
func TestAuthRefresh(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login first
	r := loginAndGetTokens(t, engine, "refresh-test-user", "yundian")
	refresh := r.RefreshToken

	// Refresh tokens
	refreshBody := `{"refresh_token":"` + refresh + `"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/refresh", refreshBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var refreshResp struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	resp.JSON(t, &refreshResp)
	if refreshResp.Data.AccessToken == "" || refreshResp.Data.RefreshToken == "" {
		t.Fatal("refreshed tokens are empty")
	}

	// Old refresh token should be revoked
	oldRefreshBody := `{"refresh_token":"` + refresh + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/refresh", oldRefreshBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reuse old refresh token: expected 401, got %d", resp.StatusCode)
	}

	// New refresh token works
	newRefreshBody := `{"refresh_token":"` + refreshResp.Data.RefreshToken + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/refresh", newRefreshBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new refresh token: expected 200, got %d", resp.StatusCode)
	}
}

// TestAuthLogout exercises logout.
func TestAuthLogout(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login first
	r := loginAndGetTokens(t, engine, "logout-test-user", "yundian")
	refresh := r.RefreshToken

	// Logout
	logoutBody := `{"refresh_token":"` + refresh + `"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/logout", logoutBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Refresh token should be revoked
	refreshBody := `{"refresh_token":"` + refresh + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/refresh", refreshBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: expected 401, got %d", resp.StatusCode)
	}
}

// TestUserProfileWithJWT exercises user endpoints with JWT auth.
func TestUserProfileWithJWT(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login to get token
	access := loginAndGetTokens(t, engine, "profile-test-user", "yundian").AccessToken
	authHeaders := map[string]string{"Authorization": "Bearer " + access}

	// Get profile
	resp := doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get profile: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var profile struct {
		Data struct {
			ID       string `json:"id"`
			Nickname string `json:"nickname"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	resp.JSON(t, &profile)
	if profile.Data.ID == "" {
		t.Fatal("user id is empty")
	}
	if profile.Data.Status != "active" {
		t.Fatalf("expected active, got %s", profile.Data.Status)
	}

	// List identities
	resp = doRequest(t, engine, http.MethodGet, "/user/identities", "", authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list identities: expected 200, got %d", resp.StatusCode)
	}
	var identities struct {
		Data []struct {
			Provider string `json:"provider"`
		} `json:"data"`
	}
	resp.JSON(t, &identities)
	if len(identities.Data) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(identities.Data))
	}
	// Provider must be "github" — a regression that drops the GitHub
	// identity (e.g. sets Provider="" or "test") would otherwise pass.
	if identities.Data[0].Provider != "github" {
		t.Errorf("identity provider: expected github, got %q", identities.Data[0].Provider)
	}

	// List subscriptions
	resp = doRequest(t, engine, http.MethodGet, "/user/subscriptions", "", authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subscriptions: expected 200, got %d", resp.StatusCode)
	}
}

// TestUnauthorizedAccess verifies user endpoints reject requests without a valid token.
func TestUnauthorizedAccess(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/user/profile"},
		{http.MethodPatch, "/user/profile"},
		{http.MethodGet, "/user/identities"},
		{http.MethodDelete, "/user/identities/00000000-0000-0000-0000-000000000000"},
		{http.MethodGet, "/user/subscriptions"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doRequest(t, engine, ep.method, ep.path, "", nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", resp.StatusCode)
			}
		})
	}
}

// TestJWKSEndpoint verifies the JWKS endpoint.
func TestJWKSEndpoint(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	resp := doRequest(t, engine, http.MethodGet, "/.well-known/jwks.json", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []map[string]interface{} `json:"keys"`
	}
	resp.JSON(t, &jwks)
	if len(jwks.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(jwks.Keys))
	}
	if jwks.Keys[0]["kty"] != "RSA" {
		t.Fatalf("expected RSA key, got %v", jwks.Keys[0]["kty"])
	}
}

// TestAppManagement verifies app listing.
func TestAppManagement(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appHeaders := appAuthHeaders(superAppID)

	// List apps
	resp := doRequest(t, engine, http.MethodGet, "/apps", "", appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var apps struct {
		Data []struct {
			AppID string `json:"app_id"`
			Name  string `json:"name"`
		} `json:"data"`
	}
	resp.JSON(t, &apps)
	if len(apps.Data) == 0 {
		t.Fatal("expected at least one app")
	}

	// Get app details
	resp = doRequest(t, engine, http.MethodGet, "/apps/"+superAppID, "", appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: expected 200, got %d", resp.StatusCode)
	}
}

// TestPlanManagement verifies plan CRUD.
func TestPlanManagement(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appHeaders := appAuthHeaders(superAppID)

	// List plans
	resp := doRequest(t, engine, http.MethodGet, "/admin/plans", "", appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list plans: expected 200, got %d", resp.StatusCode)
	}
	var plans struct {
		Data []struct {
			ID   string   `json:"id"`
			Name string   `json:"name"`
			Apps []string `json:"apps"`
		} `json:"data"`
	}
	resp.JSON(t, &plans)
	if len(plans.Data) < 2 {
		t.Fatalf("expected at least 2 plans, got %d", len(plans.Data))
	}

	// Get plan details
	resp = doRequest(t, engine, http.MethodGet, "/admin/plans/free", "", appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get plan: expected 200, got %d", resp.StatusCode)
	}
	var freePlan struct {
		Data struct {
			ID   string   `json:"id"`
			Apps []string `json:"apps"`
		} `json:"data"`
	}
	resp.JSON(t, &freePlan)
	if freePlan.Data.ID != "free" {
		t.Errorf("expected free plan, got %s", freePlan.Data.ID)
	}
}

// TestTestLoginMalformed verifies /test/login rejects a request missing the
// required email field. Replaces the old TestUnsupportedProvider which
// drove /auth/login (removed by commit 5ef27ce).
func TestTestLoginMalformed(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	resp := doRequest(t, engine, http.MethodPost, "/test/login?plan_id=monthly",
		`{"app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing email, got %d", resp.StatusCode)
	}
}
