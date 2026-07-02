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
) {
	// Health check
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	// Handlers
	authHandler := handler.NewAuthHandler(authSvc, tokenSvc)
	appHandler := handler.NewAppHandler(appRepo)
	subHandler := handler.NewSubscriptionHandler(subSvc)
	planHandler := handler.NewPlanHandler(planSvc)
	userHandler := handler.NewUserHandler(userRepo, identityRepo)
	paymentHandler := handler.NewPaymentHandler(paymentSvc)
	webhookHandler := handler.NewWebhookHandler(paymentSvc, wechatAPIv3Key, webhookVerifier)

	// Public routes (rate limited)
	publicLimiter := middleware.RateLimit(ctx, 10, 20)
	engine.GET("/.well-known/jwks.json", publicLimiter, authHandler.JWKS)
	engine.POST("/auth/login", publicLimiter, authHandler.Login)
	engine.POST("/auth/refresh", publicLimiter, authHandler.RefreshToken)
	engine.POST("/auth/logout", publicLimiter, authHandler.Logout)
	// Dev-only login endpoint for the L3 e2e-ui suite. Returns 404 unless
	// PAYPAL_L3_E2E_MODE=1 is set; the env check is inside the handler.
	engine.POST("/test/login", authHandler.TestLogin)

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
