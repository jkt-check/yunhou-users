package config

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/credentials"
)

// Config holds all runtime configuration. Required fields are validated
// in Validate(); Load() only reads from the environment.
type Config struct {
	Port        string
	DatabaseURL string
	RSAPrivate  string
	RSAPublic   string

	// OAuthStateSecret signs the state parameter on the GitHub OAuth
	// redirect flow (CSRF + replay + open-redirect defence). Required at
	// startup — operators who don't enable GitHub login can set it to any
	// non-empty value (the handler returns 404 when no apps carry the
	// github provider config, regardless of the secret).
	OAuthStateSecret string

	// WeChatOAuthMock short-circuits the WeChat OAuth redirect + callback
	// handlers. When true, /auth/wechat/redirect returns a redirect to
	// the BFF with code=mock-code&state=<real HMAC state> (no upstream
	// call to open.weixin.qq.com), and /auth/wechat/callback constructs
	// a fixed ProviderUserInfo (wechat_mock-unionid-001) instead of
	// exchanging the code with WeChat. Used by dev/staging environments
	// that don't have a registered 网站应用 yet, and by the e2e suite.
	// Real WeChat apps MUST leave this false.
	WeChatOAuthMock bool

	// WeChatPayMock short-circuits the WeChat Pay v3 webhook signature
	// verification + AES-GCM resource decryption. When true,
	// /webhooks/payment/wechat_pay accepts a plaintext JSON body (no
	// HMAC match required, no resource block to decrypt) so e2e suites
	// and dev environments can drive the order-paid → subscription
	// activated flow without a registered merchant. Pair with the
	// mock-mode NATIVE UnifiedOrder in internal/billing/wechat/.
	// Production MUST leave this false — Validate() hard-fails when it
	// combines with PAYPAL_ENV=live or a full set of real WeChat Pay
	// credentials.
	WeChatPayMock bool

	// PaypalL3E2EMode gates the dev-only POST /test/login endpoint (route
	// registration in router.Setup + check inside the handler). It mints
	// real JWTs for arbitrary emails with no OAuth, so Validate()
	// hard-fails when it combines with PAYPAL_ENV=live.
	PaypalL3E2EMode bool

	// WeChatPayMchID is the 微信支付商户号. Required when WeChatPayMock
	// is false (production); ignored otherwise. Per-app overrides live
	// in apps.config.payment_providers.wechat_pay.mch_id — this top-level
	// field is the server-wide fallback for deployments that haven't
	// registered multiple merchants yet.
	WeChatPayMchID string
	// WeChatPayAppID is the WeChat Open Platform 网站应用 appid — required
	// in the v3 NATIVE request body alongside `mchid`. Real mode only;
	// ignored in mock mode.
	WeChatPayAppID string
	// WeChatPayMchPrivateKeyPath is the path to the merchant's RSA
	// private key (PKCS#1 or PKCS#8 PEM). Required for the outbound
	// signing path — every native/JSAPI/etc. UnifiedOrder request is
	// signed with this key. Real mode only; ignored in mock mode.
	WeChatPayMchPrivateKeyPath string
	// WeChatPayMchCertPath is the path to the merchant's X.509
	// certificate (PEM). The cert's serial number is extracted at startup
	// (as UPPERCASE HEX, the WeChat-required serial_no format) and put in
	// the outbound Authorization header — WeChat uses it to look up the
	// merchant's public key for verifying our request signature. Real
	// mode only; ignored in mock mode.
	WeChatPayMchCertPath string
	// WeChatPayNotifyURL is the public callback URL
	// (e.g. https://host/webhooks/payment/wechat_pay) passed to
	// UnifiedOrder so WeChat knows where to POST async payment
	// notifications. Real mode only; ignored in mock mode.
	WeChatPayNotifyURL string

	// GitHubClientID/Secret are reserved for a future OAuth redirect flow.
	// They are not used by the current direct-login implementation but kept
	// in the env so operators can pre-provision credentials.
	GitHubClientID     string
	GitHubClientSecret string

	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	// Payment channel webhook secrets. Loaded but not strictly required
	// at startup — if a channel's secret is empty, webhooks for that channel
	// return 404 (signature verifier is nil for that channel). Operators
	// who don't accept a particular channel can leave its secret blank.
	// WECHAT_PAY_API_V3_KEY must be exactly 32 bytes when set (and a
	// non-empty value is required whenever WECHAT_PAY_MCH_ID is set in
	// real mode — see Validate).
	StripeWebhookSecret string
	WeChatAPIv3Key      string // 32 bytes, used for both signature + AES-GCM resource decrypt
	AlipayPublicKeyPath string

	// PayPal sandbox + live both loaded; PaypalEnv selects which is active.
	// Empty webhook ID for the active env → channel returns 404 for that env.
	PaypalEnv              string // "sandbox" | "live"
	PaypalWebhookIDSandbox string
	PaypalWebhookIDLive    string
	PaypalAPIBaseSandbox   string // default https://api-m.sandbox.paypal.com
	PaypalAPIBaseLive      string // default https://api-m.paypal.com
	// Client credentials for the webhook-signature verifier's OAuth token
	// fetch (verify-webhook-signature requires a Bearer token). Must match
	// the active PaypalEnv: sandbox pair on staging, live pair on prod.
	// Empty → verifier calls PayPal unauthenticated (legacy behavior) and
	// every delivery 401s — main.go warns loudly at startup.
	PaypalClientID     string
	PaypalClientSecret string

	// Order expiry: how long a pending order is valid before the sweeper
	// flips it to 'expired'. Default 30 min per design doc §"v1 decisions".
	OrderExpiryDuration time.Duration
	// Sweeper interval: how often the in-process goroutine runs. Default 1 min.
	SweeperInterval time.Duration

	// PlanAmountOverrideJSON is the (plan_id → amount-yuan) override map
	// parsed once at boot by internal/service/price_override.go. When
	// non-empty, QuoteService.Get() and PaymentService.CreateOrder()
	// replace plans.price with the override value at runtime — letting
	// dev/staging environments drive payment flows at "fake" amounts
	// without dirtying the canonical plans row or writing a per-stage
	// migration. Empty by default; format `{"monthly":0.01,"yearly":0.1}`.
	// Surface here only so operators see it in the loaded-config log;
	// the value itself is read by service.ReloadOverrideFromEnv().
	PlanAmountOverrideJSON string

	// DeepSeekAPIKey enables the POST /chat endpoint (ChatService). Empty =
	// chat not enabled (the route returns 404, mirroring how empty webhook
	// secrets disable their channels). The key belongs to yunhou — consumer
	// apps like kaya never see it; they call /chat with a user JWT and the
	// server proxies to DeepSeek with this key.
	DeepSeekAPIKey string
	// DeepSeekBaseURL is the OpenAI-compatible API origin. The service
	// appends /chat/completions. Default https://api.deepseek.com.
	DeepSeekBaseURL string
	// DeepSeekModel is the model name sent in the upstream chat.completions
	// body (e.g. deepseek-v4-flash). Default "deepseek-v4-flash".
	DeepSeekModel string
	// ChatLogPath is the file for chat access logs (one JSON line per
	// request: user_id, session_id, input messages, output text, status,
	// duration). Empty = chat access logging disabled (the /chat endpoint
	// still works, only the audit trail is skipped).
	ChatLogPath string

	// LLMProvidersJSON is the OPTIONAL compatibility import of the model
	// catalog (基线报告差距 1: LLM_PROVIDERS_JSON 环境变量目录 → 数据库配置).
	// When non-empty, cmd/server runs ONE explicit idempotent import at
	// startup: entities missing from the database are inserted as DRAFT,
	// everything already present is left untouched — the env never
	// overwrites operational DB config on restart. Invalid JSON fails
	// startup loudly. The runtime catalog truth is the published DB
	// revision, never this env.
	LLMProvidersJSON string

	// InferenceCredentialKeys is the deployment-secret key material for
	// the upstream-credential vault (AEAD). Format:
	//   1:64hexchars,2:64hexchars
	// Highest version = current encryption key; earlier versions stay for
	// decrypting old ciphertext. Injected from the deployment secret store
	// only — never committed, never logged. Empty = credential management
	// endpoints fail closed until key material is configured.
	InferenceCredentialKeys string
	// InferenceUpstreamAllowlist lists CIDRs (10.0.0.0/8, fd00::/8) and/or
	// exact hostnames that may be used as upstream deployment targets even
	// though they are not globally routable (self-hosted intranet
	// deployments, 设计 §5). Empty = only globally routable targets.
	InferenceUpstreamAllowlist []string
	// InferenceAccountRPM is the default per-billing-account requests-per-
	// minute bucket on /v1/* (Task 5; per-Key rpm_limit applies on top when
	// set). Process-local sliding window; cross-instance coordination is
	// Task 7's database leases. 0 disables the account-level bucket.
	InferenceAccountRPM int

	// InferenceKayaChatGateway is the /chat 迁移开关 (Task 8): when true,
	// POST /chat and GET /chat/models are served by the inference gateway
	// facade (JWT → principal → entitlement → quota → routing) instead of
	// the legacy DeepSeek passthrough. Default false = 旧行为直通 (legacy
	// passthrough, no entitlement gate).
	InferenceKayaChatGateway bool
	// KayaChatModel is the public inference model id the /chat facade
	// applies when the client sends no model (旧无 model 默认). Required
	// when InferenceKayaChatGateway is on.
	KayaChatModel string

	// Settlement recovery worker (Task 9): pass interval, per-pass batch
	// cap, staleness grace (must exceed the 15s settlement deadline so live
	// requests are never swept), and the reconciliation evidence window.
	// Defaults: 30s / 100 / 15m / 24h. Grace floor (12m) enforced by
	// Validate: it must exceed the worst LIVE request phase (deployment
	// RequestTimeout default 10min; nginx SSE relay 700s; settlement 15s),
	// otherwise the sweep settles live long requests at their full hold.
	InferenceRecoveryInterval       time.Duration
	InferenceRecoveryBatch          int
	InferenceRecoveryGrace          time.Duration
	InferenceReconciliationDeadline time.Duration
	// Task 10: entitlement-sync worker (outbox consumer for payment→
	// entitlement grants). Defaults: 2s / 100 / 10min backoff cap.
	InferenceEntitlementSyncInterval time.Duration
	InferenceEntitlementSyncBatch    int
}

