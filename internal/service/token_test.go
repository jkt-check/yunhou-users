package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/model"
)

func TestNewTokenService(t *testing.T) {
	t.Parallel()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "private.pem")
	pubPath := filepath.Join(tmpDir, "public.pem")

	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privBlock), 0600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(pubBlock), 0600); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	cfg := &config.Config{
		RSAPrivate:    privPath,
		RSAPublic:     pubPath,
		JWTAccessTTL:  "15m",
		JWTRefreshTTL: "168h",
	}

	svc, err := NewTokenService(cfg, newMockSessionRepo(), newMockSubscriptionRepo())
	if err != nil {
		t.Fatalf("NewTokenService error: %v", err)
	}
	if svc.PrivateKey == nil {
		t.Error("expected PrivateKey to be set")
	}
	if svc.PublicKey == nil {
		t.Error("expected PublicKey to be set")
	}

	token, err := svc.SignAccessToken("user-1", "app-1", []string{"app:read"})
	if err != nil {
		t.Fatalf("SignAccessToken error: %v", err)
	}
	claims, err := svc.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("VerifyAccessToken error: %v", err)
	}
	if claims.Subject != "user-1" {
		t.Errorf("expected subject user-1, got %s", claims.Subject)
	}
}

func TestNewTokenService_InvalidPrivateKey(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "bad.pem")
	pubPath := filepath.Join(tmpDir, "pub.pem")

	if err := os.WriteFile(privPath, []byte("not valid"), 0600); err != nil {
		t.Fatal(err)
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubDER, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(pubBlock), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{RSAPrivate: privPath, RSAPublic: pubPath, JWTAccessTTL: "15m", JWTRefreshTTL: "168h"}
	_, err := NewTokenService(cfg, newMockSessionRepo(), newMockSubscriptionRepo())
	if err == nil {
		t.Error("expected error for invalid private key")
	}
	if !strings.Contains(err.Error(), "load private key") {
		t.Errorf("expected 'load private key' in error, got %q", err.Error())
	}
}

func TestNewTokenService_InvalidPublicKey(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "priv.pem")
	pubPath := filepath.Join(tmpDir, "bad.pem")

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privBlock), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, []byte("not valid"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{RSAPrivate: privPath, RSAPublic: pubPath, JWTAccessTTL: "15m", JWTRefreshTTL: "168h"}
	_, err := NewTokenService(cfg, newMockSessionRepo(), newMockSubscriptionRepo())
	if err == nil {
		t.Error("expected error for invalid public key")
	}
	if !strings.Contains(err.Error(), "load public key") {
		t.Errorf("expected 'load public key' in error, got %q", err.Error())
	}
}

func TestNewTokenService_MissingKeyFiles(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RSAPrivate: "/no/path/private.pem", RSAPublic: "/no/path/public.pem", JWTAccessTTL: "15m", JWTRefreshTTL: "168h"}
	_, err := NewTokenService(cfg, newMockSessionRepo(), newMockSubscriptionRepo())
	if err == nil {
		t.Error("expected error for missing key files")
	}
}

func TestNewTokenService_PublicKeyNotRSA(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "priv.pem")
	pubPath := filepath.Join(tmpDir, "pub.pem")

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privBlock), 0600); err != nil {
		t.Fatal(err)
	}

	// Write an EC public key as the "public" key file.
	// x509.ParsePKIXPublicKey will parse it but return *ecdsa.PublicKey, not *rsa.PublicKey,
	// triggering the "not RSA public key" type assertion failure.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	ecPubDER, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal EC public key: %v", err)
	}
	ecPubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: ecPubDER}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(ecPubBlock), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{RSAPrivate: privPath, RSAPublic: pubPath, JWTAccessTTL: "15m", JWTRefreshTTL: "168h"}
	_, err = NewTokenService(cfg, newMockSessionRepo(), newMockSubscriptionRepo())
	if err == nil {
		t.Error("expected error for non-RSA public key")
	}
	if !strings.Contains(err.Error(), "not RSA public key") {
		t.Errorf("expected 'not RSA public key' in error, got %q", err.Error())
	}
}

