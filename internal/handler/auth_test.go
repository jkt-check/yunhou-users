package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

// ---------- mock repo implementations ----------

type mockUserRepo struct {
	findFn  func(ctx context.Context, id string) (*model.User, error)
	createFn func(ctx context.Context, u *model.User) error
	updateFn func(ctx context.Context, u *model.User) error
}

func (m *mockUserRepo) FindByID(ctx context.Context, id string) (*model.User, error) {
	if m.findFn != nil {
		return m.findFn(ctx, id)
	}
	return nil, nil
}
func (m *mockUserRepo) Create(ctx context.Context, u *model.User) error {
	if m.createFn != nil {
		return m.createFn(ctx, u)
	}
	return nil
}
func (m *mockUserRepo) Update(ctx context.Context, u *model.User) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, u)
	}
	return nil
}

type mockSocialIdentityRepo struct {
	createFn             func(ctx context.Context, si *model.SocialIdentity) error
	findByProviderUIDFn  func(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error)
	findByEmailFn        func(ctx context.Context, email string) ([]model.SocialIdentity, error)
	listByUserIDFn       func(ctx context.Context, userID string) ([]model.SocialIdentity, error)
	deleteFn             func(ctx context.Context, id string) error
	countByUserIDFn      func(ctx context.Context, userID string) (int, error)
	deleteIfNotLastFn    func(ctx context.Context, id, userID string) (bool, error)
}

func (m *mockSocialIdentityRepo) Create(ctx context.Context, si *model.SocialIdentity) error {
	if m.createFn != nil {
		return m.createFn(ctx, si)
	}
	return nil
}
func (m *mockSocialIdentityRepo) FindByProviderUID(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
	if m.findByProviderUIDFn != nil {
		return m.findByProviderUIDFn(ctx, provider, providerUID)
	}
	return nil, nil
}
func (m *mockSocialIdentityRepo) FindByEmail(ctx context.Context, email string) ([]model.SocialIdentity, error) {
	if m.findByEmailFn != nil {
		return m.findByEmailFn(ctx, email)
	}
	return nil, nil
}
func (m *mockSocialIdentityRepo) ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
	if m.listByUserIDFn != nil {
		return m.listByUserIDFn(ctx, userID)
	}
	return nil, nil
}
func (m *mockSocialIdentityRepo) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return nil
}
func (m *mockSocialIdentityRepo) CountByUserID(ctx context.Context, userID string) (int, error) {
	if m.countByUserIDFn != nil {
		return m.countByUserIDFn(ctx, userID)
	}
	return 0, nil
}
func (m *mockSocialIdentityRepo) DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error) {
	if m.deleteIfNotLastFn != nil {
		return m.deleteIfNotLastFn(ctx, id, userID)
	}
	if m.deleteFn != nil {
		err := m.deleteFn(ctx, id)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

type mockAppRepo struct {
	createFn func(ctx context.Context, a *model.App) error
	findFn   func(ctx context.Context, id string) (*model.App, error)
	updateFn func(ctx context.Context, a *model.App) error
}

func (m *mockAppRepo) Create(ctx context.Context, a *model.App) error {
	if m.createFn != nil {
		return m.createFn(ctx, a)
	}
	return nil
}
func (m *mockAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	if m.findFn != nil {
		return m.findFn(ctx, id)
	}
	return nil, nil
}
func (m *mockAppRepo) Update(ctx context.Context, a *model.App) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, a)
	}
	return nil
}

type mockSubscriptionRepo struct {
	createFn        func(ctx context.Context, s *model.Subscription) error
	findByUserAppFn func(ctx context.Context, userID, appID string) (*model.Subscription, error)
	findFn          func(ctx context.Context, id string) (*model.Subscription, error)
	listFn          func(ctx context.Context, userID string) ([]model.Subscription, error)
	updateStatusFn  func(ctx context.Context, id, status string) error
	renewFn         func(ctx context.Context, id string, expiresAt *time.Time) error
}

