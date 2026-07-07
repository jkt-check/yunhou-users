# PayPal Frontend Integration Guide

This document explains how to integrate PayPal as a payment channel on
top of the yunhou-users backend. It covers both one-time checkout and
subscription / auto-renewal flows.

> **Audience:** frontend engineers. Read this before adding a PayPal
> button to your consumer app.
>
> **Backend ownership boundary:** yunhou-users is the **primitive** that
> records payment events and unlocks subscriptions. **Business policy**
> (refund windows, eligibility, approvals, subscription downgrade
> semantics) lives in the frontend. Don't push product logic into the
> backend; push it into this guide.

---

## 0. Architecture in 30 seconds

```
┌──────────┐       ┌──────────────────────────┐       ┌────────────────┐
│ Frontend │       │ yunhou-users backend      │       │ PayPal REST API │
│ (browser)│       │  POST /webhooks/payment/paypal│  │   v2 + JS SDK  │
└────┬─────┘       └──────────────────────────┘       └────────┬───────┘
     │                                                       │
     │ 1. POST /payments/orders (yunhou)                     │
     │    → returns order_id (UUID)                          │
     │                                                       │
     │ 2. PayPal SDK: create Order (capture) or Subscription│
     │    resource.purchase_units[0].custom_id = order_id   │
     │                                                       │
     │ 3. Buyer pays via PayPal SDK (in browser)             │
     │                                                       │
     │                                       (webhook)        │
     │                                  ┌────────────────────┘
     │                                  ▼
     │   4. POST /webhooks/payment/paypal ← PayPal sends:
     │      - PAYMENT.CAPTURE.COMPLETED  (initial capture, one-time)
     │      - PAYMENT.CAPTURE.REFUNDED   (refunds)
     │      - PAYMENT.SALE.COMPLETED     (subscription renewals)
     │      - BILLING.SUBSCRIPTION.CREATED/UPDATED/CANCELLED (lifecycle)
     │
     │ 5. Frontend polls /user/subscriptions (or gets
     │    `has_access: true/false` from /auth/login → success
     │    means the sub is now active.
```

**Backend does the receiving.** PayPal's webhook is the source of truth:
if the webhook says paid, we marked paid. If the user sees
`has_access: false` in the app, the webhook didn't arrive yet — wait or
poll, don't trust the client-side flow.

---

## 1. SDK + env setup

### 1.1 Load PayPal JS SDK on the page

```html
<script src="https://www.paypal.com/sdk/js?client-id=YOUR_CLIENT_ID&intent=..." />
```

- Use `intent=capture` for one-time checkout.
- Use `vault=true` for subscriptions (PayPal loads buyer-consent flow).
- For sandbox: `client-id=sandbox-client-id`, load from `https://www.sandbox.paypal.com/sdk/js`.

### 1.2 Backend must know these PayPal-side config values

The backend reads them from env (see `cmd/server/main.go::buildWebhookVerifier`
+ `internal/config/config.go`):

| Env var | Notes |
|---|---|
| `PAYPAL_ENV` | `sandbox` or `live`. Both sandbox+live configs are loaded at the same time; this flag selects which is active. |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | The webhook ID PayPal shows you in the sandbox dashboard after you register the webhook URL. Empty = channel returns 404. |
| `PAYPAL_WEBHOOK_ID_LIVE`    | Same, for live. |
| `PAYPAL_API_BASE_SANDBOX`   | Default `https://api-m.sandbox.paypal.com`. |
| `PAYPAL_API_BASE_LIVE`      | Default `https://api-m.paypal.com`. |

Webhook URL to register in the PayPal dashboard: `POST https://your-domain/webhooks/payment/paypal`.
Subscribe to all events (or at minimum: `PAYMENT.CAPTURE.COMPLETED`,
`PAYMENT.CAPTURE.REFUNDED`, `PAYMENT.SALE.COMPLETED`,
`BILLING.SUBSCRIPTION.*`).

---

## 2. Flow A: one-time payment (Checkout-style capture)

### 2.1 Frontend

