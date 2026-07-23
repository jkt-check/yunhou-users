package wechat

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Platform certificate management for WeChat Pay v3 INBOUND webhook
// signature verification.
//
// Real WeChat v3 callbacks are signed with WeChat's platform RSA private
// key (SHA256-RSA over "<ts>\n<nonce>\n<body>\n"); the merchant verifies
// with the WeChat PLATFORM certificate's public key, selected by the
// Wechatpay-Serial header. Platform certificates are fetched from
// GET /v3/certificates (merchant-signed request) where each certificate
// is AES-256-GCM-encrypted with the merchant APIv3 key.
//
// 2026-07-23: the previous verifier HMAC-SHA256'd the payload with the
// APIv3 key — a scheme WeChat has never used for callbacks — so EVERY
// real TRANSACTION.SUCCESS on cn-staging failed verification with 400
// and paid orders never reconciled (no payment row, no subscription).
// The APIv3 key is ONLY for AES-GCM decryption (callback resource +
// platform certificates), never for signature verification.
//
// Cert rotation: WeChat rotates platform certificates; the
// Wechatpay-Serial header tells us which cert signed this delivery.
// An unknown serial triggers exactly one refresh per fetchInterval and
// a re-lookup; if the serial is still unknown the caller treats it as
// transient (500) so WeChat retries per its schedule.

// ErrUnknownPlatformSerial is returned when the requested serial is not
// in the cache even after a refresh. Callers should treat this as
// transient (WeChat may have just rotated to a cert we cannot fetch
// yet, or the request is forged — either way, rejecting with 400 would
// stop WeChat's retries for a legitimate delivery during rotation).
var ErrUnknownPlatformSerial = errors.New("unknown wechat platform certificate serial")

// platformCertsResponse mirrors GET /v3/certificates' body.
type platformCertsResponse struct {
	Data []struct {
		SerialNo          string    `json:"serial_no"`
		EffectiveTime     time.Time `json:"effective_time"`
		ExpireTime        time.Time `json:"expire_time"`
		EncryptCertificate struct {
			Algorithm      string `json:"algorithm"`
			Nonce          string `json:"nonce"`
			AssociatedData string `json:"associated_data"`
			Ciphertext     string `json:"ciphertext"`
		} `json:"encrypt_certificate"`
	} `json:"data"`
}

// PlatformCertManager fetches and caches WeChat platform certificates.
// Safe for concurrent use. The zero-TL;DR: certificates are refreshed
// lazily on serial-miss and proactively when older than maxAge.
type PlatformCertManager struct {
	Signer   *Signer
	APIv3Key []byte // 32 bytes; decrypts encrypt_certificate blocks
	BaseURL  string // https://api.mch.weixin.qq.com
	HTTPDoer HTTPDoer

	// FetchTimeout bounds each outbound /v3/certificates call. Zero → 10s.
	FetchTimeout time.Duration

	mu        sync.Mutex
	certs     map[string]*rsa.PublicKey // serial_no → public key
	fetchedAt time.Time

	// refreshing is a CAS gate that ensures at most ONE goroutine runs
	// refresh() at a time across the whole process. Without it, a burst
	// of webhook deliveries with random serials would each pass the
	// minFetchInterval check (gate becomes true for all of them when
	// fetchedAt is old) and each issue their own /v3/certificates call —
	// converting a single WeChat-side incident into N upstream requests.
	refreshing atomic.Bool
}

// platformCertMaxAge caps how long a cached cert set is trusted without
// a refresh when lookups keep hitting. WeChat's own certs live ~5 years,
// but rotation announcements advise refreshing on a schedule; 12h keeps
// us well inside any rotation overlap window while keeping outbound
// calls at ~2/day.
const platformCertMaxAge = 12 * time.Hour

// platformCertMinFetchInterval rate-limits refreshes triggered by
// serial-miss so a flood of forged webhooks with random serials cannot
// turn into an outbound request amplifier against api.mch.weixin.qq.com.
const platformCertMinFetchInterval = 60 * time.Second

