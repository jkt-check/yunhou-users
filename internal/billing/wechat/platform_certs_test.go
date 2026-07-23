package wechat

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPlatformCertManager_PublicKeyFor_AfterRefresh verifies the happy
// path: a serial present in the freshly-fetched certificate set returns
// the matching public key.
func TestPlatformCertManager_PublicKeyFor_AfterRefresh(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	platformPEM := certPEM(t, priv)
	ciphertext := encryptCertForTest(t, platformPEM, "nonce1234567", "transaction_event")

	apiKey := strings.Repeat("k", 32)
	stub := &stubHTTPDoer{resp: &HTTPResponse{
		StatusCode: 200,
		Body: []byte(`{"data":[{"serial_no":"S1","effective_time":"2024-01-01T00:00:00+08:00",` +
			`"expire_time":"2029-01-01T00:00:00+08:00","encrypt_certificate":{` +
			`"algorithm":"AEAD_AES_256_GCM","nonce":"nonce1234567",` +
			`"associated_data":"transaction_event","ciphertext":"` + ciphertext + `"}}]}`),
	}}

	mgr := &PlatformCertManager{
		Signer:   &Signer{MchID: "1900000109", SerialNo: "MERCH_SERIAL", PrivateKey: priv},
		APIv3Key: []byte(apiKey),
		BaseURL:  "https://api.mch.weixin.qq.com",
		HTTPDoer: stub,
	}

	pub, err := mgr.PublicKeyFor(t.Context(), "S1")
	if err != nil {
		t.Fatalf("PublicKeyFor: %v", err)
	}
	if pub == nil {
		t.Fatalf("expected non-nil public key")
	}
	if pub.N.Cmp(priv.PublicKey.N) != 0 {
		t.Errorf("returned key does not match generated key")
	}
}