func TestSignAccessToken(t *testing.T) {
	t.Parallel()

	ts := newTokenServiceWithKeys(newMockSessionRepo(), newMockSubscriptionRepo())

	tests := []struct {
		name   string
		userID string
		appID  string
		scope  []string
	}{
		{name: "basic signing", userID: "user-1", appID: "app-1", scope: []string{"app:read", "app:write"}},
		{name: "empty scope", userID: "user-2", appID: "app-2", scope: []string{}},
		{name: "nil scope", userID: "user-3", appID: "app-3", scope: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ts.SignAccessToken(tc.userID, tc.appID, tc.scope)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if token == "" {
				t.Error("expected non-empty token")
			}

			claims, err := ts.VerifyAccessToken(token)
			if err != nil {
				t.Fatalf("failed to verify signed token: %v", err)
			}
			if claims.Subject != tc.userID {
				t.Errorf("expected subject %q, got %q", tc.userID, claims.Subject)
			}
			if claims.AppID != tc.appID {
				t.Errorf("expected app_id %q, got %q", tc.appID, claims.AppID)
			}
			if claims.Issuer != "yunhou-users" {
				t.Errorf("expected issuer yunhou-users, got %q", claims.Issuer)
			}
			if claims.ExpiresAt == nil {
				t.Error("expected non-nil ExpiresAt")
			}
			if claims.IssuedAt == nil {
				t.Error("expected non-nil IssuedAt")
			}
		})
	}
}

