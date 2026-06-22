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
