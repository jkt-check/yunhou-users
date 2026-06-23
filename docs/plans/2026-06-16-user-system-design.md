# Yunhou Users — System Design

## Overview

A shared user management system API for multiple applications (apps, websites). One user account across all consumer apps, with social-only auth and an app marketplace subscription model.

Tech stack: Go + Gin + PostgreSQL, self-signed JWT with RSA public key verification.

## Core Domain Models

### User
Identity shell — no mandatory email/phone/password.

| Field      | Type         | Notes               |
|------------|--------------|----------------------|
| id         | uuid         | PK                   |
| nickname   | text         | nullable             |
| avatar_url | text         | nullable             |
| status     | enum         | active/suspended/deleted |
| created_at | timestamp    |                      |
| updated_at | timestamp    |                      |

### SocialIdentity
The actual login credential. provider + provider_uid is unique.

| Field        | Type     | Notes                          |
|--------------|----------|--------------------------------|
| id           | uuid     | PK                             |
| user_id      | uuid     | FK → User                      |
| provider     | text     | github / google / wechat       |
| provider_uid | text     | user ID from the provider      |
| email        | text     | email returned by provider     |
| created_at   | timestamp|                                |

### App
Registered consumer application. Identified by a stable TEXT `app_id` (e.g. `yundian`, `yundash`) so it can appear in URLs and JWT claims without UUID escaping concerns.

| Field        | Type     | Notes                                  |
|--------------|----------|----------------------------------------|
| app_id       | text     | PK                                     |
| name         | text     | display name                           |
| description  | text     | optional                               |
| config       | jsonb    | app-specific config (per-app key/value)|
| is_active    | boolean  | default true; admin can disable        |
| created_at   | timestamp|                                        |
| updated_at   | timestamp|                                        |

### Subscription
Controls whether a user can use the apps included in their plan. **One subscription is shared across all consumer apps** — a single plan gates access to every app listed in `plans.apps`. There is no per-app subscription.

| Field      | Type     | Notes                                       |
|------------|----------|---------------------------------------------|
| id         | uuid     | PK                                          |
| user_id    | uuid     | FK → User                                   |
| plan_id    | text     | FK → plans(id)                              |
| status     | enum     | active / expired / cancelled                |
| started_at | timestamp| when the subscription began                 |
| expires_at | timestamp| nullable, null = never expires              |
| created_at | timestamp|                                             |
| updated_at | timestamp|                                             |

Unique constraint: **one active subscription per user** — `UNIQUE(user_id) WHERE status='active'`. Expired and cancelled rows don't count.

### Session
Token record for audit and potential revocation.

| Field         | Type     | Notes                     |
|---------------|----------|---------------------------|
| id            | uuid     | PK                        |
| user_id       | uuid     | FK → User                 |
| app_id        | text     | FK → apps(app_id) — TEXT after 002_simplify_plans |
| refresh_token | text     | hashed                    |
| scope         | text[]   | granted scopes            |
| revoked       | boolean  | default false             |
| expires_at    | timestamp|                           |
| created_at    | timestamp|                           |

### Order
Pre-payment intent, owned by yunhou-users. Created when a user picks a paid plan, before the frontend initiates the actual payment. Carries the price snapshot at creation time so plan price changes don't retroactively affect in-flight orders.

| Field      | Type         | Notes                                          |
|------------|--------------|------------------------------------------------|
| id         | uuid         | PK                                             |
| user_id    | uuid         | FK → User                                      |
| plan_id    | text         | FK → Plan                                      |
| amount     | decimal(10,2)| price snapshot from plan at order time (see §"v1 limitations" below) |
| currency   | text         | ISO 4217, NOT NULL, default `'CNY'`            |
| status     | enum         | pending / paid / failed / refunded / cancelled / expired |
| expires_at | timestamp    | **default `now() + INTERVAL '30 minutes'`** (overridable per order, but the v1 default is 30 min, set by `ORDER_EXPIRY_DURATION` env var) |
| created_at | timestamp    |                                                |
| updated_at | timestamp    |                                                |

**v1 decisions on order lifecycle**:

- **Default expiry = 30 minutes** after creation. Configurable via `ORDER_EXPIRY_DURATION`. A sweeper job flips long-pending orders (`status='pending'` AND `expires_at < now()`) to `status='expired'`. The sweeper interval should be much shorter than the expiry window (e.g. 1 min sweeper, 30 min expiry) so state changes propagate quickly.
- **Audit log retention is unbounded in v1.** Late-arriving payments (after sweeper marked the order `expired`) write an `audit_log` row tagged `late_payment_post_expiry` for ops visibility. We do not impose a retention period — revisit when storage cost becomes a concern. See webhook doc §8.

