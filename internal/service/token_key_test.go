package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/yunhou/users/internal/config"
)

// generateTestECDSAPub is a small helper that mints an ECDSA P-256 public
// key. Used to feed loadPublicKey a non-RSA key for the rejection path.
func generateTestECDSAPub(t *testing.T) *ecdsa.PublicKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return &priv.PublicKey
}

// TestNewTokenService_LoadsPKCS1Keys covers the PKCS1 path of loadPrivateKey.
// x509.ParsePKCS1PrivateKey succeeds first; loadPublicKey is then exercised
// against a matching PKIX-encoded PEM.
func TestNewTokenService_LoadsPKCS1Keys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")

	// PKCS1 PEM (BEGIN RSA PRIVATE KEY).
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	// PKIX PEM (BEGIN PUBLIC KEY) — paired with the PKCS1 private key.
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(pubPath, pubPEM, 0600); err != nil {
		t.Fatalf("write pub: %v", err)
	}

	cfg := &config.Config{
		RSAPrivate:  privPath,
		RSAPublic:   pubPath,
		JWTAccessTTL: 15 * 60 * 1e9, // 15m in ns (config package parses duration)
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	svc, err := NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	if svc.PrivateKey == nil || svc.PublicKey == nil {
		t.Fatal("expected non-nil keys after NewTokenService")
	}
}

// TestNewTokenService_LoadsPKCS8Keys covers the PKCS8 fallback path in
// loadPrivateKey (x509.ParsePKCS1PrivateKey fails, x509.ParsePKCS8PrivateKey
// succeeds).
func TestNewTokenService_LoadsPKCS8Keys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")

	// PKCS8 PEM (BEGIN PRIVATE KEY).
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(pubPath, pubPEM, 0600); err != nil {
		t.Fatalf("write pub: %v", err)
	}

	cfg := &config.Config{
		RSAPrivate:  privPath,
		RSAPublic:   pubPath,
		JWTAccessTTL: 15 * 60 * 1e9,
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	svc, err := NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	if svc.PrivateKey == nil {
		t.Fatal("expected non-nil private key after PKCS8 load")
	}
}

// TestNewTokenService_MissingPrivateKey covers the file-not-found error
// path in loadPrivateKey.
func TestNewTokenService_MissingPrivateKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		RSAPrivate:  filepath.Join(dir, "nope.pem"),
		RSAPublic:   filepath.Join(dir, "nope-pub.pem"),
		JWTAccessTTL: 15 * 60 * 1e9,
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	_, err := NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err == nil {
		t.Fatal("expected error when private key file is missing, got nil")
	}
}

// TestNewTokenService_InvalidPEMPrivateKey covers the no-PEM-block path
// (file exists but doesn't decode to a valid PEM block).
func TestNewTokenService_InvalidPEMPrivateKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	privPath := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(privPath, []byte("not-pem-data\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pubPath := filepath.Join(dir, "public.pem")
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubBytes, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	_ = os.WriteFile(pubPath, pubPEM, 0600)

	cfg := &config.Config{
		RSAPrivate:  privPath,
		RSAPublic:   pubPath,
		JWTAccessTTL: 15 * 60 * 1e9,
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	_, err := NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err == nil {
		t.Fatal("expected error for non-PEM private key, got nil")
	}
}

// TestNewTokenService_InvalidPublicKey covers the loadPublicKey error path.
func TestNewTokenService_InvalidPublicKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	_ = os.WriteFile(privPath, privPEM, 0600)
	_ = os.WriteFile(pubPath, []byte("not-pem"), 0600)

	cfg := &config.Config{
		RSAPrivate:  privPath,
		RSAPublic:   pubPath,
		JWTAccessTTL: 15 * 60 * 1e9,
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	_, err := NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err == nil {
		t.Fatal("expected error for non-PEM public key, got nil")
	}
}

// TestNewTokenService_PublicKeyNotRSA covers the "not RSA public key" branch
// in loadPublicKey (PEM parses fine but the inner key isn't *rsa.PublicKey).
func TestNewTokenService_PublicKeyNotRSA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	privPath := filepath.Join(dir, "private.pem")
	pubPath := filepath.Join(dir, "public.pem")

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	_ = os.WriteFile(privPath, privPEM, 0600)

	// Build a non-RSA public key (ECDSA) inside a PKIX PEM. The PEM parses
	// fine but loadPublicKey must reject it.
	ecdsaPub := generateTestECDSAPub(t)
	pubBytes, err := x509.MarshalPKIXPublicKey(ecdsaPub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey(ecdsa): %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	_ = os.WriteFile(pubPath, pubPEM, 0600)

	cfg := &config.Config{
		RSAPrivate:  privPath,
		RSAPublic:   pubPath,
		JWTAccessTTL: 15 * 60 * 1e9,
		JWTRefreshTTL: 168 * 3600 * 1e9,
	}
	_, err = NewTokenService(cfg, &mockSessionRepo{}, &mockSubscriptionRepo{})
	if err == nil {
		t.Fatal("expected error for non-RSA public key, got nil")
	}
}
