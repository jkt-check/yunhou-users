# PayPal Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `channel='paypal'` as a fifth supported payment channel — wiring through migration, config, verifier (HTTP verify-webhook-signature, dual sandbox+live), parser, service predicates, renewal branch, E2E tests, and docs. Mirrors the LemonSqueezy shape commit-for-commit.

**Architecture:** Same layering as LS: `model → repo → service → handler → middleware → router`. One new migration per concern (enum, schema). `PaypalVerifier` is the only piece that does I/O (1 outbound POST per webhook); everything else is local DB / JSON. Renewal events (`PAYMENT.SALE.COMPLETED`) get their own service branch that inserts a synthetic `orders` row + a `payments` row + extends `subscriptions.expires_at`. Refund path stays at the v2-stub level.

**Tech Stack:** Go 1.25, Gin, sqlx + Postgres, PayPal REST verify-webhook-signature. `httptest.NewServer` for in-process mock verify endpoint in unit + E2E tests.

**Spec:** `docs/superpowers/specs/2026-07-01-paypal-channel-design.md`

**Working directory for all commands:** repo root inside the worktree (`/Users/lili/Downloads/github/yunhou-users/.worktrees/feat-paypal-channel`). Run `pwd` first to confirm.

---

## File map

| File | Action | Purpose |
|---|---|---|
| `migrations/005_paypal_channel.sql` | create | Extend `payments / refunds / webhook_events` CHECK to allow `'paypal'` |
| `migrations/006_paypal_sub_mapping.sql` | create | `subscriptions.external_subscription_id` + partial UNIQUE index |
| `internal/config/config.go` | modify | Add `PaypalWebhookIDSandbox / Live`, `PaypalAPIBaseSandbox / Live`, `PaypalEnv`; read from env |
| `.env.example` | modify | Document the 5 new env vars |
| `internal/middleware/webhook_sig.go` | modify | Add `PaypalVerifier` struct + slot in `MultiChannelVerifier` |
| `internal/middleware/webhook_sig_test.go` | modify | Tests for `PaypalVerifier` + MultiChannelVerifier fan-out |
| `internal/handler/webhook.go` | modify | Add `parsePaypal` + dispatch case |
| `internal/handler/webhook_test.go` | modify | Tests for `parsePaypal` per event type |
| `internal/service/payment.go` | modify | Add PayPal event names to predicate switch; `validateChannel`; new `isPaypalRenewal`; `onPaypalRenewalSucceeded`; upsert `external_subscription_id` in `onPaymentSucceeded` |
| `internal/service/payment_test.go` | modify | Tests for predicates + renewal branch |
| `cmd/server/main.go` | modify | Wire `mv.Paypal` in `buildWebhookVerifier` |
| `tests/e2e/testhelpers.go` | modify | `signPaypal` helper + register mock verify endpoint |
| `tests/e2e/paypal_test.go` | create | E2E happy + renewal + refund + bad signature |
| `CLAUDE.md` | modify | Add env-var table rows; update migration order |
| `README.md` | modify | Add PayPal to channel list |
| `docs/api-integration-guide.md` | modify | Add PayPal to channel list |
| `docs/plans/2026-06-23-payment-webhook-mechanism.md` | modify | Add PayPal row to §6 retry table + new `### PayPal` section |

`.gitignore` already updated for `.worktrees/` in commit `b8169a1`.

`go.mod` does **not** change (only stdlib `crypto`, `encoding/json`, `net/http`, `io`).

---

## Task 1: Migration 005 — allow `channel='paypal'`

**Files:**
- Create: `migrations/005_paypal_channel.sql`

- [ ] **Step 1: Create the migration file**

Create `migrations/005_paypal_channel.sql`:

```sql
-- Migration: 005_paypal_channel
-- Description: extend payments/refunds/webhook_events CHECK constraints to allow channel='paypal'.
-- 设计文档: docs/superpowers/specs/2026-07-01-paypal-channel-design.md

BEGIN;

ALTER TABLE payments DROP CONSTRAINT payments_channel_check;
ALTER TABLE payments ADD CONSTRAINT payments_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

ALTER TABLE refunds DROP CONSTRAINT refunds_channel_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_channel_check;
ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

COMMIT;
```

- [ ] **Step 2: Commit**

```bash
git add migrations/005_paypal_channel.sql
git commit -m "feat(payments): migration 005 — allow channel='paypal'"
```

---

## Task 2: Migration 006 — `subscriptions.external_subscription_id`

**Files:**
- Create: `migrations/006_paypal_sub_mapping.sql`

- [ ] **Step 1: Create the migration file**

Create `migrations/006_paypal_sub_mapping.sql`:

```sql
-- Migration: 006_paypal_sub_mapping
-- Description: add subscriptions.external_subscription_id (PayPal subscription ID) + partial UNIQUE index.
-- 设计文档: docs/superpowers/specs/2026-07-01-paypal-channel-design.md

BEGIN;

ALTER TABLE subscriptions ADD COLUMN external_subscription_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_external_sub_id
    ON subscriptions (external_subscription_id)
    WHERE external_subscription_id IS NOT NULL;

COMMIT;
```

- [ ] **Step 2: Commit**

```bash
git add migrations/006_paypal_sub_mapping.sql
git commit -m "feat(payments): migration 006 — subscriptions.external_subscription_id"
```

---

## Task 3: Config — `PAYPAL_*` env vars

**Files:**
- Modify: `internal/config/config.go` (Config struct + Load)
- Modify: `.env.example`

- [ ] **Step 1: Add fields to `Config` struct in `internal/config/config.go`**

Locate the existing channel secret fields in the `Config` struct (around line 35). Add these immediately after `LemonSqueezyWebhookSecret`:

```go
// PayPal sandbox + live both loaded at startup; PAYPAL_ENV selects.
PaypalWebhookIDSandbox string
PaypalWebhookIDLive    string
PaypalAPIBaseSandbox   string
PaypalAPIBaseLive      string
PaypalEnv              string // "sandbox" | "live"
```

- [ ] **Step 2: Wire env reads in `Load()`**

In `internal/config/config.go::Load`, immediately after the existing `LemonSqueezyWebhookSecret: os.Getenv("LEMONSQUEEZY_WEBHOOK_SECRET"),` line, add:

```go
PaypalWebhookIDSandbox: os.Getenv("PAYPAL_WEBHOOK_ID_SANDBOX"),
PaypalWebhookIDLive:    os.Getenv("PAYPAL_WEBHOOK_ID_LIVE"),
PaypalAPIBaseSandbox:   envOr("PAYPAL_API_BASE_SANDBOX", "https://api-m.sandbox.paypal.com"),
PaypalAPIBaseLive:      envOr("PAYPAL_API_BASE_LIVE", "https://api-m.paypal.com"),
PaypalEnv:              envOr("PAYPAL_ENV", "live"),
```

