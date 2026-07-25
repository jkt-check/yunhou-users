# CLAUDE.md

This file provides guidance to kscc (claude.ai/code) when working with code in this repository.

## Project Overview

Yunhou Users is a **shared user management API** serving multiple consumer applications via RESTful APIs. All apps share the same user identity — one account per person across all consumers. Authentication is **GitHub OAuth and WeChat Open Platform 网站应用 (QR-code) login** (via `/auth/github/*` and `/auth/wechat/*` redirect flows); there is no email/password registration, no `POST /auth/login`. WeChat is added as a parallel provider that mirrors the GitHub flow verbatim — same JWT issuance, same BFF-facing fragment-based redirect contract. The `social_identities.provider` CHECK constraint already permits `'wechat'`.

## Architecture

- **Shared identity**: One user account across all consumer apps
- **Plan-based access**: Apps are accessible only through an active subscription whose active Plan includes the requested app
- **API-first**: Consumer apps integrate via REST; no server-rendered UI
- **OAuth redirect flow**: Login is via `/auth/{github,wechat}/redirect` → `/auth/{github,wechat}/callback`. Yunhou holds each app's GitHub OAuth App `client_secret` / WeChat Open Platform 网站应用 AppSecret and runs the code exchange server-side. There is no `POST /auth/login` endpoint. The two providers share `util.IssueOAuthState` for the HMAC state token — the state binding is `(appID, callbackIndex)`, provider-agnostic.
- **Subscription-derived authorization**: Login remains available without a usable subscription, but JWT `scope` and `subscription.has_access` are narrowed according to the subscription decision matrix below
- **Refresh token rotation**: Every refresh invalidates the old refresh token and issues a new one

### Commercial plans

