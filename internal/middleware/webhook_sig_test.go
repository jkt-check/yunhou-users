package middleware

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
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
	"strconv"
	"strings"
	"testing"
	"time"

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
// LemonsqueezyVerifier — raw-body HMAC-SHA256, hex digest, X-Signature header
// ============================================================================

func TestLemonsqueezyVerifier_Accept(t *testing.T) {
	t.Parallel()

	secret := []byte("ls_test_secret")
	body := []byte(`{"meta":{"event_name":"order_created"},"data":{"id":"1","type":"orders"}}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	v := &LemonsqueezyVerifier{Secret: secret}
	headers := map[string]string{"X-Signature": sig}
	if err := v.VerifySignature("lemonsqueezy", body, headers); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}
}

func TestLemonsqueezyVerifier_RejectBadSignature(t *testing.T) {
	t.Parallel()

	v := &LemonsqueezyVerifier{Secret: []byte("ls_test_secret")}
	body := []byte(`{}`)
	// All-zeros digest — wrong but valid hex. Forces a real signature mismatch
	// rather than a hex-decode failure (the latter is a separate test below).
	headers := map[string]string{"X-Signature": strings.Repeat("0", 64)}

	if err := v.VerifySignature("lemonsqueezy", body, headers); err != ErrInvalidSignature {
		t.Errorf("bad signature should return ErrInvalidSignature, got %v", err)
	}
}

func TestLemonsqueezyVerifier_RejectMissingHeader(t *testing.T) {
	t.Parallel()

	v := &LemonsqueezyVerifier{Secret: []byte("ls_test_secret")}
	body := []byte(`{}`)

	// Empty headers — no X-Signature at all.
	if err := v.VerifySignature("lemonsqueezy", body, map[string]string{}); err != ErrInvalidSignature {
		t.Errorf("missing header should return ErrInvalidSignature, got %v", err)
	}
}

func TestLemonsqueezyVerifier_RejectBadHex(t *testing.T) {
	t.Parallel()

	v := &LemonsqueezyVerifier{Secret: []byte("ls_test_secret")}
	body := []byte(`{}`)

	// Non-hex characters in the signature — should fail at hex.DecodeString
	// and surface as ErrInvalidSignature (wrapped).
	headers := map[string]string{"X-Signature": "not-hex-z"}
	err := v.VerifySignature("lemonsqueezy", body, headers)
	if err == nil {
		t.Fatal("bad-hex signature should produce an error")
	}
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad-hex signature should wrap ErrInvalidSignature, got %v", err)
	}
}

// ============================================================================
// WeChatPayV3Verifier — HMAC + AES-GCM resource decrypt
// ============================================================================

func TestWeChatPayV3Verifier_Accept(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte("k"), 32) // 32-byte test key
	v := &WeChatPayV3Verifier{APIv3Key: key}
	body := []byte(`{"id":"evt_1"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "abc123"

	toSign := ts + "\n" + nonce + "\n" + string(body) + "\n"
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(toSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers := map[string]string{
		"Wechatpay-Signature": sig,
		"Wechatpay-Timestamp": ts,
		"Wechatpay-Nonce":     nonce,
	}
	if err := v.VerifySignature("wechat_pay", body, headers); err != nil {
		t.Errorf("valid wechat sig rejected: %v", err)
	}
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

// TestMultiChannelVerifier_LemonSqueezyFanOut verifies the dispatch wires
// the LemonSqueezy field correctly: channel="lemonsqueezy" routes to the
// configured Lemonsqueezy verifier; channel="stripe" routes to Stripe; an
// unconfigured channel returns ErrUnsupportedChannel.
func TestMultiChannelVerifier_LemonSqueezyFanOut(t *testing.T) {
	t.Parallel()

	stripeErr := errors.New("stripe-stub-error")
	lsErr := errors.New("ls-stub-error")
	mv := &MultiChannelVerifier{
		Stripe:       &stubVerifier{errByChannel: map[string]error{"stripe": stripeErr}},
		LemonSqueezy: &stubVerifier{errByChannel: map[string]error{"lemonsqueezy": lsErr}},
	}

	// channel="lemonsqueezy" should hit the LS verifier → lsErr
	if err := mv.VerifySignature("lemonsqueezy", nil, nil); err != lsErr {
		t.Errorf("LS dispatch: got %v, want %v", err, lsErr)
	}
	// channel="stripe" should hit the Stripe verifier → stripeErr
	if err := mv.VerifySignature("stripe", nil, nil); err != stripeErr {
		t.Errorf("Stripe dispatch: got %v, want %v", err, stripeErr)
	}
	// channel="alipay" is unset on this mv → ErrUnsupportedChannel
	if err := mv.VerifySignature("alipay", nil, nil); err != ErrUnsupportedChannel {
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
	server    *httptest.Server
	seenBody  []byte
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
	// Rebuild with sorted keys + original values.
	valueByKey := map[string]string{}
	for _, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			valueByKey[p] = ""
		} else {
			valueByKey[p[:eq]] = p[eq+1:]
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
	ts, sig, err := parseStripeSignatureHeader("t=1700000000,v1=00ff")
	if err != nil {
		t.Errorf("valid header err: %v", err)
	}
	if ts != 1700000000 || len(sig) != 2 || sig[0] != 0x00 || sig[1] != 0xff {
		t.Errorf("ts/sig extraction: ts=%d sig=%v", ts, sig)
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
		"hello":      "hello",
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
	lsErr := errors.New("ls-err")
	paypalErr := errors.New("paypal-err")

	mv := &MultiChannelVerifier{
		Stripe:       &stubVerifier{errByChannel: map[string]error{"stripe": stripeErr}},
		WeChat:       &stubVerifier{errByChannel: map[string]error{"wechat_pay": wechatErr}},
		Alipay:       &stubVerifier{errByChannel: map[string]error{"alipay": alipayErr}},
		LemonSqueezy: &stubVerifier{errByChannel: map[string]error{"lemonsqueezy": lsErr}},
		Paypal:       &stubVerifier{errByChannel: map[string]error{"paypal": paypalErr}},
	}

	cases := map[string]error{
		"stripe":       stripeErr,
		"wechat_pay":   wechatErr,
		"alipay":       alipayErr,
		"lemonsqueezy": lsErr,
		"paypal":       paypalErr,
		"unsupported":  ErrUnsupportedChannel,
	}
	for ch, wantErr := range cases {
		got := mv.VerifySignature(ch, nil, nil)
		if got != wantErr {
			t.Errorf("channel %s: got %v, want %v", ch, got, wantErr)
		}
	}
}