- [ ] **Step 3: Run unit tests to make sure config still loads**

```bash
go test ./internal/config/...
```

Expected: PASS (no behavior change yet).

- [ ] **Step 4: Append `.env.example` PayPal section**

Append to `.env.example` (after the existing `LEMONSQUEEZY_WEBHOOK_SECRET=` line):

```

# PayPal (sandbox + live simultaneously loaded; PAYPAL_ENV selects which is active.
# Leave both webhook IDs blank to disable PayPal webhooks (returns 404).
PAYPAL_ENV=live
PAYPAL_WEBHOOK_ID_SANDBOX=
PAYPAL_WEBHOOK_ID_LIVE=
PAYPAL_API_BASE_SANDBOX=https://api-m.sandbox.paypal.com
PAYPAL_API_BASE_LIVE=https://api-m.paypal.com
```

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go .env.example
git commit -m "feat(config): add PAYPAL_* env vars"
```

---

## Task 4: Verifier — start with failing test for MultiChannelVerifier dispatch

**Files:**
- Modify: `internal/middleware/webhook_sig_test.go`

- [ ] **Step 1: Find the `MultiChannelVerifier` switch test in `webhook_sig_test.go`**

Search the file for the existing test that covers `MultiChannelVerifier.VerifySignature("wechat_pay", ...)`. The pattern looks like:

```go
func TestMultiChannelVerifier_UnsupportedChannel_404(t *testing.T) { ... }
```

or similar that asserts per-channel routing. The location and exact test name varies — locate it and read 20 lines.

- [ ] **Step 2: Append a new failing test for PayPal routing**

Append a test that asserts PayPal is routed to the configured `PaypalVerifier` slot:

```go
func TestMultiChannelVerifier_RoutesPaypal(t *testing.T) {
    var seen struct {
        channel string
        body    []byte
    }
    stub := &stubVerifier{
        onVerify: func(ch string, body []byte, _ map[string]string) error {
            seen.channel = ch
            seen.body = append([]byte(nil), body...)
            return nil
        },
    }
    mv := &middleware.MultiChannelVerifier{Paypal: stub}
    body := []byte(`{"id":"WH-1"}`)
    if err := mv.VerifySignature("paypal", body, map[string]string{"X-Anything": "y"}); err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if seen.channel != "paypal" {
        t.Errorf("channel routed wrong, got %q want %q", seen.channel, "paypal")
    }
    if string(seen.body) != string(body) {
        t.Errorf("body not passed through")
    }
}

func TestMultiChannelVerifier_PaypalSlotNilReturnsUnsupportedChannel(t *testing.T) {
    mv := &middleware.MultiChannelVerifier{}
    err := mv.VerifySignature("paypal", []byte(`{}`), map[string]string{})
    if !errors.Is(err, middleware.ErrUnsupportedChannel) {
        t.Fatalf("want ErrUnsupportedChannel, got %v", err)
    }
}
```

(`stubVerifier` is the existing test helper in `webhook_sig_test.go`; if its field name differs, adjust accordingly — search the file for `stub` to find it.)

- [ ] **Step 3: Run tests to verify they fail at compile / `Paypal` field doesn't exist**

```bash
go test ./internal/middleware/ -run TestMultiChannelVerifier_RoutesPaypal -v
```

Expected: compile error (`MultiChannelVerifier` has no field `Paypal`) — that's the failing-test state we want.

- [ ] **Step 4: Commit the failing test alone**

```bash
git add internal/middleware/webhook_sig_test.go
git commit -m "test(webhook): cover MultiChannelVerifier routing for paypal"
```

---

## Task 5: Verifier — `PaypalVerifier` struct + slot in `MultiChannelVerifier`

**Files:**
- Modify: `internal/middleware/webhook_sig.go`

- [ ] **Step 1: Add the struct + constructor + slot**

In `internal/middleware/webhook_sig.go`, after the existing `LemonsqueezyVerifier` block and before `MultiChannelVerifier` (search for `// =====…====` block separators), add:

```go
// ============================================================================
// PayPal — HTTP verify-webhook-signature (sandbox + live both loaded)
// ============================================================================

// PaypalVerifier verifies PayPal webhooks by POSTing the headers + body to
// PayPal's verify-webhook-signature endpoint. The endpoint URL is selected
// from Env ("sandbox" | "live"); both webhook IDs and API bases are loaded
// at startup so deployments don't need to restart to flip environments.
//
// Replay protection is provided by the event-level dedupe (webhook_events
// UNIQUE(channel, event_id)) — same approach as LemonSqueezy. We do NOT
// enforce a local replay window because PayPal's transmission_time is meant
// only for verification, not for our own dedup.
type PaypalVerifier struct {
    HTTPClient         *http.Client // nil → http.DefaultClient; callers should set 5s Timeout.
    SandboxWebhookID   string
    LiveWebhookID      string
    SandboxAPIBase     string // default https://api-m.sandbox.paypal.com
    LiveAPIBase        string // default https://api-m.paypal.com
    Env                string // "sandbox" | "live"
}

func (v *PaypalVerifier) activeConfig() (webhookID, apiBase string, err error) {
    var (
        id, base string
    )
    switch v.Env {
    case "sandbox":
        id, base = v.SandboxWebhookID, v.SandboxAPIBase
    case "live":
        id, base = v.LiveWebhookID, v.LiveAPIBase
    default:
        return "", "", fmt.Errorf("%w: unknown PAYPAL_ENV %q", ErrUnsupportedChannel, v.Env)
    }
    if base == "" {
        base = "https://api-m.paypal.com"
    }
    return id, base, nil
}

func (v *PaypalVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
    _ = channel

    // Required PayPal headers — missing → bad signature (channel won't retry).
    authAlgo := headers["PAYPAL-AUTH-ALGO"]
    certURL := headers["PAYPAL-CERT-URL"]
    transmissionID := headers["PAYPAL-TRANSMISSION-ID"]
    transmissionSIG := headers["PAYPAL-TRANSMISSION-SIG"]
    transmissionTime := headers["PAYPAL-TRANSMISSION-TIME"]
    if authAlgo == "" || certURL == "" || transmissionID == "" ||
        transmissionSIG == "" || transmissionTime == "" {
        return ErrInvalidSignature
    }

    webhookID, apiBase, err := v.activeConfig()
    if err != nil {
        return err
    }
    if webhookID == "" {
        // Caller asked us to verify a PayPal event but no webhook_id is configured
        // for the active env. Treat as not-supported so middleware returns 404.
        return ErrUnsupportedChannel
    }

    // Wrap the raw body as JSON (`{"webhook_event": ...}`) per PayPal's spec.
    raw, _ := json.Marshal(map[string]any{"webhook_event": json.RawMessage(body)})
    payload, _ := json.Marshal(map[string]any{
        "auth_algo":         authAlgo,
        "cert_url":          certURL,
        "transmission_id":   transmissionID,
        "transmission_sig":  transmissionSIG,
        "transmission_time": transmissionTime,
        "webhook_id":        webhookID,
        "webhook_event":     json.RawMessage(body),
    })
    _ = raw // keep raw around for clarity in future debug prints

    httpClient := v.HTTPClient
    if httpClient == nil {
        httpClient = http.DefaultClient
    }
    req, err := http.NewRequest(http.MethodPost, apiBase+"/v1/notifications/verify-webhook-signature", bytes.NewReader(payload))
    if err != nil {
        return fmt.Errorf("paypal verify: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Accept", "application/json")
    resp, err := httpClient.Do(req)
    if err != nil {
        // Network error → transient (500), let PayPal retry.
        return fmt.Errorf("paypal verify http: %w", err)
    }
    defer resp.Body.Close()

    body_resp, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
    if err != nil {
        return fmt.Errorf("paypal verify read: %w", err)
    }
    if resp.StatusCode >= 500 {
        return fmt.Errorf("paypal verify status %d: %s", resp.StatusCode, string(body_resp))
    }
    if resp.StatusCode >= 400 {
        // 4xx from PayPal is rare for the verify endpoint; treat as transient
        // so we don't 400 a legitimate event due to PayPal rate-limiting us.
        return fmt.Errorf("paypal verify status %d: %s", resp.StatusCode, string(body_resp))
    }

    var out struct {
        VerificationStatus string `json:"verification_status"`
    }
    if err := json.Unmarshal(body_resp, &out); err != nil {
        return fmt.Errorf("paypal verify decode: %w", err)
    }
    if out.VerificationStatus != "SUCCESS" {
        return ErrInvalidSignature
    }
    return nil
}
```

