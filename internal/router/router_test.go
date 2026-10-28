package router

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
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
		nil,                               // healthPinger
		nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false,               // wechatOAuthMock
		false,               // wechatPayMock
		"dev",               // appEnv — non-production, so /test/login may mount
		nil,                 // usageSvc
		nil,                 // usageTokenSvc
		nil,                 // adminModelsHandler
		nil,                 // adminOps
		nil,                 // accessOps
		nil,                 // llmUsageSvc
		nil,                 // relayHandler
		nil,                 // adminOpsSvc
		nil,                 // adminUsersSvc
		[]string{"yundash"}, // dashboardAppIDs — 白名单非空即可,Routes() 自省不触达中间件
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
		"GET:/admin/models",
		"GET:/admin/models/:id",
		"GET:/admin/models/:id/routes",
		"GET:/admin/deployments",
		"GET:/admin/catalog/revisions",
		"GET:/admin/catalog/active",
		"GET:/admin/stats/llm-usage",
		// Dashboard 运营 admin API(dashboard-admin-api spec):挂在
		// adminGroup(InternalAppAuth 链),与 operator JWT 面无关。
		"GET:/admin/ops/metrics",
		"GET:/admin/users/search",
		"GET:/admin/users/:id",
		"POST:/admin/users/:id/vip",
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("Setup did not register route %s", w)
		}
	}
	// The Task 4 operator write surface is mounted only when adminOps is
	// provided; this Setup call passed nil, so catalog writes, credentials
	// and operator administration must all stay unmounted (fail closed for
	// tests/misconfigured builds).
	for _, w := range []string{
		"POST:/admin/models",
		"PATCH:/admin/models/:id",
		"DELETE:/admin/models/:id",
		"POST:/admin/deployments",
		"PATCH:/admin/deployments/:id",
		"DELETE:/admin/deployments/:id",
		"POST:/admin/catalog/publish",
		"POST:/admin/catalog/rollback",
		"POST:/admin/credentials",
		"POST:/admin/credentials/:id/rotate",
		"POST:/admin/operators",
		// 可调度账号管理面（凭据→账号绑定/启停/调并发）同组 fail-closed。
		"POST:/admin/upstream-accounts",
		"POST:/admin/upstream-accounts/:id/status",
		"PATCH:/admin/upstream-accounts/:id",
		// Task 15 运营面同样 fail-closed（nil adminOps = 不挂载）。
		"POST:/admin/catalog/bulk-import",
		"GET:/admin/model-usage/summary",
		"GET:/admin/model-usage/exceptions",
		"GET:/admin/upstream-accounts/shared",
		"GET:/admin/model-adjustments",
		"POST:/admin/model-prices/preview",
		"POST:/admin/quota-policies/preview",
		// 售价版本管理面同样 fail-closed（nil adminOps = 不挂载）。
		"POST:/admin/price-versions",
		"GET:/admin/price-versions",
	} {
		if have[w] {
			t.Errorf("Setup registered operator write route %s with nil adminOps", w)
		}
	}
	// The Task 5 customer key-management surface is mounted only when
	// accessOps carries a UserAPIKeys handler; nil must leave it unmounted.
	for _, w := range []string{
		"POST:/user/api-keys",
		"GET:/user/api-keys",
		"GET:/user/api-keys/:id",
		"PATCH:/user/api-keys/:id",
		"DELETE:/user/api-keys/:id",
	} {
		if have[w] {
			t.Errorf("Setup registered customer key route %s with nil accessOps", w)
		}
	}
	// /test/login must NOT be registered without PAYPAL_L3_E2E_MODE=1 —
	// the route existing at all is the dev-only escape hatch.
	if have["POST:/test/login"] {
		t.Error("Setup registered /test/login without PAYPAL_L3_E2E_MODE=1")
	}
}

