# CLAUDE.md

This file provides guidance to kscc (claude.ai/code) when working with code in this repository.

## Project Overview

Yunhou Users is a **shared user management API** serving multiple consumer applications via RESTful APIs. All apps share the same user identity — one account per person across all consumers. Authentication is **social OAuth only** (GitHub, Google); there is no email/password registration.

## Architecture

- **Shared identity**: One user account across all consumer apps
- **Plan-based access**: Apps are accessible based on the user's subscribed plan (free/monthly/quarterly/yearly)
- **API-first**: Consumer apps integrate via REST; no server-rendered UI
- **Direct login**: Consumer app sends provider token directly to `/auth/login` (no OAuth redirect flow for internal apps)
- **Subscription gate**: Login and token refresh check active subscription and plan app list before issuing tokens
- **Refresh token rotation**: Every refresh invalidates the old refresh token and issues a new one

### Layering

`model` (structs) → `repo` (SQL, `*sqlx.DB`) → `service` (business logic) → `handler` (HTTP I/O) → `router` (wiring)

All repos are interface-based (`repo.UserRepo`, etc.) for testability. Handler tests use hand-rolled mock structs with function fields — no external mocking libraries.

### Auth Flow

1. `POST /auth/login` — receives `{provider, provider_token, app_id}`, returns access + refresh tokens + subscription info with `has_access`
2. `POST /auth/refresh` — exchanges refresh token for new tokens (checks subscription, rotates refresh token)
3. `POST /auth/logout` — revokes refresh token
4. Consumer apps verify access tokens locally via `GET /.well-known/jwks.json` (RSA256 JWK, kid=`yunhou-users-rsa`)

**Plan-based access**: Each plan contains a list of accessible apps. When a user logs in, the response includes `has_access: true/false` for the requested app based on their subscribed plan.

### JWT Claims

| Claim | Type | Description |
|---|---|---|
| `sub` | string | User UUID |
| `iss` | string | Issuer; server-validated as `"yunhou-users"` |
| `aud` | string array | App IDs the token is valid for; server-validated to include the requested `app_id` |
| `app_id` | string | App ID the token was issued for (matches `aud[0]`) |
| `scope` | string array | Apps from the user's current Plan (`plans.apps`) |
| `exp` | int (Unix s) | Expiry — set to `iat + JWT_ACCESS_TTL` |
| `iat` | int (Unix s) | Issued-at |

## Development Commands

- `make build` — compile to `bin/server`
- `make run` — run dev server (default :8080)
- `make test` — run unit tests with race detection and coverage (`./internal/...`)
- `make e2e` — run E2E tests against local PostgreSQL (`./tests/e2e/`)
- `make lint` — run `go vet`
- `make deps` — tidy go.mod
- `make generate-keys` — generate RSA key pair in `keys/`
- `go test -race -run TestFoo ./internal/service/` — run a single test
- Database migration: apply `001_init.sql`, then `002_simplify_plans.sql`, then `003_payments.sql`, then `004_ls_channel.sql`, then `005_paypal_channel.sql`, then `006_paypal_sub_mapping.sql`, then `007_app_secret.sql` (each depends on the prior; running out of order fails). After applying 007, run the server once so `BackfillAppSecrets` populates `secret_hash` for pre-existing app rows; capture the plaintexts from the deploy log and rotate them via `POST /admin/apps/:id/rotate-secret`.

## Required Environment Variables