Also add the import at the top of `webhook_sig.go`:

```go
"encoding/json"
```

(`bytes` is already imported; `io` too. `http` too. If not, add them.)

- [ ] **Step 2: Add `Paypal` slot to `MultiChannelVerifier`**

In the same file, find the `MultiChannelVerifier` struct (around line 422 of the LS-state file). Add a `Paypal` field:

```go
type MultiChannelVerifier struct {
    Stripe       ChannelSignatureVerifier
    WeChat       ChannelSignatureVerifier
    Alipay       ChannelSignatureVerifier
    LemonSqueezy ChannelSignatureVerifier
    Paypal       ChannelSignatureVerifier
}
```

And in the `VerifySignature` switch, add:

```go
case "paypal":
    v = m.Paypal
```

- [ ] **Step 3: Run the previously-failing test from Task 4**

```bash
go test ./internal/middleware/ -run TestMultiChannelVerifier_RoutesPaypal -v
```

Expected: PASS (Paypal slot now present + dispatcher routes correctly).

- [ ] **Step 4: Run the full middleware test suite to make sure nothing else broke**

```bash
go test ./internal/middleware/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/middleware/webhook_sig.go
git commit -m "feat(webhook): add PaypalVerifier (HTTP verify-webhook-signature)"
```

---

## Task 6: Verifier — TDD for happy path + error mapping via `httptest`

**Files:**
- Modify: `internal/middleware/webhook_sig_test.go`

- [ ] **Step 1: Append test scaffolding for `PaypalVerifier`**

Add a new helper at the top of the test file (next to other helpers):

```go
// newPaypalHarness spins up an httptest server that mimics PayPal's
// verify-webhook-signature endpoint. The server returns the configured
// response on each call (or a recorded status); tests set server behavior,
// then call the verifier with the headers PayPal would send.
func newPaypalHarness(t *testing.T, respondWith func(w http.ResponseWriter, r *http.Request)) (*middleware.PaypalVerifier, *httptest.Server) {
    t.Helper()
    srv := httptest.NewServer(http.HandlerFunc(respondWith))
    t.Cleanup(srv.Close)
    v := &middleware.PaypalVerifier{
        SandboxWebhookID: "wbh_sbx",
        SandboxAPIBase:   srv.URL,
        Env:              "sandbox",
        HTTPClient:       &http.Client{Timeout: 2 * time.Second},
    }
    return v, srv
}

func paypalHeaders(transmissionID, sig string) map[string]string {
    return map[string]string{
        "PAYPAL-AUTH-ALGO":         "SHA256withRSA",
        "PAYPAL-CERT-URL":          "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42-fca2ab1b-7ce9e4e3-abcdef",
        "PAYPAL-TRANSMISSION-ID":   transmissionID,
        "PAYPAL-TRANSMISSION-SIG":  sig,
        "PAYPAL-TRANSMISSION-TIME": "2026-06-30T12:00:00Z",
    }
}
```

Add imports if missing:

```go
"net/http/httptest"
"time"
```

- [ ] **Step 2: Append happy-path test**

```go
func TestPaypalVerifier_HappyPath(t *testing.T) {
    var seen map[string]any
    v, _ := newPaypalHarness(t, func(w http.ResponseWriter, r *http.Request) {
        body, _ := io.ReadAll(r.Body)
        _ = json.Unmarshal(body, &seen)
        w.Header().Set("Content-Type", "application/json")
        fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
    })
    body := []byte(`{"id":"WH-123","event_type":"PAYMENT.CAPTURE.COMPLETED"}`)
    if err := v.VerifySignature("paypal", body, paypalHeaders("tid-1", "sig-1")); err != nil {
        t.Fatalf("want nil, got %v", err)
    }
    if seen["webhook_id"] != "wbh_sbx" {
        t.Errorf("webhook_id not forwarded: %v", seen)
    }
}
```

- [ ] **Step 3: Append FAILURE-mapping test**

```go
func TestPaypalVerifier_FailureIsInvalidSignature(t *testing.T) {
    v, _ := newPaypalHarness(t, func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintln(w, `{"verification_status":"FAILURE"}`)
    })
    err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
    if !errors.Is(err, middleware.ErrInvalidSignature) {
        t.Fatalf("want ErrInvalidSignature, got %v", err)
    }
}
```

- [ ] **Step 4: Append missing-headers test**

