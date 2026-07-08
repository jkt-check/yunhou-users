package router

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

func Setup(
	ctx context.Context,
	engine *gin.Engine,
	healthPinger handler.Pinger,
	appRepo repo.AppRepo,
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
	githubOAuthSvc *service.GitHubOAuthService,
) {
	// Health check
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	// Handlers
	authHandler := handler.NewAuthHandler(authSvc, tokenSvc)
	appHandler := handler.NewAppHandler(appRepo, providerTokenSvc)
	subHandler := handler.NewSubscriptionHandler(subSvc)
	planHandler := handler.NewPlanHandler(planSvc, appRepo, quoteSvc)
	userHandler := handler.NewUserHandler(userRepo, identityRepo)
	paymentHandler := handler.NewPaymentHandler(paymentSvc)
	webhookHandler := handler.NewWebhookHandler(paymentSvc, wechatAPIv3Key, webhookVerifier)

	// Public routes (rate limited)
	publicLimiter := middleware.RateLimit(ctx, 10, 20)
	engine.GET("/.well-known/jwks.json", publicLimiter, authHandler.JWKS)
	engine.POST("/auth/login", publicLimiter, authHandler.Login)
	engine.POST("/auth/refresh", publicLimiter, authHandler.RefreshToken)
	engine.POST("/auth/logout", publicLimiter, authHandler.Logout)
	// GitHub OAuth redirect flow — see handler.RegisterGitHubOAuthRoutes
	// and model.GitHubOAuthConfig for the boundary contract. Both
	// endpoints are public (no JWT — GitHub calls /callback directly);
	// they sit behind the public limiter like the other /auth/* routes.
	githubOAuthGroup := engine.Group("/auth/github", publicLimiter)
	handler.RegisterGitHubOAuthRoutes(githubOAuthGroup, githubOAuthSvc, appRepo, authSvc, tokenSvc)
	// Dev-only login endpoint for the L3 e2e-ui suite. Returns 404 unless
	// PAYPAL_L3_E2E_MODE=1 is set; the env check is inside the handler.
	engine.POST("/test/login", authHandler.TestLogin)

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
	}

	// Payment routes (JWT auth, user-scoped).
	// Per design doc + webhook doc §3, webhooks have their own rate limit
	// bucket (looser) and signature verification instead of JWT.
	paymentGroup := engine.Group("/payments")
	paymentGroup.Use(middleware.JWTAuth(tokenSvc), middleware.RateLimit(ctx, 30, 60))
	{
		// Order lifecycle
		paymentGroup.POST("/orders", paymentHandler.CreateOrder)
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
