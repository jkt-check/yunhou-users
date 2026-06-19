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
) {
	// Health check
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	// Handlers
	authHandler := handler.NewAuthHandler(authSvc, tokenSvc)
	appHandler := handler.NewAppHandler(appRepo)
	subHandler := handler.NewSubscriptionHandler(subSvc)
	planHandler := handler.NewPlanHandler(planSvc)
	userHandler := handler.NewUserHandler(userRepo, identityRepo, subRepo)

	// Public routes (rate limited)
	publicLimiter := middleware.RateLimit(ctx, 10, 20)
	engine.GET("/.well-known/jwks.json", publicLimiter, authHandler.JWKS)
	engine.POST("/auth/login", publicLimiter, authHandler.Login)
	engine.POST("/auth/refresh", publicLimiter, authHandler.RefreshToken)
	engine.POST("/auth/logout", publicLimiter, authHandler.Logout)

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
}
