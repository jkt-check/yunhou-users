package router

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

func Setup(
	ctx context.Context,
	engine *gin.Engine,
	healthPinger handler.Pinger,
	appRepo repo.AppRepo,
	auditLogRepo repo.AuditLogRepo,
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	planRepo repo.PlanRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *service.TokenService,
	authSvc *service.AuthService,
	subSvc *service.SubscriptionService,
	planSvc *service.PlanService,
	paymentSvc *service.PaymentService,
	webhookVerifier middleware.ChannelSignatureVerifier,
	wechatAPIv3Key []byte,
	providerTokenSvc *service.ProviderTokenService,
	quoteSvc *service.QuoteService,
	chatSvc *service.ChatService,
	chatAccessLog *log.Logger,
	githubOAuthSvc *service.GitHubOAuthService,
	wechatOAuthSvc *service.WeChatOAuthService,
	wechatOAuthMock bool,
	wechatPayMock bool,
	// appEnv is cfg.AppEnv: the production signal gating the dev-only
	// /test/login route. Anything not in config's non-production allowlist
	// (including the "prod" default) keeps the route unmounted.
	appEnv string,
	usageSvc *service.UsageService,
	adminModelsHandler *httpapi.AdminModelsHandler,
	adminOps *httpapi.AdminOps,
	accessOps *httpapi.AccessOps,
	llmUsageSvc *service.LLMUsageService,
	relayHandler *handler.RelayHandler,
	adminOpsSvc *service.AdminOpsService,
	adminUsersSvc *service.AdminUsersService,
	// dashboardAppIDs is cfg.DashboardAppIDs: the allowlist of app IDs
	// permitted to use the dashboard 运营 surface below (audit I-2). An
	// empty list denies every app (fail closed) — cmd/server refuses to
	// start without DASHBOARD_APP_IDS, and this middleware is the defence-
	// in-depth gate for router mounts that bypass the startup check.
	dashboardAppIDs []string,
) {
	// Health check
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	// Handlers
	authHandler := handler.NewAuthHandler(authSvc, tokenSvc)
	appHandler := handler.NewAppHandler(appRepo, providerTokenSvc, auditLogRepo)
	subHandler := handler.NewSubscriptionHandler(subSvc)
	planHandler := handler.NewPlanHandler(planSvc, appRepo, quoteSvc)
	userHandler := handler.NewUserHandler(userRepo, identityRepo)
	paymentHandler := handler.NewPaymentHandler(paymentSvc)
	webhookHandler := handler.NewWebhookHandler(paymentSvc, wechatAPIv3Key, webhookVerifier, wechatPayMock)
	usageHandler := handler.NewUsageHandler(usageSvc)
	llmUsageHandler := handler.NewLLMUsageHandler(llmUsageSvc)
	adminOpsHandler := handler.NewAdminOpsHandler(adminOpsSvc)
	adminUsersHandler := handler.NewAdminUsersHandler(adminUsersSvc)

	// Public routes (rate limited)
	publicLimiter := middleware.RateLimit(ctx, 10, 20)
	// Prometheus 抓取端点。无条件暴露:即使 relay 禁用,进程级
	// Go/runtime collector 仍可工作;挂在 publicLimiter 后防抓取滥用。
	engine.GET("/metrics", publicLimiter, gin.WrapH(promhttp.Handler()))
	engine.GET("/.well-known/jwks.json", publicLimiter, authHandler.JWKS)
	engine.POST("/auth/refresh", publicLimiter, authHandler.RefreshToken)
	engine.POST("/auth/logout", publicLimiter, authHandler.Logout)
	// GitHub OAuth redirect flow — see handler.RegisterGitHubOAuthRoutes
	// and model.GitHubOAuthConfig for the boundary contract. Both
	// endpoints are public (no JWT — GitHub calls /callback directly);
	// they sit behind the public limiter like the other /auth/* routes.
	githubOAuthGroup := engine.Group("/auth/github", publicLimiter)
	handler.RegisterGitHubOAuthRoutes(githubOAuthGroup, githubOAuthSvc, appRepo, authSvc, tokenSvc)
	// WeChat OAuth redirect flow — same shape as GitHub. Both endpoints
	// are public (no JWT — WeChat calls /callback directly); they sit
	// behind the public limiter like the other /auth/* routes.
	wechatOAuthGroup := engine.Group("/auth/wechat", publicLimiter)
	handler.RegisterWeChatOAuthRoutes(wechatOAuthGroup, wechatOAuthSvc, appRepo, authSvc, wechatOAuthMock)
	// Dev-only login endpoint for the L3 e2e-ui suite. The route is only
	// registered when PAYPAL_L3_E2E_MODE=1 AND APP_ENV is a non-production
	// value — anywhere else the path does not exist at all, so a stray env
	// line in a production .env is the ONLY way to expose it, and
	// config.Validate hard-fails when that combines with a production
	// APP_ENV (the primary signal, independent of PAYPAL_ENV — audit C-1)
	// or PAYPAL_ENV=live. The handler keeps its own env check as defence
	// in depth. Mounted behind the public limiter for rate-limit friction.
	if os.Getenv("PAYPAL_L3_E2E_MODE") == "1" && !config.IsProductionEnv(appEnv) {
		engine.POST("/test/login", publicLimiter, authHandler.TestLogin)
	}

	// Public plan listing — unauthenticated so marketing pages and the BFF
	// can fetch the catalog without holding admin credentials. Plan IDs and
	// prices are public info (they appear on the marketing site).
	engine.GET("/apps/:id/plans", publicLimiter, planHandler.GetAppPlans)

	// User routes (JWT auth required)
	userGroup := engine.Group("/user")
	userGroup.Use(middleware.JWTAuth(tokenSvc))
	{
		userGroup.GET("/profile", userHandler.GetProfile)
		userGroup.PATCH("/profile", userHandler.UpdateProfile)
		userGroup.GET("/identities", userHandler.ListIdentities)
		userGroup.DELETE("/identities/:id", userHandler.UnbindIdentity)
		userGroup.GET("/subscriptions", subHandler.ListUserSubscriptions)
		userGroup.POST("/subscriptions", subHandler.CreateSubscription)
		userGroup.DELETE("/subscriptions/:id", subHandler.CancelSubscription)

		// Usage heartbeat (2026-09-04-usage-analytics-design.md §3.3):
		// kaya sends one 5-min beat per login session, batched ≤100 when
		// flushing the offline buffer. Per-USER bucket (30/min sustained,
		// burst 10) — a per-IP bucket would collapse all users behind one
		// NAT into a single shared allowance. The key func runs after
		// JWTAuth, so ContextUserID is always set here.
		usageLimiter := middleware.RateLimitWithKey(ctx, 0.5, 10, func(c *gin.Context) string {
			return c.GetString(middleware.ContextUserID)
		})
		userGroup.POST("/usage/heartbeat", usageLimiter, usageHandler.PostHeartbeat)

		// Kaya Coding Plan Task 5: customer API-key self-management
		// (/user/api-keys). Ownership derives from ContextUserID set by
		// JWTAuth above — never from request-body fields. A nil
		// accessOps.UserAPIKeys leaves the surface unmounted (fail closed,
		// mirroring the Task 4 operator write surface).
		if accessOps != nil && accessOps.UserAPIKeys != nil {
			accessOps.UserAPIKeys.Register(userGroup)
		}

		// Kaya Coding Plan Task 11: customer quota/usage/subscription read
		// views (/user/model-*). Same ownership rule (JWT identity only);
		// nil handlers stay unmounted (fail closed).
		if accessOps != nil && accessOps.UserQuotas != nil {
			accessOps.UserQuotas.Register(userGroup)
		}
		if accessOps != nil && accessOps.UserUsage != nil {
			accessOps.UserUsage.Register(userGroup)
		}
		if accessOps != nil && accessOps.UserSubscriptions != nil {
			accessOps.UserSubscriptions.Register(userGroup)
		}
		// Kaya Coding Plan Task 14: customer wallet (/user/wallet*) — 余额
		// 总览/流水/套餐外开关/PAYG 开启；归属仅来自 JWT 身份。
		if accessOps != nil && accessOps.UserWallet != nil {
			accessOps.UserWallet.Register(userGroup)
		}
	}

	// Kaya Coding Plan Task 5/8: the standard-protocol /v1 surface. The
	// group fixes the auth chain — per-IP limiter as the outer perimeter
	// guard, then customer API-key authentication with per-Key/account RPM
	// buckets — so no /v1 route can ever be registered unauthenticated.
	// This chain is independent of the operator surface: X-App-Secret is
	// not a customer credential (设计 §9.2).
	if accessOps != nil && accessOps.V1Auth != nil {
		if accessOps.RPMCounter != nil {
			go accessOps.RPMCounter.RunJanitor(ctx, time.Minute, 2*time.Minute)
		}
		v1 := engine.Group("/v1", middleware.RateLimit(ctx, 60, 120), accessOps.V1Auth)
		// Task 8 protocol routes: the native OpenAI shapes, never the
		// management envelope (设计 §9.1). Nil handlers stay unmounted.
		if accessOps.V1Models != nil {
			v1.GET("/models", accessOps.V1Models.List)
		}
		if accessOps.V1ChatCompletions != nil {
			v1.POST("/chat/completions", accessOps.V1ChatCompletions.Create)
		}
		// Task 13 protocol routes: Anthropic Messages / OpenAI Responses
		// native surfaces (same auth chain, same gateway闸门).
		if accessOps.V1Messages != nil {
			v1.POST("/messages", accessOps.V1Messages.Create)
		}
		if accessOps.V1Responses != nil {
			v1.POST("/responses", accessOps.V1Responses.Create)
		}
	}

	// App routes (internal service auth)
	appLimiter := middleware.RateLimit(ctx, 30, 60)
	appGroup := engine.Group("/apps")
	appGroup.Use(appLimiter, middleware.InternalAppAuth(appRepo))
	{
		appGroup.GET("", appHandler.ListApps)
		appGroup.GET("/:id", appHandler.GetApp)
		appGroup.GET("/:id/provider-token/:channel", appHandler.GetProviderToken)
	}

	// JWT-authenticated quote endpoint — user must be logged in to ask for a
	// subscription price. Mounted at engine level (not under appGroup) so the
	// path stays /apps/:id/quote without colliding with InternalAppAuth that
	// wraps the other /apps/:id routes.
	engine.POST("/apps/:id/quote", appLimiter, middleware.JWTAuth(tokenSvc), planHandler.PostQuote)

	// Chat proxy — JWT-authenticated, subscription-gated DeepSeek streaming
	// endpoint for consumer apps (kaya). Sits outside the 20s request
	// timeout (see cmd/server timeoutMiddleware skip list) because the SSE
	// stream can legitimately run longer; its own limiter bucket is tighter
	// than the generic app bucket because every call spends upstream tokens.
	//
	// Task 8 迁移开关：accessOps.KayaChat 非空时 /chat 由 inference 网关
	// facade 服务（INFERENCE_KAYA_CHAT_GATEWAY=1），否则保持旧 DeepSeek
	// 直通。两条路径共用同一 handler（鉴权、限流、审计日志、SSE relay、
	// 错误 shape 不变）。
	chatLimiter := middleware.RateLimit(ctx, 10, 20)
	var chatStreamSvc service.ChatStreamer = chatSvc
	if accessOps != nil && accessOps.KayaChat != nil {
		chatStreamSvc = accessOps.KayaChat
	}
	chatHandler := handler.NewChatHandler(chatStreamSvc, chatAccessLog)
	engine.POST("/chat", chatLimiter, middleware.JWTAuth(tokenSvc), chatHandler.StreamChat)
	// GET /chat/models (Kaya 模型选择契约):facade 路径用 inference 目录
	// (无计费账号时空列表而非报错);否则用多模型 ChatService 的权益视图。
	if accessOps != nil && accessOps.KayaChatModels != nil {
		engine.GET("/chat/models", chatLimiter, middleware.JWTAuth(tokenSvc), accessOps.KayaChatModels.List)
	} else {
		engine.GET("/chat/models", chatLimiter, middleware.JWTAuth(tokenSvc), chatHandler.GetModels)
	}

	// Relay(kaya 远程控制)。relayHandler 为 nil = relay 禁用(RELAY_TICKET_SECRET 未配置)。
	if relayHandler != nil {
		// 签发限流 30/min/user(spec §3.1):r=0.5/s,burst=30。
		// JWTAuth 在前,key func 才能读到 user_id。
		ticketLimiter := middleware.RateLimitWithKey(ctx, 0.5, 30, func(c *gin.Context) string {
			return c.GetString(middleware.ContextUserID)
		})
		engine.POST("/relay/ticket", middleware.JWTAuth(tokenSvc), ticketLimiter, relayHandler.IssueTicket)
		// WS 长连接:不走 JWTAuth(ticket 在 hello 首帧内鉴权,spec §7),
		// 且在 timeoutMiddleware 的 skip 列表中(main.go)。
		engine.GET("/relay/ws", relayHandler.ServeWS)
	}

	// Admin routes for plan management (internal service auth)
	adminLimiter := middleware.RateLimit(ctx, 30, 60)
	adminGroup := engine.Group("/admin")
	adminGroup.Use(adminLimiter, middleware.InternalAppAuth(appRepo))
	{
		// Plan management
		adminGroup.GET("/plans", planHandler.ListPlans)
		adminGroup.GET("/plans/:id", planHandler.GetPlan)
		adminGroup.POST("/plans", planHandler.CreatePlan)
		adminGroup.PATCH("/plans/:id", planHandler.UpdatePlan)
		adminGroup.DELETE("/plans/:id", planHandler.DeletePlan)

		// App management
		adminGroup.POST("/apps", appHandler.CreateApp)
		adminGroup.PATCH("/apps/:id", appHandler.UpdateApp)
		// Secret rotation: dedicated endpoint so it has its own audit trail
		// and a response shape that always returns the new plaintext once.
		adminGroup.POST("/apps/:id/rotate-secret", appHandler.RotateSecret)

		// Usage analytics stats (2026-09-04-usage-analytics-design.md §3.3):
		// DAU/WAU/MAU, usage duration, and new-user counts over the
		// usage_events heartbeat table. Consumed by manual API calls / SQL
		// for now — no ops dashboard in v1.
		adminGroup.GET("/stats/active", usageHandler.GetActiveStats)
		adminGroup.GET("/stats/usage-duration", usageHandler.GetUsageDuration)
		adminGroup.GET("/stats/new-users", usageHandler.GetNewUsers)

		// Inference model catalog (Kaya Coding Plan Task 3): read-only
		// model/deployment/revision queries. Write endpoints (create/edit/
		// publish/rollback) are mounted below under the Task 4 operator
		// authorization chain — the read-only surface stays available to
		// verified internal apps, writes require the verified user JWT +
		// service identity combo AND the models:manage permission.
		adminModelsHandler.RegisterReadOnly(adminGroup)

		// Task 4: operator write surface — catalog writes (models:manage),
		// credential lifecycle (credentials:manage), operator administration
		// (admin role). The chain is InternalAppAuth (ancestor group) +
		// JWTAuth + per-permission authorization middleware; a nil adminOps
		// leaves the entire write surface unmounted (defence in depth for
		// tests and misconfigured builds).
		if adminOps != nil {
			opsGroup := adminGroup.Group("")
			opsGroup.Use(middleware.JWTAuth(tokenSvc))
			adminOps.Mount(opsGroup)
		}

		// LLM token metering aggregates (migration 022).
		adminGroup.GET("/stats/llm-usage", llmUsageHandler.GetByModel)

		// Dashboard 运营 API(dashboard-admin-api spec):运营指标、用户
		// 搜索/详情、VIP 加时长。挂在 InternalAppAuth 链内的一个子组,
		// 外加 DashboardAllowlist(audit I-2):仅 DASHBOARD_APP_IDS 白名单
		// 内的 app 可用 —— 否则任一持有效 app secret 的内部服务都能读
		// 邮箱 PII / 写 VIP。白名单为空 = 全部 403(fail closed),cmd/server
		// 在启动时即以空名单拒启。审计归因沿用 admin:<appID>(见
		// adminActorID);按人归因需 dashboard 鉴权改造,不在本次范围。
		dashboardGroup := adminGroup.Group("")
		dashboardGroup.Use(middleware.DashboardAllowlist(dashboardAppIDs))
		dashboardGroup.GET("/ops/metrics", adminOpsHandler.GetMetrics)
		dashboardGroup.GET("/users/search", adminUsersHandler.SearchUsers)
		dashboardGroup.GET("/users/:id", adminUsersHandler.GetUser)
		dashboardGroup.POST("/users/:id/vip", adminUsersHandler.AddVip)
	}

	// Payment routes (JWT auth, user-scoped).
	// Per design doc + webhook doc §3, webhooks have their own rate limit
	// bucket (looser) and signature verification instead of JWT.
	paymentGroup := engine.Group("/payments")
	paymentGroup.Use(middleware.JWTAuth(tokenSvc), middleware.RateLimit(ctx, 30, 60))
	{
		// Order lifecycle
		paymentGroup.POST("/orders", paymentHandler.CreateOrder)
		paymentGroup.GET("/orders", paymentHandler.ListOrders)
		paymentGroup.GET("/orders/:id", paymentHandler.GetOrder)
		paymentGroup.DELETE("/orders/:id", paymentHandler.CancelOrder)
		paymentGroup.POST("/orders/:order_id/confirm", paymentHandler.ConfirmOrder)

		// Payment reads
		paymentGroup.GET("", paymentHandler.ListPayments)
		paymentGroup.GET("/:id", paymentHandler.GetPayment)
		paymentGroup.GET("/:id/refunds", paymentHandler.ListPaymentRefunds)
	}

	// Refund routes (separate prefix per design doc).
	// /refunds is also JWT-protected; ownership is enforced in service.
	refundGroup := engine.Group("/refunds")
	refundGroup.Use(middleware.JWTAuth(tokenSvc), middleware.RateLimit(ctx, 30, 60))
	{
		refundGroup.POST("", paymentHandler.CreateRefund)
		refundGroup.GET("/:id", paymentHandler.GetRefund)
	}

	// Webhook endpoints — separate rate limit bucket (looser), signature
	// verification instead of JWT. See webhook doc §3 / §4.
	webhookLimiter := middleware.RateLimit(ctx, 200, 400)
	webhookGroup := engine.Group("/webhooks/payment")
	webhookGroup.Use(webhookLimiter, middleware.WebhookSignature(webhookVerifier))
	{
		webhookGroup.POST("/:channel", webhookHandler.Handle)
	}
}
