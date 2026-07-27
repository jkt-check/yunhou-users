package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/yunhou/users/internal/service"
)

// jwtTestKeys matches the production TokenService keypair shape (RS256). We
// generate fresh pairs per test so two tests can never share state.
func jwtTestKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return priv, &priv.PublicKey
}

// jwtTestSign signs a token with the same claims shape service.TokenService
// uses, but without depending on the service package's exported helpers.
func jwtTestSign(t *testing.T, priv *rsa.PrivateKey, userID, appID string, scope []string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":    userID,
		"iss":    "yunhou-users",
		"aud":    appID,
		"app_id": appID,
		"scope":  scope,
		"iat":    now.Unix(),
		"exp":    now.Add(15 * time.Minute).Unix(),
	})
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// jwtTestEngine mirrors middleware/webhook_sig_test.go's newTestEngine helper.
func jwtTestEngine(tokenSvc *service.TokenService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(JWTAuth(tokenSvc))
	engine.GET("/protected", func(c *gin.Context) {
		uid := c.GetString(ContextUserID)
		app := c.GetString(ContextAppID)
		scope, _ := c.Get(ContextScope)
		c.JSON(http.StatusOK, gin.H{
			"user_id": uid,
			"app_id":  app,
			"scope":   scope,
		})
	})
	return engine
}

func TestJWTAuth(t *testing.T) {
	t.Parallel()

	priv, pub := jwtTestKeys(t)
	makeSvc := func() *service.TokenService {
		return &service.TokenService{
			PrivateKey: priv,
			PublicKey:  pub,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 168 * time.Hour,
		}
	}

	t.Run("missing Authorization header → 401", func(t *testing.T) {
		t.Parallel()
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "missing or invalid authorization header") {
			t.Errorf("expected missing/invalid header message, got %s", rec.Body.String())
		}
	})

	t.Run("non-Bearer scheme → 401", func(t *testing.T) {
		t.Parallel()
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Token abc.def.ghi")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", rec.Code)
		}
	})

	t.Run("Bearer prefix only (no token) → 401", func(t *testing.T) {
		t.Parallel()
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", rec.Code)
		}
	})

	t.Run("garbage token → 401", func(t *testing.T) {
		t.Parallel()
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Bearer not.a.real.token")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "invalid or expired token") {
			t.Errorf("expected invalid-token message, got %s", rec.Body.String())
		}
	})

	t.Run("valid token populates context and reaches handler", func(t *testing.T) {
		t.Parallel()
		tok := jwtTestSign(t, priv, "user-1", "yundian", []string{"yundian", "yundash"})
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, needle := range []string{`"user_id":"user-1"`, `"app_id":"yundian"`} {
			if !strings.Contains(body, needle) {
				t.Errorf("body missing %s: %s", needle, body)
			}
		}
	})

	t.Run("expired token → 401", func(t *testing.T) {
		t.Parallel()
		// Build an expired token using the production helper so the
		// service's issuer/audience checks are exercised, then expect 401.
		expired := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"sub":    "user-x",
			"iss":    "yunhou-users",
			"aud":    "yundian",
			"app_id": "yundian",
			"scope":  []string{"yundian"},
			"iat":    time.Now().Add(-1 * time.Hour).Unix(),
			"exp":    time.Now().Add(-1 * time.Minute).Unix(),
		})
		signed, err := expired.SignedString(priv)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		engine := jwtTestEngine(makeSvc())
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Bearer "+signed)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expired token should 401, got %d (body: %s)", rec.Code, rec.Body.String())
		}
	})
}