func (m *mockSubscriptionRepo) Create(ctx context.Context, s *model.Subscription) error {
	if m.createFn != nil {
		return m.createFn(ctx, s)
	}
	return nil
}
func (m *mockSubscriptionRepo) FindByUserApp(ctx context.Context, userID, appID string) (*model.Subscription, error) {
	if m.findByUserAppFn != nil {
		return m.findByUserAppFn(ctx, userID, appID)
	}
	return nil, nil
}
func (m *mockSubscriptionRepo) FindByID(ctx context.Context, id string) (*model.Subscription, error) {
	if m.findFn != nil {
		return m.findFn(ctx, id)
	}
	return nil, nil
}
func (m *mockSubscriptionRepo) ListByUserID(ctx context.Context, userID string) ([]model.Subscription, error) {
	if m.listFn != nil {
		return m.listFn(ctx, userID)
	}
	return nil, nil
}
func (m *mockSubscriptionRepo) UpdateStatus(ctx context.Context, id, status string) error {
	if m.updateStatusFn != nil {
		return m.updateStatusFn(ctx, id, status)
	}
	return nil
}
func (m *mockSubscriptionRepo) Renew(ctx context.Context, id string, expiresAt *time.Time) error {
	if m.renewFn != nil {
		return m.renewFn(ctx, id, expiresAt)
	}
	return nil
}

type mockSessionRepo struct {
	createFn             func(ctx context.Context, s *model.Session) error
	findByRefreshTokenFn func(ctx context.Context, token string, sessionType string) (*model.Session, error)
	revokeFn             func(ctx context.Context, id string) error
	revokeIfNotRevokedFn func(ctx context.Context, id string) (bool, error)
	rotateRefreshFn      func(ctx context.Context, oldID string, newSession *model.Session) error
	exchangeAuthCodeFn  func(ctx context.Context, oldID string, newSession *model.Session) (bool, error)
}

func (m *mockSessionRepo) Create(ctx context.Context, s *model.Session) error {
	if m.createFn != nil {
		return m.createFn(ctx, s)
	}
	return nil
}
func (m *mockSessionRepo) FindByRefreshToken(ctx context.Context, token string, sessionType string) (*model.Session, error) {
	if m.findByRefreshTokenFn != nil {
		return m.findByRefreshTokenFn(ctx, token, sessionType)
	}
	return nil, nil
}
func (m *mockSessionRepo) Revoke(ctx context.Context, id string) error {
	if m.revokeFn != nil {
		return m.revokeFn(ctx, id)
	}
	return nil
}
func (m *mockSessionRepo) RevokeIfNotRevoked(ctx context.Context, id string) (bool, error) {
	if m.revokeIfNotRevokedFn != nil {
		return m.revokeIfNotRevokedFn(ctx, id)
	}
	return true, nil
}
func (m *mockSessionRepo) RotateRefresh(ctx context.Context, oldID string, newSession *model.Session) error {
	if m.rotateRefreshFn != nil {
		return m.rotateRefreshFn(ctx, oldID, newSession)
	}
	return nil
}

func (m *mockSessionRepo) ExchangeAuthCode(ctx context.Context, oldID string, newSession *model.Session) (bool, error) {
	if m.exchangeAuthCodeFn != nil {
		return m.exchangeAuthCodeFn(ctx, oldID, newSession)
	}
	return true, nil
}

// compile-time interface checks
var (
	_ repo.UserRepo           = (*mockUserRepo)(nil)
	_ repo.SocialIdentityRepo = (*mockSocialIdentityRepo)(nil)
	_ repo.AppRepo            = (*mockAppRepo)(nil)
	_ repo.SubscriptionRepo   = (*mockSubscriptionRepo)(nil)
	_ repo.SessionRepo        = (*mockSessionRepo)(nil)
)

// duplicateKeyError satisfies service.isDuplicateKey for test doubles.
type duplicateKeyError struct{}

