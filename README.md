# Yunhou Users

A shared user management API for multi-app ecosystems. One user identity across all consumer applications, with plan-based subscriptions.

## Features

- **Social OAuth** — GitHub uses the OAuth Authorization Code flow (`/auth/github/redirect` → `/auth/github/callback`). WeChat Open Platform 网站应用 uses QR-code login (`/auth/wechat/redirect` → `/auth/wechat/callback`, via `open.weixin.qq.com/connect/qrconnect`). Yunhou holds each app's GitHub `client_secret` / WeChat `app_secret` and runs the code exchange server-side. State tokens are shared between providers — `(app_id, callback_index)` HMAC binding.
- **Plan-based access** — Plans define which apps a user can access
- **RSA256 JWT** access tokens with JWKS public key endpoint
- **Subscription gating** — subscription state does not block token issuance: the JWT gets `scope=[]`, while the login/refresh response and OAuth callback fragment report `has_access=false` when the user has no active subscription
- **Refresh token rotation** — one-time-use refresh tokens
- **Rate limiting** — per-IP token bucket (10/s burst 20 on public, 30/s burst 60 on app management)

## Dev mock mode

Set `WECHAT_OAUTH_MOCK=1` to short-circuit the WeChat OAuth redirect and callback without contacting `open.weixin.qq.com`. Useful for local dev and CI e2e suites that don't have a registered WeChat 网站应用.

- `GET /auth/wechat/redirect` returns a 302 to `redirect_uri?code=mock-code&state=<real-HMAC-state>` (query parameters, no upstream call).
- The BFF forwards `app_id`, `code`, and `state` to `GET /auth/wechat/callback?app_id=<app-id>&code=mock-code&state=<...>`, which constructs a fixed `ProviderUserInfo` (unionid `wechat_mock-unionid-001`) and runs the normal login pipeline.
- Mock mode does **not** bypass the HMAC state defence — only the upstream WeChat HTTP round-trip is skipped.

**Never enable in production**; the constant unionid means anyone with knowledge of the mock sentinel can impersonate a fixed account.

Set `WECHAT_PAY_MOCK=1` to drive the WeChat Pay v3 webhook flow without a registered merchant. The `WeChatPayV3Verifier` short-circuits the HMAC check (still requires all three headers + a fresh timestamp), and the webhook handler accepts plaintext JSON (no AES-GCM resource decryption). The downstream `PaymentService.OnWebhook` path is identical to prod, so the order-paid → subscription-activated flow can be exercised end-to-end.

**Never enable in production** — anyone could POST a fake paid event for any order.

## Quick Start

```bash
# 1. Set up PostgreSQL
createdb yunhou_users

# 2. Apply migrations. The cmd/migrate binary owns the _migrations
#    ledger so re-running is a no-op. See migrations/README.md for the
#    naming + DDL rules each migration must follow.
make migrate           # apply pending
make migrate-status    # inspect ledger (✅ applied / ⏳ pending)

# 3. Generate RSA keys
make generate-keys

# 4. Configure required OAuth state signing secret
cp .env.example .env
# Set OAUTH_STATE_SECRET in .env to the output of:
openssl rand -hex 32

# 5. Run — startup backfills apps.secret_hash for any pre-existing rows
#    and prints the plaintexts to stdout (capture them, then rotate each
#    app's secret via POST /admin/apps/:id/rotate-secret).
make run

# 6. Login uses the redirect flow. Open in a browser:
#   GitHub: GET /auth/github/redirect?app_id=yundian&redirect_uri=https://yundian.com/auth/callback
#     After consent GitHub redirects to /auth/github/callback which 302s back
#     to https://yundian.com/auth/callback#token=...&refresh_token=...&user_id=...
#   WeChat: GET /auth/wechat/redirect?app_id=yundian&redirect_uri=https://yundian.com/auth/wechat-callback
#     Renders a QR code from open.weixin.qq.com/connect/qrconnect. After
#     the user scans + confirms on the WeChat mobile app, yunhou fetches
#     /sns/userinfo (requires unionid), then 302s back with the same
#     fragment shape. Rejected as #error=auth_failed&reason=wechat_no_unionid
#     if the userinfo response lacks unionid.
```

## Configuration

All configuration is via environment variables (or `.env` file):

