package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// stubAppLoader is a minimal appLoader for the GitHub OAuth handler tests.
type stubAppLoader struct {
	app *model.App
	err error
}

func (s *stubAppLoader) FindByID(_ context.Context, _ string) (*model.App, error) {
	return s.app, s.err
}

// stubAuthSvc captures the LoginWithProfile call so the callback handler's
// downstream service invocation can be exercised.
type stubAuthSvc struct {
	loginResp *service.LoginResponse
	loginErr  error
	calls     int
}

func (s *stubAuthSvc) LoginWithProfile(_ context.Context, _ service.LoginWithProfileRequest) (*service.LoginResponse, error) {
	s.calls++
	if s.loginErr != nil {
		return nil, s.loginErr
	}
	return s.loginResp, nil
}
func (s *stubAuthSvc) Logout(_ context.Context, _ string) error { return nil }
func (s *stubAuthSvc) RefreshToken(_ context.Context, _, _ string) (*service.LoginResponse, error) {
	return nil, nil
}
func (s *stubAuthSvc) TestLogin(_ context.Context, _ service.TestLoginRequest) (*service.LoginResponse, error) {
	return nil, nil
}

const ghTestSecret = "test-handler-oauth-secret-padding"

func ghAppWithOAuth(callbackURLs ...string) *model.App {
	cfg := model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			GitHub: &model.GitHubOAuthConfig{
				ClientID:     "Iv1.handler-test",
				ClientSecret: "handler-secret",
				CallbackURLs: callbackURLs,
			},
		},
	}
	raw, _ := json.Marshal(cfg)
	return &model.App{AppID: "yundian", Name: "Yundian", Config: raw, IsActive: true}
}

type tokenSvcStub struct{}

func (tokenSvcStub) JWKS() map[string]interface{}                             { return nil }
func (tokenSvcStub) SignAccessToken(string, string, []string) (string, error) { return "", nil }
func (tokenSvcStub) VerifyAccessToken(string) (*service.TokenClaims, error)   { return nil, nil }
func (tokenSvcStub) Refresh(context.Context, string, string) (string, string, error) {
	return "", "", nil
}

// =========================================================================
// /auth/github/redirect tests
// =========================================================================

func TestGitHubOAuth_Redirect_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: ghAppWithOAuth("https://yundian.com/auth/callback")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fyundian.com%2Fauth%2Fcallback", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
		t.Errorf("Location prefix wrong: %s", loc)
	}
	if !strings.Contains(loc, "client_id=Iv1.handler-test") {
		t.Errorf("Location missing client_id: %s", loc)
	}
}

func TestGitHubOAuth_Redirect_MissingAppID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/redirect?redirect_uri=https%3A%2F%2Fx", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Redirect_MissingRedirectURI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/redirect?app_id=yundian", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Redirect_AppNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{err: errors.New("db down")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=missing&redirect_uri=https%3A%2F%2Fx", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGitHubOAuth_Redirect_GitHubNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: &model.App{AppID: "yundian", Name: "Yundian", IsActive: true}}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fx", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGitHubOAuth_Redirect_CallbackURLMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: ghAppWithOAuth("https://yundian.com/auth/callback")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fevil.com%2Fcb", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Redirect_MalformedConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &model.App{
		AppID: "yundian", Name: "Yundian", IsActive: true,
		Config: json.RawMessage(`{not-json`),
	}
	appRepo := &stubAppLoader{app: app}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fx", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestGitHubOAuth_Redirect_InactiveApp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := ghAppWithOAuth("https://yundian.com/auth/callback")
	app.IsActive = false
	appRepo := &stubAppLoader{app: app}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	// Inactive apps must not get a real GitHub authorize URL — the user
	// would complete OAuth consent only to be rejected at /callback. The
	// response is the same 404 as the unknown-app branch so pre-login
	// callers can't enumerate app_ids by diffing status codes.
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fyundian.com%2Fauth%2Fcallback", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// =========================================================================
// /auth/github/callback tests
// =========================================================================

// fixedTestClock pins the handler's wall clock to a known value so state
// we sign with a fixed timestamp doesn't expire before the handler can
// verify it. Tests install this via t.Cleanup.
var fixedTestClock = func() time.Time { return time.Unix(1_783_439_737, 0) }

func installFixedClock(t *testing.T) {
	t.Helper()
	prev := githubOAuthClock
	githubOAuthClock = fixedTestClock
	t.Cleanup(func() { githubOAuthClock = prev })
}

