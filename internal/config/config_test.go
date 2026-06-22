package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear all relevant env vars so defaults are used.
	envVars := []string{
		"PORT", "DATABASE_URL", "RSA_PRIVATE_KEY_PATH", "RSA_PUBLIC_KEY_PATH",
		"JWT_ACCESS_TTL", "JWT_REFRESH_TTL",
		"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET",
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
	if cfg.GoogleClientID != "" {
		t.Errorf("GoogleClientID: got %q, want empty", cfg.GoogleClientID)
	}
	if cfg.GoogleClientSecret != "" {
		t.Errorf("GoogleClientSecret: got %q, want empty", cfg.GoogleClientSecret)
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
	if cfg.GoogleClientID != "go-id" {
		t.Errorf("GoogleClientID: got %q, want %q", cfg.GoogleClientID, "go-id")
	}
	if cfg.GoogleClientSecret != "go-secret" {
		t.Errorf("GoogleClientSecret: got %q, want %q", cfg.GoogleClientSecret, "go-secret")
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
