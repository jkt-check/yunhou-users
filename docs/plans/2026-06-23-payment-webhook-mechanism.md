# Payment Webhook Mechanism

Companion to [`2026-06-16-user-system-design.md`](./2026-06-16-user-system-design.md). Explains how yunhou-users receives, verifies, and reconciles state from payment-channel webhooks.

## 1. Why a webhook is the trust anchor

The frontend telling the backend "I'm paid" is not a fact — any client can fake it, network packets get lost, browser tabs get closed mid-payment. If we trusted the frontend, paid subscriptions would be free.

A payment **channel webhook** is different. It is an HTTP POST sent by Stripe / WeChat Pay / Alipay — servers they operate, signed with a secret only they and we know. It arrives independent of the user's browser. **The webhook is the source of truth** for whether money actually moved.

Everything else in the system — frontend SDK callbacks, our `POST /payments/:id/confirm` endpoint, polling the order status — exists to give the user a fast experience, not to establish truth.

## 2. End-to-end flow

```
                 ┌────────────────────────────────────────┐
                 │  Consumer app frontend                 │
                 │  1. user picks a paid plan             │
                 │  2. POST /payments/orders → yunhou      │
                 │  3. receive order_id + amount          │
                 │  4. open Stripe.js / WeChat Pay SDK    │
                 │  5. user pays in SDK                   │
                 │  6. SDK callback → "payment succeeded" │
                 │  7. POST /payments/:id/confirm (fast)   │
                 └────────────────────────────────────────┘
                                       │
                          ┌────────────┴────────────┐
                          ▼                         ▼
       ┌──────────────────────────────┐   ┌───────────────────────────────┐
       │  yunhou-users                │   │  Stripe / WeChat Pay / Alipay │
       │  • mark payment = "pending"  │   │  • charge the user            │
       │  • wait for webhook          │◀──│  • send webhook POST          │
       │  • webhook arrives           │   │    /webhooks/payment/:channel │
       │  • verify HMAC signature     │   └───────────────────────────────┘
       │  • dedupe on external_txn_id │
       │  • transition:               │
       │    payments.status = paid    │
       │    orders.status    = paid   │
       │    subscriptions.status=active (or extend)
       └──────────────────────────────┘
```

**Two paths, one truth.** Path A (frontend `confirm`) is a low-latency fast-track. Path B (channel webhook) is the authoritative settlement. They can arrive in any order, and both write to the same payment row keyed by `external_txn_id`.

### Why dual paths?

- If we relied on `confirm` only: a user who closes the tab after paying has no subscription even though money moved.
- If we relied on webhook only: the user waits 1–30 seconds (sometimes minutes) staring at a spinner for "nothing happened" feedback.

The fast-track makes the UX feel instant; the webhook makes the data correct under all failure modes.

## 3. Endpoint contract: `POST /webhooks/payment/:channel`

### Request

```
POST /webhooks/payment/stripe
Content-Type: application/json
Stripe-Signature: t=<timestamp>,v1=<hmac_sha256_hex>     ← per-channel header

{
  "id": "evt_xxx",
  "type": "payment_intent.succeeded",
  "data": { "object": { ... full PaymentIntent ... } }
}
```

**Where the order_id comes from.** The webhook payload has no native concept of our `orders.id`. The handler needs `order_id` to run `UPDATE orders SET status='paid'` and to insert the `payments` row. The contract: **when the frontend opens the channel SDK (creating the Stripe PaymentIntent, the WeChat JSAPI order, or the Alipay trade), it must embed `order_id` into the channel-specific metadata field**:

- **Stripe**: `payment_intent.metadata.order_id` (set when the frontend calls `stripe.paymentIntents.create` on its side).
- **WeChat Pay v3**: `out_trade_no` IS the order_id (frontend sets this when calling `/v3/pay/transactions/jsapi`). After decryption, read directly from the payload.
- **Alipay**: `out_trade_no` IS the order_id (set when calling `alipay.trade.page.pay` / `alipay.trade.app.pay`). Read from the form params.

This is enforced by **the frontend, not by us** — we cannot inject metadata into a PaymentIntent we did not create. The contract documentation lives in the design doc's `POST /payments/orders` and frontend SDK integration guide; the webhook handler just trusts what the channel echoes back and looks it up. If `order_id` from metadata doesn't resolve to an `orders` row, the handler writes an `audit_log` row tagged `webhook_for_unknown_order` and returns 404 — the channel will retry, and ops can investigate.

### Response contract

We respond **fast** (target < 500ms). DB errors are surfaced as `500` so the channel retries — we do NOT async-defer the work, because the only safe place to commit a payment state change is inside a transaction that we know committed.

| Channel outcome                  | Our HTTP response                |
|----------------------------------|----------------------------------|
| Signature valid, dedupe hit      | `200 OK` `{"code":0, "data":{"received":true,"domain_action":"...","duplicate":true}}` |
| Signature valid, processed       | `200 OK` `{"code":0, "data":{"received":true,"domain_action":"payment_paid","duplicate":false}}` |
| Signature valid, transient DB error | `500` (channel retries)       |
| Signature **invalid**            | `400` (channel logs; no retry)   |
| Timestamp outside tolerance window | `400` (replay protection)     |
| Unknown channel                  | `404` (path is checked before signature middleware) |

The webhook response uses the standard CLAUDE.md envelope (`{"code": int, "data": object|null, "message": string|null}`); `domain_action` (`"payment_paid"` / `"payment_failed"` / `"refund_paid"` / `"payment_disputed"` / `"payment_dispute_closed"` / `"none"` when an action ran, **empty string on dedupe hit**) and `duplicate` (`true` on dedupe hit) are nested under `data` for ops/dashboard introspection. Consumers should branch on `duplicate === true` to identify dedupe hits; `domain_action === ""` is the dedupe signature (an uninteresting event type with `duplicate: false` reports `"none"` instead). Channels ignore the envelope body and only check the HTTP status code, so this matches the standard surface for human-facing consumers.