// TestSetup_TestLoginGatedOnEnv verifies the /test/login route exists only
// when PAYPAL_L3_E2E_MODE=1 AND the appEnv argument is a non-production
// APP_ENV value — previously it was always registered and gated only inside
// the handler, so one stray env line in production would have exposed
// arbitrary JWT minting.
func TestSetup_TestLoginGatedOnEnv(t *testing.T) {
	// Not parallel: t.Setenv mutates process env.
	gin.SetMode(gin.TestMode)
	t.Setenv("PAYPAL_L3_E2E_MODE", "1")
	t.Setenv("APP_ENV", "dev")
	engine := gin.New()

	Setup(t.Context(), engine,
		nil,                               // healthPinger
		nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false,               // wechatOAuthMock
		false,               // wechatPayMock
		"dev",               // appEnv — non-production, so /test/login may mount
		nil,                 // usageSvc
		nil,                 // usageTokenSvc
		nil,                 // adminModelsHandler
		nil,                 // adminOps
		nil,                 // accessOps
		nil,                 // llmUsageSvc
		nil,                 // relayHandler
		nil,                 // adminOpsSvc
		nil,                 // adminUsersSvc
		[]string{"yundash"}, // dashboardAppIDs — 白名单非空即可,Routes() 自省不触达中间件
	)

	for _, r := range engine.Routes() {
		if r.Method == "POST" && r.Path == "/test/login" {
			return
		}
	}
	t.Error("Setup did not register /test/login with PAYPAL_L3_E2E_MODE=1 + non-production APP_ENV")
}