func callbackURIFor(t *testing.T, svc *service.GitHubOAuthService, appID string, callbackIdx int, cbURL string) string {
	t.Helper()
	installFixedClock(t)
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.handler-test",
		ClientSecret: "handler-secret",
		CallbackURLs: []string{cbURL},
	}
	u, err := svc.BuildAuthorizeURL(appID, cfg, callbackIdx, fixedTestClock())
	if err != nil {
		t.Fatalf("issue state: %v", err)
	}
	return extractStateFromURL(t, u)
}

func extractStateFromURL(t *testing.T, raw string) string {
	t.Helper()
	for _, part := range strings.Split(strings.SplitN(raw, "?", 2)[1], "&") {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if part[:eq] == "state" {
			return part[eq+1:]
		}
	}
	t.Fatalf("no state in %s", raw)
	return ""
}

func TestGitHubOAuth_Callback_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "gho_callback_test",
				"token_type":   "bearer",
				"scope":        "read:user,user:email",
			})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 99, "login": "octocat", "name": "Octo"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	app := ghAppWithOAuth(cbURL)
	appRepo := &stubAppLoader{app: app}
	authSvc := &stubAuthSvc{
		loginResp: &service.LoginResponse{
			AccessToken:  "yunhou-access",
			RefreshToken: "yunhou-refresh",
			User:         service.UserInfo{ID: "user-uuid"},
			Subscription: &service.SubscriptionInfo{PlanID: "free", HasAccess: true},
		},
	}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	url := "/auth/github/callback?app_id=yundian&code=auth-code&state=" + state
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cbURL+"#") {
		t.Errorf("Location prefix wrong: %s", loc)
	}
	if !strings.Contains(loc, "token=yunhou-access") {
		t.Errorf("Location missing token: %s", loc)
	}
	if !strings.Contains(loc, "has_access=true") {
		t.Errorf("Location missing has_access=true: %s", loc)
	}
	if authSvc.calls != 1 {
		t.Errorf("authSvc.calls = %d, want 1", authSvc.calls)
	}
}

func TestGitHubOAuth_Callback_ExchangeCodeFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "bad_verification_code",
			"error_description": "expired",
		})
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if authSvc.calls != 0 {
		t.Errorf("authSvc.calls = %d, want 0", authSvc.calls)
	}
}

func TestGitHubOAuth_Callback_ProfileFetchError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "gho_x",
				"token_type":   "bearer",
			})
		case "/user":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestGitHubOAuth_Callback_AuthServiceError(t *testing.T) {
	// Inject a mock that returns success for ExchangeCode + /user +
	// /user/emails, but the AuthService.Login returns
	// ErrInvalidProviderToken → handler maps to 401.
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "gho_x",
				"token_type":   "bearer",
			})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 99, "login": "octocat", "name": "Octo"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{loginErr: service.ErrInvalidProviderToken}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// New behavior: auth errors redirect back to BFF with the error in the
	// fragment instead of stranding the browser on a JSON error page.
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", w.Code)
	}
	if authSvc.calls != 1 {
		t.Errorf("authSvc.calls = %d, want 1", authSvc.calls)
	}
}

func TestGitHubOAuth_Callback_AuthServiceUnknownError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_x"})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 99, "login": "octocat"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{loginErr: errors.New("db exploded")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestGitHubOAuth_Callback_GitHubErrorParam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	// Sign a valid state so the handler can identify which callback
	// URL the user started from. Mirrors the production flow.
	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?error=access_denied&error_description=user+said+no&state="+state+"&app_id=yundian", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// New behavior: GitHub's ?error= redirects back to the BFF callback URL
	// with the error in the fragment, instead of stranding the browser
	// on a JSON 400 page.
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cbURL+"#") {
		t.Errorf("Location prefix wrong: %s", loc)
	}
	if !strings.Contains(loc, "error=access_denied") {
		t.Errorf("Location missing error fragment: %s", loc)
	}
	if !strings.Contains(loc, "error_description=user+said+no") {
		t.Errorf("Location missing error_description: %s", loc)
	}
}

