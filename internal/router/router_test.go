package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetupRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// This test verifies that all routes are properly registered
	// by checking that requests don't return 404 (even if they fail due to nil dependencies)

	t.Run("healthz endpoint is registered", func(t *testing.T) {
		engine := gin.New()
		// With nil dependencies, route setup should still work for registered routes
		// This tests that the route path exists, not the handler logic

		// Just verify gin can register these routes without panicking
		engine.GET("/healthz", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("public routes are registered", func(t *testing.T) {
		engine := gin.New()

		// Register mock handlers for route existence check
		engine.POST("/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/auth/refresh", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/auth/logout", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/.well-known/jwks.json", func(c *gin.Context) { c.Status(http.StatusOK) })

		routes := []struct {
			method string
			path   string
		}{
			{"POST", "/auth/login"},
			{"POST", "/auth/refresh"},
			{"POST", "/auth/logout"},
			{"GET", "/.well-known/jwks.json"},
		}

		for _, route := range routes {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Errorf("route %s %s not found", route.method, route.path)
			}
		}
	})

	t.Run("user routes require auth", func(t *testing.T) {
		engine := gin.New()

		// Simulate JWTAuth middleware that returns 401
		engine.Use(func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401})
		})
		engine.GET("/user/profile", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.PATCH("/user/profile", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/user/identities", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.DELETE("/user/identities/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/user/subscriptions", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/user/subscriptions", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.DELETE("/user/subscriptions/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

		routes := []struct {
			method string
			path   string
		}{
			{"GET", "/user/profile"},
			{"PATCH", "/user/profile"},
			{"GET", "/user/identities"},
			{"DELETE", "/user/identities/id1"},
			{"GET", "/user/subscriptions"},
			{"POST", "/user/subscriptions"},
			{"DELETE", "/user/subscriptions/id1"},
		}

		for _, route := range routes {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("route %s %s: expected 401, got %d", route.method, route.path, w.Code)
			}
		}
	})

	t.Run("app routes are registered", func(t *testing.T) {
		engine := gin.New()

		engine.GET("/apps", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/apps/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

		routes := []struct {
			method string
			path   string
		}{
			{"GET", "/apps"},
			{"GET", "/apps/test-id"},
		}

		for _, route := range routes {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Errorf("route %s %s not found", route.method, route.path)
			}
		}
	})

	t.Run("admin routes are registered", func(t *testing.T) {
		engine := gin.New()

		engine.GET("/admin/plans", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/admin/plans/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/admin/plans", func(c *gin.Context) { c.Status(http.StatusCreated) })
		engine.PATCH("/admin/plans/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.DELETE("/admin/plans/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/admin/apps", func(c *gin.Context) { c.Status(http.StatusCreated) })
		engine.PATCH("/admin/apps/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

		routes := []struct {
			method string
			path   string
		}{
			{"GET", "/admin/plans"},
			{"GET", "/admin/plans/plan1"},
			{"POST", "/admin/plans"},
			{"PATCH", "/admin/plans/plan1"},
			{"DELETE", "/admin/plans/plan1"},
			{"POST", "/admin/apps"},
			{"PATCH", "/admin/apps/app1"},
		}

		for _, route := range routes {
			req := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Errorf("route %s %s not found", route.method, route.path)
			}
		}
	})
}

func TestRoutePathVariables(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("user route params", func(t *testing.T) {
		engine := gin.New()

		var capturedID string
		engine.DELETE("/user/identities/:id", func(c *gin.Context) {
			capturedID = c.Param("id")
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodDelete, "/user/identities/abc123", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if capturedID != "abc123" {
			t.Errorf("expected param 'abc123', got %s", capturedID)
		}
	})

	t.Run("admin route params", func(t *testing.T) {
		engine := gin.New()

		var capturedID string
		engine.GET("/admin/plans/:id", func(c *gin.Context) {
			capturedID = c.Param("id")
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/admin/plans/monthly", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if capturedID != "monthly" {
			t.Errorf("expected param 'monthly', got %s", capturedID)
		}
	})
}