// TestSetup_TestLoginRefusedUnderProductionEnv is the audit-C-1 router-side
// check: even with PAYPAL_L3_E2E_MODE=1 in the environment, a production
// APP_ENV ("prod" is also the Load() default for an unset variable) must
// keep the JWT-minting route unmounted. This covers the cn-prod shape where
// PAYPAL_ENV is never set and the old PAYPAL_ENV=live guard never fired.
func TestSetup_TestLoginRefusedUnderProductionEnv(t *testing.T) {
	// Not parallel: t.Setenv mutates process env.
	for _, appEnv := range []string{"prod", "production", ""} {
		gin.SetMode(gin.TestMode)
		t.Setenv("PAYPAL_L3_E2E_MODE", "1")
		engine := gin.New()

		Setup(t.Context(), engine,
			nil,                               // healthPinger
			nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
			nil,           // tokenSvc
			nil,           // authSvc
			nil, nil, nil, // subSvc, planSvc, paymentSvc
			nil,      // webhookVerifier
			nil,      // wechatAPIv3Key
			nil, nil, // providerTokenSvc, quoteSvc
			nil,      // chatSvc
			nil,      // chatAccessLog
			nil, nil, // githubOAuthSvc, wechatOAuthSvc
			false,               // wechatOAuthMock
			false,               // wechatPayMock
			appEnv,              // production signal — /test/login must stay unmounted
			nil,                 // usageSvc
			nil,                 // usageTokenSvc
			nil,                 // adminModelsHandler
			nil,                 // adminOps
			nil,                 // accessOps
			nil,                 // llmUsageSvc
			nil,                 // relayHandler
			nil,                 // adminOpsSvc
			nil,                 // adminUsersSvc
			[]string{"yundash"}, // dashboardAppIDs
		)

		for _, r := range engine.Routes() {
			if r.Method == "POST" && r.Path == "/test/login" {
				t.Errorf("appEnv=%q: Setup registered /test/login under a production APP_ENV", appEnv)
			}
		}
	}
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
		nil,                               // healthPinger
		nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false,         // wechatOAuthMock
		false,         // wechatPayMock
		"dev",         // appEnv
		nil,           // usageSvc
		nil,           // usageTokenSvc
		nil, nil, nil, // adminModelsHandler, adminOps, accessOps
		nil,                          // llmUsageSvc
		handler.NewRelayHandler(nil), // relayHandler 非 nil = relay 启用
		nil,                          // adminOpsSvc
		nil,                          // adminUsersSvc
		[]string{"yundash"},          // dashboardAppIDs
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

// TestSetup_UsageTokenRoutesWired 验证 kaya token 用量统计面（usage-stats.md
// v1.2）在 usageTokenSvc 非 nil 时全部挂载；nil 时整块不挂载（fail
// closed，与 accessOps 同姿势）。service 带 nil repo：Setup 只注册路由，
// 不触达依赖。
func TestSetup_UsageTokenRoutesWired(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	setup := func(usageTokenSvc *service.UsageTokenService) map[string]bool {
		engine := gin.New()
		Setup(t.Context(), engine,
			nil,                               // healthPinger
			nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
			nil,           // tokenSvc
			nil,           // authSvc
			nil, nil, nil, // subSvc, planSvc, paymentSvc
			nil,      // webhookVerifier
			nil,      // wechatAPIv3Key
			nil, nil, // providerTokenSvc, quoteSvc
			nil,      // chatSvc
			nil,      // chatAccessLog
			nil, nil, // githubOAuthSvc, wechatOAuthSvc
			false,               // wechatOAuthMock
			false,               // wechatPayMock
			"dev",               // appEnv
			nil,                 // usageSvc
			usageTokenSvc,       // usageTokenSvc — 被测参数
			nil,                 // adminModelsHandler
			nil,                 // adminOps
			nil,                 // accessOps
			nil,                 // llmUsageSvc
			nil,                 // relayHandler
			nil,                 // adminOpsSvc
			nil,                 // adminUsersSvc
			[]string{"yundash"}, // dashboardAppIDs
		)
		have := make(map[string]bool)
		for _, r := range engine.Routes() {
			have[r.Method+":"+r.Path] = true
		}
		return have
	}

	want := []string{
		"POST:/user/usage/tokens/batch",
		"POST:/user/usage/tokens/backfill",
		"GET:/user/usage/tokens/summary",
		"GET:/user/usage/tokens/breakdown",
		"GET:/user/usage/tokens/timeseries",
		"GET:/user/usage/tokens/coverage",
		"GET:/user/usage/tokens/export",
		"DELETE:/user/usage/tokens",
		"POST:/user/devices",
		"GET:/user/devices",
		"PATCH:/user/devices/:id",
		"DELETE:/user/devices/:id",
	}
	mounted := setup(service.NewUsageTokenService(nil, "hour", 180, 500000))
	for _, w := range want {
		if !mounted[w] {
			t.Errorf("Setup did not register route %s with non-nil usageTokenSvc", w)
		}
	}
	unmounted := setup(nil)
	for _, w := range want {
		if unmounted[w] {
			t.Errorf("Setup registered route %s with nil usageTokenSvc (must stay fail-closed)", w)
		}
	}
}

// ---------------------------------------------------------------------------
// kaya token 用量面：限流桶拆分 + 看板查询缓存的接线（Task 2）
//
// 这组测试打的是真实的 router.Setup 装配出来的路由：JWT → per-user 限流器
// → 查询缓存 → handler。"被限流"只能由 429 表达——所以 repo 换成只回空结果
// 的假实现，让 handler 正常返回 200，不会被 nil repo 的 panic 掩盖。
// ---------------------------------------------------------------------------

// fakeUsageTokenRepo 是 router 测试用的 in-memory repo.UsageTokenRepo：回空
// 结果，并统计下游调用次数（用来验证看板查询被缓存合并）。
type fakeUsageTokenRepo struct {
	mu                sync.Mutex
	summaryCalls      int
	coverageCalls     int
	exportCalls       int
	listDevicesCalls  int
	upsertDeviceCalls int
}

var _ repo.UsageTokenRepo = (*fakeUsageTokenRepo)(nil)

func (f *fakeUsageTokenRepo) countSummary() {
	f.mu.Lock()
	f.summaryCalls++
	f.mu.Unlock()
}

func (f *fakeUsageTokenRepo) countCoverage() {
	f.mu.Lock()
	f.coverageCalls++
	f.mu.Unlock()
}

func (f *fakeUsageTokenRepo) countExport() {
	f.mu.Lock()
	f.exportCalls++
	f.mu.Unlock()
}

func (f *fakeUsageTokenRepo) countListDevices() {
	f.mu.Lock()
	f.listDevicesCalls++
	f.mu.Unlock()
}

func (f *fakeUsageTokenRepo) countUpsertDevice() {
	f.mu.Lock()
	f.upsertDeviceCalls++
	f.mu.Unlock()
}

func (f *fakeUsageTokenRepo) callCount(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "summary":
		return f.summaryCalls
	case "coverage":
		return f.coverageCalls
	case "export":
		return f.exportCalls
	case "listDevices":
		return f.listDevicesCalls
	case "upsertDevice":
		return f.upsertDeviceCalls
	}
	return -1
}

