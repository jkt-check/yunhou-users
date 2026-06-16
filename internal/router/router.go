package router

import (
	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

func Setup(
	engine *gin.Engine,
	appRepo repo.AppRepo,
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *service.TokenService,
	authSvc *service.AuthService,
	subSvc *service.SubscriptionService,
	oauth *service.OAuthProvider,
) {
	authHandler := handler.NewAuthHandler(authSvc, tokenSvc, oauth)
	userHandler := handler.NewUserHandler(userRepo, identityRepo, subRepo)
	appHandler := handler.NewAppHandler(appRepo, subRepo, subSvc)

	// Public routes
	engine.GET("/.well-known/jwks.json", authHandler.JWKS)
	engine.GET("/authorize", authHandler.Authorize)
	engine.GET("/callback/:provider", authHandler.Callback)
	engine.POST("/token", authHandler.ExchangeToken)
	engine.POST("/token/refresh", authHandler.RefreshToken)

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
	appGroup := engine.Group("")
	appGroup.Use(middleware.AppAuth(appRepo))
	{
		appGroup.POST("/apps", appHandler.CreateApp)
		appGroup.GET("/apps/:id", appHandler.GetApp)
		appGroup.PATCH("/apps/:id", appHandler.UpdateApp)
		appGroup.POST("/subscriptions", appHandler.CreateSubscription)
		appGroup.GET("/subscriptions/:id", appHandler.GetSubscription)
		appGroup.DELETE("/subscriptions/:id", appHandler.CancelSubscription)
	}
}
