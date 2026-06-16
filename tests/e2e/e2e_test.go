package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestFullOAuthFlow exercises the complete end-to-end OAuth 2.0 flow:
// register app → authorize → callback → exchange → user profile →
// refresh → cancel subscription → verify refresh fails.
func TestFullOAuthFlow(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	_ = db

	app := createAppViaHTTP(t, engine, "My Test App", []string{"http://localhost:3000/auth/callback"})

	// JWKS endpoint
	t.Run("jwks", func(t *testing.T) {
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
	})

	// Authorize redirect
	t.Run("authorize_redirect", func(t *testing.T) {
		resp := doRequest(t, engine, http.MethodGet,
			fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:3000/auth/callback&state=xyz", app.ID),
			"", nil)
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("expected 307, got %d", resp.StatusCode)
		}
		loc := resp.Location()
		if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
			t.Fatalf("expected redirect to github, got %s", loc)
		}
	})

	// Full flow
	t.Run("full_flow", func(t *testing.T) {
		// Authorize to get signed state
		resp := doRequest(t, engine, http.MethodGet,
			fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:3000/auth/callback&state=xyz", app.ID),
			"", nil)
		oauthState := extractQuery(t, resp.Location(), "state")

		// Simulate GitHub callback
		resp = doRequest(t, engine, http.MethodGet,
			fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState), "", nil)
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("callback: expected 307, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		loc := resp.Location()
		if !strings.HasPrefix(loc, "http://localhost:3000/auth/callback") {
			t.Fatalf("callback redirect: expected redirect to consumer app, got %s", loc)
		}
		authCode := extractQuery(t, loc, "code")
		if authCode == "" {
			t.Fatal("auth code missing from callback redirect")
		}
		if extractQuery(t, loc, "state") != "xyz" {
			t.Fatalf("callback state: expected 'xyz', got %q", extractQuery(t, loc, "state"))
		}

		// Exchange auth code for tokens
		exchangeBody := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode, app.ID, app.Secret)
		resp = doRequest(t, engine, http.MethodPost, "/token", exchangeBody, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exchange: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var tokenResp struct {
			Data struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				TokenType    string `json:"token_type"`
			} `json:"data"`
		}
		resp.JSON(t, &tokenResp)
		if tokenResp.Data.AccessToken == "" {
			t.Fatal("access token is empty")
		}
		if tokenResp.Data.RefreshToken == "" {
			t.Fatal("refresh token is empty")
		}
		if tokenResp.Data.TokenType != "Bearer" {
			t.Fatalf("expected Bearer, got %s", tokenResp.Data.TokenType)
		}

		// Use access token to get user profile
		authHeaders := map[string]string{"Authorization": "Bearer " + tokenResp.Data.AccessToken}
		resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get profile: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var profile struct {
			Data struct {
				ID        string  `json:"id"`
				Nickname  *string `json:"nickname"`
				AvatarURL *string `json:"avatar_url"`
				Status    string  `json:"status"`
			} `json:"data"`
		}
		resp.JSON(t, &profile)
		if profile.Data.ID == "" {
			t.Fatal("user id is empty")
		}
		if profile.Data.Nickname == nil || *profile.Data.Nickname != mockGitHubUser {
			t.Fatalf("expected nickname %q, got %v", mockGitHubUser, profile.Data.Nickname)
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
				Provider    string  `json:"provider"`
				ProviderUID string  `json:"provider_uid"`
				Email       *string `json:"email"`
			} `json:"data"`
		}
		resp.JSON(t, &identities)
		if len(identities.Data) != 1 {
			t.Fatalf("expected 1 identity, got %d", len(identities.Data))
		}
		if identities.Data[0].Provider != "github" {
			t.Fatalf("expected github, got %s", identities.Data[0].Provider)
		}
		if identities.Data[0].ProviderUID != mockGitHubUID {
			t.Fatalf("expected uid %s, got %s", mockGitHubUID, identities.Data[0].ProviderUID)
		}

		// List user's subscribed apps
		resp = doRequest(t, engine, http.MethodGet, "/user/apps", "", authHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list apps: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var apps struct {
			Data []struct {
				ID   string `json:"id"`
				Plan string `json:"plan"`
			} `json:"data"`
		}
		resp.JSON(t, &apps)
		if len(apps.Data) != 1 {
			t.Fatalf("expected 1 app subscription, got %d", len(apps.Data))
		}
		if apps.Data[0].Plan != "free" {
			t.Fatalf("expected free plan, got %s", apps.Data[0].Plan)
		}

		// Refresh tokens
		refreshBody := fmt.Sprintf(`{"refresh_token":"%s","app_id":"%s","app_secret":"%s"}`,
			tokenResp.Data.RefreshToken, app.ID, app.Secret)
		resp = doRequest(t, engine, http.MethodPost, "/token/refresh", refreshBody, nil)
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

		// Old refresh token should be revoked (replay protection)
		resp = doRequest(t, engine, http.MethodPost, "/token/refresh", refreshBody, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("reuse old refresh token: expected 401, got %d", resp.StatusCode)
		}

		// New access token still works
		newAuthHeaders := map[string]string{"Authorization": "Bearer " + refreshResp.Data.AccessToken}
		resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", newAuthHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("profile with refreshed token: expected 200, got %d", resp.StatusCode)
		}

		// Cancel the subscription (auto-created during callback)
		resp = doRequest(t, engine, http.MethodGet, "/user/apps", "", newAuthHeaders)
		var userApps struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		resp.JSON(t, &userApps)
		if len(userApps.Data) == 0 {
			t.Fatal("no subscriptions found for user")
		}
		subID := userApps.Data[0].ID

		cancelHeaders := map[string]string{"X-App-ID": app.ID, "X-App-Secret": app.Secret}
		resp = doRequest(t, engine, http.MethodDelete, fmt.Sprintf("/subscriptions/%s", subID), "", cancelHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cancel subscription: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}

		// After cancellation, refresh should fail
		refreshBody2 := fmt.Sprintf(`{"refresh_token":"%s","app_id":"%s","app_secret":"%s"}`,
			refreshResp.Data.RefreshToken, app.ID, app.Secret)
		resp = doRequest(t, engine, http.MethodPost, "/token/refresh", refreshBody2, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("refresh after cancellation: expected 401, got %d", resp.StatusCode)
		}
	})
}