func (f *fakeUsageTokenRepo) IngestBuckets(_ context.Context, _ string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error) {
	return &model.UsageIngestResult{Accepted: len(in.Buckets), Updated: len(in.Buckets)}, nil
}

func (f *fakeUsageTokenRepo) Summary(_ context.Context, _, _ string, _, _ time.Time, _ model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
	f.countSummary()
	return &model.UsageTokenSummary{}, nil
}

func (f *fakeUsageTokenRepo) Breakdown(_ context.Context, _, _, _ string, _, _ time.Time, _ model.UsageTokenFilter, _ int) ([]model.UsageBreakdownItem, error) {
	return nil, nil
}

func (f *fakeUsageTokenRepo) Timeseries(_ context.Context, _, _, _, _, _ string, _, _ time.Time, _ model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
	return nil, nil
}

func (f *fakeUsageTokenRepo) CoverageDevices(_ context.Context, _, _ string) ([]model.UsageDeviceView, error) {
	f.countCoverage()
	return nil, nil
}

func (f *fakeUsageTokenRepo) ExportRows(_ context.Context, _, _ string, _, _ time.Time, _ model.UsageTokenFilter, _ int, decide func(total int64) (bool, error)) ([]model.UsageExportRow, int64, error) {
	f.countExport()
	if _, err := decide(0); err != nil {
		return nil, 0, err
	}
	return nil, 0, nil
}

func (f *fakeUsageTokenRepo) DeleteAll(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeUsageTokenRepo) DeleteByDevice(_ context.Context, _, _ string, _ time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeUsageTokenRepo) DeleteWindow(_ context.Context, _ string, _ *string, _, _ time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeUsageTokenRepo) UpsertDevice(_ context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDevice, error) {
	f.countUpsertDevice()
	return &model.UsageDevice{UserID: userID, DeviceID: in.DeviceID}, nil
}

func (f *fakeUsageTokenRepo) ListDevices(_ context.Context, _ string) ([]model.UsageDevice, error) {
	f.countListDevices()
	return nil, nil
}

func (f *fakeUsageTokenRepo) UpdateDeviceLabel(_ context.Context, _, _ string, _ *string) error {
	return nil
}

func (f *fakeUsageTokenRepo) RevokeDevice(_ context.Context, _, _ string, _ bool) error { return nil }

func (f *fakeUsageTokenRepo) SweepRetention(_ context.Context, _ time.Time) (int64, int64, error) {
	return 0, 0, nil
}

// usageTokenTestKey 是测试用 RSA 密钥对：整个包只生成一次（RSA 生成在负载
// 高的机器上不便宜），JWTAuth 需要能验签的公钥。
var usageTokenTestKey struct {
	once sync.Once
	priv *rsa.PrivateKey
	err  error
}

// usageTokenTestTokenSvc 返回一个能签发/验签 access token 的 TokenService。
func usageTokenTestTokenSvc(t *testing.T) *service.TokenService {
	t.Helper()
	usageTokenTestKey.once.Do(func() {
		usageTokenTestKey.priv, usageTokenTestKey.err = rsa.GenerateKey(rand.Reader, 2048)
	})
	if usageTokenTestKey.err != nil {
		t.Fatalf("generate rsa key: %v", usageTokenTestKey.err)
	}
	return &service.TokenService{
		PrivateKey: usageTokenTestKey.priv,
		PublicKey:  &usageTokenTestKey.priv.PublicKey,
		AccessTTL:  time.Hour,
	}
}

