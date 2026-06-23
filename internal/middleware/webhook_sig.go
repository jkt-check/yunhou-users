package middleware

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ChannelSignatureVerifier is the per-channel abstraction. Production wires
// one verifier per channel (Stripe / WeChat / Alipay) with real secrets loaded
// from env at startup. Tests inject stubs.
//
// VerifySignature must:
//   - return ErrInvalidSignature if the signature is wrong (400 — channel won't retry)
//   - return ErrTimestampOutOfRange if the timestamp is outside the replay window
//   - return ErrUnsupportedChannel only when called for a channel it doesn't handle
//
// Returning a non-sentinel error is treated as transient (500 — channel retries).
type ChannelSignatureVerifier interface {
	VerifySignature(channel string, body []byte, headers map[string]string) error
}

// Sentinel errors used by both the middleware and the per-channel verifiers.
var (
	ErrInvalidSignature    = errors.New("invalid signature")
	ErrTimestampOutOfRange = errors.New("timestamp out of range")
	ErrUnsupportedChannel  = errors.New("unsupported channel")
)

// WebhookSignature is the Gin middleware factory. It reads the raw body (the
// request body is buffered, never re-serialized — see webhook doc §4.4 "use
// the raw body, not a re-serialized JSON") and calls the verifier for the
// channel named in the URL param. On success the body is restored so the
// handler can re-read it.
func WebhookSignature(verifier ChannelSignatureVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		channel := c.Param("channel")

		raw, err := readAndRestoreBody(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    400,
				"message": "could not read request body",
			})
			return
		}

		headers := collectHeaders(c)
		switch err := verifier.VerifySignature(channel, raw, headers); {
		case err == nil:
			c.Next()
		case errors.Is(err, ErrInvalidSignature), errors.Is(err, ErrTimestampOutOfRange):
			// 400: signature/timestamp issues are not retried by the channel.
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    400,
				"message": "invalid signature",
			})
		case errors.Is(err, ErrUnsupportedChannel):
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"code":    404,
				"message": "unknown channel",
			})
		default:
			// Transient (DB / config / unexpected). 500 so channel retries.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    500,
				"message": "signature verification failed",
			})
		}
	}
}

// readAndRestoreBody reads the entire request body and replaces
// c.Request.Body with a fresh reader. We cap at 1 MiB — webhook payloads
// are tiny; this guards against an attacker streaming a multi-GB body.
func readAndRestoreBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	const maxBodyBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errors.New("body too large")
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func collectHeaders(c *gin.Context) map[string]string {
	h := make(map[string]string, len(c.Request.Header))
	for k, v := range c.Request.Header {
		if len(v) > 0 {
			h[k] = v[0]
		}
	}
	return h
}

// ============================================================================
// Stripe — HMAC-SHA256 over `t.body`, replay window enforced
// ============================================================================

// StripeVerifier verifies Stripe-Signature: HMAC-SHA256 of `<t>.<raw_body>`
// with the secret; header format `t=<ts>,v1=<hex_hmac>` (multiple v1=
// values permitted — accept any that matches).
type StripeVerifier struct {
	Secret       []byte
	ReplayWindow time.Duration // default 5 min if zero
}

