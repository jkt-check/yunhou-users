# WeChat Pay v3 Real Client — Design Spec

**Date:** 2026-07-15
**Status:** Approved (brainstorming)
**Branch:** `feat/wechat-pay-real-client` (from `origin/master`)
**Author:** Claude (Yunhou Users)

## 1. Goal

Replace `internal/billing/wechat.Client.UnifiedOrder` mock-only branch with a real WeChat Pay v3 NATIVE client (HMAC-SHA256-with-RSA2048 signing, outbound to `api.mch.weixin.qq.com/v3/pay/transactions/native`). Wire `service.PaymentService.CreateOrder` to call it and persist the returned `code_url` into a new `orders.provider_intent` JSONB column.

This completes the **deferred half** of `feat/wechat-pay-mock` (A2.b) + `feat/wechat-prod-credential-prep` (A2.c) — see `PROGRESS.md` lines 19–20, 448, 463–464, 483.

The webhook-side plumbing (signature verification, AES-GCM resource decryption, mock/real envelope dispatch in `parseWeChat`) is already shipping and **stays unchanged**. This spec only covers the **outbound** client and the order-creation wiring.

## 2. Scope (decisions locked)

| # | Decision | Rationale |
|---|---|---|
| 1 | **Channel scope:** NATIVE only (`wechat://wxpay/bizpayurl?pr=...`) | Matches v1; H5/JSAPI TradeType fields stay reserved in schema but no client implementation |
| 2 | **Credentials: server-wide env only** | Single Yunhou-wide merchant for v1. Per-app overrides deferred (cost = app-config lookup + cert cache + serial lookup at request time; not needed yet) |
| 3 | **`code_url` storage:** new `orders.provider_intent JSONB` column | One row per order is enough; JSONB keeps schema thin and lets us extend with `prepay_id` etc. without further migrations |
| 4 | **5-tuple env validation in real mode** | `WECHAT_PAY_MCH_ID` + `WECHAT_PAY_API_V3_KEY` + `WECHAT_PAY_MCH_PRIVATE_KEY_PATH` + `WECHAT_PAY_MCH_CERT_PATH` + `WECHAT_PAY_NOTIFY_URL` must all be present (non-empty) when `WECHAT_PAY_MOCK=false`. Mock mode unchanged (any subset). Prevents silent partial-wire boot |
| 5 | **HTTPDoer interface retained** | Already exists in `internal/billing/wechat/wechat.go:35-54`. Lets unit tests stub the WeChat HTTP endpoint without `httptest`. Existing mock branch uses no HTTPDoer and keeps working |
| 6 | **5xx / network errors → immediate return, no retry** | Local state (`orders` row + `provider_intent`) and remote state (`code_url`) can diverge under retry; v1 lets the BFF cancel-and-retry. Marked `TODO` for v2 |
| 7 | **No response-signature verification** | WeChat signs some success responses with their platform cert (different from APIv3Key). v1 relies on TLS only. Noted in `sign.go` comment |
| 8 | **No refund API** | `RefundAPI.Refund(channel="wechat_pay", ...)` stays at the existing v1 stub (`ErrRefundChannelFailed`). Deferred to a follow-up PR (matches LS / Stripe / Alipay v1 posture) |
| 9 | **Out of scope:** JSAPI / H5 TradeType, plan_mapping routing, per-app cred overrides, WeChat platform-cert response verification | Reserved for follow-up |

## 3. Architecture

```
POST /payments/orders  (channel=wechat_pay)
  └─ service.PaymentService.CreateOrder
       ├─ orderRepo.Create(order)                         ← existing
       ├─ [wechat_pay + real mode] wechat.Client.UnifiedOrder(ctx, req)
       │     ├─ JSON body: { appid/mch_id, description, out_trade_no,
       │     │              notify_url, amount:{total,currency} }
       │     ├─ sign.BuildAuthHeader(method, path, body)
       │     │     └─ sign string = METHOD\nPATH\nTS\nNONCE\nBODY\n
       │     │     └─ SHA256withRSA(privateKey) → base64
       │     │     └─ "WECHATPAY2-SHA256-RSA2048 mchid=...,nonce_str=...,
       │     │                 timestamp=...,serial_no=...,signature=..."
       │     ├─ HTTPDoer.Do(POST api.mch.weixin.qq.com/v3/pay/transactions/native)
       │     └─ parse 200 → {code_url}; map 4xx/5xx → typed errors
       └─ orderRepo.UpdateProviderIntent(orderID, {code_url, out_trade_no, mch_id})
```

