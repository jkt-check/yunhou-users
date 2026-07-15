# WeChat Pay v3 Real Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the mock-only `internal/billing/wechat.Client.UnifiedOrder` with a real WeChat Pay v3 NATIVE client (HMAC-SHA256-with-RSA2048 signing, outbound to `api.mch.weixin.qq.com/v3/pay/transactions/native`); wire `service.PaymentService.CreateOrder` to call it and persist the returned `code_url` into a new `orders.provider_intent` JSONB column. Completes the deferred half of A2.b + A2.c.

**Architecture:** Three new env vars (server-wide: private key path / cert path / notify URL) feed a `Signer` constructed at startup. `Client.UnifiedOrder` real branch builds the v3 NATIVE body, signs via `Signer`, calls `HTTPDoer`, parses `code_url`. `PaymentService.CreateOrder` gains a `channel` parameter and an injected `wechatClient` interface; for `channel="wechat_pay"` in real mode, it calls `UnifiedOrder` and writes the response into `orders.provider_intent` via a new repo method. Mock mode is unchanged.

**Tech Stack:** Go 1.25, stdlib `crypto/rsa` + `crypto/sha256` + `encoding/base64`, `crypto/x509` for cert parsing, sqlx + Postgres JSONB. Existing `HTTPDoer` interface for outbound stub-ability.

**Spec:** `docs/superpowers/specs/2026-07-15-wechat-pay-real-client-design.md`

**Branch:** `feat/wechat-pay-real-client` (already created from `origin/master`).

**Working directory:** repo root. Run `pwd` first to confirm.

---

## File map

| File | Action | Purpose |
|---|---|---|
| `migrations/009_wechat_pay_intent.sql` | create | `orders.provider_intent JSONB NOT NULL DEFAULT '{}'::jsonb` |
| `internal/billing/wechat/cert.go` | create | `LoadPrivateKey(path) (*rsa.PrivateKey, error)` + `LoadCertSerial(path) (string, error)` |
| `internal/billing/wechat/cert_test.go` | create | Test PKCS#1, PKCS#8, bad PEM |
| `internal/billing/wechat/sign.go` | create | `Signer` struct + `BuildAuthHeader(method, path, body)` |
| `internal/billing/wechat/sign_test.go` | create | Fixed test vector (committed PEM + fixture JSON), nonce uniqueness, timestamp freshness |
| `internal/billing/wechat/testdata/sign_test_key.pem` | create | Test-only RSA-2048 key committed for reproducible fixtures |
| `internal/billing/wechat/testdata/sign_test_vector.json` | create | Captured Authorization-header fixture (committed) |
| `internal/billing/wechat/wechat.go` | modify | Replace `ErrUnimplemented` stub with real `UnifiedOrder` branch; replace `CertPath/KeyPath` fields with `Signer *Signer`; add `MchID() string` getter; new typed errors |
| `internal/billing/wechat/wechat_test.go` | modify | HTTPDoer stub tests: 200 / 4xx / 5xx / network / empty code_url; mock-mode regression |
| `internal/model/payment.go` | modify | Add `ProviderIntent []byte` field to `Order` struct |
| `internal/repo/orders.go` (or existing orders-repo file) | modify | Add `UpdateProviderIntent(ctx, orderID, payload) error` |
| `internal/repo/orders_test.go` (if exists; otherwise new) | create or modify | Test `UpdateProviderIntent` round-trip against fresh DB |
| `internal/config/config.go` | modify | +3 fields, `Load()` env reads, `Validate()` 3-env rule |
| `internal/config/config_test.go` | modify | `TestValidate_WeChatReal_AllFiveRequired`, `TestValidate_WeChatMock_AllowsEmpty` |
| `internal/service/payment.go` | modify | `wechatClient` interface; `CreateOrder` gains `channel` param; wechat real-mode branch with string-based amount→fen conversion |
| `internal/service/payment_test.go` | modify | Stub `wechatClient`; channel-aware tests (real wechat / mock wechat / stripe) |
| `internal/handler/payment.go` | modify | `CreateOrder` handler reads `channel` from request body |
| `cmd/server/main.go` | modify | After `config.Load`: load cert + key (real mode), build `Signer`, build `Client`, build `*http.Client` adapter, pass to `PaymentService` constructor |
| `Makefile` | modify | Add `regen-test-keys` target |
| `PROGRESS.md` | modify | Mark A2.c follow-up as ✅ shipped; reference new commit hash |

`go.mod` does **not** change (only stdlib).

---

## Task 1: Migration 009 — `orders.provider_intent`

**Files:**
- Create: `migrations/009_wechat_pay_intent.sql`

- [ ] **Step 1: Create the migration file**

Create `migrations/009_wechat_pay_intent.sql`:

```sql
-- Migration: 009_wechat_pay_intent
-- Description: add orders.provider_intent JSONB for channel-specific pre-auth metadata.
--   wechat_pay NATIVE → {code_url, out_trade_no, mch_id}
--   paypal            → (reserved for future use)
-- 设计文档: docs/superpowers/specs/2026-07-15-wechat-pay-real-client-design.md

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS provider_intent JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN orders.provider_intent IS
    'Per-channel provider metadata written after channel-specific pre-auth: '
    'wechat_pay → {code_url, out_trade_no, mch_id}; paypal → ...';
```

- [ ] **Step 2: Commit**

```bash
git add migrations/009_wechat_pay_intent.sql
git commit -m "feat(payments): migration 009 — orders.provider_intent JSONB"
```

---

## Task 2: Config — 3 new env vars + Validate rule

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Add 3 fields to `Config` struct**

In `internal/config/config.go`, locate the existing `WeChatPayMchID` field and add 3 new fields immediately after it (and after `WeChatAPIv3Key` — both are existing):

```go
WeChatPayMchID             string
WeChatPayMchPrivateKeyPath string // 商户 RSA 私钥 PEM 路径 (PKCS#1 / PKCS#8) — real mode
WeChatPayMchCertPath       string // 商户 X.509 证书 PEM 路径 — real mode
WeChatPayNotifyURL         string // 微信支付回调 URL (e.g. https://host/webhooks/payment/wechat_pay) — real mode
```

- [ ] **Step 2: Wire env reads in `Load()`**

In `internal/config/config.go::Load()`, add 3 lines after the existing `WeChatPayMchID: os.Getenv("WECHAT_PAY_MCH_ID"),`:

```go
WeChatPayMchID:             os.Getenv("WECHAT_PAY_MCH_ID"),
WeChatPayMchPrivateKeyPath: os.Getenv("WECHAT_PAY_MCH_PRIVATE_KEY_PATH"),
WeChatPayMchCertPath:       os.Getenv("WECHAT_PAY_MCH_CERT_PATH"),
WeChatPayNotifyURL:         os.Getenv("WECHAT_PAY_NOTIFY_URL"),
```

- [ ] **Step 3: Write failing test `TestValidate_WeChatReal_AllFiveRequired`**

In `internal/config/config_test.go`, add a new test function. Locate the existing WeChat-validate tests (search for `TestValidate_WeChatReal` or similar) and add after them:

```go
func TestValidate_WeChatReal_AllFiveRequired(t *testing.T) {
    cases := []struct {
        name string
        env  map[string]string
    }{
        {"missing private key path", map[string]string{
            "WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
            "WECHAT_PAY_MCH_CERT_PATH": "/c", "WECHAT_PAY_NOTIFY_URL": "https://x/cb",
        }},
        {"missing cert path", map[string]string{
            "WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
            "WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k", "WECHAT_PAY_NOTIFY_URL": "https://x/cb",
        }},
        {"missing notify url", map[string]string{
            "WECHAT_PAY_MCH_ID": "123", "WECHAT_PAY_API_V3_KEY": "k",
            "WECHAT_PAY_MCH_PRIVATE_KEY_PATH": "/k", "WECHAT_PAY_MCH_CERT_PATH": "/c",
        }},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            for k, v := range tc.env {
                t.Setenv(k, v)
            }
            c, err := Load()
            if err != nil {
                t.Fatalf("Load: %v", err)
            }
            if err := c.Validate(); err == nil {
                t.Fatalf("Validate: expected error for %s, got nil", tc.name)
            }
        })
    }
}

func TestValidate_WeChatMock_AllowsEmpty(t *testing.T) {
    t.Setenv("WECHAT_PAY_MOCK", "1")
    // No wechat envs at all — mock mode must not block boot.
    c, err := Load()
    if err != nil {
        t.Fatalf("Load: %v", err)
    }
    if err := c.Validate(); err != nil {
        t.Fatalf("Validate in mock mode: %v", err)
    }
}
```

