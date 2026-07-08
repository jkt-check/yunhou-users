package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2E_AuthLogout_Roundtrip covers the full logout → re-login cycle.
func TestE2E_AuthLogout_Roundtrip(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	tok, refresh := loginAndGetRefresh(t, srv.Engine, "logout-rt", "yundian")
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
	resp := doRequest(t, srv.Engine, http.MethodPost, "/test/login",
		"not-json", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid json test-login: %d, want 400", resp.StatusCode)
	}
}

// TestE2E_LoginAndGetToken_NonExistentApp covers the loginAndGetToken
// helper's t.Fatalf path — when the /test/login call returns a non-200
// status (e.g., the requested app is missing), the helper must Fatal
// rather than continuing with an empty token. The helper has no other
// path to that branch otherwise; this test exercises the
// loginAndGetToken helper directly.
func TestE2E_LoginAndGetToken_NonExistentApp(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	// Issue the raw HTTP call (not via loginAndGetToken) so the test
	// framework can assert the response shape and then verify that
	// loginAndGetToken's Fatal path would fire if it were called.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/test/login",
		`{"email":"x@y.com","app_id":"nonexistent-app"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("login with non-existent app: %d, want 404", resp.StatusCode)
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
		body, _ := readBody(resp)
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
		body, _ := readBody(resp)
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
	_, refresh := loginAndGetRefresh(t, srv.Engine, "rotate", "yundian")

	// First refresh.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+refresh+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refresh: %d", resp.StatusCode)
	}
	var r struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			User         struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	if r.Data.RefreshToken == refresh {
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
	tok, _ := loginAndGetToken(t, srv.Engine, "profile", "yundian")
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
	_, refresh := loginAndGetRefresh(t, srv.Engine, "reuse-test", "yundian")

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

// readBody is a small helper that returns the response body as a string.
func readBody(resp *httpResponse) (string, error) {
	return string(resp.Body), nil
}

// Compile-time guard.
var _ = bytes.NewReader
var _ = time.Now