| Variable | Required | Default | Notes |
|---|---|---|---|
| `PORT` | No | `8080` | |
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` | |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` | File path |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` | File path |
| `JWT_ACCESS_TTL` | No | `15m` | Must be positive |
| `JWT_REFRESH_TTL` | No | `168h` (7 days) | Must be > access TTL; ≤ 365 days |
| `ORDER_EXPIRY_DURATION` | No | `30m` | Pending order TTL; sweeper flips to `expired` after this |
| `SWEEPER_INTERVAL` | No | `1m` | Must be strictly < `ORDER_EXPIRY_DURATION` |
| `STRIPE_WEBHOOK_SECRET` | No | (empty) | Empty = Stripe webhooks return 404 |
| `WECHAT_PAY_API_V3_KEY` | No | (empty) | 32 bytes; empty = WeChat webhooks return 404 |
| `ALIPAY_PUBLIC_KEY_PATH` | No | (empty) | PEM path; empty = Alipay webhooks return 404 |
| `LEMONSQUEEZY_WEBHOOK_SECRET` | No | (empty) | Empty = LemonSqueezy webhooks return 404 |
| `PAYPAL_ENV` | No | `live` | `sandbox` \| `live`; selects which webhook_id/API base is active |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | No | (empty) | PayPal sandbox webhook ID; empty = sandbox disabled |
| `PAYPAL_WEBHOOK_ID_LIVE` | No | (empty) | PayPal live webhook ID; empty = live disabled |
| `PAYPAL_API_BASE_SANDBOX` | No | `https://api-m.sandbox.paypal.com` | |
| `PAYPAL_API_BASE_LIVE` | No | `https://api-m.paypal.com` | |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | No | (empty) | Reserved for future OAuth redirect flow; not consumed in v1 |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | No | (empty) | Reserved for future OAuth redirect flow; not consumed in v1 |
| `OAUTH_STATE_SECRET` | Yes | (required) | HMAC key for the GitHub OAuth state parameter (`/auth/github/redirect` + `/auth/github/callback`). Server-side only — multi-instance deployments must share the same value. Operators who don't enable GitHub login can set any non-empty value. |
| `PAYPAL_L3_E2E_MODE` | No | (empty) | Dev-only gate for `POST /test/login`. `1` enables the endpoint; any other value (or unset) makes the handler return 404. Used by `tests/e2e-ui/` to mint JWTs without OAuth. |

## API Response Format

All endpoints return:
```json
{"code": <int>, "data": <object|null>, "message": <string|null>}
```

- `code`: 0 on success, HTTP status code on error
- `data`: response payload on success, null on error
- `message`: null on success, error description on error

## Endpoints

**Public** (rate-limited 10/s burst 20 per IP):
- `GET /.well-known/jwks.json`, `POST /auth/login`, `POST /auth/refresh`, `POST /auth/logout`, `GET /apps/:id/plans`
- `GET /auth/github/redirect` and `GET /auth/github/callback` — the GitHub OAuth Authorization Code flow. Yunhou holds the OAuth App's `client_secret` and runs the code exchange server-side; the BFF supplies only `client_id` (via redirect URL) and a `redirect_uri` matching an entry in `apps.config.oauth_providers.github.callback_urls`. See "Boundary" below.
- `POST /test/login` — **dev-only** (gated by `PAYPAL_L3_E2E_MODE=1`, otherwise returns 404). Used by `tests/e2e-ui/` to mint JWTs without going through OAuth. Never exposed in production.

**Health probe** (NOT rate-limited — registered before the public limiter in `internal/router/router.go:35`):
- `GET /healthz` — DB-backed liveness/readiness. Returns 200 `{"code":0,"data":{"status":"ok"}}` or 503 `{"code":503,"message":"db unavailable"}` if the DB ping fails.

**User** (JWT Bearer auth, no explicit rate limit on `/user/*`):
- `GET /user/profile`, `PATCH /user/profile`, `GET /user/identities`, `DELETE /user/identities/:id`, `GET /user/subscriptions`, `POST /user/subscriptions`, `DELETE /user/subscriptions/:id`
- `POST /apps/:id/quote` — JWT-authed quote endpoint mounted at the engine level (NOT under `/user`) so the URL stays `/apps/:id/quote`; the handler reads `user_id` from JWT context but does NOT gate on subscription status — any authenticated user may quote any plan. `currency` is hardcoded `"USD"` in v1.

**Payments & Refunds** (JWT Bearer auth, rate-limited 30/s burst 60 per IP):
- `POST /payments/orders`, `GET /payments/orders/:id`, `DELETE /payments/orders/:id`, `POST /payments/orders/:order_id/confirm`
- `GET /payments`, `GET /payments/:id`, `GET /payments/:id/refunds`
- `POST /refunds`, `GET /refunds/:id`
- New orders write `currency = "CNY"` hardcoded in `internal/service/payment.go:125`. Channel webhooks carry `sub_expires_at` through `payment.metadata`/`resource.sub_expires_at`/`meta.custom_data.sub_expires_at` — yunhou-users does not derive it server-side.