func (d *duplicateKeyError) Error() string      { return "duplicate key" }
func (d *duplicateKeyError) DuplicateKey() bool { return true }

// ---------- helpers ----------

// errNotFound simulates a not-found error from repos.
var errNotFound = errors.New("not found")

// sha256Hex replicates service.hashToken for testing.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// encodeTestState creates a valid OAuth state parameter for callback tests.
func encodeTestState(appID, redirectURI, state string) string {
	h := &AuthHandler{hmacKey: "test-hmac-key"}
	s, err := h.encodeState(appID, redirectURI, state)
	if err != nil {
		panic(fmt.Sprintf("encodeTestState: %v", err))
	}
	return s
}

// testConfig returns a *config.Config pointing to test RSA keys (PKCS1 format).
func testConfig() *config.Config {
	return &config.Config{
		Port:           "8080",
		RSAPrivate:     "/tmp/test_private.pem",
		RSAPublic:      "/tmp/test_public.pem",
		JWTAccessTTL:   "15m",
		JWTRefreshTTL:  "168h",
	}
}

// newTestTokenService creates a TokenService with real RSA keys and mock repos.
func newTestTokenService(sessionRepo repo.SessionRepo, subRepo repo.SubscriptionRepo) *service.TokenService {
	ts, err := service.NewTokenService(testConfig(), sessionRepo, subRepo)
	if err != nil {
		panic(fmt.Sprintf("failed to create test TokenService: %v", err))
	}
	return ts
}

// setupAuthRouter creates a gin.Engine with auth routes registered.
func setupAuthRouter(h *AuthHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/authorize", h.Authorize)
	r.GET("/callback/:provider", h.Callback)
	r.POST("/token", h.ExchangeToken)
	r.POST("/token/refresh", h.RefreshToken)
	r.GET("/.well-known/jwks.json", h.JWKS)
	return r
}

// performRequest executes an HTTP request against a gin router.
func performRequest(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- Authorize tests ----------

func TestAuthorize(t *testing.T) {
	t.Parallel()

	oauth := service.NewOAuthProvider(testConfig(), &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: id, RedirectURIs: []string{"http://localhost/cb"}, Providers: []string{"github"}}, nil
	}})
	h := NewAuthHandler(nil, nil, oauth, "test-hmac-key")

	tests := []struct {
		name     string
		query    string
		wantCode int
		wantBody string
	}{
		{
			name:     "missing all required params",
			query:    "",
			wantCode: http.StatusBadRequest,
			wantBody: "missing required parameters",
		},
		{
			name:     "missing provider",
			query:    "app_id=app1&redirect_uri=http://localhost/cb",
			wantCode: http.StatusBadRequest,
			wantBody: "missing required parameters",
		},
		{
			name:     "missing app_id",
			query:    "provider=github&redirect_uri=http://localhost/cb",
			wantCode: http.StatusBadRequest,
			wantBody: "missing required parameters",
		},
		{
			name:     "missing redirect_uri",
			query:    "app_id=app1&provider=github",
			wantCode: http.StatusBadRequest,
			wantBody: "missing required parameters",
		},
		{
			name:     "provider not allowed for app",
			query:    "app_id=app1&provider=facebook&redirect_uri=http://localhost/cb",
			wantCode: http.StatusBadRequest,
			wantBody: "provider not allowed for this app",
		},
		{
			name:     "success redirect for github",
			query:    "app_id=app1&provider=github&redirect_uri=http://localhost/cb&state=xyz",
			wantCode: http.StatusTemporaryRedirect,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rCopy := setupAuthRouter(h) // fresh router per sub-test to avoid data races
			w := performRequest(rCopy, http.MethodGet, "/authorize?"+tt.query, "")
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body = %s", w.Code, tt.wantCode, w.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body = %s, want containing %q", w.Body.String(), tt.wantBody)
			}
			if tt.wantCode == http.StatusTemporaryRedirect {
				loc := w.Header().Get("Location")
				if loc == "" {
					t.Error("expected redirect Location header")
				}
				if !strings.Contains(loc, "github.com") {
					t.Errorf("Location = %q, want containing github.com", loc)
				}
			}
		})
	}
}

