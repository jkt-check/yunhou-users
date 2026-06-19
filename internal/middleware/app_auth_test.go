package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
)

type mockAppRepoForMiddleware struct {
	app  *model.App
	err  error
}

func (m *mockAppRepoForMiddleware) List(ctx context.Context) ([]model.App, error) {
	if m.app != nil {
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
		app := &model.App{AppID: "test-app", Name: "Test", IsActive: false}
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

	t.Run("valid active app", func(t *testing.T) {
		app := &model.App{AppID: "test-app", Name: "Test", IsActive: true}
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

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}