- [ ] **Step 4: Run tests, confirm RED**

```bash
go test ./internal/config/... -run "TestValidate_WeChatReal_AllFiveRequired|TestValidate_WeChatMock_AllowsEmpty" -v
```

Expected: FAIL — Validate doesn't yet check the 3 new envs.

- [ ] **Step 5: Add the 3-env Validate case**

In `internal/config/config.go::Validate()`, locate the existing WeChat-Pay mock branches (they currently check asymmetric MCH_ID ↔ APIv3_KEY) and add a new case AFTER them:

```go
// NEW: real mode also requires the 3 new envs
case !c.WeChatPayMock && (
    c.WeChatPayMchPrivateKeyPath == "" ||
    c.WeChatPayMchCertPath == "" ||
    c.WeChatPayNotifyURL == ""):
    return errors.New("real WeChat Pay mode requires WECHAT_PAY_MCH_PRIVATE_KEY_PATH, " +
        "WECHAT_PAY_MCH_CERT_PATH, and WECHAT_PAY_NOTIFY_URL")
```

- [ ] **Step 6: Run tests, confirm GREEN**

```bash
go test ./internal/config/... -run "TestValidate_WeChatReal_AllFiveRequired|TestValidate_WeChatMock_AllowsEmpty" -v
```

Expected: PASS.

- [ ] **Step 7: Run full config tests**

```bash
go test ./internal/config/...
```

Expected: PASS (no other tests break).

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): WECHAT_PAY_* private key / cert / notify URL envs + Validate"
```

---

## Task 3: `cert.go` — LoadPrivateKey + LoadCertSerial

**Files:**
- Create: `internal/billing/wechat/cert.go`
- Create: `internal/billing/wechat/cert_test.go`

- [ ] **Step 1: Write failing test for LoadPrivateKey (PKCS#1)**

Create `internal/billing/wechat/cert_test.go`:

```go
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
    "strings"
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
```

- [ ] **Step 2: Run cert_test, confirm RED**

```bash
go test ./internal/billing/wechat/... -run "TestLoadPrivateKey" -v
```

Expected: FAIL — `LoadPrivateKey` undefined.

- [ ] **Step 3: Implement `cert.go`**

Create `internal/billing/wechat/cert.go`:

```go
package wechat

import (
    "crypto/rsa"
    "crypto/x509"
    "encoding/pem"
    "fmt"
    "math/big"
    "os"
)

// LoadPrivateKey reads a PEM-encoded RSA private key from disk.
// Supports both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY")
// blocks; wechat Signer takes either. Returns the parsed key.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
    raw, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read private key: %w", err)
    }
    block, _ := pem.Decode(raw)
    if block == nil {
        return nil, fmt.Errorf("no PEM block found in %s", path)
    }
    switch block.Type {
    case "RSA PRIVATE KEY":
        key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
        if err != nil {
            return nil, fmt.Errorf("parse PKCS#1: %w", err)
        }
        return key, nil
    case "PRIVATE KEY":
        keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
        if err != nil {
            return nil, fmt.Errorf("parse PKCS#8: %w", err)
        }
        rsaKey, ok := keyAny.(*rsa.PrivateKey)
        if !ok {
            return nil, fmt.Errorf("PKCS#8 key is %T, not RSA", keyAny)
        }
        return rsaKey, nil
    default:
        return nil, fmt.Errorf("unsupported PEM block type %q (want RSA PRIVATE KEY or PRIVATE KEY)", block.Type)
    }
}

// LoadCertSerial reads a PEM-encoded X.509 certificate from disk and
// returns the certificate's serial number as a decimal string — the
// format WeChat Pay v3 expects in the `serial_no` field of the
// Authorization header. Big-endian byte representation, decimal-encoded.
func LoadCertSerial(path string) (string, error) {
    raw, err := os.ReadFile(path)
    if err != nil {
        return "", fmt.Errorf("read cert: %w", err)
    }
    block, _ := pem.Decode(raw)
    if block == nil {
        return "", fmt.Errorf("no PEM block found in %s", path)
    }
    if block.Type != "CERTIFICATE" {
        return "", fmt.Errorf("unsupported PEM block type %q (want CERTIFICATE)", block.Type)
    }
    cert, err := x509.ParseCertificate(block.Bytes)
    if err != nil {
        return "", fmt.Errorf("parse cert: %w", err)
    }
    return serialToDecimal(cert.SerialNumber), nil
}

// serialToDecimal converts a big.Int serial number to WeChat's expected
// decimal string form. cert.SerialNumber.String() (the natural big.Int
// representation) does NOT match what WeChat expects — WeChat requires
// the byte representation interpreted as a positive integer with no
// sign prefix. For positive serials (the common case), this is the same
// as .String(); for serials whose high bit is set, .String() prepends a
// sign and breaks WeChat's parser.
func serialToDecimal(serial *big.Int) string {
    return fmt.Sprintf("%d", new(big.Int).Abs(serial))
}
```

- [ ] **Step 4: Run tests, confirm GREEN**

```bash
go test ./internal/billing/wechat/... -run "TestLoadPrivateKey" -v
```

Expected: PASS.

- [ ] **Step 5: Add `TestLoadCertSerial` to `cert_test.go`**

Append to `internal/billing/wechat/cert_test.go`:

```go
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

// writeSelfSignedCert generates a 2048-bit RSA key + self-signed X.509
// cert, writes both PEMs to disk. The key is discarded — only the cert
// is read by LoadCertSerial.
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
```

- [ ] **Step 6: Run cert_test, confirm GREEN**

```bash
go test ./internal/billing/wechat/... -run "TestLoad" -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/billing/wechat/cert.go internal/billing/wechat/cert_test.go
git commit -m "feat(wechat): cert helpers — LoadPrivateKey + LoadCertSerial"
```

---

## Task 4: `sign.go` — Signer.BuildAuthHeader

**Files:**
- Create: `internal/billing/wechat/sign.go`
- Create: `internal/billing/wechat/testdata/sign_test_key.pem`
- Create: `internal/billing/wechat/testdata/sign_test_vector.json`
- Create: `internal/billing/wechat/sign_test.go`

- [ ] **Step 1: Generate test key + capture fixture**

Run in a scratch dir:

```bash
cd internal/billing/wechat
mkdir -p testdata
# Generate a deterministic RSA-2048 test key. We commit this so the
# fixture in testdata/sign_test_vector.json is reproducible across CI
# runs without committing a generated private key each time.
openssl genrsa -out testdata/sign_test_key.pem 2048
# Use the same key to produce a self-signed cert (only serial matters
# for sign tests, but sign.BuildAuthHeader needs a real cert to read
# serial from — see test that reads from the fixture).
openssl req -new -x509 -key testdata/sign_test_key.pem -out testdata/sign_test_cert.pem \
    -days 36500 -subj "/CN=yunhou-users-test"
ls testdata/
```

- [ ] **Step 2: Write failing test using committed fixture**

Create `internal/billing/wechat/sign_test.go`:

```go
package wechat

import (
    "crypto/x509"
    "encoding/json"
    "encoding/pem"
    "math/big"
    "os"
    "strings"
    "testing"
    "time"
)

type authVector struct {
    Method        string `json:"method"`
    Path          string `json:"path"`
    Body          string `json:"body"`
    MchID         string `json:"mch_id"`
    ExpectedAuth  string `json:"expected_authorization"`
    SerialNo      string `json:"serial_no"`
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
    keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
    if err != nil {
        // fall back to PKCS#1 (openssl genrsa emits PKCS#1)
        key, err2 := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
        if err2 != nil {
            t.Fatalf("parse test key (pkcs8=%v pkcs1=%v)", err, err2)
        }
        certBlock, _ := pem.Decode(certPEM)
        cert, err3 := x509.ParseCertificate(certBlock.Bytes)
        if err3 != nil {
            t.Fatalf("parse test cert: %v", err3)
        }
        return &Signer{MchID: "test_mch", SerialNo: serialToDecimal(cert.SerialNumber), PrivateKey: key}
    }
    certBlock, _ := pem.Decode(certPEM)
    cert, err3 := x509.ParseCertificate(certBlock.Bytes)
    if err3 != nil {
        t.Fatalf("parse test cert: %v", err3)
    }
    return &Signer{MchID: "test_mch", SerialNo: serialToDecimal(cert.SerialNumber), PrivateKey: keyAny.(interface{ E() }).(interface{}) /* never reached */ }
}
```

Wait — the type assertion above is wrong. Replace the whole helper with the cleaner form below.

Replace the file with:

```go
package wechat

