package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

func main() {
	_ = godotenv.Load()

	cfg := config.Load()

	db, err := sqlx.Connect("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		log.Fatalf("failed to initialize token service: %v", err)
	}

	authSvc := service.NewAuthService(userRepo, identityRepo, appRepo, subRepo, sessionRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo)
	oauth := service.NewOAuthProvider(cfg)

	engine := gin.Default()
	router.Setup(engine, appRepo, userRepo, identityRepo, subRepo, sessionRepo, tokenSvc, authSvc, subSvc, oauth)

	log.Printf("starting server on :%s", cfg.Port)
	if err := engine.Run(":" + cfg.Port); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