// ---------- Callback tests ----------

func TestCallback_MissingCode(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodGet, "/callback/github", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "missing authorization code") {
		t.Errorf("body = %s, want containing 'missing authorization code'", w.Body.String())
	}
}

func TestCallback_InvalidState(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")

	tests := []struct {
		name        string
		state       string
		wantBody    string
	}{
		{
			name:     "non-base64 state",
			state:    "not-encoded!!!",
			wantBody: "invalid state parameter",
		},
		{
			name:     "valid base64 but invalid JSON",
			state:    base64.RawURLEncoding.EncodeToString([]byte("not-json")),
			wantBody: "invalid state parameter",
		},
		{
			name:     "valid base64 JSON missing app_id",
			state:    base64.RawURLEncoding.EncodeToString([]byte(`{"r":"http://localhost/cb","s":"xyz"}`)),
			wantBody: "invalid state parameter",
		},
		{
			name:     "valid base64 JSON missing redirect_uri",
			state:    base64.RawURLEncoding.EncodeToString([]byte(`{"a":"app1","s":"xyz"}`)),
			wantBody: "invalid state parameter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rCopy := setupAuthRouter(h)
			w := performRequest(rCopy, http.MethodGet, "/callback/github?code=some-code&state="+tt.state, "")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body = %s, want containing %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestCallback_UnsupportedProviderError(t *testing.T) {
	t.Parallel()

	// Encode a valid state for the callback
	encodedState := encodeTestState("app1", "http://localhost/cb", "xyz")

	callbackAppRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: id, RedirectURIs: []string{"http://localhost/cb"}}, nil
	}}
	h := NewAuthHandler(nil, nil, service.NewOAuthProvider(testConfig(), callbackAppRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodGet, "/callback/facebook?code=some-code&state="+encodedState, "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to authenticate with provider") {
		t.Errorf("body = %s, want containing 'failed to authenticate with provider'", w.Body.String())
	}
}

func TestCallback_Success(t *testing.T) {
	// Set up a mock GitHub server that handles token exchange and user info
	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "mock-github-token",
				"token_type":   "bearer",
			})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":         12345,
				"login":      "testuser",
				"avatar_url": "https://example.com/avatar.png",
				"email":      "test@example.com",
			})
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"email": "test@example.com", "primary": true},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockGitHub.Close()

	// Create an HTTP client that redirects all requests to the mock server
	redirectTransport := &redirectTransport{
		url:       mockGitHub.URL,
		transport: http.DefaultTransport,
	}
	oauthClient := &http.Client{Transport: redirectTransport}

	callbackAppRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: id, RedirectURIs: []string{"http://localhost/cb"}, DefaultPlan: "free"}, nil
	}}
	// Create an OAuth provider with a custom HTTP client and mock AppRepo for redirect_uri validation
	oauth := service.NewOAuthProvider(testConfig(), callbackAppRepo)
	oauth.Client = oauthClient

	// Set up service repos for successful AuthorizeOrCreate
	sessionRepo := &mockSessionRepo{
		createFn: func(ctx context.Context, s *model.Session) error { return nil },
	}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})

	userRepo := &mockUserRepo{createFn: func(ctx context.Context, u *model.User) error { return nil }}
	identityRepo := &mockSocialIdentityRepo{
		findByProviderUIDFn: func(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
			// Return existing identity for user1
			email := "test@example.com"
			return &model.SocialIdentity{ID: "si1", UserID: "user1", Provider: "github", ProviderUID: "12345", Email: &email}, nil
		},
		createFn: func(ctx context.Context, si *model.SocialIdentity) error { return nil },
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "app1", DefaultPlan: "free"}, nil
	}}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return &model.Subscription{ID: "sub1", UserID: "user1", AppID: "app1", Status: "active"}, nil
		},
		createFn: func(ctx context.Context, s *model.Subscription) error { return nil },
	}

	authSvc := service.NewAuthService(userRepo, identityRepo, appRepo, subRepo, sessionRepo, tokenSvc)

	h := NewAuthHandler(authSvc, tokenSvc, oauth, "test-hmac-key")
	r := setupAuthRouter(h)

	// Call the callback with a valid code and properly encoded state
	encodedState := encodeTestState("app1", "http://localhost/cb", "xyz")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/callback/github?code=valid-code&state="+encodedState, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusTemporaryRedirect, w.Body.String())
	}

	loc := w.Header().Get("Location")
	if loc == "" {
		t.Error("expected redirect Location header")
	}
	if !strings.Contains(loc, "code=") {
		t.Errorf("Location = %q, want containing 'code='", loc)
	}
	if !strings.Contains(loc, "state=xyz") {
		t.Errorf("Location = %q, want containing 'state=xyz'", loc)
	}
	if !strings.Contains(loc, "http://localhost/cb") {
		t.Errorf("Location = %q, want containing 'http://localhost/cb'", loc)
	}
}