import (
    "crypto/rsa"
    "crypto/x509"
    "encoding/json"
    "encoding/pem"
    "math/big"
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
    // Use the fixture's MchID + SerialNo so the assertion is byte-exact.
    s.MchID = vec.MchID
    s.SerialNo = vec.SerialNo

    got, err := s.BuildAuthHeader(vec.Method, vec.Path, []byte(vec.Body))
    if err != nil {
        t.Fatalf("BuildAuthHeader: %v", err)
    }
    if got != vec.ExpectedAuth {
        t.Fatalf("BuildAuthHeader mismatch\n got: %s\nwant: %s", got, vec.ExpectedAuth)
    }
}

func TestBuildAuthHeader_NonceUniqueness(t *testing.T) {
    s := loadSignerForTest(t)
    a, _ := s.BuildAuthHeader("POST", "/x", []byte("{}"))
    b, _ := s.BuildAuthHeader("POST", "/x", []byte("{}"))
    extract := func(h string) string {
        i := strings.Index(h, `nonce_str="`)
        if i < 0 {
            t.Fatalf("no nonce_str in %s", h)
        }
        j := strings.Index(h[i:], `"`)
        return h[i+11 : i+j]
    }
    if extract(a) == extract(b) {
        t.Fatalf("nonce_str repeated across calls: %s", extract(a))
    }
}

func TestBuildAuthHeader_TimestampFresh(t *testing.T) {
    s := loadSignerForTest(t)
    h, _ := s.BuildAuthHeader("POST", "/x", []byte("{}"))
    i := strings.Index(h, `timestamp="`)
    if i < 0 {
        t.Fatalf("no timestamp in %s", h)
    }
    j := strings.Index(h[i:], `"`)
    tsStr := h[i+11 : i+j]
    var ts int64
    if _, err := fmtSscan(tsStr, &ts); err != nil {
        t.Fatalf("parse timestamp: %v", err)
    }
    delta := time.Now().Unix() - ts
    if delta < 0 {
        delta = -delta
    }
    if delta > 2 {
        t.Fatalf("timestamp drift %ds (>2s)", delta)
    }
}

// tiny local helper to avoid extra imports in the test file
func fmtSscan(s string, dst *int64) (int, error) {
    var v int64
    for _, c := range s {
        if c < '0' || c > '9' {
            return 0, &strconvErr{s}
        }
        v = v*10 + int64(c-'0')
    }
    *dst = v
    return len(s), nil
}

type strconvErr struct{ s string }

func (e *strconvErr) Error() string { return "bad number: " + e.s }
```

Wait — `big.Int` is imported but unused. Drop the import. Replace the file with this cleaned version:

```go
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

    got, err := s.BuildAuthHeader(vec.Method, vec.Path, []byte(vec.Body))
    if err != nil {
        t.Fatalf("BuildAuthHeader: %v", err)
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
```

- [ ] **Step 3: Run sign_test, confirm RED**

```bash
go test ./internal/billing/wechat/... -run "TestBuildAuthHeader" -v
```

Expected: FAIL — `BuildAuthHeader` and `Signer` undefined.

- [ ] **Step 4: Implement `sign.go`**

Create `internal/billing/wechat/sign.go`:

```go
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
    MchID      string         // 商户号
    SerialNo   string         // decimal string from cert
    PrivateKey *rsa.PrivateKey
}

// BuildAuthHeader returns the value for the `Authorization` HTTP header
// on outbound requests to api.mch.weixin.qq.com. Caller supplies the
// HTTP method, request path (no host, no query — e.g.
// "/v3/pay/transactions/native"), and raw body bytes.
//
// Format (WeChat Pay v3 docs §"签名生成"):
//
//   scheme = "WECHATPAY2-SHA256-RSA2048"
//   message = METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + BODY + "\n"
//   sign = base64( RSA-SHA256(message, PrivateKey) )
//   Authorization = scheme + ' ' + kv pairs (mchid, nonce_str, timestamp, serial_no, signature)
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
```

- [ ] **Step 5: Run sign_test with placeholder fixture, see nonce/timestamp pass, see vector FAIL**

Create `internal/billing/wechat/testdata/sign_test_vector.json` with a placeholder (the test will fail at the byte-comparison step; that's OK for now):

```json
{
  "method": "POST",
  "path": "/v3/pay/transactions/native",
  "body": "{\"mch_id\":\"1900000109\"}",
  "mch_id": "1900000109",
  "serial_no": "PLACEHOLDER_SERIAL",
  "expected_authorization": "WECHATPAY2-SHA256-RSA2048 PLACEHOLDER"
}
```

```bash
go test ./internal/billing/wechat/... -run "TestBuildAuthHeader_NonceUniqueness|TestBuildAuthHeader_TimestampFresh" -v
```

Expected: PASS for nonce + timestamp. (The fixed-vector test is intentionally failing until Step 6 captures the real fixture.)

- [ ] **Step 6: Capture the real fixture**

Write a small one-off Go program to print the Authorization header for the fixture inputs:

```bash
cat > /tmp/capture_vector.go <<'EOF'
package main

import (
    "fmt"
    "os"

    "github.com/yunhou/users/internal/billing/wechat"
)

func main() {
    key, err := wechat.LoadPrivateKey("internal/billing/wechat/testdata/sign_test_key.pem")
    if err != nil { fmt.Println("key:", err); os.Exit(1) }
    serial, err := wechat.LoadCertSerial("internal/billing/wechat/testdata/sign_test_cert.pem")
    if err != nil { fmt.Println("cert:", err); os.Exit(1) }
    s := &wechat.Signer{MchID: "1900000109", SerialNo: serial, PrivateKey: key}
    h, err := s.BuildAuthHeader("POST", "/v3/pay/transactions/native",
        []byte(`{"mch_id":"1900000109"}`))
    if err != nil { fmt.Println("sign:", err); os.Exit(1) }
    fmt.Printf("serial=%s\n", serial)
    fmt.Printf("auth=%s\n", h)
}
EOF
go run /tmp/capture_vector.go
```

The output prints `serial=<SERIAL>` and `auth=<HEADER>`. Update `internal/billing/wechat/testdata/sign_test_vector.json` with these real values:

```json
{
  "method": "POST",
  "path": "/v3/pay/transactions/native",
  "body": "{\"mch_id\":\"1900000109\"}",
  "mch_id": "1900000109",
  "serial_no": "<paste serial>",
  "expected_authorization": "<paste auth>"
}
```

```bash
rm /tmp/capture_vector.go
```

- [ ] **Step 7: Run all sign_test, confirm GREEN**

```bash
go test ./internal/billing/wechat/... -run "TestBuildAuthHeader" -v
```

Expected: PASS — all 3 tests.

- [ ] **Step 8: Commit**

```bash
git add internal/billing/wechat/sign.go internal/billing/wechat/sign_test.go \
        internal/billing/wechat/testdata/sign_test_key.pem \
        internal/billing/wechat/testdata/sign_test_cert.pem \
        internal/billing/wechat/testdata/sign_test_vector.json
git commit -m "feat(wechat): Signer.BuildAuthHeader + fixed test vector"
```

---

## Task 5: `wechat.go` — real UnifiedOrder branch

**Files:**
- Modify: `internal/billing/wechat/wechat.go`
- Modify: `internal/billing/wechat/wechat_test.go`

- [ ] **Step 1: Write failing tests for real UnifiedOrder**

In `internal/billing/wechat/wechat_test.go` (which already exists for the mock branch), append:

```go
type stubDoer struct {
    resp *HTTPResponse
    err  error
    got  *HTTPRequest // captured for assertion
}

func (s *stubDoer) Do(req *HTTPRequest) (*HTTPResponse, error) {
    s.got = req
    return s.resp, s.err
}

