package service

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

func ensureActiveSubscription(ctx context.Context, subRepo repo.SubscriptionRepo, userID string) error {
	sub, err := subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		return fmt.Errorf("check subscription: %v", err)
	}
	if sub == nil {
		return fmt.Errorf("subscription not active")
	}
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now()) {
		if err := subRepo.UpdateStatus(ctx, sub.ID, "expired"); err != nil {
			return fmt.Errorf("mark subscription expired: %v", err)
		}
		return fmt.Errorf("subscription expired")
	}
	return nil
}

type TokenService struct {
	PrivateKey  *rsa.PrivateKey
	PublicKey   *rsa.PublicKey
	AccessTTL   string
	RefreshTTL  string
	SessionRepo repo.SessionRepo
	SubRepo     repo.SubscriptionRepo
}

func NewTokenService(cfg *config.Config, sessionRepo repo.SessionRepo, subRepo repo.SubscriptionRepo) (*TokenService, error) {
	priv, err := loadPrivateKey(cfg.RSAPrivate)
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	pub, err := loadPublicKey(cfg.RSAPublic)
	if err != nil {
		return nil, fmt.Errorf("load public key: %w", err)
	}
	return &TokenService{
		PrivateKey:  priv,
		PublicKey:   pub,
		AccessTTL:  cfg.JWTAccessTTL,
		RefreshTTL: cfg.JWTRefreshTTL,
		SessionRepo: sessionRepo,
		SubRepo:     subRepo,
	}, nil
}

type TokenClaims struct {
	jwt.RegisteredClaims
	AppID string   `json:"app_id,omitempty"`
	Scope []string `json:"scope,omitempty"`
}

func (s *TokenService) SignAccessToken(userID, appID string, scope []string) (string, error) {
	claims := TokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    "yunhou-users",
			Audience:  jwt.ClaimStrings{appID},
		},
		AppID: appID,
		Scope: scope,
	}
	ttl := parseDuration(s.AccessTTL)
	now := time.Now()
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	claims.IssuedAt = jwt.NewNumericDate(now)

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(s.PrivateKey)
}

func (s *TokenService) VerifyAccessToken(tokenStr string) (*TokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TokenClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.PublicKey, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*TokenClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

func (s *TokenService) Refresh(ctx context.Context, refreshToken, appID string) (string, string, error) {
	session, err := s.SessionRepo.FindByRefreshToken(ctx, hashToken(refreshToken), "refresh")
	if err != nil {
		return "", "", fmt.Errorf("invalid or expired refresh token")
	}

	if err := ensureActiveSubscription(ctx, s.SubRepo, session.UserID); err != nil {
		return "", "", err
	}

	newAccess, err := s.SignAccessToken(session.UserID, session.AppID, session.Scope)
	if err != nil {
		return "", "", err
	}

	newRefresh, err := GenerateRefreshToken()
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	newSession := &model.Session{
		ID:           GenerateUUID(),
		UserID:       session.UserID,
		AppID:        session.AppID,
		SessionType:  "refresh",
		RefreshToken: hashToken(newRefresh),
		Scope:        session.Scope,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(parseDuration(s.RefreshTTL)),
	}
	if err := s.SessionRepo.RotateRefresh(ctx, session.ID, newSession); err != nil {
		return "", "", fmt.Errorf("rotate refresh token: %w", err)
	}

	return newAccess, newRefresh, nil
}

func (s *TokenService) JWKS() map[string]interface{} {
	kid := "yunhou-users-rsa"
	jwk := map[string]interface{}{
		"kty": "RSA",
		"kid": kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(s.PublicKey.N.Bytes()),
		"e":   encodeExponent(s.PublicKey.E),
	}
	return map[string]interface{}{
		"keys": []map[string]interface{}{jwk},
	}
}

func encodeExponent(e int) string {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(e))
	// Trim leading zero bytes
	i := 0
	for i < 3 && buf[i] == 0 {
		i++
	}
	return base64.RawURLEncoding.EncodeToString(buf[i:])
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not RSA private key")
	}
	return rsaKey, nil
}

func loadPublicKey(path string) (*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not RSA public key")
	}
	return rsaPub, nil
}
