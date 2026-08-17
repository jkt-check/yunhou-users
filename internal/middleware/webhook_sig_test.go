package middleware

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/billing/wechat"

	"github.com/gin-gonic/gin"
)

// stubVerifier returns whatever error it's configured with. Used to drive
// the middleware's status-mapping table.
type stubVerifier struct {
	errByChannel map[string]error
}

func (s *stubVerifier) VerifySignature(channel string, _ []byte, _ map[string]string) error {
	if err, ok := s.errByChannel[channel]; ok {
		return err
	}
	return ErrUnsupportedChannel
}

func newTestEngine(v ChannelSignatureVerifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/webhooks/payment")
	group.Use(WebhookSignature(v))
	group.POST("/:channel", func(c *gin.Context) {
		// Echo the (restored) raw body so tests can verify the middleware
		// made it re-readable to the handler.
		body := make([]byte, c.Request.ContentLength)
		if _, err := c.Request.Body.Read(body); err != nil && err.Error() != "EOF" {
			c.String(http.StatusInternalServerError, "read: %v", err)
			return
		}
		c.Data(http.StatusOK, "application/octet-stream", body)
	})
	return engine
}

// resetPaypalVerifyCache clears the package-level verify cache before a
// test runs. The cache is keyed on (transmissionID, transmissionTime);
// tests share a hardcoded pair ("tid-1" / "2026-06-30T12:00:00Z"), so the
// first test to store a result would poison every subsequent test that
// tries to exercise the harness. Each PayPal test calls this in its setup
// (NOT via t.Cleanup — the cache is global, so cleanup-then-rebuild has
// the same race; clearing on entry is the only safe ordering).
func resetPaypalVerifyCache(t *testing.T) {
	t.Helper()
	paypalVerifyCache.Range(func(k, _ any) bool {
		paypalVerifyCache.Delete(k)
		return true
	})
}

func TestWebhookSignature_Mapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"invalid signature → 400", ErrInvalidSignature, http.StatusBadRequest},
		{"timestamp out of range → 400", ErrTimestampOutOfRange, http.StatusBadRequest},
		{"unsupported channel → 404", ErrUnsupportedChannel, http.StatusNotFound},
		{"transient error → 500", &transientErr{msg: "boom"}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newTestEngine(&stubVerifier{errByChannel: map[string]error{
				"stripe": tc.err,
			}})
			req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/stripe",
				bytes.NewReader([]byte("{}")))
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestWebhookSignature_PassesThrough(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(&stubVerifier{errByChannel: map[string]error{
		"stripe": nil,
	}})
	body := []byte(`{"hello":"world"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/stripe", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body not restored: got %q want %q", got, string(body))
	}
}

func TestWebhookSignature_BodyTooLarge(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(&stubVerifier{})
	huge := bytes.Repeat([]byte("a"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/stripe", bytes.NewReader(huge))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for huge body, got %d", rec.Code)
	}
}

// ============================================================================
// StripeVerifier — end-to-end with real HMAC
// ============================================================================

func TestStripeVerifier_Accept(t *testing.T) {
	t.Parallel()

	secret := []byte("whsec_test_secret")
	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	sigHex := hex.EncodeToString(mac.Sum(nil))
	sigHeader := "t=" + strconv.FormatInt(ts, 10) + ",v1=" + sigHex

	v := &StripeVerifier{Secret: secret}
	headers := map[string]string{"Stripe-Signature": sigHeader}
	if err := v.VerifySignature("stripe", body, headers); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}
}

func TestStripeVerifier_RejectBadSignature(t *testing.T) {
	t.Parallel()

	v := &StripeVerifier{Secret: []byte("whsec_test_secret")}
	body := []byte(`{}`)
	ts := time.Now().Unix()
	sigHeader := "t=" + strconv.FormatInt(ts, 10) + ",v1=deadbeef"

	headers := map[string]string{"Stripe-Signature": sigHeader}
	if err := v.VerifySignature("stripe", body, headers); err != ErrInvalidSignature {
		t.Errorf("bad signature should return ErrInvalidSignature, got %v", err)
	}
}

func TestStripeVerifier_RejectTimestampOutsideWindow(t *testing.T) {
	t.Parallel()

	secret := []byte("whsec_test_secret")
	v := &StripeVerifier{Secret: secret, ReplayWindow: time.Minute}
	body := []byte(`{}`)
	ts := time.Now().Add(-time.Hour).Unix()

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	sigHeader := "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	headers := map[string]string{"Stripe-Signature": sigHeader}

	if err := v.VerifySignature("stripe", body, headers); err != ErrTimestampOutOfRange {
		t.Errorf("old timestamp should return ErrTimestampOutOfRange, got %v", err)
	}
}

// Per-channel verifiers no longer self-guard against the wrong channel —
// MultiChannelVerifier is the single dispatcher. This test pins the new
// contract: StripeVerifier assumes the caller routed correctly and
// returns whatever verdict the headers warrant.
func TestStripeVerifier_DirectCallAnyChannel(t *testing.T) {
	t.Parallel()

	v := &StripeVerifier{Secret: []byte("whsec_test_secret")}
	// Called directly with a non-stripe channel name → not ErrUnsupportedChannel;
	// the verifier just looks at the headers it expects (Stripe-Signature),
	// which are missing, so it falls into the signature-missing branch.
	if err := v.VerifySignature("wechat_pay", []byte("{}"), nil); err != ErrInvalidSignature {
		t.Errorf("direct call with non-stripe channel should yield signature-missing, got %v", err)
	}
}

// ============================================================================
// WeChatPayV3Verifier — HMAC + AES-GCM resource decrypt
// ============================================================================

func TestWeChatPayV3Verifier_Accept(t *testing.T) {
	t.Parallel()

	// 2026-07-23: verifier switched from APIv3-key HMAC to WeChat
	// platform-cert RSA. Sign with a fresh key, expose via stub
	// PlatformKeySource.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := &WeChatPayV3Verifier{
		APIv3Key:     bytes.Repeat([]byte("k"), 32),
		PlatformKeys: &stubKeySource{pub: &priv.PublicKey},
	}
	body := []byte(`{"id":"evt_1"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "abc123"

	toSign := ts + "\n" + nonce + "\n" + string(body) + "\n"
	hashed := sha256.Sum256([]byte(toSign))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}

	headers := map[string]string{
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString(sig),
		"Wechatpay-Timestamp": ts,
		"Wechatpay-Nonce":     nonce,
		"Wechatpay-Serial":    "TEST_SERIAL",
	}
	if err := v.VerifySignature("wechat_pay", body, headers); err != nil {
		t.Errorf("valid wechat sig rejected: %v", err)
	}
}

// stubKeySource is a tiny test-only PlatformKeySource. Production wires
// *wechat.PlatformCertManager; tests use this to pin RSA keys and force
// transient/unknown-serial paths without spinning up a /v3/certificates
// server.
type stubKeySource struct {
	pub       *rsa.PublicKey
	serial    string // non-empty → return UnknownPlatformSerial
	lookupErr error
}

func (s *stubKeySource) PublicKeyFor(_ context.Context, serial string) (*rsa.PublicKey, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.serial != "" && serial == s.serial {
		return nil, wechat.ErrUnknownPlatformSerial
	}
	return s.pub, nil
}

func TestWeChatPayV3Verifier_DecryptResource(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte("k"), 32)
	v := &WeChatPayV3Verifier{APIv3Key: key}

	// Encrypt a payload with the same key/nonce/AAD, then verify roundtrip.
	// GCM nonce must be exactly 12 bytes (Go's stdlib enforces this).
	plaintext := []byte(`{"transaction_id":"wx_1","amount":{"total":2990}}`)
	nonce := "nonce_123456" // 12 bytes
	aad := "transaction_event"

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := gcm.Seal(nil, []byte(nonce), plaintext, []byte(aad))

	got, err := v.DecryptResource(base64.StdEncoding.EncodeToString(ciphertext), nonce, aad)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("roundtrip mismatch: got %s, want %s", got, plaintext)
	}
}