func TestGitHubOAuth_Callback_GitHubErrorParamNoAppID(t *testing.T) {
	// No app_id supplied → can't identify the BFF's redirect_uri →
	// falls back to JSON 400 so the caller at least sees the error.
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?error=access_denied&state=x", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Callback_AppInactiveRedirects(t *testing.T) {
	// Race: app was active when state was issued, then disabled before
	// the callback. The user has completed GitHub consent — stranding
	// them on a JSON 401 is bad UX. Redirect to the BFF callback URL
	// with the failure reason in the fragment.
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_x"})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 99, "login": "octocat"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{loginErr: service.ErrAppInactive}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (race: app disabled mid-callback)", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cbURL+"#") {
		t.Errorf("Location prefix wrong: %s", loc)
	}
	if !strings.Contains(loc, "error=auth_failed") {
		t.Errorf("Location missing error=auth_failed: %s", loc)
	}
	if !strings.Contains(loc, "reason=app_disabled") {
		t.Errorf("Location missing reason=app_disabled: %s", loc)
	}
}

func TestGitHubOAuth_Callback_UserSuspendedRedirects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_x"})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 99, "login": "octocat"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		}
	}))
	defer upstream.Close()

	cbURL := "https://yundian.com/auth/callback"
	appRepo := &stubAppLoader{app: ghAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{loginErr: service.ErrUserSuspended}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	svc.SetAccessTokenURL(upstream.URL + "/login/oauth/access_token")
	svc.SetUserURL(upstream.URL + "/user")
	svc.SetEmailsURL(upstream.URL + "/user/emails")
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, authSvc, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, cbURL)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "reason=user_suspended") {
		t.Errorf("Location missing reason=user_suspended: %s", loc)
	}
}

func TestGitHubOAuth_Callback_MissingCodeOrState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: ghAppWithOAuth("https://x")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?app_id=yundian", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Callback_MissingAppID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: ghAppWithOAuth("https://x")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Callback_InvalidState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{app: ghAppWithOAuth("https://x")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state=not-a-valid-state", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGitHubOAuth_Callback_AppNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &stubAppLoader{err: errors.New("not found")}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, "https://yundian.com/auth/callback")
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGitHubOAuth_Callback_GitHubNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &model.App{AppID: "yundian", Name: "Yundian", IsActive: true}
	appRepo := &stubAppLoader{app: app}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	// Need a valid state for "yundian" so state-verify passes and we
	// exercise the GitHubNotConfigured branch that follows.
	state := callbackURIFor(t, svc, "yundian", 0, "https://x")
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// GitHubNotConfigured → 404 (handler branch for missing oauth config).
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGitHubOAuth_Callback_MalformedAppConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &model.App{
		AppID: "yundian", Name: "Yundian", IsActive: true,
		Config: json.RawMessage(`{not-json`),
	}
	appRepo := &stubAppLoader{app: app}
	svc := service.NewGitHubOAuthService(ghTestSecret)
	engine := gin.New()
	RegisterGitHubOAuthRoutes(engine.Group("/auth/github"), svc, appRepo, &stubAuthSvc{}, tokenSvcStub{})

	state := callbackURIFor(t, svc, "yundian", 0, "https://x")
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?app_id=yundian&code=c&state="+state, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// =========================================================================
// lookupGitHubConfig tests
// =========================================================================

func TestLookupGitHubConfig_NilApp(t *testing.T) {
	_, _, err := lookupGitHubConfig(nil, "")
	if err != service.ErrGitHubNotConfigured {
		t.Errorf("err = %v, want service.ErrGitHubNotConfigured", err)
	}
}

func TestLookupGitHubConfig_EmptyConfig(t *testing.T) {
	_, _, err := lookupGitHubConfig(&model.App{}, "")
	if err != service.ErrGitHubNotConfigured {
		t.Errorf("err = %v, want service.ErrGitHubNotConfigured", err)
	}
}

func TestLookupGitHubConfig_MalformedJSON(t *testing.T) {
	app := &model.App{Config: json.RawMessage(`{not-json`)}
	_, _, err := lookupGitHubConfig(app, "")
	if err == nil {
		t.Error("expected error for malformed config")
	}
}

func TestLookupGitHubConfig_NoOAuthProviders(t *testing.T) {
	raw, _ := json.Marshal(model.AppConfig{Brand: &model.BrandConfig{Name: "x"}})
	app := &model.App{Config: raw}
	_, _, err := lookupGitHubConfig(app, "")
	if err != service.ErrGitHubNotConfigured {
		t.Errorf("err = %v, want service.ErrGitHubNotConfigured", err)
	}
}

