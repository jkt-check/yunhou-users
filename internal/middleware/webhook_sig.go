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
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// MultiChannelVerifier already routes by channel before calling us;
	// the per-channel channel-name guard is defensive scaffolding.
	_ = channel
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
	// GCM requires exactly 12-byte nonce (Go stdlib hardcodes NonceSize()=12).
	// Without this guard, gcm.Open panics on wrong-length nonces, which Gin
	// recovers as a 500 — a DoS vector under crafted input.
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length: got %d, want %d", len(nonce), gcm.NonceSize())
	}
	return gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
}

func (v *WeChatPayV3Verifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	// MultiChannelVerifier already routes by channel before calling us;
	// the per-channel channel-name guard is defensive scaffolding.
	_ = channel
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
	// MultiChannelVerifier already routes by channel before calling us;
	// the per-channel channel-name guard is defensive scaffolding.
	_ = channel
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

	// Replay window defaults to 5 min (matches Stripe/WeChat). Alipay retries
	// notifications for ~24h, so without this guard a captured notification
	// could be replayed well outside the legitimate retry schedule.
	replayWindow := v.ReplayWindow
	if replayWindow == 0 {
		replayWindow = 5 * time.Minute
	}
	if notifyTime := values.Get("notify_time"); notifyTime != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", notifyTime); err == nil {
			if delta := time.Since(t); delta > replayWindow || delta < -replayWindow {
				return ErrTimestampOutOfRange
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
// PayPal — HTTP verify-webhook-signature (sandbox + live both loaded)
// ============================================================================

// PaypalVerifier verifies PayPal webhooks by POSTing the request body + the
// five PayPal signature headers to PayPal's verify-webhook-signature REST
// endpoint. The endpoint base URL is selected from Env ("sandbox" | "live");
// both webhook IDs and API bases are loaded at startup so deployments don't
// need to restart to flip environments.
//
// Replay protection is provided by the event-level dedupe (webhook_events
// UNIQUE(channel, event_id)). PayPal's transmission_time is meant only for
// PayPal's own verification, not for our dedup, so we don't enforce a local
// replay window.
//
// verifyCache is a tiny in-process cache that absorbs PayPal's retry bursts.
// PayPal retries on transient errors within seconds; without the cache every
// retry pays a full DNS+TCP+TLS+RTT to api-m.paypal.com. Key is the unique
// (transmission_id, transmission_time) tuple so a genuine second delivery at
// a different time isn't suppressed.
const paypalVerifyCacheTTL = 60 * time.Second

type paypalVerifyCacheEntry struct {
	status    string // "SUCCESS" | "FAILURE" | other (mirrors verification_status)
	expiresAt time.Time
	err       error // first-call verdict; subsequent calls return the same
}

var paypalVerifyCache sync.Map // map[paypalVerifyCacheKey]paypalVerifyCacheEntry

type paypalVerifyCacheKey struct {
	transmissionID   string
	transmissionTime string
}

func lookupVerifyCache(k paypalVerifyCacheKey) (paypalVerifyCacheEntry, bool) {
	if v, ok := paypalVerifyCache.Load(k); ok {
		entry := v.(paypalVerifyCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry, true
		}
		paypalVerifyCache.Delete(k)
	}
	return paypalVerifyCacheEntry{}, false
}

func storeVerifyCache(k paypalVerifyCacheKey, status string, err error) {
	paypalVerifyCache.Store(k, paypalVerifyCacheEntry{
		status:    status,
		expiresAt: time.Now().Add(paypalVerifyCacheTTL),
		err:       err,
	})
}

type PaypalVerifier struct {
	HTTPClient       *http.Client // nil → http.DefaultClient; production should set Timeout.
	SandboxWebhookID string
	LiveWebhookID    string
	SandboxAPIBase   string // default https://api-m.sandbox.paypal.com
	LiveAPIBase      string // default https://api-m.paypal.com
	Env              string // "sandbox" | "live"
}

// activeConfig resolves which (webhookID, apiBase) pair the verifier should
// use for the current Env. Unknown Env or empty base returns defaults /
// errors so they surface as ErrUnsupportedChannel at the middleware.
func (v *PaypalVerifier) activeConfig() (webhookID, apiBase string, err error) {
	switch v.Env {
	case "sandbox":
		return v.SandboxWebhookID, v.SandboxAPIBase, nil
	case "live":
		return v.LiveWebhookID, v.LiveAPIBase, nil
	default:
		return "", "", fmt.Errorf("%w: unknown PAYPAL_ENV %q", ErrUnsupportedChannel, v.Env)
	}
}

func (v *PaypalVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	// MultiChannelVerifier already routes by channel before calling us; the
	// per-channel channel-name guard is defensive scaffolding.
	_ = channel

	authAlgo := headers["PAYPAL-AUTH-ALGO"]
	certURL := headers["PAYPAL-CERT-URL"]
	transmissionID := headers["PAYPAL-TRANSMISSION-ID"]
	transmissionSIG := headers["PAYPAL-TRANSMISSION-SIG"]
	transmissionTime := headers["PAYPAL-TRANSMISSION-TIME"]
	if authAlgo == "" || certURL == "" || transmissionID == "" ||
		transmissionSIG == "" || transmissionTime == "" {
		return ErrInvalidSignature
	}

	// Cache hit: short-circuit the upstream call. PayPal retries on
	// transient errors within seconds; the (transmission_id, transmission_time)
	// tuple uniquely identifies a delivery, so a cached SUCCESS applies to
	// the genuine retry burst only.
	if entry, ok := lookupVerifyCache(paypalVerifyCacheKey{
		transmissionID:   transmissionID,
		transmissionTime: transmissionTime,
	}); ok {
		return entry.err
	}

	webhookID, apiBase, err := v.activeConfig()
	if err != nil {
		return err
	}
	if webhookID == "" {
		// Channel was requested but no webhook ID is configured for the
		// active env → treat as unsupported so middleware returns 404.
		return ErrUnsupportedChannel
	}
	if apiBase == "" {
		// Misconfigured: Env says sandbox/live but the matching API base
		// is unset. Rather than silently fall through to a hardcoded
		// default (which used to default to LIVE, a footgun), treat as
		// unsupported so the operator notices via 404.
		return ErrUnsupportedChannel
	}

	payload, err := json.Marshal(map[string]any{
		"auth_algo":         authAlgo,
		"cert_url":          certURL,
		"transmission_id":   transmissionID,
		"transmission_sig":  transmissionSIG,
		"transmission_time": transmissionTime,
		"webhook_id":        webhookID,
		"webhook_event":     json.RawMessage(body),
	})
	if err != nil {
		return fmt.Errorf("paypal verify marshal: %w", err)
	}

	httpClient := v.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodPost,
		apiBase+"/v1/notifications/verify-webhook-signature",
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("paypal verify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		// Network error → transient (500), let PayPal retry.
		return fmt.Errorf("paypal verify http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("paypal verify read: %w", err)
	}
	if resp.StatusCode >= 400 {
		// PayPal's verify endpoint rarely returns 4xx; treat any non-2xx
		// as transient so PayPal retries per its schedule.
		return fmt.Errorf("paypal verify status %d: %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("paypal verify decode: %w", err)
	}
	if out.VerificationStatus != "SUCCESS" {
		storeVerifyCache(paypalVerifyCacheKey{
			transmissionID:   transmissionID,
			transmissionTime: transmissionTime,
		}, out.VerificationStatus, ErrInvalidSignature)
		return ErrInvalidSignature
	}
	storeVerifyCache(paypalVerifyCacheKey{
		transmissionID:   transmissionID,
		transmissionTime: transmissionTime,
	}, out.VerificationStatus, nil)
	return nil
}

// ============================================================================
// MultiChannelVerifier — fan-out for production wiring
// ============================================================================

// MultiChannelVerifier dispatches to per-channel verifiers. Pass nil for any
// channel not configured yet — the middleware returns 404 for it.
type MultiChannelVerifier struct {
	Stripe       ChannelSignatureVerifier
	WeChat       ChannelSignatureVerifier
	Alipay ChannelSignatureVerifier
	Paypal ChannelSignatureVerifier
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
	case "paypal":
		v = m.Paypal
	default:
		return ErrUnsupportedChannel
	}
	if v == nil {
		return ErrUnsupportedChannel
	}
	return v.VerifySignature(channel, body, headers)
}
