# Yunhou Users

A shared user management API for multi-app ecosystems. One user identity across all consumer applications, with plan-based subscriptions.

## Features

- **Social OAuth only** — GitHub, Google (provider token sent directly)
- **Plan-based access** — Plans define which apps a user can access
- **RSA256 JWT** access tokens with JWKS public key endpoint
- **Subscription gating** — tokens only issued for active subscriptions based on plan
- **Refresh token rotation** — one-time-use refresh tokens
- **Rate limiting** — per-IP token bucket (10/s burst 20 on public, 30/s burst 60 on app management)

## Quick Start

```bash
# 1. Set up PostgreSQL
createdb yunhou_users
# Run ALL migrations in order — 002 alters tables created by 001, 003
# adds payment/webhook tables, 005 adds apps.secret_hash. Each depends on
# the prior; running out of order will fail.
psql -d yunhou_users -f migrations/001_init.sql
psql -d yunhou_users -f migrations/002_simplify_plans.sql
psql -d yunhou_users -f migrations/003_payments.sql
psql -d yunhou_users -f migrations/004_ls_channel.sql
psql -d yunhou_users -f migrations/005_paypal_channel.sql
psql -d yunhou_users -f migrations/006_paypal_sub_mapping.sql
psql -d yunhou_users -f migrations/007_app_secret.sql

# 2. Generate RSA keys
make generate-keys

# 3. Run — startup backfills apps.secret_hash for any pre-existing rows
#    and prints the plaintexts to stdout (capture them, then rotate each
#    app's secret via POST /admin/apps/:id/rotate-secret).
make run

# 4. Login (example with GitHub token)
curl -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"provider":"github","provider_token":"your-github-token","app_id":"yundian"}'
```

## Configuration

All configuration is via environment variables (or `.env` file):

| Variable | Required | Default | Notes |
|---|---|---|---|
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` | |
| `PORT` | No | `8080` | |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` | |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` | |
| `JWT_ACCESS_TTL` | No | `15m` | Must be positive |
| `JWT_REFRESH_TTL` | No | `168h` (7 days) | Must be > access TTL; ≤ 365 days |
| `ORDER_EXPIRY_DURATION` | No | `30m` | Pending order expiry; sweeper flips to `expired` after this |
| `SWEEPER_INTERVAL` | No | `1m` | Must be strictly < `ORDER_EXPIRY_DURATION` |
| `STRIPE_WEBHOOK_SECRET` | No | (empty) | Empty = Stripe webhooks return 404 |
| `WECHAT_PAY_API_V3_KEY` | No | (empty) | 32 bytes; empty = WeChat webhooks return 404 |
| `ALIPAY_PUBLIC_KEY_PATH` | No | (empty) | PEM file path; empty = Alipay webhooks return 404 |
| `LEMONSQUEEZY_WEBHOOK_SECRET` | No | (empty) | Empty = LemonSqueezy webhooks return 404 |
| `PAYPAL_ENV` | No | `live` | `sandbox` \| `live`; selects which PayPal webhook_id/base URL is active |
| `PAYPAL_WEBHOOK_ID_SANDBOX` | No | (empty) | Empty = PayPal sandbox webhooks return 404 |
| `PAYPAL_WEBHOOK_ID_LIVE` | No | (empty) | Empty = PayPal live webhooks return 404 |
| `PAYPAL_API_BASE_SANDBOX` | No | `https://api-m.sandbox.paypal.com` | |
| `PAYPAL_API_BASE_LIVE` | No | `https://api-m.paypal.com` | |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | No | (empty) | Reserved for future OAuth redirect flow; not consumed in v1 |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | No | (empty) | Reserved for future OAuth redirect flow; not consumed in v1 |

## API Overview

### Public Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness / readiness probe. **Not rate-limited.** Returns 200 `{"code":0,"data":{"status":"ok"}}` or 503 `{"code":503,"message":"db unavailable"}` |
| GET | `/.well-known/jwks.json` | RSA public key (JWK format) |
| POST | `/auth/login` | Login with provider token |
| POST | `/auth/refresh` | Refresh tokens |
| POST | `/auth/logout` | Logout (revoke refresh token) |
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
| POST | `/apps/:id/quote` | Quote a plan for an app (amount, sub_expires_at, per-channel provider_data) |

### App/Plan Management Endpoints (Internal Auth)

| Method | Path | Description |
|---|---|---|
| GET | `/apps` | List all apps |
| GET | `/apps/:id` | Get app details |
| GET | `/apps/:id/provider-token/:channel` | Fetch upstream credential for `paypal` / `lemonsqueezy` (PayPal OAuth is cached in-process ~1h, LS returns the static api_key) |
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
- `GET /apps/:id/provider-token/:channel` and every `/admin/*` route — **internal service auth** (`X-App-ID` + `X-App-Secret` headers). BFF calls these with its own service credentials; never expose to end users. `X-App-Secret` is the bcrypt-hashed value returned once by `POST /admin/apps` or `POST /admin/apps/:id/rotate-secret` — losing it requires a rotation.

User endpoints (`/user/*`, `/payments/*`, `/refunds/*`) require JWT Bearer only.

### v2 known limitations

- `POST /apps/:id/quote` response hardcodes `currency = "USD"` (`internal/service/quote.go`); `POST /payments/orders` hardcodes `currency = "CNY"` (`internal/service/payment.go:125`); WeChat/Alipay webhooks default `CNY`. Multi-currency is not supported in v1; the `plans` table has no currency column today.
- Channel webhooks carry `sub_expires_at` via `payment.metadata.sub_expires_at` / `resource.sub_expires_at` / `meta.custom_data.sub_expires_at`. yunhou-users does not derive it server-side — the BFF computes it from `plan.interval_days` + business rules and embeds it into checkout creation.
- `POST /apps/:id/quote` requires JWT but does **not** enforce `has_access` against the user's subscription. Any authenticated user can quote any plan any app exposes.

### Cycle precedence (PayPal vs LemonSqueezy)

When both providers are configured for the same `plan_id`, the resolved cycle (and therefore `sub_expires_at`) uses **PayPal's `trial_days + billing_cycle_days`**. LemonSqueezy's `trial_days` / `billing_cycle_days` in the quote response are ignored — only its `variant_id` flows downstream. Keep PayPal's billing-cycle definition in sync with operator config or `sub_expires_at` will diverge from what PayPal actually bills.

## Authentication Flow

```
Consumer App        Yunhou Users API        OAuth Provider
    |                      |                      |
    |-- POST /auth/login -->|                      |
    |  {provider_token}     |-- verify token ------>|
    |                      |<-- user info ---------|
    |<-- JWT + refresh ----|                      |
    |                      |                      |
    |-- POST /auth/refresh->|                      |
    |<-- new JWT + refresh-| (with token rotation) |
```

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
psql -d yunhou_users -f migrations/001_init.sql
psql -d yunhou_users -f migrations/002_simplify_plans.sql
psql -d yunhou_users -f migrations/003_payments.sql
psql -d yunhou_users -f migrations/004_ls_channel.sql
psql -d yunhou_users -f migrations/005_paypal_channel.sql
psql -d yunhou_users -f migrations/006_paypal_sub_mapping.sql
psql -d yunhou_users -f migrations/007_app_secret.sql
```

## Tech Stack

- Go 1.25 + Gin
- PostgreSQL + sqlx (no ORM)
- RSA256 JWT + JWKS
- SHA-256 token hashing (refresh tokens); bcrypt app-secret hashing (`apps.secret_hash`)