func TestCallback_AuthorizeOrCreateError(t *testing.T) {
	// Set up a mock GitHub server
	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "mock-github-token",
				"token_type":   "bearer",
			})
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":         12345,
				"login":      "testuser",
				"avatar_url": "https://example.com/avatar.png",
				"email":      "test@example.com",
			})
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"email": "test@example.com", "primary": true},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockGitHub.Close()

	mockPort := mockGitHub.Listener.Addr().String()
	mockCfg := &config.Config{
		Port:               mockPort,
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		RSAPrivate:         "/tmp/test_private.pem",
		RSAPublic:          "/tmp/test_public.pem",
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
	}

	callbackAppRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: id, RedirectURIs: []string{"http://localhost/cb"}, DefaultPlan: "free"}, nil
	}}
	oauth := service.NewOAuthProvider(mockCfg, callbackAppRepo)
	oauth.Client = &http.Client{Transport: &redirectTransport{
		url:       mockGitHub.URL,
		transport: http.DefaultTransport,
	}}

	sessionRepo := &mockSessionRepo{createFn: func(ctx context.Context, s *model.Session) error { return nil }}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})

	// App not found → AuthorizeOrCreate will fail
	authSvc := service.NewAuthService(
		&mockUserRepo{createFn: func(ctx context.Context, u *model.User) error { return nil }},
		&mockSocialIdentityRepo{
			findByProviderUIDFn: func(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
				return nil, errNotFound
			},
			findByEmailFn: func(ctx context.Context, email string) ([]model.SocialIdentity, error) {
				return nil, nil
			},
			createFn: func(ctx context.Context, si *model.SocialIdentity) error { return nil },
		},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return nil, errNotFound
		}},
		&mockSubscriptionRepo{},
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, oauth, "test-hmac-key")
	r := setupAuthRouter(h)

	encodedState := encodeTestState("app1", "http://localhost/cb", "xyz")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/callback/github?code=valid-code&state="+encodedState, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

// ---------- ExchangeToken tests ----------