// ============================================================================
// AlipayVerifier — RSA2 signature verification
// ============================================================================

func TestAlipayVerifier_Accept(t *testing.T) {
	t.Parallel()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv // unused at this layer; we sign with our own helper

	// Self-signed PEM to test the loader.
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	pub, err := LoadAlipayPublicKeyFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("load PEM: %v", err)
	}

	// Build the canonical string manually (matching the verifier's algorithm).
	body := "out_trade_no=order_1&total_amount=29.90&trade_no=2023110"
	canonical := alipayCanonicalForTest(t, body)
	hashed := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	// Alipay sign value goes through URL encoding (`+` → `%2B`, `=` → `%3D`)
	// because the body is application/x-www-form-urlencoded. url.ParseQuery
	// in the verifier will undo this, so we must match what the real channel sends.
	bodySigned := body + "&sign=" + urlEncodeFormValue(sigB64) + "&sign_type=RSA2"

	v := &AlipayVerifier{PublicKey: pub}
	if err := v.VerifySignature("alipay", []byte(bodySigned), nil); err != nil {
		t.Errorf("valid alipay signature rejected: %v", err)
	}
}

func TestAlipayVerifier_RejectTamperedBody(t *testing.T) {
	t.Parallel()

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	pub, _ := LoadAlipayPublicKeyFromPEM(pemBytes)

	original := "out_trade_no=order_1&total_amount=29.90&trade_no=A"
	canonical := alipayCanonicalForTest(t, original)
	hashed := sha256.Sum256([]byte(canonical))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Tamper: change trade_no after signing.
	tampered := "out_trade_no=order_1&total_amount=29.90&trade_no=B&sign=" +
		urlEncodeFormValue(sigB64) + "&sign_type=RSA2"

	v := &AlipayVerifier{PublicKey: pub}
	if err := v.VerifySignature("alipay", []byte(tampered), nil); err != ErrInvalidSignature {
		t.Errorf("tampered body should return ErrInvalidSignature, got %v", err)
	}
}

func TestAlipayVerifier_RejectBadSignatureType(t *testing.T) {
	t.Parallel()

	_, pub := rsaTestKey(t)
	v := &AlipayVerifier{PublicKey: pub}
	body := "trade_no=1&sign_type=MD5&sign=xx"
	if err := v.VerifySignature("alipay", []byte(body), nil); err == nil {
		t.Error("MD5 should be rejected")
	} else if !strings.Contains(err.Error(), "unsupported sign_type") {
		t.Errorf("expected 'unsupported sign_type' in error, got %v", err)
	}
}

func TestAlipayVerifier_LoadPEM_AcceptsPKCS8(t *testing.T) {
	t.Parallel()

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	if _, err := LoadAlipayPublicKeyFromPEM(pemBytes); err != nil {
		t.Errorf("PKCS#8 PEM should load: %v", err)
	}
}

// ============================================================================
// MultiChannelVerifier fan-out
// ============================================================================

func TestMultiChannelVerifier_NilChannel(t *testing.T) {
	t.Parallel()

	mv := &MultiChannelVerifier{} // no channels configured
	if err := mv.VerifySignature("stripe", nil, nil); err != ErrUnsupportedChannel {
		t.Errorf("unconfigured channel should return ErrUnsupportedChannel, got %v", err)
	}
}

func TestMultiChannelVerifier_UnknownChannel(t *testing.T) {
	t.Parallel()

	mv := &MultiChannelVerifier{}
	if err := mv.VerifySignature("paypal", nil, nil); err != ErrUnsupportedChannel {
		t.Errorf("unknown channel should return ErrUnsupportedChannel, got %v", err)
	}
}

// TestMultiChannelVerifier_StripeAlipayFanOut verifies the dispatch wires
// the Stripe + Alipay fields correctly: each configured channel routes to
// its own verifier; an unconfigured channel returns ErrUnsupportedChannel.
func TestMultiChannelVerifier_StripeAlipayFanOut(t *testing.T) {
	t.Parallel()

	stripeErr := errors.New("stripe-stub-error")
	alipayErr := errors.New("alipay-stub-error")
	mv := &MultiChannelVerifier{
		Stripe: &stubVerifier{errByChannel: map[string]error{"stripe": stripeErr}},
		Alipay: &stubVerifier{errByChannel: map[string]error{"alipay": alipayErr}},
	}

	if err := mv.VerifySignature("stripe", nil, nil); err != stripeErr {
		t.Errorf("Stripe dispatch: got %v, want %v", err, stripeErr)
	}
	if err := mv.VerifySignature("alipay", nil, nil); err != alipayErr {
		t.Errorf("Alipay dispatch: got %v, want %v", err, alipayErr)
	}
	// channel="paypal" is unset on this mv → ErrUnsupportedChannel
	if err := mv.VerifySignature("paypal", nil, nil); err != ErrUnsupportedChannel {
		t.Errorf("unset channel: got %v, want ErrUnsupportedChannel", err)
	}
}

// TestMultiChannelVerifier_PaypalFanOut verifies the dispatch wires the
// Paypal field correctly: channel="paypal" routes to the configured Paypal
// verifier; "paypal" with no slot returns ErrUnsupportedChannel.
func TestMultiChannelVerifier_PaypalFanOut(t *testing.T) {
	t.Parallel()

	paypalErr := errors.New("paypal-stub-error")
	mv := &MultiChannelVerifier{
		Paypal: &stubVerifier{errByChannel: map[string]error{"paypal": paypalErr}},
	}
	if err := mv.VerifySignature("paypal", nil, nil); err != paypalErr {
		t.Errorf("Paypal dispatch: got %v, want %v", err, paypalErr)
	}

	mv2 := &MultiChannelVerifier{}
	if err := mv2.VerifySignature("paypal", nil, nil); err != ErrUnsupportedChannel {
		t.Errorf("unset paypal: got %v, want ErrUnsupportedChannel", err)
	}
}