// Load reads configuration from process env vars. Defaults match the values
// documented in README.md and .env.example.
func Load() *Config {
	return &Config{
		Port:        envOr("PORT", "8080"),
		DatabaseURL: envOr("DATABASE_URL", "postgres://localhost/yunhou_users?sslmode=disable"),
		RSAPrivate:  envOr("RSA_PRIVATE_KEY_PATH", "keys/private.pem"),
		RSAPublic:   envOr("RSA_PUBLIC_KEY_PATH", "keys/public.pem"),

		OAuthStateSecret:           os.Getenv("OAUTH_STATE_SECRET"),
		GitHubClientID:             os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret:         os.Getenv("GITHUB_CLIENT_SECRET"),
		WeChatOAuthMock:            os.Getenv("WECHAT_OAUTH_MOCK") == "1",
		WeChatPayMock:              os.Getenv("WECHAT_PAY_MOCK") == "1",
		PaypalL3E2EMode:            os.Getenv("PAYPAL_L3_E2E_MODE") == "1",
		WeChatPayMchID:             os.Getenv("WECHAT_PAY_MCH_ID"),
		WeChatPayAppID:             os.Getenv("WECHAT_PAY_APP_ID"),
		WeChatPayMchPrivateKeyPath: os.Getenv("WECHAT_PAY_MCH_PRIVATE_KEY_PATH"),
		WeChatPayMchCertPath:       os.Getenv("WECHAT_PAY_MCH_CERT_PATH"),
		WeChatPayNotifyURL:         os.Getenv("WECHAT_PAY_NOTIFY_URL"),

		JWTAccessTTL:  parseDurationOr(envOr("JWT_ACCESS_TTL", "15m"), 15*time.Minute),
		JWTRefreshTTL: parseDurationOr(envOr("JWT_REFRESH_TTL", "168h"), 168*time.Hour),

		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		WeChatAPIv3Key:      os.Getenv("WECHAT_PAY_API_V3_KEY"),
		AlipayPublicKeyPath: os.Getenv("ALIPAY_PUBLIC_KEY_PATH"),

		PaypalEnv:              envOr("PAYPAL_ENV", ""),
		PaypalWebhookIDSandbox: os.Getenv("PAYPAL_WEBHOOK_ID_SANDBOX"),
		PaypalWebhookIDLive:    os.Getenv("PAYPAL_WEBHOOK_ID_LIVE"),
		PaypalAPIBaseSandbox:   envOr("PAYPAL_API_BASE_SANDBOX", "https://api-m.sandbox.paypal.com"),
		PaypalAPIBaseLive:      envOr("PAYPAL_API_BASE_LIVE", "https://api-m.paypal.com"),
		PaypalClientID:         os.Getenv("PAYPAL_CLIENT_ID"),
		PaypalClientSecret:     os.Getenv("PAYPAL_CLIENT_SECRET"),

		OrderExpiryDuration: parseDurationOr(envOr("ORDER_EXPIRY_DURATION", "30m"), 30*time.Minute),
		SweeperInterval:     parseDurationOr(envOr("SWEEPER_INTERVAL", "1m"), 1*time.Minute),

		PlanAmountOverrideJSON: os.Getenv("PLAN_AMOUNT_OVERRIDE_JSON"),

		DeepSeekAPIKey:  os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL: envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel:   envOr("DEEPSEEK_MODEL", "deepseek-v4-flash"),
		ChatLogPath:     os.Getenv("CHAT_LOG_PATH"),

		LLMProvidersJSON: os.Getenv("LLM_PROVIDERS_JSON"),

		InferenceCredentialKeys:    os.Getenv("INFERENCE_CREDENTIAL_KEYS"),
		InferenceUpstreamAllowlist: splitComma(os.Getenv("INFERENCE_UPSTREAM_ALLOWLIST")),
		InferenceAccountRPM:        parseIntOr(envOr("INFERENCE_ACCOUNT_RPM", "120"), 120),
		InferenceKayaChatGateway:   os.Getenv("INFERENCE_KAYA_CHAT_GATEWAY") == "1",
		KayaChatModel:              os.Getenv("KAYA_CHAT_MODEL"),

		InferenceRecoveryInterval:        parseDurationOr(envOr("INFERENCE_RECOVERY_INTERVAL", "30s"), 30*time.Second),
		InferenceRecoveryBatch:           parseIntOr(envOr("INFERENCE_RECOVERY_BATCH", "100"), 100),
		InferenceRecoveryGrace:           parseDurationOr(envOr("INFERENCE_RECOVERY_GRACE", "15m"), 15*time.Minute),
		InferenceReconciliationDeadline:  parseDurationOr(envOr("INFERENCE_RECONCILIATION_DEADLINE", "24h"), 24*time.Hour),
		InferenceEntitlementSyncInterval: parseDurationOr(envOr("INFERENCE_ENTITLEMENT_SYNC_INTERVAL", "2s"), 2*time.Second),
		InferenceEntitlementSyncBatch:    parseIntOr(envOr("INFERENCE_ENTITLEMENT_SYNC_BATCH", "100"), 100),
	}
}