func TestVerifyAccessToken(t *testing.T) {
	t.Parallel()

	ts := newTokenServiceWithKeys(newMockSessionRepo(), newMockSubscriptionRepo())

	tests := []struct {
		name    string
		setup   func() string
		wantErr bool
	}{
		{name: "valid token", setup: func() string {
			tok, _ := ts.SignAccessToken("user-1", "app-1", []string{"app:read"})
			return tok
		}},
		{name: "tampered token", setup: func() string {
			tok, _ := ts.SignAccessToken("user-1", "app-1", []string{"app:read"})
			return tok + "x"
		}, wantErr: true},
		{name: "empty token", setup: func() string { return "" }, wantErr: true},
		{name: "different key", setup: func() string {
			other := newTokenServiceWithKeys(newMockSessionRepo(), newMockSubscriptionRepo())
			tok, _ := other.SignAccessToken("user-x", "app-x", []string{"app:read"})
			return tok
		}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokenStr := tc.setup()
			_, err := ts.VerifyAccessToken(tokenStr)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	createSession := func(ssr *mockSessionRepo, userID, appID, refreshToken string, scope []string, expiresAt time.Time) {
		s := &model.Session{
			ID:           GenerateUUID(),
			UserID:       userID,
			AppID:        appID,
			RefreshToken: hashToken(refreshToken),
			Scope:        scope,
			Revoked:      false,
			ExpiresAt:    expiresAt,
		}
		ssr.sessions[s.ID] = s
		ssr.byToken[hashToken(refreshToken)] = s // key by hashed token since FindByRefreshToken receives the hash
	}

	t.Run("valid refresh", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		userID := "user-1"
		appID := "app-1"
		refreshToken := "valid-refresh-token"
		expiresAt := timeNow().Add(7 * 24 * time.Hour)

		createSession(ssr, userID, appID, refreshToken, []string{"app:read"}, expiresAt)

		sub := &model.Subscription{ID: GenerateUUID(), UserID: userID, AppID: appID, Plan: "pro", Status: "active"}
		sr.Create(ctx, sub)

		newAccess, newRefresh, err := ts.Refresh(ctx, refreshToken, appID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if newAccess == "" {
			t.Error("expected non-empty access token")
		}
		if newRefresh == "" {
			t.Error("expected non-empty refresh token")
		}

		claims, err := ts.VerifyAccessToken(newAccess)
		if err != nil {
			t.Errorf("failed to verify new access token: %v", err)
		}
		if claims.Subject != userID {
			t.Errorf("expected subject %q, got %q", userID, claims.Subject)
		}

		// Old session should be revoked
		oldSess := ssr.byToken[hashToken(refreshToken)]
		if oldSess != nil && !oldSess.Revoked {
			t.Error("expected old session to be revoked")
		}

		// New session should exist
		found := false
		for _, s := range ssr.sessions {
			if s.RefreshToken == hashToken(newRefresh) && !s.Revoked {
				found = true
			}
		}
		if !found {
			t.Error("expected new session for new refresh token")
		}
	})

	t.Run("expired session", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		createSession(ssr, "user-2", "app-2", "expired-refresh", []string{"app:read"}, timeNow().Add(-1*time.Hour))

		_, _, err := ts.Refresh(ctx, "expired-refresh", "app-2")
		if err == nil {
			t.Error("expected error for expired session")
		}
		if !strings.Contains(err.Error(), "invalid or expired refresh token") {
			t.Errorf("unexpected error: %q", err.Error())
		}
	})

	t.Run("inactive subscription", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		userID := "user-3"
		appID := "app-3"
		refreshToken := "inactive-sub-refresh"

		createSession(ssr, userID, appID, refreshToken, []string{"app:read"}, timeNow().Add(7*24*time.Hour))

		sub := &model.Subscription{ID: GenerateUUID(), UserID: userID, AppID: appID, Plan: "pro", Status: "cancelled"}
		sr.Create(ctx, sub)

		_, _, err := ts.Refresh(ctx, refreshToken, appID)
		if err == nil {
			t.Error("expected error for inactive subscription")
		}
		if !strings.Contains(err.Error(), "subscription not active") {
			t.Errorf("unexpected error: %q", err.Error())
		}
	})

	t.Run("no subscription found", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		createSession(ssr, "user-4", "app-4", "no-sub-refresh", []string{"app:read"}, timeNow().Add(7*24*time.Hour))

		_, _, err := ts.Refresh(ctx, "no-sub-refresh", "app-4")
		if err == nil {
			t.Error("expected error for missing subscription")
		}
		if !strings.Contains(err.Error(), "subscription not active") {
			t.Errorf("unexpected error: %q", err.Error())
		}
	})

	t.Run("session creation failure", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		userID := "user-5"
		appID := "app-5"
		refreshToken := "session-fail-refresh"

		createSession(ssr, userID, appID, refreshToken, []string{"app:read"}, timeNow().Add(7*24*time.Hour))

		sub := &model.Subscription{ID: GenerateUUID(), UserID: userID, AppID: appID, Plan: "free", Status: "active"}
		sr.Create(ctx, sub)

		ssr.failAfter = 1 // the next Create call will fail

		_, _, err := ts.Refresh(ctx, refreshToken, appID)
		if err == nil {
			t.Error("expected error for session creation failure")
		}
	})

	t.Run("wrong app", func(t *testing.T) {
		ssr := newMockSessionRepo()
		sr := newMockSubscriptionRepo()
		ts := newTokenServiceWithKeys(ssr, sr)

		createSession(ssr, "user-6", "app-6", "wrong-app-refresh", []string{"app:read"}, timeNow().Add(7*24*time.Hour))

		_, _, err := ts.Refresh(ctx, "wrong-app-refresh", "other-app")
		if err == nil {
			t.Error("expected error for wrong app")
		}
		if !strings.Contains(err.Error(), "not issued for this app") {
			t.Errorf("unexpected error: %q", err.Error())
		}
	})
}