```go
func TestPaypalVerifier_MissingHeader(t *testing.T) {
    v := &middleware.PaypalVerifier{
        SandboxWebhookID: "wbh_sbx",
        SandboxAPIBase:   "http://127.0.0.1:1",
        Env:              "sandbox",
    }
    err := v.VerifySignature("paypal", []byte(`{}`), map[string]string{
        // PAYPAL-AUTH-ALGO missing on purpose
        "PAYPAL-CERT-URL":         "x",
        "PAYPAL-TRANSMISSION-ID":  "x",
        "PAYPAL-TRANSMISSION-SIG": "x",
        "PAYPAL-TRANSMISSION-TIME": "x",
    })
    if !errors.Is(err, middleware.ErrInvalidSignature) {
        t.Fatalf("want ErrInvalidSignature, got %v", err)
    }
}
```

- [ ] **Step 5: Append env-selection test**

```go
func TestPaypalVerifier_EnvSelectsLive(t *testing.T) {
    v, _ := newPaypalHarness(t, func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
    })
    // Harness uses SandboxAPIBase; flip verifier to point at a SAME-URL
    // override via LiveAPIBase + Env="live".
    v.Env = "live"
    v.LiveAPIBase = v.SandboxAPIBase
    v.LiveWebhookID = "wbh_live"
    body := []byte(`{"id":"WH-1"}`)
    if err := v.VerifySignature("paypal", body, paypalHeaders("tid-1", "sig-1")); err != nil {
        t.Fatalf("want nil, got %v", err)
    }
}
```

- [ ] **Step 6: Append no-webhook-id-for-active-env test**

```go
func TestPaypalVerifier_NoWebhookIDForEnvReturnsUnsupported(t *testing.T) {
    v := &middleware.PaypalVerifier{
        SandboxAPIBase: "https://api-m.sandbox.paypal.com",
        Env:            "sandbox",
        // SandboxWebhookID intentionally empty
    }
    err := v.VerifySignature("paypal", []byte(`{}`), paypalHeaders("tid-1", "sig-1"))
    if !errors.Is(err, middleware.ErrUnsupportedChannel) {
        t.Fatalf("want ErrUnsupportedChannel, got %v", err)
    }
}
```

- [ ] **Step 7: Run all middlewares tests**

```bash
go test ./internal/middleware/...
```

Expected: PASS for all new and existing tests.

- [ ] **Step 8: Commit**

```bash
git add internal/middleware/webhook_sig_test.go
git commit -m "test(webhook): cover PaypalVerifier happy + failure + env selection"
```

---

## Task 7: Parser — TDD for `parsePaypal` event-type dispatch

**Files:**
- Modify: `internal/handler/webhook_test.go`
- Modify: `internal/handler/webhook.go`

- [ ] **Step 1: Append failing tests for `parsePaypal` in `internal/handler/webhook_test.go`**

Locate the existing `parseLemonsqueezy` tests in the file (search for `parseLemonsqueezy`). Append:

```go
func TestParsePaypal_CaptureCompleted(t *testing.T) {
    raw := []byte(`{
        "id": "WH-PAYPAL-1",
        "event_type": "PAYMENT.CAPTURE.COMPLETED",
        "resource": {
            "id": "3C93638325N1234567",
            "custom_id": "order-uuid-123",
            "amount": {"value": "29.90", "currency_code": "USD"}
        }
    }`)
    h := &WebhookHandler{}
    evt, err := h.parseEvent("paypal", raw)
    if err != nil {
        t.Fatalf("parseEvent: %v", err)
    }
    if evt.Channel != "paypal" || evt.EventID != "WH-PAYPAL-1" || evt.EventType != "PAYMENT.CAPTURE.COMPLETED" {
        t.Errorf("unexpected envelope: %+v", evt)
    }
    if evt.OrderID != "order-uuid-123" || evt.TransactionID != "3C93638325N1234567" {
        t.Errorf("order/txn ID wrong: %+v", evt)
    }
    if evt.Amount != 29.90 || evt.Currency != "USD" {
        t.Errorf("amount/currency: %+v", evt)
    }
}

func TestParsePaypal_CaptureRefunded(t *testing.T) {
    raw := []byte(`{
        "id": "WH-PAYPAL-2",
        "event_type": "PAYMENT.CAPTURE.REFUNDED",
        "resource": {
            "id": "REFUND-1",
            "custom_id": "order-uuid-123",
            "amount": {"value": "29.90", "currency_code": "USD"}
        }
    }`)
    h := &WebhookHandler{}
    evt, err := h.parseEvent("paypal", raw)
    if err != nil {
        t.Fatalf("parseEvent: %v", err)
    }
    if evt.RefundAmount != 29.90 || evt.ExternalRefundID != "paypal-REFUND-1" {
        t.Errorf("refund fields: %+v", evt)
    }
    if evt.EventType != "PAYMENT.CAPTURE.REFUNDED" {
        t.Errorf("event type: %s", evt.EventType)
    }
}

func TestParsePaypal_SaleCompletedSetsExternalSubID(t *testing.T) {
    raw := []byte(`{
        "id": "WH-PAYPAL-3",
        "event_type": "PAYMENT.SALE.COMPLETED",
        "resource": {
            "id": "SALE-1",
            "billing_agreement_id": "I-BWX42XYZ",
            "custom_id": "order-uuid-456",
            "amount": {"value": "9.99", "currency_code": "USD"},
            "billing_info": {"next_billing_time": "2026-08-30T12:00:00Z"}
        }
    }`)
    h := &WebhookHandler{}
    evt, err := h.parseEvent("paypal", raw)
    if err != nil {
        t.Fatalf("parseEvent: %v", err)
    }
    if evt.ExternalSubscriptionID != "I-BWX42XYZ" {
        t.Errorf("external sub id: %q", evt.ExternalSubscriptionID)
    }
    if evt.SubExpiresAt == nil {
        t.Errorf("SubExpiresAt should be set")
    }
}

func TestParsePaypal_SubscriptionCreatedSetsExternalSubID(t *testing.T) {
    raw := []byte(`{
        "id": "WH-PAYPAL-4",
        "event_type": "BILLING.SUBSCRIPTION.CREATED",
        "resource": {
            "id": "I-BWX42ABCD",
            "custom_id": "order-uuid-789"
        }
    }`)
    h := &WebhookHandler{}
    evt, err := h.parseEvent("paypal", raw)
    if err != nil {
        t.Fatalf("parseEvent: %v", err)
    }
    if evt.ExternalSubscriptionID != "I-BWX42ABCD" {
        t.Errorf("subscription id: %q", evt.ExternalSubscriptionID)
    }
    if evt.EventType != "BILLING.SUBSCRIPTION.CREATED" {
        t.Errorf("event type: %s", evt.EventType)
    }
}

func TestParsePaypal_MissingFields(t *testing.T) {
    raw := []byte(`{"id": "WH-1"}`) // no event_type, no resource
    h := &WebhookHandler{}
    _, err := h.parseEvent("paypal", raw)
    if err == nil {
        t.Fatalf("expected error for malformed event")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/handler/ -run TestParsePaypal -v
```