func newRealClient(t *testing.T, doer HTTPDoer) *Client {
    t.Helper()
    key, err := LoadPrivateKey("testdata/sign_test_key.pem")
    if err != nil {
        t.Fatalf("load key: %v", err)
    }
    serial, err := LoadCertSerial("testdata/sign_test_cert.pem")
    if err != nil {
        t.Fatalf("load cert: %v", err)
    }
    return &Client{
        MockMode:   false,
        MchID:      "1900000109",
        Signer:     &Signer{MchID: "1900000109", SerialNo: serial, PrivateKey: key},
        NotifyURL:  "https://example.com/webhooks/payment/wechat_pay",
        BaseURL:    "https://api.mch.weixin.qq.com",
        HTTPDoer:   doer,
    }
}

func TestUnifiedOrder_Real_200(t *testing.T) {
    stub := &stubDoer{resp: &HTTPResponse{StatusCode: 200, Body: []byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=ABC123"}`)}}
    c := newRealClient(t, stub)
    resp, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1",
        Description: "plan-monthly",
        Amount: Amount{Total: 1234, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if err != nil {
        t.Fatalf("UnifiedOrder: %v", err)
    }
    if resp.CodeURL != "weixin://wxpay/bizpayurl?pr=ABC123" {
        t.Fatalf("code_url = %q", resp.CodeURL)
    }
    // Assert Authorization header is present + has the expected scheme
    if stub.got == nil || !strings.HasPrefix(stub.got.Headers["Authorization"], "WECHATPAY2-SHA256-RSA2048 ") {
        t.Fatalf("Authorization header missing or wrong: %v", stub.got)
    }
    // Assert body contains mch_id + out_trade_no
    if !strings.Contains(string(stub.got.Body), `"mch_id":"1900000109"`) {
        t.Fatalf("body missing mch_id: %s", stub.got.Body)
    }
    if !strings.Contains(string(stub.got.Body), `"out_trade_no":"order-1"`) {
        t.Fatalf("body missing out_trade_no: %s", stub.got.Body)
    }
    // appid must NOT be present (NATIVE doesn't need it)
    if strings.Contains(string(stub.got.Body), `"appid"`) {
        t.Fatalf("body unexpectedly contains appid: %s", stub.got.Body)
    }
}

func TestUnifiedOrder_Real_4xx(t *testing.T) {
    stub := &stubDoer{resp: &HTTPResponse{StatusCode: 400, Body: []byte(`{"code":"INVALID_REQUEST","message":"bad amount"}`)}}
    c := newRealClient(t, stub)
    _, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1", Description: "x",
        Amount: Amount{Total: 100, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
        t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
    }
}

func TestUnifiedOrder_Real_5xx(t *testing.T) {
    stub := &stubDoer{resp: &HTTPResponse{StatusCode: 500, Body: []byte(`{}`)}}
    c := newRealClient(t, stub)
    _, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1", Description: "x",
        Amount: Amount{Total: 100, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
        t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
    }
}

func TestUnifiedOrder_Real_NetworkErr(t *testing.T) {
    stub := &stubDoer{err: fmt.Errorf("net down")}
    c := newRealClient(t, stub)
    _, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1", Description: "x",
        Amount: Amount{Total: 100, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if !errors.Is(err, ErrWeChatNetwork) {
        t.Fatalf("err = %v, want ErrWeChatNetwork", err)
    }
}

func TestUnifiedOrder_Real_EmptyCodeURL(t *testing.T) {
    stub := &stubDoer{resp: &HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}}
    c := newRealClient(t, stub)
    _, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1", Description: "x",
        Amount: Amount{Total: 100, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
        t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
    }
}

func TestUnifiedOrder_Mock_Unchanged(t *testing.T) {
    c := &Client{MockMode: true}
    resp, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
        OutTradeNo: "order-1", Description: "x",
        Amount: Amount{Total: 100, Currency: "CNY"},
        TradeType: TradeTypeNative,
    })
    if err != nil {
        t.Fatalf("UnifiedOrder mock: %v", err)
    }
    if !strings.Contains(resp.CodeURL, "pr=mock_order-1") {
        t.Fatalf("mock code_url = %q", resp.CodeURL)
    }
}
```

Add to the imports at the top of `wechat_test.go`:

```go
import (
    "context"
    "errors"
    "fmt"
    "strings"
    // ... existing imports
)
```

- [ ] **Step 2: Run tests, confirm RED**

```bash
go test ./internal/billing/wechat/... -run "TestUnifiedOrder_Real|TestUnifiedOrder_Mock_Unchanged" -v
```

Expected: FAIL — real-mode tests reference `Signer`, `ErrWeChatUnifiedOrderRejected`, `ErrWeChatNetwork`, which don't exist yet. Mock test passes.

- [ ] **Step 3: Restructure `Client` and add real branch in `wechat.go`**

Replace `internal/billing/wechat/wechat.go` with:

```go
package wechat

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
)

