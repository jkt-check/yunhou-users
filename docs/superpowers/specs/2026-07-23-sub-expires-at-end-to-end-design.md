# sub_expires_at End-to-End Plumbing — Design Spec

**Date:** 2026-07-23
**Status:** Draft
**Author:** Claude (Yunhou Users + yunhou-website)
**Companion spec:** `2026-07-23-login-subscription-decouple-design.md` (orthogonal — this one fixes the *data channel*; the other fixes the *login-vs-ability coupling*)
**Related incidents:** cn-staging 2026-07-23 (root: `subscriptions.expires_at` has been NULL on every WeChat subscriber since launch; `findUsableSubscription` skips expiry check when NULL, which accidentally masks the missing data path).

## 1. Problem statement

`sub_expires_at` — the future timestamp at which a user's paid subscription should lapse — is the source of truth for `findUsableSubscription`'s "is this login still entitled?" check (and under the `login-subscription-decouple` companion spec, for the *non-login* rendering of `HasAccess=false` in `/auth/me`).

Today the value never reaches the database. The chain:

| Step | Today | Missing |
|---|---|---|
| BFF `POST /apps/:id/quote` response | includes `sub_expires_at: <RFC3339>` computed by yunhou-users `QuoteService` (`service/quote.go:77`) | — |
| BFF reads `quote.sub_expires_at` | yes — `quote.ts:39` types it, `getQuote()` returns it | — |
| BFF passes it into `POST /payments/orders` | **NO** — `/payments/orders` doesn't accept it (`handler/payment.go:51` accepts only `plan_id` + `channel`) | **gap 1** |
| yunhou-users persists on `model.Order` | n/a — model has no `sub_expires_at` column | **gap 2** |
| webhook handler reads from `orders.sub_expires_at` when WeChat's `resource` block has no `sub_expires_at` | n/a — `parseWeChat` (`handler/webhook.go:180`) and `parseWeChatMock` (line 245) and the channel-agnostic path (line 522) all leave `SubExpiresAt = nil` if the upstream payload doesn't carry one | **gap 3** |
| `reconcileFromChannel` writes from `order.sub_expires_at` | n/a — `buildReconcileWebhookEvent` returns `SubExpiresAt` as nil by design (per `c9ab516`) | **gap 4** |