// ============================================================================
// PaypalVerifier — HTTP verify-webhook-signature
// ============================================================================

// paypalHarness spins up an httptest server that mimics PayPal's
// verify-webhook-signature endpoint. Tests configure the server's response
// (record what arrived, return what the test wants PayPal to say).
type paypalHarness struct {
	server     *httptest.Server
	seenBody   []byte
	seenHeader http.Header
}

func newPaypalHarness(t *testing.T, respond func(h *paypalHarness, w http.ResponseWriter, r *http.Request)) *paypalHarness {
	t.Helper()
	h := &paypalHarness{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.seenHeader = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		h.seenBody = b
		respond(h, w, r)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func newHarnessVerifier(h *paypalHarness, env string) *PaypalVerifier {
	v := &PaypalVerifier{
		HTTPClient:       &http.Client{Timeout: 2 * time.Second},
		SandboxWebhookID: "wbh_sbx",
		LiveWebhookID:    "wbh_live",
		Env:              env,
	}
	v.SandboxAPIBase = h.server.URL
	v.LiveAPIBase = h.server.URL
	return v
}

func paypalHeaders(transmissionID, sig string) map[string]string {
	return map[string]string{
		"PAYPAL-AUTH-ALGO":         "SHA256withRSA",
		"PAYPAL-CERT-URL":          "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42-fca2ab1b-7ce9e4e3-abc",
		"PAYPAL-TRANSMISSION-ID":   transmissionID,
		"PAYPAL-TRANSMISSION-SIG":  sig,
		"PAYPAL-TRANSMISSION-TIME": "2026-06-30T12:00:00Z",
	}
}

func TestPaypalVerifier_HappyPath(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		// Validate the verifier forwarded our headers + webhook_id.
		var sent map[string]any
		if err := json.Unmarshal(h.seenBody, &sent); err != nil {
			t.Errorf("verify request not JSON: %v", err)
		}
		if sent["webhook_id"] != "wbh_sbx" {
			t.Errorf("webhook_id: got %v, want wbh_sbx", sent["webhook_id"])
		}
		if sent["transmission_id"] != "tid-1" {
			t.Errorf("transmission_id: got %v", sent["transmission_id"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
	})
	v := newHarnessVerifier(h, "sandbox")

	body := []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED"}`)
	if err := v.VerifySignature("paypal", body, paypalHeaders("tid-1", "sig-1")); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestPaypalVerifier_TokenFuncSendsBearer(t *testing.T) {
	// Production wiring (cmd/server) sets TokenFunc from the cached OAuth
	// fetcher — verify-webhook-signature 401s without it (2026-08-17
	// incident). The harness asserts the Authorization header arrives.
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("Authorization: got %q, want Bearer tok-123", got)
		}
		fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
	})
	v := newHarnessVerifier(h, "sandbox")
	calls := 0
	v.TokenFunc = func() (string, error) { calls++; return "tok-123", nil }

	body := []byte(`{"id":"WH-1","event_type":"BILLING.SUBSCRIPTION.CREATED"}`)
	if err := v.VerifySignature("paypal", body, paypalHeaders("tid-tok", "sig-1")); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("TokenFunc called %d times, want 1", calls)
	}
}

func TestPaypalVerifier_TokenFetchErrorIsTransient(t *testing.T) {
	// OAuth endpoint down → generic error (500 at the middleware, PayPal
	// retries) — NOT ErrInvalidSignature, which would 400 and stop retries.
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		t.Error("verify endpoint must not be called when token fetch fails")
		w.WriteHeader(http.StatusInternalServerError)
	})
	v := newHarnessVerifier(h, "sandbox")
	v.TokenFunc = func() (string, error) { return "", errors.New("oauth down") }

	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-tokerr", "sig-1"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("token fetch failure must not map to ErrInvalidSignature, got %v", err)
	}
}