// TestPlatformCertManager_RefreshFailure_UsesStaleCache verifies the
// graceful-degradation path: when the upstream /v3/certificates call
// fails, a previously-cached but stale (>maxAge) key is still trusted
// because platform certificates have ~5-year validity. Rejecting with
// 400 during a transient upstream outage would lose legitimate
// deliveries.
func TestPlatformCertManager_RefreshFailure_UsesStaleCache(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	apiKey := strings.Repeat("k", 32)
	ciphertext := encryptCertForTest(t, certPEM(t, priv), "nonce1234567", "transaction_event")

	// First call: success — caches "S1" with fetchedAt=now.
	stub := &stubHTTPDoer{resp: &HTTPResponse{
		StatusCode: 200,
		Body: []byte(`{"data":[{"serial_no":"S1","encrypt_certificate":{` +
			`"algorithm":"AEAD_AES_256_GCM","nonce":"nonce1234567",` +
			`"associated_data":"transaction_event","ciphertext":"` + ciphertext + `"}}]}`),
	}}
	mgr := &PlatformCertManager{
		Signer:   &Signer{MchID: "1900000109", PrivateKey: priv},
		APIv3Key: []byte(apiKey),
		BaseURL:  "https://api.mch.weixin.qq.com",
		HTTPDoer: stub,
	}
	if _, err := mgr.PublicKeyFor(t.Context(), "S1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// Now make upstream fail AND age the cache past maxAge.
	stub.status = 500
	mgr.fetchedAt = time.Now().Add(-2 * platformCertMaxAge)
	if _, err := mgr.PublicKeyFor(t.Context(), "S1"); err != nil {
		t.Errorf("stale cache should still serve on refresh failure, got %v", err)
	}
	if stub.calls.Load() == 0 {
		t.Errorf("expected an upstream refresh attempt")
	}
}

// TestPlatformCertManager_ConcurrentMisses_OneRefresh pins that a burst
// of concurrent goroutines hitting an empty cache triggers exactly ONE
// upstream /v3/certificates call, not N. Without the single-flight
// gate (review N1 from the 2026-07-23 independent review), the
// non-atomic time.Since + refresh pair would let every goroutine pass
// the rate-limit check and issue its own refresh — turning a single
// cert-rotation event into an outbound amplifier.
func TestPlatformCertManager_ConcurrentMisses_OneRefresh(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	apiKey := strings.Repeat("k", 32)
	ciphertext := encryptCertForTest(t, certPEM(t, priv), "nonce1234567", "transaction_event")
	stub := &stubHTTPDoer{resp: &HTTPResponse{
		StatusCode: 200,
		Body: []byte(`{"data":[{"serial_no":"S1","encrypt_certificate":{` +
			`"algorithm":"AEAD_AES_256_GCM","nonce":"nonce1234567",` +
			`"associated_data":"transaction_event","ciphertext":"` + ciphertext + `"}}]}`),
	}}
	mgr := &PlatformCertManager{
		Signer:   &Signer{MchID: "1900000109", PrivateKey: priv},
		APIv3Key: []byte(apiKey),
		BaseURL:  "https://api.mch.weixin.qq.com",
		HTTPDoer: stub,
	}

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := mgr.PublicKeyFor(t.Context(), "S1"); err != nil {
				t.Errorf("PublicKeyFor: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := stub.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 refresh across %d concurrent goroutines, got %d", N, got)
	}
}

// TestPlatformCertManager_RateLimitOnSerialMiss pins that a fresh
// serial triggers at most one refresh per minFetchInterval; subsequent
// misses within the window return the prior error without hammering
// /v3/certificates.
func TestPlatformCertManager_RateLimitOnSerialMiss(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubHTTPDoer{resp: &HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}}
	mgr := &PlatformCertManager{
		Signer:   &Signer{MchID: "1900000109", SerialNo: "MERCH", PrivateKey: priv},
		APIv3Key: []byte(strings.Repeat("k", 32)),
		BaseURL:  "https://api.mch.weixin.qq.com",
		HTTPDoer: stub,
	}
	// 5 misses within the window.
	for i := 0; i < 5; i++ {
		_, err := mgr.PublicKeyFor(t.Context(), "MISS")
		if err == nil {
			t.Errorf("expected ErrUnknownPlatformSerial")
		}
	}
	// First miss triggers refresh; remaining 4 hit the cached (empty)
	// result set. A 5x call total proves the rate limit.
	if got := stub.calls.Load(); got > 1 {
		t.Errorf("expected ≤1 upstream refresh, got %d", got)
	}
}

// helper: generate a self-signed PEM certificate for the given public key.
// Takes a signer (full keypair) because x509.CreateCertificate needs
// crypto.Signer; the public key to embed is `signer.Public()`.
func certPEM(t *testing.T, signer *rsa.PrivateKey) []byte {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &signer.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// helper: AES-256-GCM-encrypt the platform PEM exactly the way WeChat
// publishes it (with the test APIv3 key, the provided nonce and AAD),
// base64-encode for the JSON body.
func encryptCertForTest(t *testing.T, plaintext []byte, nonce, aad string) string {
	t.Helper()
	key := []byte(strings.Repeat("k", 32))
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != gcm.NonceSize() {
		t.Fatalf("test nonce must be %d bytes", gcm.NonceSize())
	}
	ct := gcm.Seal(nil, []byte(nonce), plaintext, []byte(aad))
	return base64.StdEncoding.EncodeToString(ct)
}

// stubHTTPDoer is a count-and-resp HTTPDoer for the platform-cert tests.
// `calls` counts the number of outbound requests so rate-limit assertions
// stay concrete; `status` is set after the first success path to flip
// the same Doer into a failure.
type stubHTTPDoer struct {
	resp   *HTTPResponse
	status int
	calls  atomic.Int32
}

func (s *stubHTTPDoer) Do(_ context.Context, req *HTTPRequest) (*HTTPResponse, error) {
	s.calls.Add(1)
	if s.status != 0 {
		return &HTTPResponse{StatusCode: s.status, Body: []byte(`{"code":"FAIL","message":"down"}`)}, nil
	}
	return s.resp, nil
}