package config

import (
	"errors"
	"log"
	"net/url"
	"os"
	"time"
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
	// Production MUST leave this false.
	WeChatPayMock bool

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

		OrderExpiryDuration: parseDurationOr(envOr("ORDER_EXPIRY_DURATION", "30m"), 30*time.Minute),
		SweeperInterval:     parseDurationOr(envOr("SWEEPER_INTERVAL", "1m"), 1*time.Minute),

		PlanAmountOverrideJSON: os.Getenv("PLAN_AMOUNT_OVERRIDE_JSON"),

		DeepSeekAPIKey:  os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekBaseURL: envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel:   envOr("DEEPSEEK_MODEL", "deepseek-v4-flash"),
		ChatLogPath:     os.Getenv("CHAT_LOG_PATH"),
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
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