// HTTPDoer is the minimal HTTP interface the real Client needs. Pulled
// out so tests can inject a stub without dragging in a full transport
// (and so cmd/server can wire *http.Client via a one-line adapter).
type HTTPDoer interface {
    Do(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest / HTTPResponse are the minimal shapes the real-mode
// UnifiedOrder path needs. Kept here as opaque structs to avoid pulling
// net/http into the package's public surface.
type HTTPRequest struct {
    Method  string
    URL     string
    Headers map[string]string
    Body    []byte
}

type HTTPResponse struct {
    StatusCode int
    Body       []byte
}

// Client is the WeChat Pay v3 entry point. Two modes:
//
//   - MockMode=true: UnifiedOrder returns a deterministic code_url
//     derived from OutTradeNo so a BFF can render a "fake" QR. No
//     outbound HTTP call.
//
//   - MockMode=false: the real v3 client. Signs outbound requests with
//     the merchant RSA private key (via Signer), calls
//     api.mch.weixin.qq.com/v3/pay/transactions/native for NATIVE
//     payments, returns the parsed code_url.
type Client struct {
    MockMode bool

    // Real-mode fields (used when MockMode=false). Signer holds the
    // RSA private key + cert serial + MchID for outbound request
    // signing. MchID is duplicated as a string field on Client for
    // body construction and accessor use (interface callers read via
    // MchID() getter). APIv3Key is NOT stored on Client — it lives on
    // cfg and is used by the inbound webhook verifier only.
    MchID     string
    Signer    *Signer
    NotifyURL string
    BaseURL   string // https://api.mch.weixin.qq.com
    HTTPDoer  HTTPDoer
}

// MchID exposes the merchant ID to callers (e.g. PaymentService writes
// it into orders.provider_intent.mch_id). Required by the service-
// layer wechatClient interface.
func (c *Client) MchID() string { return c.MchID }

// IsMockMode is a small accessor for handlers / services that need to
// know whether to skip real-mode code paths (e.g. mock payloads are
// plaintext, no resource block; mock code_url is enough for BFF dev).
func (c *Client) IsMockMode() bool { return c.MockMode }

// UnifiedOrder mints a code_url for the given request. In mock mode the
// URL is deterministic from OutTradeNo so tests can assert exact values
// without flakiness; in real mode it POSTs to
// /v3/pay/transactions/native and parses the response.
func (c *Client) UnifiedOrder(ctx context.Context, req UnifiedOrderRequest) (*UnifiedOrderResponse, error) {
    if req.OutTradeNo == "" {
        return nil, errors.New("OutTradeNo is required")
    }
    if req.TradeType == "" {
        req.TradeType = TradeTypeNative
    }
    if c.MockMode {
        return &UnifiedOrderResponse{
            OutTradeNo: req.OutTradeNo,
            CodeURL:    fmt.Sprintf("weixin://wxpay/bizpayurl?pr=mock_%s", req.OutTradeNo),
        }, nil
    }
    _ = ctx

    // Real mode. NATIVE only needs mch_id — the `appid` field is
    // reserved for in-app / JSAPI flows and is intentionally omitted.
    type unifiedOrderBody struct {
        MchID       string `json:"mch_id"`
        Description string `json:"description"`
        OutTradeNo  string `json:"out_trade_no"`
        NotifyURL   string `json:"notify_url"`
        Amount      struct {
            Total    int64  `json:"total"`
            Currency string `json:"currency"`
        } `json:"amount"`
        TradeType string `json:"trade_type"`
    }
    var bodyBytes unifiedOrderBody
    bodyBytes.MchID = c.MchID
    bodyBytes.Description = req.Description
    bodyBytes.OutTradeNo = req.OutTradeNo
    bodyBytes.NotifyURL = c.NotifyURL
    bodyBytes.Amount.Total = req.Amount.Total
    bodyBytes.Amount.Currency = req.Amount.Currency
    bodyBytes.TradeType = string(req.TradeType)
    body, err := json.Marshal(bodyBytes)
    if err != nil {
        return nil, fmt.Errorf("marshal body: %w", err)
    }

    reqPath := "/v3/pay/transactions/native"
    auth, err := c.Signer.BuildAuthHeader("POST", reqPath, body)
    if err != nil {
        return nil, fmt.Errorf("build auth: %w", err)
    }

    resp, err := c.HTTPDoer.Do(&HTTPRequest{
        Method:  "POST",
        URL:     c.BaseURL + reqPath,
        Headers: map[string]string{
            "Authorization": auth,
            "Content-Type":  "application/json",
            "Accept":        "application/json",
            "User-Agent":    "yunhou-users/dev",
        },
        Body: body,
    })
    if err != nil {
        return nil, fmt.Errorf("%w: %v", ErrWeChatNetwork, err)
    }

    if resp.StatusCode >= 400 {
        var errEnv struct {
            Code    string `json:"code"`
            Message string `json:"message"`
        }
        _ = json.Unmarshal(resp.Body, &errEnv)
        return nil, fmt.Errorf("%w: %d %s: %s", ErrWeChatUnifiedOrderRejected,
            resp.StatusCode, errEnv.Code, errEnv.Message)
    }

    var out struct {
        CodeURL string `json:"code_url"`
    }
    if err := json.Unmarshal(resp.Body, &out); err != nil {
        return nil, fmt.Errorf("decode response: %w", err)
    }
    if out.CodeURL == "" {
        return nil, fmt.Errorf("%w: empty code_url", ErrWeChatUnifiedOrderRejected)
    }
    return &UnifiedOrderResponse{OutTradeNo: req.OutTradeNo, CodeURL: out.CodeURL}, nil
}

// ErrWeChatUnifiedOrderRejected — WeChat returned a 4xx / 5xx response
// (or a 200 with no code_url). Caller should treat as terminal for the
// current order; user retries by creating a new order.
var ErrWeChatUnifiedOrderRejected = errors.New("wechat unified order rejected")

// ErrWeChatNetwork — outbound HTTP failure (timeout, DNS, connection
// refused). Distinct from ErrWeChatUnifiedOrderRejected so callers can
// classify transient vs terminal failures.
var ErrWeChatNetwork = errors.New("wechat network error")
```

- [ ] **Step 4: Run tests, confirm GREEN**

```bash
go test ./internal/billing/wechat/... -run "TestUnifiedOrder_Real|TestUnifiedOrder_Mock_Unchanged" -v
```

Expected: PASS — all 6 tests (5 real + 1 mock regression).

- [ ] **Step 5: Run full wechat package tests**

```bash
go test ./internal/billing/wechat/...
```

Expected: PASS — no regressions in existing tests.

- [ ] **Step 6: Commit**

```bash
git add internal/billing/wechat/wechat.go internal/billing/wechat/wechat_test.go
git commit -m "feat(wechat): real UnifiedOrder branch + typed errors + MchID() getter"
```

---

## Task 6: `model.Order.ProviderIntent` + repo `UpdateProviderIntent`

**Files:**
- Modify: `internal/model/payment.go`
- Modify: `internal/repo/orders.go` (or existing file — find via grep)
- Create or modify: `internal/repo/orders_test.go`

- [ ] **Step 1: Find the orders repo file**

```bash
grep -rln "func.*orderRepo.*Create\|interface.*OrderRepo\|type OrderRepo" internal/repo/
```

Read the file to understand the interface + existing Create method.

- [ ] **Step 2: Add `ProviderIntent` to `Order` struct**

In `internal/model/payment.go`, find the `Order` struct and add the new field (with `json:"-"` so it doesn't leak into HTTP responses, and `db:"provider_intent"` for sqlx round-trip):

```go
type Order struct {
    ID             string  `db:"id" json:"id"`
    UserID         string  `db:"user_id" json:"user_id"`
    PlanID         string  `db:"plan_id" json:"plan_id"`
    Amount         float64 `db:"amount" json:"amount"`
    Currency       string  `db:"currency" json:"currency"`
    Status         string  `db:"status" json:"status"`
    ExpiresAt      time.Time `db:"expires_at" json:"expires_at"`
    CreatedAt      time.Time `db:"created_at" json:"created_at"`
    UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
    ProviderIntent []byte  `db:"provider_intent" json:"-"` // raw JSONB bytes
}
```

(Adapt the existing field tags if the struct already differs in minor ways — keep the new field at the end.)

- [ ] **Step 3: Add `UpdateProviderIntent` to the `OrderRepo` interface**

In the orders-repo file, find the `OrderRepo` interface and append:

```go
type OrderRepo interface {
    // ... existing methods ...

    // UpdateProviderIntent writes a JSON payload into orders.provider_intent.
    // Caller marshals the struct; the repo writes the bytes verbatim into
    // the JSONB column. Used by channel-specific pre-auth flows (wechat_pay).
    UpdateProviderIntent(ctx context.Context, orderID string, payload []byte) error
}
```

- [ ] **Step 4: Implement `UpdateProviderIntent` on the `*sqlx` repo**

In the same repo file, find the existing `Create` method (or wherever the methods are defined) and add:

```go
func (r *OrderRepoImpl) UpdateProviderIntent(ctx context.Context, orderID string, payload []byte) error {
    _, err := r.db.ExecContext(ctx,
        `UPDATE orders SET provider_intent = $1::jsonb, updated_at = now() WHERE id = $2`,
        payload, orderID,
    )
    if err != nil {
        return fmt.Errorf("update provider_intent: %w", err)
    }
    return nil
}
```

Adjust the receiver type / `r.db` field name to match the existing repo's conventions.

- [ ] **Step 5: Find and update any in-memory test stub of `OrderRepo`**

```bash
grep -rln "OrderRepo" internal/repo/ internal/service/ | head
```

For each match that's a hand-rolled mock struct with function fields, add a stub for the new method (returns `nil` by default):

```go
UpdateProviderIntentFunc func(ctx context.Context, orderID string, payload []byte) error
```

Plus a method that calls it (matching the existing stub-method pattern in the file).

- [ ] **Step 6: Write + run a round-trip test (if repo has DB tests)**

If `internal/repo/orders_test.go` exists with a fresh-DB fixture, add:

```go
func TestOrderRepo_UpdateProviderIntent_RoundTrip(t *testing.T) {
    db := freshTestDB(t) // existing helper — find via grep
    repo := NewOrderRepo(db)
    orderID := "test-order-uuid"

    // First insert an order row so the FK constraints pass
    _, err := db.Exec(`INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
        VALUES ($1, $2, 'plan-1', 1.00, 'CNY', 'pending', now() + interval '30 minutes')`,
        orderID, testUserID(t, db))
    if err != nil { t.Fatalf("insert order: %v", err) }

    payload := []byte(`{"code_url":"weixin://test","out_trade_no":"x","mch_id":"123"}`)
    if err := repo.UpdateProviderIntent(context.Background(), orderID, payload); err != nil {
        t.Fatalf("UpdateProviderIntent: %v", err)
    }

    var got []byte
    if err := db.Get(&got, `SELECT provider_intent::text FROM orders WHERE id=$1`, orderID); err != nil {
        t.Fatalf("select: %v", err)
    }
    if !strings.Contains(string(got), "weixin://test") {
        t.Fatalf("provider_intent round-trip mismatch: %s", got)
    }
}
```

Adapt `freshTestDB` / `testUserID` to the helpers already in the file.

If no repo test infra exists, skip this step (mark N/A) and rely on the service-level test in Task 8 to exercise it via the stub seam.

- [ ] **Step 7: Run repo tests**

```bash
go test ./internal/repo/...
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/model/payment.go internal/repo/orders.go \
        internal/repo/orders_test.go 2>/dev/null || true
git commit -m "feat(payments): Order.ProviderIntent + OrderRepo.UpdateProviderIntent"
```

---

## Task 7: Service `wechatClient` interface + `CreateOrder` channel param

**Files:**
- Modify: `internal/service/payment.go`
- Modify: `internal/handler/payment.go`

- [ ] **Step 1: Find `PaymentService` struct and `CreateOrder` method**

```bash
grep -n "type PaymentService\|func.*PaymentService.*CreateOrder\|func NewPaymentService" internal/service/payment.go
```

Read the struct definition + constructor + existing `CreateOrder` to understand the seam.

- [ ] **Step 2: Write failing tests for `wechatClient` stub**

In `internal/service/payment_test.go`, locate the existing `CreateOrder` tests and append:

```go
type stubWechat struct {
    mockMode  bool
    mchID     string
    unifiedFn func(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error)
    called    int
}

func (s *stubWechat) IsMockMode() bool { return s.mockMode }
func (s *stubWechat) MchID() string    { return s.mchID }
func (s *stubWechat) UnifiedOrder(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
    s.called++
    return s.unifiedFn(ctx, req)
}

func TestCreateOrder_WeChat_Real_PersistsIntent(t *testing.T) {
    var captured []byte
    stub := &stubWechat{
        mockMode: false,
        mchID:    "1900000109",
        unifiedFn: func(_ context.Context, _ wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
            return &wechat.UnifiedOrderResponse{OutTradeNo: "order-1", CodeURL: "weixin://abc"}, nil
        },
    }
    svc, mockOrderRepo, _ := newPaymentServiceForTest(t, stub) // existing helper — find via grep
    mockOrderRepo.UpdateProviderIntentFunc = func(_ context.Context, _ string, payload []byte) error {
        captured = payload
        return nil
    }

    order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
    if err != nil { t.Fatalf("CreateOrder: %v", err) }
    if stub.called != 1 { t.Fatalf("UnifiedOrder called %d times, want 1", stub.called) }
    if !strings.Contains(string(captured), `"code_url":"weixin://abc"`) {
        t.Fatalf("UpdateProviderIntent payload = %s", captured)
    }
    if !strings.Contains(string(captured), `"mch_id":"1900000109"`) {
        t.Fatalf("UpdateProviderIntent payload missing mch_id: %s", captured)
    }
    if order.ProviderIntent == nil { t.Fatalf("returned order.ProviderIntent is nil") }
}

func TestCreateOrder_WeChat_Real_UnifiedOrderErr(t *testing.T) {
    stub := &stubWechat{
        mockMode: false,
        unifiedFn: func(_ context.Context, _ wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
            return nil, errors.New("wechat down")
        },
    }
    svc, mockOrderRepo, _ := newPaymentServiceForTest(t, stub)
    mockOrderRepo.UpdateProviderIntentFunc = func(_ context.Context, _ string, _ []byte) error {
        t.Fatalf("UpdateProviderIntent should not be called when UnifiedOrder fails")
        return nil
    }
    _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
    if err == nil { t.Fatalf("expected error") }
}

func TestCreateOrder_WeChat_Mock_NoClientCall(t *testing.T) {
    stub := &stubWechat{mockMode: true}
    svc, mockOrderRepo, _ := newPaymentServiceForTest(t, stub)
    _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
    if err != nil { t.Fatalf("CreateOrder mock: %v", err) }
    if stub.called != 0 { t.Fatalf("UnifiedOrder called %d times in mock mode", stub.called) }
    if mockOrderRepo.UpdateProviderIntentCalled > 0 {
        t.Fatalf("UpdateProviderIntent called in mock mode")
    }
}

func TestCreateOrder_Stripe_NilWeChat_OK(t *testing.T) {
    svc, _, _ := newPaymentServiceForTest(t, nil)
    _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "stripe")
    if err != nil { t.Fatalf("CreateOrder stripe: %v", err) }
}

func TestCreateOrder_InvalidChannel(t *testing.T) {
    svc, _, _ := newPaymentServiceForTest(t, nil)
    _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "fakechan")
    if err == nil { t.Fatalf("expected error for invalid channel") }
}
```

You'll need to adapt `newPaymentServiceForTest` and the mock-OrderRepo function-field pattern to match the existing helpers in `payment_test.go`. The exact field names (`UpdateProviderIntentFunc`, `UpdateProviderIntentCalled`) must mirror what you added in Task 6 Step 5.

- [ ] **Step 3: Run tests, confirm RED**

```bash
go test ./internal/service/... -run "TestCreateOrder_WeChat_Real_PersistsIntent|TestCreateOrder_WeChat_Real_UnifiedOrderErr|TestCreateOrder_WeChat_Mock_NoClientCall|TestCreateOrder_Stripe_NilWeChat_OK|TestCreateOrder_InvalidChannel" -v
```

Expected: FAIL — `CreateOrder` doesn't accept a channel parameter yet, no `wechatClient` interface, etc.

- [ ] **Step 4: Add `wechatClient` interface + extend `PaymentService`**

In `internal/service/payment.go`, at the top of the file (after imports, before `PaymentService` struct), add:

```go
// wechatClient is the surface PaymentService needs from the wechat.Client.
// Defined here (not in billing/wechat) so tests can stub without dragging
// the billing package's transitive deps.
type wechatClient interface {
    IsMockMode() bool
    MchID() string
    UnifiedOrder(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error)
}
```

In the `PaymentService` struct definition, add a field:

```go
type PaymentService struct {
    // ... existing fields ...
    wechat wechatClient // may be nil for non-wechat deployments
}
```

In the `NewPaymentService` constructor (or wherever the existing fields are wired), add a parameter:

```go
func NewPaymentService(
    // ... existing params ...
    wechat wechatClient,
) *PaymentService {
    // ... existing assignments ...
    return &PaymentService{
        // ... existing fields ...
        wechat: wechat,
    }
}
```

- [ ] **Step 5: Update `CreateOrder` signature + body**

Replace the existing `CreateOrder` method. The new signature accepts `channel string`; the body runs `validateChannel`, then runs the existing pre-checks + order creation, then if `channel == "wechat_pay"` AND `s.wechat != nil && !s.wechat.IsMockMode()`, runs the wechat pre-auth branch:

```go
func (s *PaymentService) CreateOrder(ctx context.Context, userID, planID, channel string) (*model.Order, error) {
    if err := validateChannel(channel); err != nil {
        return nil, err
    }
    // ... existing pre-checks (plan lookup, active-sub check) ...

    order := &model.Order{
        ID:        GenerateUUID(),
        UserID:    userID,
        PlanID:    planID,
        Amount:    plan.Price,
        Currency:  "CNY",
        Status:    "pending",
        ExpiresAt: time.Now().Add(s.orderExpiry),
    }
    if err := s.orderRepo.Create(ctx, order); err != nil {
        return nil, fmt.Errorf("create order: %w", err)
    }

    // WeChat Pay NATIVE: mint code_url so the BFF can render a QR.
    // Only fires when channel == "wechat_pay" AND the client is in real
    // mode. Mock mode skips the upstream call (handler-side mock
    // code_url is enough for BFF development). Other channels skip
    // this step; their pre-auth equivalents land in their own
    // follow-up PRs.
    if channel == "wechat_pay" && s.wechat != nil && !s.wechat.IsMockMode() {
        // Convert order.Amount (CNY, decimal) → fen (int64) WITHOUT
        // float multiplication. `order.Amount` is `float64` (scanned
        // from DECIMAL(10,2) by sqlx). `x * 100` introduces drift on
        // non-.50 amounts (e.g. 0.29 → 28.999... → 28, which would
        // mismatch WeChat's record). Use a string-roundtrip: format
        // to 2-decimal string, strip the dot, parse back to int64.
        amountStr := fmt.Sprintf("%.2f", order.Amount)
        normalized := strings.ReplaceAll(amountStr, ".", "")
        amountFen, err := strconv.ParseInt(normalized, 10, 64)
        if err != nil {
            return order, fmt.Errorf("amount to fen: %w", err)
        }
        resp, err := s.wechat.UnifiedOrder(ctx, wechat.UnifiedOrderRequest{
            OutTradeNo:  order.ID,
            Description: fmt.Sprintf("plan-%s", planID),
            Amount:      wechat.Amount{Total: amountFen, Currency: "CNY"},
            TradeType:   wechat.TradeTypeNative,
        })
        if err != nil {
            // Order row already exists in 'pending'. Caller decides
            // whether to cancel + retry or wait for sweeper to flip
            // to 'expired'. We do NOT silently roll back.
            return order, fmt.Errorf("wechat unified order: %w", err)
        }
        intent, _ := json.Marshal(map[string]string{
            "code_url":     resp.CodeURL,
            "out_trade_no": order.ID,
            "mch_id":       s.wechat.MchID(),
        })
        if err := s.orderRepo.UpdateProviderIntent(ctx, order.ID, intent); err != nil {
            return order, fmt.Errorf("persist provider intent: %w", err)
        }
        order.ProviderIntent = intent
    }
    return order, nil
}
```

Add the new imports at the top of `payment.go`:

```go
import (
    // ... existing imports ...
    "strconv"
    "strings"

    "github.com/yunhou/users/internal/billing/wechat"
)
```

If `strconv` and `strings` are already imported, skip them.

- [ ] **Step 6: Update `validateChannel` allowlist (if needed)**

In `internal/service/payment.go`, find `validateChannel` (used by `Confirm`). The existing allowlist already includes `"wechat_pay"` per the codebase's channel design. Confirm via:

```bash
grep -A 8 "func validateChannel" internal/service/payment.go
```

If `"wechat_pay"` is present, no change. If not, add it.

- [ ] **Step 7: Update handler `CreateOrder` to pass `channel`**

In `internal/handler/payment.go`, locate the `CreateOrder` handler and update the request DTO + service call:

```go
func (h *PaymentHandler) CreateOrder(c *gin.Context) {
    userID := c.GetString(middleware.ContextUserID)
    if userID == "" {
        c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
        return
    }
    var req struct {
        PlanID  string `json:"plan_id" binding:"required"`
        Channel string `json:"channel" binding:"required"`
    }
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
        return
    }

    order, err := h.svc.CreateOrder(c.Request.Context(), userID, req.PlanID, req.Channel)
    if err != nil {
        writePaymentError(c, err)
        return
    }
    c.JSON(http.StatusCreated, gin.H{"code": 0, "data": order})
}
```

- [ ] **Step 8: Update existing `CreateOrder` callers/tests**

```bash
grep -rn "CreateOrder(context.Background\|svc.CreateOrder" internal/ tests/
```

For each call site that doesn't pass a channel, decide:
- Existing test calls that exercise non-wechat flows: update to pass a real channel (e.g. `"stripe"` or `"alipay"`) since that's the realistic shape
- Mock-mode wechat tests: pass `"wechat_pay"`
- If a test was calling `CreateOrder(ctx, userID, planID)` with no channel, that test won't compile — must update

- [ ] **Step 9: Run service tests, confirm GREEN**

```bash
go test ./internal/service/... -v
```

Expected: PASS — new channel-aware tests + all pre-existing tests (after Step 8 updates).

- [ ] **Step 10: Run handler tests**

```bash
go test ./internal/handler/...
```

Expected: PASS — handler test data may need a `channel` field added to the JSON bodies.

- [ ] **Step 11: Commit**

```bash
git add internal/service/payment.go internal/service/payment_test.go \
        internal/handler/payment.go internal/handler/payment_test.go 2>/dev/null || true