**v1 limitations on pricing** — explicit non-goals for this iteration:

- **No promo codes / coupons** — `orders.amount` is a direct copy of `plans.price` at creation time. Discount codes require a separate `discounts` table, application logic, and webhook payload changes (channels must report the discounted amount). Not in v1.
- **No multi-currency** — every order is in the merchant's configured base currency (default `CNY`). FX conversion is the channel's responsibility; we just store what arrives.
- **No partial capture** — the order is for the full plan price. If the channel settles a different amount (rare; usually a bank-side fee adjustment), we record it on `payments.amount` but the plan is still fully activated.

These are extension points, not oversights. The schema doesn't preclude them; the service layer doesn't implement them.

### Payment
Payment channel transaction, one Order can have at most one successful Payment (retries create new attempts with distinct `external_txn_id`; only one transitions `order.status` to `paid` — enforced by partial unique index `idx_payments_one_paid_per_order`).

| Field           | Type          | Notes                                            |
|-----------------|---------------|--------------------------------------------------|
| id              | uuid          | PK                                               |
| order_id        | uuid          | FK → Order                                       |
| channel         | text          | stripe / wechat_pay / alipay                     |
| external_txn_id | text          | channel's transaction ID; UNIQUE (channel, external_txn_id) |
| amount          | decimal(10,2) | actual settled amount (may differ from order amount on partial capture) |
| currency        | text          | NOT NULL; **source of truth at INSERT time** — webhook handler reads from the decrypted channel payload (with fallback to `orders.currency` if absent); confirm endpoint reads from `orders.currency` |
| status          | enum          | pending / paid / failed / refunded               |
| paid_at         | timestamp     | nullable; set when status transitions to `paid`  |
| failed_reason   | text          | nullable; populated when status = `failed`       |
| disputed        | boolean       | default false; set true by `charge.dispute.created` webhook |
| disputed_at     | timestamp     | nullable; mirrors when `disputed=true`           |
| raw_payload     | jsonb         | full channel webhook body for audit / debugging  |
| created_at      | timestamp     |                                                  |
| updated_at      | timestamp     |                                                  |

### WebhookEvent
Per-event audit row. The webhook handler inserts here **before** any business action; this is the event-level idempotency key (see webhook doc §5.1).

| Field        | Type        | Notes                                            |
|--------------|-------------|--------------------------------------------------|
| id           | uuid        | PK                                               |
| channel      | text        | stripe / wechat_pay / alipay                     |
| event_id     | text        | channel's event ID (Stripe `event.id`, WeChat `notify_id`, Alipay `notify_id`); UNIQUE (channel, event_id) |
| event_type   | text        | channel's event type string (e.g. `payment_intent.succeeded`) |
| received_at  | timestamp   | when we got the bytes                            |
| processed_at | timestamp   | nullable; set when handler finishes — NULL = queued, NOT NULL = done (success OR ignored-for-our-handler) |
| raw_payload  | jsonb       | the raw webhook body                             |

### AuditLog
Ops-critical event log written from the service layer (NOT from webhooks — those write `webhook_events`).

| Field      | Type        | Notes                                                              |
|------------|-------------|--------------------------------------------------------------------|
| id         | uuid        | PK                                                                 |
| occurred_at| timestamp   | default now()                                                      |
| actor      | text        | "sweeper" / "service" / "user:<user_id>" / "admin:<app_id>"       |
| action     | text        | short verb-noun, e.g. `late_payment_post_expiry`, `cancel_order`   |
| target     | text        | resource reference, e.g. `order:<order_id>`                        |
| tags       | text[]      | searchable labels; partial index for ad-hoc queries                |
| context    | jsonb       | structured payload — old state, new state, ids involved            |

Retention is **unbounded in v1** — revisit when storage cost matters.

**Amount unit convention**: All `decimal(10,2)` amount fields in this schema (and `plans.price`, `orders.amount`, `payments.amount`, `refunds.amount`) are in **major currency units** — e.g. `29.90` means 29.90 yuan / 29.90 dollars, NOT 2990 cents.

Channels transmit amounts in different conventions:
- **Stripe** uses smallest units (cents): `$29.90` → `2990`. **Divide by 100 at the webhook boundary.**
- **WeChat Pay v3** uses fen (分): `¥29.90` → `2990`. **Divide by 100.**
- **Alipay** uses yuan as a string: `29.90`. **Parse as `decimal.Parse`, no conversion needed.**

Each channel's webhook decoder normalizes to `decimal(10,2)` major units before inserting. Doing this at the boundary means downstream code never sees mixed units — a subtle bug class that has bitten many payment integrations.

