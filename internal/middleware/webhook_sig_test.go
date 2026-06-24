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
	"encoding/pem"
	"errors"
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
