package service

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yunhou/users/internal/model"
)

func TestTokenService_SignAndVerify(t *testing.T) {
	t.Parallel()

	priv, pub := generateTestRSAKeyPair()
	tokenSvc := &TokenService{
		PrivateKey: priv,
		PublicKey:  pub,
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 168 * time.Hour,
	}

	t.Run("sign and verify access token", func(t *testing.T) {
		userID := "user-123"
		appID := "yundian"
		scope := []string{"yundian", "yundash"}

		token, err := tokenSvc.SignAccessToken(userID, appID, scope)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		if token == "" {
			t.Fatal("token is empty")
		}

		claims, err := tokenSvc.VerifyAccessToken(token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}

		if claims.Subject != userID {
			t.Errorf("expected subject %s, got %s", userID, claims.Subject)
		}
		if claims.AppID != appID {
			t.Errorf("expected app_id %s, got %s", appID, claims.AppID)
		}
		if len(claims.Scope) != len(scope) {
			t.Errorf("expected scope len %d, got %d", len(scope), len(claims.Scope))
		}
	})

	t.Run("verify invalid token", func(t *testing.T) {
		_, err := tokenSvc.VerifyAccessToken("invalid-token")
		if err == nil {
			t.Error("expected error for invalid token")
		}
	})

	t.Run("verify token with wrong key", func(t *testing.T) {
		otherPriv, _ := generateTestRSAKeyPair()
		otherSvc := &TokenService{
			PrivateKey: otherPriv,
			PublicKey:  pub,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 168 * time.Hour,
		}

		token, _ := otherSvc.SignAccessToken("user", "app", nil)
		_, err := tokenSvc.VerifyAccessToken(token)
		if err == nil {
			t.Error("expected error for token signed with different key")
		}
	})

	t.Run("rejects token signed with non-RS256 algorithm", func(t *testing.T) {
		// Forge an HS256-signed token using the *public* key bytes as the
		// HMAC secret. If the verifier accepts any RSA-shaped method, this
		// is the classic algorithm-confusion attack.
		// x509.MarshalPKIXPublicKey + pem encode so we have []byte to pass.
		pubPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "PUBLIC KEY",
			Bytes: mustMarshalPKIX(t, pub),
		})
		hackedToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub":    "attacker",
			"iss":    "yunhou-users",
			"aud":    []string{"yundian"},
			"app_id": "yundian",
			"exp":    time.Now().Add(time.Hour).Unix(),
		})
		signed, err := hackedToken.SignedString(pubPEM)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if _, err := tokenSvc.VerifyAccessToken(signed); err == nil {
			t.Error("expected error for HS256 token, got nil")
		}
	})

	t.Run("rejects token with unexpected issuer", func(t *testing.T) {
		// Mint a valid token but rewrite the issuer claim to something
		// other than "yunhou-users". The verifier must refuse it.
		claims := TokenClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "user-1",
				Issuer:    "some-other-service",
				Audience:  jwt.ClaimStrings{"yundian"},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			AppID: "yundian",
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		signed, err := token.SignedString(priv)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if _, err := tokenSvc.VerifyAccessToken(signed); err == nil {
			t.Error("expected error for unexpected issuer, got nil")
		}
	})

	t.Run("rejects token whose audience doesn't match its app_id", func(t *testing.T) {
		// Token claims app_id=yundian but audience is something else.
		// Verifier must refuse — otherwise a token issued for one consumer
		// could be replayed at another.
		claims := TokenClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "user-1",
				Issuer:    "yunhou-users",
				Audience:  jwt.ClaimStrings{"yundash"},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			AppID: "yundian",
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		signed, err := token.SignedString(priv)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if _, err := tokenSvc.VerifyAccessToken(signed); err == nil {
			t.Error("expected error for audience mismatch, got nil")
		}
	})
}

func TestTokenService_JWKS(t *testing.T) {
	t.Parallel()

	priv, pub := generateTestRSAKeyPair()
	tokenSvc := &TokenService{
		PrivateKey: priv,
		PublicKey:  pub,
	}

	t.Run("JWKS returns correct format", func(t *testing.T) {
		jwks := tokenSvc.JWKS()

		keys, ok := jwks["keys"].([]map[string]interface{})
		if !ok {
			t.Fatal("keys not found or wrong type")
		}
		if len(keys) != 1 {
			t.Fatalf("expected 1 key, got %d", len(keys))
		}

		key := keys[0]
		if key["kty"] != "RSA" {
			t.Errorf("expected kty=RSA, got %v", key["kty"])
		}
		if key["kid"] != "yunhou-users-rsa" {
			t.Errorf("expected kid=yunhou-users-rsa, got %v", key["kid"])
		}
		if key["alg"] != "RS256" {
			t.Errorf("expected alg=RS256, got %v", key["alg"])
		}
		if key["use"] != "sig" {
			t.Errorf("expected use=sig, got %v", key["use"])
		}
		if key["n"] == nil || key["n"] == "" {
			t.Error("expected n to be set")
		}
		if key["e"] == nil || key["e"] == "" {
			t.Error("expected e to be set")
		}
	})
}

// TestParseDuration was removed: the helper it tested (`parseDuration` in
// token.go) was deleted along with `ensureActiveSubscription` and its tests.
// TTL parsing now happens once in config.Load via `parseDurationOr`, which
// has its own test in internal/config/config_test.go.