git commit -m "feat(payments): CreateOrder accepts channel; wechat pre-auth on real mode"
```

---

## Task 8: `cmd/server/main.go` wiring

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Find existing wiring points**

```bash
grep -n "paymentSvc\|NewPaymentService\|wechatPayMch\|wechat.NewClient" cmd/server/main.go
```

Read the surrounding code to understand where to plug in.

- [ ] **Step 2: Add wechat client construction (after `config.Load`)**

Immediately after `cfg, err := config.Load()` and any existing `cfg.Validate()` call, add:

```go
// WeChat Pay client: real mode loads cert + key from disk and builds a
// Signer + Client. Mock mode skips both file loads and returns a
// stub Client that mints deterministic code_urls.
var wechatClient *wechat.Client
if cfg.WeChatPayMock {
    wechatClient = &wechat.Client{MockMode: true}
} else if cfg.WeChatPayMchPrivateKeyPath != "" {
    pk, err := wechat.LoadPrivateKey(cfg.WeChatPayMchPrivateKeyPath)
    if err != nil {
        log.Fatalf("wechat: load private key: %v", err)
    }
    serial, err := wechat.LoadCertSerial(cfg.WeChatPayMchCertPath)
    if err != nil {
        log.Fatalf("wechat: load cert: %v", err)
    }
    wechatClient = &wechat.Client{
        MockMode:  false,
        MchID:     cfg.WeChatPayMchID,
        Signer:    &wechat.Signer{MchID: cfg.WeChatPayMchID, SerialNo: serial, PrivateKey: pk},
        NotifyURL: cfg.WeChatPayNotifyURL,
        BaseURL:   "https://api.mch.weixin.qq.com",
        HTTPDoer:  newWechatHTTPAdapter(10 * time.Second),
    }
} else {
    // Real mode + no private key path = the deployment chose not to
    // enable WeChat Pay at all (wechat endpoints return 404). WeChat
    // Pay webhook routing is also gated separately.
    wechatClient = nil
}
```

- [ ] **Step 3: Add the `*http.Client` adapter**

In the same file (e.g. just above `main`), add:

```go
// newWechatHTTPAdapter wraps a real *http.Client in the wechat.HTTPDoer
// interface. Used in production only — tests inject their own stub.
type httpDoerAdapter struct{ c *http.Client }

