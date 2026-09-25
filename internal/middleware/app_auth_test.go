package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

type mockAppRepoForMiddleware struct {
	app *model.App
	err error
}

func (m *mockAppRepoForMiddleware) List(ctx context.Context) ([]model.App, error) {
	if m.app != nil {
		return []model.App{*m.app}, nil
	}
	return []model.App{}, nil
}

func (m *mockAppRepoForMiddleware) ListUnhashed(ctx context.Context) ([]model.App, error) {
	if m.app != nil && m.app.SecretHash == "" {
		return []model.App{*m.app}, nil
	}
	return []model.App{}, nil
}

func (m *mockAppRepoForMiddleware) FindByID(ctx context.Context, id string) (*model.App, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.app, nil
}

func (m *mockAppRepoForMiddleware) Create(ctx context.Context, a *model.App) error {
	return nil
}

func (m *mockAppRepoForMiddleware) Update(ctx context.Context, a *model.App) error {
	return nil
}

func (m *mockAppRepoForMiddleware) RotateSecretHash(ctx context.Context, appID, newHash string) error {
	if m.app != nil {
		m.app.SecretHash = newHash
	}
	return nil
}

func (m *mockAppRepoForMiddleware) BackfillSecretHash(ctx context.Context, appID, newHash string) (bool, error) {
	if m.app != nil && m.app.SecretHash != "" {
		return true, nil
	}
	if m.app != nil {
		m.app.SecretHash = newHash
	}
	return false, nil
}

// hashedApp builds a mock app whose SecretHash matches the given plaintext.
// Returns nil hash when plaintext is empty so tests can drive the "secret not
// initialised" branch.
func hashedApp(appID, plaintext string) *model.App {
	a := &model.App{AppID: appID, Name: appID, IsActive: true}
	if plaintext != "" {
		h, err := util.HashSecret(plaintext)
		if err != nil {
			panic(err)
		}
		a.SecretHash = h
	}
	return a
}

func TestInternalAppAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("missing X-App-ID header", func(t *testing.T) {
		appRepo := &mockAppRepoForMiddleware{}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("invalid app_id", func(t *testing.T) {
		appRepo := &mockAppRepoForMiddleware{err: errors.New("not found")}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "invalid-app")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("app is inactive", func(t *testing.T) {
		app := &model.App{AppID: "test-app", Name: "Test", IsActive: false, SecretHash: "unused"}
		appRepo := &mockAppRepoForMiddleware{app: app}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "test-app")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// Info-leak prevention: a disabled app and an invalid secret
		// both surface as 401 "invalid app_secret" so an attacker can't
		// enumerate which X-App-ID values exist or are active. The
		// operator-visible reason goes to the log.
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d (info-leak prevention: disabled app surfaces same code as invalid secret)", w.Code)
		}
	})

	t.Run("valid active app with X-App-ID only", func(t *testing.T) {
		// pre-migration state — secret_hash empty because the app row predates
		// 007_app_secret backfill. Refuse rather than fall through to the
		// network-trust model: that is the exact gap X-App-Secret closes.
		app := hashedApp("test-app", "")
		appRepo := &mockAppRepoForMiddleware{app: app}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "test-app")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 (secret not initialised), got %d", w.Code)
		}
	})

	t.Run("valid active app missing X-App-Secret header", func(t *testing.T) {
		app := hashedApp("test-app", "correct-secret")
		appRepo := &mockAppRepoForMiddleware{app: app}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "test-app")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 (missing X-App-Secret), got %d", w.Code)
		}
	})

	t.Run("valid active app wrong X-App-Secret", func(t *testing.T) {
		app := hashedApp("test-app", "correct-secret")
		appRepo := &mockAppRepoForMiddleware{app: app}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "test-app")
		req.Header.Set("X-App-Secret", "wrong-secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 (invalid app_secret), got %d", w.Code)
		}
	})

	t.Run("valid active app correct X-App-Secret", func(t *testing.T) {
		app := hashedApp("test-app", "correct-secret")
		appRepo := &mockAppRepoForMiddleware{app: app}
		handler := InternalAppAuth(appRepo)

		router := gin.New()
		router.Use(handler)
		router.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-App-ID", "test-app")
		req.Header.Set("X-App-Secret", "correct-secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

// TestInternalAppAuth_MissingAppBurnsDummyCompare (M-8): the app-not-found
// path must run a bcrypt comparison against the dummy hash before the 401 —
// otherwise response time alone reveals whether an X-App-ID exists. Timing
// assertions are flaky, so this spies on the package-level checkSecret seam
// and verifies structurally: exactly one comparison, against
// util.DummyBcryptHash, and the response is still 401.
func TestInternalAppAuth_MissingAppBurnsDummyCompare(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type call struct{ hash, plain string }
	calls := []call{}
	orig := checkSecret
	checkSecret = func(hashed, plain string) bool {
		calls = append(calls, call{hashed, plain})
		return false
	}
	defer func() { checkSecret = orig }()

	appRepo := &mockAppRepoForMiddleware{err: errors.New("not found")}
	router := gin.New()
	router.Use(InternalAppAuth(appRepo))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-App-ID", "ghost-app")
	req.Header.Set("X-App-Secret", "guess")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if len(calls) != 1 {
		t.Fatalf("compare calls = %d, want exactly 1 dummy compare on missing app", len(calls))
	}
	if calls[0].hash != util.DummyBcryptHash {
		t.Fatalf("missing-app path compared hash %q, want the dummy hash (timing oracle open)", calls[0].hash)
	}
	if calls[0].plain != "guess" {
		t.Fatalf("dummy compare plain = %q, want the caller's secret (timing profile must match a real mismatch)", calls[0].plain)
	}
}

// TestInternalAppAuth_DisabledAppBurnsDummyCompare: the disabled-app branch
// is the same timing-oracle class as the missing-app branch (exists-but-
// disabled enumerable via response time) — it must burn the dummy compare
// too.
func TestInternalAppAuth_DisabledAppBurnsDummyCompare(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var burned bool
	orig := checkSecret
	checkSecret = func(hashed, plain string) bool {
		if hashed == util.DummyBcryptHash {
			burned = true
		}
		return false
	}
	defer func() { checkSecret = orig }()

	app := &model.App{AppID: "test-app", Name: "Test", IsActive: false, SecretHash: "unused"}
	router := gin.New()
	router.Use(InternalAppAuth(&mockAppRepoForMiddleware{app: app}))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-App-ID", "test-app")
	req.Header.Set("X-App-Secret", "guess")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if !burned {
		t.Fatal("disabled-app path did not run the dummy compare (timing oracle open)")
	}
}
