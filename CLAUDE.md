# CLAUDE.md

This file provides guidance to kscc (claude.ai/code) when working with code in this repository.

## Project Overview

Yunhou Users is a **shared user management API** serving multiple consumer applications via RESTful APIs. All apps share the same user identity — one account per person across all consumers. Authentication is **social OAuth only** (GitHub, Google, WeChat); there is no email/password registration.

## Architecture

- **Shared identity**: One user account across all consumer apps
- **App-level isolation**: Different apps may have different scopes and subscription tiers on top of the shared identity
- **API-first**: Consumer apps integrate via REST; no server-rendered UI
- **OAuth 2.0 Authorization Code flow**: Provider → callback → auth code → token exchange → JWT access token + opaque refresh token
- **Subscription gate**: Both token exchange and token refresh check active subscription before issuing tokens
- **Refresh token rotation**: Every refresh invalidates the old refresh token and issues a new one

### Layering

`model` (structs) → `repo` (SQL, `*sqlx.DB`) → `service` (business logic) → `handler` (HTTP I/O) → `router` (wiring)

All repos are interface-based (`repo.UserRepo`, etc.) for testability. Handler tests use hand-rolled mock structs with function fields — no external mocking libraries.

### Auth Flow

1. `GET /authorize?app_id&provider&redirect_uri&state` — HMAC-signs state, redirects to OAuth provider
2. `GET /callback/:provider?code&state` — verifies state, re-validates redirect_uri, fetches provider user info, find-or-creates user + identity, issues auth code, redirects to consumer app
3. `POST /token` — exchanges `{code, app_id, app_secret}` for access + refresh tokens (checks subscription, atomically revokes auth code)
4. `POST /token/refresh` — exchanges `{refresh_token, app_id, app_secret}` for new tokens (checks subscription including expiration, rotates refresh token)
5. Consumer apps verify access tokens locally via `GET /.well-known/jwks.json` (RSA256 JWK, kid=`yunhou-users-rsa`)

## Development Commands

- `make build` — compile to `bin/server`
- `make run` — run dev server (default :8080)
- `make test` — run all tests with race detection and coverage
- `make lint` — run `go vet`
- `make deps` — tidy go.mod
- `make generate-keys` — generate RSA key pair in `keys/`
- `go test -race -run TestFoo ./internal/service/` — run a single test
- Database migration: `psql -d yunhou_users -f migrations/001_init.sql`

## Required Environment Variables

| Variable | Required | Default | Notes |
|---|---|---|---|
| `STATE_HMAC_KEY` | **Yes** | — | App exits if not set |
| `GITHUB_CLIENT_ID` | Yes (for GitHub auth) | — | |
| `GITHUB_CLIENT_SECRET` | Yes (for GitHub auth) | — | |
| `GITHUB_CALLBACK_URL` | No | `http://localhost:{PORT}/callback/github` | |
| `DATABASE_URL` | No | `postgres://localhost/yunhou_users?sslmode=disable` | |
| `RSA_PRIVATE_KEY_PATH` | No | `keys/private.pem` | File path |
| `RSA_PUBLIC_KEY_PATH` | No | `keys/public.pem` | File path |
| `JWT_ACCESS_TTL` | No | `15m` | |
| `JWT_REFRESH_TTL` | No | `168h` (7 days) | |

## API Response Format

All endpoints return:
```json
{"code": <int>, "data": <object|null>, "message": <string|null>}
```

- `code`: 0 on success, HTTP status code on error
- `data`: response payload on success, null on error
- `message`: null on success, error description on error

## Endpoints

**Public** (rate-limited 10/s per IP):
- `GET /.well-known/jwks.json`, `GET /authorize`, `GET /callback/:provider`, `POST /token`, `POST /token/refresh`

**User** (JWT Bearer auth):
- `GET /user/profile`, `PATCH /user/profile`, `GET /user/identities`, `DELETE /user/identities/:id`, `GET /user/apps`

**App management** (`X-App-ID` + `X-App-Secret` headers, rate-limited 30/s per IP):
- `POST /apps`, `GET /apps/:id`, `PATCH /apps/:id`, `POST /subscriptions`, `GET /subscriptions/:id`, `DELETE /subscriptions/:id`

## Design Principles

- Immutable data patterns where appropriate
- Input validation at system boundaries
- No hardcoded secrets — all from env vars
- Parameterized queries only — no string interpolation
- App secrets stored as bcrypt hashes, tokens as SHA-256 hashes
