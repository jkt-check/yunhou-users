package e2e

import (
	"net/http"
	"testing"
)

// TestLoginFlow exercises the complete login flow:
// login → user profile → refresh → logout.
func TestLoginFlow(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login with GitHub provider token
	t.Run("login", func(t *testing.T) {
		loginBody := `{"provider":"github","provider_token":"test-user-token","app_id":"yundian"}`
		resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var loginResp struct {
			Data struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				User         struct {
					ID string `json:"id"`
				} `json:"user"`
				Subscription struct {
					PlanID    string `json:"plan_id"`
					HasAccess bool   `json:"has_access"`
				} `json:"subscription"`
			} `json:"data"`
		}
		resp.JSON(t, &loginResp)
		if loginResp.Data.AccessToken == "" {
			t.Fatal("access token is empty")
		}
		if loginResp.Data.RefreshToken == "" {
			t.Fatal("refresh token is empty")
		}
		if loginResp.Data.User.ID == "" {
			t.Fatal("user id is empty")
		}
		if loginResp.Data.Subscription.PlanID != "free" {
			t.Fatalf("expected free plan, got %s", loginResp.Data.Subscription.PlanID)
		}
		if !loginResp.Data.Subscription.HasAccess {
			t.Error("expected has_access=true for yundian on free plan")
		}
	})

	// Login to app not in free plan
	t.Run("login_paid_app_no_subscription", func(t *testing.T) {
		loginBody := `{"provider":"github","provider_token":"another-user-token","app_id":"yundash"}`
		resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var loginResp struct {
			Data struct {
				Subscription struct {
					PlanID    string `json:"plan_id"`
					HasAccess bool   `json:"has_access"`
				} `json:"subscription"`
			} `json:"data"`
		}
		resp.JSON(t, &loginResp)
		if loginResp.Data.Subscription.HasAccess {
			t.Error("expected has_access=false for yundash on free plan")
		}
	})
}

// TestAuthRefresh exercises token refresh with rotation.
func TestAuthRefresh(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login first
	loginBody := `{"provider":"github","provider_token":"refresh-test-user","app_id":"yundian"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	var loginResp struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	resp.JSON(t, &loginResp)

	// Refresh tokens
	refreshBody := `{"refresh_token":"` + loginResp.Data.RefreshToken + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/refresh", refreshBody, nil)
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
	oldRefreshBody := `{"refresh_token":"` + loginResp.Data.RefreshToken + `"}`
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
	loginBody := `{"provider":"github","provider_token":"logout-test-user","app_id":"yundian"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	var loginResp struct {
		Data struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	resp.JSON(t, &loginResp)

	// Logout
	logoutBody := `{"refresh_token":"` + loginResp.Data.RefreshToken + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/logout", logoutBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Refresh token should be revoked
	refreshBody := `{"refresh_token":"` + loginResp.Data.RefreshToken + `"}`
	resp = doRequest(t, engine, http.MethodPost, "/auth/refresh", refreshBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: expected 401, got %d", resp.StatusCode)
	}
}

// TestUserProfileWithJWT exercises user endpoints with JWT auth.
func TestUserProfileWithJWT(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// Login to get token
	loginBody := `{"provider":"github","provider_token":"profile-test-user","app_id":"yundian"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	var loginResp struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	resp.JSON(t, &loginResp)

	authHeaders := map[string]string{"Authorization": "Bearer " + loginResp.Data.AccessToken}

	// Get profile
	resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders)
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
	if identities.Data[0].Provider != "github" {
		t.Fatalf("expected github, got %s", identities.Data[0].Provider)
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
			ID    string   `json:"id"`
			Name  string   `json:"name"`
			Apps  []string `json:"apps"`
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
			ID    string   `json:"id"`
			Apps  []string `json:"apps"`
		} `json:"data"`
	}
	resp.JSON(t, &freePlan)
	if freePlan.Data.ID != "free" {
		t.Errorf("expected free plan, got %s", freePlan.Data.ID)
	}
}

// TestUnsupportedProvider verifies unsupported provider is rejected.
func TestUnsupportedProvider(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	loginBody := `{"provider":"facebook","provider_token":"test","app_id":"yundian"}`
	resp := doRequest(t, engine, http.MethodPost, "/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported provider, got %d", resp.StatusCode)
	}
}
