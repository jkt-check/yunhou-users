package wechat

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"
)

// Signer builds the WeChat Pay v3 Authorization header.
// Construct once at startup (cert serial + private key are immutable
// for the lifetime of the process); share across goroutines — methods
// are read-only.
type Signer struct {
	MchID      string // 商户号
	SerialNo   string // uppercase hex from cert (WeChat's `serial_no` field format)
	PrivateKey *rsa.PrivateKey
}

// BuildAuthHeader returns the value for the `Authorization` HTTP header
// on outbound requests to api.mch.weixin.qq.com. Caller supplies the
// HTTP method, request path (no host, no query — e.g.
// "/v3/pay/transactions/native"), and raw body bytes.
//
// Format (WeChat Pay v3 docs §"签名生成"):
//
//	scheme = "WECHATPAY2-SHA256-RSA2048"
//	message = METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + BODY + "\n"
//	sign = base64( RSA-SHA256(message, PrivateKey) )
//	Authorization = scheme + ' ' + kv pairs (mchid, nonce_str, timestamp, serial_no, signature)
//
// NOTE: APIv3Key is NOT used for outbound signing — it is reserved for
// inbound webhook HMAC + AES-GCM resource decryption (handled by
// WeChatPayV3Verifier in middleware/webhook_sig.go). Outbound requests
// to api.mch.weixin.qq.com are signed with the merchant RSA private key
// only; WeChat's successful responses (e.g. UnifiedOrder) carry the
// result in the body and are NOT response-signed. Transport-level
// integrity relies on TLS. Verifying WeChat's platform-cert response
// signatures (a v3 feature for refund/close-order flows) is deferred.
func (s *Signer) BuildAuthHeader(method, reqPath string, body []byte) (string, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("nonce gen: %w", err)
	}
	nonce := fmt.Sprintf("%x", nonceBytes)
	return s.buildAuthHeaderWith(method, reqPath, body, ts, nonce)
}

// buildAuthHeaderWith is the deterministic core of BuildAuthHeader: it
// takes the timestamp + nonce as inputs so the FixedVector test can
// replay the captured values and re-derive the signature. RSA-PKCS1v15
// is deterministic given a (key, message) pair, so feeding the captured
// timestamp + nonce through this helper reproduces the captured
// signature byte-for-byte — no time / RNG mocking needed.
func (s *Signer) buildAuthHeaderWith(method, reqPath string, body []byte, ts, nonce string) (string, error) {
	msg := method + "\n" + reqPath + "\n" + ts + "\n" + nonce + "\n" + string(body) + "\n"
	h := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.PrivateKey, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("rsa sign: %w", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	return fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",timestamp="%s",serial_no="%s",signature="%s"`,
		s.MchID, nonce, ts, s.SerialNo, sigB64,
	), nil
}