Expected: compile error (no case `"paypal"` in `parseEvent`).

- [ ] **Step 3: Add `ExternalSubscriptionID` to `WebhookEvent`**

In `internal/service/payment.go`, find the `WebhookEvent` struct (around line 548). Add a new field:

```go
ExternalSubscriptionID string // PayPal renewal binding (resource.billing_agreement_id or resource.id)
```

- [ ] **Step 4: Implement `parsePaypal`**

In `internal/handler/webhook.go`, in the `parseEvent` switch (around line 99), add:

```go
case "paypal":
    return h.parsePaypal(raw)
```

Then, near the existing `parseLemonsqueezy` (around line 351), add the implementation:

```go
// parsePaypal extracts fields from a PayPal webhook. PayPal's webhook event
// shape is:
//
//   {
//     "id":         "WH-...",
//     "event_type": "PAYMENT.CAPTURE.COMPLETED" | ...,
//     "resource":   { "id": "...", "custom_id": "<our order uuid>",
//                     "amount": { "value": "29.90", "currency_code": "USD" },
//                     "billing_agreement_id": "I-..." (renewal only),
//                     "billing_info": { "next_billing_time": "..." } (renewal only) }
//   }
//
// Order binding uses resource.custom_id, which the frontend sets to our
// order UUID at PayPal Order / Subscription creation time. PAYMENT.SALE.* and
// BILLING.SUBSCRIPTION.* events also carry resource.billing_agreement_id —
// we map that to WebhookEvent.ExternalSubscriptionID for the renewal branch.
func (h *WebhookHandler) parsePaypal(raw []byte) (*service.WebhookEvent, error) {
    var evt struct {
        ID        string `json:"id"`
        EventType string `json:"event_type"`
        Resource  struct {
            ID                 string `json:"id"`
            CustomID           string `json:"custom_id"`
            BillingAgreementID string `json:"billing_agreement_id"`
            Amount             struct {
                Value        string `json:"value"`
                CurrencyCode string `json:"currency_code"`
            } `json:"amount"`
            BillingInfo *struct {
                NextBillingTime string `json:"next_billing_time"`
            } `json:"billing_info"`
        } `json:"resource"`
    }
    if err := json.Unmarshal(raw, &evt); err != nil {
        return nil, fmt.Errorf("paypal body: %w", err)
    }
    if evt.ID == "" || evt.EventType == "" {
        return nil, fmt.Errorf("paypal missing id or event_type")
    }
    if evt.Resource.CustomID == "" {
        return nil, fmt.Errorf("paypal missing resource.custom_id")
    }

    we := &service.WebhookEvent{
        Channel:               "paypal",
        EventID:               evt.ID,
        EventType:             evt.EventType,
        OrderID:               evt.Resource.CustomID,
        TransactionID:         evt.Resource.ID,
        ExternalSubscriptionID: evt.Resource.BillingAgreementID,
        Currency:              strings.ToUpper(evt.Resource.Amount.CurrencyCode),
    }

    if v, err := strconv.ParseFloat(evt.Resource.Amount.Value, 64); err == nil {
        we.Amount = v
    }

    if evt.Resource.BillingInfo != nil && evt.Resource.BillingInfo.NextBillingTime != "" {
        if t, err := time.Parse(time.RFC3339, evt.Resource.BillingInfo.NextBillingTime); err == nil {
            we.SubExpiresAt = &t
        }
    }

    if isPaypalRefundEvent(evt.EventType) {
        we.RefundAmount = we.Amount
        we.ExternalRefundID = "paypal-" + evt.Resource.ID
    }
    return we, nil
}

func isPaypalRefundEvent(eventType string) bool {
    return eventType == "PAYMENT.CAPTURE.REFUNDED"
}

func isPaypalRenewal(eventType string) bool {
    return eventType == "PAYMENT.SALE.COMPLETED"
}
```

