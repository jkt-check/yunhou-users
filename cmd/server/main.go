package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/yunhou/users/internal/billing/paypal"
	"github.com/yunhou/users/internal/billing/wechat"
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

	// WeChat Pay client: real mode loads cert + key from disk and builds a
	// Signer + Client. Mock mode skips both file loads and returns a
	// stub Client that mints deterministic code_urls. Real-mode production
	// deployments must have all 6 WECHAT_PAY_* envs set (gated by
	// config.Validate); dev/mock environments with WECHAT_PAY_MOCK=1 get
	// a non-functional mock client.
	//
	// Declared as the wechat.ClientIface interface type (not *wechat.Client)
	// so an untyped `= nil` assignment produces a true zero-value interface.
	// A *wechat.Client(nil) wrapped in an interface field is a typed-nil
	// (interface holds type=*Client, value=nil), which fails the
	// `s.wechat != nil` guard in PaymentService.CreateOrder and panics
	// when the pre-auth path calls IsMockMode() on a nil receiver.
	var wechatClient wechat.ClientIface
	var wechatHTTPDoer wechat.HTTPDoer
	var wechatSigner *wechat.Signer
	if cfg.WeChatPayMock {
		wechatClient = &wechat.Client{MockMode: true}
	} else if cfg.WeChatPayMchPrivateKeyPath != "" {
		pk, err := wechat.LoadPrivateKey(cfg.WeChatPayMchPrivateKeyPath)
		if err != nil {
			log.Fatalf("wechat: load private key %q: %v", cfg.WeChatPayMchPrivateKeyPath, err)
		}
		serial, err := wechat.LoadCertSerial(cfg.WeChatPayMchCertPath)
		if err != nil {
			log.Fatalf("wechat: load cert %q: %v", cfg.WeChatPayMchCertPath, err)
		}
		wechatHTTPDoer = newWechatHTTPAdapter(10 * time.Second)
		wechatSigner = &wechat.Signer{MchID: cfg.WeChatPayMchID, SerialNo: serial, PrivateKey: pk}
		wechatClient = &wechat.Client{
			MockMode:   false,
			Signer:     wechatSigner,
			AppIDValue: cfg.WeChatPayAppID,
			NotifyURL:  cfg.WeChatPayNotifyURL,
			BaseURL:    "https://api.mch.weixin.qq.com",
			HTTPDoer:   wechatHTTPDoer,
		}
	} else {
		// Real mode + no private key path = the deployment chose not to
		// enable WeChat Pay at all (wechat endpoints return 404). Untyped
		// nil keeps the interface == nil so the service guard sees it.
		wechatClient = nil
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
	planChangeLogRepo := repo.NewPlanChangeLogRepo(db)
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

	planSvc := service.NewPlanService(planRepo, appRepo, planChangeLogRepo)
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
		wechatClient,
		cfg.OrderExpiryDuration,
	)

	// Validate PayPal environment BEFORE building anything that depends on it.
	// config.PaypalEnv 默认 ""（cn 域不启用 PayPal）。空值 = 未启用：webhook
	// verifier 会构建 nil verifier（渠道返回 404），这里给 provider-token
	// client 一个 live 占位 BaseURL（不会真正发请求，因为渠道未配置）。
	// 非空但既非 sandbox 又非 live = 拼写错误，响亮崩溃。
	paypalMode := paypal.ModeLive
	if cfg.PaypalEnv != "" {
		paypalMode = paypal.Mode(cfg.PaypalEnv)
		if paypalMode != paypal.ModeSandbox && paypalMode != paypal.ModeLive {
			log.Fatalf("paypal: PAYPAL_ENV=%q is invalid; must be sandbox or live", cfg.PaypalEnv)
		}
	}

	// Webhook signature verifier. Each channel is optional — empty secret
	// means that channel returns 404 (not configured).
	webhookVerifier := buildWebhookVerifier(cfg, wechatSigner, wechatHTTPDoer)

	// Provider-token plumbing. PayPal's base URL tracks PAYPAL_ENV so the
	// /apps/:id/provider-token/paypal endpoint hits the same environment as
	// the webhook verifier (api-m.sandbox.paypal.com for sandbox,
	// api-m.paypal.com for live). The cache collapses repeat fetches within
	// the same client_id's TTL window. LS is webhook-only in Yunhou — no
	// outbound HTTP, the service reads the api_key directly from
	// apps.config.payment_providers.lemonsqueezy.
	paypalHTTPClient := &http.Client{Timeout: 5 * time.Second}
	paypalOAuth := paypal.NewOAuthClient(paypalHTTPClient, paypalMode.BaseURL())
	paypalCache := paypal.NewTokenCache(60 * time.Second)
	paypalFetcher := paypal.NewCachedClient(paypalOAuth, paypalCache)
	providerTokenSvc := service.NewProviderTokenService(appRepo, paypalFetcher)

	// Quote service — assembles price + cycle + provider_data for BFF checkout.
	quoteSvc := service.NewQuoteService(planRepo, appRepo)

	// Chat proxy — server-side DeepSeek key; empty key = /chat returns 404.
	chatSvc := service.NewChatService(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel, subRepo, planRepo)

	// Chat access audit log: one JSON line per request (user_id, session_id,
	// input, output, status, duration). Optional — empty CHAT_LOG_PATH
	// disables it. Fail-fast when configured but unopenable: silently
	// dropping audit lines is worse than refusing to start.
	// 0o600: the file holds full user conversation content (PII); same-OS
	// users must not read it.
	var chatAccessLog *log.Logger
	if cfg.ChatLogPath != "" {
		f, err := os.OpenFile(cfg.ChatLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("open chat log file %s: %v", cfg.ChatLogPath, err)
		}
		chatAccessLog = log.New(f, "", 0) // no prefix/ts — the JSON line carries its own ts
	}

	// Order expiry sweeper (in-process goroutine).
	sweeper := service.NewOrderSweeper(orderRepo, cfg.SweeperInterval)

	// One-shot secret backfill for rows created before migration 007_app_secret
	// added the secret_hash column. Idempotent — once every row has a hash,
	// subsequent restarts are no-ops. Plaintext is logged exactly once per row
	// to stdout; capture it from the deploy log and rotate via the dedicated
	// endpoint so the backfill secret never lives past the next deploy.
	if n, err := service.BackfillAppSecrets(context.Background(), appRepo); err != nil {
		log.Printf("app secret backfill error: %v (continuing startup)", err)
	} else if n > 0 {
		log.Printf("app secret backfill: %d row(s) initialised — capture plaintexts from the lines above and rotate via POST /admin/apps/:id/rotate-secret", n)
	}

	engine := gin.New()
	engine.Use(gin.Recovery())
	// Pin trusted proxies: gin's default trusts EVERY proxy, so ClientIP()
	// (which keys all rate-limit buckets, including the paid-upstream /chat
	// bucket) would take a client-supplied X-Forwarded-For — rotating XFF
	// bypasses the limiter. Trust only loopback and private ranges: the app
	// sits behind nginx on the same host (or the docker bridge gateway when
	// containerized), so legitimate peers are always private, and an
	// external client's injected XFF is ignored.
	if err := engine.SetTrustedProxies([]string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}); err != nil {
		log.Fatalf("set trusted proxies: %v", err)
	}
	// Bound how long any handler can run before the client disconnects, to
	// limit the blast radius of a slow downstream call (e.g. the OAuth
	// provider timeout is 10s; we leave a little headroom here).
	// /chat is exempt: it relays an upstream SSE stream whose legitimate
	// lifetime exceeds 20s. Its own safety net is chatUpstreamTimeout
	// (5m, inside ChatService) plus a per-response write deadline set by
	// the chat handler (the server-wide WriteTimeout below is an absolute
	// per-request deadline — it would hard-cut a longer stream).
	engine.Use(timeoutMiddleware(20*time.Second, "/chat"))

	// A cancelable context so the rate-limiter cleanup goroutines and the
	// sweeper exit on shutdown.
	rootCtx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sweeper.Start(rootCtx)

	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)

	router.Setup(rootCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, webhookVerifier, []byte(cfg.WeChatAPIv3Key),
		providerTokenSvc, quoteSvc, chatSvc, chatAccessLog, githubOAuthSvc, wechatOAuthSvc,
		cfg.WeChatOAuthMock, cfg.WeChatPayMock)

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
func buildWebhookVerifier(cfg *config.Config, wechatSigner *wechat.Signer, wechatDoer wechat.HTTPDoer) middleware.ChannelSignatureVerifier {
	mv := &middleware.MultiChannelVerifier{}

	if cfg.StripeWebhookSecret != "" {
		mv.Stripe = &middleware.StripeVerifier{Secret: []byte(cfg.StripeWebhookSecret)}
	}
	// WeChat verifier wires whenever EITHER a real API v3 key is present
	// OR mock mode is enabled. The previous `cfg.WeChatAPIv3Key != ""`
	// guard was the BLOCKER 1 from the independent review: cn-staging
	// runs with WECHAT_PAY_MOCK=1 and an empty WECHAT_PAY_API_V3_KEY
	// (mock doesn't need a real key), so the guard left mv.WeChat nil
	// and the middleware returned ErrUnsupportedChannel (404) on
	// every inbound POST. The mock verifier doesn't need the key
	// (it short-circuits on header presence), so building it with
	// an empty key in mock mode is safe. Real mode also requires a
	// platform-key source (signer+doer) for the new RSA verify path;
	// without it the verifier returns transient 500 on every call.
	if cfg.WeChatAPIv3Key != "" || cfg.WeChatPayMock {
		verifier := &middleware.WeChatPayV3Verifier{
			APIv3Key: []byte(cfg.WeChatAPIv3Key),
			MockMode: cfg.WeChatPayMock,
		}
		if wechatSigner != nil && wechatDoer != nil && cfg.WeChatAPIv3Key != "" {
			verifier.PlatformKeys = &wechat.PlatformCertManager{
				Signer:   wechatSigner,
				APIv3Key: []byte(cfg.WeChatAPIv3Key),
				BaseURL:  "https://api.mch.weixin.qq.com",
				HTTPDoer: wechatDoer,
			}
		}
		mv.WeChat = verifier
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
//
// Routes listed in skipPaths are exempt — used for /chat, whose upstream SSE
// relay legitimately outlives the generic cap (its own bounds live in
// ChatService.chatUpstreamTimeout).
func timeoutMiddleware(d time.Duration, skipPaths ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range skipPaths {
			if c.FullPath() == p {
				c.Next()
				return
			}
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// httpDoerAdapter wraps a real *http.Client in the wechat.HTTPDoer
// interface. Used in production only — tests inject their own stub.
// The ctx is propagated into http.NewRequestWithContext so a Gin
// request timeout (timeoutMiddleware) cancels the in-flight WeChat
// outbound call at the same lifecycle boundary.
type httpDoerAdapter struct{ c *http.Client }

func (a *httpDoerAdapter) Do(ctx context.Context, req *wechat.HTTPRequest) (*wechat.HTTPResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	httpResp, err := a.c.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	return &wechat.HTTPResponse{StatusCode: httpResp.StatusCode, Body: body}, nil
}

func newWechatHTTPAdapter(timeout time.Duration) wechat.HTTPDoer {
	return &httpDoerAdapter{c: &http.Client{Timeout: timeout}}
}