### Refund
One row per refund event. A payment can be refunded multiple times (partial refunds sum to ≤ original amount — enforced at the service layer, NOT a DB constraint, since cross-row sum checks aren't trivial in Postgres).

| Field              | Type          | Notes                                       |
|--------------------|---------------|---------------------------------------------|
| id                 | uuid          | PK                                          |
| payment_id         | uuid          | FK → Payment                                |
| channel            | text          | denormalized from `payments.channel` — duplicated here so the UNIQUE constraint below can be enforced at the DB layer without a JOIN |
| amount             | decimal(10,2) | refund amount; sum of all refunds per payment must not exceed payment.amount |
| reason             | text          | nullable                                    |
| idempotency_key    | text          | caller-supplied unique key (HTTP `Idempotency-Key` header); **UNIQUE** so duplicate POST /refunds requests with the same key resolve to the same row |
| external_refund_id | text          | channel's refund ID (nullable; populated only after the channel's refund API returns). **UNIQUE (channel, external_refund_id)** — Postgres treats NULLs as distinct, so the column being nullable does not weaken the uniqueness guarantee. |
| status             | enum          | `pending` / `paid` / `failed` — see "Refund status semantics" below |
| created_at         | timestamp     |                                            |
| updated_at         | timestamp     |                                            |

**Refund status semantics.** `pending` is the initial state after `POST /refunds` calls the channel's refund API successfully and the row is INSERTed. `paid` is set when the channel confirms settlement via webhook (`charge.refunded` / `TRANSACTION.REFUND` / `TRADE_CLOSED` for full or partial). The `failed` enum value is **reserved for future use** — v1 has no transition path into it (the POST /refunds endpoint aborts before INSERT if the channel API call fails, so no row enters `failed` from that path; no channel emits a "refund.failed" event in the documented set). Documented for forward-compatibility.

## Authentication Flow