**Transaction boundary**: every webhook handler does its work inside a single SQL transaction (event-level insert + payment upsert + subscription upsert + order update). The HTTP response is sent **only after** `COMMIT` returns. If the transaction fails for any reason, we return `500`; the channel retries; on retry we re-run the same idempotent INSERTs/UPDATEs.

### Rate limiting

Webhook endpoints get their own rate-limit bucket (separate from `/auth/*` and `/admin/*`), looser (e.g. 200/s burst 400), because:

- A channel may burst-retry on transient failures
- Webhooks originate from a small, known set of IPs we can allowlist

We also maintain a separate IP allowlist per channel so random internet noise hitting the endpoint can't trigger DB lookups.

### 3.1 Transport requirements

- **HTTPS is mandatory.** Stripe, WeChat Pay v3, and Alipay all reject `http://` callback URLs. Self-signed certs are not accepted by Stripe or Alipay. Use Let's Encrypt or a CA-issued cert.
- **mTLS may be required by WeChat Pay v3** depending on merchant onboarding configuration. Confirm with merchant operations at setup time. If enabled, we need to present a client cert during the TLS handshake; configure this in the reverse proxy (Caddy / nginx) rather than the Go app.
- **TLS / mTLS is terminated at a reverse proxy** (Caddy / nginx / cloud LB). yunhou-users speaks plain HTTP behind the proxy; we don't manage certs in the Go process. This is an operational assumption — the deploy environment owns cert rotation.
- **Public callback URL** — the URL must be reachable from the channel's servers. Localhost / internal-only addresses won't work. For staging, use a public tunnel (ngrok, Cloudflare Tunnel) or a publicly-resolvable staging domain.
- **URL stability** — channels cache the registered callback URL per merchant. URL changes require a merchant console re-registration, which can take minutes to hours to propagate. Don't change the URL without coordination.

## 4. Signature verification

Each channel has its own scheme. We must implement them all — never trust the body without verifying.

### 4.1 Stripe

- Header: `Stripe-Signature: t=<unix_ts>,v1=<hmac>`
- Signed payload: `t + "." + raw_body`
- Algorithm: `HMAC-SHA256` with `STRIPE_WEBHOOK_SECRET`
- **Timestamp tolerance**: reject if `|now - t| > 300s` (replay protection)
- **Verification library**: `github.com/stripe/stripe-go/v76/webhook` provides `ConstructEvent(payload, header, secret)` which does both timestamp + signature checks

```go
event, err := webhook.ConstructEvent(rawBody, sigHeader, s.stripeSecret)
if err != nil { return 400 }
```

### 4.2 WeChat Pay v3

**Two-stage decode**: the webhook body itself is signed but the *business payload* inside it is encrypted.

**Stage 1 — verify the HMAC signature on the raw body** (replay protection via the timestamp header):

- Headers we care about:
  - `Wechatpay-Signature: <base64 hmac>` — the signature itself
  - `Wechatpay-Timestamp: <unix_ts>` — for replay window check
  - `Wechatpay-Nonce: <random>` — bound into the signed string
  - `Wechatpay-Serial: <cert_serial>` — which WeChat platform cert was used; for cert rotation tracking
- Signed string format: `timestamp\n\nnonce\n\nbody\n\n` (literal newlines)
- Algorithm: `HMAC-SHA256` with `WECHAT_PAY_API_V3_KEY`, base64-encoded
- Reject if `|now - timestamp| > 300s` BEFORE doing the HMAC compute

```go
toSign := ts + "\n" + nonce + "\n" + body + "\n"
mac := hmac.New(sha256.New, []byte(apiV3Key))
mac.Write([]byte(toSign))
expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(sigHeader)) { return 400 }
```

**Stage 2 — decrypt `resource` to get the business payload.** The body parses as JSON:

```json
{
  "id": "...",
  "create_time": "...",
  "resource_type": "encrypt-resource",
  "event_type": "TRANSACTION.SUCCESS",
  "summary": "...",
  "resource": {
    "ciphertext": "...",
    "associated_data": "transaction_event",
    "nonce": "..."
  }
}
```

`resource.ciphertext` is **AES-256-GCM encrypted** with the same `WECHAT_PAY_API_V3_KEY`. Pseudocode:

```go
block, _ := aes.NewCipher([]byte(apiV3Key))
gcm, _ := cipher.NewGCM(block)
plaintext, err := gcm.Open(nil,
    []byte(resource.Nonce),
    base64Decode(resource.Ciphertext),
    []byte(resource.AssociatedData),
)
if err != nil { return 400 }
// plaintext is JSON: { "transaction_id": "...", "amount": { "total": 100, ... }, ... }
```

**Only after both stages can you read `transaction_id`, `amount`, etc.** Many failed WeChat integrations stop at Stage 1 and never realize the business fields are encrypted — they end up dedup'ing on the wrong key.

**Cert rotation**: `WECHAT_PAY_API_V3_KEY` is rotated via the WeChat merchant console (no fixed schedule; ~yearly). Rotation is graceful — old key continues to verify old signatures for a grace window, then fails. Operationally: when verification suddenly fails for known-good payloads, suspect a key rotation. Re-fetch from console, never store secrets in code.

### 4.3 Alipay

Alipay's signature scheme is **fundamentally different** from Stripe/WeChat — it's **asymmetric**, and the canonical-string construction has more footguns than the other two.