func TestLookupGitHubConfig_NoCallbackURLs(t *testing.T) {
	raw, _ := json.Marshal(model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			GitHub: &model.GitHubOAuthConfig{ClientID: "x", ClientSecret: "y"},
		},
	})
	app := &model.App{Config: raw}
	_, _, err := lookupGitHubConfig(app, "")
	if err != service.ErrGitHubNotConfigured {
		t.Errorf("err = %v, want service.ErrGitHubNotConfigured", err)
	}
}

func TestLookupGitHubConfig_RedirectURIMatch(t *testing.T) {
	raw, _ := json.Marshal(model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			GitHub: &model.GitHubOAuthConfig{
				ClientID:     "x",
				ClientSecret: "y",
				CallbackURLs: []string{"https://a", "https://b", "https://c"},
			},
		},
	})
	app := &model.App{Config: raw}
	cfg, idx, err := lookupGitHubConfig(app, "https://b")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg == nil || idx != 1 {
		t.Errorf("cfg=%v idx=%d", cfg, idx)
	}
}

func TestLookupGitHubConfig_RedirectURIMismatch(t *testing.T) {
	raw, _ := json.Marshal(model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			GitHub: &model.GitHubOAuthConfig{
				ClientID:     "x",
				ClientSecret: "y",
				CallbackURLs: []string{"https://a"},
			},
		},
	})
	app := &model.App{Config: raw}
	_, _, err := lookupGitHubConfig(app, "https://other")
	if err != service.ErrCallbackURLMismatch {
		t.Errorf("err = %v, want service.ErrCallbackURLMismatch", err)
	}
}

// =========================================================================
// isAcceptableCallbackURL tests
// =========================================================================

func TestIsAcceptableCallbackURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://example.com/cb", true},
		{"https://example.com:8443/cb", true},
		{"http://127.0.0.1/cb", true},
		{"http://127.0.0.1:3000/cb", true},
		{"http://localhost/cb", true},
		{"http://localhost:8080/cb", true},
		{"http://[::1]/cb", true},
		{"http://example.com/cb", false},
		{"http://10.0.0.1/cb", false},
		{"ftp://example.com/cb", false},
		{"", false},
		{"example.com/cb", false},
	}
	for _, tc := range cases {
		if got := isAcceptableCallbackURL(tc.in); got != tc.want {
			t.Errorf("isAcceptableCallbackURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// =========================================================================
// validateGitHubOAuthConfig tests
// =========================================================================

func TestValidateGitHubOAuthConfig_HappyPath(t *testing.T) {
	g := &model.GitHubOAuthConfig{
		ClientID:     "x",
		ClientSecret: "y",
		CallbackURLs: []string{"https://example.com/cb"},
	}
	if err := validateGitHubOAuthConfig(g); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestValidateGitHubOAuthConfig_MultipleURLs(t *testing.T) {
	g := &model.GitHubOAuthConfig{
		ClientID:     "x",
		ClientSecret: "y",
		CallbackURLs: []string{
			"https://yundian.com/cb",
			"https://yundian.com/mobile/cb",
			"http://127.0.0.1:3000/cb",
		},
	}
	if err := validateGitHubOAuthConfig(g); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestValidateGitHubOAuthConfig_MissingClientID(t *testing.T) {
	g := &model.GitHubOAuthConfig{ClientSecret: "y", CallbackURLs: []string{"https://x"}}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error")
	}
}

func TestValidateGitHubOAuthConfig_MissingClientSecret(t *testing.T) {
	g := &model.GitHubOAuthConfig{ClientID: "x", CallbackURLs: []string{"https://x"}}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error")
	}
}

func TestValidateGitHubOAuthConfig_NoCallbackURLs(t *testing.T) {
	g := &model.GitHubOAuthConfig{ClientID: "x", ClientSecret: "y"}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error")
	}
}

func TestValidateGitHubOAuthConfig_EmptyURLEntry(t *testing.T) {
	g := &model.GitHubOAuthConfig{ClientID: "x", ClientSecret: "y", CallbackURLs: []string{""}}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error for empty URL entry")
	}
}

func TestValidateGitHubOAuthConfig_InsecureURL(t *testing.T) {
	g := &model.GitHubOAuthConfig{ClientID: "x", ClientSecret: "y", CallbackURLs: []string{"http://example.com/cb"}}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error for non-https / non-loopback URL")
	}
}