```ts
// Step 1 — create the Yunhou order (this opens the order slot the
// webhook will resolve back to via custom_id).
const orderRes = await fetch(`${API}/payments/orders`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({ plan_id: 'monthly' }),
});
const { data: { id: orderId } } = await orderRes.json();
// orderId is a UUID. KEEP THIS — PayPal will echo it back via
// custom_id, and that's how the webhook matches the charge to the order.

// Step 2 — create the PayPal Order (REST or SDK). CRITICAL: embed
// the Yunhou orderId in custom_id.
const paypalOrder = await fetch('https://api-m.sandbox.paypal.com/v2/checkout/orders', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${paypalAccessToken}`,
  },
  body: JSON.stringify({
    intent: 'CAPTURE',
    purchase_units: [{
      reference_id: orderId,           // ← already what Yunhou wants
      custom_id: orderId,             // ← also set this — PayPal echoes
                                      //   it back in the webhook's resource
      amount: { currency_code: 'USD', value: '29.90' },
      description: 'Cloud Sync — Monthly',
    }],
  }),
});
// paypalOrder.id is PayPal's order ID ('PAYID-xxx'). Use it to launch
// the SDK buttons.

// Step 3 — render PayPal buttons and call .capture() on click. PayPal
// will fire CAPTURE.COMPLETED to /webhooks/payment/paypal; that triggers
// Yunhou to flip orders.status='paid' and activate the subscription.

// Step 4 — DON'T trust the SDK's onCapture callback as truth. Tell the
// user "Processing your payment…" and poll /user/subscriptions OR
// /payments/orders/<orderId> until orders.status==='paid'. Typical
// latency: 1–5 seconds.
```

### 2.2 Use `reference_id` AND `custom_id`

PayPal echoes `reference_id` and `custom_id` back in different places.
**Set both to the same Yunhou `orderId`** so whichever one survives
schema changes in PayPal, we still resolve the order:

| PayPal field | Where it shows up | Yunhou reads it as |
|---|---|---|
| `purchase_units[0].reference_id` | captured transaction's `reference_id` | (not consumed) |
| `purchase_units[0].custom_id` | webhook's `resource.custom_id` | **`OrderID`** — primary match key |
| `purchase_units[0].payments.captures[0].custom_id` | (less reliable) | backup |

### 2.3 Compute `expires_at` for /payments/confirm — NOT for /webhook

If you're using the **Confirm** path (i.e., the user-facing flow that
calls `POST /payments/orders/:order_id/confirm` with channel=paypal before
the webhook arrives), the caller must supply `expires_at` (RFC3339) in
the request body — Yunhou is **primitive**; it does NOT derive from
`plan.interval_days`. Compute it from your business rules
(rollover / grace / trial / upgrade stacking) and pass it in.

For the PayPal webhook-driven path (recommended — see §2.4), `expires_at`
travels inside the webhook itself: when PayPal sends
`PAYMENT.CAPTURE.COMPLETED` (the initial subscription charge), Yunhou
does **not** consume any sub-expires field. The frontend must set the
expiry via `confirm` if it needs timed sub-expiry; the webhook creates
an active sub with `expires_at=NULL` (never expires) by default.

### 2.4 Live / sandbox selection

If your frontend needs to **lazily discover** whether the backend is in
sandbox, hit `GET /.well-known/jwks.json` (always works) and additionally
check the build of the backend in CI logs — there's no introspection
endpoint for `PAYPAL_ENV` exposed.

For local dev, expose a debug-only endpoint or front the call with an
env-toggle that the frontend reads.

---

## 3. Flow B: subscriptions (auto-renewal)

### 3.1 One-time setup: create the Product + Plan

In PayPal dashboard (or REST once):

```
POST /v1/catalogs/products
  { "name": "Cloud Sync", "type": "SERVICE" }

POST /v1/billing/plans
  { product_id: <above>,
    name: "Monthly",
    billing_cycles: [{ frequency: { interval_unit: "MONTH", interval_count: 1 },
                      tenure_type: "REGULAR", sequence: 1,
                      pricing_scheme: { fixed_price: { value: "29.90", currency_code: "USD" } } }],
    payment_preferences: { auto_bill_outstanding: true } }
```

Save `plan_id` (e.g. `P-5AB12345CD1234567`) — you'll pass it to the JS
SDK on subscription creation.

### 3.2 Frontend subscription flow

```ts
// Step 1 — create Yunhou order (same as Flow A step 1).
const orderRes = await fetch(`${API}/payments/orders`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({
    plan_id: 'monthly',
    expires_at: computeExpiryRFC3339(),  // see §2.3 — frontend decides
  }),
});
const { data: { id: orderId } } = await orderRes.json();