**Algorithm**: RSA2 (SHA256WithRSA). The notification body is signed by **Alipay using their private key**; we verify using **Alipay's public key** (downloaded from the merchant console). NOT the merchant's key — there's nothing for the merchant to sign with here.

**Canonical string construction** (the part that breaks naive implementations):

1. Take all received params from the notification form body
2. **Exclude** `sign` and `sign_type` from the set
3. Sort remaining params alphabetically by key
4. URL-encode each `key=value` (Alipay-specific encoding: space → `%20`, not `+`; certain Chinese characters have specific handling — see Alipay's official sample notifications)
5. Concatenate with `&`

```go
// Pseudocode — actual implementation must follow Alipay's documented canonicalization
keys := sortedKeys(formParams)
parts := make([]string, 0, len(keys))
for _, k := range keys {
    if k == "sign" || k == "sign_type" { continue }
    parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(formParams[k]))
}
canonical := strings.Join(parts, "&")
```

**Verification**:

```go
// 1. Parse PEM public key (Alipay publishes both PKCS#1 and PKCS#8; support both)
pubKey, err := parseAlipayPublicKey(alipayPublicKeyPEM)

// 2. SHA256 + RSA verify
hashed := sha256.Sum256([]byte(canonical))
err = rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hashed[:], signBytes)
if err != nil { return 400 }
```

**Pitfalls**:

- **`sign_type` is variable** — `RSA2`, `RSA` (legacy SHA1), or `MD5` (very legacy). Branch on it; reject anything other than `RSA2`. If `sign_type` is missing, default to `RSA2` per current Alipay spec, but log loudly — that case shouldn't happen for modern integrations.
- **Public key rotation** is silent. Alipay can rotate their public key (typically yearly); if our cached key is stale, every notification fails verification. Solutions:
  - Periodically (e.g. hourly) re-fetch the public key from Alipay's open API endpoint (`alipay.open.app.alipaycert`)
  - OR monitor for verification failures and alert
- **PKCS#1 vs PKCS#8** — Alipay has historically published both. Newer console output is PKCS#8; older docs reference PKCS#1. Support both via `pem.Decode` + a tag on the DER structure.
- **Encoding edge cases** — Chinese characters, plus signs, equals signs in values. Alipay's URL encoding is not identical to `net/url`'s default. **Read the official sample notifications and test against them**, not against the library's readme.
- **Library caveat**: `github.com/smartwalle/alipay` covers ~80% but lags on edge cases (encoding changes, new key formats). **Plan for an audit pass before going live** — write a unit test per documented Alipay sample notification. The library is a starting point; verification logic for production should be reviewed by someone who's actually read the Alipay signing spec.

**Recommended test vectors** (golden samples to lock in unit tests):
- `TRADE_SUCCESS` for a CNY payment with non-ASCII merchant order ID
- `TRADE_CLOSED` partial refund
- `TRADE_CLOSED` full refund where refund amount exactly equals original

### 4.4 General principles

- **Use the raw body, not a re-serialized JSON** — re-serializing changes whitespace and breaks the HMAC. Buffer the body before JSON parsing.
- **Compare with `hmac.Equal`** (constant-time) — never `==`.
- **Verify timestamp window** before the HMAC; reject replays cheaply.
- **Per-channel secret** stored in env vars, loaded at startup. No hot-reload needed.

### 4.5 LemonSqueezy

LemonSqueezy is the only channel that does **not** include a timestamp in the body or headers — its signature scheme is a raw-body HMAC only.

- Header: `X-Signature: <hex_hmac_sha256>`
- Algorithm: `HMAC-SHA256` with `LEMONSQUEEZY_WEBHOOK_SECRET`
- Signed payload: **raw request body** (no `t.` prefix, no timestamp)
- Output: hex digest (lowercase, no prefix)

**No replay window.** LS doesn't give us a timestamp to anchor a window. We rely on `webhook_events.UNIQUE(channel, event_id)` for replay protection (see §5.7) — that UNIQUE constraint catches the same event whether replayed 30 seconds or 30 days later. The 5-minute window used by Stripe/WeChat/Alipay is therefore an artifact of those channels' signature specs, not a baseline we must mirror.

If we ever need to add a window (e.g. for compliance with a channel change), the only safe source would be `data.attributes.created_at` on the resource — but that's the resource-creation time, not the delivery time, so catch-up deliveries would falsely look stale. Don't add a window without confirming LS's stance on delivery timing.

```go
// internal/middleware/webhook_sig.go: ~LemonsqueezyVerifier
mac := hmac.New(sha256.New, v.Secret)
mac.Write(body) // raw body — DO NOT re-serialize
if !hmac.Equal(expectedHex, mac.Sum(nil)) {
    return ErrInvalidSignature
}
```

## 5. Idempotency

A single payment event will arrive multiple times. Channels retry on non-2xx, network glitches, our own restart mid-processing, etc. We must dedupe at **two distinct layers**:

### 5.1 Event-level dedup: "don't process the same webhook twice"

Every retry of the same logical event must produce no extra work. We achieve this with a `webhook_events` audit table keyed on the channel's event ID.

| Channel | Event-level key              | Notes                                                                  |
|---------|------------------------------|------------------------------------------------------------------------|
| Stripe  | `event.id` (e.g. `evt_xxx`)  | Identical across retries of the same event                             |
| WeChat  | `notify_id`                  | **Per-event, stable across WeChat's own retries.** Different from `transaction_id`, which is per-payment (see §5.2). |
| Alipay  | `notify_id`                  | Stable per event; sent as a top-level form param                       |

**Schema**:

```sql
CREATE TABLE webhook_events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel         TEXT NOT NULL,
    event_id        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at    TIMESTAMPTZ,
    raw_payload     JSONB NOT NULL,
    UNIQUE (channel, event_id)
);
```

The webhook handler does `INSERT ... ON CONFLICT DO NOTHING RETURNING id`; if empty, we've seen this event before — ack 200 and move on.

### 5.2 Business-level dedup: "link the event to a payment row"

Once we've decided to process the event, we need to find or create the `payments` row. The ID here is the channel's **business** identifier (the thing the channel calls "the payment"), distinct from the event ID.

| Channel | Business-level key (`payments.external_txn_id`) | Notes                                                                                  |
|---------|------------------------------------------------|----------------------------------------------------------------------------------------|
| Stripe  | `payment_intent.id` (e.g. `pi_xxx`)            | Multiple events per PaymentIntent (succeeded / refunded / dispute) — all link to the same row |
| WeChat  | `transaction_id`                               | Read from inside the AES-256-GCM decrypted `resource` block, NOT the top-level body    |
| Alipay  | `trade_no`                                     | Top-level form param; equivalent to Stripe's PaymentIntent ID                          |

**Schema constraint**: `UNIQUE (channel, external_txn_id)` on `payments`. The handler does:

```sql
INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status)
VALUES ($1, $2, $3, $4, $5, 'pending')
ON CONFLICT (channel, external_txn_id) DO NOTHING
RETURNING id;
```

If `RETURNING id` is empty, the payment row already exists — re-read it and apply the event's state transition idempotently. If a new id is returned, this is a fresh payment attempt — proceed with activation.

**Additional schema constraint** (prevents two `paid` payments on one order):

```sql
CREATE UNIQUE INDEX idx_payments_one_paid_per_order
    ON payments(order_id) WHERE status = 'paid';
```

This enforces the design rule "one Order → at most one successful Payment" at the database layer. Concurrent webhook + confirm races that would otherwise produce two `paid` rows for the same order fail at the index; one wins, the other treats it as "already done".

### 5.3 Side-effect idempotency — subscription activation

Activating the subscription must be idempotent under:
- Two concurrent webhooks (Stripe retry + new event)
- Webhook + frontend `confirm` racing

The activation flow is a **UPSERT** (INSERT-or-UPDATE), not a pure UPDATE, because the user might have **no subscription row at all** (new user, first paid purchase). Use a single SQL transaction wrapping:

1. `INSERT INTO webhook_events ... ON CONFLICT DO NOTHING` (event-level gate)
2. `INSERT INTO payments ... ON CONFLICT DO NOTHING` (business-level gate, returns existing id if dedupe hit)
3. **Subscription activation** — UPSERT semantics, **single-row target**:
   ```sql
   -- Step 1: UPDATE the target row.
   -- Target = the active row if any (plan upgrade), else the most recent non-active row
   -- (reactivation / recovery from expired/cancelled). ORDER BY ensures we pick exactly ONE row,
   -- even if the user has multiple historical sub rows (e.g. cancelled + expired).
   -- Without the LIMIT, the UPDATE would flip multiple non-active rows to 'active' simultaneously
   -- and trip the partial unique index `UNIQUE(user_id) WHERE status='active'`.
   UPDATE subscriptions SET
       plan_id = $plan_id,
       started_at = now(),
       expires_at = $expires_at,
       status = 'active'
   WHERE id = (
       SELECT id FROM subscriptions
       WHERE user_id = $user_id
       ORDER BY
           CASE WHEN status = 'active' THEN 0 ELSE 1 END,
           created_at DESC
       LIMIT 1
   );

   -- Step 2: INSERT a new active row if no sub existed at all (new user).
   -- ON CONFLICT absorbs the race where another concurrent transaction inserted first;
   -- the conflicting transaction treats it as "already activated" and proceeds.
   INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at)
   VALUES ($user_id, $plan_id, 'active', now(), $expires_at)
   ON CONFLICT (user_id) WHERE status = 'active' DO NOTHING;
   ```
   **Why two statements, not one**: a single `INSERT ... ON CONFLICT` against the partial unique index can only reactivate an existing active row. It cannot reactivate an `expired`/`cancelled` row (those don't match the conflict target). Splitting the activation into "update the latest row, or insert if none" handles all three cases (new user, plan upgrade, recovery) without locking the whole table.

   The first statement handles "sub exists, in any state" — including `active` (re-purchase / plan upgrade), so an upgrade from monthly to yearly rewrites `plan_id`, `started_at`, `expires_at` on the same row. The `subscriptions` history is preserved because we never delete — old sub rows stay as `cancelled` / `expired` if a newer one takes over. (To preserve a record of *which* sub replaced *which*, the `cancelled` rows can carry `superseded_by_subscription_id` — out of scope for v1; v1 preserves the row but not the lineage pointer.)
4. `UPDATE orders SET status='paid' WHERE id=$order AND status IN ('pending', 'expired', 'cancelled')` — covers the normal path AND the "honor late payment" case from §8 (the `expired → paid` transition is encoded here, NOT in the state machine diagram)

The `WHERE status IN (...)` clauses are the guards. If two transactions race, one wins on the row update; the other sees 0 rows affected and treats it as "already done" — no error, no duplicate activation.

### 5.4 Refund concurrency

Refund creation must serialize per-payment to enforce the sum-≤-original invariant. Two concurrent refunds for the same payment each computing "current sum + my refund ≤ payment.amount" without locking will both pass validation and overshoot the limit.

```sql
-- Inside the refund service
BEGIN;
SELECT amount FROM payments WHERE id = $payment_id FOR UPDATE;   -- lock the payment row
-- Count both `paid` and `pending` rows: the prior version excluded
-- `pending` and let N concurrent refunds each pass the check before any
-- of them flipped to `paid`. With pending counted, the next tx waits
-- for the prior lock release and sees the new row in its sum.
SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $payment_id AND status IN ('paid', 'pending');  -- under lock
-- application-level check: SUM + new_amount ≤ payments.amount
INSERT INTO refunds (...) VALUES (...);
COMMIT;
```

The `SELECT FOR UPDATE` on the payment row blocks concurrent refund attempts until our transaction commits; they then see the new sum (including our `pending` insert) and validate against it. Webhook-driven refunds (channel-initiated) take the same path — they also `SELECT FOR UPDATE` the payment row before inserting a refund. `failed` rows are excluded from the sum so a denied refund attempt doesn't permanently block a retry of the same amount.

**Known performance trade-off**: POST /refunds holds the payment row lock for the duration of the channel refund API call (step 4). Stripe's typical latency is <2s but occasionally 30s+. Concurrent refund attempts on the same payment are serialized for that duration. Acceptable for v1 — a primitive service should not introduce async coordination for a low-volume, eventually-consistent flow. If refund throughput becomes a bottleneck, the fix is to release the lock after validation, call the channel API, then re-acquire for INSERT — but that introduces a window where two refunds can both pass the sum check before either INSERTs. We'd then rely on retry-on-unique-violation. Documented as a known constraint; not addressed in v1.

A webhook arriving twice should produce the same end state as a webhook arriving once. No double-charges, no duplicate subscriptions, no phantom refunds.

### 5.5 Refund webhook handler

The payment-success webhook handler (§5.2) is well-covered; the refund webhook handler is its mirror and needs the same rigor. When a `charge.refunded` (Stripe) / `TRANSACTION.REFUND` (WeChat) / `TRADE_CLOSED` (Alipay) event arrives:

```sql
-- Inside the refund webhook service (one transaction, all four steps):
BEGIN;

-- 1. Event-level dedup (same pattern as §5.1 for payments)
INSERT INTO webhook_events (channel, event_id, event_type, raw_payload)
VALUES ($channel, $event_id, $event_type, $raw_payload)
ON CONFLICT (channel, event_id) DO NOTHING
RETURNING id;
-- if no id returned: already processed, COMMIT and return 200.

-- 2. Find the payment row by the channel's transaction ID (NOT the event ID).
--    transaction_id from the decrypted payload maps to payments.external_txn_id.
SELECT id, amount, order_id FROM payments
WHERE channel = $channel AND external_txn_id = $transaction_id;
-- if not found: this refund event has no matching payment — log to audit_log,
-- COMMIT, return 200 (channel retries are pointless; the payment row was never created).
-- In practice this should never happen — POST /refunds always creates the refund row
-- after the channel API call, before the webhook can arrive.

-- 3. Lock the payment row + find-or-create the refund row.
--    external_refund_id comes from the channel's refund object (e.g. Stripe `re.id`).
--    channel is denormalized from payments.channel so the UNIQUE(channel, external_refund_id) can be enforced.
SELECT amount FROM payments WHERE id = $payment_id FOR UPDATE;
INSERT INTO refunds (payment_id, channel, amount, external_refund_id, status)
VALUES ($payment_id, $channel, $refund_amount, $external_refund_id, 'paid')
ON CONFLICT (channel, external_refund_id) DO NOTHING
RETURNING id;
-- if conflict: existing refund row (likely from POST /refunds), no-op INSERT.
-- Re-read it and apply the same status transition idempotently.

-- 4. Apply the data side effect based on whether this is full or partial refund.
--    "Full" = refund_amount == payment_amount. Anything less is partial.
--    See §7 for subscription semantics.
COMMIT;
```

**Recovery role**: this handler is the **recovery path** for the failure mode in POST /refunds where step 5 (INSERT) fails after step 4 (channel API call) succeeds. In that case the original POST /refunds row is missing, but the channel still sends a refund webhook. Step 3 above INSERTs the row directly with `status='paid'` — the channel has already confirmed settlement, so we trust it. The caller that initiated the refund receives 200 only after this webhook lands; an orphan row exists only if the webhook itself fails to deliver (covered by channel retry semantics — see §6).

### 5.6 Defensive transitions

The state machines in §9 are intentionally strict. When the handler encounters a transition the SQL guards don't allow (e.g. webhook reports `paid` for a row already in `failed`):

- **The SQL guard makes the UPDATE a no-op** (0 rows affected).
- **The transaction commits successfully** — we return 200 to the channel so it stops retrying.
- **An `audit_log` row is written** with the original + attempted state, for ops visibility.
- **The handler does NOT raise an error** — silent no-op + audit trail is the right primitive. Escalating to ops alerts is a separate concern (out of scope for this service).

In particular: `failed → paid` is a transition the SQL guard disallows. Stripe does not send `.succeeded` after `.failed` / `.canceled` for the same PaymentIntent. If we observe this pattern, it's an attack (someone forged a webhook) or a channel bug. We log it via `audit_log` with action `unexpected_state_transition` and proceed.

### 5.7 LemonSqueezy event-level dedup

LemonSqueezy payloads **do not carry a top-level unique event ID** — the same subscription emits multiple events (`subscription_created`, `subscription_updated`, `subscription_payment_success`, etc.) and there's no field that uniquely identifies each delivery.

To fit the `webhook_events.UNIQUE(channel, event_id)` invariant, the parser synthesizes an event_id:

```
<event_name>:<data.id>
```

For invoice events (`subscription_payment_*`), `data.id` is the **invoice** ID, not the subscription ID — this is intentional. Two distinct renewal invoices on the same subscription get distinct event_ids, so legitimate follow-on events dedupe independently instead of collapsing.

| Event                                | Resource                  | Synthesized event_id                          |
|--------------------------------------|---------------------------|-----------------------------------------------|
| `order_created`                      | orders                    | `order_created:<LS order ID>`                 |
| `subscription_created`               | subscriptions             | `subscription_created:<LS subscription ID>`   |
| `order_refunded`                     | orders                    | `order_refunded:<LS order ID>`                |
| `subscription_payment_refunded`      | subscription-invoices     | `subscription_payment_refunded:<invoice ID>`  |
| `subscription_payment_failed`        | subscription-invoices     | `subscription_payment_failed:<invoice ID>`    |
| `subscription_payment_success` etc.  | (varied)                  | `<event_name>:<data.id>`                      |

## 6. Retry semantics

| Channel      | Retry policy                                                          |
|--------------|-----------------------------------------------------------------------|
| Stripe       | Up to 3 days, exponential backoff (~hours); can manually retry via dashboard |
| WeChat       | Up to 4 retries over ~24h with backoff; if all fail, merchant must re-initiate |
| Alipay       | Up to 24h with backoff; same notification URL reused                  |
| LemonSqueezy | Up to 5 retries over ~24h with exponential backoff; same URL reused. Configurable per-store in the LS dashboard. |

Implications for us:

- A payment may transition `pending → paid` minutes or hours after the order was created
- The `orders.expires_at` defaults to **30 minutes** after creation (configurable via `ORDER_EXPIRY_DURATION`). A sweeper job flips long-pending orders to `status='expired'`. If a webhook arrives after that, we **honor the payment anyway** — transition `expired → paid` on the order and activate the subscription. The user paid and the channel confirmed it; refusing service creates chargeback risk. See §8 for the full policy and audit logging.
- We do **not** delete webhook payloads; `payments.raw_payload` keeps them for forensics

## 7. Event types we care about

### Stripe

| Event                          | Action                                                |
|--------------------------------|-------------------------------------------------------|
| `payment_intent.succeeded`     | payment → `paid`, order → `paid`, activate sub        |
| `payment_intent.payment_failed`| payment → `failed`, order → `failed`. **If the order was previously `paid` (rare race: confirm fired before this webhook), also flip `orders.status → 'failed'` and deactivate subscription** — see §9 state machine `paid → failed`. |
| `payment_intent.canceled`      | payment → `failed`, order → `failed` (user/system canceled the PaymentIntent before completion). Same cascade rule as `payment_failed` if the order was previously `paid`. |
| `charge.refunded` (full)       | payment → `refunded`, **subscription → `cancelled`**, user reverts to default plan. If a `paid` order exists, also flip `orders.status → 'refunded'`. |
| `charge.refunded` (partial)    | payment stays `paid`, **no subscription change** (we don't prorate in v1). New `refunds` row at status `paid`. |
| `charge.dispute.created`       | payment → `disputed=true`, `disputed_at=now()`; subscription stays active until dispute resolves (chargeback rates climb if we proactively cancel). |
| `charge.dispute.closed` (won)  | payment → `disputed=false`; subscription unchanged. Audit log records the resolution. |
| `charge.dispute.closed` (lost) | handled via the chargeback path — Stripe issues a `charge.refunded` event afterwards. **No separate action here.** |
| **other event types**          | log to `webhook_events` with `processed_at=now()`, no domain action. Stripe sends many event types we don't care about (`payment_method.attached` etc.); ignore them. |

### WeChat Pay v3

| Notification type           | Action                                              |
|-----------------------------|-----------------------------------------------------|
| `TRANSACTION.SUCCESS`       | payment → `paid`, activate sub                       |
| `TRANSACTION.REFUND` (full) | payment → `refunded`, subscription → `cancelled`     |
| `TRANSACTION.REFUND` (partial) | payment stays `paid`, no subscription change      |

(WeChat sends the same callback URL for all event types; we decrypt the `resource` field and dispatch by `event_type` inside. Amount comparison — `refund.amount.total` vs original `amount.total` — determines full vs partial.)

### Alipay

| Notification type                  | Action                                              |
|------------------------------------|-----------------------------------------------------|
| `trade_status_sync` (or `TRADE_SUCCESS` legacy) | payment → `paid`, activate sub           |
| `trade_closed` (full refund, or `TRADE_CLOSED` legacy) | payment → `refunded`, subscription → `cancelled` |
| `trade_closed` (partial refund)    | payment stays `paid`, no subscription change         |

**Note on event_type casing**: real Alipay production traffic uses lowercase
`trade_status_sync` for paid notifications and `trade_closed` for refunds.
The capitalized forms (`TRADE_SUCCESS`, `TRADE_CLOSED`) are accepted as
legacy aliases — both are normalized to the same handler dispatch in
`service.isPaymentSuccess` / `service.isRefundEvent`. When in doubt, match
what the actual channel sends: lowercase for current Alipay, uppercase
for older docs / sample payloads.

### LemonSqueezy

LemonSqueezy sends JSON:API-shaped payloads with `meta.event_name` (also echoed in the `X-Event-Name` header) and the resource under `data.{type,id,attributes}`. Order/Subscription events carry a `meta.custom_data` block we use to receive the Yunhou order_id and the frontend-computed `sub_expires_at`. Subscription-Invoice events (`subscription_payment_*`) do NOT echo `custom_data` — refund lookup happens via `(channel, external_txn_id)` instead.

| Event                                  | Action                                                                          |
|----------------------------------------|---------------------------------------------------------------------------------|
| `order_created`                        | payment → `paid`, order → `paid`, activate sub                                    |
| `subscription_created`                 | payment → `paid`, order → `paid`, activate sub. **Paired with a same-day `order_created` event** (LS docs: *"An `order_created` event will always be sent alongside a `subscription_created` event"*). The first event to arrive inserts the payment row; the second collides on `UNIQUE(channel, external_txn_id)` and is absorbed by `ON CONFLICT DO NOTHING`. Two `webhook_events` rows + one `payments` row is the expected shape — audit-log readers must understand this is by design, not a duplicate. |
| `order_refunded` (full)                | payment → `refunded`, subscription → `cancelled`. Refund row keyed on `data.id` (LS order ID) — matches the originating `order_created` payment row. |
| `order_refunded` (partial)             | payment stays `paid`, no subscription change. New `refunds` row. |
| `subscription_payment_refunded` (full) | payment → `refunded`, subscription → `cancelled`. Refund row keyed on `data.attributes.subscription_id` (LS subscription ID) — matches the originating `subscription_created` payment row. |
| `subscription_payment_refunded` (partial) | payment stays `paid`, no subscription change. |
| `subscription_payment_failed`          | **log to `webhook_events` only, no domain action.** Renewal-failure policy lives in the frontend. |
| `subscription_payment_success`         | **log only, no domain action.** Yunhou never extends `expires_at` from a renewal event — that would require yunhou to implement renewal-window logic, which is product policy. The frontend mirrors renewals via `POST /payments/orders` + `/confirm` (or its own sweeper). |
| `subscription_updated` / `subscription_cancelled` / `subscription_resumed` / `subscription_expired` / `subscription_paused` / `subscription_unpaused` / `customer_updated` / `license_key_*` / `affiliate_activated` | log to `webhook_events` only. The honor-payment policy on orders (already-paid honored even past `expires_at`) provides natural grace; cancelling subscriptions here would be a business-policy violation. |

**Note on `data.attributes.ends_at`**: the Subscription object carries an `ends_at` field (LS's idea of when the subscription period ends). **Do NOT map this to `sub_expires_at`.** The frontend computes `sub_expires_at` from `plan.interval_days` plus business rules (rollover, grace, trial) and embeds it in `meta.custom_data.sub_expires_at` at LS checkout creation. Yunhou records what the frontend told it — never what LS thinks the subscription period should be. Mapping `ends_at` directly would lock yunhou-users into LS's notion of subscription period and prevent frontend business rules (e.g. "monthly = 30 days from activation, not from LS's period start").

### Refund → subscription semantics (primitive, v1)

These rules describe the **data side effects** only. **Who triggers a refund, when it's allowed, how much can be refunded, and whether approval is needed** are all decided by the caller (frontend / admin tooling / payment-service) — not by us. We just provide the primitive operation; the caller composes business policy on top.

- **Full refund** → cancel the subscription immediately. User reverts to the default plan; their JWT scope shrinks on next refresh. The threshold for "full" is `refund.amount == payment.amount`; we don't enforce any other definition. The comparison is done in **integer cents** (DECIMAL(10,2) → int64 × 100) to avoid float64 round-trip drift — an epsilon-based comparison silently mis-classified fee-inclusive refunds.
- **Partial refund** → no subscription change. We do NOT prorate (subtract N days from `expires_at`). Out of scope for v1; explicitly a known limitation.
- **Refund after subscription already expired/cancelled** → still record the refund on the payment row; do NOT touch the (already terminal) subscription.
- **Multiple partial refunds on the same payment** are allowed as separate rows; the sum-≤-original invariant is enforced in the service layer before insert (see design doc Refund table note).

### Subscription expiry (`sub_expires_at`)

The `subscriptions.expires_at` value is a **frontend product decision** (rollover rules, grace periods, trial handling, plan-upgrade stacking) and yunhou-users MUST NOT compute it from `plans.interval_days`. The caller supplies it on each activation path:

| Activation path        | Where the caller sets `expires_at`                                                                                                          |
|------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|
| `POST /payments/orders/:id/confirm` | JSON body field `expires_at` (RFC3339, optional). Omit → never expires.                                                          |
| Stripe webhook         | `data.object.metadata.sub_expires_at` (RFC3339 string). Omit → never expires.                                                              |
| WeChat webhook         | Decrypted `resource.sub_expires_at` (RFC3339 string). Omit → never expires.                                                                |
| Alipay webhook         | Form field `sub_expires_at` (RFC3339 string). Omit → never expires.                                                                         |
| LemonSqueezy webhook   | `meta.custom_data.sub_expires_at` (RFC3339 string, set by frontend at LS checkout). Omit → never expires. ABSENT on `subscription_payment_*` events (refund path doesn't activate subscriptions, so it doesn't need it). |

A malformed (unparseable) `sub_expires_at` is silently dropped to nil rather than failing the webhook — the activation proceeds with `expires_at = NULL` and the subscription never expires. This is a known loud-failure surface: a paid plan with `expires_at = NULL` should never ship; CI / staging tests must assert the field round-trips for monthly/quarterly/yearly plans.

## 8. Failure modes and how we handle them

| Scenario                                          | What happens                                                          |
|---------------------------------------------------|-----------------------------------------------------------------------|
| Webhook arrives before order row exists (race)    | Reject as 404. Frontend will retry `confirm`; user can also re-create order (idempotency key = `external_txn_id`, conflict-resolved). |
| Webhook arrives after we restarted mid-process    | Idempotency key replay: row already exists, re-apply side effects, ack 200 |
| Our DB is down when webhook arrives               | Return 500; channel retries; we recover and process on next attempt  |
| Signature check fails                             | Return 400; **do not** retry, do not log the body (could be forged)   |
| Webhook older than replay window                  | Return 400; payment row not created; user must re-initiate payment    |
| Channel sends `paid` for an order we marked `expired` | **Honor the payment.** Set `payments.status = paid`, transition the order via `expired → paid` (NOT `expired → pending` first) and activate the subscription as normal. The user paid and the channel confirmed it; refusing service here creates chargeback risk. Write an `audit_log` row tagged `late_payment_post_expiry` for ops visibility. **Audit log retention is unbounded in v1** — revisit when storage cost matters. |
| Refund webhook for a payment that's been refunded via admin tool | Idempotent: no-op, ack 200                                        |
| Frontend `confirm` arrives for a payment that webhook marks `failed` | Reject `confirm` with 409; the webhook's `failed` is authoritative |

## 9. State machine

Both `payments.status` and `orders.status` use the same vocabulary: **`pending / paid / failed / refunded`**. Orders additionally have **`cancelled`** and **`expired`**.

### Payment status

```
        ┌──────────┐  confirm OR webhook paid   ┌──────────┐
        │ pending  │ ─────────────────────────▶ │   paid   │
        └──────────┘                            └──────────┘
              │                                      │
              │ webhook (failed/canceled)            │ webhook (refunded)
              ▼                                      ▼
        ┌──────────┐                              ┌──────────┐
        │ failed   │                              │ refunded │
        └──────────┘                              └──────────┘
```

Allowed transitions:
- `pending → paid` — webhook OR frontend `confirm`
- `pending → failed` — webhook (Stripe `.payment_failed` / `.canceled`)
- `paid → refunded` — webhook (`charge.refunded` full)
- `paid → failed` — webhook (rare; bank clawback after `.succeeded`)
- `failed → paid` is **NOT a real transition** — it was an earlier artifact in this doc and has been removed. Stripe does not emit `.succeeded` after `.failed` for the same PaymentIntent; if we see this, it's an attack or a bug.

### Order status

```
                      ┌─────────────────────────┐
                      │       pending           │
                      └─────────────────────────┘
                       ↓       ↓        ↓       ↓
                       │       │        │       │
                  webhook    webhook   user/admin  sweeper
                  paid       failed    cancel      expire
                       │       │        │       │
                       ▼       ▼        ▼       ▼
                  ┌───────┐ ┌───────┐ ┌──────────┐ ┌─────────┐
                  │ paid  │ │failed │ │cancelled │ │expired  │
                  └───────┘ └───────┘ └──────────┘ └─────────┘
                       │                            │
                       │  full refund webhook      │ late webhook (honor)
                       ▼                            ▼
                  ┌──────────┐            (transitions back to `paid`)
                  │ refunded │
                  └──────────┘
```

**Allowed transitions** (enforced by SQL `WHERE status IN (...)` guards in §5.3):

| From        | To          | Trigger                                          |
|-------------|-------------|--------------------------------------------------|
| `pending`   | `paid`      | webhook OR confirm (normal happy path)           |
| `pending`   | `failed`    | webhook (`.payment_failed` / `.canceled`)         |
| `pending`   | `cancelled` | user or admin cancels via `DELETE /payments/orders/:id` |
| `pending`   | `expired`   | sweeper (after `expires_at`)                      |
| `paid`      | `refunded`  | webhook (`charge.refunded` full)                  |
| `expired`   | `paid`      | webhook arrives late — **honor the payment** (§8) |
| `cancelled` | `paid`      | webhook arrives after manual cancel — **honor the payment** (§8) |

Note the last two: `expired → paid` and `cancelled → paid` are not new states — they're the same `paid` state, reachable from terminal states via the honor-payment policy. The §5.3 SQL guards include `'expired'` and `'cancelled'` in the allowed source states for the order update, deliberately.

The refund state machine is documented separately in §7 ("Refund → subscription semantics") and §5.5 ("Refund webhook handler"). Refund rows follow `pending → paid` only; `failed` is reserved for future use (see design doc Refund table note).

Defensive handling of disallowed transitions (e.g. `failed → paid`) is in §5.6: silent no-op + `audit_log` + 200 OK.

## 10. Implementation checklist

When we add this code, the work splits roughly as:

1. **Schema**: `003_payments.sql` with `orders`, `payments`, `refunds` tables; UNIQUE constraint on `(channel, external_txn_id)`
2. **Models**: `internal/model/order.go`, `payment.go`, `refund.go`
3. **Repo**: `internal/repo/{order,payment,refund}_repo.go` with `Create / FindByID / FindByChannelTxn / MarkPaid / MarkFailed / MarkRefunded`
4. **Service**: `internal/service/payment.go` exposing `CreateOrder / OnWebhook(channel, rawBody, signature) / Confirm / Refund`
5. **Handlers**: `internal/handler/payment.go` for user-facing endpoints, `internal/handler/webhook.go` for channel webhooks
6. **Router**: separate `/webhooks/payment/:channel` group with own rate limiter and signature middleware
7. **Signature middleware**: one per channel (`stripe.go`, `wechat.go`, `alipay.go`) registered as middleware factory
8. **Tests**: unit tests for each signature verifier with golden vectors from the channels' docs; integration test that fires a webhook and asserts subscription activation
9. **Env vars**: `STRIPE_WEBHOOK_SECRET`, `WECHAT_PAY_API_V3_KEY`, `ALIPAY_PUBLIC_KEY_PATH` (and per-channel merchant IDs / mch certs)

## 11. What this document deliberately does NOT cover

- **PCI compliance**: irrelevant — we never see card data; channel-encrypted payloads only
- **Multi-currency FX**: handled at channel level; we just store the currency string
- **Tax / invoicing**: out of scope; can be layered later as a separate service that reads from `payments`
- **Subscription proration** on upgrade/downgrade: needs explicit policy; not assumed here