func (v *StripeVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	if channel != "stripe" {
		return ErrUnsupportedChannel
	}
	sigHeader := headers["Stripe-Signature"]
	if sigHeader == "" {
		return ErrInvalidSignature
	}
	ts, expectedHMAC, err := parseStripeSignatureHeader(sigHeader)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	window := v.ReplayWindow
	if window == 0 {
		window = 5 * time.Minute
	}
	if delta := time.Since(time.Unix(ts, 0)); delta > window || delta < -window {
		return ErrTimestampOutOfRange
	}
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(expectedHMAC, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

func parseStripeSignatureHeader(h string) (int64, []byte, error) {
	var ts int64
	var sigHex string
	for _, kv := range strings.Split(h, ",") {
		kv = strings.TrimSpace(kv)
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		switch kv[:eq] {
		case "t":
			n, err := strconv.ParseInt(kv[eq+1:], 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("bad timestamp")
			}
			ts = n
		case "v1":
			sigHex = kv[eq+1:]
		}
	}
	if ts == 0 || sigHex == "" {
		return 0, nil, fmt.Errorf("missing t or v1")
	}
	decoded, err := hex.DecodeString(sigHex)
	if err != nil {
		return 0, nil, fmt.Errorf("bad hex: %w", err)
	}
	return ts, decoded, nil
}

// ============================================================================
// WeChat Pay v3 — HMAC + AES-256-GCM resource decryption
// ============================================================================

// WeChatPayV3Verifier verifies WeChat Pay v3 webhooks. HMAC over
// `<ts>\n<nonce>\n<body>\n`, then handlers decrypt resource.ciphertext
// with AES-256-GCM (key = APIv3Key).
//
// Headers: Wechatpay-Signature, Wechatpay-Timestamp, Wechatpay-Nonce.
type WeChatPayV3Verifier struct {
	APIv3Key     []byte // 32 bytes
	ReplayWindow time.Duration
}

// DecryptResource decrypts a WeChat Pay v3 resource block. Called by the
// handler AFTER VerifySignature succeeds; not used by the middleware.
func (v *WeChatPayV3Verifier) DecryptResource(ciphertextB64, nonce, associatedData string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(v.APIv3Key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
}

func (v *WeChatPayV3Verifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	if channel != "wechat_pay" {
		return ErrUnsupportedChannel
	}
	sigB64 := headers["Wechatpay-Signature"]
	tsStr := headers["Wechatpay-Timestamp"]
	nonce := headers["Wechatpay-Nonce"]
	if sigB64 == "" || tsStr == "" || nonce == "" {
		return ErrInvalidSignature
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrInvalidSignature)
	}
	window := v.ReplayWindow
	if window == 0 {
		window = 5 * time.Minute
	}
	if delta := time.Since(time.Unix(ts, 0)); delta > window || delta < -window {
		return ErrTimestampOutOfRange
	}
	toSign := tsStr + "\n" + nonce + "\n" + string(body) + "\n"
	mac := hmac.New(sha256.New, v.APIv3Key)
	mac.Write([]byte(toSign))
	got, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("%w: bad base64 sig", ErrInvalidSignature)
	}
	if !hmac.Equal(mac.Sum(nil), got) {
		return ErrInvalidSignature
	}
	return nil
}

// ============================================================================
// Alipay — RSA2 (SHA256WithRSA) over canonical-string of form params
// ============================================================================

// AlipayVerifier verifies Alipay RSA2 (SHA256WithRSA) signatures. The signed
// canonical string is the URL-encoded form params (excluding sign / sign_type)
// sorted alphabetically by key. Public key is loaded once at startup.
//
// Note: Alipay sends the payload as application/x-www-form-urlencoded in the
// request body, NOT in headers — the middleware hands us the body.
type AlipayVerifier struct {
	PublicKey    *rsa.PublicKey
	ReplayWindow time.Duration // applied to notify_time field, 0 = disabled
}

func (v *AlipayVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	if channel != "alipay" {
		return ErrUnsupportedChannel
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return fmt.Errorf("%w: bad form body", ErrInvalidSignature)
	}
	sign := values.Get("sign")
	signType := values.Get("sign_type")
	if sign == "" {
		return ErrInvalidSignature
	}
	if signType == "" {
		signType = "RSA2" // default per Alipay modern spec
	}
	if signType != "RSA2" {
		return fmt.Errorf("%w: unsupported sign_type %s", ErrInvalidSignature, signType)
	}

	// Canonical string: keys sorted, excluding sign/sign_type, Alipay-encoded.
	keys := make([]string, 0, len(values))
	for k := range values {
		if k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, alipayURLEncode(k)+"="+alipayURLEncode(values.Get(k)))
	}
	canonical := strings.Join(parts, "&")

	sig, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return fmt.Errorf("%w: bad base64 sig", ErrInvalidSignature)
	}
	hashed := sha256.Sum256([]byte(canonical))
	if err := rsa.VerifyPKCS1v15(v.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
		return ErrInvalidSignature
	}

	if v.ReplayWindow > 0 {
		if notifyTime := values.Get("notify_time"); notifyTime != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", notifyTime); err == nil {
				if delta := time.Since(t); delta > v.ReplayWindow || delta < -v.ReplayWindow {
					return ErrTimestampOutOfRange
				}
			}
		}
	}
	return nil
}

// alipayURLEncode mirrors Alipay's encoding (space → %20, not '+').
// Standard net/url uses '+' for space; this differs.
func alipayURLEncode(s string) string {
	const hexChars = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			sb.WriteString("%20")
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

// LoadAlipayPublicKeyFromPEM parses either PKCS#1 or PKCS#8 PEM-encoded RSA
// public keys. Alipay's merchant console publishes PKCS#8 today but older
// docs reference PKCS#1 — accept both.
func LoadAlipayPublicKeyFromPEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaPub, ok := pub.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
		return nil, errors.New("not an RSA public key")
	}
	if pub, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return pub, nil
	}
	return nil, errors.New("failed to parse PEM (tried PKIX and PKCS#1)")
}

// ============================================================================
// MultiChannelVerifier — fan-out for production wiring
// ============================================================================

// MultiChannelVerifier dispatches to per-channel verifiers. Pass nil for any
// channel not configured yet — the middleware returns 404 for it.
type MultiChannelVerifier struct {
	Stripe ChannelSignatureVerifier
	WeChat ChannelSignatureVerifier
	Alipay ChannelSignatureVerifier
}

func (m *MultiChannelVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	var v ChannelSignatureVerifier
	switch channel {
	case "stripe":
		v = m.Stripe
	case "wechat_pay":
		v = m.WeChat
	case "alipay":
		v = m.Alipay
	default:
		return ErrUnsupportedChannel
	}
	if v == nil {
		return ErrUnsupportedChannel
	}
	return v.VerifySignature(channel, body, headers)
}