func TestTokenService_Refresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("refresh is deprecated — use AuthService", func(t *testing.T) {
		priv, pub := generateTestRSAKeyPair()
		sessionRepo := newMockSessionRepo()
		subRepo := newMockSubscriptionRepo()

		tokenSvc := &TokenService{
			PrivateKey:  priv,
			PublicKey:   pub,
			AccessTTL:   15 * time.Minute,
			RefreshTTL:  168 * time.Hour,
			SessionRepo: sessionRepo,
			SubRepo:     subRepo,
		}

		// Seed a valid session; TokenService.Refresh is now a thin
		// deprecation stub and must refuse to mint new tokens even when
		// everything else looks correct. Real refresh logic lives in
		// AuthService.RefreshToken (see auth_test.go).
		refreshToken := "valid-refresh-token"
		session := &model.Session{
			ID:           "session-1",
			UserID:       "user-1",
			AppID:        "yundian",
			SessionType:  "refresh",
			RefreshToken: hashToken(refreshToken),
			Scope:        []string{"yundian"},
			Revoked:      false,
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		sessionRepo.sessions[session.ID] = session
		sessionRepo.byToken[hashToken(refreshToken)] = session

		_, _, err := tokenSvc.Refresh(ctx, refreshToken, "yundian")
		if err == nil {
			t.Fatal("expected deprecation error, got nil")
		}
		if !strings.Contains(err.Error(), "deprecated") {
			t.Errorf("expected deprecation message, got %q", err.Error())
		}
	})

	t.Run("refresh with invalid token", func(t *testing.T) {
		priv, pub := generateTestRSAKeyPair()
		sessionRepo := newMockSessionRepo()
		subRepo := newMockSubscriptionRepo()

		tokenSvc := &TokenService{
			PrivateKey:  priv,
			PublicKey:   pub,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 168 * time.Hour,
			SessionRepo: sessionRepo,
			SubRepo:     subRepo,
		}

		_, _, err := tokenSvc.Refresh(ctx, "invalid-token", "yundian")
		if err == nil {
			t.Error("expected error for invalid token")
		}
	})

	t.Run("refresh with expired subscription", func(t *testing.T) {
		priv, pub := generateTestRSAKeyPair()
		sessionRepo := newMockSessionRepo()
		subRepo := newMockSubscriptionRepo()

		tokenSvc := &TokenService{
			PrivateKey:  priv,
			PublicKey:   pub,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 168 * time.Hour,
			SessionRepo: sessionRepo,
			SubRepo:     subRepo,
		}

		// Create session
		refreshToken := "valid-refresh-token"
		session := &model.Session{
			ID:           "session-2",
			UserID:       "user-2",
			AppID:        "yundian",
			SessionType:  "refresh",
			RefreshToken: hashToken(refreshToken),
			Scope:        []string{"yundian"},
			Revoked:      false,
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		sessionRepo.sessions[session.ID] = session
		sessionRepo.byToken[hashToken(refreshToken)] = session

		// Add expired subscription
		expiresAt := time.Now().Add(-1 * time.Hour) // expired
		subRepo.subs["sub-expired"] = &model.Subscription{
			ID:        "sub-expired",
			UserID:    "user-2",
			PlanID:    "monthly",
			Status:    "active",
			ExpiresAt: &expiresAt,
		}
		subRepo.byUserID["user-2"] = subRepo.subs["sub-expired"]

		_, _, err := tokenSvc.Refresh(ctx, refreshToken, "yundian")
		if err == nil {
			t.Error("expected error for expired subscription")
		}
	})
}

func TestLoadPrivateKey(t *testing.T) {
	t.Run("load valid PKCS1 private key", func(t *testing.T) {
		priv, _ := generateTestRSAKeyPair()
		tmpFile := t.TempDir() + "/test_key.pem"
		writePrivateKeyToFile(t, tmpFile, priv)

		loaded, err := loadPrivateKey(tmpFile)
		if err != nil {
			t.Fatalf("failed to load private key: %v", err)
		}
		if loaded == nil {
			t.Fatal("expected non-nil key")
		}
	})

	t.Run("load non-existent file", func(t *testing.T) {
		_, err := loadPrivateKey("/nonexistent/path/key.pem")
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})
}

func TestLoadPublicKey(t *testing.T) {
	t.Run("load valid public key", func(t *testing.T) {
		_, pub := generateTestRSAKeyPair()
		tmpFile := t.TempDir() + "/test_pub.pem"
		writePublicKeyToFile(t, tmpFile, pub)

		loaded, err := loadPublicKey(tmpFile)
		if err != nil {
			t.Fatalf("failed to load public key: %v", err)
		}
		if loaded == nil {
			t.Fatal("expected non-nil key")
		}
	})

	t.Run("load non-existent file", func(t *testing.T) {
		_, err := loadPublicKey("/nonexistent/path/key.pem")
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})
}

// Helper functions for key file writing
func writePrivateKeyToFile(t *testing.T, path string, key *rsa.PrivateKey) {
	t.Helper()
	privBytes := x509.MarshalPKCS1PrivateKey(key)
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes})
	if err := os.WriteFile(path, pemData, 0600); err != nil {
		t.Fatalf("failed to write private key: %v", err)
	}
}

func writePublicKeyToFile(t *testing.T, path string, key *rsa.PublicKey) {
	t.Helper()
	pubBytes, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(path, pemData, 0644); err != nil {
		t.Fatalf("failed to write public key: %v", err)
	}
}

// mustMarshalPKIX returns the PKIX-marshaled bytes for a public key, failing
// the test if marshaling fails. Used by the algorithm-confusion test.
func mustMarshalPKIX(t *testing.T, key *rsa.PublicKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return b
}
