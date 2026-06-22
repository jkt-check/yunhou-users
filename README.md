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
# Run BOTH migrations in order — 002 alters tables created by 001 and
# will fail if 001 hasn't been applied yet.
psql -d yunhou_users -f migrations/001_init.sql
psql -d yunhou_users -f migrations/002_simplify_plans.sql

# 2. Generate RSA keys
make generate-keys

# 3. Run
make run

# 4. Login (example with GitHub token)
curl -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"provider":"github","provider_token":"your-github-token","app_id":"yundian"}'
```

## Configuration

All configuration is via environment variables (or `.env` file):

| Variable | Required | Default |
|---|---|---|
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` |
| `PORT` | No | `8080` |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` |
| `JWT_ACCESS_TTL` | No | `15m` |
| `JWT_REFRESH_TTL` | No | `168h` |

## API Overview

### Public Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness / readiness probe |
| GET | `/.well-known/jwks.json` | RSA public key (JWK format) |
| POST | `/auth/login` | Login with provider token |
| POST | `/auth/refresh` | Refresh tokens |
| POST | `/auth/logout` | Logout (revoke refresh token) |

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

### App/Plan Management Endpoints (Internal Auth)

| Method | Path | Description |
|---|---|---|
| GET | `/apps` | List all apps |
| GET | `/apps/:id` | Get app details |
| GET | `/admin/plans` | List all plans |
| GET | `/admin/plans/:id` | Get plan details |
| POST | `/admin/plans` | Create plan |
| PATCH | `/admin/plans/:id` | Update plan |
| DELETE | `/admin/plans/:id` | Delete plan |
| POST | `/admin/apps` | Create app |
| PATCH | `/admin/apps/:id` | Update app |

App management endpoints require `X-App-ID` header. Public and user endpoints do not.

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
psql -d yunhou_users -f migrations/002_simplify_plans.sql
```

## Tech Stack

- Go 1.25 + Gin
- PostgreSQL + sqlx (no ORM)
- RSA256 JWT + JWKS
- SHA-256 token hashing
