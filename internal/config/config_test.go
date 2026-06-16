package config

import (
	"os"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear all relevant env vars so defaults are used.
	envVars := []string{
		"PORT", "DATABASE_URL", "RSA_PRIVATE_KEY_PATH", "RSA_PUBLIC_KEY_PATH",
		"JWT_ACCESS_TTL", "JWT_REFRESH_TTL",
		"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET",
		"WECHAT_CLIENT_ID", "WECHAT_CLIENT_SECRET",
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
	if cfg.JWTAccessTTL != "15m" {
		t.Errorf("JWTAccessTTL: got %q, want %q", cfg.JWTAccessTTL, "15m")
	}
	if cfg.JWTRefreshTTL != "168h" {
		t.Errorf("JWTRefreshTTL: got %q, want %q", cfg.JWTRefreshTTL, "168h")
	}
	// OAuth fields should be empty when env vars are unset
	if cfg.GitHubClientID != "" {
		t.Errorf("GitHubClientID: got %q, want empty", cfg.GitHubClientID)
	}
	if cfg.GitHubClientSecret != "" {
		t.Errorf("GitHubClientSecret: got %q, want empty", cfg.GitHubClientSecret)
	}
	if cfg.GoogleClientID != "" {
		t.Errorf("GoogleClientID: got %q, want empty", cfg.GoogleClientID)
	}
	if cfg.GoogleClientSecret != "" {
		t.Errorf("GoogleClientSecret: got %q, want empty", cfg.GoogleClientSecret)
	}
	if cfg.WeChatClientID != "" {
		t.Errorf("WeChatClientID: got %q, want empty", cfg.WeChatClientID)
	}
	if cfg.WeChatClientSecret != "" {
		t.Errorf("WeChatClientSecret: got %q, want empty", cfg.WeChatClientSecret)
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
		"GOOGLE_CLIENT_ID":    "go-id",
		"GOOGLE_CLIENT_SECRET": "go-secret",
		"WECHAT_CLIENT_ID":    "wc-id",
		"WECHAT_CLIENT_SECRET": "wc-secret",
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
	if cfg.JWTAccessTTL != "30m" {
		t.Errorf("JWTAccessTTL: got %q, want %q", cfg.JWTAccessTTL, "30m")
	}
	if cfg.JWTRefreshTTL != "72h" {
		t.Errorf("JWTRefreshTTL: got %q, want %q", cfg.JWTRefreshTTL, "72h")
	}
	if cfg.GitHubClientID != "gh-id" {
		t.Errorf("GitHubClientID: got %q, want %q", cfg.GitHubClientID, "gh-id")
	}
	if cfg.GitHubClientSecret != "gh-secret" {
		t.Errorf("GitHubClientSecret: got %q, want %q", cfg.GitHubClientSecret, "gh-secret")
	}
	if cfg.GoogleClientID != "go-id" {
		t.Errorf("GoogleClientID: got %q, want %q", cfg.GoogleClientID, "go-id")
	}
	if cfg.GoogleClientSecret != "go-secret" {
		t.Errorf("GoogleClientSecret: got %q, want %q", cfg.GoogleClientSecret, "go-secret")
	}
	if cfg.WeChatClientID != "wc-id" {
		t.Errorf("WeChatClientID: got %q, want %q", cfg.WeChatClientID, "wc-id")
	}
	if cfg.WeChatClientSecret != "wc-secret" {
		t.Errorf("WeChatClientSecret: got %q, want %q", cfg.WeChatClientSecret, "wc-secret")
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
