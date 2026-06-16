package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

// ---------- mock repos ----------

type mockSessionRepo struct{}

func (m *mockSessionRepo) Create(_ context.Context, _ *model.Session) error { return nil }
func (m *mockSessionRepo) FindByRefreshToken(_ context.Context, _ string) (*model.Session, error) {
	return nil, nil
}
func (m *mockSessionRepo) Revoke(_ context.Context, _ string) error { return nil }
func (m *mockSessionRepo) RevokeIfNotRevoked(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (m *mockSessionRepo) RotateRefresh(_ context.Context, _ string, _ *model.Session) error {
	return nil
}

type mockSubRepo struct{}

func (m *mockSubRepo) Create(_ context.Context, _ *model.Subscription) error              { return nil }
func (m *mockSubRepo) FindByUserApp(_ context.Context, _, _ string) (*model.Subscription, error) {
	return nil, nil
}
func (m *mockSubRepo) FindByID(_ context.Context, _ string) (*model.Subscription, error)  { return nil, nil }
func (m *mockSubRepo) ListByUserID(_ context.Context, _ string) ([]model.Subscription, error) {
	return nil, nil
}
func (m *mockSubRepo) UpdateStatus(_ context.Context, _, _ string) error                   { return nil }
func (m *mockSubRepo) Renew(_ context.Context, _ string, _ *time.Time) error              { return nil }

// compile-time interface check
var (
	_ repo.SessionRepo     = (*mockSessionRepo)(nil)
	_ repo.SubscriptionRepo = (*mockSubRepo)(nil)
)

// ---------- helpers ----------

func newTokenService(t *testing.T) *service.TokenService {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return &service.TokenService{
		PrivateKey:  key,
		PublicKey:   &key.PublicKey,
		AccessTTL:   "15m",
		RefreshTTL:  "168h",
		SessionRepo: &mockSessionRepo{},
		SubRepo:     &mockSubRepo{},
	}
}

func signToken(t *testing.T, svc *service.TokenService, userID, appID string, scope []string) string {
	t.Helper()
	tok, err := svc.SignAccessToken(userID, appID, scope)
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	return tok
}

func signTokenWithWrongKey(t *testing.T, userID, appID string, scope []string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate wrong RSA key: %v", err)
	}
	claims := service.TokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    "yunhou-users",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		AppID: appID,
		Scope: scope,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign with wrong key: %v", err)
	}
	return s
}

func signExpiredToken(t *testing.T, svc *service.TokenService, userID, appID string, scope []string) string {
	t.Helper()
	claims := service.TokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    "yunhou-users",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
		AppID: appID,
		Scope: scope,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(svc.PrivateKey)
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	return s
}

// ---------- tests ----------

func TestJWTAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokenSvc := newTokenService(t)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
		wantCode   float64
		wantMsg    string
	}{
		{
			name:       "missing authorization header",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing or invalid authorization header",
		},
		{
			name:       "invalid bearer prefix - Basic",
			authHeader: "Basic dXNlcjpwYXNz",
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing or invalid authorization header",
		},
		{
			name:       "invalid bearer prefix - just token no Bearer",
			authHeader: "sometoken",
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing or invalid authorization header",
		},
		{
			name:       "token signed with wrong key",
			authHeader: "Bearer " + signTokenWithWrongKey(t, "user-1", "app-1", []string{"read"}),
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "invalid or expired token",
		},
		{
			name:       "expired token",
			authHeader: "Bearer " + signExpiredToken(t, tokenSvc, "user-1", "app-1", []string{"read"}),
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "invalid or expired token",
		},
		{
			name:       "malformed JWT string",
			authHeader: "Bearer not-a-jwt",
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "invalid or expired token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := gin.New()
			r.Use(JWTAuth(tokenSvc))
			r.GET("/", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}

			var body map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if body["code"] != tt.wantCode {
				t.Errorf("code: got %v, want %v", body["code"], tt.wantCode)
			}
			if body["message"] != tt.wantMsg {
				t.Errorf("message: got %v, want %v", body["message"], tt.wantMsg)
			}
		})
	}
}

func TestJWTAuth_ValidTokenSetsContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()

	tokenSvc := newTokenService(t)
	token := signToken(t, tokenSvc, "user-99", "app-3", []string{"admin"})

	var gotUserID, gotAppID string
	var gotScope []string

	r := gin.New()
	r.Use(JWTAuth(tokenSvc))
	r.GET("/", func(c *gin.Context) {
		gotUserID = c.GetString(ContextUserID)
		gotAppID = c.GetString(ContextAppID)
		if v, ok := c.Get(ContextScope); ok {
			gotScope, _ = v.([]string)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusOK)
	}

	if gotUserID != "user-99" {
		t.Errorf("user_id: got %q, want %q", gotUserID, "user-99")
	}
	if gotAppID != "app-3" {
		t.Errorf("app_id: got %q, want %q", gotAppID, "app-3")
	}
	if len(gotScope) != 1 || gotScope[0] != "admin" {
		t.Errorf("scope: got %v, want []string{\"admin\"}", gotScope)
	}
}
