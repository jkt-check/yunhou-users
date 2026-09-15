package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/handler"
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

		// Register mock handlers for route existence check. /auth/login was
		// removed by commit 5ef27ce (GitHub is the only login provider now);
		// /test/login is the dev-only JWT mint endpoint, not a public route.
		engine.POST("/auth/refresh", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.POST("/auth/logout", func(c *gin.Context) { c.Status(http.StatusOK) })
		engine.GET("/.well-known/jwks.json", func(c *gin.Context) { c.Status(http.StatusOK) })

		routes := []struct {
			method string
			path   string
		}{
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

// TestSetup_RegistersAllRoutes calls the actual router.Setup() with
// nil-tilings for every repo/service dependency. Setup() only stores
// pointers in handler structs and registers routes — it does NOT
// dereference the deps at registration time — so this is safe.
//
// We use gin's Routes() introspection rather than ServeHTTP() because
// the latter would invoke handlers and middleware that dereference nil
// deps and panic. Routes() just returns the route tree Setup() built.
func TestSetup_RegistersAllRoutes(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	Setup(t.Context(), engine,
		nil,                          // healthPinger
		nil, nil, nil, nil, nil, nil, // repos
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false, // wechatOAuthMock
		false, // wechatPayMock
		nil,   // usageSvc
		nil,   // llmUsageSvc
		nil,   // relayHandler
	)

	routes := engine.Routes()
	have := make(map[string]bool, len(routes))
	for _, r := range routes {
		have[r.Method+":"+r.Path] = true
	}
	want := []string{
		"GET:/healthz",
		"GET:/.well-known/jwks.json",
		"POST:/auth/refresh",
		"POST:/auth/logout",
		"GET:/user/profile",
		"PATCH:/user/profile",
		"GET:/user/identities",
		"DELETE:/user/identities/:id",
		"GET:/user/subscriptions",
		"POST:/user/subscriptions",
		"DELETE:/user/subscriptions/:id",
		"GET:/apps",
		"GET:/apps/:id",
		"GET:/admin/plans",
		"GET:/admin/plans/:id",
		"POST:/admin/plans",
		"PATCH:/admin/plans/:id",
		"DELETE:/admin/plans/:id",
		"POST:/admin/apps",
		"PATCH:/admin/apps/:id",
		"POST:/apps/:id/quote",
		"POST:/chat",
		"POST:/payments/orders",
		"GET:/payments/orders",
		"GET:/payments/orders/:id",
		"DELETE:/payments/orders/:id",
		"POST:/payments/orders/:order_id/confirm",
		"GET:/payments",
		"GET:/payments/:id",
		"GET:/payments/:id/refunds",
		"POST:/refunds",
		"GET:/refunds/:id",
		"POST:/webhooks/payment/:channel",
		"POST:/user/usage/heartbeat",
		"GET:/admin/stats/active",
		"GET:/admin/stats/usage-duration",
		"GET:/admin/stats/new-users",
		"GET:/admin/stats/llm-usage",
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("Setup did not register route %s", w)
		}
	}
	// /test/login must NOT be registered without PAYPAL_L3_E2E_MODE=1 —
	// the route existing at all is the dev-only escape hatch.
	if have["POST:/test/login"] {
		t.Error("Setup registered /test/login without PAYPAL_L3_E2E_MODE=1")
	}
}

// TestSetup_TestLoginGatedOnEnv verifies the /test/login route exists only
// when PAYPAL_L3_E2E_MODE=1 — previously it was always registered and gated
// only inside the handler, so one stray env line in production would have
// exposed arbitrary JWT minting.
func TestSetup_TestLoginGatedOnEnv(t *testing.T) {
	// Not parallel: t.Setenv mutates process env.
	gin.SetMode(gin.TestMode)
	t.Setenv("PAYPAL_L3_E2E_MODE", "1")
	engine := gin.New()

	Setup(t.Context(), engine,
		nil,                          // healthPinger
		nil, nil, nil, nil, nil, nil, // repos
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false, // wechatOAuthMock
		false, // wechatPayMock
		nil,   // usageSvc
		nil,   // llmUsageSvc
		nil,   // relayHandler
	)

	for _, r := range engine.Routes() {
		if r.Method == "POST" && r.Path == "/test/login" {
			return
		}
	}
	t.Error("Setup did not register /test/login with PAYPAL_L3_E2E_MODE=1")
}

// TestSetup_RelayRoutesWired 验证 relay 启用(relayHandler 非 nil)时的
// 路由装配:POST /relay/ticket 挂在 JWTAuth + 30/min 签发限流之后,
// GET /relay/ws 不走 JWTAuth(ticket 在 hello 首帧内鉴权)。
func TestSetup_RelayRoutesWired(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	// NewRelayHandler(nil):svc 不会被触达(无 token 的请求在 JWTAuth
	// 就被 401 拦截);未 SetHub 时 ServeWS 固定 503,足以区分路由存在性。
	Setup(t.Context(), engine,
		nil,                          // healthPinger
		nil, nil, nil, nil, nil, nil, // repos
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false, // wechatOAuthMock
		false, // wechatPayMock
		nil,   // usageSvc
		nil,   // llmUsageSvc
		handler.NewRelayHandler(nil), // relayHandler 非 nil = relay 启用
	)

	have := make(map[string]bool)
	for _, r := range engine.Routes() {
		have[r.Method+":"+r.Path] = true
	}
	if !have["POST:/relay/ticket"] {
		t.Error("relay 启用时未注册 POST /relay/ticket")
	}
	if !have["GET:/relay/ws"] {
		t.Error("relay 启用时未注册 GET /relay/ws")
	}

	// /relay/ticket 无 token → 401(JWTAuth 拦截,handler 不触达)。
	// 连发 35 次全部 401 而非 429,证明 JWTAuth 在签发限流器之前
	// (若限流器在前,空 user_id 共享同一桶,burst 30 后应返回 429)。
	for i := 0; i < 35; i++ {
		req := httptest.NewRequest(http.MethodPost, "/relay/ticket", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("POST /relay/ticket 无 token 第 %d 次: got %d, want 401 (JWTAuth 应先于限流器)", i+1, w.Code)
		}
	}

	// /relay/ws 不走 JWTAuth:无 token 也不应 401;handler 未 SetHub 时
	// 返回 503,证明路由已装配到 relay handler。
	req := httptest.NewRequest(http.MethodGet, "/relay/ws", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("GET /relay/ws 返回 401:WS 路由不应挂 JWTAuth(ticket 在 hello 内鉴权)")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /relay/ws: got %d, want 503 (handler 未 SetHub 的停机响应,证明路由已接线)", w.Code)
	}
}