**App management** (internal service auth via `X-App-ID` + `X-App-Secret` headers, rate-limited 30/s burst 60 per IP):
- `GET /apps`, `GET /apps/:id`, `GET /apps/:id/provider-token/:channel` (PayPal OAuth token cached in-process for `expires_in − 60s` — typically ~9h, never shorter than the safety margin; singleflight dedupe avoids N concurrent fetches per `client_id`; LemonSqueezy returns the static `api_key`)
- `GET /admin/plans`, `GET /admin/plans/:id`, `POST /admin/plans`, `PATCH /admin/plans/:id`, `DELETE /admin/plans/:id`
- `POST /admin/apps` (returns plaintext `secret` once — only bcrypt hash is persisted), `PATCH /admin/apps/:id`, `POST /admin/apps/:id/rotate-secret` (returns new plaintext, invalidates the old one immediately)

**Channel webhooks** (signature verification, NOT JWT; rate-limited 200/s burst 400 per IP — looser bucket because traffic is upstream-driven):
- `POST /webhooks/payment/:channel` — `:channel` is `stripe` / `wechat_pay` / `alipay` / `lemonsqueezy`. Returns 404 when the corresponding webhook secret env var is empty.

**Cycle precedence**: when both PayPal and LemonSqueezy are configured for the same `plan_id`, the resolved cycle (and therefore `sub_expires_at`) uses PayPal's `trial_days + billing_cycle_days`. LemonSqueezy's `trial_days` / `billing_cycle_days` are ignored in the quote response — only its `variant_id` is used downstream. Keep PayPal's billing-cycle definition in sync with operator config or the computed `sub_expires_at` will diverge from what PayPal actually bills.

## Design Principles

- Immutable data patterns where appropriate
- Input validation at system boundaries
- No hardcoded secrets — all from env vars
- Parameterized queries only — no string interpolation
- Tokens stored as SHA-256 hashes; app shared secrets (`apps.secret_hash`) stored as bcrypt hashes — bcrypt is slow on purpose to defend against DB leaks
- Server-side secrets (X-App-Secret, refresh tokens) are returned to the caller **exactly once** at create/rotate/login time; only hashes persist. Rotation invalidates the prior value immediately — no grace period.

## GitHub OAuth Boundary

The design principle for user/identity/payment key information: **Yunhou holds it; the website (BFF / consumer app) only handles lightweight business logic and never sees the raw secrets.**

| Key information | Held by | Reachable by |
|---|---|---|
| GitHub OAuth App `client_id` | yunhou-users | Echoed in the upstream `/login/oauth/authorize` URL the BFF redirects to. Public by design (appears in OAuth redirects anyway). |
| GitHub OAuth App `client_secret` | yunhou-users only | **Never** sent to the BFF. Used only inside `/auth/github/callback` to exchange the auth code for an access token. |
| `callback_urls` whitelist | yunhou-users | Compared against the BFF-supplied `redirect_uri` on every callback. Stored as plaintext array in `apps.config.oauth_providers.github.callback_urls` because Yunhou needs the values to construct the upstream URL. Multiple entries allowed (web/iOS/Android sharing one GitHub OAuth App). |
| GitHub `access_token` (after code exchange) | yunhou-users (transient) | Used exactly twice — `/user` then `/user/emails` — then dropped. Never written to DB. Never returned to the BFF. |
| `state` token (CSRF + open-redirect defence) | yunhou-users | HMAC-signed (`OAUTH_STATE_SECRET`); stateless — multi-instance deployment shares the secret. Binds `(app_id, callback_index)`. Expiry 5 min. |
| yunhou's own JWT | yunhou-users | Returned to the BFF via the redirect URL's **fragment** (`#token=...`) — fragment is not sent to servers, so the access_token doesn't leak via referer / logs. |

The legacy `POST /auth/login` direct-token path (`{provider: "github", provider_token: <token>}`) is **removed**: the HTTP handler now rejects `provider=github` with 400 and instructs the caller to use the redirect flow. Google's direct-token path remains for backward compatibility.
