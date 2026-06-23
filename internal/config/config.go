package config

import (
	"errors"
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

	// GitHubClientID/Secret are reserved for a future OAuth redirect flow.
	// They are not used by the current direct-login implementation but kept
	// in the env so operators can pre-provision credentials.
	GitHubClientID     string
	GitHubClientSecret string
	GoogleClientID     string
	GoogleClientSecret string

	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	// Payment channel webhook secrets. Loaded but not strictly required
	// at startup — if a channel's secret is empty, webhooks for that channel
	// return 404 (signature verifier is nil for that channel). Operators
	// who don't accept a particular channel can leave its secret blank.
	StripeWebhookSecret string
	WeChatAPIv3Key      string // 32 bytes, used for both signature + AES-GCM resource decrypt
	AlipayPublicKeyPath string

	// Order expiry: how long a pending order is valid before the sweeper
	// flips it to 'expired'. Default 30 min per design doc §"v1 decisions".
	OrderExpiryDuration time.Duration
	// Sweeper interval: how often the in-process goroutine runs. Default 1 min.
	SweeperInterval time.Duration
}

// Load reads configuration from process env vars. Defaults match the values
// documented in README.md and .env.example.
func Load() *Config {
	return &Config{
		Port:        envOr("PORT", "8080"),
		DatabaseURL: envOr("DATABASE_URL", "postgres://localhost/yunhou_users?sslmode=disable"),
		RSAPrivate:  envOr("RSA_PRIVATE_KEY_PATH", "keys/private.pem"),
		RSAPublic:   envOr("RSA_PUBLIC_KEY_PATH", "keys/public.pem"),

		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),

		JWTAccessTTL:  parseDurationOr(envOr("JWT_ACCESS_TTL", "15m"), 15*time.Minute),
		JWTRefreshTTL: parseDurationOr(envOr("JWT_REFRESH_TTL", "168h"), 168*time.Hour),

		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		WeChatAPIv3Key:      os.Getenv("WECHAT_PAY_API_V3_KEY"),
		AlipayPublicKeyPath: os.Getenv("ALIPAY_PUBLIC_KEY_PATH"),

		OrderExpiryDuration: parseDurationOr(envOr("ORDER_EXPIRY_DURATION", "30m"), 30*time.Minute),
		SweeperInterval:     parseDurationOr(envOr("SWEEPER_INTERVAL", "1m"), 1*time.Minute),
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
		return fallback
	}
	return d
}