Add `strconv` to the import list of `webhook.go` if not already imported.

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./internal/handler/ -run TestParsePaypal -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/webhook.go internal/handler/webhook_test.go internal/service/payment.go
git commit -m "feat(handler,service): add parsePaypal + WebhookEvent.ExternalSubscriptionID"
```

---

## Task 8: Service — predicates + `validateChannel`

**Files:**
- Modify: `internal/service/payment.go`

- [ ] **Step 1: Add `"paypal"` to `validateChannel`**

Find the `validateChannel` function (around line 1145). Update:

```go
func validateChannel(channel string) error {
    switch channel {
    case "stripe", "wechat_pay", "alipay", "lemonsqueezy", "paypal":
        return nil
    default:
        return fmt.Errorf("%w: %s", ErrInvalidChannel, channel)
    }
}
```

- [ ] **Step 2: Add PayPal event names to predicates**

Find each of these and append PayPal event names:

`isPaymentSuccess`:
```go
func isPaymentSuccess(eventType string) bool {
    switch eventType {
    case "payment_intent.succeeded", "TRANSACTION.SUCCESS",
        "TRADE_SUCCESS", "trade_status_sync",
        "order_created", "subscription_created",
        "PAYMENT.CAPTURE.COMPLETED", "BILLING.SUBSCRIPTION.CREATED":
        return true
    }
    return false
}
```

`isPaymentFailed`:
```go
func isPaymentFailed(eventType string) bool {
    switch eventType {
    case "payment_intent.payment_failed", "payment_intent.canceled",
        "TRANSACTION.PAY_FAILED", "TRANSACTION.REVOKED",
        "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.FAILED":
        return true
    }
    return false
}
```

`isRefundEvent`:
```go
func isRefundEvent(eventType string) bool {
    switch eventType {
    case "charge.refunded", "TRANSACTION.REFUND",
        "TRADE_CLOSED", "trade_closed",
        "order_refunded", "subscription_payment_refunded",
        "PAYMENT.CAPTURE.REFUNDED":
        return true
    }
    return false
}
```

- [ ] **Step 3: Run tests to make sure nothing broke**

```bash
go test ./internal/service/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service/payment.go
git commit -m "feat(payment): accept channel=paypal; map capture/denied/FAILED/refunded/subscription_created"
```

---

## Task 9: Service — `onPaymentSucceeded` PayPal extension (upsert `external_subscription_id`)

**Files:**
- Modify: `internal/service/payment.go`

- [ ] **Step 1: Locate `onPaymentSucceeded`**

Find the function (around line 668). The transaction closes around `tx.Commit()`; we'll add ONE extra SQL statement inside the existing transaction, right after `activateSubscriptionOnTx`.

- [ ] **Step 2: Insert the upsert**

Immediately after the `activateSubscriptionOnTx` call, before `wasLate := order.Status == "expired"`:

```go
    // PayPal: stamp the PayPal subscription ID on the active subscription row
    // so renewal webhooks (PAYMENT.SALE.COMPLETED) can find us. The partial
    // UNIQUE index subs.external_subscription_id makes a no-op if it's
    // already set, which is exactly what we want on retries.
    if e.ExternalSubscriptionID != "" {
        res, err := tx.ExecContext(ctx, `
            UPDATE subscriptions
            SET external_subscription_id = $1
            WHERE user_id = $2
              AND plan_id = $3
              AND status = 'active'
              AND external_subscription_id IS NULL
        `, e.ExternalSubscriptionID, order.UserID, order.PlanID)
        if err != nil {
            return fmt.Errorf("set external_subscription_id: %w", err)
        }
        _ = res
    }
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/service/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service/payment.go
git commit -m "feat(payment): stamp subscriptions.external_subscription_id on PayPal events"
```

---

## Task 10: Service — `onPaypalRenewalSucceeded`

**Files:**
- Modify: `internal/service/payment.go`
- Modify: `internal/service/interfaces.go` (or appropriate interfaces file)
- Modify: `internal/service/payment_test.go`

- [ ] **Step 1: Confirm `SubscriptionRepo` exposes the lookup we need**

Open `internal/service/interfaces.go`, find the `SubscriptionRepo` interface. Confirm it has a method like `FindByExternalSubID(ctx, extSubID) (*model.Subscription, error)`. If not:

- Add to `internal/repo/subscription.go` (real impl):
  ```go
  func (r *SubscriptionRepo) FindByExternalSubID(ctx context.Context, extSubID string) (*model.Subscription, error) {
      var s model.Subscription
      err := r.db.GetContext(ctx, &s, `SELECT * FROM subscriptions WHERE external_subscription_id = $1 LIMIT 1`, extSubID)
      if err != nil { return nil, err }
      return &s, nil
  }
  ```

- Add to the interface in `internal/service/interfaces.go`:
  ```go
  FindByExternalSubID(ctx context.Context, extSubID string) (*model.Subscription, error)
  ```

If `subscription_test.go` has a hand-rolled mock, add the same method there. (Search for `SubscriptionRepo` in test files.)

- [ ] **Step 2: Add `OnWebhook` dispatch case**

In `internal/service/payment.go::OnWebhook`, find the `switch` block (around line 620). Add a new arm **before** the `default` case:

```go
    case isPaypalRenewal(e.EventType):
        domainAction = "payment_paid"
        if err := s.onPaypalRenewalSucceeded(ctx, e); err != nil {
            return nil, err
        }
```

- [ ] **Step 3: Implement `onPaypalRenewalSucceeded`**

After `onDisputeClosed` (around line 996) and before the `// =====…==== Pure helpers` block, add:

```go
// onPaypalRenewalSucceeded handles PAYMENT.SALE.COMPLETED — the renewal
// charge that PayPal fires automatically when a subscription auto-renews.
// We don't have an `order` row for renewals (the original order was months
// ago); the fresh approach: synthesize an `orders` row, INSERT the payment,
// and extend subscriptions.expires_at from resource.billing_info.next_billing_time.
func (s *PaymentService) onPaypalRenewalSucceeded(ctx context.Context, e WebhookEvent) error {
    if e.ExternalSubscriptionID == "" {
        return s.writeAudit(ctx, "service", "paypal_renewal_missing_external_sub_id",
            fmt.Sprintf("event:%s", e.EventID),
            []string{"webhook", "renewal", "missing_field"},
            map[string]any{"channel": e.Channel, "event_id": e.EventID})
    }
    tx, err := s.db.BeginTxx(ctx, nil)
    if err != nil { return fmt.Errorf("begin tx: %w", err) }
    defer tx.Rollback() //nolint:errcheck

    var sub model.Subscription
    err = tx.GetContext(ctx, &sub, `
        SELECT * FROM subscriptions WHERE external_subscription_id = $1 LIMIT 1
    `, e.ExternalSubscriptionID)
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return s.writeAudit(ctx, "service", "paypal_renewal_unknown_subscription",
                fmt.Sprintf("event:%s", e.EventID),
                []string{"webhook", "renewal", "unknown_sub"},
                map[string]any{"channel": e.Channel, "event_id": e.EventID, "external_subscription_id": e.ExternalSubscriptionID})
        }
        return fmt.Errorf("find subscription: %w", err)
    }

    var orderID string
    err = tx.QueryRowxContext(ctx, `
        INSERT INTO orders (user_id, plan_id, amount, currency, status)
        VALUES ($1, $2, $3, $4, 'paid')
        RETURNING id
    `, sub.UserID, sub.PlanID, e.Amount, e.Currency).Scan(&orderID)
    if err != nil { return fmt.Errorf("insert synthetic order: %w", err) }

    now := time.Now()
    p := &model.Payment{
        OrderID:       orderID,
        Channel:       e.Channel,
        ExternalTxnID: e.TransactionID,
        Amount:        e.Amount,
        Currency:      e.Currency,
        Status:        "paid",
        PaidAt:        &now,
        RawPayload:    e.RawPayload,
    }
    if _, _, err := insertPaymentOnTx(ctx, tx, p); err != nil {
        return fmt.Errorf("insert payment: %w", err)
    }

    if e.SubExpiresAt != nil {
        if _, err := tx.ExecContext(ctx, `
            UPDATE subscriptions SET expires_at = $1, updated_at = now()
            WHERE id = $2 AND status = 'active'
        `, *e.SubExpiresAt, sub.ID); err != nil {
            return fmt.Errorf("extend expires_at: %w", err)
        }
    }

    return tx.Commit()
}
```

Note: `p.ID = uuid_generate_v4()` is left out because `insertPaymentOnTx` uses `RETURNING id` from the SQL ON CONFLICT path — confirm by reading `insertPaymentOnTx`'s source. If it instead expects `p.ID` to be pre-set, prepend `p.ID = GenerateUUID()` before the call.

