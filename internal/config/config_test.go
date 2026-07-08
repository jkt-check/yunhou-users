package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear all relevant env vars so defaults are used.
	envVars := []string{
		"PORT", "DATABASE_URL", "RSA_PRIVATE_KEY_PATH", "RSA_PUBLIC_KEY_PATH",
		"JWT_ACCESS_TTL", "JWT_REFRESH_TTL",
		"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET",
	}
	for _, k := range envVars {
		orig, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, orig)
			} else {
				os.Unsetenv(k)
			}
		})
	}

	cfg := Load()

	if cfg.Port != "8080" {
		t.Errorf("Port: got %q, want %q", cfg.Port, "8080")
	}
	if cfg.DatabaseURL != "postgres://localhost/yunhou_users?sslmode=disable" {
		t.Errorf("DatabaseURL: got %q, want %q", cfg.DatabaseURL, "postgres://localhost/yunhou_users?sslmode=disable")
	}
	if cfg.RSAPrivate != "keys/private.pem" {
		t.Errorf("RSAPrivate: got %q, want %q", cfg.RSAPrivate, "keys/private.pem")
	}
	if cfg.RSAPublic != "keys/public.pem" {
		t.Errorf("RSAPublic: got %q, want %q", cfg.RSAPublic, "keys/public.pem")
	}
	if cfg.JWTAccessTTL != 15*time.Minute {
		t.Errorf("JWTAccessTTL: got %v, want %v", cfg.JWTAccessTTL, 15*time.Minute)
	}
	if cfg.JWTRefreshTTL != 168*time.Hour {
		t.Errorf("JWTRefreshTTL: got %v, want %v", cfg.JWTRefreshTTL, 168*time.Hour)
	}
	// OAuth fields should be empty when env vars are unset
	if cfg.GitHubClientID != "" {
		t.Errorf("GitHubClientID: got %q, want empty", cfg.GitHubClientID)
	}
	if cfg.GitHubClientSecret != "" {
		t.Errorf("GitHubClientSecret: got %q, want empty", cfg.GitHubClientSecret)
	}
	}

func TestLoad_EnvVarsOverride(t *testing.T) {
	envVars := map[string]string{
		"PORT":                "3000",
		"DATABASE_URL":        "postgres://user:pass@host/db",
		"RSA_PRIVATE_KEY_PATH": "/tmp/priv.pem",
		"RSA_PUBLIC_KEY_PATH":  "/tmp/pub.pem",
		"JWT_ACCESS_TTL":      "30m",
		"JWT_REFRESH_TTL":     "72h",
		"GITHUB_CLIENT_ID":    "gh-id",
		"GITHUB_CLIENT_SECRET": "gh-secret",
	}

	for k, v := range envVars {
		orig, had := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, orig)
			} else {
				os.Unsetenv(k)
			}
		})
	}

	cfg := Load()

	if cfg.Port != "3000" {
		t.Errorf("Port: got %q, want %q", cfg.Port, "3000")
	}
	if cfg.DatabaseURL != "postgres://user:pass@host/db" {
		t.Errorf("DatabaseURL: got %q, want %q", cfg.DatabaseURL, "postgres://user:pass@host/db")
	}
	if cfg.RSAPrivate != "/tmp/priv.pem" {
		t.Errorf("RSAPrivate: got %q, want %q", cfg.RSAPrivate, "/tmp/priv.pem")
	}
	if cfg.RSAPublic != "/tmp/pub.pem" {
		t.Errorf("RSAPublic: got %q, want %q", cfg.RSAPublic, "/tmp/pub.pem")
	}
	if cfg.JWTAccessTTL != 30*time.Minute {
		t.Errorf("JWTAccessTTL: got %v, want %v", cfg.JWTAccessTTL, 30*time.Minute)
	}
	if cfg.JWTRefreshTTL != 72*time.Hour {
		t.Errorf("JWTRefreshTTL: got %v, want %v", cfg.JWTRefreshTTL, 72*time.Hour)
	}
	if cfg.GitHubClientID != "gh-id" {
		t.Errorf("GitHubClientID: got %q, want %q", cfg.GitHubClientID, "gh-id")
	}
	if cfg.GitHubClientSecret != "gh-secret" {
		t.Errorf("GitHubClientSecret: got %q, want %q", cfg.GitHubClientSecret, "gh-secret")
	}
}

func TestEnvOr(t *testing.T) {
	t.Parallel()

	t.Run("returns env value when set", func(t *testing.T) {
		t.Parallel()
		key := "TEST_ENVOR_SET"
		os.Setenv(key, "from-env")
		defer os.Unsetenv(key)
		if got := envOr(key, "fallback"); got != "from-env" {
			t.Errorf("envOr: got %q, want %q", got, "from-env")
		}
	})

	t.Run("returns fallback when env is empty", func(t *testing.T) {
		t.Parallel()
		key := "TEST_ENVOR_EMPTY"
		os.Unsetenv(key)
		if got := envOr(key, "fallback"); got != "fallback" {
			t.Errorf("envOr: got %q, want %q", got, "fallback")
		}
	})

	t.Run("returns fallback when env is not set", func(t *testing.T) {
		t.Parallel()
		key := "TEST_ENVOR_UNSET"
		os.Unsetenv(key)
		if got := envOr(key, "default"); got != "default" {
			t.Errorf("envOr: got %q, want %q", got, "default")
		}
	})

	t.Run("returns env value even if fallback is empty", func(t *testing.T) {
		t.Parallel()
		key := "TEST_ENVOR_SET_EMPTY_FB"
		os.Setenv(key, "has-value")
		defer os.Unsetenv(key)
		if got := envOr(key, ""); got != "has-value" {
			t.Errorf("envOr: got %q, want %q", got, "has-value")
		}
	})
}

