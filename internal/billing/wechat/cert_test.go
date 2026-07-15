package wechat

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPrivateKey_PKCS1(t *testing.T) {
	dir := t.TempDir()
	pkcs1Path := filepath.Join(dir, "pkcs1.pem")
	writePKCS1Key(t, pkcs1Path)

	key, err := LoadPrivateKey(pkcs1Path)
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	if key == nil || key.N == nil {
		t.Fatalf("LoadPrivateKey: nil or zero key")
	}
}

func TestLoadPrivateKey_PKCS8(t *testing.T) {
	dir := t.TempDir()
	pkcs8Path := filepath.Join(dir, "pkcs8.pem")
	writePKCS8Key(t, pkcs8Path)

	key, err := LoadPrivateKey(pkcs8Path)
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	if key == nil || key.N == nil {
		t.Fatalf("LoadPrivateKey: nil or zero key")
	}
}

func TestLoadPrivateKey_BadPEM(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPath, []byte("not a pem"), 0600); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if _, err := LoadPrivateKey(badPath); err == nil {
		t.Fatalf("expected error on garbage PEM")
	}
}

func TestLoadCertSerial_DecimalString(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	writeSelfSignedCert(t, certPath)

	serial, err := LoadCertSerial(certPath)
	if err != nil {
		t.Fatalf("LoadCertSerial: %v", err)
	}
	if serial == "" {
		t.Fatalf("LoadCertSerial: empty serial")
	}
	// Must be all decimal digits (no "0x", no sign, no hex letters).
	for _, r := range serial {
		if r < '0' || r > '9' {
			t.Fatalf("LoadCertSerial: non-decimal char %q in %q", r, serial)
		}
	}
}

func TestLoadCertSerial_BadPEM(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPath, []byte("not a pem"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadCertSerial(badPath); err == nil {
		t.Fatalf("expected error on garbage PEM")
	}
}

// --- helpers ---

func writePKCS1Key(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("write pkcs1: %v", err)
	}
}

func writePKCS8Key(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("write pkcs8: %v", err)
	}
}

func writeSelfSignedCert(t *testing.T, certPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1234567890),
		Subject:      pkix.Name{CommonName: "yunhou-users-test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	block := &pem.Block{Type: "CERTIFICATE", Bytes: der}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
}