Plans are the commercial source of truth. In addition to identity, price, interval, app scope, and active state, each Plan carries `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, nullable `description`, `display_order`, and DB-managed `updated_at`. `currency` is restricted to `CNY` / `USD` / `EUR`, `trial_days` must be non-negative, and quote/order currency plus quote trial duration are derived from the Plan.

`is_listed` controls catalog presentation independently from `accepting_new_subscriptions`, which controls whether new subscriptions/orders may be created. Migration 014 retired the `free` Plan (`is_active=false`, `accepting_new_subscriptions=false`), dropped the `is_default` column, and removed the default-plan fallback. Admin create/patch reject any legacy `is_default` input with 400. Migration 016 re-prices `monthly` (¥19.9/mo) and `yearly` (¥199.9/yr) to match the yunhou-website frontend promo, and fully retires `quarterly` (`is_listed=false`, `is_active=false`) and hides `free` from the public catalog (`is_listed=false`). The public `GET /apps/:id/plans` therefore returns exactly `monthly` + `yearly`.

### Layering

`model` (structs) → `repo` (SQL, `*sqlx.DB`) → `service` (business logic) → `handler` (HTTP I/O) → `router` (wiring)

All repos are interface-based (`repo.UserRepo`, etc.) for testability. Handler tests use hand-rolled mock structs with function fields — no external mocking libraries.

### Auth Flow

1. `GET /auth/{github,wechat}/redirect` → `GET /auth/{github,wechat}/callback` — the GitHub OAuth / WeChat Open Platform 网站应用 redirect flows. Yunhou holds each app's `client_secret` (GitHub) / `app_secret` (WeChat) and runs the code exchange server-side. The callback redirects to the BFF with the JWT in the URL fragment. WeChat additionally requires `unionid` from `/sns/userinfo`; logins without it are rejected with `reason=wechat_no_unionid`.
2. `POST /auth/refresh` — exchanges refresh token for new tokens (checks subscription, rotates refresh token)
3. `POST /auth/logout` — revokes refresh token
4. Consumer apps verify access tokens locally via `GET /.well-known/jwks.json` (RSA256 JWK, kid=`yunhou-users-rsa`)

**Plan-based access decision matrix**:

| Subscription state | JWT `scope` | `subscription.plan_id` | `subscription.has_access` |
|---|---|---|---|
| No subscription | `[]` | Empty / null-equivalent | `false` |
| Expired subscription | `[]` | Historical Plan ID preserved | `false` |
| Active subscription, active Plan | `plan.apps` | Current Plan ID | `true` only when `plan.apps` contains the requested `app_id`; otherwise `false` |
| Active subscription, inactive Plan | `[]` | Current Plan ID preserved | `false` |

`subscription.is_accepting_new` is always present. It is `true` only when the selected/historical Plan is active and has `accepting_new_subscriptions=true`; it does not itself grant access.

### JWT Claims

| Claim | Type | Description |
|---|---|---|
| `sub` | string | User UUID |
| `iss` | string | Issuer; server-validated as `"yunhou-users"` |
| `aud` | string array | App IDs the token is valid for; server-validated to include the requested `app_id` |
| `app_id` | string | App ID the token was issued for (matches `aud[0]`) |
| `scope` | string array | Apps from the user's active Plan; empty for no subscription, expired subscriptions, or inactive Plans |
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
- Database migration: apply `001_init.sql` through `016_plan_pricing_and_hide.sql` in numeric order (each depends on the prior; running out of order fails). Migration 012 adds the commercial Plan fields and the `plan_change_log` audit table; migration 013 relaxes the audit-log FK to `ON DELETE SET NULL`; migration 014 retires `free` and drops `is_default` + `plans_one_default`; migration 015 makes `plan_change_log.before`/`after` nullable so CreatePlan (`before=NULL`) and DeletePlan (`after=NULL`) can write audit rows per spec §6.1; migration 016 re-prices `monthly`/`yearly` to ¥19.9/¥199.9 and retires `quarterly` + hides `free` from the public catalog (see `docs/superpowers/specs/2026-07-25-plan-pricing-realignment-design.md`). Migrations 008 (removes `lemonsqueezy`) and the Phase-2 014+015 default-plan removal must deploy with the corresponding code that no longer references `is_default` / `FindDefault` / default-plan fallback. After applying 007, run the server once so `BackfillAppSecrets` populates `secret_hash` for pre-existing app rows; capture the plaintexts from the deploy log and rotate them via `POST /admin/apps/:id/rotate-secret`.

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
| `WECHAT_PAY_API_V3_KEY` | Required when enabling real-mode WeChat Pay | (empty) | Exactly 32 bytes in real mode; part of the six-field all-or-none tuple below. All six may be empty when real WeChat Pay is disabled. |
| `WECHAT_PAY_MCH_ID` | Required when enabling real-mode WeChat Pay | (empty) | 微信支付商户号; part of the six-field all-or-none tuple enforced by `Config.Validate`. |
| `WECHAT_PAY_APP_ID` | Required when enabling real-mode WeChat Pay | (empty) | WeChat Open Platform 网站应用 appid, written into the v3 NATIVE `UnifiedOrder` request body as `appid`. Part of the six-field tuple. |
| `WECHAT_PAY_NOTIFY_URL` | Required when enabling real-mode WeChat Pay | (empty) | Public callback URL passed to `UnifiedOrder` so WeChat knows where to POST async notifications. Part of the six-field tuple. |
| `WECHAT_PAY_MCH_PRIVATE_KEY_PATH` | Required when enabling real-mode WeChat Pay | (empty) | PEM path for the merchant's RSA private key (PKCS#1 or PKCS#8); signs every outbound `UnifiedOrder`. Part of the six-field tuple. |
| `WECHAT_PAY_MCH_CERT_PATH` | Required when enabling real-mode WeChat Pay | (empty) | PEM path for the merchant's X.509 certificate; serial (UPPERCASE HEX) goes into the outbound `Authorization` `serial_no`. Part of the six-field tuple. |
| `WECHAT_PAY_MOCK` | No | (empty) | `1` enables mock WeChat Pay; mock mode may leave the six real-mode fields empty or partially populated. Never enable in production. |
| `WECHAT_OAUTH_MOCK` | No | (empty) | `1` short-circuits WeChat OAuth upstream calls for development/testing. Never enable in production. |
| `ALIPAY_PUBLIC_KEY_PATH` | No | (empty) | PEM path; empty = Alipay webhooks return 404 |
| `PAYPAL_ENV` | No | `live` | `sandbox` \| `live`; selects which webhook_id/API base is active |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | No | (empty) | PayPal sandbox webhook ID; empty = sandbox disabled |
| `PAYPAL_WEBHOOK_ID_LIVE` | No | (empty) | PayPal live webhook ID; empty = live disabled |
| `PAYPAL_API_BASE_SANDBOX` | No | `https://api-m.sandbox.paypal.com` | |
| `PAYPAL_API_BASE_LIVE` | No | `https://api-m.paypal.com` | |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | No | (empty) | Not consumed by the running redirect flow — `/auth/github/*` reads each app's GitHub OAuth App credentials from `apps.config.oauth_providers.github` in the DB. Safe to leave blank. |
| `OAUTH_STATE_SECRET` | Yes | (required) | HMAC key for the OAuth `state` parameter (`/auth/{github,wechat}/redirect` + `/auth/{github,wechat}/callback`). The state binding is provider-agnostic — both flows share `util.IssueOAuthState` and bind `(app_id, callback_index)`. Server-side only — multi-instance deployments must share the same value. **Minimum 32 characters** — `Config.Validate` rejects shorter values. Generate with `openssl rand -hex 32`. |
| `PAYPAL_L3_E2E_MODE` | No | (empty) | Dev-only gate for `POST /test/login?plan_id=<plan-id>`. `1` enables the endpoint; any other value (or unset) makes the handler return 404. Every enabled request must supply an explicit Plan ID. Used by `tests/e2e-ui/` to mint JWTs without OAuth. |