// TestValidate_HappyPath confirms a fully-populated Config passes validation.
func TestValidate_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		DatabaseURL:        "postgres://x",
		RSAPrivate:         "priv",
		RSAPublic:          "pub",
		JWTAccessTTL:       15 * time.Minute,
		JWTRefreshTTL:      168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     1 * time.Minute,
		OAuthStateSecret:    "test-state-secret-thirty-two-bytes-min-len",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

// TestValidate_ErrorPaths walks every Validate() rejection branch.
func TestValidate_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(c *Config)
		needleSub string // substring the error message should contain
	}{
		{"missing database_url",
			func(c *Config) { c.DatabaseURL = "" },
			"DATABASE_URL",
		},
		{"missing rsa_private",
			func(c *Config) { c.RSAPrivate = "" },
			"RSA_PRIVATE_KEY_PATH",
		},
		{"missing rsa_public",
			func(c *Config) { c.RSAPublic = "" },
			"RSA_PUBLIC_KEY_PATH",
		},
		{"zero jwt_access_ttl",
			func(c *Config) { c.JWTAccessTTL = 0 },
			"JWT_ACCESS_TTL",
		},
		{"refresh_ttl <= access_ttl",
			func(c *Config) {
				c.JWTAccessTTL = 10 * time.Minute
				c.JWTRefreshTTL = 10 * time.Minute
			},
			"JWT_REFRESH_TTL must be strictly greater",
		},
		{"refresh_ttl > 365 days",
			func(c *Config) {
				c.JWTAccessTTL = 24 * time.Hour
				c.JWTRefreshTTL = 400 * 24 * time.Hour
			},
			"JWT_REFRESH_TTL must be at most 365 days",
		},
		{"zero order_expiry",
			func(c *Config) { c.OrderExpiryDuration = 0 },
			"ORDER_EXPIRY_DURATION",
		},
		{"zero sweeper_interval",
			func(c *Config) { c.SweeperInterval = 0 },
			"SWEEPER_INTERVAL",
		},
		{"sweeper_interval >= order_expiry",
			func(c *Config) {
				c.OrderExpiryDuration = 5 * time.Minute
				c.SweeperInterval = 5 * time.Minute
			},
			"SWEEPER_INTERVAL must be strictly less",
		},
		{"missing oauth_state_secret",
			func(c *Config) { c.OAuthStateSecret = "" },
			"OAUTH_STATE_SECRET",
		},
		{"oauth_state_secret too short",
			func(c *Config) { c.OAuthStateSecret = "short" },
			"OAUTH_STATE_SECRET",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{
				DatabaseURL:        "postgres://x",
				RSAPrivate:         "priv",
				RSAPublic:          "pub",
				JWTAccessTTL:       15 * time.Minute,
				JWTRefreshTTL:      168 * time.Hour,
				OrderExpiryDuration: 30 * time.Minute,
				SweeperInterval:     1 * time.Minute,
				OAuthStateSecret:    "test-state-secret",
			}
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.needleSub) {
				t.Errorf("error message missing %q: %v", tc.needleSub, err)
			}
		})
	}
}