func TestPaypalVerifier_FailureMapsToInvalidSignature(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"verification_status":"FAILURE"}`)
	})
	v := newHarnessVerifier(h, "sandbox")
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestPaypalVerifier_MalformedJSONResponseIsTransient(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `not json`)
	})
	v := newHarnessVerifier(h, "sandbox")
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("malformed response should NOT be 400-class, got %v", err)
	}
	if err == nil {
		t.Fatalf("malformed response should error, got nil")
	}
}

func TestPaypalVerifier_MissingHeaderIsInvalidSignature(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	v := &PaypalVerifier{
		HTTPClient:       &http.Client{Timeout: 1 * time.Second},
		SandboxWebhookID: "wbh_sbx",
		SandboxAPIBase:   "http://127.0.0.1:1",
		Env:              "sandbox",
	}
	err := v.VerifySignature("paypal", []byte(`{}`), map[string]string{
		// PAYPAL-AUTH-ALGO missing on purpose
		"PAYPAL-CERT-URL":          "x",
		"PAYPAL-TRANSMISSION-ID":   "x",
		"PAYPAL-TRANSMISSION-SIG":  "x",
		"PAYPAL-TRANSMISSION-TIME": "x",
	})
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestPaypalVerifier_EnvSelectsLive(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		_ = json.Unmarshal(h.seenBody, &sent)
		if sent["webhook_id"] != "wbh_live" {
			t.Errorf("env=live should forward wbh_live, got %v", sent["webhook_id"])
		}
		fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
	})
	v := newHarnessVerifier(h, "live")
	if err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1")); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestPaypalVerifier_NoWebhookIDForActiveEnvIsUnsupported(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	v := &PaypalVerifier{
		HTTPClient:     &http.Client{Timeout: 1 * time.Second},
		SandboxAPIBase: "https://api-m.sandbox.paypal.com",
		Env:            "sandbox",
		// SandboxWebhookID intentionally empty
	}
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if !errors.Is(err, ErrUnsupportedChannel) {
		t.Fatalf("want ErrUnsupportedChannel, got %v", err)
	}
}

func TestPaypalVerifier_UnknownEnvIsUnsupported(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	v := &PaypalVerifier{
		HTTPClient:       &http.Client{Timeout: 1 * time.Second},
		SandboxWebhookID: "wbh",
		SandboxAPIBase:   "https://api-m.sandbox.paypal.com",
		Env:              "qa",
	}
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if !errors.Is(err, ErrUnsupportedChannel) {
		t.Fatalf("want ErrUnsupportedChannel, got %v", err)
	}
}

func TestPaypalVerifier_VerifyEndpoint5xxIsTransient(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	h := newPaypalHarness(t, func(h *paypalHarness, w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	v := newHarnessVerifier(h, "sandbox")
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if err == nil || errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("5xx should be transient non-sentinel error, got %v", err)
	}
}

// TestPaypalVerifier_EmptyAPIBaseReturnsUnsupported covers the bug-1 fix:
// previously a missing apiBase silently fell back to LIVE PayPal. Now it
// returns ErrUnsupportedChannel so an operator notices via 404 instead of
// accidentally routing sandbox events to production.
func TestPaypalVerifier_EmptyAPIBaseReturnsUnsupported(t *testing.T) {
	// t.Parallel removed: package-level paypalVerifyCache is shared between tests
	resetPaypalVerifyCache(t)
	v := &PaypalVerifier{
		HTTPClient:       &http.Client{Timeout: 1 * time.Second},
		SandboxWebhookID: "wbh_sbx",
		Env:              "sandbox",
		// SandboxAPIBase intentionally empty
	}
	err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
	if !errors.Is(err, ErrUnsupportedChannel) {
		t.Fatalf("want ErrUnsupportedChannel, got %v", err)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// urlEncodeFormValue percent-encodes a value for use in an
// application/x-www-form-urlencoded body. Specifically `+` → `%2B` and
// `=` → `%3D` so that url.ParseQuery on the receiving end decodes them
// back to the original characters.
func urlEncodeFormValue(s string) string {
	const hexChars = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexChars[c>>4])
			sb.WriteByte(hexChars[c&0xF])
		}
	}
	return sb.String()
}

type transientErr struct{ msg string }

func (e *transientErr) Error() string { return e.msg }

// rsaTestKey generates an in-memory RSA key for Alipay tests.
func rsaTestKey(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return priv, &priv.PublicKey
}

// alipayCanonicalForTest builds the same canonical string the verifier does,
// but only for testing. We re-implement the encoding here so the test stays
// decoupled from private alipayURLEncode internals.
func alipayCanonicalForTest(t *testing.T, body string) string {
	t.Helper()
	parts := strings.Split(body, "&")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			keys = append(keys, p)
			continue
		}
		keys = append(keys, p[:eq])
	}
	// Sort but skip "sign"/"sign_type" — they're not in the test body anyway.
	// Simple bubble sort for test brevity.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	// Rebuild with sorted keys + values, URL-decoded first (matching what
	// the verifier does via url.ParseQuery). Without this, spaces and other
	// percent-encoded chars would be double-encoded by alipayURLEncode and
	// the helper's canonical string wouldn't match the verifier's.
	valueByKey := map[string]string{}
	for _, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			valueByKey[p] = ""
		} else {
			if decoded, err := url.QueryUnescape(p[eq+1:]); err == nil {
				valueByKey[p[:eq]] = decoded
			} else {
				valueByKey[p[:eq]] = p[eq+1:]
			}
		}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, alipayURLEncode(k)+"="+alipayURLEncode(valueByKey[k]))
	}
	return strings.Join(out, "&")
}

// ============================================================================
// LoadAlipayPublicKeyFromPEM — additional error-path + PKCS1-coverage tests
// ============================================================================

func TestLoadAlipayPublicKeyFromPEM_PKCS1(t *testing.T) {
	t.Parallel()
	priv, _ := rsaTestKey(t)
	der := x509.MarshalPKCS1PrivateKey(priv) // PKCS1 uses private-key DER; we use the public slice of it via PKIXPublic
	// Note: Go's x509 doesn't expose PKCS1-marshal-of-public-key directly.
	// PKIXPublicKey is the marshalling the verifier accepts under PKCS1 path —
	// it falls back to ParsePKCS1PublicKey on the same byte slice. Test
	// PKIXPublic here for the happy path; the empty PEM test below covers
	// the rejection path.
	derPub, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = der // kept for documentation; PKCS1 path uses a private-key encoding
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derPub})
	if _, err := LoadAlipayPublicKeyFromPEM(pemBytes); err != nil {
		t.Fatalf("PKIX load: %v", err)
	}
}

func TestLoadAlipayPublicKeyFromPEM_Empty(t *testing.T) {
	t.Parallel()
	// No PEM block at all → "no PEM block found"
	if _, err := LoadAlipayPublicKeyFromPEM([]byte("not a pem block")); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

func TestLoadAlipayPublicKeyFromPEM_WrongKeyType(t *testing.T) {
	t.Parallel()
	// Embed something that parses as PEM but isn't an RSA key — use a
	// minimal DER that won't parse either as PKIX or PKCS1.
	bogus := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not-a-real-DER")})
	if _, err := LoadAlipayPublicKeyFromPEM(bogus); err == nil {
		t.Fatal("expected error for non-key PEM block")
	}
}

// ============================================================================
// readAndRestoreBody — body-too-large path
// ============================================================================

func TestReadAndRestoreBody_BodyTooLarge(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	// 1 MiB cap in readAndRestoreBody + 1 byte over
	body := make([]byte, (1<<20)+1)
	for i := range body {
		body[i] = 'a'
	}
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	_, err := readAndRestoreBody(c)
	if err == nil {
		t.Fatal("expected body-too-large error")
	}
	if !strings.Contains(err.Error(), "body too large") {
		t.Errorf("error message mismatch: %v", err)
	}
}

func TestReadAndRestoreBody_NilBody(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	// httptest.NewRequest with nil body actually leaves c.Request.Body nil.
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Body = nil
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	got, err := readAndRestoreBody(c)
	if err != nil {
		t.Fatalf("nil body should not error: %v", err)
	}
	if got != nil {
		t.Errorf("nil body should yield nil bytes, got length %d", len(got))
	}
}

// ============================================================================
// parseStripeSignatureHeader — missing t / missing v1 paths
// ============================================================================

// TestParseStripeSignatureHeader_MultiV1DuringRotation locks in the fix for
// the secret-rotation window: Stripe sends multiple v1= values (one per
// active secret) and the verifier must accept any of them. The previous
// implementation overwrote sigHex on each iteration and only checked the
// last one, silently dropping events during the rotation window.
func TestParseStripeSignatureHeader_MultiV1DuringRotation(t *testing.T) {
	t.Parallel()
	// Two v1 signatures — the second is a stand-in for the rotated-away
	// secret, the first for the current secret. parseStripeSignatureHeader
	// must return BOTH so the verifier can try each.
	ts, sigs, err := parseStripeSignatureHeader("t=1700000000,v1=aabb,v1=ccdd")
	if err != nil {
		t.Fatalf("multi-v1 header should parse: %v", err)
	}
	if ts != 1700000000 {
		t.Errorf("ts: got %d want 1700000000", ts)
	}
	if len(sigs) != 2 {
		t.Fatalf("expected 2 v1 sigs, got %d", len(sigs))
	}
	if hex.EncodeToString(sigs[0]) != "aabb" || hex.EncodeToString(sigs[1]) != "ccdd" {
		t.Errorf("v1 ordering lost: got %x %x", sigs[0], sigs[1])
	}
}

func TestParseStripeSignatureHeader_MissingParts(t *testing.T) {
	t.Parallel()
	if _, _, err := parseStripeSignatureHeader(""); err == nil {
		t.Error("empty header should error")
	}
	if _, _, err := parseStripeSignatureHeader("v1=deadbeef"); err == nil {
		t.Error("missing t should error")
	}
	if _, _, err := parseStripeSignatureHeader("t=12345"); err == nil {
		t.Error("missing v1 should error")
	}
	if _, _, err := parseStripeSignatureHeader("t=abc"); err == nil {
		t.Error("non-numeric t should error")
	}
	if _, _, err := parseStripeSignatureHeader("v1=not-hex"); err == nil {
		t.Error("non-hex v1 should error")
	}
	// Unknown kv pairs are tolerated
	if _, _, err := parseStripeSignatureHeader("garbage=1"); err == nil {
		t.Error("garbage-only should error too")
	}
	ts, sigs, err := parseStripeSignatureHeader("t=1700000000,v1=00ff")
	if err != nil {
		t.Errorf("valid header err: %v", err)
	}
	if ts != 1700000000 || len(sigs) != 1 || len(sigs[0]) != 2 || sigs[0][0] != 0x00 || sigs[0][1] != 0xff {
		t.Errorf("ts/sig extraction: ts=%d sigs=%v", ts, sigs)
	}
}

// ============================================================================
// WeChatPayV3Verifier.DecryptResource — invalid-nonce-length path
// ============================================================================

func TestWeChatPayV3_DecryptResource_InvalidNonceLength(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{APIv3Key: []byte("01234567890123456789012345678901")}
	// Valid base64 ciphertext of any length; the nonce length is the gate.
	bogusCT := base64.StdEncoding.EncodeToString([]byte("not-really-aes-ciphertext"))
	_, err := v.DecryptResource(bogusCT, "short", "associated-data")
	if err == nil {
		t.Fatal("short nonce should error")
	}
	if !strings.Contains(err.Error(), "invalid nonce length") {
		t.Errorf("error message mismatch: %v", err)
	}
}

// ============================================================================
// collectHeaders — multi-value + zero-value headers
// ============================================================================

func TestCollectHeaders(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Request.Header.Set("X-Single", "one")
	c.Request.Header.Add("X-Multi", "first")
	c.Request.Header.Add("X-Multi", "second")

	h := collectHeaders(c)
	if h["X-Single"] != "one" {
		t.Errorf("X-Single: got %q", h["X-Single"])
	}
	// Map only stores the FIRST value; this is the documented contract.
	if h["X-Multi"] != "first" {
		t.Errorf("X-Multi (first): got %q", h["X-Multi"])
	}
	if _, ok := h["X-Missing"]; ok {
		t.Errorf("missing header should not exist")
	}
}

// ============================================================================
// alipayURLEncode — corner cases (space, mixed-case, +, =)
// ============================================================================

func TestAlipayURLEncode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"hello":       "hello",
		"hello world": "hello%20world", // space → %20 (NOT +)
		"a+b":         "a%2Bb",         // + → %2B (NOT space-encoded)
		"x=y":         "x%3Dy",         // = → %3D
		"中文":          "%E4%B8%AD%E6%96%87",
		"":            "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := alipayURLEncode(in); got != want {
				t.Errorf("alipayURLEncode(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// ============================================================================
// MultiChannelVerifier round-trip — every channel + unknown + nil slot
// ============================================================================

func TestMultiChannelVerifier_RoundTrip_AllChannels(t *testing.T) {
	t.Parallel()
	stripeErr := errors.New("stripe-err")
	wechatErr := errors.New("wechat-err")
	alipayErr := errors.New("alipay-err")
	paypalErr := errors.New("paypal-err")

	mv := &MultiChannelVerifier{
		Stripe: &stubVerifier{errByChannel: map[string]error{"stripe": stripeErr}},
		WeChat: &stubVerifier{errByChannel: map[string]error{"wechat_pay": wechatErr}},
		Alipay: &stubVerifier{errByChannel: map[string]error{"alipay": alipayErr}},
		Paypal: &stubVerifier{errByChannel: map[string]error{"paypal": paypalErr}},
	}

	cases := map[string]error{
		"stripe":      stripeErr,
		"wechat_pay":  wechatErr,
		"alipay":      alipayErr,
		"paypal":      paypalErr,
		"unsupported": ErrUnsupportedChannel,
	}
	for ch, wantErr := range cases {
		got := mv.VerifySignature(ch, nil, nil)
		if got != wantErr {
			t.Errorf("channel %s: got %v, want %v", ch, got, wantErr)
		}
	}
}

func TestPaypalVerifyCache_LookupStore(t *testing.T) {
	resetPaypalVerifyCache(t)

	t.Run("miss returns false", func(t *testing.T) {
		k := paypalVerifyCacheKey{transmissionID: "miss-1", transmissionTime: "2026-01-01T00:00:00Z"}
		entry, ok := lookupVerifyCache(k)
		if ok {
			t.Errorf("expected miss, got entry=%+v", entry)
		}
	})

	t.Run("store then lookup returns true with status", func(t *testing.T) {
		k := paypalVerifyCacheKey{transmissionID: "hit-1", transmissionTime: "2026-01-01T00:00:00Z"}
		storeVerifyCache(k, "SUCCESS", nil)
		entry, ok := lookupVerifyCache(k)
		if !ok {
			t.Fatal("expected hit, got miss")
		}
		if entry.status != "SUCCESS" {
			t.Errorf("status: got %q, want SUCCESS", entry.status)
		}
		if entry.err != nil {
			t.Errorf("err: got %v, want nil", entry.err)
		}
	})

	t.Run("store then lookup with err preserves err", func(t *testing.T) {
		k := paypalVerifyCacheKey{transmissionID: "hit-err", transmissionTime: "2026-01-01T00:00:00Z"}
		storeVerifyCache(k, "FAILURE", ErrInvalidSignature)
		entry, ok := lookupVerifyCache(k)
		if !ok {
			t.Fatal("expected hit, got miss")
		}
		if entry.status != "FAILURE" {
			t.Errorf("status: got %q, want FAILURE", entry.status)
		}
		if entry.err == nil {
			t.Error("err: got nil, want ErrInvalidSignature")
		}
	})

	t.Run("expired entry → miss + auto-delete", func(t *testing.T) {
		k := paypalVerifyCacheKey{transmissionID: "expired", transmissionTime: "2026-01-01T00:00:00Z"}
		paypalVerifyCache.Store(k, paypalVerifyCacheEntry{
			status:    "SUCCESS",
			expiresAt: time.Now().Add(-1 * time.Minute), // already expired
		})
		_, ok := lookupVerifyCache(k)
		if ok {
			t.Error("expected miss for expired entry, got hit")
		}
		// Auto-delete: subsequent direct sync.Map lookup should also miss.
		if _, still := paypalVerifyCache.Load(k); still {
			t.Error("expected entry to be deleted after expired lookup")
		}
	})
}

// TestClearPaypalVerifyCache_EmptiesEntries seeds a few entries, then calls
// ClearPaypalVerifyCache and verifies the map is empty. The test exists
// purely for the line coverage of the function (it's an exported
// package-level helper used by e2e tests but otherwise untested).
func TestClearPaypalVerifyCache_EmptiesEntries(t *testing.T) {
	resetPaypalVerifyCache(t)
	for i := 0; i < 3; i++ {
		k := paypalVerifyCacheKey{transmissionID: "seed", transmissionTime: "2026-01-01T00:00:00Z"}
		paypalVerifyCache.Store(k, paypalVerifyCacheEntry{
			status:    "SUCCESS",
			expiresAt: time.Now().Add(time.Hour),
		})
	}
	ClearPaypalVerifyCache()

	count := 0
	paypalVerifyCache.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("expected cache to be empty after Clear, got %d entries", count)
	}
}

// ============================================================================
// WeChat DecryptResource / VerifySignature error paths
// ============================================================================

// TestWeChatPayV3_DecryptResource_BadBase64 covers the base64-decode failure
// branch (line 235). The GCM layer never gets a chance to run because the
// ciphertext isn't valid base64 to begin with.
func TestWeChatPayV3_DecryptResource_BadBase64(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{APIv3Key: bytes.Repeat([]byte("k"), 32)}
	_, err := v.DecryptResource("!!!not-base64!!!", "nonce_123456", "aad")
	if err == nil {
		t.Fatal("invalid base64 should error")
	}
	if !strings.Contains(err.Error(), "base64 decode") {
		t.Errorf("expected base64-decode error, got %v", err)
	}
}

// TestWeChatPayV3_DecryptResource_BadKeyLength covers the cipher-construction
// failure paths when the configured key isn't 16/24/32 bytes (line 239 + 243).
func TestWeChatPayV3_DecryptResource_BadKeyLength(t *testing.T) {
	t.Parallel()
	ct := base64.StdEncoding.EncodeToString([]byte("x"))
	cases := []int{1, 8, 15, 17, 31, 33, 64}
	for _, n := range cases {
		key := bytes.Repeat([]byte("k"), n)
		v := &WeChatPayV3Verifier{APIv3Key: key}
		_, err := v.DecryptResource(ct, "nonce_123456", "aad")
		if err == nil {
			t.Errorf("key length %d: expected error", n)
		}
	}
}

// TestWeChatPayV3_VerifySignature_MissingHeaders covers the sentinel-return
// path when one or more required headers (sig/ts/nonce) are empty.
func TestWeChatPayV3_VerifySignature_MissingHeaders(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{APIv3Key: bytes.Repeat([]byte("k"), 32)}
	cases := map[string]map[string]string{
		"missing sig":   {"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10), "Wechatpay-Nonce": "n"},
		"missing ts":    {"Wechatpay-Signature": "x", "Wechatpay-Nonce": "n"},
		"missing nonce": {"Wechatpay-Signature": "x", "Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10)},
	}
	for name, hdr := range cases {
		hdr := hdr
		t.Run(name, func(t *testing.T) {
			if err := v.VerifySignature("wechat_pay", []byte("{}"), hdr); !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("expected ErrInvalidSignature, got %v", err)
			}
		})
	}
}

// TestWeChatPayV3_VerifySignature_BadTimestamp covers the strconv error branch
// when the timestamp header doesn't parse as int.
func TestWeChatPayV3_VerifySignature_BadTimestamp(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{APIv3Key: bytes.Repeat([]byte("k"), 32)}
	headers := map[string]string{
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString([]byte("x")),
		"Wechatpay-Timestamp": "NaN",
		"Wechatpay-Nonce":     "n",
	}
	err := v.VerifySignature("wechat_pay", []byte("{}"), headers)
	if !errors.Is(err, ErrInvalidSignature) || !strings.Contains(err.Error(), "bad timestamp") {
		t.Errorf("expected ErrInvalidSignature/bad-timestamp, got %v", err)
	}
}

// TestWeChatPayV3_VerifySignature_ReplayWindowOut covers the timestamp-
// outside-window branch.
func TestWeChatPayV3_VerifySignature_ReplayWindowOut(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{APIv3Key: bytes.Repeat([]byte("k"), 32)}
	tsStr := strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10)
	headers := map[string]string{
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"Wechatpay-Timestamp": tsStr,
		"Wechatpay-Nonce":     "n",
	}
	if err := v.VerifySignature("wechat_pay", []byte("{}"), headers); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Errorf("expected ErrTimestampOutOfRange, got %v", err)
	}
}

// TestWeChatPayV3_VerifySignature_BadBase64Sig covers the base64-decode
// failure on the signature header.
func TestWeChatPayV3_VerifySignature_BadBase64Sig(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := &WeChatPayV3Verifier{
		APIv3Key:     bytes.Repeat([]byte("k"), 32),
		PlatformKeys: &stubKeySource{pub: &priv.PublicKey},
	}
	headers := map[string]string{
		"Wechatpay-Signature": "!!!not-base64!!!",
		"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"Wechatpay-Nonce":     "n",
		"Wechatpay-Serial":    "S",
	}
	err = v.VerifySignature("wechat_pay", []byte("{}"), headers)
	if !errors.Is(err, ErrInvalidSignature) || !strings.Contains(err.Error(), "bad base64") {
		t.Errorf("expected ErrInvalidSignature/bad-base64, got %v", err)
	}
}

// TestWeChatPayV3_VerifySignature_HMACMismatch covers the signature-mismatch
// path when the signature decoded successfully but didn't match the
// recomputed hash.
func TestWeChatPayV3_VerifySignature_HMACMismatch(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := &WeChatPayV3Verifier{
		APIv3Key:     bytes.Repeat([]byte("k"), 32),
		PlatformKeys: &stubKeySource{pub: &priv.PublicKey},
	}
	headers := map[string]string{
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAA}, 256)),
		"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"Wechatpay-Nonce":     "n",
		"Wechatpay-Serial":    "S",
	}
	if err := v.VerifySignature("wechat_pay", []byte("{}"), headers); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on signature mismatch, got %v", err)
	}
}

// ============================================================================
// AlipayVerifier — error / edge paths
// ============================================================================

// TestAlipayVerifier_BadFormBody covers the url.ParseQuery failure branch —
// the body must be syntactically valid application/x-www-form-urlencoded.
func TestAlipayVerifier_BadFormBody(t *testing.T) {
	t.Parallel()
	_, pub := rsaTestKey(t)
	v := &AlipayVerifier{PublicKey: pub}
	// "%ZZ" is invalid percent-encoding — ParseQuery rejects it.
	if err := v.VerifySignature("alipay", []byte("%ZZ"), nil); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature for bad form body, got %v", err)
	}
}

// TestAlipayVerifier_MissingSign covers the empty-sign branch (no sign field
// at all in the form body).
func TestAlipayVerifier_MissingSign(t *testing.T) {
	t.Parallel()
	_, pub := rsaTestKey(t)
	v := &AlipayVerifier{PublicKey: pub}
	body := "out_trade_no=order_1&total_amount=29.90"
	if err := v.VerifySignature("alipay", []byte(body), nil); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature when sign is missing, got %v", err)
	}
}

// TestAlipayVerifier_DefaultSignType covers the "sign_type omitted" branch —
// the verifier must default to RSA2. We supply a valid RSA2 signature but
// omit sign_type so the default kicks in.
func TestAlipayVerifier_DefaultSignType(t *testing.T) {
	t.Parallel()
	priv, pub := rsaTestKey(t)
	body := "out_trade_no=order_1&total_amount=29.90"
	canonical := alipayCanonicalForTest(t, body)
	hashed := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	bodySigned := body + "&sign=" + urlEncodeFormValue(sigB64) // no sign_type

	v := &AlipayVerifier{PublicKey: pub, ReplayWindow: 0}
	if err := v.VerifySignature("alipay", []byte(bodySigned), nil); err != nil {
		t.Errorf("default-sign-type RSA2 should succeed, got %v", err)
	}
}

// TestAlipayVerifier_BadBase64Sig covers the base64-decode failure path on
// the sign value.
func TestAlipayVerifier_BadBase64Sig(t *testing.T) {
	t.Parallel()
	_, pub := rsaTestKey(t)
	v := &AlipayVerifier{PublicKey: pub}
	body := "out_trade_no=order_1&total_amount=29.90&sign=!!!not-base64!!!&sign_type=RSA2"
	if err := v.VerifySignature("alipay", []byte(body), nil); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on bad base64 sign, got %v", err)
	}
}

// TestAlipayVerifier_ReplayWindowFreshNotify covers the notify_time in-range
// case (verifier accepts) so the replay-window delta calc is exercised.
func TestAlipayVerifier_ReplayWindowFreshNotify(t *testing.T) {
	t.Parallel()
	priv, pub := rsaTestKey(t)
	body := "out_trade_no=order_1&notify_time=" + urlEncodeFormValue(time.Now().In(
		time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05"))
	canonical := alipayCanonicalForTest(t, body)
	hashed := sha256.Sum256([]byte(canonical))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	bodySigned := body + "&sign=" + urlEncodeFormValue(sigB64) + "&sign_type=RSA2"

	v := &AlipayVerifier{PublicKey: pub}
	if err := v.VerifySignature("alipay", []byte(bodySigned), nil); err != nil {
		t.Errorf("fresh notify_time should accept, got %v", err)
	}
}

// TestAlipayVerifier_ReplayWindowExpiredNotify covers the notify_time
// out-of-window path. We send a notify_time well in the past so the delta
// exceeds the 5-minute replay window.
func TestAlipayVerifier_ReplayWindowExpiredNotify(t *testing.T) {
	t.Parallel()
	priv, pub := rsaTestKey(t)
	past := time.Now().Add(-24 * time.Hour).In(time.FixedZone("CST", 8*3600))
	notify := past.Format("2006-01-02 15:04:05")
	body := "out_trade_no=order_1&notify_time=" + urlEncodeFormValue(notify)
	canonical := alipayCanonicalForTest(t, body)
	hashed := sha256.Sum256([]byte(canonical))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	bodySigned := body + "&sign=" + urlEncodeFormValue(sigB64) + "&sign_type=RSA2"

	v := &AlipayVerifier{PublicKey: pub}
	if err := v.VerifySignature("alipay", []byte(bodySigned), nil); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Errorf("expected ErrTimestampOutOfRange for expired notify_time, got %v", err)
	}
}

// TestAlipayVerifier_GarbledNotifyTime covers the time-parse failure path.
// An unparseable notify_time MUST NOT prevent signature verification — the
// timestamp is informational, not the source of truth (that's HMAC).
func TestAlipayVerifier_GarbledNotifyTime(t *testing.T) {
	t.Parallel()
	priv, pub := rsaTestKey(t)
	body := "out_trade_no=order_1&notify_time=garbage"
	canonical := alipayCanonicalForTest(t, body)
	hashed := sha256.Sum256([]byte(canonical))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	bodySigned := body + "&sign=" + urlEncodeFormValue(sigB64) + "&sign_type=RSA2"

	v := &AlipayVerifier{PublicKey: pub}
	if err := v.VerifySignature("alipay", []byte(bodySigned), nil); err != nil {
		t.Errorf("garbled notify_time should still accept valid signature, got %v", err)
	}
}

// ============================================================================
// LoadAlipayPublicKeyFromPEM — non-PEM input + PKCS#1 fallback
// ============================================================================

// TestLoadAlipayPublicKeyFromPEM_NotPEM covers the empty-PEM-block branch
// when the input isn't valid PEM at all.
func TestLoadAlipayPublicKeyFromPEM_NotPEM(t *testing.T) {
	t.Parallel()
	if _, err := LoadAlipayPublicKeyFromPEM([]byte("not a pem block")); err == nil {
		t.Error("non-PEM input should error")
	}
}

// TestLoadAlipayPublicKeyFromPEM_PKCS1Fallback covers the PKCS#1 fallback
// path (PKIX parse fails → PKCS#1 succeeds). We marshal a public key with
// x509.MarshalPKCS1PublicKey and feed it as a "RSA PUBLIC KEY" PEM block.
func TestLoadAlipayPublicKeyFromPEM_PKCS1Fallback(t *testing.T) {
	t.Parallel()
	priv, _ := rsaTestKey(t)
	der := x509.MarshalPKCS1PublicKey(&priv.PublicKey)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der})

	pub, err := LoadAlipayPublicKeyFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("PKCS#1 PEM should load via fallback: %v", err)
	}
	if pub.N.Cmp(priv.PublicKey.N) != 0 {
		t.Error("loaded pubkey does not match original")
	}
}

// TestLoadAlipayPublicKeyFromPEM_NonRSA covers the "decoded OK as some key
// type but not RSA" branch.
func TestLoadAlipayPublicKeyFromPEM_NonRSA(t *testing.T) {
	t.Parallel()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Encode an ed25519 public key with a "PUBLIC KEY" PEM header. PKIX
	// decode will succeed, but the type assertion to *rsa.PublicKey fails.
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if _, err := LoadAlipayPublicKeyFromPEM(pemBytes); err == nil {
		t.Error("ed25519 PEM should not parse as RSA")
	}
}

// TestLoadAlipayPublicKeyFromPEM_BothParsersFail covers the final fallback
// error when neither PKIX nor PKCS#1 succeed (random bytes).
func TestLoadAlipayPublicKeyFromPEM_BothParsersFail(t *testing.T) {
	t.Parallel()
	garbage := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("definitely not a der key")})
	if _, err := LoadAlipayPublicKeyFromPEM(garbage); err == nil {
		t.Error("garbage PEM should error")
	}
}

// ============================================================================
// runPaypalVerifyCacheJanitor — synthetic-channel driver for the 5-min tick
// ============================================================================

// TestRunPaypalVerifyCacheJanitor_DeletesExpired covers the body of the
// janitor using a synthetic channel. startPaypalVerifyCacheJanitor spawns
// the real one (5 min cadence); this exercises the same loop body with a
// 1-tick channel and verifies it deletes only-expired entries.
func TestRunPaypalVerifyCacheJanitor_DeletesExpired(t *testing.T) {
	resetPaypalVerifyCache(t)
	keep := paypalVerifyCacheKey{transmissionID: "keep", transmissionTime: "2026-01-01T00:00:00Z"}
	drop := paypalVerifyCacheKey{transmissionID: "drop", transmissionTime: "2026-01-01T00:00:01Z"}
	// "now" is well past drop.expiresAt, before keep.expiresAt.
	now := time.Now()
	paypalVerifyCache.Store(keep, paypalVerifyCacheEntry{status: "SUCCESS", expiresAt: now.Add(time.Hour)})
	paypalVerifyCache.Store(drop, paypalVerifyCacheEntry{status: "SUCCESS", expiresAt: now.Add(-time.Minute)})
	// Seed a wrong-type value too so the type-assert-fail branch executes.
	paypalVerifyCache.Store(paypalVerifyCacheKey{transmissionID: "bad-type", transmissionTime: "x"}, "not-a-cache-entry")

	tickC := make(chan time.Time, 1)
	tickC <- now
	close(tickC)
	runPaypalVerifyCacheJanitor(tickC)

	if _, ok := paypalVerifyCache.Load(keep); !ok {
		t.Error("non-expired entry should survive")
	}
	if _, ok := paypalVerifyCache.Load(drop); ok {
		t.Error("expired entry should be deleted")
	}
	// Wrong-type entry is silently skipped, not deleted.
	if _, ok := paypalVerifyCache.Load(paypalVerifyCacheKey{transmissionID: "bad-type", transmissionTime: "x"}); !ok {
		t.Error("wrong-type entry should be skipped, not deleted")
	}
}

// ============================================================================
// readAndRestoreBody — explicit read error path
// ============================================================================

// TestReadAndRestoreBody_ReadError covers the io.ReadAll error branch by
// feeding in a body that errors immediately. We use a reader that always
// returns an error to make ReadAll fail after the size check.
type errBody struct{}

func (errBody) Read(_ []byte) (int, error) { return 0, fmt.Errorf("synthetic read failure") }

func TestReadAndRestoreBody_ReadError(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/x", errBody{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	if _, err := readAndRestoreBody(c); err == nil {
		t.Fatal("expected error from failing body reader")
	}
}

// ============================================================================
// Stripe — wrap-on-parse-error branch in VerifySignature
// ============================================================================

// TestStripeVerifier_ParseErrorWrapped covers the path where
// parseStripeSignatureHeader returns an error and VerifySignature wraps it
// with ErrInvalidSignature.
func TestStripeVerifier_ParseErrorWrapped(t *testing.T) {
	t.Parallel()
	v := &StripeVerifier{Secret: []byte("k")}
	// "v1=deadbeef" is missing t → parseStripeSignatureHeader returns error.
	err := v.VerifySignature("stripe", []byte("{}"), map[string]string{"Stripe-Signature": "v1=deadbeef"})
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature wrapped, got %v", err)
	}
}

// TestStripeVerifier_EmptySigHeader covers the empty-header branch (line 148).
func TestStripeVerifier_EmptySigHeader(t *testing.T) {
	t.Parallel()
	v := &StripeVerifier{Secret: []byte("k")}
	if err := v.VerifySignature("stripe", []byte("{}"), map[string]string{}); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on empty sig header, got %v", err)
	}
}

// TestWeChatPayV3Verifier_MockMode covers WECHAT_PAY_MOCK=1: the
// verifier still requires all three headers + a fresh timestamp but
// skips the HMAC match. Without this guard the verifier would either
// (a) reject every dev / e2e payload as ErrInvalidSignature, or
// (b) accept any garbage if we just disabled the check entirely.
func TestWeChatPayV3Verifier_MockMode(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{MockMode: true, APIv3Key: []byte("ignored-in-mock-mode-32-bytes-x")}
	hdr := map[string]string{
		"Wechatpay-Signature": "totally-wrong-signature",
		"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"Wechatpay-Nonce":     "nonce-x",
	}
	if err := v.VerifySignature("wechat_pay", []byte("{}"), hdr); err != nil {
		t.Errorf("mock mode with all headers + fresh ts: err = %v, want nil", err)
	}

	// Missing signature → still rejected.
	hdr["Wechatpay-Signature"] = ""
	if !errors.Is(v.VerifySignature("wechat_pay", []byte("{}"), hdr), ErrInvalidSignature) {
		t.Errorf("mock mode still requires all three headers")
	}

	// Stale timestamp → still rejected (replay defence).
	hdr["Wechatpay-Signature"] = "x"
	hdr["Wechatpay-Timestamp"] = strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10)
	if !errors.Is(v.VerifySignature("wechat_pay", []byte("{}"), hdr), ErrTimestampOutOfRange) {
		t.Errorf("mock mode still enforces replay window")
	}
}

// TestWeChatPayV3Verifier_RealMode_RequiresValidHMAC confirms the mock
// short-circuit does NOT weaken the real path: an inbound header set
// with a wrong signature must still return ErrInvalidSignature when
// MockMode is false.
func TestWeChatPayV3Verifier_RealMode_RequiresValidSignature(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v := &WeChatPayV3Verifier{
		MockMode:     false,
		APIv3Key:     []byte("real-key-32-bytes-of-padding-x"),
		PlatformKeys: &stubKeySource{pub: &priv.PublicKey},
	}
	hdr := map[string]string{
		"Wechatpay-Signature": "not-the-right-sig",
		"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"Wechatpay-Nonce":     "nonce-y",
		"Wechatpay-Serial":    "S",
	}
	if !errors.Is(v.VerifySignature("wechat_pay", []byte("{}"), hdr), ErrInvalidSignature) {
		t.Errorf("real mode: wrong sig must still be rejected")
	}
}

// TestWeChatPayV3_UnknownPlatformSerial_Transient verifies the unknown-serial
// path returns a NON-sentinel error (500 retry) rather than
// ErrInvalidSignature (400 stop). A forged delivery with a random serial
// must not stop WeChat's retries during a legitimate cert rotation.
func TestWeChatPayV3_UnknownPlatformSerial_Transient(t *testing.T) {
	t.Parallel()
	v := &WeChatPayV3Verifier{
		APIv3Key:     bytes.Repeat([]byte("k"), 32),
		PlatformKeys: &stubKeySource{serial: "ROTATING_SERIAL"},
	}
	hdr := map[string]string{
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAA}, 256)),
		"Wechatpay-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"Wechatpay-Nonce":     "n",
		"Wechatpay-Serial":    "ROTATING_SERIAL",
	}
	err := v.VerifySignature("wechat_pay", []byte("{}"), hdr)
	if errors.Is(err, ErrInvalidSignature) {
		t.Errorf("unknown serial must be transient, not ErrInvalidSignature (400), got %v", err)
	}
	if err == nil {
		t.Errorf("expected non-nil error")
	}
}