## API Response Format

JSON business endpoints generally return:
```json
{"code": <int>, "data": <object|null>, "message": <string|null>}
```

- `code`: 0 on success, HTTP status code on error
- `data`: response payload on success, null on error
- `message`: null on success, error description on error

Exceptions: `GET /.well-known/jwks.json` returns a raw JWKS object, while successful OAuth redirect/callback endpoints return HTTP 302 redirects rather than JSON.

## Endpoints

**Public** (rate-limited 10/s burst 20 per IP):
- `GET /.well-known/jwks.json`, `POST /auth/refresh`, `POST /auth/logout`
- `GET /apps/:id/plans` — returns active and listed Plans (`is_active=true AND is_listed=true`) as `PublicPlan` DTOs, including `currency`, `trial_days`, nullable `description`, and `display_order` alongside price/app/provider/cycle data.
- `GET /auth/github/redirect` and `GET /auth/github/callback` — the GitHub OAuth Authorization Code flow. Yunhou holds the OAuth App's `client_secret` and runs the code exchange server-side; the BFF supplies Yunhou `app_id` and a whitelisted `redirect_uri`; Yunhou loads the OAuth credentials from the app configuration. See "Boundary" below.
- `GET /auth/wechat/redirect` and `GET /auth/wechat/callback` — the WeChat Open Platform 网站应用 (QR-code) OAuth2.0 flow. Same posture as GitHub: Yunhou holds each app's `app_secret` and runs the code exchange server-side. The authorize URL is `open.weixin.qq.com/connect/qrconnect` (with mandatory `#wechat_redirect` fragment), and `redirect_uri` must match an entry in `apps.config.oauth_providers.wechat.callback_urls`. Yunhou REJECTS logins where `/sns/userinfo` lacks `unionid` (returned as `#error=auth_failed&reason=wechat_no_unionid`).
- `POST /test/login?plan_id=<plan-id>` — **dev-only** (gated by `PAYPAL_L3_E2E_MODE=1`, otherwise returns 404). `plan_id` is required; missing returns 400, unknown returns 404, inactive Plans return 400, while non-accepting Plans return 409 with `plan is not accepting new subscriptions`. Used by `tests/e2e-ui/` and `tests/integration/` to mint JWTs without going through OAuth. Never exposed in production.

**Health probe** (NOT rate-limited — registered before the public limiter in `router.Setup`):
- `GET /healthz` — DB-backed liveness/readiness. Returns 200 `{"code":0,"data":{"status":"ok"}}` or 503 `{"code":503,"message":"db unavailable"}` if the DB ping fails.

**User** (JWT Bearer auth, no explicit rate limit on `/user/*`):
- `GET /user/profile`, `PATCH /user/profile`, `GET /user/identities`, `DELETE /user/identities/:id`, `GET /user/subscriptions`, `POST /user/subscriptions`, `DELETE /user/subscriptions/:id`
- `POST /apps/:id/quote` — JWT-authed quote endpoint mounted at the engine level (NOT under `/user`) so the URL stays `/apps/:id/quote`; the handler reads `user_id` from JWT context but does NOT gate on subscription status — any authenticated user may quote any plan. `currency` and `cycle_config.trial_days` are derived from the Plan.