func TestJWKS(t *testing.T) {
	t.Parallel()

	ts := newTokenServiceWithKeys(newMockSessionRepo(), newMockSubscriptionRepo())

	jwks := ts.JWKS()

	keys, ok := jwks["keys"].([]map[string]interface{})
	if !ok || len(keys) != 1 {
		t.Fatalf("expected 1 key in JWKS, got %v", jwks)
	}

	jwk := keys[0]

	if jwk["kty"] != "RSA" {
		t.Errorf("expected kty=RSA, got %v", jwk["kty"])
	}
	if jwk["kid"] != "yunhou-users-rsa" {
		t.Errorf("expected kid=yunhou-users-rsa, got %v", jwk["kid"])
	}
	if jwk["alg"] != "RS256" {
		t.Errorf("expected alg=RS256, got %v", jwk["alg"])
	}
	if jwk["use"] != "sig" {
		t.Errorf("expected use=sig, got %v", jwk["use"])
	}
	if jwk["e"] != "AQAB" {
		t.Errorf("expected e=AQAB, got %v", jwk["e"])
	}

	n, ok := jwk["n"].(string)
	if !ok || n == "" {
		t.Errorf("expected non-empty n, got %v", jwk["n"])
	}

	expectedN := base64.RawURLEncoding.EncodeToString(ts.PublicKey.N.Bytes())
	if n != expectedN {
		t.Errorf("n mismatch: got %s, want %s", n, expectedN)
	}
}

func TestSignAndVerify_Roundtrip(t *testing.T) {
	t.Parallel()

	ts := newTokenServiceWithKeys(newMockSessionRepo(), newMockSubscriptionRepo())

	userID := "roundtrip-user"
	appID := "roundtrip-app"
	scope := []string{"app:read", "app:write"}

	token, err := ts.SignAccessToken(userID, appID, scope)
	if err != nil {
		t.Fatalf("SignAccessToken error: %v", err)
	}

	claims, err := ts.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("VerifyAccessToken error: %v", err)
	}

	if claims.Subject != userID {
		t.Errorf("Subject mismatch: got %q, want %q", claims.Subject, userID)
	}
	if claims.AppID != appID {
		t.Errorf("AppID mismatch: got %q, want %q", claims.AppID, appID)
	}
	if claims.Issuer != "yunhou-users" {
		t.Errorf("Issuer mismatch: got %q, want %q", claims.Issuer, "yunhou-users")
	}
}

func TestSignAccessToken_InvalidTTL(t *testing.T) {
	t.Parallel()

	priv, _ := generateTestRSAKeyPair()
	ts := &TokenService{
		PrivateKey:  priv,
		PublicKey:   &priv.PublicKey,
		AccessTTL:   "not-a-duration",
		RefreshTTL:  "168h",
		SessionRepo: newMockSessionRepo(),
		SubRepo:     newMockSubscriptionRepo(),
	}

	token, err := ts.SignAccessToken("user-1", "app-1", []string{"app:read"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token with invalid TTL (fallback)")
	}

	claims, err := ts.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("failed to verify token: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set")
	}
}