Net effect: every WeChat subscriber's `subscriptions.expires_at` is NULL. After `c9ab516` the *new* rows behave correctly (no aliased past timestamp → no accidental past expiry in the past), but the *real* expiry isn't written either. Without this fix, every WeChat subscription is an indefinite subscription. The `login-subscription-decouple` companion spec removes the *consequences* of this gap (no one's login breaks anymore), but the underlying data inconsistency persists — every WeChat customer has effectively a lifetime plan, which is not the business intent.

## 2. Goal

End-to-end plumbing of `sub_expires_at` from the source of truth (`QuoteService` knows it because `plan.interval_days` is in `plans`) through the order row, the webhook handler's fallback read, and the reconcile path's propagation. After this ships:

- A user paying through WeChat for `plan_id=monthly` ends up with `subscriptions.expires_at = now() + 30 days` (or whatever `interval_days * '1 day'` resolves to).
- A user paying through PayPal, where `QuoteService` already surfaces `sub_expires_at` in the quote response, reaches the same DB state — and the webhook propagates it through `custom_data.sub_expires_at` (PayPal-side) → `WebhookEvent.SubExpiresAt` → `activateSubscriptionOnTx`.
- A `sub_expires_at = NULL` row only exists for **deliberately** zero-expiry plans (none of which exist in the seeded catalog today, but the data model permits them).

## 3. Non-goals

- **No frontend changes** to `/console` or `/billing/*`. The SPA reads `subscription.expires_at` from `/auth/me` and already renders "expired" / countdown UI based on it; that path doesn't need to change.
- **No changes to the PayPal provider's `custom_data` shape.** PayPal already embeds `sub_expires_at` in `custom_data` (per `quote.ts:24-37` doc). Backend just needs to start *reading* it.
- **No sweeper / cleanup of the NULL rows.** The `login-subscription-decouple` spec's one-shot backfill handles the cn-staging pollution; beyond that, NULL rows reflect historical behaviour. Future rows get the right value; we don't touch history.

## 4. Design decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Migration adds `orders.sub_expires_at TIMESTAMPTZ NULL`** | Nullable because not every order is a paid subscription order (smoke-test `monthly` orders, perhaps future order types). Same shape as `provider_intent` — nullable, default NULL. |
| 2 | **`model.Order` adds `SubExpiresAt *time.Time` with `db:"sub_expires_at"` and `json:"sub_expires_at,omitempty"`** | Pointer so NULL → nil scan works. Not in `provider_intent`'s JSON because the BFF currently only reads `expires_at` (order expiry), `provider_intent`, and order ID — adding `sub_expires_at` to the response shape is a *separate* BFF question, so we limit the change to "make it visible to backend". |
| 3 | **`POST /payments/orders` accepts an optional `sub_expires_at` (RFC3339) in the request body** | The BFF already knows this value from the prior `POST /apps/:id/quote` round-trip. Mirrors `POST /payments/orders/:id/confirm` which already accepts a similar `expires_at` shape. |
| 4 | **`QuoteService` stays the source of truth — handler doesn't compute** | The BFF used to compute `sub_expires_at` client-side (referred to in `quote.ts:36-39` doc-comment as the v1 shape); we now consume what Yunhou returns. The handler accepts whatever the BFF passes because the BFF has the freshly-returned quote; doing server-side recomputation would be redundant and risk a drift if `interval_days` ever changes mid-rename. |
| 5 | **WeChat webhook reads `orders.sub_expires_at` as fallback when `parseWeChat`'s resource-block didn't carry one** | Today WeChat never carries `sub_expires_at` in `resource` (verified — see `internal/billing/wechat/pay_mapping.go` for what fields are mapped out of `transaction_id`/`out_trade_no`/`amount`). The fallback is the only way WeChat orders get the value. |
| 6 | **All other channels (PayPal, Stripe, Alipay) read from their own payload first, then fall back to `orders.sub_expires_at`** | PayPal already passes `sub_expires_at` in `custom_data` and the webhook's `parsePayPal`-equivalent can just read it. A channel that never carries it (Stripe / Alipay today) gets the order-row fallback. |
| 7 | **`reconcileFromChannel` propagates `orders.sub_expires_at` into `WebhookEvent.SubExpiresAt`** | Closes gap 4 from §1. Pre-`c9ab516` the helper assigned `successTime` (a past time); today's `c9ab516` leaves it nil. After this spec it reads from the order row when set, else remains nil. The helper's nil semantics (meaning "no signal, defer to order-row fallback downstream") are unchanged. |
| 8 | **`activateSubscriptionOnTx` reads `WebhookEvent.SubExpiresAt` if set, otherwise reads `orders.sub_expires_at` for the order ID** | Already does the *WebhookeEvent* side; the order-row fallback closes the loop. Decision mirrors #5 and #7. |
| 9 | **BFF `POST /payments/orders` body includes `sub_expires_at`** when the prior `/quote` returned one | One-line change in `routes/billing.ts`. Falls back to omitted if the quote had no `sub_expires_at` (free plan / weird channel). |

## 5. Architecture changes (file map)

### 5.1 yunhou-users

| # | File | Change |
|---|---|---|
| 1 | `migrations/013_orders_sub_expires_at.sql` | NEW. `ALTER TABLE orders ADD COLUMN IF NOT EXISTS sub_expires_at TIMESTAMPTZ NULL;` plus `CREATE INDEX IF NOT EXISTS orders_sub_expires_at_idx ON orders (sub_expires_at)` for future sweeper / lookup convenience (even though we don't ship a sweeper today). |
| 2 | `internal/model/order.go` | Add `SubExpiresAt *time.Time \`db:"sub_expires_at" json:"sub_expires_at,omitempty"\``. Bump comment to reference the new e2e chain. |
| 3 | `internal/handler/payment.go` `CreateOrder` | Accept `sub_expires_at *time.Time` in the request body (optional). Pass into service. |
| 4 | `internal/service/payment.go` `CreateOrder` | Persist `req.SubExpiresAt` on the new `model.Order`. Service signature gains a parameter or struct field — follow whichever pattern the file already uses for similar `CreateOrder`-shaped services. **The handler does NOT recompute from `plan.interval_days`** — the BFF has already provided the value from the prior `/quote` round-trip; server-side recomputation would risk drift if `interval_days` ever changes mid-rename (per Decision #4). |
| 5 | `internal/handler/webhook.go` `parseWeChat` (real + mock) and the channel-agnostic fallback (line 522) | When the channel-decoded `WebhookEvent.SubExpiresAt` is nil, look up `orders.sub_expires_at` by `o.ID` and assign it to the event before returning. Same for the mock path. Add a small helper `func (h *WebhookHandler) fallbackSubExpiresAt(ctx, orderID) (*time.Time, error)` to keep the lookup centralised. |
| 6 | `internal/service/payment.go` `buildReconcileWebhookEvent` | After reading `o := orderRepo.FindByID(o.ID)` (which the caller already does in `reconcileFromChannel`), the helper sets `event.SubExpiresAt = o.SubExpiresAt` only if the order row has it. Same `c9ab516` "never invent from past timestamps" discipline preserved. |
| 7 | `internal/service/payment_test.go` | New tests: `TestBuildReconcileWebhookEvent_PropagatesOrderSubExpiresAt` (when `o.SubExpiresAt = &t`, the helper copies); `TestBuildReconcileWebhookEvent_NilOrderSubExpiresAt_StaysNil` (lock down c9ab516's invariant). |
| 8 | `internal/handler/webhook_test.go` | New tests: `TestParseWeChat_FallbackToOrderSubExpiresAt` — feeds a WeChat payload with no resource-block expiry, mocks `orderRepo.FindByID` to return a row with `SubExpiresAt = &t`, asserts the returned `WebhookEvent.SubExpiresAt = &t`. Mirror for the mock path. |

### 5.2 yunhou-website (BFF)

| # | File | Change |
|---|---|---|
| 9 | `server/src/routes/billing.ts` | When forwarding to Yunhou's `POST /payments/orders`, include `sub_expires_at: <quote result>` in the JSON body. Cast `quote.sub_expires_at` (string) — server-side accepts RFC3339. |
| 10 | `server/src/providers/types.ts` | No change to types. `YunhouOrderData` already only types fields the BFF reads; the `sub_expires_at` outgoing from this route is request-only. |
| 11 | `e2e/cn-wechat-payment.staging.spec.ts` | Add an assertion that after a successful WeChat QR-code-driven checkout, polling `GET /payments/orders/:id` and decoding `sub_expires_at` returns a timestamp > now + 29 days (the smoke variant). |
| 12 | `server/src/routes/billing.test.ts` | Unit coverage for the new request body shape. |

## 6. Data flow (after fix)

```
1. BFF  POST /apps/:id/quote    → response: { sub_expires_at: "...+30d", ... }
2. BFF  POST /payments/orders   → body: { plan_id, channel, sub_expires_at }
                                  yunhou-users persists `orders.sub_expires_at = sub_expires_at`
3. User pays via WeChat → webhook hits /webhooks/payment/wechat
   a. parseWeChat reads resource block — `SubExpiresAt` is nil (WeChat has no field for this)
   b. parseWeChat falls back: orderRepo.FindByID(orderID).SubExpiresAt → assigned to event
4. OnWebhook processes WebhookEvent:
   a. webhookEvents dedupe by event_id (real UUID; reconcile uses "reconcile:..." prefix)
   b. activateSubscriptionOnTx(order, event):
        subscriptions.expires_at = event.SubExpiresAt ?? order.SubExpiresAt
                                = (wechat: same string from orders row)
5. subscription row now has the correct future expiry
```

For a PayPal flow steps 3a/3b differ — PayPal passes `sub_expires_at` in `custom_data`, `parsePayPal`-equivalent assigns it directly, the order-row fallback is "belt and braces".

For reconcile (FE poll hitting `GetOrder`, no webhook ever arrived):
```
reconcileFromChannel(orderID):
  o = orderRepo.FindByID(orderID)
  res = WeChat.QueryOrder(...)
  event = buildReconcileWebhookEvent(res)
    event.SubExpiresAt = o.SubExpiresAt  // from order row, set at step 2
  → OnWebhook(event)
  → activateSubscriptionOnTx(order, event)
    → subscriptions.expires_at = o.SubExpiresAt
```

## 7. Tests

### 7.1 Required unit
- `migrations` are tested implicitly via `make migrate` against a fresh DB in CI.
- `TestModelOrderSubExpiresAtField_DBScansNULL`: a SQL NULL must scan to `nil` (pointer semantics), not panic.
- `TestCreateOrder_PersistsSubExpiresAt`: roundtrip create → fetch from DB → assert the column has the value the request carried.
- `TestCreateOrder_OmitsSubExpiresAt_LeavesNULL`: omitted field → NULL → nil pointer on read.
- `TestParseWeChat_FallbackToOrderSubExpiresAt`: covered in §5.1.8.
- `TestParseWeChat_NoOrderSubExpiresAt_StaysNil`: locked-down invariant — when neither path has a value, the helper returns nil.
- `TestBuildReconcileWebhookEvent_PropagatesOrderSubExpiresAt`: covered in §5.1.7.
- `TestBuildReconcileWebhookEvent_NilOrderSubExpiresAt_StaysNil`: same.
- Existing `TestBuildReconcileWebhookEvent_DoesNotSetSubExpiresAt` continues to pass — guards the pre-`c9ab516` regression class.

### 7.2 Required E2E
- `tests/e2e-ui` (cn-staging Playwright suite) — `cn-wechat-payment.staging.spec.ts`: assert that after a WeChat paid checkout, the user's `GET /user/subscriptions` returns an `expires_at` in the future (e.g. now + 29d ≤ actual ≤ now + 31d for `monthly` plan).
- Same suite, PayPal path — exercises the `custom_data.sub_expires_at` roundtrip.

### 7.3 Required integration (yunhou-deploy)
- Smoke script `tests/smoke-cn-staging.sh` (if it doesn't already query subscription state) — adds an assertion: after the smoke-create user pays, the subscription row has a future `expires_at`. Cheaper than the full E2E; protects against regressions in the productionised pipeline.

## 8. Rollout

1. Land migration `013_*` first as a separate commit on its own — it must reach staging before the code that uses the column.
2. Land backend changes (`model`, `CreateOrder`, `parseWeChat` fallback, `buildReconcileWebhookEvent` propagation, tests) in a second commit on the same branch.
3. Land BFF changes (`billing.ts` body shape, e2e spec update) in a third commit.
4. Deploy backend → staging → wait for smoke run.
5. Deploy BFF → staging → wait for smoke + the new e2e assertion.
6. Verify: on a fresh smoke user, `subscriptions.expires_at` is now(ish) + 30d, not NULL.

## 9. Out of scope (acknowledged)

- **Sweeper** that periodically writes `subscriptions.expires_at` from `orders.sub_expires_at` when one is set but the other isn't. Considered; declined because all *new* rows will already have both, and historic NULL rows are a different problem (see the `login-subscription-decouple` companion spec's §7 backfill which addresses today's pollution).
- **Frontend console UI changes** that surface the new `sub_expires_at` in places other than `/auth/me`'s response. The existing console already reads `subscription.expires_at` — no FE work needed.
- **Renaming the field** (`sub_expires_at` vs `subscription_expires_at`, vs `expires_at_v2`). Existing column family is `expires_at`; `sub_expires_at` is a clearer disambiguator than repeating the prefix. Leaving as-is.
- **Backwards-compat shim** for orders created before the migration shipped (NULL `sub_expires_at`). They get a NULL → user gets an indefinite subscription in the database; this matches today. Future top-ups recompute via `interval_days`. Acceptable for this PR.