func TestExchangeToken_InvalidBody(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})
	authSvc := service.NewAuthService(&mockUserRepo{}, &mockSocialIdentityRepo{}, &mockAppRepo{}, &mockSubscriptionRepo{}, sessionRepo, tokenSvc)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	// Empty body
	w := performRequest(r, http.MethodPost, "/token", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body = %s, want containing 'invalid request body'", w.Body.String())
	}

	// Invalid JSON
	w = performRequest(r, http.MethodPost, "/token", "not-json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// Missing required fields
	w = performRequest(r, http.MethodPost, "/token", `{"code":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestExchangeToken_InvalidAppID(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})
	authSvc := service.NewAuthService(
		&mockUserRepo{},
		&mockSocialIdentityRepo{},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return nil, errNotFound
		}},
		&mockSubscriptionRepo{},
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	body := `{"code":"some-code","app_id":"nonexistent","app_secret":"secret"}`
	w := performRequest(r, http.MethodPost, "/token", body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid credentials or authorization code") {
		t.Errorf("body = %s, want containing 'invalid credentials or authorization code'", w.Body.String())
	}
}

func TestExchangeToken_InvalidAppSecret(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})

	// Hash "correct-secret" for the stored app
	storedHash, err := util.HashSecret("correct-secret")
	if err != nil {
		t.Fatalf("failed to hash test secret: %v", err)
	}

	authSvc := service.NewAuthService(
		&mockUserRepo{},
		&mockSocialIdentityRepo{},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: "app1", Secret: storedHash}, nil
		}},
		&mockSubscriptionRepo{},
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	body := `{"code":"some-code","app_id":"app1","app_secret":"wrong-secret"}`
	w := performRequest(r, http.MethodPost, "/token", body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid credentials or authorization code") {
		t.Errorf("body = %s, want containing 'invalid credentials or authorization code'", w.Body.String())
	}
}

func TestExchangeToken_InvalidAuthCode(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return nil, errNotFound
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})

	// Hash "correct-secret" for the stored app
	storedHash, err := util.HashSecret("correct-secret")
	if err != nil {
		t.Fatalf("failed to hash test secret: %v", err)
	}

	authSvc := service.NewAuthService(
		&mockUserRepo{},
		&mockSocialIdentityRepo{},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: "app1", Secret: storedHash}, nil
		}},
		&mockSubscriptionRepo{},
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	body := `{"code":"bad-auth-code","app_id":"app1","app_secret":"correct-secret"}`
	w := performRequest(r, http.MethodPost, "/token", body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid credentials or authorization code") {
		t.Errorf("body = %s, want containing 'invalid credentials or authorization code'", w.Body.String())
	}
}

func TestExchangeToken_CodeIssuedForDifferentApp(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{
				ID:     "sess1",
				UserID: "user1",
				AppID:  "app2", // Code was for app2
				Scope:  []string{"app:read"},
				SessionType:  "auth_code",
			}, nil
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})

	storedHash, err := util.HashSecret("correct-secret")
	if err != nil {
		t.Fatalf("failed to hash test secret: %v", err)
	}

	authSvc := service.NewAuthService(
		&mockUserRepo{},
		&mockSocialIdentityRepo{},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: "app1", Secret: storedHash}, nil
		}},
		&mockSubscriptionRepo{},
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	body := `{"code":"some-code","app_id":"app1","app_secret":"correct-secret"}`
	w := performRequest(r, http.MethodPost, "/token", body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid credentials or authorization code") {
		t.Errorf("body = %s, want containing 'invalid credentials or authorization code'", w.Body.String())
	}
}

func TestExchangeToken_Success(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{
				ID:     "sess1",
				UserID: "user1",
				AppID:  "app1",
				Scope:  []string{"app:read"},
				SessionType:  "auth_code",
			}, nil
		},
		createFn: func(ctx context.Context, s *model.Session) error { return nil },
		revokeFn: func(ctx context.Context, id string) error { return nil },
	}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return &model.Subscription{ID: "sub1", UserID: userID, AppID: appID, Status: "active"}, nil
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, subRepo)

	// Create a bcrypt hash for "correct-secret"
	storedHash, err := util.HashSecret("correct-secret")
	if err != nil {
		t.Fatalf("HashSecret failed: %v", err)
	}

	authSvc := service.NewAuthService(
		&mockUserRepo{},
		&mockSocialIdentityRepo{},
		&mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: "app1", Secret: storedHash}, nil
		}},
		subRepo,
		sessionRepo,
		tokenSvc,
	)

	h := NewAuthHandler(authSvc, tokenSvc, service.NewOAuthProvider(testConfig(), nil), "test-hmac-key")
	r := setupAuthRouter(h)

	// Get a valid auth code by using AuthorizeOrCreate
	authCode, err := authSvc.AuthorizeOrCreate(context.Background(), service.ProviderUserInfo{
		Provider:    "github",
		ProviderUID: "12345",
		Email:       "test@example.com",
		Nickname:    "testuser",
		AvatarURL:   "https://example.com/avatar.png",
	}, "app1")
	if err != nil {
		t.Fatalf("AuthorizeOrCreate failed: %v", err)
	}

	// Set up findByRefreshTokenFn to return the session for the hashed auth code
	storedHash2 := sha256Hex(authCode)
	sessionRepo.findByRefreshTokenFn = func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
		if token == storedHash2 {
			return &model.Session{ID: "sess-auth", UserID: "user1", AppID: "app1", SessionType: "auth_code", Scope: []string{"app:read"}}, nil
		}
		return nil, errNotFound
	}

	body := fmt.Sprintf(`{"code":"%s","app_id":"app1","app_secret":"correct-secret"}`, authCode)
	w := performRequest(r, http.MethodPost, "/token", body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Error("expected access_token in response")
	}
	if data["refresh_token"] == nil || data["refresh_token"] == "" {
		t.Error("expected refresh_token in response")
	}
	if data["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", data["token_type"])
	}
}

// ---------- RefreshToken tests ----------

func TestRefreshToken_InvalidBody(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})
	h := NewAuthHandler(nil, tokenSvc, nil, "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body = %s, want containing 'invalid request body'", w.Body.String())
	}

	// Missing required fields
	w = performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestRefreshToken_InvalidRefreshToken(t *testing.T) {
	t.Parallel()

	storedHash, err := util.HashSecret("app-secret")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "a1", Secret: storedHash}, nil
	}}
	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return nil, errNotFound
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, &mockSubscriptionRepo{})
	h := NewAuthHandler(nil, tokenSvc, service.NewOAuthProvider(testConfig(), appRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"bad-token","app_id":"a1","app_secret":"app-secret"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid or expired refresh token") {
		t.Errorf("body = %s, want containing 'invalid or expired refresh token'", w.Body.String())
	}
}

func TestRefreshToken_InactiveSubscription(t *testing.T) {
	t.Parallel()

	storedHash, err := util.HashSecret("app-secret")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "a1", Secret: storedHash}, nil
	}}
	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{ID: "s1", UserID: "u1", AppID: "a1", SessionType: "refresh", Scope: []string{"app:read"}}, nil
		},
	}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return &model.Subscription{ID: "sub1", UserID: "u1", AppID: "a1", Status: "cancelled"}, nil
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, subRepo)
	h := NewAuthHandler(nil, tokenSvc, service.NewOAuthProvider(testConfig(), appRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"valid-token","app_id":"a1","app_secret":"app-secret"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid or expired refresh token") {
		t.Errorf("body = %s, want containing 'invalid or expired refresh token'", w.Body.String())
	}
}

func TestRefreshToken_SubscriptionNotFound(t *testing.T) {
	t.Parallel()

	storedHash, err := util.HashSecret("app-secret")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "a1", Secret: storedHash}, nil
	}}
	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{ID: "s1", UserID: "u1", AppID: "a1", SessionType: "refresh", Scope: []string{"app:read"}}, nil
		},
	}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return nil, errNotFound
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, subRepo)
	h := NewAuthHandler(nil, tokenSvc, service.NewOAuthProvider(testConfig(), appRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"valid-token","app_id":"a1","app_secret":"app-secret"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestRefreshToken_SessionCreateError(t *testing.T) {
	storedHash, err := util.HashSecret("app-secret")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "a1", Secret: storedHash}, nil
	}}
	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{ID: "s1", UserID: "u1", AppID: "a1", SessionType: "refresh", Scope: []string{"app:read"}}, nil
		},
		rotateRefreshFn: func(ctx context.Context, oldID string, newSession *model.Session) error {
			return errors.New("db error")
		},
	}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return &model.Subscription{ID: "sub1", UserID: "u1", AppID: "a1", Status: "active"}, nil
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, subRepo)
	h := NewAuthHandler(nil, tokenSvc, service.NewOAuthProvider(testConfig(), appRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"valid-token","app_id":"a1","app_secret":"app-secret"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestRefreshToken_Success(t *testing.T) {
	storedHash, err := util.HashSecret("app-secret")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	appRepo := &mockAppRepo{findFn: func(ctx context.Context, id string) (*model.App, error) {
		return &model.App{ID: "app1", Secret: storedHash}, nil
	}}
	sessionRepo := &mockSessionRepo{
		findByRefreshTokenFn: func(ctx context.Context, token string, sessionType string) (*model.Session, error) {
			return &model.Session{
				ID:     "sess1",
				UserID: "user1",
				AppID:  "app1",
				Scope:  []string{"app:read"},
				SessionType:  "refresh",
			}, nil
		},
		createFn: func(ctx context.Context, s *model.Session) error {
			return nil
		},
		revokeFn: func(ctx context.Context, id string) error {
			return nil
		},
	}
	subRepo := &mockSubscriptionRepo{
		findByUserAppFn: func(ctx context.Context, userID, appID string) (*model.Subscription, error) {
			return &model.Subscription{ID: "sub1", UserID: "user1", AppID: "app1", Status: "active"}, nil
		},
	}
	tokenSvc := newTestTokenService(sessionRepo, subRepo)
	h := NewAuthHandler(nil, tokenSvc, service.NewOAuthProvider(testConfig(), appRepo), "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodPost, "/token/refresh", `{"refresh_token":"valid-refresh-token","app_id":"app1","app_secret":"app-secret"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["access_token"] == nil || data["access_token"] == "" {
		t.Error("expected access_token in response")
	}
	if data["refresh_token"] == nil || data["refresh_token"] == "" {
		t.Error("expected refresh_token in response")
	}
	if data["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", data["token_type"])
	}
}

// ---------- JWKS tests ----------

func TestJWKS(t *testing.T) {
	tokenSvc := newTestTokenService(&mockSessionRepo{}, &mockSubscriptionRepo{})
	h := NewAuthHandler(nil, tokenSvc, nil, "test-hmac-key")
	r := setupAuthRouter(h)

	w := performRequest(r, http.MethodGet, "/.well-known/jwks.json", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	keys, ok := resp["keys"].([]interface{})
	if !ok || len(keys) == 0 {
		t.Fatal("response missing keys array")
	}
	jwk := keys[0].(map[string]interface{})
	if jwk["kty"] != "RSA" {
		t.Errorf("kty = %v, want RSA", jwk["kty"])
	}
	if jwk["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", jwk["alg"])
	}
	if jwk["use"] != "sig" {
		t.Errorf("use = %v, want sig", jwk["use"])
	}
	if jwk["kid"] != "yunhou-users-rsa" {
		t.Errorf("kid = %v, want yunhou-users-rsa", jwk["kid"])
	}
	if jwk["n"] == nil || jwk["n"] == "" {
		t.Error("expected non-empty 'n' in JWK")
	}
	if jwk["e"] != "AQAB" {
		t.Errorf("e = %v, want AQAB", jwk["e"])
	}
}

// ---------- unused import guards ----------

var _ = middleware.ContextUserID
var _ = util.CheckSecret

// ---------- redirectTransport redirects all HTTP requests to a test server ----------

type redirectTransport struct {
	url       string
	transport http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace the scheme+host with the test server URL, keep the path and query
	target := t.url + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return t.transport.RoundTrip(newReq)
}