func TestVerifyAccessToken_ExpiredToken(t *testing.T) {
	t.Parallel()

	priv, _ := generateTestRSAKeyPair()
	ts := &TokenService{
		PrivateKey:  priv,
		PublicKey:   &priv.PublicKey,
		AccessTTL:   "1ns",
		RefreshTTL:  "168h",
		SessionRepo: newMockSessionRepo(),
		SubRepo:     newMockSubscriptionRepo(),
	}

	token, err := ts.SignAccessToken("user-exp", "app-exp", []string{"app:read"})
	if err != nil {
		t.Fatalf("SignAccessToken error: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	_, err = ts.VerifyAccessToken(token)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestOAuthHTTPClient(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	client := p.httpClient()
	if client == nil {
		t.Error("expected non-nil http client")
	}
}

func TestExchangeGitHubCode_NetworkError(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Port: "8080", GitHubClientID: "id", GitHubClientSecret: "secret"}
	p := NewOAuthProvider(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := p.exchangeGitHubCode(ctx, "code")
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestGetGitHubUser_NetworkError(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := p.getGitHubUser(ctx, "token")
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestGetGitHubPrimaryEmail_NetworkError(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := p.getGitHubPrimaryEmail(ctx, "token")
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

// Tests below modify http.DefaultTransport and MUST NOT use t.Parallel()

func TestGetGitHubPrimaryEmail_NoPrimaryFallback(t *testing.T) {
	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	emailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"email":"first@example.com","primary":false},{"email":"second@example.com","primary":false}]`)
	}))
	defer emailServer.Close()

	transport := &singleRouteTransport{
		url:     "https://api.github.com/user/emails",
		handler: emailServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	email, err := p.getGitHubPrimaryEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if email != "first@example.com" {
		t.Errorf("expected first@example.com as fallback, got %s", email)
	}
}

func TestGetGitHubPrimaryEmail_EmptyList(t *testing.T) {
	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	emailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[]`)
	}))
	defer emailServer.Close()

	transport := &singleRouteTransport{
		url:     "https://api.github.com/user/emails",
		handler: emailServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.getGitHubPrimaryEmail(context.Background(), "token")
	if err == nil {
		t.Error("expected error for empty email list")
	}
	if !strings.Contains(err.Error(), "no email found") {
		t.Errorf("expected 'no email found' error, got %q", err.Error())
	}
}

func TestExchangeGitHubCode_InvalidResponse(t *testing.T) {
	cfg := &config.Config{Port: "8080", GitHubClientID: "id", GitHubClientSecret: "secret"}
	p := NewOAuthProvider(cfg, nil)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{invalid json`)
	}))
	defer tokenServer.Close()

	transport := &singleRouteTransport{
		url:     "https://github.com/login/oauth/access_token",
		handler: tokenServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.exchangeGitHubCode(context.Background(), "code")
	if err == nil {
		t.Error("expected error for invalid token response")
	}
	if !strings.Contains(err.Error(), "invalid token response") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestGetGitHubUser_InvalidResponse(t *testing.T) {
	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{invalid json`)
	}))
	defer userServer.Close()

	transport := &singleRouteTransport{
		url:     "https://api.github.com/user",
		handler: userServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.getGitHubUser(context.Background(), "token")
	if err == nil {
		t.Error("expected error for invalid user response")
	}
	if !strings.Contains(err.Error(), "invalid user response") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestExchangeGitHubCode_EmptyAccessToken(t *testing.T) {
	cfg := &config.Config{Port: "8080", GitHubClientID: "id", GitHubClientSecret: "secret"}
	p := NewOAuthProvider(cfg, nil)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token_type":"bearer"}`)
	}))
	defer tokenServer.Close()

	transport := &singleRouteTransport{
		url:     "https://github.com/login/oauth/access_token",
		handler: tokenServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.exchangeGitHubCode(context.Background(), "code")
	if err == nil {
		t.Error("expected error for empty access token")
	}
	if !strings.Contains(err.Error(), "no access token in response") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestGetGitHubPrimaryEmail_InvalidResponse(t *testing.T) {
	cfg := &config.Config{Port: "8080"}
	p := NewOAuthProvider(cfg, nil)

	emailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{invalid json`)
	}))
	defer emailServer.Close()

	transport := &singleRouteTransport{
		url:     "https://api.github.com/user/emails",
		handler: emailServer.Config.Handler,
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.getGitHubPrimaryEmail(context.Background(), "token")
	if err == nil {
		t.Error("expected error for invalid email response")
	}
	if !strings.Contains(err.Error(), "invalid email response") {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

type singleRouteTransport struct {
	url     string
	handler http.Handler
}

func (s *singleRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() == s.url {
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		return rec.Result(), nil
	}
	return nil, fmt.Errorf("no route for %s", req.URL.String())
}