// TestAuthCodeReplayProtection verifies that an auth code cannot be used twice.
func TestAuthCodeReplayProtection(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Replay Test App", []string{"http://localhost:4000/cb"})

	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:4000/cb&state=abc", app.ID),
		"", nil)
	oauthState := extractQuery(t, resp.Location(), "state")

	resp =doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState), "", nil)
	authCode := extractQuery(t, resp.Location(), "code")

	// First exchange succeeds
	body := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode, app.ID, app.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first exchange: expected 200, got %d", resp.StatusCode)
	}

	// Second exchange fails
	resp = doRequest(t, engine, http.MethodPost, "/token", body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second exchange: expected 401, got %d", resp.StatusCode)
	}
}

// TestWrongAppSecret verifies exchange fails with wrong app secret.
func TestWrongAppSecret(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Wrong Secret App", []string{"http://localhost:5000/cb"})

	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:5000/cb&state=def", app.ID),
		"", nil)
	oauthState := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState), "", nil)
	authCode := extractQuery(t, resp.Location(), "code")

	// Exchange with wrong secret
	body := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"wrong-secret"}`, authCode, app.ID)
	resp = doRequest(t, engine, http.MethodPost, "/token", body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong secret, got %d", resp.StatusCode)
	}
}

// TestRefreshWithWrongApp verifies that a refresh token cannot be used by a different app.
func TestRefreshWithWrongApp(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app1 := createAppViaHTTP(t, engine, "App One", []string{"http://localhost:6001/cb"})
	app2 := createAppViaHTTP(t, engine, "App Two", []string{"http://localhost:6002/cb"})

	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:6001/cb&state=s1", app1.ID),
		"", nil)
	oauthState := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState), "", nil)
	authCode := extractQuery(t, resp.Location(), "code")

	body := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode, app1.ID, app1.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token", body, nil)
	var tokenResp struct {
		Data struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	resp.JSON(t, &tokenResp)

	// Try to refresh using app2's credentials
	refreshBody := fmt.Sprintf(`{"refresh_token":"%s","app_id":"%s","app_secret":"%s"}`,
		tokenResp.Data.RefreshToken, app2.ID, app2.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token/refresh", refreshBody, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh with wrong app: expected 401, got %d", resp.StatusCode)
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
		{http.MethodGet, "/user/apps"},
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

// TestInvalidAuthorizeParams verifies the authorize endpoint validates input.
func TestInvalidAuthorizeParams(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"missing app_id", "provider=github&redirect_uri=http://localhost/cb", http.StatusBadRequest},
		{"missing provider", "app_id=x&redirect_uri=http://localhost/cb", http.StatusBadRequest},
		{"missing redirect_uri", "app_id=x&provider=github", http.StatusBadRequest},
		{"invalidapp_id", "app_id=nonexistent&provider=github&redirect_uri=http://localhost/cb", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, engine, http.MethodGet, "/authorize?"+tc.query, "", nil)
			if resp.StatusCode != tc.status {
				t.Errorf("expected %d, got %d — body: %s", tc.status, resp.StatusCode, string(resp.Body))
			}
		})
	}
}

// TestInvalidGitHubCode verifies that an invalid GitHub code is handled.
func TestInvalidGitHubCode(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Bad Code App", []string{"http://localhost:7000/cb"})

	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:7000/cb&state=bad", app.ID),
		"", nil)
	oauthState := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=wrong-code&state=%s", oauthState), "", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 for invalid GitHub code, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
}

// TestUpdateUserProfile verifies the user profile update endpoint.
func TestUpdateUserProfile(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Profile App", []string{"http://localhost:8000/cb"})

	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:8000/cb&state=p1", app.ID),
		"", nil)
	oauthState := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState), "", nil)
	authCode := extractQuery(t, resp.Location(), "code")

	body := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode, app.ID, app.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token", body, nil)
	var tokenResp struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	resp.JSON(t, &tokenResp)

	authHeaders := map[string]string{"Authorization": "Bearer " + tokenResp.Data.AccessToken}

	// Update profile
	resp = doRequest(t, engine, http.MethodPatch, "/user/profile",
		`{"nickname":"updated-name","avatar_url":"https://avatars.test/new"}`, authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update profile: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Verify updated profile
	resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders)
	var profile struct {
		Data struct {
			Nickname  *string `json:"nickname"`
			AvatarURL *string `json:"avatar_url"`
		} `json:"data"`
	}
	resp.JSON(t, &profile)
	if profile.Data.Nickname == nil || *profile.Data.Nickname != "updated-name" {
		t.Fatalf("expected nickname 'updated-name', got %v", profile.Data.Nickname)
	}
	if profile.Data.AvatarURL == nil || *profile.Data.AvatarURL != "https://avatars.test/new" {
		t.Fatalf("expected avatar_url 'https://avatars.test/new', got %v", profile.Data.AvatarURL)
	}
}

// TestAppManagement verifies the full app lifecycle.
func TestAppManagement(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Managed App", []string{"http://localhost:9000/cb"})

	appHeaders := map[string]string{"X-App-ID": app.ID, "X-App-Secret": app.Secret}

	// Get app details
	resp := doRequest(t, engine, http.MethodGet, fmt.Sprintf("/apps/%s", app.ID), "", appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var appDetail struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	resp.JSON(t, &appDetail)
	if appDetail.Data.Name != "Managed App" {
		t.Fatalf("expected name 'Managed App', got %s", appDetail.Data.Name)
	}

	// Update app
	resp = doRequest(t, engine, http.MethodPatch, fmt.Sprintf("/apps/%s", app.ID),
		`{"name":"Renamed App"}`, appHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update app: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Verify update
	resp = doRequest(t, engine, http.MethodGet, fmt.Sprintf("/apps/%s", app.ID), "", appHeaders)
	resp.JSON(t, &appDetail)
	if appDetail.Data.Name != "Renamed App" {
		t.Fatalf("expected 'Renamed App', got %s", appDetail.Data.Name)
	}

	// Cross-app access forbidden
	superHeaders := map[string]string{"X-App-ID": superAppID, "X-App-Secret": superAppSecret}
	resp = doRequest(t, engine, http.MethodGet, fmt.Sprintf("/apps/%s", app.ID), "", superHeaders)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-app access: expected 403, got %d", resp.StatusCode)
	}
}

// TestHMACStateValidation verifies that tampered state parameters are rejected.
func TestHMACStateValidation(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	_ = createAppViaHTTP(t, engine, "HMAC Test App", []string{"http://localhost:10000/cb"})

	// Callback with fabricated state
	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=tampered-state-value", mockGitHubCode), "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for tampered state, got %d", resp.StatusCode)
	}
}

// TestSameUserSecondLogin verifies that logging in again returns the same user.
func TestSameUserSecondLogin(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	app := createAppViaHTTP(t, engine, "Second Login App", []string{"http://localhost:11000/cb"})

	// First login
	resp := doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:11000/cb&state=first", app.ID),
		"", nil)
	oauthState1 := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState1), "", nil)
	authCode1 := extractQuery(t, resp.Location(), "code")

	body1:= fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode1, app.ID, app.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token", body1, nil)
	var token1 struct {
		Data struct{
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	resp.JSON(t, &token1)

	authHeaders1 := map[string]string{"Authorization": "Bearer " + token1.Data.AccessToken}
	resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders1)
	var profile1 struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &profile1)

	// Second login (same GitHub user)
	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/authorize?app_id=%s&provider=github&redirect_uri=http://localhost:11000/cb&state=second", app.ID),
		"", nil)
	oauthState2 := extractQuery(t, resp.Location(), "state")

	resp = doRequest(t, engine, http.MethodGet,
		fmt.Sprintf("/callback/github?code=%s&state=%s", mockGitHubCode, oauthState2), "", nil)
	authCode2 := extractQuery(t, resp.Location(), "code")

	body2 := fmt.Sprintf(`{"code":"%s","app_id":"%s","app_secret":"%s"}`, authCode2, app.ID, app.Secret)
	resp = doRequest(t, engine, http.MethodPost, "/token", body2, nil)
	var token2 struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	resp.JSON(t, &token2)

	authHeaders2 := map[string]string{"Authorization": "Bearer " + token2.Data.AccessToken}
	resp = doRequest(t, engine, http.MethodGet, "/user/profile", "", authHeaders2)
	var profile2 struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &profile2)

	if profile1.Data.ID != profile2.Data.ID {
		t.Fatalf("expected same user ID, got %s and %s", profile1.Data.ID, profile2.Data.ID)
	}
}