- [ ] **Step 4: Append unit test for the renewal branch**

In `internal/service/payment_test.go`, find an existing `OnWebhook` test (search `TestOnWebhook_`) and pattern-match the mock setup. Add:

```go
func TestOnWebhook_PaypalRenewal_HappyPath(t *testing.T) {
    // mock db, mock repos per existing patterns; assert: insertPaymentOnTx
    // called once with status=paid, expires_at extended on subscription row.
}

func TestOnWebhook_PaypalRenewal_UnknownSubscriptionWritesAudit(t *testing.T) {
    // assert: no payment inserted, audit_log contains "paypal_renewal_unknown_subscription".
}
```

(Read two existing OnWebhook tests and copy the mock setup; this is the largest piece of "write tests that look like the existing ones" in the plan.)

- [ ] **Step 5: Run service tests**

```bash
go test ./internal/service/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service/payment.go internal/service/payment_test.go internal/service/interfaces.go internal/repo/subscription.go
git commit -m "feat(payment): handle PAYMENT.SALE.COMPLETED via onPaypalRenewalSucceeded"
```

---

## Task 11: Server wiring — `cmd/server/main.go`

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Locate `buildWebhookVerifier`**

Find the function (around line 156). Append after the existing `LemonSqueezyWebhookSecret` block:

```go
    if cfg.PaypalEnv == "sandbox" || cfg.PaypalEnv == "live" {
        mv.Paypal = &middleware.PaypalVerifier{
            SandboxWebhookID: cfg.PaypalWebhookIDSandbox,
            LiveWebhookID:    cfg.PaypalWebhookIDLive,
            SandboxAPIBase:   cfg.PaypalAPIBaseSandbox,
            LiveAPIBase:      cfg.PaypalAPIBaseLive,
            Env:              cfg.PaypalEnv,
            HTTPClient:       &http.Client{Timeout: 5 * time.Second},
        }
    }
```

- [ ] **Step 2: Add `net/http` import if missing**

Search imports at the top of `main.go` for `net/http`; if absent, add it.

- [ ] **Step 3: Build the binary to make sure it compiles**

```bash
make build
```

Expected: builds to `bin/server` with no errors.

- [ ] **Step 4: Run all unit tests one more time**

```bash
make test
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat(server): wire PAYPAL_* env vars into buildWebhookVerifier"
```

---

## Task 12: E2E — PayPal fixtures + signing helper

**Files:**
- Modify: `tests/e2e/testhelpers.go`

- [ ] **Step 1: Add PayPal test constants**

In `tests/e2e/testhelpers.go`, find `e2eLemonSqueezySecret` (line ~47 area) and append:

```go
    e2ePaypalWebhookIDSandbox = "wbh_e2e_sandbox"
    e2ePaypalWebhookIDLive    = "wbh_e2e_live"
    // e2ePaypalEnv tells the verifier which to use.
    e2ePaypalEnv       = "sandbox"
    e2ePaypalAPIBaseSBX = "" // set per test to a httptest server URL
```

- [ ] **Step 2: Add `signPaypal` helper**

Append after `signLemonSqueezy`:

```go
// signPaypal returns the five headers PayPal sends, and the body bytes, so
// tests can fire HTTP against the running server. The verification endpoint
// is whatever httptest server the test wires up in Verify.
func signPaypal(transmissionID string) (map[string]string, []byte) {
    body := []byte(`{
        "id": "WH-"` + transmissionID + `",
        "event_type": "PAYMENT.CAPTURE.COMPLETED",
        "resource": {"id": "CAPTURE-1", "custom_id": "order-uuid"}
    }`)
    h := map[string]string{
        "PAYPAL-AUTH-ALGO":         "SHA256withRSA",
        "PAYPAL-CERT-URL":          "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-360caa42-fca2ab1b-abcdef",
        "PAYPAL-TRANSMISSION-ID":   transmissionID,
        "PAYPAL-TRANSMISSION-SIG":  "transmission-sig-stub",
        "PAYPAL-TRANSMISSION-TIME": time.Now().UTC().Format(time.RFC3339),
    }
    return h, body
}
```

Add `"time"` to imports if missing.

- [ ] **Step 3: Wire `mv.Paypal` to a stub that hits an httptest verify server**

Find the setup block that constructs `middleware.MultiChannelVerifier` for the e2e harness. Append a new field + assignment using `httptest.NewServer`:

```go
    var paypalVerify *httptest.Server
    paypalVerify = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // In real PayPal, this would re-verify the signature. Our E2E only
        // cares that the verifier makes the round-trip and parses the body.
        w.Header().Set("Content-Type", "application/json")
        fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
    }))
    defer paypalVerify.Close()
    mv.Paypal = &middleware.PaypalVerifier{
        SandboxWebhookID: e2ePaypalWebhookIDSandbox,
        LiveWebhookID:    e2ePaypalWebhookIDLive,
        SandboxAPIBase:   paypalVerify.URL,
        LiveAPIBase:      paypalVerify.URL,
        Env:              e2ePaypalEnv,
        HTTPClient:       &http.Client{Timeout: 2 * time.Second},
    }
```

Add `"net/http/httptest"` to imports of `testhelpers.go` if missing.

- [ ] **Step 4: Compile-check the package**

```bash
go build ./tests/e2e/...
```

Expected: success.

- [ ] **Step 5: Commit**

```bash
git add tests/e2e/testhelpers.go
git commit -m "test(e2e): add PayPal payload + signing fixtures"
```

---

## Task 13: E2E — happy path + renewal + refund + bad signature

**Files:**
- Create: `tests/e2e/paypal_test.go`

- [ ] **Step 1: Set up the test file scaffold**

Create `tests/e2e/paypal_test.go`:

