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
	inferenceaccess "github.com/yunhou/users/internal/inference/access"
	inferenceaccounting "github.com/yunhou/users/internal/inference/accounting"
	inferencecatalog "github.com/yunhou/users/internal/inference/catalog"
	inferencecredentials "github.com/yunhou/users/internal/inference/credentials"
	inferencedomain "github.com/yunhou/users/internal/inference/domain"
	inferencegateway "github.com/yunhou/users/internal/inference/gateway"
	inferencehttpapi "github.com/yunhou/users/internal/inference/httpapi"
	inferencemanagement "github.com/yunhou/users/internal/inference/management"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	inferenceproviders "github.com/yunhou/users/internal/inference/providers"
	inferenceconnector "github.com/yunhou/users/internal/inference/providers/connector"
	inferencequota "github.com/yunhou/users/internal/inference/quota"
	inferencerouting "github.com/yunhou/users/internal/inference/routing"
	inferenceworkers "github.com/yunhou/users/internal/inference/workers"
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

	// Provider-token plumbing. PayPal's base URL tracks PAYPAL_ENV so the
	// /apps/:id/provider-token/paypal endpoint hits the same environment as
	// the webhook verifier (api-m.sandbox.paypal.com for sandbox,
	// api-m.paypal.com for live). The cache collapses repeat fetches within
	// the same client_id's TTL window. LS is webhook-only in Yunhou — no
	// outbound HTTP, the service reads the api_key directly from
	// apps.config.payment_providers.lemonsqueezy.
	//
	// Built BEFORE buildWebhookVerifier: the PayPal webhook verifier needs
	// the same cached fetcher — verify-webhook-signature 401s without a
	// Bearer token (2026-08-17 incident: all genuine deliveries 500'd,
	// orders stuck pending until the sweeper expired them).
	paypalHTTPClient := &http.Client{Timeout: 5 * time.Second}
	paypalOAuth := paypal.NewOAuthClient(paypalHTTPClient, paypalMode.BaseURL())
	paypalCache := paypal.NewTokenCache(60 * time.Second)
	paypalFetcher := paypal.NewCachedClient(paypalOAuth, paypalCache)
	providerTokenSvc := service.NewProviderTokenService(appRepo, paypalFetcher)

	// Webhook signature verifier. Each channel is optional — empty secret
	// means that channel returns 404 (not configured).
	webhookVerifier := buildWebhookVerifier(cfg, wechatSigner, wechatHTTPDoer, paypalFetcher)

	// Quote service — assembles price + cycle + provider_data for BFF checkout.
	quoteSvc := service.NewQuoteService(planRepo, appRepo)

	// Chat proxy — server-side DeepSeek key; empty key = /chat returns 404.
	chatSvc := service.NewChatService(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel, subRepo, planRepo)

	// Usage analytics: heartbeat intake + admin stats reads over
	// usage_events (migration 021).
	usageRepo := repo.NewUsageRepo(db)
	usageSvc := service.NewUsageService(usageRepo)

	// Inference model catalog (Kaya Coding Plan Task 3): draft CRUD with
	// optimistic locking, atomic publish/rollback and immutable snapshots
	// over migration 024. The LLM_PROVIDERS_JSON import is explicit and
	// idempotent: it inserts only what is missing and never overwrites
	// DB-operational config on restart (基线报告差距 1).
	infStore := inferencepostgres.NewStore(db)
	catalogSvc := inferencecatalog.NewService(infStore)

	// Task 10: payment → entitlement closed loop. The benefit repo gates
	// coding-plan purchasability (no plan_benefit_configs row = not
	// purchasable, 设计 §4.3) and the outbox enqueue rides the payment
	// transaction so a state flip and its entitlement-sync message commit
	// or roll back together.
	benefitRepo := repo.NewPlanBenefitRepo(db)
	paymentSvc.SetBenefitRepo(benefitRepo)
	paymentSvc.SetBenefitSync(infStore)
	subSvc.SetBenefitSync(db, infStore)
	quoteSvc.SetBenefitRepo(benefitRepo)
	catalogCache := inferencecatalog.NewSnapshotCache(infStore, func(err error) {
		log.Printf("WARN inference catalog snapshot refresh failed; continuing on last verified snapshot: %v", err)
	})

	// Task 4: upstream credential vault + egress guard + operator authz.
	// Key material comes from the deployment secret (INFERENCE_CREDENTIAL_KEYS);
	// an empty value leaves the vault nil and every credential operation
	// fails closed. The egress validator enforces the SSRF policy on
	// deployment base URLs at write time.
	var credVault *inferencecredentials.Vault
	if cfg.InferenceCredentialKeys != "" {
		keys, current, err := inferencecredentials.ParseKeysEnv(cfg.InferenceCredentialKeys)
		if err != nil {
			log.Fatalf("INFERENCE_CREDENTIAL_KEYS: %v", err)
		}
		credVault, err = inferencecredentials.NewVault(keys, current)
		if err != nil {
			log.Fatalf("credential vault: %v", err)
		}
		log.Printf("credential vault: %d key version(s) loaded, current=v%d", len(keys), current)
	}
	egressValidator, err := inferencecredentials.NewEgressValidator(cfg.InferenceUpstreamAllowlist)
	if err != nil {
		log.Fatalf("INFERENCE_UPSTREAM_ALLOWLIST: %v", err)
	}
	if len(cfg.InferenceUpstreamAllowlist) > 0 {
		log.Printf("egress policy: %d internal target(s) allowlisted", len(cfg.InferenceUpstreamAllowlist))
	}
	credSvc := inferencecredentials.NewService(credVault, infStore, infStore)
	catalogMgr := inferencemanagement.NewCatalogManager(catalogSvc, infStore, egressValidator.ValidateURL)
	adminModelsHandler := inferencehttpapi.NewAdminModelsHandler(catalogMgr)

	// Task 12: upstream OAuth connector registry + authorization/refresh
	// services. The registry comes from the deployment secret env
	// (INFERENCE_OAUTH_CONNECTORS_JSON); empty registry = authorization
	// endpoints reject every connector key as unknown (fail closed). The
	// connector HTTP client reuses the egress (SSRF) policy — vendor
	// endpoints are outbound targets like any other upstream. This OAuth
	// flow is fully separate from social login (GitHub/WeChat), by design.
	oauthRegistry, err := inferenceconnector.ParseRegistry(cfg.InferenceOAuthConnectorsJSON)
	if err != nil {
		log.Fatalf("INFERENCE_OAUTH_CONNECTORS_JSON: %v", err)
	}
	connectorClient := &inferenceconnector.Client{HTTP: inferenceproviders.NewHTTPClient(egressValidator)}
	oauthSvc := inferencecredentials.NewOAuthService(credVault, infStore, infStore, connectorClient, oauthRegistry, credSvc, nil)
	credRefresher := inferencecredentials.NewRefresher(credVault, infStore, infStore, connectorClient, oauthRegistry, nil)

	adminOps := &inferencehttpapi.AdminOps{
		RequireModels:      inferencehttpapi.OperatorAuthz(infStore, inferencemanagement.PermModelsManage),
		RequireCredentials: inferencehttpapi.OperatorAuthz(infStore, inferencemanagement.PermCredentialsManage),
		RequireAdmin:       inferencehttpapi.OperatorRequireRole(infStore, inferencemanagement.RoleAdmin),
		// Task 14: 钱包运营面（调整/冲正/PAYG 发布配置）走 billing:adjust。
		RequireBilling: inferencehttpapi.OperatorAuthz(infStore, inferencemanagement.PermBillingAdjust),
		Models:         adminModelsHandler,
		Credentials:    inferencehttpapi.NewAdminCredentialsHandler(credSvc),
		Auth:           inferencehttpapi.NewAdminAuthHandler(infStore, infStore),
		OAuth:          inferencehttpapi.NewAdminOAuthHandler(oauthSvc, credRefresher, infStore),
		Adjustments:    inferencehttpapi.NewAdminAdjustmentsHandler(infStore, nil),
	}

	// Task 5: customer API keys + caller principal resolution. The
	// resolver authenticates /v1/* keys straight from the store on every
	// call (revocation/expiry take effect immediately); the key service
	// backs /user/api-keys with ownership bound to the JWT identity.
	accessResolver := inferenceaccess.NewResolver(infStore, nil)
	keySvc := inferenceaccess.NewKeyService(infStore, nil)
	rpmCounter := inferenceaccess.NewRPMCounter(nil)

	// Task 8: the inference gateway — adapters, routing (account pool +
	// concurrency leases), quota admission, and the orchestration service
	// that pins ONE catalog snapshot per call (设计 §5: catalogCache 按请求
	// pin；发布原子切换在各进程有界延迟内生效).
	adapters := map[inferencedomain.Protocol]inferenceproviders.Adapter{
		inferencedomain.ProtocolOpenAIChat:       inferenceproviders.NewOpenAIChat(),
		inferencedomain.ProtocolAnthropicMessage: inferenceproviders.NewAnthropicMessages(),
	}
	routingSvc := inferencerouting.NewService(infStore, adapters, nil)
	quotaSvc := inferencequota.NewService(infStore, nil)
	entitlementResolver := inferenceaccess.NewEntitlementResolver(infStore, nil)
	gatewayHTTPClient := inferenceproviders.NewHTTPClient(egressValidator)
	gatewaySvc := inferencegateway.NewService(
		catalogCache, infStore, entitlementResolver, quotaSvc, routingSvc,
		credSvc, gatewayHTTPClient, egressValidator, nil)
	// Task 13: sticky-session binder wiring (Responses 会话链钉住上游账号;
	// 失效显式迁移,绝不静默换号 — Task 12 binder 语义).
	gatewaySvc.SetSessionBinder(inferencerouting.NewSessionBinder(infStore, nil))

	// Sellable gate for the published-model listings (/v1/models and
	// /chat/models): a model without an effective sale-credit price version
	// is NOT sellable (新模型默认不可售; deny by default).
	catalogSvc.SetPriceCheck(func(ctx context.Context, modelID string) (bool, error) {
		_, err := infStore.LatestPriceVersion(ctx, modelID, string(inferenceaccounting.PriceSaleCredit), time.Now())
		if err != nil {
			if inferencedomain.CodeOf(err) == inferencedomain.CodeNotFound {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})

	accessOps := &inferencehttpapi.AccessOps{
		UserAPIKeys:       inferencehttpapi.NewUserAPIKeysHandler(keySvc),
		V1Auth:            inferencehttpapi.APIKeyAuth(accessResolver, rpmCounter, cfg.InferenceAccountRPM),
		RPMCounter:        rpmCounter,
		V1Models:          inferencehttpapi.NewModelsHandler(catalogSvc, accessResolver),
		V1ChatCompletions: inferencehttpapi.NewChatCompletionsHandler(gatewaySvc),
		// Task 13: Anthropic Messages / OpenAI Responses 编程工具面（与 chat
		// 面共用同一 principal/预占/结算链；Responses 会话链落库 + 粘性会话
		// 绑定经 routing.SessionBinding）。
		V1Messages:  inferencehttpapi.NewMessagesHandler(gatewaySvc),
		V1Responses: inferencehttpapi.NewResponsesHandler(gatewaySvc, infStore, nil),
		// Task 11: customer quota/usage/subscription read views over the
		// inference store (quota reads are the authoritative current state;
		// usage reads carry as_of/complete_through).
		UserQuotas:        inferencehttpapi.NewUserQuotasHandler(inferencemanagement.NewQuotaViewService(infStore, nil)),
		UserUsage:         inferencehttpapi.NewUserUsageHandler(inferencemanagement.NewUsageViewService(infStore, nil)),
		UserSubscriptions: inferencehttpapi.NewUserSubscriptionsHandler(inferencemanagement.NewSubscriptionViewService(infStore, nil)),
		// Task 14: 客户钱包面（派生余额/流水/套餐外开关/PAYG 开启）。
		UserWallet: inferencehttpapi.NewUserWalletHandler(infStore, nil),
	}
	// /chat 迁移开关（默认关闭 = 旧 DeepSeek 直通）: 开启时 POST /chat 与
	// GET /chat/models 由网关 facade 承接，JWT/错误 shape/审计 relay 不变。
	if cfg.InferenceKayaChatGateway {
		accessOps.KayaChat = service.NewChatGatewayFacade(gatewaySvc, accessResolver, cfg.KayaChatModel)
		accessOps.KayaChatModels = inferencehttpapi.NewKayaModelsHandler(catalogSvc, accessResolver, cfg.KayaChatModel)
		log.Printf("kaya /chat gateway facade enabled (default model %s)", cfg.KayaChatModel)
	}
	if cfg.LLMProvidersJSON != "" {
		res, err := catalogSvc.ImportEnvCatalog(context.Background(), cfg.LLMProvidersJSON)
		if err != nil {
			log.Fatalf("LLM_PROVIDERS_JSON import failed: %v", err)
		}
		log.Printf("LLM_PROVIDERS_JSON import: +%d providers, +%d models, +%d deployments, +%d routes, %d already present (skipped)",
			res.ProvidersInserted, res.ModelsInserted, res.DeploymentsInserted, res.RoutesInserted, res.Skipped)
	}

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

	// Order expiry sweeper (in-process goroutine). Also marks naturally
	// lapsed entitlements 'expired' on the same cadence (Task 10).
	sweeper := service.NewOrderSweeper(orderRepo, cfg.SweeperInterval)
	sweeper.SetEntitlementExpirer(infStore)

	// One-shot secret backfill for rows created before migration 007_app_secret
	// added the secret_hash column. Idempotent — once every row has a hash,
	// subsequent restarts are no-ops. The plaintext is NEVER logged (see
	// service.BackfillAppSecrets); operators must rotate via the dedicated
	// endpoint to obtain each app's new secret.
	if n, err := service.BackfillAppSecrets(context.Background(), appRepo); err != nil {
		log.Printf("app secret backfill error: %v (continuing startup)", err)
	} else if n > 0 {
		log.Printf("app secret backfill: %d row(s) initialised — rotate each via POST /admin/apps/:id/rotate-secret to obtain the new plaintext", n)
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
	// /chat and /v1/chat/completions are exempt: both relay upstream SSE
	// streams whose legitimate lifetime exceeds 20s. Their own safety nets
	// are per-attempt deployment request timeouts (gateway, Task 8) /
	// chatUpstreamTimeout (legacy /chat) plus per-response write deadlines
	// set by the handlers (the server-wide WriteTimeout below is an absolute
	// per-request deadline — it would hard-cut a longer stream).
	// Task 13: /v1/messages and /v1/responses get the same exemption (same
	// SSE relay pattern, same per-response write deadline in the handlers).
	engine.Use(timeoutMiddleware(20*time.Second, "/chat", "/v1/chat/completions", "/v1/messages", "/v1/responses"))

	// Global request-body cap — defence in depth behind nginx's
	// client_max_body_size. Any direct-to-Go exposure (alternate ingress,
	// misconfigured proxy) would otherwise let ShouldBindJSON handlers and
	// decodeAdminPlanRequest read unbounded bodies into memory. /chat keeps
	// its own tighter cap (320 KiB) inside the handler and webhooks cap at
	// 1 MiB in the signature middleware; this outer 1 MiB matches both.
	engine.Use(maxRequestBodyBytes(1 << 20))

	// Security headers at the app layer. nginx sets the same trio, but any
	// path that bypasses nginx (direct container port, health probes) must
	// not lose them.
	engine.Use(securityHeaders())

	// A cancelable context so the rate-limiter cleanup goroutines and the
	// sweeper exit on shutdown.
	rootCtx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sweeper.Start(rootCtx)

	// Task 9: settlement recovery worker — crash recovery for stranded
	// in-flight requests (conservative-estimate settlement = reserved hold,
	// reconciliation queue, deadline escalation, ledger/window rebuild
	// check). Never zeroes unknown usage, never releases by TTL alone, never
	// double-charges (guarded transitions + unique keys). The verifier is
	// nil: mainstream Chat/Messages upstreams have no per-request execution
	// query API (设计 §7.2 补充段), so recovery always estimates.
	recoveryWorker := inferenceworkers.NewSettlementRecovery(infStore, nil, inferenceworkers.RecoveryConfig{
		Interval:               cfg.InferenceRecoveryInterval,
		BatchLimit:             cfg.InferenceRecoveryBatch,
		Grace:                  cfg.InferenceRecoveryGrace,
		ReconciliationDeadline: cfg.InferenceReconciliationDeadline,
	}, nil)
	go recoveryWorker.Start(rootCtx)

	// Task 10: entitlement sync worker — consumes the inference_outbox
	// messages the payment pipeline enqueues in the SAME transaction as the
	// payment state flip, and converges entitlements to the state demanded
	// by (subscription, paying order snapshot). Idempotent and order-safe
	// by construction (access.DecideSync/Converge).
	entitlementSyncWorker := inferenceworkers.NewEntitlementSync(infStore, nil, inferenceworkers.EntitlementSyncConfig{
		Interval:   cfg.InferenceEntitlementSyncInterval,
		BatchLimit: cfg.InferenceEntitlementSyncBatch,
	})
	go entitlementSyncWorker.Start(rootCtx)

	// Task 14: wallet sync worker — consumes wallet.sync outbox messages
	// (余额充值入账/现金退款), idempotent by business key (重复回调只生效
	// 一次); reuses the entitlement-sync tuning knobs.
	walletSyncWorker := inferenceworkers.NewWalletSync(infStore, nil, inferenceworkers.EntitlementSyncConfig{
		Interval:   cfg.InferenceEntitlementSyncInterval,
		BatchLimit: cfg.InferenceEntitlementSyncBatch,
	})
	go walletSyncWorker.Start(rootCtx)

	// Task 12: OAuth credential refresh + upstream health workers. The
	// refresh worker rotates expiring oauth credentials under a cross-
	// instance advisory lock + generation CAS (双实例同时刷新安全：输家收敛
	// 不覆盖); vendor-side invalid_grant flips bound accounts to
	// reauth_required and ends their session bindings in one transaction.
	// The health worker probes accounts, observes upstream quota snapshots
	// (unknown stays unknown — never derived from customer balances), and
	// cools down / recovers accounts on retryable failures.
	credentialRefreshWorker := inferenceworkers.NewCredentialRefresh(infStore, credRefresher, inferenceworkers.CredentialRefreshConfig{
		Interval:    cfg.InferenceCredentialRefreshInterval,
		BatchLimit:  cfg.InferenceCredentialRefreshBatch,
		RefreshSkew: cfg.InferenceCredentialRefreshSkew,
	}, nil)
	go credentialRefreshWorker.Start(rootCtx)
	upstreamHealthWorker := inferenceworkers.NewUpstreamHealth(infStore, credVault, connectorClient, credRefresher, oauthRegistry, infStore, inferenceworkers.UpstreamHealthConfig{
		Interval:   cfg.InferenceUpstreamHealthInterval,
		BatchLimit: cfg.InferenceUpstreamHealthBatch,
		Cooldown:   cfg.InferenceUpstreamHealthCooldown,
	}, nil)
	go upstreamHealthWorker.Start(rootCtx)

	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)

	router.Setup(rootCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, webhookVerifier, []byte(cfg.WeChatAPIv3Key),
		providerTokenSvc, quoteSvc, chatSvc, chatAccessLog, githubOAuthSvc, wechatOAuthSvc,
		cfg.WeChatOAuthMock, cfg.WeChatPayMock, usageSvc, adminModelsHandler, adminOps, accessOps)

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
func buildWebhookVerifier(cfg *config.Config, wechatSigner *wechat.Signer, wechatDoer wechat.HTTPDoer, paypalFetcher *paypal.CachedClient) middleware.ChannelSignatureVerifier {
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
		pv := &middleware.PaypalVerifier{
			HTTPClient:       &http.Client{Timeout: 5 * time.Second},
			SandboxWebhookID: cfg.PaypalWebhookIDSandbox,
			LiveWebhookID:    cfg.PaypalWebhookIDLive,
			SandboxAPIBase:   cfg.PaypalAPIBaseSandbox,
			LiveAPIBase:      cfg.PaypalAPIBaseLive,
			Env:              cfg.PaypalEnv,
		}
		// verify-webhook-signature requires a Bearer token. Wire the shared
		// cached fetcher with the deployment's client credentials; without
		// them every genuine delivery fails upstream auth and surfaces as a
		// 500 (PayPal retries forever, orders never activate). Warn loudly
		// rather than silently running unauthenticated.
		if cfg.PaypalClientID != "" && cfg.PaypalClientSecret != "" && paypalFetcher != nil {
			pv.TokenFunc = func() (string, error) {
				tok, err := paypalFetcher.FetchToken(context.Background(), cfg.PaypalClientID, cfg.PaypalClientSecret)
				if err != nil {
					return "", err
				}
				return tok.AccessToken, nil
			}
		} else {
			log.Printf("paypal: PAYPAL_CLIENT_ID/SECRET unset — webhook signature verification will fail upstream auth (500); set them to match PAYPAL_ENV=%s", cfg.PaypalEnv)
		}
		mv.Paypal = pv
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

// maxRequestBodyBytes caps every request body at n bytes (see engine.Use
// above for why this exists even though nginx has client_max_body_size).
func maxRequestBodyBytes(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}

// securityHeaders mirrors deploy/nginx.conf's header trio at the app layer.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Next()
	}
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