**Payments & Refunds** (JWT Bearer auth, rate-limited 30/s burst 60 per IP):
- `POST /payments/orders`, `GET /payments/orders/:id`, `DELETE /payments/orders/:id`, `POST /payments/orders/:order_id/confirm`
- `GET /payments`, `GET /payments/:id`, `GET /payments/:id/refunds`
- `POST /refunds`, `GET /refunds/:id`
- New orders snapshot `plan.price` and `plan.currency`; PayPal requires a USD Plan and WeChat Pay requires a CNY Plan (`ErrPlanCurrencyMismatch` otherwise). Channel webhooks carry `sub_expires_at` through `payment.metadata`/`resource.sub_expires_at`/`meta.custom_data.sub_expires_at` — yunhou-users does not derive it server-side.

**App management** (internal service auth via `X-App-ID` + `X-App-Secret` headers, rate-limited 30/s burst 60 per IP):
- `GET /apps`, `GET /apps/:id`, `GET /apps/:id/provider-token/:channel` (PayPal OAuth token cached in-process for `expires_in − 60s` — typically ~9h, never shorter than the safety margin; singleflight dedupe avoids N concurrent fetches per `client_id`)
- `GET /admin/plans`, `GET /admin/plans/:id`, `POST /admin/plans`, `PATCH /admin/plans/:id`, `DELETE /admin/plans/:id`; create/patch accept the commercial fields but reject any `is_default` input with 400
- `POST /admin/apps` (returns plaintext `secret` once — only bcrypt hash is persisted), `PATCH /admin/apps/:id`, `POST /admin/apps/:id/rotate-secret` (returns new plaintext, invalidates the old one immediately)

**Channel webhooks** (signature verification, NOT JWT; rate-limited 200/s burst 400 per IP — looser bucket because traffic is upstream-driven):
- `POST /webhooks/payment/:channel` — `:channel` is `stripe` / `wechat_pay` / `alipay` / `paypal`. Returns 404 when the corresponding webhook secret env var is empty. Unknown channels also return 404 (defence-in-depth — the CHECK constraint is the backstop).

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

The legacy `POST /auth/login` direct-token path has been **removed entirely** — GitHub and WeChat are the only login providers, both via the redirect flow described above.

## WeChat OAuth Boundary

The same boundary applies to WeChat Open Platform 网站应用 (website app) login. The flow uses `open.weixin.qq.com/connect/qrconnect` for the user-facing QR-code login, then exchanges the auth code at `api.weixin.qq.com/sns/oauth2/access_token` and fetches the user profile at `api.weixin.qq.com/sns/userinfo`.

| Key information | Held by | Reachable by |
|---|---|---|
| WeChat Open Platform 网站应用 AppID | yunhou-users | Echoed in the upstream `open.weixin.qq.com/connect/qrconnect` URL. Public by design. |
| WeChat Open Platform 网站应用 AppSecret | yunhou-users only | **Never** sent to the BFF. Used only inside `/auth/wechat/callback` to exchange the auth code for an access token. |
| `callback_urls` whitelist | yunhou-users | Compared against the BFF-supplied `redirect_uri` on every callback. Stored as plaintext array in `apps.config.oauth_providers.wechat.callback_urls`. |
| WeChat `access_token` (after code exchange) | yunhou-users (transient) | Used exactly once — `/sns/userinfo` — then dropped. Never written to DB. Never returned to the BFF. |
| WeChat `refresh_token` (after code exchange) | yunhou-users (transient) | Discarded. Yunhou has its own refresh-token model; the WeChat refresh_token has no use beyond refreshing the WeChat access_token, which Yunhou does not need post-login. |

**Identity key:** `social_identities.provider_uid = "wechat_" + unionid`. Yunhou REJECTS logins where `/sns/userinfo` does not return `unionid`. The 网站应用 flow requests only `scope=snsapi_login`; a missing `unionid` generally indicates that the website app is not bound under the same WeChat Open Platform account required for cross-app identity unification. This rejects per-app identity fragmentation — without unionid, the same WeChat user across two Yunhou consumer apps would get two Yunhou accounts.

**Cross-app unionid unification** requires all Yunhou consumer apps to register their WeChat 网站应用 under the SAME 微信开放平台 account. This is a Tencent-side requirement, not enforced in code; operators document it in the consumer-app onboarding runbook.
