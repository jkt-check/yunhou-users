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
Registered consumer application.

| Field        | Type     | Notes                       |
|--------------|----------|------------------------------|
| id           | uuid     | PK, also used as app_id      |
| secret       | text     | hashed app_secret            |
| name         | text     |                              |
| redirect_uris| text[]   | allowed redirect URIs        |
| providers    | text[]   | allowed social providers     |
| default_plan | text     | free / trial / paid          |
| created_at   | timestamp|                              |
| updated_at   | timestamp|                              |

### Subscription
Controls whether a user can use an App.

| Field      | Type     | Notes                            |
|------------|----------|----------------------------------|
| id         | uuid     | PK                               |
| user_id    | uuid     | FK → User                        |
| app_id     | uuid     | FK → App                         |
| plan       | text     | free / trial / paid              |
| status     | enum     | active / expired / cancelled      |
| expires_at | timestamp| nullable, null = never expires   |
| created_at | timestamp|                                  |
| updated_at | timestamp|                                  |

Unique constraint: (user_id, app_id).

### Session
Token record for audit and potential revocation.

| Field         | Type     | Notes                     |
|---------------|----------|---------------------------|
| id            | uuid     | PK                        |
| user_id       | uuid     | FK → User                 |
| app_id        | uuid     | FK → App                  |
| refresh_token | text     | hashed                    |
| scope         | text[]   | granted scopes            |
| revoked       | boolean  | default false             |
| expires_at    | timestamp|                           |
| created_at    | timestamp|                           |

## Authentication Flow

Single path — social OAuth only:

```
App frontend → User system /authorize?app_id=x&provider=github&redirect_uri=y&state=z
             → Provider OAuth consent
             → User system callback: find or create User + SocialIdentity
             → Redirect back to app redirect_uri?code=auth_code
App backend  → User system POST /token { code, app_id, app_secret }
             → Returns { access_token, refresh_token, id_token }
App backend  → Verify access_token locally via JWKS public key
```

### Auto-merge on same email
When a new social login returns an email that matches an existing User's SocialIdentity, bind to that User instead of creating a new one. For providers without email (WeChat), manual binding only.

### Token details
- **access_token**: JWT, RSA256 signed, 15min TTL. Payload: `sub` (user_id), `app_id`, `scope`, `iat`, `exp`.
- **refresh_token**: opaque, 7d TTL, stored hashed in Session table. Used to get new access_token.
- **JWKS endpoint**: `GET /.well-known/jwks.json` serves RSA public key. Apps fetch on startup and cache.
- **Revocation**: not implemented in v1. Expired tokens naturally expire in 15min. Add Redis blocklist later if needed.

## App Marketplace & Subscription

- App defines a `default_plan` at registration. Apps with `default_plan=free` auto-create Subscription on first user login; paid-only apps require purchase first.
- Subscription status flows: active → expired (on expires_at pass) / cancelled (on explicit cancel).
- Token scope reflects subscription status: next refresh after expiry drops app scope.
- User system does NOT handle payment. External payment system (Stripe, WeChat Pay) calls `POST /subscriptions` after successful payment.

## API Endpoints

### Auth (public + app-side)

| Method | Path                    | Description                          |
|--------|-------------------------|--------------------------------------|
| GET    | /authorize              | Social login entry point             |
| GET    | /callback/:provider     | OAuth callback handler               |
| POST   | /token                  | Exchange auth code for tokens        |
| POST   | /token/refresh          | Refresh access_token                  |
| GET    | /.well-known/jwks.json  | RSA public key for token verification|

### User (requires access_token)

| Method | Path                     | Description                      |
|--------|--------------------------|----------------------------------|
| GET    | /user/profile            | Current user info                |
| PATCH  | /user/profile            | Update nickname / avatar         |
| GET    | /user/identities         | List bound social accounts       |
| DELETE | /user/identities/:id     | Unbind social account (min 1)    |
| GET    | /user/apps               | List user's subscribed apps      |

### App Management (requires app_id + app_secret, server-to-server)

| Method | Path                     | Description                      |
|--------|--------------------------|----------------------------------|
| POST   | /apps                    | Register new app                 |
| GET    | /apps/:id                | App details                      |
| PATCH  | /apps/:id                | Update app config                |
| POST   | /subscriptions           | Create / renew subscription      |
| GET    | /subscriptions/:id       | Subscription details             |
| DELETE | /subscriptions/:id       | Cancel subscription              |

## Project Structure

```
yunhou-users/
├── cmd/
│   └── server/
│       └── main.go              — Entry point
├── internal/
│   ├── config/
│   │   └── config.go            — Env loading (DB, keys, OAuth config)
│   ├── model/
│   │   ├── user.go
│   │   ├── social_identity.go
│   │   ├── app.go
│   │   ├── subscription.go
│   │   └── session.go
│   ├── repo/
│   │   ├── user.go
│   │   ├── social_identity.go
│   │   ├── app.go
│   │   └── subscription.go
│   ├── service/
│   │   ├── auth.go              — OAuth callback, User find/create
│   │   ├── token.go             — JWT sign/refresh, JWKS
│   │   └── subscription.go      — Subscription lifecycle
│   ├── handler/
│   │   ├── auth.go              — /authorize, /callback, /token
│   │   ├── user.go              — /user/*
│   │   └── app.go               — /apps/*, /subscriptions/*
│   ├── middleware/
│   │   ├── auth.go              — JWT verification, extract user_id/app_id/scope
│   │   └── app_auth.go          — app_id + app_secret verification
│   └── router/
│       └── router.go            — Route registration
├── migrations/
│   └── 001_init.sql
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
3. **Payment is external**: user system only tracks subscription state. Keeps scope narrow.
4. **sqlx over ORM**: user system has strong query patterns, raw SQL is more controllable and performant.
5. **Single binary**: monolith for this scale. No microservices overhead.
