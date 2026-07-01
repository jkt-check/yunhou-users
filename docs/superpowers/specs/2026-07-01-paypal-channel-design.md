# PayPal Channel — Design Spec

**Date:** 2026-07-01
**Status:** Approved
**Branch:** `feat/paypal-channel`
**Author:** Claude (Yunhou Users)

## 1. Goal

Add PayPal as a fourth payment channel alongside Stripe, WeChat Pay, Alipay, and LemonSqueezy. Webhook ingestion only; refund/charge API surface stays at v2 parity (stubbed, like the other channels' v1).

Everything follows the same layered pattern those channels use: migration → config → verifier → parser → service predicates → wiring → tests → docs. PayPal differs from the others in two physical ways: (1) signature verification requires an outbound HTTPS call instead of pure HMAC, and (2) subscription renewals are an explicit, in-scope event class that the existing `OnWebhook` dispatch is not built to handle — both are addressed below in §3 and §5.

## 2. Scope (decisions locked)

| # | Decision | Rationale |
|---|---|---|
| 1 | **Channel scope:** one-time + subscription-first + subscription-renewal | Includes `PAYMENT.CAPTURE.*`, `BILLING.SUBSCRIPTION.CREATED`, and `PAYMENT.SALE.COMPLETED` (renewal). Renewal is an *expansion* over LemonSqueezy (which never processed `subscription_payment_success`). |
| 2 | **Sandbox + live must coexist** in a single binary | Dev / CI will hit PayPal's sandbox. Two `(apiBase, webhookID)` tuples live in `PaypalVerifier`; `PAYPAL_ENV=sandbox\|live` selects which is used. |
| 3 | **Renewal side effects:** upsert into `payments` (synthetic order UUID) **and** extend `subscriptions.expires_at` | User-facing requirement that auto-renew keeps the subscription alive. Storing the renewal as a payment row maintains the financial trail (same as initial capture). |
| 4 | **Refund API stubbed** in v1 | Matches LS / Stripe / WeChat / Alipay v1 behavior. The `RefundAPI.Refund(channel="paypal", ...)` call returns `ErrRefundChannelFailed`. Real PayPal refund API is v2 work. |
| 5 | **Channel string is `"paypal"` (single)** | No `paypal_sandbox` / `paypal_live` split. Sandbox vs live is a runtime concern of the verifier only. URLs and DB rows are environment-agnostic. |
| 6 | **Order binding via PayPal `custom_id`** | Frontend product decision still lives in the frontend; yunhou-users stays primitive. We set `custom_id` on PayPal Order / Subscription at creation time and the webhook resource echoes it back. |

## 3. Architecture (file map)

Commit-by-commit, mirroring the LS rollout shape:

| # | Commit | Files | Purpose |
|---|---|---|---|
| 1 | `feat(payments): migration 005 — allow channel='paypal'` | `migrations/005_paypal_channel.sql` | Add `'paypal'` to `payments / refunds / webhook_events` CHECK constraints (same shape as `004_ls_channel.sql`). |
| 2 | `feat(payments): migration 006 — subscriptions.external_subscription_id` | `migrations/006_paypal_sub_mapping.sql` | Add `subscriptions.external_subscription_id TEXT` (nullable, **UNIQUE** partial index — `WHERE external_subscription_id IS NOT NULL`). |
| 3 | `feat(config): add PAYPAL_* env vars` | `internal/config/config.go`, `.env.example` | Add `PaypalWebhookIDSandbox`, `PaypalWebhookIDLive`, `PaypalAPIBaseSandbox`, `PaypalAPIBaseLive`, `PaypalEnv`. |
| 4 | `feat(webhook): add PaypalVerifier` | `internal/middleware/webhook_sig.go` | `PaypalVerifier` (HTTP verify) + slot in `MultiChannelVerifier`. |
| 5 | `feat(handler): add parsePaypal` | `internal/handler/webhook.go` | Add `case "paypal"` to `parseEvent` + `parsePaypal` body. |
| 6 | `feat(payment): handle PayPal events + renewal branch` | `internal/service/payment.go` | Add PayPal event names to `isPaymentSuccess / Refund / Failed`, new `isPaypalRenewal` predicate, new `onPaypalRenewalSucceeded` branch, allowlist `"paypal"` in `validateChannel`. |
| 7 | `feat(server): wire PAYPAL_* env vars` | `cmd/server/main.go` | Wire `mv.Paypal` in `buildWebhookVerifier`. |
| 8 | `test(webhook): cover PaypalVerifier + MultiChannelVerifier` | `internal/middleware/webhook_sig_test.go` | `httptest.Server` mock for verify endpoint + MultiChannelVerifier fan-out. |
| 9 | `test(webhook): cover parsePaypal + dispatch` | `internal/handler/webhook_test.go` | Event-type dispatch (CAPTURE / SALE / SUBSCRIPTION). |
| 10 | `test(payment): cover PayPal predicates + renewal path` | `internal/service/payment_test.go` | Predicate matches + renewal handler unit test (mock sub + payment repo). |
| 11 | `test(e2e): PayPal payload + signing fixtures` | `tests/e2e/testhelpers.go`, `tests/e2e/paypal_test.go` | `signPaypal` helper + E2E setup with mock verify endpoint. |
| 12 | `test(e2e): cover PayPal happy path + refund + bad signature` | `tests/e2e/paypal_test.go` | E2E: order, capture, subscription-created, renewal, refund, bad signature. |
| 13 | `docs: document PayPal channel` | `.env.example`, `CLAUDE.md`, `README.md`, `docs/plans/2026-06-23-payment-webhook-mechanism.md` | Same doc shape as `1fee1d9` did for LS. |

## 4. `PaypalVerifier` design

PayPal webhook verification is **not local**. The signature header carries:

```
PAYPAL-TRANSMISSION-ID
PAYPAL-TRANSMISSION-TIME
PAYPAL-TRANSMISSION-SIG
PAYPAL-CERT-URL
PAYPAL-AUTH-ALGO
```

We POST the request body + these headers + our local `webhook_id` to PayPal's REST endpoint:

```
POST {apiBase}/v1/notifications/verify-webhook-signature
{
  "auth_algo": "...",
  "cert_url": "...",
  "transmission_id": "...",
  "transmission_sig": "...",
  "transmission_time": "...",
  "webhook_id": "<our webhook_id for the active env>",
  "webhook_event": <request body JSON>
}
```

PayPal returns `{ "verification_status": "SUCCESS" | "FAILURE" }`. **No replay window** is checked locally; PayPal's window is much longer than LS's. Replay protection via DB event-level dedupe (`webhook_events.UNIQUE(channel, event_id)`) is sufficient, exactly as it is for LS.

### Struct shape

```go
type PaypalVerifier struct {
    HTTPClient     *http.Client          // 5s timeout; nil = http.DefaultClient
    SandboxWebhookID string              // sandbox webhook_id
    LiveWebhookID    string              // live webhook_id
    SandboxAPIBase   string              // default https://api-m.sandbox.paypal.com
    LiveAPIBase      string              // default https://api-m.paypal.com
    Env              string              // "sandbox" | "live"; resolved at startup
}

func (v *PaypalVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error
```

### Verification flow

1. Pull the five PayPal headers out. Missing → `ErrInvalidSignature` (400, channel won't retry — bad headers are a client bug).
2. Pick `webhook_id` / `api_base` based on `Env`. `Env` empty or unknown → constructor returns `nil`, so `MultiChannelVerifier` returns `ErrUnsupportedChannel` (404), matching the empty-secret-disable pattern.
3. Decode `body` as JSON. Decode-fail → `ErrInvalidSignature` (400) — PayPal will not generate an event with invalid JSON; treat as bad-headers.
4. Build request, POST to `{api_base}/v1/notifications/verify-webhook-signature`.
   - Network error / 5xx → return wrapped error (500, retryable).
   - 4xx (other than the rare verify-rate-limit) → treat as 500 retryable.
5. Parse response. `verification_status == "SUCCESS"` → return nil. Anything else → `ErrInvalidSignature`.

### Senitel-error mapping

Already covered by the middleware in `webhook_sig.go`; no change needed there. `ErrInvalidSignature` → 400, network error → 500.

### Two-config wiring

`PaypalVerifier` holds **both** configs. Even if one is empty (e.g., live deployment with no sandbox ID configured), `Env` selects the populated one. Empty `webhook_id` for the active env → startup fails loudly in `buildWebhookVerifier` (logged once, `mv.Paypal` set to `nil`, same "channel returns 404" behavior as a missing Stripe secret).

## 5. `parsePaypal` design

`parsePaypal(raw []byte) (*service.WebhookEvent, error)` lives in `internal/handler/webhook.go`. Shape: dispatch on `event_type`, then a per-type inner extractor.

### PayPal event-type → handler flow

| PayPal event_type | WebhookEvent.EventType | Action |
|---|---|---|
| `PAYMENT.CAPTURE.COMPLETED` | `PAYMENT.CAPTURE.COMPLETED` | `isPaymentSuccess` → `onPaymentSucceeded` (matches Confirm-flow). Order comes from `resource.custom_id` (set by frontend at capture-create time). |
| `PAYMENT.CAPTURE.REFUNDED` | `PAYMENT.CAPTURE.REFUNDED` | `isRefundEvent` → `onRefundSucceeded` (matches LS refund path). |
| `PAYMENT.CAPTURE.DENIED` | `PAYMENT.CAPTURE.DENIED` | `isPaymentFailed` → `onPaymentFailed`. |
| `PAYMENT.CAPTURE.FAILED` | `PAYMENT.CAPTURE.FAILED` | `isPaymentFailed` → `onPaymentFailed`. |
| `BILLING.SUBSCRIPTION.CREATED` | `BILLING.SUBSCRIPTION.CREATED` | `isPaymentSuccess` → `onPaymentSucceeded` (subscription initial charge). Order from `resource.custom_id`. **Side effect (also in `onPaymentSucceeded`):** upsert `subscriptions.external_subscription_id = resource.id`. |
| `BILLING.SUBSCRIPTION.UPDATED` | `BILLING.SUBSCRIPTION.UPDATED` | Acknowledge only (MarkProcessed). No domain action — billing cycle state is captured by `PAYMENT.SALE.COMPLETED` and we let the frontend decide renewal semantics on the .UPDATED event. |
| `BILLING.SUBSCRIPTION.CANCELLED` | `BILLING.SUBSCRIPTION.CANCELLED` | Acknowledge only. Cancellation was already initiated by the channel; mirroring LS disposition, we don't tear down `subscriptions.status` here (the frontend's `DELETE /user/subscriptions/:id` is the primitive). |
| `PAYMENT.SALE.COMPLETED` | `PAYMENT.SALE.COMPLETED` | **Renewal branch:** `isPaypalRenewal` → new `onPaypalRenewalSucceeded`. Look up sub by `external_subscription_id`, mint a synthetic `orders.id = "renewal-<uuid>"`, INSERT `payments` (status=paid), extend `subscriptions.expires_at` from `resource.billing_info.next_billing_time` if present. |
| (anything else) | (echo) | Mark processed, no action, ack 200. |

### Common field extraction

PayPal's `resource` JSON shape varies by event type. The extractor pulls:

- `OrderID` ← `resource.custom_id` (set at PayPal Order / Subscription creation time)
- `TransactionID` ← `resource.id` (PayPal's own ID for the payment capture / sale)
- `Amount` ← `resource.amount.value` (already in major units, no division)
- `Currency` ← `resource.amount.currency_code` → uppercased to match LS handling

For renewal: `ExternalSubscriptionID` ← `resource.billing_agreement_id` and falls back to `resource.id` when `billing_agreement_id` is absent (PayPal legacy).

For refund events: `RefundAmount` ← `resource.amount.value` and `ExternalRefundID` ← `"paypal-" + resource.id` (mirrors LS naming convention).

### Renewal event handling — `onPaypalRenewalSucceeded`

This is the one new code path in the service. It runs inside an `s.db.BeginTxx` transaction:

1. Lookup subscription by `e.ExternalSubscriptionID`. Not found → `writeAudit("webhook_for_unknown_subscription")`, return ack-200 (we have no domain action to take).
2. Lookup the user's existing active or most-recent paid payment for this subscription_id. We don't strictly need it — the renewal payment row will use a synthetic order, not a real one — but if `e.Amount != subscription.recorded_amount` we log the discrepancy to audit and continue. (Pricing changes are out of scope here; the frontend handles tier upgrades.)
3. INSERT a synthetic `orders` row. The DB default `uuid_generate_v4()` fills `id`:
   ```sql
   INSERT INTO orders (user_id, plan_id, amount, currency, status, expires_at)
   VALUES ($user_id, $plan_id, $amount, $currency, 'paid', NULL)
   RETURNING id
   ```
   `orders.status CHECK` already includes `'paid'`. The FK on `payments.order_id` resolves normally — the synthetic order satisfies it the same as a user-created order would.
4. INSERT `payments` (status=paid, paid_at=now()) bound to that synthetic order (use the `id` returned above), `external_txn_id = e.TransactionID`. The `(channel, external_txn_id)` UNIQUE constraint still dedupes re-runs.
5. UPDATE `subscriptions.expires_at` if `e.SubExpiresAt != nil`. Else leave it (frontend's job to plan-expiry).
6. Commit.

### Subscription activation row binding

`onPaymentSucceeded` (called for both `PAYMENT.CAPTURE.COMPLETED` *and* `BILLING.SUBSCRIPTION.CREATED`) needs a small extension: when `e.ExternalSubscriptionID != ""`, upsert `subscriptions.external_subscription_id` on the active row. This is guarded by a UNIQUE partial index, so a second sub for the same PayPal subscription ID is impossible.

```sql
UPDATE subscriptions
SET external_subscription_id = $ext_id
WHERE user_id = $user_id AND status = 'active'
  AND external_subscription_id IS NULL
```

A separate transaction (one SQL statement, no FOR UPDATE) keeps the renewal branch and the activate branch decoupled. Tradeoff: a concurrent `BILLING.SUBSCRIPTION.CREATED` and renewal could race on this column write, but the UNIQUE partial index makes the second one a no-op.

## 6. Migrations

### 005 — channel enum

```sql
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

Verbatim copy of `004_ls_channel.sql` with `'paypal'` added.

### 006 — subscription mapping

```sql
BEGIN;
ALTER TABLE subscriptions ADD COLUMN external_subscription_id TEXT;
CREATE UNIQUE INDEX idx_subscriptions_external_sub_id
    ON subscriptions (external_subscription_id)
    WHERE external_subscription_id IS NOT NULL;
COMMIT;
```

The partial UNIQUE index prevents two active subs from claiming the same PayPal subscription ID. Existing rows are fine — column is nullable.

## 7. Tests

| Layer | What is covered |
|---|---|
| `middleware/webhook_sig_test.go` | `PaypalVerifier.VerifySignature` happy path, missing-header → `ErrInvalidSignature`, network error → wrapped 500-error, `verification_status=FAILURE` → `ErrInvalidSignature`, env selection (sandbox vs live). |
| `handler/webhook_test.go` | `parsePaypal` per event type, missing fields → error, refund event produces `ExternalRefundID = "paypal-<id>"`, renewal event produces `ExternalSubscriptionID`. |
| `service/payment_test.go` | `isPaypalRenewal`, `validateChannel("paypal")` passes. `onPaypalRenewalSucceeded`: happy (sub found, payment inserted, expires_at extended), unknown sub → ack-200 with audit log. |
| `tests/e2e/paypal_test.go` | Happy path: `PAYMENT.CAPTURE.COMPLETED` for one-time + subscription-first → 200, subscription row has `external_subscription_id` set. Renewal: `PAYMENT.SALE.COMPLETED` → 200, new synthetic order + payment row, expires_at advanced. Refund: `PAYMENT.CAPTURE.REFUNDED` → 200, full vs partial handling matches LS path. Bad signature (verify endpoint returns FAILURE) → 400. |
| `tests/e2e/testhelpers.go` | `e2ePaypalClientID` + `e2ePaypalClientSecret` (sandbox, fake), `signPaypal` helper that builds the five headers and signs the body for the local mock-verify endpoint. |

Mock-verify is a small `httptest.NewServer` that mimics PayPal's `verify-webhook-signature` response shape; in production we hit the real PayPal API.

## 8. Docs

Three places to update:

1. `.env.example` — append `PAYPAL_ENV=`, `PAYPAL_WEBHOOK_ID_SANDBOX=`, `PAYPAL_WEBHOOK_ID_LIVE=`, `PAYPAL_API_BASE_SANDBOX=`, `PAYPAL_API_BASE_LIVE=`; add a PayPal row to the "channel webhook secrets" comment.
2. `CLAUDE.md` — append `PAYPAL_*` env vars to the Required Environment Variables table.
3. `README.md` and `docs/api-integration-guide.md` — mention PayPal in the channel list.
4. `docs/plans/2026-06-23-payment-webhook-mechanism.md`:
   - Add a PayPal row to the §6 retry-semantics table.
   - Add a §-sub-section `### PayPal` documenting signature flow + event mapping + dedup. Mirror LS's section in the same file.

## 9. Non-goals / explicit deferrals

- **Real refund API** → v2 (matches every other channel's v1).
- **Real OAuth client_credentials to mint the verify auth** → PayPal's verify endpoint is unauthenticated; no OAuth needed.
- **Frontend PayPal Order creation flow** → frontend product code, not in this repo.
- **Subscription downgrade / plan-swap mid-cycle** → BILLING.SUBSCRIPTION.UPDATED is acknowledged only; the product semantics of a tier swap belong to the frontend.
- **Multi-merchant PayPal** → single merchant per deployment; if multi-merchant lands later, `PaypalVerifier` will get a (merchant_id → keys) lookup map.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| PayPal verify endpoint down: every PayPal webhook returns 500, channel retries pile up. | HTTP client has a 5s timeout. The middleware already returns 500 on transient (non-sentinel) errors — PayPal will retry per its schedule. |
| Renaming `external_subscription_id` later means breaking the migration. | Acceptable: column is rare and not user-visible. The partial UNIQUE index survives name changes. |
| A second PayPal webhook (different env) lands on the same order row. | `payments.channel='paypal'` is constant; the `webhook_events.UNIQUE(channel, event_id)` dedupe is per-event regardless. If sandbox and live ever share a DB, the rows are tagged by `webhook_event.event_id` only, not env — call this out in deployment docs. |
| Renewal happens after a user cancels their subscription. | On a cancelled sub (`status != 'active'`), the renewal branch skips the sub-update and inserts only the payment row, leaving expires_at untouched. Audit-logged. |
| `next_billing_time` is null on `PAYMENT.SALE.COMPLETED`. | Frontend is responsible for projecting expiry. We extend `expires_at` *if* the field is set; otherwise we leave it untouched. |

## 11. Acceptance criteria

- [ ] `migrations/005_paypal_channel.sql`, `migrations/006_paypal_sub_mapping.sql` apply without manual intervention.
- [ ] `go test ./...` passes (unit).
- [ ] `make e2e` passes (Playbook mock verify covers happy path + renewal + refund + bad signature).
- [ ] `make lint` clean (`go vet`).
- [ ] `go build ./...` clean (`make build`).
- [ ] With `PAYPAL_ENV=live` and `PAYPAL_WEBHOOK_ID_LIVE=…` unset, `POST /webhooks/payment/paypal` returns 404 (consistent with empty-secret-disable pattern for other channels).
- [ ] Running with both `PAYPAL_WEBHOOK_ID_SANDBOX` and `PAYPAL_WEBHOOK_ID_LIVE` set: switching `PAYPAL_ENV` between `sandbox` and `live` flips which base URL the verifier POSTs to (verified via a stub-spy in middleware tests).
