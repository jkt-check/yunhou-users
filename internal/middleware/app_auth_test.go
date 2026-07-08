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

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d", w.Code)
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