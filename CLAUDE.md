# CLAUDE.md

This file provides guidance to kscc (claude.ai/code) when working with code in this repository.

## Project Overview

Yunhou Users is a **shared user management API** serving multiple consumer applications via RESTful APIs. All apps share the same user identity — one account per person across all consumers. Authentication is **social OAuth only** (GitHub, Google, WeChat); there is no email/password registration.

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

## Development Commands

- `make build` — compile to `bin/server`
- `make run` — run dev server (default :8080)
- `make test` — run unit tests with race detection and coverage (`./internal/...`)
- `make e2e` — run E2E tests against local PostgreSQL (`./tests/e2e/`)
- `make lint` — run `go vet`
- `make deps` — tidy go.mod
- `make generate-keys` — generate RSA key pair in `keys/`
- `go test -race -run TestFoo ./internal/service/` — run a single test
- Database migration: `psql -d yunhou_users -f migrations/002_simplify_plans.sql`

## Required Environment Variables

| Variable | Required | Default | Notes |
|---|---|---|---|
| `PORT` | No | `8080` | |
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

**Public** (rate-limited 10/s burst 20 per IP):
- `GET /.well-known/jwks.json`, `POST /auth/login`, `POST /auth/refresh`, `POST /auth/logout`

**User** (JWT Bearer auth):
- `GET /user/profile`, `PATCH /user/profile`, `GET /user/identities`, `DELETE /user/identities/:id`, `GET /user/subscriptions`, `POST /user/subscriptions`, `DELETE /user/subscriptions/:id`

**App management** (internal service auth via `X-App-ID` header, rate-limited 30/s burst 60 per IP):
- `GET /apps`, `GET /apps/:id`
- `GET /admin/plans`, `GET /admin/plans/:id`, `POST /admin/plans`, `PATCH /admin/plans/:id`, `DELETE /admin/plans/:id`
- `POST /admin/apps`, `PATCH /admin/apps/:id`

## Design Principles

- Immutable data patterns where appropriate
- Input validation at system boundaries
- No hardcoded secrets — all from env vars
- Parameterized queries only — no string interpolation
- Tokens stored as SHA-256 hashes
