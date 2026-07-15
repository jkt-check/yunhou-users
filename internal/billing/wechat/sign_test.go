package wechat

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type authVector struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	Body         string `json:"body"`
	MchID        string `json:"mch_id"`
	ExpectedAuth string `json:"expected_authorization"`
	SerialNo     string `json:"serial_no"`
}

func loadSignerForTest(t *testing.T) *Signer {
	t.Helper()
	keyPEM, err := os.ReadFile("testdata/sign_test_key.pem")
	if err != nil {
		t.Fatalf("read test key: %v", err)
	}
	certPEM, err := os.ReadFile("testdata/sign_test_cert.pem")
	if err != nil {
		t.Fatalf("read test cert: %v", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatalf("decode key PEM")
	}
	var rsaKey *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err == nil {
		rsaKey = k
	} else {
		keyAny, perr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if perr != nil {
			t.Fatalf("parse test key (pkcs1=%v pkcs8=%v)", err, perr)
		}
		var ok bool
		rsaKey, ok = keyAny.(*rsa.PrivateKey)
		if !ok {
			t.Fatalf("test key is %T, not RSA", keyAny)
		}
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatalf("decode cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse test cert: %v", err)
	}
	return &Signer{MchID: "test_mch", SerialNo: serialToDecimal(cert.SerialNumber), PrivateKey: rsaKey}
}

func TestBuildAuthHeader_FixedVector(t *testing.T) {
	s := loadSignerForTest(t)
	vecData, err := os.ReadFile("testdata/sign_test_vector.json")
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var vec authVector
	if err := json.Unmarshal(vecData, &vec); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	s.MchID = vec.MchID
	s.SerialNo = vec.SerialNo

	// The captured fixture's signature is bound to the timestamp + nonce
	// captured in the same header. RSA-PKCS1v15 is deterministic given
	// (key, message), so replaying the captured ts + nonce through the
	// helper reproduces the signature byte-for-byte — no clock / RNG
	// mocking required.
	capturedTS := extractAuthField(t, vec.ExpectedAuth, "timestamp")
	capturedNonce := extractAuthField(t, vec.ExpectedAuth, "nonce_str")

	got, err := s.buildAuthHeaderWith(vec.Method, vec.Path, []byte(vec.Body), capturedTS, capturedNonce)
	if err != nil {
		t.Fatalf("buildAuthHeaderWith: %v", err)
	}
	if got != vec.ExpectedAuth {
		t.Fatalf("BuildAuthHeader mismatch\n got: %s\nwant: %s", got, vec.ExpectedAuth)
	}
}

func TestBuildAuthHeader_NonceUniqueness(t *testing.T) {
	s := loadSignerForTest(t)
	a, err := s.BuildAuthHeader("POST", "/x", []byte("{}"))
	if err != nil {
		t.Fatalf("BuildAuthHeader a: %v", err)
	}
	b, err := s.BuildAuthHeader("POST", "/x", []byte("{}"))
	if err != nil {
		t.Fatalf("BuildAuthHeader b: %v", err)
	}
	extract := func(h string) string {
		i := strings.Index(h, `nonce_str="`)
		if i < 0 {
			t.Fatalf("no nonce_str in %s", h)
		}
		rest := h[i+11:]
		j := strings.Index(rest, `"`)
		return rest[:j]
	}
	if extract(a) == extract(b) {
		t.Fatalf("nonce_str repeated across calls: %s", extract(a))
	}
}

func TestBuildAuthHeader_TimestampFresh(t *testing.T) {
	s := loadSignerForTest(t)
	h, err := s.BuildAuthHeader("POST", "/x", []byte("{}"))
	if err != nil {
		t.Fatalf("BuildAuthHeader: %v", err)
	}
	i := strings.Index(h, `timestamp="`)
	if i < 0 {
		t.Fatalf("no timestamp in %s", h)
	}
	rest := h[i+11:]
	j := strings.Index(rest, `"`)
	tsStr := rest[:j]
	ts, err := parseInt64(tsStr)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", tsStr, err)
	}
	delta := time.Now().Unix() - ts
	if delta < 0 {
		delta = -delta
	}
	if delta > 2 {
		t.Fatalf("timestamp drift %ds (>2s)", delta)
	}
}

func extractAuthField(t *testing.T, header, field string) string {
	t.Helper()
	key := field + `="`
	i := strings.Index(header, key)
	if i < 0 {
		t.Fatalf("no %s in %s", field, header)
	}
	rest := header[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated %s in %s", field, header)
	}
	return rest[:j]
}

func parseInt64(s string) (int64, error) {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q in %q", c, s)
		}
		v = v*10 + int64(c-'0')
	}
	return v, nil
}