// Validate enforces required fields and reasonable bounds. Call once at
// startup so misconfiguration fails fast instead of surfacing as 500s at
// first request.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if c.RSAPrivate == "" || c.RSAPublic == "" {
		return errors.New("RSA_PRIVATE_KEY_PATH and RSA_PUBLIC_KEY_PATH are required")
	}
	if c.JWTAccessTTL <= 0 {
		return errors.New("JWT_ACCESS_TTL must be a positive duration")
	}
	if c.JWTRefreshTTL <= c.JWTAccessTTL {
		return errors.New("JWT_REFRESH_TTL must be strictly greater than JWT_ACCESS_TTL")
	}
	if c.JWTRefreshTTL > 365*24*time.Hour {
		return errors.New("JWT_REFRESH_TTL must be at most 365 days")
	}
	if c.OrderExpiryDuration <= 0 {
		return errors.New("ORDER_EXPIRY_DURATION must be a positive duration")
	}
	if c.SweeperInterval <= 0 {
		return errors.New("SWEEPER_INTERVAL must be a positive duration")
	}
	if c.SweeperInterval >= c.OrderExpiryDuration {
		return errors.New("SWEEPER_INTERVAL must be strictly less than ORDER_EXPIRY_DURATION")
	}
	if c.OAuthStateSecret == "" {
		return errors.New("OAUTH_STATE_SECRET is required")
	}
	// 32 bytes minimum — 1-byte secrets are brute-forceable in
	// microseconds against the HMAC-SHA256 state token. Operators should
	// generate via `openssl rand -hex 32`.
	if len(c.OAuthStateSecret) < 32 {
		return errors.New("OAUTH_STATE_SECRET must be at least 32 characters (use `openssl rand -hex 32`)")
	}
	// Real-mode WeChat Pay credentials are a six-field all-or-none tuple:
	//   WECHAT_PAY_API_V3_KEY + WECHAT_PAY_MCH_ID  (used for webhook
	//     verification, AES-GCM resource decryption, and to form the
	//     Authorization header scheme value)
	//   WECHAT_PAY_APP_ID                          (NATIVE request body
	//     field "appid")
	//   WECHAT_PAY_MCH_PRIVATE_KEY_PATH            (outbound request
	//     signing key)
	//   WECHAT_PAY_MCH_CERT_PATH                   (cert serial → outbound
	//     Authorization "serial_no")
	//   WECHAT_PAY_NOTIFY_URL                      (outbound body notify_url)
	// The first two cases keep the MCH_ID/APIv3Key error messages explicit;
	// the final case rejects any other partial tuple while allowing all six
	// fields to remain empty when WeChat Pay is not enabled. Mock-mode
	// deployments may leave all six fields empty or partially populated.
	switch {
	case c.WeChatPayMchID == "" && c.WeChatAPIv3Key != "" && !c.WeChatPayMock:
		return errors.New("WECHAT_PAY_MCH_ID is required when WECHAT_PAY_API_V3_KEY is set and WECHAT_PAY_MOCK is not enabled")
	case c.WeChatPayMchID != "" && c.WeChatAPIv3Key == "" && !c.WeChatPayMock:
		return errors.New("WECHAT_PAY_API_V3_KEY is required when WECHAT_PAY_MCH_ID is set and WECHAT_PAY_MOCK is not enabled")
	case !c.WeChatPayMock &&
		((c.WeChatPayMchID != "" || c.WeChatAPIv3Key != "" ||
			c.WeChatPayAppID != "" || c.WeChatPayMchPrivateKeyPath != "" ||
			c.WeChatPayMchCertPath != "" || c.WeChatPayNotifyURL != "") &&
			(c.WeChatPayMchID == "" || c.WeChatAPIv3Key == "" ||
				c.WeChatPayAppID == "" || c.WeChatPayMchPrivateKeyPath == "" ||
				c.WeChatPayMchCertPath == "" || c.WeChatPayNotifyURL == "")):
		return errors.New("real WeChat Pay mode requires ALL of: WECHAT_PAY_MCH_ID, WECHAT_PAY_API_V3_KEY, WECHAT_PAY_APP_ID, " +
			"WECHAT_PAY_MCH_PRIVATE_KEY_PATH, WECHAT_PAY_MCH_CERT_PATH, WECHAT_PAY_NOTIFY_URL")
	}
	// Test/mock escape hatches each bypass a real security boundary
	// (/test/login mints arbitrary JWTs, WECHAT_PAY_MOCK accepts unsigned
	// payment webhooks, WECHAT_OAUTH_MOCK logs in a fixed identity). They
	// exist for dev/e2e only — hard-fail at startup when one combines with
	// a production signal so a stray env line in a production .env is
	// caught at deploy time instead of silently opening the bypass.
	if c.PaypalEnv == "live" {
		if c.PaypalL3E2EMode {
			return errors.New("PAYPAL_L3_E2E_MODE must not be enabled when PAYPAL_ENV=live")
		}
		if c.WeChatPayMock {
			return errors.New("WECHAT_PAY_MOCK must not be enabled when PAYPAL_ENV=live")
		}
		if c.WeChatOAuthMock {
			return errors.New("WECHAT_OAUTH_MOCK must not be enabled when PAYPAL_ENV=live")
		}
	}
	// A fully-populated real WeChat Pay credential tuple alongside mock
	// mode is the same class of misconfig: the deployment looks production
	// but the webhook verifier is disarmed.
	if c.WeChatPayMock &&
		c.WeChatPayMchID != "" && c.WeChatAPIv3Key != "" &&
		c.WeChatPayAppID != "" && c.WeChatPayMchPrivateKeyPath != "" &&
		c.WeChatPayMchCertPath != "" && c.WeChatPayNotifyURL != "" {
		return errors.New("WECHAT_PAY_MOCK must not be enabled when real WeChat Pay credentials are fully configured")
	}
	// APIv3Key is 32 bytes exactly — used both as the HMAC key for
	// inbound signature verification and as the AES-GCM key for resource
	// decryption. Wrong-sized values would silently misalign AES block
	// boundaries at request time; catch them at startup.
	if !c.WeChatPayMock && c.WeChatAPIv3Key != "" && len(c.WeChatAPIv3Key) != 32 {
		return errors.New("WECHAT_PAY_API_V3_KEY must be exactly 32 bytes")
	}
	// Chat (DeepSeek proxy) is optional: empty key = endpoint disabled.
	// When the key is set, the base URL and model must be sane — an empty
	// model would be rejected upstream with a confusing 400 instead of a
	// clear startup error.
	if c.DeepSeekAPIKey != "" && c.DeepSeekBaseURL == "" {
		return errors.New("DEEPSEEK_BASE_URL is required when DEEPSEEK_API_KEY is set")
	}
	if c.DeepSeekAPIKey != "" && c.DeepSeekModel == "" {
		return errors.New("DEEPSEEK_MODEL is required when DEEPSEEK_API_KEY is set")
	}
	// A malformed base URL (e.g. missing scheme, or a non-HTTP scheme like
	// ftp://) only surfaces at request time as a permanent 502 — catch it
	// at startup instead.
	if c.DeepSeekAPIKey != "" {
		u, err := url.Parse(c.DeepSeekBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("DEEPSEEK_BASE_URL must be an absolute http(s) URL (e.g. https://api.deepseek.com)")
		}
	}
	// Inference credential vault keys: when present they must parse (the
	// vault itself fails closed at request time when unset). Rejecting bad
	// material at startup beats discovering a hex typo the first time an
	// operator tries to store a credential.
	if c.InferenceCredentialKeys != "" {
		if _, _, err := credentials.ParseKeysEnv(c.InferenceCredentialKeys); err != nil {
			return fmt.Errorf("INFERENCE_CREDENTIAL_KEYS: %v", err)
		}
	}
	// /chat 网关迁移开关：开启时必须配置默认模型（旧无 model 默认的承接
	// 者），否则 facade 无模型可路由。
	if c.InferenceKayaChatGateway && c.KayaChatModel == "" {
		return errors.New("KAYA_CHAT_MODEL is required when INFERENCE_KAYA_CHAT_GATEWAY=1")
	}
	// Recovery grace floor (Task 9 审查修复): the sweep grace must exceed
	// the worst LIVE phase of one request — deployment RequestTimeout
	// (default 10min) covers dispatch, nginx lets /v1/chat/completions SSE
	// run 700s, settlement adds 15s. A shorter grace sweeps live long
	// requests and settles them at the full hold while still streaming.
	// 联动校验：raising a deployment's RequestTimeout above this floor
	// requires raising INFERENCE_RECOVERY_GRACE accordingly.
	if c.InferenceRecoveryGrace < 12*time.Minute {
		return fmt.Errorf("INFERENCE_RECOVERY_GRACE=%s is below the 12m floor (must exceed worst live request phase: 700s SSE relay + 15s settlement)", c.InferenceRecoveryGrace)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		// Log loudly so operators see typos (e.g. JWT_ACCESS_TTL=15
		// without a unit) at startup instead of finding out in
		// production when a token TTL is wildly wrong.
		log.Printf("config: parse duration %q failed (%v); using fallback %s", s, err, fallback)
		return fallback
	}
	return d
}

func parseIntOr(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		log.Printf("config: parse int %q failed; using fallback %d", s, fallback)
		return fallback
	}
	return n
}
