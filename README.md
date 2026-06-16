# Yunhou Users

A shared user management API for multi-app ecosystems. One user identity across all consumer applications, with per-app subscriptions and scopes.

## Features

- **Social OAuth only** — GitHub (implemented), Google and WeChat (planned)
- **OAuth 2.0 Authorization Code flow** with HMAC-signed state parameter
- **RSA256 JWT** access tokens with JWKS public key endpoint
- **Subscription gating** — tokens only issued for active subscriptions
- **Refresh token rotation** — one-time-use refresh tokens
- **Rate limiting** — per-IP token bucket on all endpoints

## Quick Start

```bash
# 1. Set up PostgreSQL
createdb yunhou_users
psql -d yunhou_users -f migrations/001_init.sql

# 2. Generate RSA keys
make generate-keys

# 3. Set required env vars
export STATE_HMAC_KEY="your-random-secret-at-least-32-chars"
export GITHUB_CLIENT_ID="your-github-client-id"
export GITHUB_CLIENT_SECRET="your-github-client-secret"

# 4. Run
make run
```

## Configuration

All configuration is via environment variables (or `.env` file):

| Variable | Required | Default |
|---|---|---|
| `STATE_HMAC_KEY` | **Yes** | — |
| `GITHUB_CLIENT_ID` | Yes* | — |
| `GITHUB_CLIENT_SECRET` | Yes* | — |
| `GITHUB_CALLBACK_URL` | No | `http://localhost:{PORT}/callback/github` |
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` |
| `PORT` | No | `8080` |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` |
| `JWT_ACCESS_TTL` | No | `15m` |
| `JWT_REFRESH_TTL` | No | `168h` |

*Required when using GitHub as an OAuth provider.

## API Overview

### Public Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/.well-known/jwks.json` | RSA public key (JWK format) |
| GET | `/authorize` | Start OAuth flow (redirects to provider) |
| GET | `/callback/:provider` | OAuth callback (redirects to consumer app with auth code) |
| POST | `/token` | Exchange auth code for access + refresh tokens |
| POST | `/token/refresh` | Refresh tokens (requires app credentials) |

### User Endpoints (JWT Bearer)

| Method | Path | Description |
|---|---|---|
| GET | `/user/profile` | Get current user profile |
| PATCH | `/user/profile` | Update profile fields |
| GET | `/user/identities` | List linked social identities |
| DELETE | `/user/identities/:id` | Unbind a social identity |
| GET | `/user/apps` | List subscribed apps |

### App Management Endpoints (App Credentials)

| Method | Path | Description |
|---|---|---|
| POST | `/apps` | Register a new application |
| GET | `/apps/:id` | Get app details |
| PATCH | `/apps/:id` | Update app settings |
| POST | `/subscriptions` | Createsubscription |
| GET | `/subscriptions/:id` | Get subscription details |
| DELETE | `/subscriptions/:id` | Cancel subscription |

All endpoints use `X-App-ID` and `X-App-Secret` headers for app authentication.

## Authentication Flow

```
Consumer App        Yunhou Users API        OAuth Provider
    |                      |                      |
    |-- GET /authorize --->|                      |
    |                      |-- redirect ---------->|
    |                      |                      |-- user consents
    |                      |<-- callback ----------|
    |<-- redirect + code --|                      |
    |-- POST /token ------>|                      |
    |                      |-- exchange code ------>|
    |                      |<-- provider token ----|
    |                      |-- fetch user info ---->|
    |                      |<-- user profile -------|
    |<-- JWT + refresh ----|                      |
```

## Development

```bash
make build          # Compile binary
make test           # Run tests with race detection
make lint           # Run go vet
make deps           # Tidy dependencies
```

Run a single test:
```bash
go test -race -run TestExchangeCode ./internal/service/
```

## Tech Stack

- Go 1.25 + Gin
- PostgreSQL + sqlx (no ORM)
- RSA256 JWT + JWKS
- bcrypt app secret hashing
- SHA-256 token hashing