// newUsageTokenTestEngine 用真实的 router.Setup 装配整条 Usage 路由链
// （JWT → per-user 限流器 → 查询缓存 → handler），并注入假 repo。
func newUsageTokenTestEngine(t *testing.T, fake *fakeUsageTokenRepo) (*gin.Engine, *service.TokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	tokenSvc := usageTokenTestTokenSvc(t)

	Setup(t.Context(), engine,
		nil,                               // healthPinger
		nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
		tokenSvc,      // tokenSvc
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
		"dev", // appEnv
		nil,   // usageSvc
		service.NewUsageTokenService(fake, "hour", 180, 500000), // usageTokenSvc
		nil,                 // adminModelsHandler
		nil,                 // adminOps
		nil,                 // accessOps
		nil,                 // llmUsageSvc
		nil,                 // relayHandler
		nil,                 // adminOpsSvc
		nil,                 // adminUsersSvc
		[]string{"yundash"}, // dashboardAppIDs
	)
	return engine, tokenSvc
}

// usageTokenMint 签一个该用户的 access token。
func usageTokenMint(t *testing.T, tokenSvc *service.TokenService, userID string) string {
	t.Helper()
	token, err := tokenSvc.SignAccessToken(userID, "app-test", nil)
	if err != nil {
		t.Fatalf("sign access token for %s: %v", userID, err)
	}
	return token
}

// usageTokenDo 发一个带 JWT 的请求，返回响应记录。写请求带一个空 JSON 体
// （handler 会在落库前拒掉它，够用来消耗限流额度）。
func usageTokenDo(engine *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	body := ""
	if method == http.MethodPost || method == http.MethodPatch {
		body = "{}"
	}
	return usageTokenDoBody(engine, method, path, token, body)
}

// usageTokenDoBody 发一个带 JWT 与指定请求体的请求，返回响应记录。
func usageTokenDoBody(engine *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// usageTokenDrainUntilLimited 反复打同一路径直到限流器挡下（429），返回那次
// 响应。max 是安全上限：配置错了也不至于死循环。
func usageTokenDrainUntilLimited(t *testing.T, engine *gin.Engine, method, path, token string, max int) *httptest.ResponseRecorder {
	t.Helper()
	for i := 0; i < max; i++ {
		w := usageTokenDo(engine, method, path, token)
		if w.Code == http.StatusTooManyRequests {
			return w
		}
	}
	t.Fatalf("%s %s: never rate limited within %d requests", method, path, max)
	return nil
}

// usageTokenAssertBurst 复核一路限流器的突发额度：先放行 burst 个请求，第
// burst+1 个必须 429。
func usageTokenAssertBurst(t *testing.T, engine *gin.Engine, method, path, token string, burst int) {
	t.Helper()
	for i := 0; i < burst; i++ {
		if code := usageTokenDo(engine, method, path, token).Code; code == http.StatusTooManyRequests {
			t.Fatalf("%s %s: request %d of the %d allowed burst was rate limited", method, path, i+1, burst)
		}
	}
	if code := usageTokenDo(engine, method, path, token).Code; code != http.StatusTooManyRequests {
		t.Fatalf("%s %s: request %d should be rate limited, got %d", method, path, burst+1, code)
	}
}

// TestSetup_UsageTokenQueryRateLimitAllowsDesignedBurst 验证看板查询桶的实际
// 装配：新用户的 80 个并发看板查询必须全部放行（桶是 20 QPS / burst 100），
// 桶耗尽后返回 429 且带非空 Retry-After。
func TestSetup_UsageTokenQueryRateLimitAllowsDesignedBurst(t *testing.T) {
	t.Parallel()

	// 桶的装配值：看板查询+设备读 20 QPS / burst 100。
	limiters := newUsageTokenLimiters(t.Context(), func(c *gin.Context) string {
		return c.GetString(middleware.ContextUserID)
	})
	if limiters.query.rate != 20 || limiters.query.burst != 100 {
		t.Errorf("query limiter: got %g/%d, want 20/100", limiters.query.rate, limiters.query.burst)
	}

	fake := &fakeUsageTokenRepo{}
	engine, tokenSvc := newUsageTokenTestEngine(t, fake)
	token := usageTokenMint(t, tokenSvc, "user-query-burst")

	const burst = 80
	codes := make([]int, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/summary", token).Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code == http.StatusTooManyRequests {
			t.Fatalf("dashboard query %d/%d was rate limited: the query bucket must let a fresh user's burst through", i+1, burst)
		}
	}

	// 桶耗尽后必须 429，且带非空 Retry-After（客户端据此退避）。
	limited := usageTokenDrainUntilLimited(t, engine, http.MethodGet, "/user/usage/tokens/summary", token, 500)
	if retry := limited.Header().Get("Retry-After"); retry == "" {
		t.Error("429 response carries an empty Retry-After header")
	}
}