| Variable | Required | Default | Notes |
|---|---|---|---|
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` | |
| `PORT` | No | `8080` | |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` | |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` | |
| `OAUTH_STATE_SECRET` | **Yes** | (required) | HMAC key for the OAuth `state` parameter (provider-agnostic — both `/auth/github/*` and `/auth/wechat/*` share it). Binds `(app_id, callback_index)`. **Minimum 32 characters** — server startup rejects shorter values. Generate with `openssl rand -hex 32`. Multi-instance must share the same value. |
| `JWT_ACCESS_TTL` | No | `15m` | Must be positive |
| `JWT_REFRESH_TTL` | No | `168h` (7 days) | Must be > access TTL; ≤ 365 days |
| `ORDER_EXPIRY_DURATION` | No | `30m` | Pending order expiry; sweeper flips to `expired` after this |
| `SWEEPER_INTERVAL` | No | `1m` | Must be strictly < `ORDER_EXPIRY_DURATION` |
| `STRIPE_WEBHOOK_SECRET` | No | (empty) | Empty = Stripe webhooks return 404 |
| `WECHAT_PAY_API_V3_KEY` | Required when enabling real-mode WeChat Pay | (empty) | Exactly 32 bytes in real mode; part of the six-field all-or-none tuple. All six may be empty when real WeChat Pay is disabled. |
| `WECHAT_PAY_MOCK` | No | (empty) | `1` enables WeChat Pay v3 webhook plaintext mode (skips HMAC match + AES decrypt); empty / `0` = production. Pairs with `WECHAT_PAY_MCH_ID` (required when not in mock mode). |
| `WECHAT_PAY_MCH_ID` | Required when enabling real-mode WeChat Pay | (empty) | 微信支付商户号. When `WECHAT_PAY_MOCK` is not `1`, the six real-mode fields must be all set or all empty; all empty disables real WeChat Pay. Mock mode may leave them empty or partially populated. |
| `WECHAT_PAY_APP_ID` | Required when enabling real-mode WeChat Pay | (empty) | WeChat Open Platform 网站应用 appid, written into the v3 NATIVE `UnifiedOrder` request body as `appid`. Part of the six-field production tuple (see `WECHAT_PAY_MCH_ID`). |
| `WECHAT_PAY_NOTIFY_URL` | Required when enabling real-mode WeChat Pay | (empty) | Public callback URL (e.g. `https://host/webhooks/payment/wechat_pay`) passed to `UnifiedOrder` so WeChat knows where to POST async notifications. Part of the six-field production tuple (see `WECHAT_PAY_MCH_ID`). |
| `WECHAT_PAY_MCH_PRIVATE_KEY_PATH` | Required when enabling real-mode WeChat Pay | (empty) | PEM file path for the merchant's RSA private key (PKCS#1 or PKCS#8). Signs every outbound `UnifiedOrder` request. Part of the six-field production tuple (see `WECHAT_PAY_MCH_ID`). |
| `WECHAT_PAY_MCH_CERT_PATH` | Required when enabling real-mode WeChat Pay | (empty) | PEM file path for the merchant's X.509 certificate. Its serial number (UPPERCASE HEX) goes into the outbound `Authorization` header `serial_no`. Part of the six-field production tuple (see `WECHAT_PAY_MCH_ID`). |
| `PAYPAL_L3_E2E_MODE` | No | (empty) | Dev-only gate for `POST /test/login?plan_id=<plan-id>`. Set to `1` to enable; any other value (or unset) makes the handler return 404. Every enabled request must supply an explicit Plan ID. Used by `tests/e2e-ui/` and `tests/integration/` to mint JWTs without OAuth. |
| `WECHAT_OAUTH_MOCK` | No | (empty) | `1` short-circuits `/auth/wechat/*` (no upstream `open.weixin.qq.com` call); empty / `0` = production. Never enable in prod. |
| `ALIPAY_PUBLIC_KEY_PATH` | No | (empty) | PEM file path; empty = Alipay webhooks return 404 |
| `PAYPAL_ENV` | No | (空=禁用) | `sandbox` \| `live`; selects which PayPal webhook_id/base URL is active. 空值=未启用（渠道返回 404），仅在真正启用 PayPal 时设值 |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | No | (empty) | Empty = PayPal sandbox webhooks return 404 |
| `PAYPAL_WEBHOOK_ID_LIVE` | No | (empty) | Empty = PayPal live webhooks return 404 |
| `PAYPAL_API_BASE_SANDBOX` | No | `https://api-m.sandbox.paypal.com` | |
| `PAYPAL_API_BASE_LIVE` | No | `https://api-m.paypal.com` | |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | No | (empty) | Not consumed by the running redirect flow — `/auth/github/*` reads each app's GitHub OAuth App credentials from `apps.config.oauth_providers.github` in the DB. Safe to leave blank. |
| `PLAN_AMOUNT_OVERRIDE_JSON` | No | (empty) | Optional `{plan_id: amount-yuan}` map for dev/staging payment flows, e.g. `{"monthly":0.01,"yearly":0.1}` — overrides `plans.price` at runtime without a migration. See `internal/service/price_override.go`. |
| `DEEPSEEK_API_KEY` | No (empty = chat disabled) | (empty) | Server-side DeepSeek API key backing `POST /chat`. Empty = the endpoint returns 404 (same convention as empty webhook secrets). Never shipped to consumer apps — they call `/chat` with a user JWT. |
| `DEEPSEEK_BASE_URL` | No | `https://api.deepseek.com` | OpenAI-compatible API origin; the service appends `/chat/completions`. |
| `DEEPSEEK_MODEL` | No | `deepseek-v4-flash` | Model name sent in the upstream `chat.completions` body. |
| `CHAT_LOG_PATH` | No | (empty) | Chat access audit log file: one JSON line per request (`user_id`, `app_id`, `session_id`, input messages, parsed output text, status, duration). Empty = disabled (endpoint still works, no audit trail). File is opened 0o600 (conversation content is PII). Server fails to start if the path is set but unopenable. |

## API Overview

> Full BFF/consumer-app integration contract (endpoint-by-endpoint request/response shapes, OAuth flows, error codes, rate limits, quick-start checklist): **[docs/api-integration-guide.md](docs/api-integration-guide.md)**. Deployment ops: [docs/deployment.md](docs/deployment.md).

### Public Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness / readiness probe. **Not rate-limited.** Returns 200 `{"code":0,"data":{"status":"ok"}}` or 503 `{"code":503,"message":"db unavailable"}` |
| GET | `/.well-known/jwks.json` | RSA public key (JWK format) |
| GET | `/auth/github/redirect` | Begin the GitHub OAuth Authorization Code flow (302 to GitHub). Requires `app_id` + `redirect_uri` matching the app's configured whitelist. |
| GET | `/auth/github/callback` | GitHub redirects here after consent. Yunhou exchanges the code server-side and returns its JWT in the **URL fragment** (`#token=...&refresh_token=...&user_id=...`) so the access token never leaves the browser. |
| GET | `/auth/wechat/redirect` | Begin the WeChat Open Platform 网站应用 QR-code login (302 to `open.weixin.qq.com/connect/qrconnect`). Same `app_id` + `redirect_uri` shape as GitHub. |
| GET | `/auth/wechat/callback` | WeChat redirects here after the user scans + confirms in the WeChat mobile app. Yunhou exchanges the code at `/sns/oauth2/access_token`, fetches `/sns/userinfo` (requires `unionid`), then returns its JWT in the URL fragment. Rejected with `#error=auth_failed&reason=wechat_no_unionid` if unionid is missing. |
| POST | `/auth/refresh` | Refresh tokens |
| POST | `/auth/logout` | Logout (revoke refresh token) |
| POST | `/test/login` | **Dev-only** — returns 404 unless `PAYPAL_L3_E2E_MODE=1`. Used by `tests/e2e-ui/` to mint JWTs without OAuth. |
| GET | `/apps/:id/plans` | Public plan catalog (price + provider plan IDs + cycle) |

### User Endpoints (JWT Bearer)

| Method | Path | Description |
|---|---|---|
| GET | `/user/profile` | Get current user profile |
| PATCH | `/user/profile` | Update profile fields |
| GET | `/user/identities` | List linked social identities |
| DELETE | `/user/identities/:id` | Unbind a social identity |
| GET | `/user/subscriptions` | List user's subscriptions |
| POST | `/user/subscriptions` | Create subscription |
| DELETE | `/user/subscriptions/:id` | Cancel subscription |
| POST | `/apps/:id/quote` | Quote a plan for an app (amount, sub_expires_at, per-channel provider_data). JWT-authed; mounted at the engine level so the URL stays `/apps/:id/quote`. |
| POST | `/chat` | Chat proxy for consumer apps (kaya): relays `{"messages":[{"role","content"}...]}` to DeepSeek (`deepseek-v4-flash`) and streams the SSE answer back verbatim. JWT-authed + subscription-gated (active subscription whose active plan's `apps[]` contains the JWT `app_id`); requires `DEEPSEEK_API_KEY` (404 otherwise). Exempt from the 20s request timeout; bounded by `chatUpstreamTimeout` (5m). Errors before the stream are JSON `{code,data,message}`; a `200` response is `text/event-stream`. |

### Payments & Refunds (JWT Bearer)

| Method | Path | Description |
|---|---|---|
| POST | `/payments/orders` | Create a pending order (snapshots `plan.price` + `plan.currency`; PayPal requires USD Plan, WeChat Pay requires CNY Plan, else `ErrPlanCurrencyMismatch`) |
| GET | `/payments/orders` | List the current user's orders (newest first) |
| GET | `/payments/orders/:id` | Fetch an order (pending / paid / failed / expired / refunded / cancelled) |
| DELETE | `/payments/orders/:id` | Cancel a pending order |
| POST | `/payments/orders/:order_id/confirm` | BFF-confirmed payment completion; activates the subscription on success |
| GET | `/payments` | List payments |
| GET | `/payments/:id` | Fetch a payment |
| GET | `/payments/:id/refunds` | List refunds for a payment |
| POST | `/refunds` | Create a refund |
| GET | `/refunds/:id` | Fetch a refund |

### Channel Webhooks (signature verification, NOT JWT)

| Method | Path | Description |
|---|---|---|
| POST | `/webhooks/payment/:channel` | `:channel` is `stripe` / `wechat_pay` / `alipay` / `paypal`. Returns 404 when the corresponding webhook secret env var is empty; unknown channels also return 404 (defence-in-depth). Most channels ship `sub_expires_at` in the payload (Stripe `metadata`, PayPal `resource.billing_info.next_billing_time` or `custom_data`, Alipay `passback_params`); yunhou-users trusts them verbatim. WeChat v3 NATIVE does NOT carry `sub_expires_at` — when missing, the webhook (and `POST /payments/orders/:id/confirm`) falls back to `plan.interval_days` (`time.Now() + interval_days*24h`) and audit-logs `subscription_expiry_plan_missing` if the plan row is gone. PayPal renewal is a deliberate exception: missing `next_billing_time` is treated as a contract drift, audit-logged, and the sub is NOT extended. |

### App/Plan Management Endpoints (Internal Auth)

| Method | Path | Description |
|---|---|---|
| GET | `/apps` | List all apps |
| GET | `/apps/:id` | Get app details |
| GET | `/apps/:id/provider-token/:channel` | Fetch upstream credential for `paypal` (OAuth token cached in-process for `expires_in − 60s`) |
| GET | `/admin/plans` | List all plans |
| GET | `/admin/plans/:id` | Get plan details |
| POST | `/admin/plans` | Create plan |
| PATCH | `/admin/plans/:id` | Update plan |
| DELETE | `/admin/plans/:id` | Delete plan |
| POST | `/admin/apps` | Create app (returns plaintext `secret` once — only bcrypt hash is persisted) |
| PATCH | `/admin/apps/:id` | Update app (cannot change `secret` — use rotate-secret) |
| POST | `/admin/apps/:id/rotate-secret` | Generate a new shared secret, invalidate the old one immediately |

**Auth flavors**

- `GET /apps/:id/plans` — **public**, no header, no JWT.
- `POST /apps/:id/quote` — **JWT Bearer**. The quote is computed per-user (JWT identifies `user_id`); mounted at the engine level so it does not collide with the `X-App-ID` wrapper around the other `/apps/:id/*` routes.
- `GET /apps/:id/provider-token/:channel` and every `/admin/*` route — **internal service auth** (`X-App-ID` + `X-App-Secret` headers). BFF calls these with its own service credentials; never expose to end users. `X-App-Secret` is the shared secret — returned in plaintext once at creation/rotation time; the database stores only its bcrypt hash. Losing it requires a rotation.

User endpoints (`/user/*`, `/payments/*`, `/refunds/*`) require JWT Bearer only.

### Commercial currency and cycle behavior

- Quote and order currency come from `plan.currency`; orders snapshot `plan.price` and `plan.currency`. PayPal requires USD and WeChat Pay requires CNY.
- The quote endpoint derives `sub_expires_at = now + plan.trial_days + plan.interval_days` from the Plan. The BFF should embed that server-produced value in checkout metadata. Channels that carry `sub_expires_at` (Stripe metadata, PayPal custom_data, Alipay passback) echo it back and yunhou-users stores it verbatim. **WeChat v3 NATIVE does not carry `sub_expires_at`** — when the BFF omits it from `/payments/orders/:id/confirm` and the channel webhook lacks it, yunhou-users falls back to `time.Now() + plan.interval_days*24h` (or `NULL` if the plan row was deleted — see `migrations/017_sub_expiry_does_not_backfill.sql`).
- `PublicPlan.cycle` is only a summary of configured provider-side trial and billing-cycle values for catalog display and reconciliation. It does not control quote calculation; quote behavior uses the top-level Plan fields. Operators should keep provider billing definitions aligned with the Plan.
- `POST /apps/:id/quote` requires JWT but does **not** enforce `has_access` against the user's subscription. Any authenticated user can quote any plan any app exposes.

## Authentication Flow

Two providers, both via the OAuth redirect flow (there is no `/auth/login` endpoint):

**GitHub** (`/auth/github/redirect` → `/auth/github/callback`):
```
Browser            Yunhou Users API               GitHub            Consumer App (BFF)
    |-- GET /auth/github/redirect?app_id=...&redirect_uri=... --> |
    |<-- 302 to github.com/login/oauth/authorize?... --------- |
    |-- consent on github.com ---------------------------> |
    |<-- 302 to /auth/github/callback?code=...&state=... -- |
    |                                              |-- exchange code (server-side, uses apps.config.oauth_providers.github.client_secret) --> GitHub
    |                                              |<-- access_token, user, emails
    |<-- 302 to redirect_uri#token=...&refresh_token=...&user_id=...&has_access=...
    |     (fragment never reaches the server; the BFF parses it client-side)
```

**WeChat** (`/auth/wechat/redirect` → `/auth/wechat/callback`) — QR-code login:
```
Browser            Yunhou Users API              WeChat Open           Consumer App (BFF)
    |-- GET /auth/wechat/redirect?app_id=...&redirect_uri=... --> |
    |<-- 302 to open.weixin.qq.com/connect/qrconnect?...#wechat_redirect -- |
    |-- render QR code; user scans + confirms on WeChat mobile app ---> |
    |<-- 302 to /auth/wechat/callback?code=...&state=...&app_id=... -- |
    |                                              |-- exchange code at /sns/oauth2/access_token (uses apps.config.oauth_providers.wechat.app_secret) --> WeChat
    |                                              |-- fetch profile at /sns/userinfo; REQUIRE unionid --> WeChat
    |                                              |<-- openid, unionid, nickname, headimgurl
    |<-- 302 to redirect_uri#token=...&refresh_token=...&user_id=...&has_access=...
    |     (same fragment shape as GitHub; or #error=auth_failed&reason=wechat_no_unionid if unionid missing)
```

> State tokens are shared between providers (`util.IssueOAuthState` binds `(app_id, callback_index)`, 5-minute expiry). `OAUTH_STATE_SECRET` is provider-agnostic.
> 
> **Cross-app unionid unification** requires all Yunhou consumer apps to register their WeChat 网站应用 under the **same** 微信开放平台 account (Tencent-side requirement, not enforced in code).

## Development

```bash
make build          # Compile binary
make test           # Run unit tests with race detection
make e2e            # Run E2E tests (requires local PostgreSQL)
make lint           # Run go vet
make deps           # Tidy dependencies
```

Run a single test:
```bash
go test -race -run TestAuthService_Login ./internal/service/
```

Database migration:
```bash
# Run in order. Each migration depends on the prior.
# Recommended: use `make migrate` (cmd/migrate binary) instead of running psql
# manually — it owns the _migrations ledger so re-running is a no-op.
# See migrations/README.md for the current file table (001 through 019 as of
# this writing) and the DDL rules each migration must follow.
psql -d yunhou_users -f migrations/001_init.sql
psql -d yunhou_users -f migrations/002_simplify_plans.sql
psql -d yunhou_users -f migrations/003_payments.sql
psql -d yunhou_users -f migrations/004_ls_channel.sql
psql -d yunhou_users -f migrations/005_paypal_channel.sql
psql -d yunhou_users -f migrations/006_paypal_sub_mapping.sql
psql -d yunhou_users -f migrations/007_app_secret.sql
psql -d yunhou_users -f migrations/008_drop_lemonsqueezy.sql
psql -d yunhou_users -f migrations/009_wechat_pay_intent.sql
psql -d yunhou_users -f migrations/010_provider_intent_nullable.sql
psql -d yunhou_users -f migrations/011_order_reconcile.sql
psql -d yunhou_users -f migrations/012_plan_commercial_fields.sql
psql -d yunhou_users -f migrations/013_plan_change_log_fk_set_null.sql
psql -d yunhou_users -f migrations/014_remove_default_plan.sql
psql -d yunhou_users -f migrations/015_plan_change_log_nullable_snapshots.sql
psql -d yunhou_users -f migrations/016_plan_pricing_and_hide.sql
psql -d yunhou_users -f migrations/017_sub_expiry_does_not_backfill.sql
psql -d yunhou_users -f migrations/018_trial_plan.sql
psql -d yunhou_users -f migrations/019_kaya_bridge.sql
```

## Tech Stack

- Go 1.25 + Gin
- PostgreSQL + sqlx (no ORM)
- RSA256 JWT + JWKS
- SHA-256 token hashing (refresh tokens); bcrypt app-secret hashing (`apps.secret_hash`)
