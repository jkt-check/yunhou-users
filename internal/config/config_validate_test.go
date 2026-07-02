package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidate_HappyPath(t *testing.T) {
	c := &Config{
		DatabaseURL:        "postgres://localhost/db",
		RSAPrivate:         "/tmp/priv.pem",
		RSAPublic:          "/tmp/pub.pem",
		JWTAccessTTL:       15 * time.Minute,
		JWTRefreshTTL:      168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:    1 * time.Minute,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_MissingDatabaseURL(t *testing.T) {
	c := &Config{
		RSAPrivate: "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL: 15 * time.Minute, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("err = %v, want missing DATABASE_URL", err)
	}
}

func TestValidate_MissingRSAKeys(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "RSA_") {
		t.Errorf("err = %v, want missing RSA", err)
	}
}

func TestValidate_JWTAccessTTLNotPositive(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        0, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "JWT_ACCESS_TTL") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate_JWTRefreshTTLTooShort(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 15 * time.Minute,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "JWT_REFRESH_TTL must be strictly greater") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate_JWTRefreshTTLTooLong(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 366 * 24 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "at most 365 days") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate_OrderExpiryNotPositive(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 0, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "ORDER_EXPIRY_DURATION") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate_SweeperNotPositive(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute, SweeperInterval: 0,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SWEEPER_INTERVAL") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate_SweeperNotLessThanExpiry(t *testing.T) {
	c := &Config{
		DatabaseURL:         "postgres://localhost/db",
		RSAPrivate:          "/tmp/priv.pem", RSAPublic: "/tmp/pub.pem",
		JWTAccessTTL:        15 * time.Minute, JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 1 * time.Minute, SweeperInterval: 1 * time.Minute,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "strictly less") {
		t.Errorf("err = %v", err)
	}
}

func TestParseDurationOr_Invalid(t *testing.T) {
	cases := []struct {
		in       string
		fallback time.Duration
		want     time.Duration
	}{
		{"not-a-duration", 30 * time.Second, 30 * time.Second},
		{"", 1 * time.Minute, 1 * time.Minute},
		{"5s", 1 * time.Minute, 5 * time.Second},
		{"2h", 1 * time.Minute, 2 * time.Hour},
	}
	for _, c := range cases {
		got := parseDurationOr(c.in, c.fallback)
		if got != c.want {
			t.Errorf("parseDurationOr(%q, %v) = %v, want %v", c.in, c.fallback, got, c.want)
		}
	}
}

// setEnv sets env var for the duration of the test, restoring on cleanup.
func setEnv(t *testing.T, k, v string) {
	t.Helper()
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

func TestLoad_PaymentChannelDefaults(t *testing.T) {
	// Verify the payment-channel fields default to "" when env vars are unset.
	for _, k := range []string{
		"STRIPE_WEBHOOK_SECRET", "WECHAT_PAY_API_V3_KEY",
		"ALIPAY_PUBLIC_KEY_PATH", "LEMONSQUEEZY_WEBHOOK_SECRET",
		"ORDER_EXPIRY_DURATION", "SWEEPER_INTERVAL",
	} {
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
	if cfg.StripeWebhookSecret != "" {
		t.Errorf("StripeWebhookSecret = %q", cfg.StripeWebhookSecret)
	}
	if cfg.WeChatAPIv3Key != "" {
		t.Errorf("WeChatAPIv3Key = %q", cfg.WeChatAPIv3Key)
	}
	if cfg.AlipayPublicKeyPath != "" {
		t.Errorf("AlipayPublicKeyPath = %q", cfg.AlipayPublicKeyPath)
	}
	if cfg.LemonSqueezyWebhookSecret != "" {
		t.Errorf("LemonSqueezyWebhookSecret = %q", cfg.LemonSqueezyWebhookSecret)
	}
	if cfg.OrderExpiryDuration != 30*time.Minute {
		t.Errorf("OrderExpiryDuration = %v, want 30m", cfg.OrderExpiryDuration)
	}
	if cfg.SweeperInterval != 1*time.Minute {
		t.Errorf("SweeperInterval = %v, want 1m", cfg.SweeperInterval)
	}
}

func TestLoad_PaymentChannelOverrides(t *testing.T) {
	setEnv(t, "STRIPE_WEBHOOK_SECRET", "whsec_x")
	setEnv(t, "WECHAT_PAY_API_V3_KEY", "12345678901234567890123456789012") // 32 bytes
	setEnv(t, "ALIPAY_PUBLIC_KEY_PATH", "/etc/alipay.pem")
	setEnv(t, "LEMONSQUEEZY_WEBHOOK_SECRET", "lssec_x")
	setEnv(t, "ORDER_EXPIRY_DURATION", "5m")
	setEnv(t, "SWEEPER_INTERVAL", "30s")
	cfg := Load()
	if cfg.StripeWebhookSecret != "whsec_x" {
		t.Errorf("StripeWebhookSecret = %q", cfg.StripeWebhookSecret)
	}
	if cfg.WeChatAPIv3Key != "12345678901234567890123456789012" {
		t.Errorf("WeChatAPIv3Key = %q", cfg.WeChatAPIv3Key)
	}
	if cfg.AlipayPublicKeyPath != "/etc/alipay.pem" {
		t.Errorf("AlipayPublicKeyPath = %q", cfg.AlipayPublicKeyPath)
	}
	if cfg.LemonSqueezyWebhookSecret != "lssec_x" {
		t.Errorf("LemonSqueezyWebhookSecret = %q", cfg.LemonSqueezyWebhookSecret)
	}
	if cfg.OrderExpiryDuration != 5*time.Minute {
		t.Errorf("OrderExpiryDuration = %v, want 5m", cfg.OrderExpiryDuration)
	}
	if cfg.SweeperInterval != 30*time.Second {
		t.Errorf("SweeperInterval = %v, want 30s", cfg.SweeperInterval)
	}
}