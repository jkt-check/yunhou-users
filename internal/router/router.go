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
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *service.TokenService,
	authSvc *service.AuthService,
	subSvc *service.SubscriptionService,
	oauth *service.OAuthProvider,
	stateHMACKey string,
) {
	// Health check — registered before the public rate limit so monitors
	// are never throttled.
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	authHandler := handler.NewAuthHandler(authSvc, tokenSvc, oauth, stateHMACKey)
	userHandler := handler.NewUserHandler(userRepo, identityRepo, subRepo)
	appHandler := handler.NewAppHandler(appRepo, subRepo, subSvc)

	// Public routes (rate limited)
	publicLimiter := middleware.RateLimit(ctx, 10, 20)
	engine.GET("/.well-known/jwks.json", publicLimiter, authHandler.JWKS)
	engine.GET("/authorize", publicLimiter, authHandler.Authorize)
	engine.GET("/callback/:provider", publicLimiter, authHandler.Callback)
	engine.POST("/token", publicLimiter, authHandler.ExchangeToken)
	engine.POST("/token/refresh", publicLimiter, authHandler.RefreshToken)

	// User routes (JWT auth required)
	userGroup := engine.Group("/user")
	userGroup.Use(middleware.JWTAuth(tokenSvc))
	{
		userGroup.GET("/profile", userHandler.GetProfile)
		userGroup.PATCH("/profile", userHandler.UpdateProfile)
		userGroup.GET("/identities", userHandler.ListIdentities)
		userGroup.DELETE("/identities/:id", userHandler.UnbindIdentity)
		userGroup.GET("/apps", userHandler.ListApps)
	}

	// App management routes (app_id + app_secret auth required)
	appLimiter := middleware.RateLimit(ctx, 30, 60)
	appGroup := engine.Group("")
	appGroup.Use(appLimiter, middleware.AppAuth(appRepo))
	{
		appGroup.POST("/apps", appHandler.CreateApp)
		appGroup.GET("/apps/:id", appHandler.GetApp)
		appGroup.PATCH("/apps/:id", appHandler.UpdateApp)
		appGroup.POST("/subscriptions", appHandler.CreateSubscription)
		appGroup.GET("/subscriptions/:id", appHandler.GetSubscription)
		appGroup.DELETE("/subscriptions/:id", appHandler.CancelSubscription)
	}
}