// Step 2 — create the PayPal Subscription via SDK with the plan_id.
// Pass custom_id through the SDK's `custom_id` per-activation.
// PayPal's Billing API accepts custom_id on the subscription itself,
// not on the plan. Capture it on /subscriptions then PUT it via:
//   PATCH /v1/billing/subscriptions/<sub_id>
//     body: { custom_id: orderId }
//
// Why PATCH? Subscription objects don't accept custom_id at creation
// time. You create empty, then patch before the user confirms in the
// SDK.

// Step 3 — render PayPal Buttons with the subscription id; user
// confirms. PayPal fires BILLING.SUBSCRIPTION.CREATED + a paired
// PAYMENT.CAPTURE.COMPLETED for the first cycle. Yunhou handles
// both via the same /webhooks/payment/paypal endpoint.

paypal.Buttons({
  createSubscription: async (data, actions) =>
    actions.subscription.create({
      plan_id: 'P-5AB12345CD1234567',
      custom_id: orderId,  // ← PayPal JS SDK does accept custom_id here.
                            // (REST API doesn't, but the JS SDK's adapter does.)
    }),
  onApprove: async (data, actions) =>
    fetch(`${PAYPAL}/v1/billing/subscriptions/${data.subscriptionID}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify({ custom_id: orderId }),  // belt + suspenders
    }),
  // ...
});
```

### 3.3 Why dual custom_id (SDK + PATCH)

The PayPal v1 Billing REST API's `POST /v1/billing/subscriptions` does
NOT accept `custom_id` on the create call (the field is read-only).
But the JS SDK's `actions.subscription.create()` does include it under
the hood. To be safe across API versions, set it twice — once via the
SDK (covers current PayPal versions) and once via PATCH right after
approval (covers edge cases where the SDK version is older than the
API).

### 3.4 On renewals

PayPal auto-charges the buyer on each cycle and emits
`PAYMENT.SALE.COMPLETED` to your webhook. Yunhou's renewal branch:

1. Looks up the active subscription by `external_subscription_id` (which
   Yunhou stored on activation).
2. Synthesizes a fresh `orders` row (uuid_generate_v4).
3. Inserts a `payments` row keyed on `(channel='paypal', external_txn_id)`.
4. Extends `subscriptions.expires_at` from `resource.billing_info.next_billing_time`
   if PayPal sent it. **If absent, Yunhou does NOT compute one** — the
   frontend must surface a "your plan won't renew" UX. Compute and pass
   via `confirm` if you want guaranteed timed expiry.

---

## 4. Custom_id binding — the one thing you must get right

| Field | Yunhou reads it as | Where PayPal echoes it |
|---|---|---|
| `resource.custom_id` | `OrderID` | One-time: webhook `resource.custom_id`. Subscription: `subscription.custom_id` (PATCH or SDK-embed). |
| `resource.billing_agreement_id` | `ExternalSubscriptionID` (renewal binding) | Subscription + sale events. Subscription CREATED sometimes uses `resource.id` instead — Yunhou handles both via the subscription parser. |

If `custom_id` is missing, Yunhou returns **400** with `paypal missing
resource.custom_id`. Never bypass this — without `custom_id`, Yunhou
can't resolve the charge to an order.

---

## 5. Refunds (v1)

**Yunhou does NOT call PayPal's refund API**. There's a stub
`RefundAPI` that returns `ErrRefundChannelFailed` for v1 — same as for
Stripe / WeChat / Alipay.

If you want refunds to work in v1:

1. Call PayPal's `POST /v2/payments/refunds` directly from your frontend
   or admin tooling using a server-side PayPal API token (NOT from the
   browser — the PayPal access token is admin-only).
2. PayPal sends `PAYMENT.CAPTURE.REFUNDED` to your webhook.
3. Yunhou's `onRefundSucceeded` matches `PAYMENT.CAPTURE.REFUNDED` and
   processes the refund row, with the full/partial distinction:
   full refund → `payments.status='refunded'` + subscription cancelled;
   partial refund → only the new `refunds` row + `payments.status` stays
   `paid` (no subscription change).

Refunds for renewal payments: PayPal sends `PAYMENT.SALE.REFUNDED` for
that case. Yunhou doesn't special-case it — it falls into the same
`isRefundEvent` predicate as `PAYMENT.CAPTURE.REFUNDED`. The lookup keys
on `(channel, external_txn_id)` and PayPal's refund id, so the same
payment row gets its `status` updated.

---

## 6. Subscriptions lifecycle events

`BILLING.SUBSCRIPTION.*` events that don't carry an `amount` block are
acknowledge-only (Yunhou marks the webhook processed but takes no domain
action). This includes:

- `BILLING.SUBSCRIPTION.UPDATED`
- `BILLING.SUBSCRIPTION.CANCELLED`

`BILLING.SUBSCRIPTION.CREATED` is treated as a payment success (it's
paired with the first `PAYMENT.CAPTURE.COMPLETED` of the cycle).

If you need to drive cancellation of a subscription, do it directly via
PayPal's REST API (`POST /v1/billing/subscriptions/<id>/cancel`) and
emit a `DELETE /user/subscriptions/:id` against Yunhou to remove the
local row. The next PayPal webhook (CANCELLED) is just acknowledgement.

---

## 7. Polling vs webhook-driven UX

The webhook is asynchronous. Don't trust the SDK's `onApprove` callback
to tell you the order is paid. Pattern:

```ts
async function pollUntilPaid(orderId, token) {
  for (let i = 0; i < 30; i++) {
    const res = await fetch(`${API}/payments/orders/${orderId}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    const { data: order } = await res.json();
    if (order.status === 'paid') return order;
    await sleep(500);  // 500ms × 30 = 15s max
  }
  throw new Error('payment not confirmed in 15s — possible webhook delay');
}
```

Then redirect the user to their dashboard or show a "Done" state. Never
use SDK callback as the source of truth; it's a UX hint at most.

For subscriptions, your local subscription row is the source of truth on
display — `GET /user/subscriptions` returns it. Don't show "subscribed"
until that endpoint says `status='active'`.

---

## 8. Error handling

| Status from Yunhou | Meaning | What to do |
|---|---|---|
| 400 + `invalid signature` | Webhook signature verification failed or malformed body | Retry on transient; otherwise surface an ops alert. |
| 404 + `unknown channel` | `paypal` channel not configured (env vars missing) | Operator issue. Show "payment temporarily unavailable" to user. |
| 502 + `channel refund API call failed` | Yunhou-side refund stub returned an error | Not relevant to PayPal webhooks. |
| 409 + `order already has a paid payment on a different channel` | Frontend must have submitted the order with `channel=stripe`, PayPal webhook arrives with `channel=paypal`. Don't mix channels per order. |

---

## 9. Sandbox-vs-live

Backend env can have both sandbox and live configured simultaneously;
`PAYPAL_ENV` selects which is active. Deploys don't need to restart
to flip environments — change only `PAYPAL_ENV`.

If your frontend needs to talk to a different PayPal base URL than the
backend is verifying webhooks for, you're doing something wrong — the
backend's verify-mode must match your creation-mode. If the backend is
in sandbox mode, your PayPal JS SDK must also use `client-id=sandbox`
and Create Order / Subscription API calls must hit `api-m.sandbox.paypal.com`.

---

## 10. Quick checklist for first integration

- [ ] PayPal Product + Plan created (for subscriptions only)
- [ ] Yunhou backend env wired: `PAYPAL_ENV`, `PAYPAL_WEBHOOK_ID_*`, `PAYPAL_API_BASE_*`
- [ ] Webhook URL registered in the PayPal dashboard → `POST /webhooks/payment/paypal`
- [ ] All five events subscribed in PayPal: `PAYMENT.CAPTURE.*`, `PAYMENT.SALE.*`, `BILLING.SUBSCRIPTION.*`
- [ ] `custom_id` set on every PayPal Order / Subscription to the Yunhou `orderId`
- [ ] `expires_at` decision: product policy in frontend → `confirm` path; **no** derivation in Yunhou
- [ ] `pollUntilPaid` (or equivalent) on success before showing "paid"
- [ ] Refund path uses PayPal REST directly; Yunhou's onRefundSucceeded handles the inverse webhook
