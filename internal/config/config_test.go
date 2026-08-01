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
		"PORT":                 "3000",
		"DATABASE_URL":         "postgres://user:pass@host/db",
		"RSA_PRIVATE_KEY_PATH": "/tmp/priv.pem",
		"RSA_PUBLIC_KEY_PATH":  "/tmp/pub.pem",
		"JWT_ACCESS_TTL":       "30m",
		"JWT_REFRESH_TTL":      "72h",
		"GITHUB_CLIENT_ID":     "gh-id",
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
		DatabaseURL:                "postgres://x",
		RSAPrivate:                 "priv",
		RSAPublic:                  "pub",
		JWTAccessTTL:               15 * time.Minute,
		JWTRefreshTTL:              168 * time.Hour,
		OrderExpiryDuration:        30 * time.Minute,
		SweeperInterval:            1 * time.Minute,
		OAuthStateSecret:           "test-state-secret-thirty-two-bytes-min-len",
		WeChatAPIv3Key:             "0123456789abcdef0123456789abcdef", // 32 bytes
		WeChatPayMchID:             "1900000001",
		WeChatPayAppID:             "wx1900000109",
		WeChatPayMchPrivateKeyPath: "/etc/wechat/apiclient_key.pem",
		WeChatPayMchCertPath:       "/etc/wechat/apiclient_cert.pem",
		WeChatPayNotifyURL:         "https://example.com/webhooks/payment/wechat_pay",
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
				DatabaseURL:         "postgres://x",
				RSAPrivate:          "priv",
				RSAPublic:           "pub",
				JWTAccessTTL:        15 * time.Minute,
				JWTRefreshTTL:       168 * time.Hour,
				OrderExpiryDuration: 30 * time.Minute,
				SweeperInterval:     1 * time.Minute,
				OAuthStateSecret:    "test-state-secret-thirty-two-bytes-min-len",
				WeChatAPIv3Key:      "0123456789abcdef0123456789abcdef",
				WeChatPayMchID:      "1900000001",
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
	if cfg.PaypalEnv != "" {
		t.Errorf("PaypalEnv default: got %q, want empty (must be set explicitly; avoids accidental live)", cfg.PaypalEnv)
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

// TestLoad_WeChatOAuthMock covers the WECHAT_OAUTH_MOCK toggle:
//
//	"1" → true (handler short-circuits); any other value or unset → false
//
// (production behaviour). The boolean must NOT be a string-true; an
// operator setting "true" by accident should NOT enable mock mode.
func TestLoad_WeChatOAuthMock(t *testing.T) {
	cases := []struct {
		name, set, want string
	}{
		{"unset → false", "", "false"},
		{"=1 → true", "1", "true"},
		{"=0 → false", "0", "false"},
		{"=true (literal) → false", "true", "false"},
		{"=yes → false", "yes", "false"},
		{"=random → false", "xxx", "false"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			orig, had := os.LookupEnv("WECHAT_OAUTH_MOCK")
			if tc.set != "" {
				os.Setenv("WECHAT_OAUTH_MOCK", tc.set)
			} else {
				os.Unsetenv("WECHAT_OAUTH_MOCK")
			}
			t.Cleanup(func() {
				if had {
					os.Setenv("WECHAT_OAUTH_MOCK", orig)
				} else {
					os.Unsetenv("WECHAT_OAUTH_MOCK")
				}
			})
			cfg := Load()
			got := "false"
			if cfg.WeChatOAuthMock {
				got = "true"
			}
			if got != tc.want {
				t.Errorf("WeChatOAuthMock with %q: got %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

// TestLoad_WeChatPayMock mirrors TestLoad_WeChatOAuthMock for the
// pay-channel toggle. Same strict "1" semantics; no string-true alias
// (operators who write WECHAT_PAY_MOCK=true by accident must NOT enable
// mock mode).
func TestLoad_WeChatPayMock(t *testing.T) {
	cases := []struct {
		name, set, want string
	}{
		{"unset → false", "", "false"},
		{"=1 → true", "1", "true"},
		{"=0 → false", "0", "false"},
		{"=true (literal) → false", "true", "false"},
		{"=random → false", "xxx", "false"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			orig, had := os.LookupEnv("WECHAT_PAY_MOCK")
			if tc.set != "" {
				os.Setenv("WECHAT_PAY_MOCK", tc.set)
			} else {
				os.Unsetenv("WECHAT_PAY_MOCK")
			}
			t.Cleanup(func() {
				if had {
					os.Setenv("WECHAT_PAY_MOCK", orig)
				} else {
					os.Unsetenv("WECHAT_PAY_MOCK")
				}
			})
			cfg := Load()
			got := "false"
			if cfg.WeChatPayMock {
				got = "true"
			}
			if got != tc.want {
				t.Errorf("WeChatPayMock with %q: got %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

// TestLoad_WeChatPayMchID covers WECHAT_PAY_MCH_ID resolution: empty
// by default, populated when set. The Validate()-level guard is
// exercised in TestValidate_WeChatPayMchID.
func TestLoad_WeChatPayMchID(t *testing.T) {
	orig, had := os.LookupEnv("WECHAT_PAY_MCH_ID")
	os.Unsetenv("WECHAT_PAY_MCH_ID")
	t.Cleanup(func() {
		if had {
			os.Setenv("WECHAT_PAY_MCH_ID", orig)
		} else {
			os.Unsetenv("WECHAT_PAY_MCH_ID")
		}
	})

	if got := Load().WeChatPayMchID; got != "" {
		t.Errorf("default WeChatPayMchID = %q, want empty", got)
	}

	os.Setenv("WECHAT_PAY_MCH_ID", "1900000001")
	if got := Load().WeChatPayMchID; got != "1900000001" {
		t.Errorf("WeChatPayMchID override: got %q, want 1900000001", got)
	}
}

// TestValidate_WeChatReal_AllSixRequired covers the 6-tuple guard for
// real-mode WeChat Pay: in real mode (mock=false), the configuration must
// provide all six env vars — MCH_ID, APIv3_KEY, APP_ID, MCH_PRIVATE_KEY_PATH,
// MCH_CERT_PATH, NOTIFY_URL. Each row omits exactly one of the 4 newer envs
// and expects Validate() to error. The two existing MCH_ID/APIv3_KEY rules
// are also covered elsewhere; this test pins the partial-tuple arm.
func TestValidate_WeChatReal_AllSixRequired(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"missing app id", map[string]string{
			"WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
			"WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k", "WECHAT_PAY_MCH_CERT_PATH": "/c",
			"WECHAT_PAY_NOTIFY_URL": "https://x/cb",
		}},
		{"missing private key path", map[string]string{
			"WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
			"WECHAT_PAY_APP_ID": "wxabc", "WECHAT_PAY_MCH_CERT_PATH": "/c",
			"WECHAT_PAY_NOTIFY_URL": "https://x/cb",
		}},
		{"missing cert path", map[string]string{
			"WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
			"WECHAT_PAY_APP_ID": "wxabc", "WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k",
			"WECHAT_PAY_NOTIFY_URL": "https://x/cb",
		}},
		{"missing notify url", map[string]string{
			"WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
			"WECHAT_PAY_APP_ID": "wxabc", "WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k",
			"WECHAT_PAY_MCH_CERT_PATH": "/c",
		}},
		{"only private key path, all else empty", map[string]string{
			"WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k",
		}},
		{"only cert path, all else empty", map[string]string{
			"WECHAT_PAY_MCH_CERT_PATH": "/c",
		}},
		{"only notify url, all else empty", map[string]string{
			"WECHAT_PAY_NOTIFY_URL": "https://x/cb",
		}},
		{"private key + cert set, no MCH_ID/APIv3Key", map[string]string{
			"WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k",
			"WECHAT_PAY_MCH_CERT_PATH":        "/c",
			"WECHAT_PAY_NOTIFY_URL":           "https://x/cb",
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Wipe any leftover WeChat + state-secret env vars so the test
			// isolates the new 4-env check (otherwise Validate fails on
			// OAUTH_STATE_SECRET first and the test "passes" for the wrong
			// reason).
			for _, k := range []string{
				"WECHAT_PAY_MCH_ID", "WECHAT_PAY_API_V3_KEY", "WECHAT_PAY_APP_ID",
				"WECHAT_PAY_MCH_PRIVATE_KEY_PATH", "WECHAT_PAY_MCH_CERT_PATH",
				"WECHAT_PAY_NOTIFY_URL", "WECHAT_PAY_MOCK",
				"OAUTH_STATE_SECRET",
			} {
				orig, had := os.LookupEnv(k)
				os.Unsetenv(k)
				if had {
					t.Cleanup(func() { os.Setenv(k, orig) })
				} else {
					t.Cleanup(func() { os.Unsetenv(k) })
				}
			}
			t.Setenv("OAUTH_STATE_SECRET", "test-state-secret-thirty-two-bytes-min-len")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := Load()
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate: expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestValidate_WeChatReal_AllSixSet_OK guards the all-six-set side of the
// real-mode tuple rule. All six fields together are a valid configuration.
func TestValidate_WeChatReal_AllSixSet_OK(t *testing.T) {
	t.Setenv("OAUTH_STATE_SECRET", "test-state-secret-thirty-two-bytes-min-len")
	t.Setenv("WECHAT_PAY_MOCK", "0")
	t.Setenv("WECHAT_PAY_MCH_ID", "1900000001")
	t.Setenv("WECHAT_PAY_API_V3_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("WECHAT_PAY_APP_ID", "wx1900000109")
	t.Setenv("WECHAT_PAY_MCH_PRIVATE_KEY_PATH", "/k")
	t.Setenv("WECHAT_PAY_MCH_CERT_PATH", "/c")
	t.Setenv("WECHAT_PAY_NOTIFY_URL", "https://x/cb")

	c := Load()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with all 6 set: got %v, want nil", err)
	}
}

// TestValidate_WeChatMock_AllowsEmpty confirms that mock-mode deployments
// (WECHAT_PAY_MOCK=1) may leave all six WeChat Pay env vars unset — the
// mock short-circuits both the inbound webhook verification and the
// outbound signing path, so there is no material to load.
func TestValidate_WeChatMock_AllowsEmpty(t *testing.T) {
	t.Setenv("OAUTH_STATE_SECRET", "test-state-secret-thirty-two-bytes-min-len")
	t.Setenv("WECHAT_PAY_MOCK", "1")
	// No wechat pay envs at all — mock mode must not block boot.
	c := Load()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate in mock mode: %v", err)
	}
}

// TestValidate_WeChatReal_APIv3KeyWrongLength confirms that when real mode
// is enabled AND the APIv3 key is set, it must be exactly 32 bytes.
// Used for AES-GCM (16/24/32-byte keys) and as an HMAC-SHA256 key in the
// inbound webhook signature verifier. Wrong-sized values would silently
// misalign AES block boundaries at request time.
func TestValidate_WeChatReal_APIv3KeyWrongLength(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
	}{
		{"too short", "short"},
		{"too long", strings.Repeat("a", 33)},
		{"31 bytes", strings.Repeat("a", 31)},
		{"33 bytes", strings.Repeat("a", 33)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validRealWeChatConfig()
			cfg.WeChatAPIv3Key = tc.key
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for APIv3Key=%q (len %d)", tc.key, len(tc.key))
			}
			if !strings.Contains(err.Error(), "exactly 32 bytes") {
				t.Errorf("error message missing 'exactly 32 bytes': %v", err)
			}
		})
	}
}

// validRealWeChatConfig builds a baseline Config that satisfies every
// required field for real-mode WeChat Pay. Tests then mutate one field
// at a time to exercise Validate()'s error branches.
func validRealWeChatConfig() *Config {
	return &Config{
		DatabaseURL:                "postgres://x",
		RSAPrivate:                 "priv",
		RSAPublic:                  "pub",
		JWTAccessTTL:               15 * time.Minute,
		JWTRefreshTTL:              168 * time.Hour,
		OrderExpiryDuration:        30 * time.Minute,
		SweeperInterval:            1 * time.Minute,
		OAuthStateSecret:           "test-state-secret-thirty-two-bytes-min-len",
		WeChatAPIv3Key:             "0123456789abcdef0123456789abcdef", // 32 bytes
		WeChatPayMchID:             "1900000001",
		WeChatPayAppID:             "wx1900000109",
		WeChatPayMchPrivateKeyPath: "/k",
		WeChatPayMchCertPath:       "/c",
		WeChatPayNotifyURL:         "https://x/cb",
	}
}

// TestValidate_WeChatPayMchID exercises the production guard:
// WECHAT_PAY_MCH_ID is required when WECHAT_PAY_MOCK is unset / "0",
// but mock mode is allowed to leave it blank.
func TestValidate_WeChatPayMchID(t *testing.T) {
	t.Parallel()
	base := func() *Config {
		return &Config{
			DatabaseURL:         "postgres://x",
			RSAPrivate:          "priv",
			RSAPublic:           "pub",
			JWTAccessTTL:        15 * time.Minute,
			JWTRefreshTTL:       168 * time.Hour,
			OrderExpiryDuration: 30 * time.Minute,
			SweeperInterval:     1 * time.Minute,
			OAuthStateSecret:    "test-state-secret-thirty-two-bytes-min-len",
		}
	}

	t.Run("real mode, empty MCH_ID + APIv3Key set → error (symmetric rule)", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.WeChatPayMock = false
		cfg.WeChatPayMchID = ""
		cfg.WeChatAPIv3Key = "0123456789abcdef0123456789abcdef"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "WECHAT_PAY_MCH_ID") {
			t.Errorf("err = %v, want one mentioning WECHAT_PAY_MCH_ID", err)
		}
	})

	t.Run("real mode, populated MCH_ID → ok", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.WeChatPayMock = false
		cfg.WeChatAPIv3Key = ""
		cfg.WeChatPayMchID = ""
		if err := cfg.Validate(); err != nil {
			t.Errorf("deployments without WeChat Pay should pass, got: %v", err)
		}
	})

	t.Run("real mode + MCH_ID set, empty APIv3Key → error", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.WeChatPayMock = false
		cfg.WeChatAPIv3Key = ""
		cfg.WeChatPayMchID = "1900000001"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "WECHAT_PAY_API_V3_KEY") {
			t.Errorf("MCH_ID-without-APIv3Key should be rejected, got: %v", err)
		}
	})

	t.Run("real mode + APIv3Key, populated MCH_ID → ok", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.WeChatPayMock = false
		cfg.WeChatAPIv3Key = "0123456789abcdef0123456789abcdef"
		cfg.WeChatPayMchID = "1900000001"
		cfg.WeChatPayAppID = "wx1900000109"
		cfg.WeChatPayMchPrivateKeyPath = "/k"
		cfg.WeChatPayMchCertPath = "/c"
		cfg.WeChatPayNotifyURL = "https://x/cb"
		if err := cfg.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("mock mode, empty MCH_ID → ok", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.WeChatPayMock = true
		cfg.WeChatPayMchID = ""
		if err := cfg.Validate(); err != nil {
			t.Errorf("mock mode should allow empty MCH_ID, got: %v", err)
		}
	})
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