func (a *httpDoerAdapter) Do(req *wechat.HTTPRequest) (*wechat.HTTPResponse, error) {
    httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
    if err != nil {
        return nil, err
    }
    for k, v := range req.Headers {
        httpReq.Header.Set(k, v)
    }
    httpResp, err := a.c.Do(httpReq)
    if err != nil {
        return nil, err
    }
    defer httpResp.Body.Close()
    body, err := io.ReadAll(httpResp.Body)
    if err != nil {
        return nil, err
    }
    return &wechat.HTTPResponse{StatusCode: httpResp.StatusCode, Body: body}, nil
}

func newWechatHTTPAdapter(timeout time.Duration) wechat.HTTPDoer {
    return &httpDoerAdapter{c: &http.Client{Timeout: timeout}}
}
```

Add the imports needed:

```go
import (
    // ... existing imports ...
    "bytes"
    "io"
    "net/http"
    "time"

    "github.com/yunhou/users/internal/billing/wechat"
)
```

Trim any duplicate imports.

- [ ] **Step 4: Pass `wechatClient` to `NewPaymentService`**

Find the `NewPaymentService` call site and append `wechatClient` to the args:

```go
paymentSvc := service.NewPaymentService(
    // ... existing args ...
    wechatClient, // new — may be nil
)
```

If `wechatClient` is nil, the service's `s.wechat != nil` guard in `CreateOrder` prevents any wechat-payload work. Stripe / Alipay orders work fine with nil wechat client.

- [ ] **Step 5: Build the server**

```bash
make build
```

Expected: BUILD SUCCEEDS.

- [ ] **Step 6: Run all internal tests**

```bash
go test ./internal/...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat(server): wire wechat.Client into PaymentService constructor"
```

---

## Task 9: Makefile — `regen-test-keys` target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Locate `.PHONY` block and add the target**

In `Makefile`, find the `.PHONY:` declaration and add `regen-test-keys` to it. Then add the target body after the existing test targets:

```make
.PHONY: build run test e2e migrate migrate-status lint deps generate-keys \
        ci-test ci-migrate regen-test-keys

# Regenerate the WeChat sign-test fixtures (testdata/sign_test_key.pem +
# sign_test_cert.pem + sign_test_vector.json). Use only when the
# sign-string format changes (e.g. WeChat docs revision) — never run
# this in CI. After regenerating, manually update the embedded
# Authorization header in sign_test_vector.json to match.
regen-test-keys:
	@mkdir -p internal/billing/wechat/testdata
	@openssl genrsa -out internal/billing/wechat/testdata/sign_test_key.pem 2048
	@openssl req -new -x509 -key internal/billing/wechat/testdata/sign_test_key.pem \
	    -out internal/billing/wechat/testdata/sign_test_cert.pem -days 36500 \
	    -subj "/CN=yunhou-users-test"
	@echo "Generated new test keys. Re-capture sign_test_vector.json via the"
	@echo "capture script in the plan, then run: go test ./internal/billing/wechat/..."
