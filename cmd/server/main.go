package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

func main() {
	_ = godotenv.Load()

	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config validation failed: %v", err)
	}

	db, err := sqlx.Connect("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	// Docker HEALTHCHECK calls the binary with `-healthcheck`. We handle it
	// here, after DB is ready, and exit before the HTTP server starts.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		runHealthcheck(db)
	}

	// Repos
	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	orderRepo := repo.NewOrderRepo(db)
	paymentRepo := repo.NewPaymentRepo(db)
	refundRepo := repo.NewRefundRepo(db)
	webhookEventRepo := repo.NewWebhookEventRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)

	// Services
	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		log.Fatalf("failed to initialize token service: %v", err)
	}

	planSvc := service.NewPlanService(planRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	// Payment service. Channel refund API is wired in v2 (real Stripe/WeChat/Alipay
	// HTTP clients); v1's noChannelRefundAPI stub returns an error so any
	// call surfaces immediately rather than silently no-op'ing.
	paymentSvc := service.NewPaymentService(
		db,
		orderRepo, paymentRepo, refundRepo,
		subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo,
		&noChannelRefundAPI{},
		cfg.OrderExpiryDuration,
	)

	// Webhook signature verifier. Each channel is optional — empty secret
	// means that channel returns 404 (not configured).
	webhookVerifier := buildWebhookVerifier(cfg)

	// Order expiry sweeper (in-process goroutine).
	sweeper := service.NewOrderSweeper(orderRepo, cfg.SweeperInterval)

	engine := gin.New()
	engine.Use(gin.Recovery())
	// Bound how long any handler can run before the client disconnects, to
	// limit the blast radius of a slow downstream call (e.g. the OAuth
	// provider timeout is 10s; we leave a little headroom here).
	engine.Use(timeoutMiddleware(20 * time.Second))

	// A cancelable context so the rate-limiter cleanup goroutines and the
	// sweeper exit on shutdown.
	rootCtx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sweeper.Start(rootCtx)

	router.Setup(rootCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, webhookVerifier, []byte(cfg.WeChatAPIv3Key))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       25 * time.Second,
		WriteTimeout:      25 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run the server in a goroutine so we can wait for SIGINT/SIGTERM
	// in this one and shut down gracefully.
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("starting server on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("server failed: %v", err)
		}
	case <-rootCtx.Done():
		log.Printf("shutdown signal received, draining...")
		sweeper.Stop()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}

// buildWebhookVerifier assembles the multi-channel signature verifier from
// config. Each channel is optional — if the corresponding secret is empty,
// that channel is unreachable (returns 404 via ErrUnsupportedChannel).
//
// Alipay's public key is loaded from ALIPAY_PUBLIC_KEY_PATH. A missing file
// is treated as "Alipay not configured" — same effect as empty secret.
func buildWebhookVerifier(cfg *config.Config) middleware.ChannelSignatureVerifier {
	mv := &middleware.MultiChannelVerifier{}

	if cfg.StripeWebhookSecret != "" {
		mv.Stripe = &middleware.StripeVerifier{Secret: []byte(cfg.StripeWebhookSecret)}
	}
	if cfg.WeChatAPIv3Key != "" {
		mv.WeChat = &middleware.WeChatPayV3Verifier{APIv3Key: []byte(cfg.WeChatAPIv3Key)}
	}
	if cfg.AlipayPublicKeyPath != "" {
		if pemBytes, err := os.ReadFile(cfg.AlipayPublicKeyPath); err == nil {
			if pub, err := middleware.LoadAlipayPublicKeyFromPEM(pemBytes); err == nil {
				mv.Alipay = &middleware.AlipayVerifier{PublicKey: pub}
			} else {
				log.Printf("alipay: failed to parse public key: %v", err)
			}
		} else {
			log.Printf("alipay: could not read public key file: %v", err)
		}
	}
	if cfg.LemonSqueezyWebhookSecret != "" {
		mv.LemonSqueezy = &middleware.LemonsqueezyVerifier{Secret: []byte(cfg.LemonSqueezyWebhookSecret)}
	}
	if cfg.PaypalEnv == "sandbox" || cfg.PaypalEnv == "live" {
		mv.Paypal = &middleware.PaypalVerifier{
			HTTPClient:       &http.Client{Timeout: 5 * time.Second},
			SandboxWebhookID: cfg.PaypalWebhookIDSandbox,
			LiveWebhookID:    cfg.PaypalWebhookIDLive,
			SandboxAPIBase:   cfg.PaypalAPIBaseSandbox,
			LiveAPIBase:      cfg.PaypalAPIBaseLive,
			Env:              cfg.PaypalEnv,
		}
	} else if cfg.PaypalEnv != "" {
		log.Printf("paypal: PAYPAL_ENV=%q is not sandbox|live, channel will return 404", cfg.PaypalEnv)
	}
	return mv
}

// noChannelRefundAPI is the v1 stub for RefundAPI. Real Stripe/WeChat/Alipay
// HTTP clients land in v2; v1 returns an error so any caller hits a clear
// 502 instead of silently no-op'ing.
type noChannelRefundAPI struct{}

func (noChannelRefundAPI) Refund(_ context.Context, _, _ string, _ float64, _ string) (string, error) {
	return "", errors.New("channel refund API not wired in v1")
}

// timeoutMiddleware caps each request's total wall-clock time. Without it
// a slow upstream (e.g. provider userinfo) can hold a goroutine past
// proxy_read_timeout and result in a partial response.
func timeoutMiddleware(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