Single path — **direct social login**. The consumer app handles OAuth consent with the provider directly (typically via the provider's JS SDK); we receive the resulting provider token and exchange it for our JWT. No OAuth redirect through yunhou-users.

```
Consumer app frontend                yunhou-users                  Provider (GitHub/Google)
        │                                  │                                │
        │  1. user clicks "Sign in with X"  │                                │
        │  2. SDK → provider consent UI     │                                │
        │  3. SDK callback: provider_token  │                                │
        │                                  │                                │
        │  4. POST /auth/login ────────────▶│                                │
        │     { provider, provider_token,   │  5. GET provider userinfo     │
        │       app_id }                    │ ──────────────────────────────▶│
        │                                  │  ◀───────── user info ──────── │
        │                                  │  6. find/create user + identity│
        │                                  │  7. resolve subscription      │
        │                                  │  8. mint access + refresh     │
        │  ◀───── { access_token, ─────────│                                │
        │           refresh_token,         │                                │
        │           user, subscription }   │                                │
        │                                  │                                │
        │  9. subsequent calls send        │                                │
        │     Authorization: Bearer ...    │                                │
        │  ──────────────────────────────▶ │  10. verify JWT locally via    │
        │                                  │      JWKS public key (no API   │
        │                                  │      roundtrip needed)         │
```

### Auto-merge on same email
When a new social login returns an email that matches an existing User's SocialIdentity, bind to that User instead of creating a new one. (WeChat mentioned here is a hypothetical future login provider; v1 only supports `github` and `google`.)

### Token details
- **access_token**: JWT, RSA256 signed, 15min TTL. Payload: `sub` (user_id), `iss` (`"yunhou-users"`), `aud` (array containing `app_id`), `app_id`, `scope` (array of plan apps), `iat`, `exp`.
- **refresh_token**: opaque, 7d TTL, stored hashed in Session table. Used to get new access_token.
- **JWKS endpoint**: `GET /.well-known/jwks.json` serves RSA public key. Apps fetch on startup and cache.
- **Revocation**: not implemented in v1. Expired tokens naturally expire in 15min. Add Redis blocklist later if needed.

## App Marketplace & Subscription

- App defines a `default_plan` at registration. **v1 does NOT auto-create a Subscription row on first login** for any plan — a user's subscription state is computed from `subscriptions` table (active row if any, else `plans.is_default` fallback). Token scope on first login reflects the default plan's `apps`.
- Subscription status flows: active → expired (on expires_at pass) / cancelled (on explicit cancel).
- Token scope reflects subscription status: next refresh after expiry drops app scope.

## Payment Boundary

Yunhou Users is the **user-related data store**. Payments sit on the user side of that line.

**In scope for yunhou-users** (data + records, owned by this service):
- `orders` — pre-payment intent (user picks a plan, server mints an order row)
- `payments` — payment channel transactions bound to an order (`channel`, `amount`, `currency`, `external_txn_id`, `paid_at`, `status`)
- `refunds` — partial / full refund records linked back to a payment
- All state transitions: `pending → paid → refunded / failed`
- Subscription activation as the downstream effect of a `paid` payment (still inside this service — `subscriptions` already lives here, FK to `payments` is local)

**Out of scope** (handled elsewhere):
- The payment execution itself — the **consumer app's frontend** drives this. It opens the SDK (Stripe.js, WeChat Pay H5, Alipay SDK), shows the cashier, captures the password / QR scan.
- The money movement — Stripe / WeChat Pay / Alipay are the actual processors.

**The trust boundary** is the critical piece. If yunhou-users accepted "the frontend says I'm paid" as the sole signal, anyone with curl could mint paid subscriptions for free. Two paths write `paid` status:

1. **Frontend notify** — the consumer app's frontend, after the SDK callback succeeds, calls `POST /payments/:id/confirm` to give the user low-latency feedback (sub-second activation). This is an *optimization*, never the fact source.
2. **Channel webhook** — the payment processor POSTs to `POST /webhooks/payment/:channel` whenever the underlying transaction changes state. This is **idempotent, signature-verified, and authoritative**. Whether or not the frontend ever calls `confirm`, the webhook will eventually settle the payment record.

**The rule** (not a contradiction — both paths write, but only one wins):

- **Both paths can transition `pending → paid`.** The frontend fast-track exists for UX latency.
- **The channel webhook is the terminating authority.** If a `failed` or `refunded` webhook arrives after an optimistic `paid` write from `confirm`, the webhook **overrides** — the payment lands in `failed` / `refunded`, the subscription is deactivated, and the optimistic `paid` is rolled back.
- We never persist a `paid` state that isn't consistent with what the channel eventually tells us. The system reconciles to the channel's view, not the frontend's.

Both paths converge on `external_txn_id` (the channel's transaction ID) — a unique constraint on `(channel, external_txn_id)` provides idempotency for retries.

See [`docs/plans/2026-06-23-payment-webhook-mechanism.md`](./2026-06-23-payment-webhook-mechanism.md) for the full mechanism: per-channel quirks, signature schemes, idempotency keys, retry behavior, and failure-mode handling.

### Scope of this service (primitive operations only)

yunhou-users exposes **raw primitive operations** that the consumer-app frontend composes into business workflows. We are deliberately **not** a business-rules engine. The split:

**In scope — we implement and store**:

- Plan CRUD (`/admin/plans`)
- Order creation, payment confirmation, refund creation — the data-flow primitives
- Webhook reception, signature verification, idempotency, side-effect application
- State machines for `subscription`, `order`, `payment`, `refund`
- Channel-internal amount-unit normalization (Stripe/WeChat/Alipay → `decimal(10,2)` major units)
- Audit log rows for ops-critical events (`late_payment_post_expiry` etc.)
- Ownership checks: a caller can only read/write their own payments and refunds (security primitive)
- 30-minute order expiry (configurable via env var)

**Out of scope — caller composes these on top**:

- **Who can refund what** — admin vs user vs self-service. We don't have `/admin/refunds`; we have `/refunds` and the caller decides whether it's acting as admin (internal app auth) or user (JWT + ownership).
- **Refund window / approval workflow** — product policy; the frontend shows/hides the refund button based on its own rules.
- **Auto-renewal refund scope** — whether a refund covers just the current period or also rolls back the upcoming renewal. We just refund what the caller asks for.
- **Refund amount calculation** — full vs partial, proration, fees. We accept any amount ≤ `payment.amount`; the caller decides what amount to send.
- **Eligibility checks** — has the user used enough of the service to "deserve" a refund? That's product judgment.
- **Notifications / emails / in-app messages** — we record state; the notification service reads it.
- **Fraud / risk decisions** — device fingerprinting, velocity checks, chargeback history lookups.

The test for whether something belongs in this service: **can it be expressed as a state transition on a row in our database?** If yes, primitive. If it requires cross-row reasoning, role-based authorization, or user-facing UX decisions, it does NOT belong here.

### Ownership → 404 (not 403)

A JWT-authenticated caller can only read/write their own orders/payments/refunds. The service layer returns **404** (not 403) on ownership mismatch so non-owners cannot enumerate the existence of resource IDs. This applies uniformly across `GET / DELETE / POST` endpoints — when you see "403 not owner" in the per-endpoint error lists below, the actual HTTP response is 404 with the same code/message envelope. Internal-app-auth callers bypass the ownership check entirely.

## API Endpoints

### Auth (public + app-side)

| Method | Path                       | Description                                                |
|--------|----------------------------|------------------------------------------------------------|
| POST   | /auth/login                | Exchange `provider_token` for `access_token` + `refresh_token` |
| POST   | /auth/refresh              | Rotate `refresh_token`, issue new pair                     |
| POST   | /auth/logout               | Revoke `refresh_token`                                     |
| GET    | /.well-known/jwks.json     | RSA public key for token verification                      |
| GET    | /healthz                   | Liveness probe                                             |

### User (requires access_token)

| Method | Path                          | Description                                                |
|--------|-------------------------------|------------------------------------------------------------|
| GET    | /user/profile                 | Current user info                                          |
| PATCH  | /user/profile                 | Update nickname / avatar                                   |
| GET    | /user/identities              | List bound social accounts                                 |
| DELETE | /user/identities/:id          | Unbind social account (min 1)                              |
| GET    | /user/subscriptions           | List user's subscriptions (active + historical)            |
| DELETE | /user/subscriptions/:id       | Cancel an active subscription — `active → cancelled`. **Primitive only**: does not refund; the caller (frontend) decides whether to issue a refund alongside the cancel. |

### App Management (requires X-App-ID header, server-to-server)

> **Note**: an `app_secret` was originally planned but never implemented. v1 uses a single `X-App-ID` header to identify the calling internal app; all app management endpoints run on the internal network and rely on network-level isolation. If per-app secret enforcement becomes necessary, it lands as a separate change with explicit migration of existing apps.

| Method | Path                     | Description                      |
|--------|--------------------------|----------------------------------|
| POST   | /apps                    | Register new app                 |
| GET    | /apps/:id                | App details                      |
| PATCH  | /apps/:id                | Update app config                |

**Note**: a `POST /subscriptions` or `POST /user/subscriptions` endpoint does **not** exist in v1. Subscriptions are created **only** as the downstream side effect of a successful payment (see webhook doc §5.3). Any path that lets a caller mint a subscription without paying is the trust boundary violation §"Payment Boundary" warns against. If v2 introduces non-payment-driven subscription creation (gift codes, promo grants, admin gift), that endpoint lands as a separate design iteration with explicit authorization rules.

### Payments (requires user JWT)

These endpoints expose the primitive operations for the payment flow. They are NOT scoped to "user-facing only" — admin tooling and internal services can also reach them with internal-app auth (the middleware enforces ownership: a JWT user can only read/write their own payments; an internal app can act on any).

| Method | Path                                  | Description                                                                 |
|--------|---------------------------------------|-----------------------------------------------------------------------------|
| POST   | /payments/orders                      | Create an Order for a paid plan                                            |
| GET    | /payments/orders/:id                  | Order status                                                                |
| DELETE | /payments/orders/:id                  | Cancel a pending order (`pending → cancelled`)                              |
| POST   | /payments/orders/:order_id/confirm    | Frontend SDK callback notify — fast-track activation; idempotent            |
| GET    | /payments                             | Caller's payment history (filtered by JWT user_id)                          |
| GET    | /payments/:id                         | Payment details; ownership check vs JWT user_id                             |
| POST   | /refunds                              | Primitive: request a refund on a payment                                    |
| GET    | /refunds/:id                          | Refund details                                                              |
| GET    | /payments/:id/refunds                 | List refunds for a payment                                                  |

**Note on `/refunds`**: this is a primitive. **We do not encode admin-vs-user distinction in the path** (no `/admin/refunds`). The caller picks its auth mode — internal-app-auth for admin tooling, JWT for end-user self-service — and we enforce ownership (a caller can only refund their own payment, or any payment if authenticated as an internal app). Business rules (refund windows, approval flows, who-can-refund-when) are composed by the frontend.

### Endpoint contracts

Every primitive endpoint's contract — what we promise callers, what we own.

#### `POST /payments/orders`

```
Request:
  { "plan_id": "monthly" }

Response 201:
  {
    "id": "<order_uuid>",
    "user_id": "<user_uuid>",
    "plan_id": "monthly",
    "amount": "29.90",
    "currency": "CNY",
    "status": "pending",
    "expires_at": "2026-06-23T10:30:00Z"
  }

Errors:
  400 plan not found / inactive
  401 missing/invalid JWT
  409 user already has an active subscription
```

- **Auth**: JWT. `user_id` from JWT claim.
- **Side effects**: `INSERT INTO orders`.
- **Ownership**: implicit (created for caller).
- **Primitive semantics**: We do NOT call the payment channel. We mint an order row and return its details. The frontend then drives the channel SDK to actually collect money.

#### `GET /payments/orders/:id`

```
Response 200: order object (same shape as above)

Errors:
  404 not found
  403 caller is not the order's owner
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: `order.user_id == JWT.user_id` (JWT case) OR any caller with internal-app-auth.
- **Side effects**: none.

#### `DELETE /payments/orders/:id`

```
Response 200: { "status": "cancelled" }

Errors:
  404 not found
  403 not owner
  409 order is not in `pending` status (already paid / failed / cancelled / expired / refunded — terminal states don't accept cancel)
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: same as GET.
- **Side effects**: `UPDATE orders SET status='cancelled' WHERE id=:id AND status='pending'`. The `WHERE status='pending'` guard means this is a no-op on already-terminal orders (returns 409).
- **Primitive semantics**: we don't refund anything, don't notify anyone. Just flips the order status. The frontend decides whether to show a "cancel" button.

#### `POST /payments/orders/:order_id/confirm`

Frontend SDK callback after a successful in-SDK payment. The `:order_id` is the **order UUID** (NOT a payment id — the payment row is created by this call or by the eventual webhook).

```
Request:
  {
    "channel": "stripe",          // required
    "external_txn_id": "pi_xxx"   // required — channel's transaction ID
    // amount and currency are intentionally NOT accepted; the order row
    // is the authoritative source for both.
  }

Response 200:
  {
    "payment_id": "<payment_uuid>",
    "order_id": "<order_uuid>",
    "status": "paid",
    "activated_subscription": true,
    "was_late_payment": false   // true when the order was 'expired' and we honored the payment per "Late payment" below
  }

> Note: `amount` and `currency` in the request body are NOT honored by this endpoint — the order row is the authoritative source. (Previously accepted and superseded in this revision.)

Errors:
  404 order not found
  403 not owner
  409 order is in a non-recoverable terminal state (`failed` or `refunded`). `expired` and `cancelled` orders are recoverable per "Late payment" below — confirm honors the payment on those.
  409 channel mismatch — this order already has a `paid` payment on a different channel (see "Channel mismatch pre-check" below).
  409 existing payment on this order is `failed` (rare race with `.payment_failed` webhook). User must retry with a new external_txn_id.
```

- **Auth**: JWT.
- **Ownership**: `order.user_id == JWT.user_id`.
- **Channel mismatch pre-check** (BEFORE the INSERT in step 1): a single SELECT to detect "this order already has a paid payment on a different channel", which would otherwise surface as a 500 from `idx_payments_one_paid_per_order` instead of an actionable 409:
  ```sql
  SELECT channel FROM payments WHERE order_id = $order_id AND status = 'paid';
  ```
  If non-empty AND `channel != $request_channel` → return 409 with the existing channel in the error body so the frontend knows which channel won. This pre-check is best-effort under concurrency; the partial unique index remains the authoritative guard against two paid payments.
- **Side effects** (single transaction):
  1. Channel mismatch pre-check (above).
  2. `INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status='paid', paid_at=now()) ON CONFLICT (channel, external_txn_id) DO NOTHING RETURNING id`
  3. If conflict: re-read the existing payment row and apply the state transition idempotently (the same logic as the webhook handler — see webhook doc §5).
  4. Activate subscription per webhook doc §5.3 (single-row UPSERT).
  5. `UPDATE orders SET status='paid' WHERE id=:id AND status IN ('pending', 'expired', 'cancelled')`.
  6. On `expired → paid` transition: write `audit_log` row tagged `late_payment_post_expiry`.
- **Idempotency**: confirmed via the `(channel, external_txn_id)` unique constraint. A retried confirm with the same body is a no-op (the second call sees the existing payment row and returns 200 with the same `payment_id`).

**Late payment** — if the order is in `expired` or `cancelled` status, confirm honors the payment per §8 policy. The 409 only fires for `failed`/`refunded` orders (terminal-non-recoverable) and channel mismatches.

#### `GET /payments`

```
Response 200: [payment_object, payment_object, ...]

payment_object shape:
  {
    "id": "<payment_uuid>",
    "order_id": "<order_uuid>",
    "channel": "stripe",
    "amount": "29.90",
    "currency": "CNY",
    "status": "paid",
    "paid_at": "...",
    "created_at": "..."
  }
```

- **Auth**: JWT.
- **Ownership**: `WHERE orders.user_id = JWT.user_id` (filter at the SQL layer, never trust client to filter).
- **Side effects**: none.

#### `GET /payments/:id`

```
Response 200: payment_object (same shape)

Errors:
  404 not found
  403 not owner
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: same.

#### `POST /refunds`

Primitive refund operation. The service layer calls the channel's refund API to **trigger** the refund, then writes a `refunds` row in `pending` status. The channel eventually sends a webhook (`charge.refunded` etc.) that flips the row to `paid`.

**Requires `Idempotency-Key` header** (HTTP convention; Stripe / Adyen / many processors use the same pattern). Without it, a network glitch between caller's request and our response causes the caller to retry — and a naïve retry calls the channel refund API again, doubling the refund. With it, the second call resolves to the existing row's status without re-calling the channel.

```
Request:
  Headers: Idempotency-Key: <uuid>     ← REQUIRED; stored on refunds.idempotency_key with UNIQUE constraint
  Body:
  {
    "payment_id": "<payment_uuid>",  // required
    "amount": "29.90",                // required; must be > 0 and ≤ payment.amount; sum of all refunds per payment ≤ payment.amount (enforced)
    "reason": "user requested"        // optional, free text
  }

Response 200 (initial):
  {
    "id": "<refund_uuid>",
    "payment_id": "<payment_uuid>",
    "amount": "29.90",
    "status": "pending",
    "external_refund_id": "re_xxx"     // populated after channel API call succeeds
  }

Response 200 (retry with same Idempotency-Key):
  { ... same body as above, current status ... }
  // The handler returns the existing row without calling the channel API a second time.

Errors:
  400 missing Idempotency-Key header
  400 amount ≤ 0 / amount > payment.amount
  400 sum of existing refunds + this amount > payment.amount
  404 payment not found
  403 not owner (and not internal auth)
  409 payment.status is not `paid` (cannot refund a `pending`/`failed`/`refunded` payment)
  502 channel refund API call failed (Stripe / WeChat / Alipay returned error or timed out)
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: `payment.user_id == JWT.user_id` OR internal-app-auth.
- **Side effects** (single transaction with `SELECT FOR UPDATE` on payment row):
  1. **Idempotency check** — `SELECT id, status, external_refund_id FROM refunds WHERE idempotency_key = $key`. If a row exists, return its current state with 200; **do not call the channel API a second time**.
  2. Lock payment row.
  3. Validate `payment.status = 'paid'`.
  4. Validate sum invariant.
  5. **Call channel refund API** (Stripe `POST /v1/refunds` with `Idempotency-Key` header echoed, WeChat refund, Alipay refund). If this fails, abort with 502 — we did not create the row.
  6. `INSERT INTO refunds (..., idempotency_key=$key, status='pending', external_refund_id=<channel response>)`.
  7. Commit. The webhook arrives later and flips to `paid` (see webhook doc §5.5).
- **Primitive semantics**: We do NOT decide whether to refund, when to refund, or how much — the caller does. We DO enforce the math (sum invariant, amount > 0) and the channel-side state (can only refund a `paid` payment). And we DO serialize concurrent attempts via row lock.
- **Known performance trade-off**: step 5 holds the payment row lock for the duration of the channel API call (typically <2s, occasionally 30s+ for Stripe). Concurrent refund attempts on the same payment are serialized. Acceptable for v1 — see webhook doc §5.4.

#### `GET /refunds/:id`

```
Response 200: refund_object

refund_object:
  {
    "id": "<refund_uuid>",
    "payment_id": "<payment_uuid>",
    "amount": "29.90",
    "reason": "user requested",
    "status": "pending",   // pending → paid (channel webhook confirms). `failed` reserved; see Refund table note.
    "external_refund_id": "re_xxx",
    "created_at": "...",
    "updated_at": "..."
  }

Errors:
  404 not found
  403 not owner
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: via `refunds.payment_id → payments → orders → user_id`.

#### `GET /payments/:id/refunds`

```
Response 200: [refund_object, refund_object, ...]

Errors:
  404 payment not found
  403 not owner
```

- **Auth**: JWT or internal-app-auth.
- **Ownership**: same.

### Order ↔ Payment ↔ Channel relationship

- An **Order** has no `channel` field — the user picks a plan, not a channel, when creating an order.
- A **Payment** belongs to one Order and declares its `channel`. The same Order may have multiple Payment attempts across different channels (Stripe failed → WeChat succeeded).
- The `idx_payments_one_paid_per_order` partial unique index guarantees that at most one Payment per Order reaches `status='paid'`. So while cross-channel retries are allowed, only one wins.
- When the frontend calls `POST /payments/orders/:order_id/confirm`, it must specify the `channel` it's confirming. If a payment with the same `(channel, external_txn_id)` already exists (idempotency), we re-use it. If the order already has a `paid` payment on a different channel, the confirm returns 409.

### Payment Webhooks (channel → yunhou-users, signature-verified, idempotent)

| Method | Path                                  | Description                                                                 |
|--------|---------------------------------------|-----------------------------------------------------------------------------|
| POST   | /webhooks/payment/stripe              | Stripe `payment_intent.succeeded` / `.payment_failed` / `.canceled` / `.refunded` / `.dispute.created` |
| POST   | /webhooks/payment/wechat_pay          | WeChat Pay async notify (paid / refunded)                                  |
| POST   | /webhooks/payment/alipay              | Alipay async notify (TRADE_SUCCESS / TRADE_CLOSED)                          |

Webhook endpoints have **separate, looser rate limits** (so retry storms from a channel don't fight user traffic) and require **HMAC / RSA signature verification** keyed per-channel secret stored in env vars. They are NOT JWT-authenticated — the signature IS the auth. See the webhook mechanism doc for full details.

**Transport**: TLS / mTLS is terminated at the reverse proxy (Caddy / nginx / cloud LB) in front of yunhou-users. We do not manage certs in the Go process.

**Anomaly note**: a webhook arriving for an order that doesn't exist in our DB is an attack / configuration error / test artifact. We **return 200** and write an `audit_log` row tagged `webhook_for_unknown_order` (or `webhook_refund_unknown_payment` for refund events). The 200 stops the channel's retry loop — retrying an event for an order that will never exist just creates noise; the audit_log row gives ops the signal to investigate. **Normal operation always has the order row created before the channel is invoked**, so this branch should not fire in production.

(An earlier draft of this doc said "return 404" — the policy was changed to 200+audit after M5 e2e exercise showed that 404 makes the channel retry the same unresolvable event, which is operationally noisier than a silent ack + audit. See `TestWebhook_Stripe_UnknownOrder_Audited` in `tests/e2e/webhooks_test.go` for the asserted behavior.)

## Project Structure

```
yunhou-users/
├── cmd/
│   └── server/
│       └── main.go              — Entry point
├── internal/
│   ├── config/
│   │   └── config.go            — Env loading (DB, keys, OAuth config, payment channel secrets)
│   ├── model/
│   │   ├── user.go
│   │   ├── social_identity.go
│   │   ├── app.go
│   │   ├── plan.go              — Plan (added in 002_simplify_plans)
│   │   ├── subscription.go
│   │   ├── session.go
│   │   ├── order.go             — Order (planned: 003_payments)
│   │   ├── payment.go           — Payment, Refund (planned: 003_payments)
│   │   └── webhook_event.go     — WebhookEvent audit row (planned: 003_payments)
│   ├── repo/
│   │   ├── user.go
│   │   ├── social_identity.go
│   │   ├── app.go
│   │   ├── plan.go              — added in 002
│   │   ├── subscription.go
│   │   ├── session.go
│   │   ├── order_repo.go        — planned: 003
│   │   ├── payment_repo.go      — planned: 003
│   │   └── webhook_event_repo.go — planned: 003
│   ├── service/
│   │   ├── auth.go              — OAuth callback, User find/create
│   │   ├── token.go             — JWT sign/refresh, JWKS
│   │   ├── subscription.go      — Subscription lifecycle
│   │   ├── plan.go              — Plan CRUD + CheckAppAccess
│   │   └── payment.go           — Order create, OnWebhook, Confirm, Refund (planned: 003)
│   ├── handler/
│   │   ├── auth.go              — /auth/login, /auth/refresh, /auth/logout
│   │   ├── user.go              — /user/*
│   │   ├── app.go               — /apps/*, /admin/plans/*, /admin/apps/*
│   │   ├── payment.go           — /payments/* (planned: 003)
│   │   └── webhook.go           — /webhooks/payment/:channel (planned: 003)
│   ├── middleware/
│   │   ├── auth.go              — JWT verification, extract user_id/app_id/scope
│   │   ├── app_auth.go          — app_id + app_secret verification
│   │   └── webhook_sig.go       — per-channel signature verification (planned: 003)
│   └── router/
│       └── router.go            — Route registration
├── migrations/
│   ├── 001_init.sql
│   ├── 002_simplify_plans.sql
│   └── 003_payments.sql         — planned: orders, payments, refunds, webhook_events
├── go.mod
├── go.sum
├── Makefile
└── CLAUDE.md
```

Layering: model (pure structs) → repo (SQL only) → service (business logic) → handler (HTTP I/O).

No ORM — use sqlx or database/sql for query control.

## Design Decisions

1. **Social-only auth**: no email/phone/password required. Cuts ~40% complexity vs traditional auth.
2. **Self-signed JWT + JWKS**: apps verify tokens locally, zero runtime dependency on user system. 15min TTL makes revocation delay acceptable.
3. **Payment execution is external, payment data is owned here**: yunhou-users does not move money and does not call payment SDKs. It does own `orders` / `payments` / `refunds` records, accepts channel webhooks as the source of truth, and exposes payment history to users. The trust boundary is the channel webhook — frontend `confirm` is an optimization, not a fact source.
4. **sqlx over ORM**: user system has strong query patterns, raw SQL is more controllable and performant.
5. **Single binary**: monolith for this scale. No microservices overhead.