// TestLoad_PaymentChannelDefaults confirms that when the payment-channel
// env vars are unset, the corresponding Config fields are empty (the docs
// promise "Empty = <channel> webhooks return 404") and ORDER_EXPIRY_*
// fall back to the documented defaults. Without this, a typo in
// config.go:77-80 would silently drop every channel secret and turn all
// four webhook endpoints into 404s at runtime — no Validate() catches it
// because the fields are allowed to be empty.
func TestLoad_PaymentChannelDefaults(t *testing.T) {
	for _, k := range []string{
		"STRIPE_WEBHOOK_SECRET", "WECHAT_PAY_API_V3_KEY",
		"ALIPAY_PUBLIC_KEY_PATH",
		"ORDER_EXPIRY_DURATION", "SWEEPER_INTERVAL",
	} {
		orig, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			t.Cleanup(func() { os.Setenv(k, orig) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
	}

	cfg := Load()
	if cfg.StripeWebhookSecret != "" {
		t.Errorf("StripeWebhookSecret default: got %q, want empty", cfg.StripeWebhookSecret)
	}
	if cfg.WeChatAPIv3Key != "" {
		t.Errorf("WeChatAPIv3Key default: got %q, want empty", cfg.WeChatAPIv3Key)
	}
	if cfg.AlipayPublicKeyPath != "" {
		t.Errorf("AlipayPublicKeyPath default: got %q, want empty", cfg.AlipayPublicKeyPath)
	}
	if cfg.OrderExpiryDuration != 30*time.Minute {
		t.Errorf("OrderExpiryDuration default: got %v, want 30m", cfg.OrderExpiryDuration)
	}
	if cfg.SweeperInterval != 1*time.Minute {
		t.Errorf("SweeperInterval default: got %v, want 1m", cfg.SweeperInterval)
	}
}

// TestLoad_PaymentChannelOverrides confirms the same six env vars override
// the defaults when set. Pairs with TestLoad_PaymentChannelDefaults — the
// two together cover the Load() path for every payment-channel field.
func TestLoad_PaymentChannelOverrides(t *testing.T) {
	envVars := map[string]string{
		"STRIPE_WEBHOOK_SECRET":  "whsec_x",
		"WECHAT_PAY_API_V3_KEY":  "12345678901234567890123456789012", // 32 bytes
		"ALIPAY_PUBLIC_KEY_PATH": "/etc/alipay.pem",
		"ORDER_EXPIRY_DURATION":  "5m",
		"SWEEPER_INTERVAL":       "30s",
	}
	for k, v := range envVars {
		orig, had := os.LookupEnv(k)
		os.Setenv(k, v)
		if had {
			t.Cleanup(func() { os.Setenv(k, orig) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
	}

	cfg := Load()
	if cfg.StripeWebhookSecret != "whsec_x" {
		t.Errorf("StripeWebhookSecret override: got %q", cfg.StripeWebhookSecret)
	}
	if cfg.WeChatAPIv3Key != "12345678901234567890123456789012" {
		t.Errorf("WeChatAPIv3Key override: got %q", cfg.WeChatAPIv3Key)
	}
	if cfg.AlipayPublicKeyPath != "/etc/alipay.pem" {
		t.Errorf("AlipayPublicKeyPath override: got %q", cfg.AlipayPublicKeyPath)
	}
	if cfg.OrderExpiryDuration != 5*time.Minute {
		t.Errorf("OrderExpiryDuration override: got %v, want 5m", cfg.OrderExpiryDuration)
	}
	if cfg.SweeperInterval != 30*time.Second {
		t.Errorf("SweeperInterval override: got %v, want 30s", cfg.SweeperInterval)
	}
}

// TestLoad_PaypalDefaults verifies the new PAYPAL_* defaults.
func TestLoad_PaypalDefaults(t *testing.T) {
	for _, k := range []string{
		"PAYPAL_ENV", "PAYPAL_WEBHOOK_ID_SANDBOX", "PAYPAL_WEBHOOK_ID_LIVE",
		"PAYPAL_API_BASE_SANDBOX", "PAYPAL_API_BASE_LIVE",
	} {
		orig, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			t.Cleanup(func() { os.Setenv(k, orig) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
	}

	cfg := Load()
	if cfg.PaypalEnv != "live" {
		t.Errorf("PaypalEnv default: got %q, want live", cfg.PaypalEnv)
	}
	if cfg.PaypalAPIBaseSandbox != "https://api-m.sandbox.paypal.com" {
		t.Errorf("PaypalAPIBaseSandbox default: got %q", cfg.PaypalAPIBaseSandbox)
	}
	if cfg.PaypalAPIBaseLive != "https://api-m.paypal.com" {
		t.Errorf("PaypalAPIBaseLive default: got %q", cfg.PaypalAPIBaseLive)
	}
	if cfg.PaypalWebhookIDSandbox != "" || cfg.PaypalWebhookIDLive != "" {
		t.Errorf("webhook IDs should default to empty, got %q / %q",
			cfg.PaypalWebhookIDSandbox, cfg.PaypalWebhookIDLive)
	}
}

// TestLoad_PaypalEnvOverride confirms PAYPAL_ENV=sandbox flows through Load.
func TestLoad_PaypalEnvOverride(t *testing.T) {
	os.Setenv("PAYPAL_ENV", "sandbox")
	defer os.Unsetenv("PAYPAL_ENV")
	cfg := Load()
	if cfg.PaypalEnv != "sandbox" {
		t.Errorf("PaypalEnv override: got %q, want sandbox", cfg.PaypalEnv)
	}
}

func TestParseDurationOr(t *testing.T) {
	t.Parallel()
	t.Run("valid → parsed value", func(t *testing.T) {
		t.Parallel()
		got := parseDurationOr("5m", 99*time.Minute)
		if got != 5*time.Minute {
			t.Errorf("got %v, want 5m", got)
		}
	})
	t.Run("invalid → fallback", func(t *testing.T) {
		t.Parallel()
		got := parseDurationOr("not-a-duration", 7*time.Second)
		if got != 7*time.Second {
			t.Errorf("got %v, want fallback", got)
		}
	})
	t.Run("empty → fallback", func(t *testing.T) {
		t.Parallel()
		got := parseDurationOr("", 99*time.Hour)
		if got != 99*time.Hour {
			t.Errorf("got %v, want fallback", got)
		}
	})
}