```go
package e2e

import (
    "net/http"
    "testing"

    "github.com/yunhou/users/internal/middleware"
)

// TestE2E_Paypal_CaptureCompleted_HappyPath posts a PAYMENT.CAPTURE.COMPLETED
// event and asserts that a payment row is created and the order is paid.
func TestE2E_Paypal_CaptureCompleted_HappyPath(t *testing.T) {
    srv := setupE2EServerWithVerifier(t)

    // 1. Create a user, a plan, an order.
    userID, planID, orderID := paypalSeedOrder(t, srv)

    // 2. Send the webhook.
    headers, body := signPaypal("tid-capture")
    headers, body = setResourceCustomID(headers, body, orderID)
    resp := doRequestWithHeaders(t, srv.Engine, http.MethodPost,
        "/webhooks/payment/paypal", body, headers)
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("want 200, got %d", resp.StatusCode)
    }

    // 3. Assert payment row created.
    p := paypalMustFindPayment(t, srv.DB, "paypal", "CAPTURE-1")
    if p.Status != "paid" || p.OrderID != orderID {
        t.Errorf("payment not created correctly: %+v", p)
    }

    _ = userID
    _ = planID
}

// TestE2E_Paypal_Renewal_ExtendsExpiresAt posts PAYMENT.SALE.COMPLETED and
// asserts that subscriptions.expires_at is updated.
func TestE2E_Paypal_Renewal_ExtendsExpiresAt(t *testing.T) {
    srv := setupE2EServerWithVerifier(t)
    userID, _, subID := paypalSeedActiveSubscription(t, srv)

    headers, body := signPaypalRenewal(subID)
    resp := doRequestWithHeaders(t, srv.Engine, http.MethodPost,
        "/webhooks/payment/paypal", body, headers)
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("want 200, got %d", resp.StatusCode)
    }

    // Assert subscription.expires_at > original seed value.
    expiresAt := paypalMustSubExpiresAt(t, srv.DB, subID)
    if !expiresAt.After(paypalSeedInitialExpiry) {
        t.Errorf("expires_at not advanced: %v", expiresAt)
    }

    _ = userID
}

// TestE2E_Paypal_BadSignature_400 posts PayPal-style headers but with the
// mock verify server returning FAILURE — asserts 400.
func TestE2E_Paypal_BadSignature_400(t *testing.T) {
    srv := setupE2EServerWithVerifierBadVerify(t) // override verify server to return FAILURE

    headers, body := signPaypal("tid-bad")
    resp := doRequestWithHeaders(t, srv.Engine, http.MethodPost,
        "/webhooks/payment/paypal", body, headers)
    if resp.StatusCode != http.StatusBadRequest {
        t.Errorf("want 400, got %d", resp.StatusCode)
    }
}

// TestE2E_Paypal_ChannelDisabled_404 asserts that the middleware returns 404
// when no PayPal verifier is configured (covers the empty-secret path).
func TestE2E_Paypal_ChannelDisabled_404(t *testing.T) {
    srv := setupE2EServerWithoutChannel(t, "paypal")
    headers, body := signPaypal("tid-disabled")
    resp := doRequestWithHeaders(t, srv.Engine, http.MethodPost,
        "/webhooks/payment/paypal", body, headers)
    if resp.StatusCode != http.StatusNotFound {
        t.Errorf("want 404, got %d", resp.StatusCode)
    }
    _ = middleware.ErrUnsupportedChannel
}
```

(The helper functions `paypalSeedOrder`, `paypalSeedActiveSubscription`, `signPaypalRenewal`, `paypalMustFindPayment`, `paypalMustSubExpiresAt`, `setResourceCustomID`, `doRequestWithHeaders`, `setupE2EServerWithoutChannel` need to be written in this same file. Look at `webhooks_test.go::setupE2EServerWithVerifier` for the reference, and to `payments_test.go` for `setupE2EServer` and seed helpers. Match the existing naming style.)

- [ ] **Step 2: Run the e2e tests**

```bash
make e2e
```

Expected: PASS. (If they fail, fix the helpers to match existing e2e patterns; do NOT refactor the test setup.)

- [ ] **Step 3: Commit**

```bash
git add tests/e2e/paypal_test.go
git commit -m "test(e2e): cover PayPal happy path + renewal + bad signature + 404"
```

---

## Task 14: Docs — sync channel list

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`
- Modify: `docs/api-integration-guide.md`
- Modify: `docs/plans/2026-06-23-payment-webhook-mechanism.md`

- [ ] **Step 1: Update `CLAUDE.md`**

Two edits:

a. Add 5 rows to the "Required Environment Variables" table after the `LEMONSQUEEZY_WEBHOOK_SECRET` row:

```markdown
| `PAYPAL_ENV` | No | `live` | `sandbox` \| `live`; selects which webhook_id/API base is active |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | No | (empty) | PayPal sandbox webhook ID; empty = sandbox disabled |
| `PAYPAL_WEBHOOK_ID_LIVE` | No | (empty) | PayPal live webhook ID; empty = live disabled |
| `PAYPAL_API_BASE_SANDBOX` | No | `https://api-m.sandbox.paypal.com` | |
| `PAYPAL_API_BASE_LIVE` | No | `https://api-m.paypal.com` | |
```

b. Update the migration-order sentence in "Development Commands":

> "Database migration: apply `001_init.sql`, then `002_simplify_plans.sql`, then `003_payments.sql`, then `004_ls_channel.sql`, then `005_paypal_channel.sql`, then `006_paypal_sub_mapping.sql` (each depends on the prior; running out of order fails)."

- [ ] **Step 2: Update `README.md`**

Find the existing LemonSqueezy row in any "Supported payment channels" list. Add PayPal alongside.

- [ ] **Step 3: Update `docs/api-integration-guide.md`**

Find the channel list near the top and add PayPal in alphabetical order.

- [ ] **Step 4: Update `docs/plans/2026-06-23-payment-webhook-mechanism.md`**

a. Find the §6 retry-semantics table and add a PayPal row mirroring the LemonSqueezy row:

```
| PayPal | verify-webhook-signature HTTP (5xx → retry; FAILURE → 200 ack eventual dedup) | n/a (no local replay window) |
```

b. Append a `### PayPal` sub-section after the existing `### LemonSqueezy` section documenting:

- Signature header list (5 headers)
- Verification flow (POST → SUCCESS/FAILURE)
- Event-type → handler mapping table (mirror §3 of the spec)
- Dedup note (event-level)
- Renewal branch (synthetic order + payments insert + expires_at extend)

- [ ] **Step 5: Run `make lint`**

```bash
make lint
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md README.md docs/api-integration-guide.md docs/plans/2026-06-23-payment-webhook-mechanism.md
git commit -m "docs: document PayPal channel (signature, event mapping, dedup, renewal)"
```

---

## Self-review checklist

Before declaring done, verify against the spec acceptance criteria (§11):

- [ ] Migrations `005` and `006` apply without manual intervention
- [ ] `go test ./...` passes (unit)
- [ ] `make e2e` passes
- [ ] `make lint` clean
- [ ] `make build` clean
- [ ] With `PAYPAL_ENV=live` and `PAYPAL_WEBHOOK_ID_LIVE=` unset, `POST /webhooks/payment/paypal` returns 404
- [ ] Env toggle between sandbox + live flips which base URL the verifier POSTs to

If any checkbox fails, **fix before pushing** — do not push a partial.