// PublicKeyFor returns the platform certificate public key for the
// given serial (from the Wechatpay-Serial header), refreshing the cache
// on miss or when stale. Returns ErrUnknownPlatformSerial if the serial
// is unknown after a refresh.
func (m *PlatformCertManager) PublicKeyFor(ctx context.Context, serial string) (*rsa.PublicKey, error) {
	if serial == "" {
		return nil, fmt.Errorf("%w: empty serial", ErrUnknownPlatformSerial)
	}

	m.mu.Lock()
	if key, ok := m.certs[serial]; ok && time.Since(m.fetchedAt) < platformCertMaxAge {
		m.mu.Unlock()
		return key, nil
	}
	// Miss (or stale cache). Try to win the single-flight gate so only
	// one goroutine issues /v3/certificates at a time. Other concurrent
	// callers either (a) lose the CAS and spin-wait for the in-flight
	// refresh to finish, then re-read the cache, or (b) lose the rate-
	// limit gate (already refreshed within platformCertMinFetchInterval)
	// and skip the refresh entirely. Either way, the upstream is hit
	// at most once per cache-miss event regardless of goroutine count.
	canRefresh := time.Since(m.fetchedAt) >= platformCertMinFetchInterval
	m.mu.Unlock()

	if canRefresh && m.refreshing.CompareAndSwap(false, true) {
		// We're the designated refresher.
		err := m.refresh(ctx)
		m.refreshing.Store(false)
		if err != nil {
			// Refresh failed. A cached-but-stale key is still
			// cryptographically valid (platform certs live ~5 years;
			// maxAge is a freshness policy, not certificate expiry), so
			// fall back to it rather than rejecting a legitimate
			// delivery during an upstream outage. Mark fetchedAt so the
			// min-fetch-interval guard engages on the next miss
			// instead of retrying the upstream on every incoming
			// delivery.
			m.mu.Lock()
			key, ok := m.certs[serial]
			m.fetchedAt = time.Now()
			m.mu.Unlock()
			if ok {
				return key, nil
			}
			return nil, err
		}
	} else if canRefresh {
		// Another goroutine is already refreshing. Spin-wait for it to
		// finish so we read its cache (which may contain the serial we
		// just missed). Cap the wait at the fetch timeout so a stuck
		// upstream doesn't wedge the verifier.
		deadline := time.Now().Add(10 * time.Second)
		for m.refreshing.Load() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.certs[serial]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlatformSerial, serial)
	}
	return key, nil
}

// refresh fetches /v3/certificates and atomically swaps the cache.
func (m *PlatformCertManager) refresh(ctx context.Context) error {
	timeout := m.FetchTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const reqPath = "/v3/certificates"
	auth, err := m.Signer.BuildAuthHeader("GET", reqPath, nil)
	if err != nil {
		return fmt.Errorf("build auth: %w", err)
	}
	resp, err := m.HTTPDoer.Do(ctx, &HTTPRequest{
		Method: "GET",
		URL:    m.BaseURL + reqPath,
		Headers: map[string]string{
			"Authorization": auth,
			"Accept":        "application/json",
			"User-Agent":    userAgent,
		},
	})
	if err != nil {
		return fmt.Errorf("fetch platform certificates: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("fetch platform certificates: status %d", resp.StatusCode)
	}

	var parsed platformCertsResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return fmt.Errorf("decode platform certificates: %w", err)
	}
	if len(parsed.Data) == 0 {
		return errors.New("fetch platform certificates: empty data array")
	}

	fresh := make(map[string]*rsa.PublicKey, len(parsed.Data))
	for i := range parsed.Data {
		d := &parsed.Data[i]
		pub, err := m.decryptCert(d.EncryptCertificate.Ciphertext, d.EncryptCertificate.Nonce, d.EncryptCertificate.AssociatedData)
		if err != nil {
			return fmt.Errorf("decrypt platform cert %s: %w", d.SerialNo, err)
		}
		fresh[d.SerialNo] = pub
	}

	m.mu.Lock()
	m.certs = fresh
	m.fetchedAt = time.Now()
	m.mu.Unlock()
	return nil
}

// decryptCert AES-256-GCM-decrypts one encrypt_certificate block with
// the merchant APIv3 key and parses the PEM certificate's RSA public key.
func (m *PlatformCertManager) decryptCert(ciphertextB64, nonce, associatedData string) (*rsa.PublicKey, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(m.APIv3Key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length: got %d, want %d", len(nonce), gcm.NonceSize())
	}
	pemBytes, err := gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
	if err != nil {
		return nil, fmt.Errorf("gcm open: %w", err)
	}
	pemBlock, _ := pem.Decode(pemBytes)
	if pemBlock == nil {
		return nil, errors.New("no PEM block in decrypted certificate")
	}
	cert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("platform cert public key is %T, want *rsa.PublicKey", cert.PublicKey)
	}
	return pub, nil
}