**Dependencies on entry path:**
- `cmd/server/main.go` at startup reads `WECHAT_PAY_MCH_PRIVATE_KEY_PATH` → parses RSA private key (PKCS#1 or PKCS#8); reads `WECHAT_PAY_MCH_CERT_PATH` → parses X.509 cert, extracts `SerialNumber.String()` for `serial_no`. Builds `Signer{MchID, SerialNo, PrivateKey}`. Builds `Client{MockMode: cfg.WeChatPayMock, MchID, Signer, NotifyURL, BaseURL, HTTPDoer}`. (APIv3Key lives on the `Signer` struct too if we ever need it for outbound signing — but v1 outbound signing uses the RSA private key, so APIv3Key stays only for inbound webhook HMAC + decrypt and is read directly from cfg in `cmd/server/main.go`.) Passes the client into `PaymentService` via the constructor — **new constructor parameter** added in this PR.
- HTTPDoer in production: thin adapter over `*http.Client` (with timeout). In tests: stub returns canned body or error.
- `PaymentService.CreateOrder` is channel-aware: when `Channel="wechat_pay"` AND the client is in real mode, it calls `UnifiedOrder` and persists `provider_intent`. Otherwise it returns the order unchanged (existing behavior for all other channels and for wechat_pay mock mode).

## 4. File map

| # | File | Change | Purpose |
|---|---|---|---|
| 1 | `migrations/009_wechat_pay_intent.sql` | **new** | `ALTER TABLE orders ADD COLUMN IF NOT EXISTS provider_intent JSONB NOT NULL DEFAULT '{}'::jsonb;` + COMMENT. Replay-safe (uses `IF NOT EXISTS`). Migration applies BEFORE server boot |
| 2 | `internal/billing/wechat/sign.go` | **new** | `Signer` struct + `BuildAuthHeader(ctx, method, path, body) (string, error)`. Pure function; no I/O |
| 3 | `internal/billing/wechat/sign_test.go` | **new** | Fixed test vector: known RSA key + body → known Authorization string. Catches drift in sign-string format |
| 4 | `internal/billing/wechat/wechat.go` | **extend** | (a) Replace `ErrUnimplemented` stub with real `UnifiedOrder` branch: build request JSON, call Signer, HTTPDoer, parse response. Mock branch stays byte-for-byte identical. New typed errors: `ErrWeChatUnifiedOrderRejected`, `ErrWeChatNetwork`. (b) Replace `CertPath`/`KeyPath` string fields on `Client` with a single `Signer *Signer` field. (c) Add `MchID() string` getter on `*Client` to satisfy the service-layer interface |
| 5 | `internal/billing/wechat/wechat_test.go` | **extend** | HTTPDoer stub tests: 200 happy / 4xx envelope / 5xx / network error / bad base64 cert / bad key / mock-mode still works. Existing mock-mode tests untouched |
| 6 | `internal/billing/wechat/cert.go` | **new** | `LoadPrivateKey(path string) (*rsa.PrivateKey, error)` and `LoadCertSerial(path string) (string, error)`. PEM parse + serial decimal-string extraction. Pure helpers; no I/O outside file read |
| 7 | `internal/billing/wechat/cert_test.go` | **new** | Test vectors: generated 2048-bit RSA key + self-signed cert, assert key parses, serial extracted matches expected format |
| 8 | `internal/config/config.go` | **extend** | +3 fields: `WeChatPayMchPrivateKeyPath`, `WeChatPayMchCertPath`, `WeChatPayNotifyURL`. `Load()` parses them. `Validate()` extends the asymmetric WECHAT_PAY_MCH_ID ↔ APIv3_KEY rule (existing) to require the 5-tuple when real mode |
| 9 | `internal/config/config_test.go` | **extend** | `TestValidate_WeChatReal_AllFiveRequired` (5 missing permutations fail), `TestValidate_WeChatMock_AllowsEmptyPartial` (mock + any subset of new envs still passes) |
| 10 | `internal/service/payment.go` | **extend** | `PaymentService` gets a new constructor field `wechat *wechat.Client`. `CreateOrder` adds a `wechat_pay`-and-real-mode branch: after `orderRepo.Create`, call `wechat.UnifiedOrder`, then `orderRepo.UpdateProviderIntent(orderID, providerIntentJSON)`. Other channels unchanged |
| 11 | `internal/service/payment_test.go` | **extend** | Stub `wechat.Client` via the existing interface seam (or wrap into a small `wechatClient` interface so tests can stub). Cover: real-mode CreateOrder persists `code_url`; mock-mode CreateOrder doesn't touch client; non-wechat channels don't touch client |
| 12 | `cmd/server/main.go` | **extend** | After `cfg, err := config.Load()`, call `wechat.LoadPrivateKey(...)` and `wechat.LoadCertSerial(...)`. Fail-fast if real mode + any load error. Build `wechat.Client`. Pass into `PaymentService` constructor. Adapt `*http.Client` → `wechat.HTTPDoer` (one-line adapter) |
| 13 | `PROGRESS.md` | **update** | Mark A2.c follow-up as ✅ shipped. Reference new commit hash. Note deferred items remain: refund API, plan_mapping, per-app overrides |

## 5. Data model

### `migrations/009_wechat_pay_intent.sql`

```sql
-- WeChat Pay NATIVE: code_url echoed to BFF for QR rendering.
-- Other channels may write other fields later (e.g. paypal order id).
-- Default '{}' keeps NOT NULL safe for legacy rows / pre-009 INSERTs.
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS provider_intent JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN orders.provider_intent IS
    'Per-channel provider metadata written after channel-specific pre-auth: '
    'wechat_pay → {code_url, out_trade_no, mch_id}; paypal → ...';
```

Replaysafe on re-run.

### `orders.provider_intent` JSON shape (wechat_pay only in v1)

```json
{
  "code_url": "weixin://wxpay/bizpayurl?pr=AB12CD...",
  "out_trade_no": "550e8400-e29b-41d4-a716-446655440000",
  "mch_id": "1234567890"
}
```

The repo layer exposes `UpdateProviderIntent(ctx, orderID, payload []byte) error` — service layer marshals the struct, repo writes `provider_intent = $payload::jsonb`. No ORM-mapped column.

## 6. Env vars (server-wide, real mode only)

| Var | Required | Default | Notes |
|---|---|---|---|
| `WECHAT_PAY_MCH_ID` | Conditionally | (empty) | **Existing** — 商户号. Real mode requires + symmetric with APIv3_KEY (unchanged from A2.c review fix) |
| `WECHAT_PAY_API_V3_KEY` | Conditionally | (empty) | **Existing** — 32 bytes. Used for inbound webhook HMAC + AES-GCM. Real mode requires (unchanged) |
| `WECHAT_PAY_MCH_PRIVATE_KEY_PATH` | Real mode | (empty) | **New** — Path to merchant RSA private key PEM (PKCS#1 or PKCS#8). Used to sign outbound requests |
| `WECHAT_PAY_MCH_CERT_PATH` | Real mode | (empty) | **New** — Path to merchant X.509 cert PEM. `serial_no` extracted at startup, used in `Authorization` header |
| `WECHAT_PAY_NOTIFY_URL` | Real mode | (empty) | **New** — Full URL WeChat calls back to (typically `https://<host>/webhooks/payment/wechat_pay`). Sent in the unified-order body |

**Validation rule** (additions to the existing asymmetric MCH_ID ↔ APIv3_KEY rule — the two existing case branches below stay; we add one more case after them):

```go
// Existing rule (unchanged) — asymmetric MCH_ID ↔ APIv3_KEY guard
case c.WeChatPayMchID == "" && c.WeChatAPIv3Key != "" && !c.WeChatPayMock:
    return errors.New("WECHAT_PAY_MCH_ID is required when WECHAT_PAY_API_V3_KEY is set and WECHAT_PAY_MOCK is not enabled")
case c.WeChatPayMchID != "" && c.WeChatAPIv3Key == "" && !c.WeChatPayMock:
    return errors.New("WECHAT_PAY_API_V3_KEY is required when WECHAT_PAY_MCH_ID is set and WECHAT_PAY_MOCK is not enabled")

// NEW rule — real mode also requires the 3 new envs to be present
case !c.WeChatPayMock && (
    c.WeChatPayMchPrivateKeyPath == "" ||
    c.WeChatPayMchCertPath == "" ||
    c.WeChatPayNotifyURL == ""):
    return errors.New("real WeChat Pay mode requires WECHAT_PAY_MCH_PRIVATE_KEY_PATH, "
        "WECHAT_PAY_MCH_CERT_PATH, and WECHAT_PAY_NOTIFY_URL")
```

The two existing cases still gate the symmetric 2-tuple. The new case covers the 3 new envs independently so a deployment with MCH_ID + APIv3_KEY but missing private key path fails fast with a clear message instead of crashing at HTTP-call time.

## 7. `internal/billing/wechat/sign.go`

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
// NOTE: WeChat also signs SOME success responses with their platform
// cert (not the APIv3Key). v1 does not verify those — we rely on TLS
// for transport-level integrity. Future PR.
func (s *Signer) BuildAuthHeader(method, path string, body []byte) (string, error) {
    ts := strconv.FormatInt(time.Now().Unix(), 10)
    nonceBytes := make([]byte, 16)
    if _, err := rand.Read(nonceBytes); err != nil {
        return "", fmt.Errorf("nonce gen: %w", err)
    }
    nonce := fmt.Sprintf("%x", nonceBytes)

    msg := method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" + string(body) + "\n"
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

## 8. `internal/billing/wechat/wechat.go` — real branch

```go
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

    // Real mode
    body, err := json.Marshal(map[string]interface{}{
        "appid":        "", // optional for sub-merchant, omit in v1
        "mch_id":       c.MchID,
        "description":  req.Description,
        "out_trade_no": req.OutTradeNo,
        "notify_url":   c.NotifyURL,
        "amount":       map[string]interface{}{"total": req.Amount.Total, "currency": req.Amount.Currency},
        "trade_type":   string(req.TradeType),
    })
    if err != nil {
        return nil, fmt.Errorf("marshal body: %w", err)
    }

    path := "/v3/pay/transactions/native"
    auth, err := c.Signer.BuildAuthHeader("POST", path, body)
    if err != nil {
        return nil, fmt.Errorf("build auth: %w", err)
    }

    resp, err := c.HTTPDoer.Do(&HTTPRequest{
        Method:  "POST",
        URL:     c.BaseURL + path,
        Headers: map[string]string{
            "Authorization": auth,
            "Content-Type":  "application/json",
            "Accept":        "application/json",
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
```

```go
var (
    ErrWeChatUnifiedOrderRejected = errors.New("wechat unified order rejected")
    ErrWeChatNetwork              = errors.New("wechat network error")
)
```

`ErrUnimplemented` stays exported for any external caller that referenced it during the deferred period; deprecation comment notes the new errors.

## 9. `internal/service/payment.go` — CreateOrder branch

```go
// CreateOrder mints an order row + (for wechat_pay real mode) mints the
// upstream code_url and persists it in orders.provider_intent.
func (s *PaymentService) CreateOrder(ctx context.Context, userID, planID string) (*model.Order, error) {
    // ... existing pre-checks (FindByID, active-sub check) ...

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
    // Only fires when the channel is wechat_pay AND the client is in
    // real mode (mock mode skips the upstream call entirely — the
    // handler-side mock code_url is enough for BFF development).
    // Other channels skip this step; their code_url equivalents land
    // in their own follow-up PRs.
    if s.wechat != nil && !s.wechat.IsMockMode() {
        amountFen := int64(order.Amount * 100) // CNY → fen
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
            "mch_id":       s.wechat.MchID,
        })
        if err := s.orderRepo.UpdateProviderIntent(ctx, order.ID, intent); err != nil {
            return order, fmt.Errorf("persist provider intent: %w", err)
        }
        order.ProviderIntent = intent // so the handler can echo it
    }
    return order, nil
}
```

`PaymentService` gets a small interface in front of `*wechat.Client` so unit tests can stub:

```go
type wechatClient interface {
    IsMockMode() bool
    MchID() string                       // service layer echoes this into provider_intent.mch_id
    UnifiedOrder(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error)
}
```

Stored on `PaymentService` as `wechat wechatClient` (interface, not concrete type). The interface lives in `service/payment.go` and is satisfied by `*wechat.Client` from `internal/billing/wechat/`. Existing tests pass `nil` for non-wechat flows; the new tests pass a hand-rolled stub implementing these three methods.

## 10. Tests

| File | Test | Covers |
|---|---|---|
| `sign_test.go` | `TestBuildAuthHeader_FixedVector` | Known RSA key + body → assert exact Authorization string matches a fixture captured from WeChat docs. Locks sign-string format |
| `sign_test.go` | `TestBuildAuthHeader_NonceUniqueness` | Two consecutive calls produce different `nonce_str` |
| `sign_test.go` | `TestBuildAuthHeader_TimestampFresh` | `timestamp` is within ±2s of `time.Now().Unix()` |
| `cert_test.go` | `TestLoadPrivateKey_PKCS1` | Generate PKCS#1 PEM, load, assert usable for sign |
| `cert_test.go` | `TestLoadPrivateKey_PKCS8` | Same for PKCS#8 |
| `cert_test.go` | `TestLoadCertSerial_DecimalString` | Self-signed cert, assert serial extracted is decimal form (matches WeChat's expected format) |
| `cert_test.go` | `TestLoadCertSerial_BadPEM` | Garbage file → returns error |
| `wechat_test.go` | `TestUnifiedOrder_Real_200` | HTTPDoer stub returns 200 + `{"code_url":"weixin://..."}` → client returns the URL, mocks order body shape (assert Authorization header present) |
| `wechat_test.go` | `TestUnifiedOrder_Real_4xx` | Stub returns 400 + WeChat error envelope → `ErrWeChatUnifiedOrderRejected` |
| `wechat_test.go` | `TestUnifiedOrder_Real_5xx` | Stub returns 500 → `ErrWeChatUnifiedOrderRejected` |
| `wechat_test.go` | `TestUnifiedOrder_Real_NetworkErr` | HTTPDoer returns net error → `ErrWeChatNetwork` |
| `wechat_test.go` | `TestUnifiedOrder_Real_EmptyCodeURL` | 200 with `{}` → `ErrWeChatUnifiedOrderRejected` |
| `wechat_test.go` | `TestUnifiedOrder_Mock_Unchanged` | Mock mode → still returns deterministic `pr=mock_<OutTradeNo>` URL (regression) |
| `config_test.go` | `TestValidate_WeChatReal_AllFiveRequired` | 5-tuple permutations: any one missing → error |
| `config_test.go` | `TestValidate_WeChatMock_AllowsEmpty` | Mock + zero new envs → no error (regression) |
| `config_test.go` | `TestValidate_WeChatReal_BothMCHs_NoOthers` | MCH_ID + APIv3_KEY set, no private key path → error (regression of A2.c asymmetric rule) |
| `payment_test.go` | `TestCreateOrder_WeChat_Real_PersistsIntent` | Stub wechatClient returns code_url → assert `orderRepo.UpdateProviderIntent` called with `{code_url, out_trade_no, mch_id}` JSON; assert returned `order.ProviderIntent` populated |
| `payment_test.go` | `TestCreateOrder_WeChat_Real_UnifiedOrderErr` | Stub returns error → assert order is still returned (callable decides cancel); assert `provider_intent` NOT written |
| `payment_test.go` | `TestCreateOrder_WeChat_Mock_NoClientCall` | Mock mode → stub's `UnifiedOrder` is NOT called; order has no `provider_intent` change |
| `payment_test.go` | `TestCreateOrder_Stripe_NilWeChat_OK` | Other channels unaffected when wechatClient is nil |

Plus a smoke build check: `make build` succeeds, `make lint` passes (go vet clean), `go test -race ./internal/...` green, `make ci-migrate` (idempotency) green.

## 11. Risk & rollback

| Risk | Mitigation |
|---|---|
| Real-mode call to WeChat fails → order row orphaned in `pending` | Documented in §9: caller decides cancel/retry; sweeper flips to `expired` after `ORDER_EXPIRY_DURATION`. No silent rollback |
| Cert rotation requires server restart | OK for v1; merchant cert change is rare and operational, not a runtime path |
| `provider_intent` JSON shape evolves | JSONB; no migration needed for new keys |
| Migration 009 fails on a DB that already has `provider_intent` (rare) | `IF NOT EXISTS` makes it replay-safe |
| Wrong sign-string format (off-by-one `\n`, missing trailing `\n`) | Fixed test vector in `sign_test.go` captures the canonical example from WeChat docs; any drift fails the test |

Rollback: revert the commit. Migration 009 with `IF NOT EXISTS` cannot be un-applied without manual `ALTER TABLE ... DROP COLUMN` — acceptable since the column is harmless empty JSONB on rollback.

## 12. Out of scope (follow-up PRs)

- WeChat Pay **refund** API (`/v3/refund/domestic/refunds`) — needs platform-cert response verification; deferred
- **JSAPI / H5** TradeType client impls — schema fields stay reserved
- **plan_mapping** routing — server-wide only for v1
- **Per-app credential overrides** via `apps.config.payment_providers.wechat_pay` — interface not exposed
- **WeChat platform-cert response-signature verification** — noted in `sign.go` comment
- **5xx retry with backoff** — v1 fails fast; caller retries