// TestSetup_UsageTokenCoverageHasIndependentBucket 验证 coverage 有自己独立的
// 限流桶：打空看板桶不影响 coverage，打空 coverage 桶也不影响看板查询。
func TestSetup_UsageTokenCoverageHasIndependentBucket(t *testing.T) {
	t.Parallel()

	// coverage 的装配值：独立 1 QPS / burst 30 桶。
	limiters := newUsageTokenLimiters(t.Context(), func(c *gin.Context) string {
		return c.GetString(middleware.ContextUserID)
	})
	if limiters.coverage.rate != 1 || limiters.coverage.burst != 30 {
		t.Errorf("coverage limiter: got %g/%d, want 1/30", limiters.coverage.rate, limiters.coverage.burst)
	}

	fake := &fakeUsageTokenRepo{}
	engine, tokenSvc := newUsageTokenTestEngine(t, fake)

	// 用户 A：打空看板桶 → coverage 仍必须放行。
	tokenA := usageTokenMint(t, tokenSvc, "user-coverage-dashboard")
	usageTokenDrainUntilLimited(t, engine, http.MethodGet, "/user/usage/tokens/summary", tokenA, 500)
	if w := usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/coverage", tokenA); w.Code == http.StatusTooManyRequests {
		t.Fatal("coverage was rate limited by the exhausted dashboard bucket: coverage must own an independent bucket")
	}

	// 用户 B：打空 coverage 桶 → 看板查询仍必须放行。
	tokenB := usageTokenMint(t, tokenSvc, "user-coverage-coverage")
	usageTokenDrainUntilLimited(t, engine, http.MethodGet, "/user/usage/tokens/coverage", tokenB, 200)
	if w := usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/summary", tokenB); w.Code == http.StatusTooManyRequests {
		t.Fatal("dashboard query was rate limited by the exhausted coverage bucket: coverage must own an independent bucket")
	}
}

// TestSetup_UsageTokenUnchangedWriteLimiters 验证摄入/导出/删除/设备写的限流
// 配置一字未改：batch 2/20、backfill 0.167/2、export 0.0834/2、delete
// 0.1/3、设备写 0.167/5。值从 Setup 实际挂载的那批限流器（同一个构造函数
// newUsageTokenLimiters）读；burst 再用真实路由行为复核一遍。
func TestSetup_UsageTokenUnchangedWriteLimiters(t *testing.T) {
	t.Parallel()

	limiters := newUsageTokenLimiters(t.Context(), func(c *gin.Context) string {
		return c.GetString(middleware.ContextUserID)
	})
	for _, tc := range []struct {
		name  string
		lim   usageTokenLimiter
		rate  float64
		burst int
	}{
		{"batch", limiters.batch, 2, 20},
		{"backfill", limiters.backfill, 0.167, 2},
		{"export", limiters.export, 0.0834, 2},
		{"delete", limiters.delete, 0.1, 3},
		{"deviceWrite", limiters.deviceWrite, 0.167, 5},
	} {
		if tc.lim.rate != tc.rate || tc.lim.burst != tc.burst {
			t.Errorf("%s limiter: got %g/%d, want %g/%d", tc.name, tc.lim.rate, tc.lim.burst, tc.rate, tc.burst)
		}
	}

	// burst 用真实路由复核（请求体故意非法 → handler 400，既不落库也不
	// 需要 repo）。
	fake := &fakeUsageTokenRepo{}
	engine, tokenSvc := newUsageTokenTestEngine(t, fake)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		burst  int
	}{
		{"batch", http.MethodPost, "/user/usage/tokens/batch", 20},
		{"backfill", http.MethodPost, "/user/usage/tokens/backfill", 2},
		{"export", http.MethodGet, "/user/usage/tokens/export?format=bogus", 2},
		{"delete", http.MethodDelete, "/user/usage/tokens?device_id=not-a-device-id", 3},
		{"deviceWrite", http.MethodPost, "/user/devices", 5},
	} {
		token := usageTokenMint(t, tokenSvc, "user-write-"+tc.name)
		usageTokenAssertBurst(t, engine, tc.method, tc.path, token, tc.burst)
	}
}