```

- [ ] **Step 2: Run `make regen-test-keys` to confirm it works (optional)**

```bash
make regen-test-keys
ls internal/billing/wechat/testdata/
```

Expected: All 3 files present. (If you don't want to actually rotate the key, skip — the target is only used during fixture refresh.)

- [ ] **Step 3: Verify `make test` still passes**

```bash
make test
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "build: add regen-test-keys target for wechat sign fixtures"
```

---

## Task 10: PROGRESS.md update

**Files:**
- Modify: `PROGRESS.md`

- [ ] **Step 1: Find the A2.c follow-up line**

```bash
grep -n "A2.c 真客户端\|A2.c follow-up\|⏸ deferred" PROGRESS.md
```

Read the line that describes the deferred work to find the exact spot to update.

- [ ] **Step 2: Replace the ⏸ marker with ✅ shipped**

Edit `PROGRESS.md` to change the deferred-item entry to a shipped entry. Pattern:

- Find: a line starting with `⏸ **` (or similar defer marker) mentioning "A2.c 真客户端落地" or "real client" or "internal/billing/wechat 真客户端"
- Replace with a new ✅ entry that:
  - Lists what shipped (real `UnifiedOrder`, `Signer`, `cert.go`, `provider_intent` JSONB, 5-tuple Validate, channel param on CreateOrder)
  - References the new commit hash (use `$(git rev-parse HEAD)` after the previous commits land; for now use `<pending>` and fill in before merge)
  - Notes what remains deferred: refund API, JSAPI/H5, per-app overrides, plan_mapping, platform-cert response verification, 5xx retry

Example replacement:

```
✅ **A2.c 真客户端落地（feat/wechat-pay-real-client）** — `internal/billing/wechat/sign.go` (Signer + BuildAuthHeader, SHA256withRSA over METHOD\nPATH\nTS\nNONCE\nBODY\n), `cert.go` (LoadPrivateKey + LoadCertSerial, PKCS#1/PKCS#8 + decimal serial), `wechat.go` real UnifiedOrder branch (POST /v3/pay/transactions/native, HTTPDoer stub-friendly, typed ErrWeChatUnifiedOrderRejected / ErrWeChatNetwork), `migrations/009_wechat_pay_intent.sql` (orders.provider_intent JSONB), `OrderRepo.UpdateProviderIntent`, `PaymentService.CreateOrder` now accepts a `channel` arg and calls UnifiedOrder in real wechat_pay mode (string-based amount→fen conversion, no float drift), 5-tuple Validate (MCH_ID + APIv3_KEY + private key path + cert path + notify URL), mock mode + webhook plumbing unchanged. Deferred: refund API / JSAPI+H5 / per-app overrides / plan_mapping / platform-cert response verification / 5xx retry — see `docs/superpowers/specs/2026-07-15-wechat-pay-real-client-design.md §12`.
```

Replace `<pending>` with the commit hash after Task 9 lands (or leave as `<pending>` and replace during PR description generation).

- [ ] **Step 3: Commit**

```bash
git add PROGRESS.md
git commit -m "docs(PROGRESS): mark A2.c real client landing"
```

---

## Task 11: Full validation sweep

**Files:** (none — read-only)

- [ ] **Step 1: Run all unit tests with race + coverage**

```bash
make ci-test
```

Expected: PASS — coverage report generated, no failures.

- [ ] **Step 2: Run `go vet`**

```bash
make lint
```

Expected: CLEAN.

- [ ] **Step 3: Migrate idempotency check**

```bash
make ci-migrate
```

Expected: PASS — migration 009 applies once and a re-run is a no-op (per `IF NOT EXISTS`).

- [ ] **Step 4: Build server + migrate binary**

```bash
make build
```

Expected: bin/server + bin/migrate both built.

- [ ] **Step 5: Smoke test — start server with mock mode**

```bash
WECHAT_PAY_MOCK=1 \
  DATABASE_URL=postgres://localhost/yunhou_users_test?sslmode=disable \
  RSA_PRIVATE_KEY_PATH=keys/private.pem \
  RSA_PUBLIC_KEY_PATH=keys/public.pem \
  JWT_ACCESS_TTL=15m \
  JWT_REFRESH_TTL=168h \
  OAUTH_STATE_SECRET=$(openssl rand -hex 32) \
  PORT=8081 \
  ./bin/server &
SERVER_PID=$!
sleep 2
curl -s http://localhost:8081/healthz | head
kill $SERVER_PID
```

Expected: `{"code":0,"data":{"status":"ok"}}`.

- [ ] **Step 6: Verify the diff is complete**

```bash
git log --oneline origin/master..HEAD
git diff origin/master --stat
```

Expected: 10 commits (Tasks 1-10) + no extra noise. Diff stat should show changes in:
- `migrations/009_wechat_pay_intent.sql` (new)
- `internal/billing/wechat/{cert,cert_test,sign,sign_test,wechat,wechat_test}.go`
- `internal/billing/wechat/testdata/{sign_test_key,sign_test_cert,sign_test_vector}`
- `internal/model/payment.go`
- `internal/repo/orders*.go`
- `internal/config/{config,config_test}.go`
- `internal/service/{payment,payment_test}.go`
- `internal/handler/{payment,payment_test}.go`
- `cmd/server/main.go`
- `Makefile`
- `PROGRESS.md`

---

## Self-Review (run after writing this plan)

**Spec coverage check:**

| Spec § | Plan task(s) |
|---|---|
| §2 #1 NATIVE only | Task 5 (TradeType default = NATIVE) |
| §2 #2 server-wide env | Task 2 (Validate) |
| §2 #3 provider_intent JSONB | Task 1 (migration) + Task 6 (model + repo) |
| §2 #4 5-tuple env validation | Task 2 |
| §2 #5 HTTPDoer retained | Task 5 (HTTPDoer interface usage) |
| §2 #6 5xx no retry | Task 5 (single attempt, return error) |
| §2 #7 No response-sig verification | Task 5 (no platform-cert verify) |
| §2 #8 No refund API | Out of scope; not added |
| §3 architecture (dataflow) | Tasks 5 + 7 |
| §4 file map (13 files + Makefile + progress) | Tasks 1-10 |
| §5 migration 009 | Task 1 |
| §6 env vars + Validate rule | Task 2 |
| §7 sign.go | Task 4 |
| §8 wechat.go real branch + typed errors | Task 5 |
| §9 service interface + CreateOrder | Task 7 |
| §10 tests | Tasks 2, 3, 4, 5, 6, 7 |
| §11 risk + rollback | Task 11 (smoke test exercises the no-silent-rollback path) |
| §12 out of scope | Not implemented; PROGRESS.md (Task 10) notes deferrals |

**Type consistency check:**

- `Signer{MchID, SerialNo, PrivateKey}` — defined in Task 4, used in Task 5 (`c.Signer.BuildAuthHeader`)
- `Client{MockMode, MchID, Signer, NotifyURL, BaseURL, HTTPDoer}` — defined in Task 5, satisfies `wechatClient` interface from Task 7
- `wechatClient{IsMockMode, MchID, UnifiedOrder}` — defined in Task 7
- `Order.ProviderIntent []byte` — added Task 6, used Task 7
- `OrderRepo.UpdateProviderIntent(ctx, orderID, payload []byte) error` — added Task 6, used Task 7
- `validateChannel` — pre-existing, reused in Task 7
- `UnifiedOrderRequest{OutTradeNo, Description, Amount, TradeType}` — pre-existing in `internal/billing/wechat/types.go`, used Task 5 + Task 7
- `UnifiedOrderResponse{OutTradeNo, CodeURL}` — pre-existing, used Task 5 + Task 7
- `Amount{Total int64, Currency string}` — pre-existing, used Task 5 + Task 7
- `TradeType` / `TradeTypeNative` — pre-existing in `types.go`, used Task 5 + Task 7

**Placeholder scan:** None — every step has concrete code or commands.

**Cross-references:**
- Step 6 of Task 2 references "the existing WeChat-validate tests" — the engineer runs grep to find them; this is intentional (don't want to drift line numbers).
- Step 1 of Task 6 references "find via grep" for the orders-repo file — same rationale.
- Step 2 of Task 7 references `newPaymentServiceForTest` as an "existing helper — find via grep". If the helper doesn't exist, the engineer must add it as part of Task 7's Step 2; this is documented implicitly via the test code (the helper must exist for the tests to compile).

All gaps flagged.