func TestValidateGitHubOAuthConfig_DuplicateURL(t *testing.T) {
	g := &model.GitHubOAuthConfig{
		ClientID:     "x",
		ClientSecret: "y",
		CallbackURLs: []string{"https://x/cb", "https://x/cb"},
	}
	if err := validateGitHubOAuthConfig(g); err == nil {
		t.Error("expected error for duplicate URLs")
	}
}

// =========================================================================
// validateWeChatOAuthConfig tests — mirror the GitHub set
// =========================================================================

func TestValidateWeChatOAuthConfig_HappyPath(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	if err := validateWeChatOAuthConfig(w); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestValidateWeChatOAuthConfig_MissingAppID(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for missing app_id")
	}
}

func TestValidateWeChatOAuthConfig_InvalidAppIDFormat(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "not-a-wechat-appid",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for invalid app_id format")
	}
}

func TestValidateWeChatOAuthConfig_AppIDUppercase(t *testing.T) {
	// Tencent website-app AppIDs can be issued with uppercase A-F in
	// the hex tail; the validator must accept either case so operators
	// aren't locked out by a case mismatch on a real assignment. The
	// "wx" prefix is anchored lowercase per Tencent's convention.
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789ABCDEF",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err != nil {
		t.Errorf("expected uppercase hex AppID to be accepted, got %v", err)
	}
}

func TestValidateWeChatOAuthConfig_AppIDUppercasePrefix(t *testing.T) {
	// The "wx" prefix is anchored lowercase — operators who typo it as
	// uppercase get rejected at admin time rather than discovering the
	// mismatch via a confusing errcode=40013 in production.
	w := &model.WeChatOAuthConfig{
		AppID:        "WX0123456789ABCDEF",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for uppercase 'WX' prefix")
	}
}

func TestValidateWeChatOAuthConfig_AppIDMixedCase(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789AbCdEf",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err != nil {
		t.Errorf("expected mixed-case hex AppID to be accepted, got %v", err)
	}
}

func TestValidateWeChatOAuthConfig_InvalidAppSecretLength(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "tooshort",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for invalid app_secret length")
	}
}

func TestValidateWeChatOAuthConfig_NoCallbackURLs(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:     "wx0123456789abcdef",
		AppSecret: "0123456789abcdef0123456789abcdef",
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for empty callback_urls")
	}
}

func TestValidateWeChatOAuthConfig_InsecureURL(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"http://attacker.example.com/cb"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for non-https callback URL")
	}
}

func TestValidateWeChatOAuthConfig_DuplicateURL(t *testing.T) {
	w := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://x/cb", "https://x/cb"},
	}
	if err := validateWeChatOAuthConfig(w); err == nil {
		t.Error("expected error for duplicate URLs")
	}
}

func TestValidateAppConfig_WeChatBranch(t *testing.T) {
	cfg := &model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			WeChat: &model.WeChatOAuthConfig{
				AppID:        "wx0123456789abcdef",
				AppSecret:    "0123456789abcdef0123456789abcdef",
				CallbackURLs: []string{"https://b"},
			},
		},
	}
	if err := validateAppConfig(cfg); err != nil {
		t.Errorf("err: %v", err)
	}
}

// =========================================================================
// validateAppConfig — covers the GitHub OAuth branch
// =========================================================================

func TestValidateAppConfig_GitHubBranch(t *testing.T) {
	cases := []struct {
		name string
		cfg  model.AppConfig
		ok   bool
	}{
		{
			name: "no oauth providers — ok",
			cfg:  model.AppConfig{},
			ok:   true,
		},
		{
			name: "github happy",
			cfg: model.AppConfig{
				OAuthProviders: &model.OAuthProvidersConfig{
					GitHub: &model.GitHubOAuthConfig{
						ClientID:     "x",
						ClientSecret: "y",
						CallbackURLs: []string{"https://x/cb"},
					},
				},
			},
			ok: true,
		},
		{
			name: "github bad config — error",
			cfg: model.AppConfig{
				OAuthProviders: &model.OAuthProvidersConfig{
					GitHub: &model.GitHubOAuthConfig{
						ClientID:     "",
						ClientSecret: "y",
						CallbackURLs: []string{"https://x/cb"},
					},
				},
			},
			ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAppConfig(&tc.cfg)
			if tc.ok && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected err")
			}
		})
	}
}
