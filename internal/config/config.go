package config

import "os"

type Config struct {
	Port        string
	DatabaseURL string
	RSAPrivate  string
	RSAPublic   string

	GitHubClientID     string
	GitHubClientSecret string
	GitHubCallbackURL   string
	GoogleClientID     string
	GoogleClientSecret string
	WeChatClientID     string
	WeChatClientSecret string

	JWTAccessTTL  string
	JWTRefreshTTL string
}

func Load() *Config {
	return &Config{
		Port:        envOr("PORT", "8080"),
		DatabaseURL: envOr("DATABASE_URL", "postgres://localhost/yunhou_users?sslmode=disable"),
		RSAPrivate:  envOr("RSA_PRIVATE_KEY_PATH", "keys/private.pem"),
		RSAPublic:   envOr("RSA_PUBLIC_KEY_PATH", "keys/public.pem"),

		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubCallbackURL:   os.Getenv("GITHUB_CALLBACK_URL"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		WeChatClientID:     os.Getenv("WECHAT_CLIENT_ID"),
		WeChatClientSecret: os.Getenv("WECHAT_CLIENT_SECRET"),

		JWTAccessTTL:  envOr("JWT_ACCESS_TTL", "15m"),
		JWTRefreshTTL: envOr("JWT_REFRESH_TTL", "168h"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