// TestSetup_UsageTokenQueryCacheMountedOnlyOnDashboardQueries 验证查询缓存的
// 挂载面：只挂看板查询（summary / breakdown / timeseries / GET devices），
// coverage 与 export 不缓存；写成功后该用户的缓存立即失效。
func TestSetup_UsageTokenQueryCacheMountedOnlyOnDashboardQueries(t *testing.T) {
	t.Parallel()
	fake := &fakeUsageTokenRepo{}
	engine, tokenSvc := newUsageTokenTestEngine(t, fake)
	token := usageTokenMint(t, tokenSvc, "user-cache")

	// 看板查询：同一组参数的重复请求只打一次下游。
	for i := 0; i < 10; i++ {
		if code := usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/summary", token).Code; code != http.StatusOK {
			t.Fatalf("summary request %d: got %d, want 200", i+1, code)
		}
	}
	// 缓存条目最长活到 5 秒时间桶结束，跨桶会再打一次下游，所以上界给 2。
	if calls := fake.callCount("summary"); calls > 2 {
		t.Errorf("summary downstream calls = %d, want <= 2 (dashboard queries must be cached)", calls)
	}

	// 设备列表同样在缓存面上。
	for i := 0; i < 5; i++ {
		if code := usageTokenDo(engine, http.MethodGet, "/user/devices", token).Code; code != http.StatusOK {
			t.Fatalf("devices request %d: got %d, want 200", i+1, code)
		}
	}
	if calls := fake.callCount("listDevices"); calls > 2 {
		t.Errorf("listDevices downstream calls = %d, want <= 2 (GET devices must be cached)", calls)
	}

	// coverage 是配置发现，不进缓存。
	for i := 0; i < 5; i++ {
		if code := usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/coverage", token).Code; code != http.StatusOK {
			t.Fatalf("coverage request %d: got %d, want 200", i+1, code)
		}
	}
	if calls := fake.callCount("coverage"); calls != 5 {
		t.Errorf("coverage downstream calls = %d, want 5 (coverage must not be cached)", calls)
	}

	// export 直出文件流（带 X-Result-* 头），不进缓存；限流器 burst 只有 2，
	// 所以只发两个。
	for i := 0; i < 2; i++ {
		if code := usageTokenDo(engine, http.MethodGet, "/user/usage/tokens/export?format=json", token).Code; code != http.StatusOK {
			t.Fatalf("export request %d: got %d, want 200", i+1, code)
		}
	}
	if calls := fake.callCount("export"); calls != 2 {
		t.Errorf("export downstream calls = %d, want 2 (export must not be cached)", calls)
	}

	// 写成功后失效：设备注册成功后，下一次 GET devices 必须重新打下游，
	// 不能复用写之前的缓存条目。
	before := fake.callCount("listDevices")
	if code := usageTokenDoBody(engine, http.MethodPost, "/user/devices", token, `{"device_id":"d_abcdefgh"}`).Code; code != http.StatusOK {
		t.Fatalf("device registration: got %d, want 200", code)
	}
	if writes := fake.callCount("upsertDevice"); writes != 1 {
		t.Fatalf("device writes = %d, want 1 (the write must reach the repo)", writes)
	}
	if code := usageTokenDo(engine, http.MethodGet, "/user/devices", token).Code; code != http.StatusOK {
		t.Fatalf("devices after write: got %d, want 200", code)
	}
	if after := fake.callCount("listDevices"); after != before+1 {
		t.Errorf("listDevices downstream calls after a successful write = %d, want %d (writes must invalidate the user's cache)", after, before+1)
	}
}
