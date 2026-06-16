# CLAUDE.md

This file provides guidance to kscc (claude.ai/code) when working with code in this repository.

## Project Overview

Yunhou Users is a **shared user management system API** designed to serve multiple applications (mobile apps, websites, etc.) through API-level integration. All consumer applications share the same user identity and authentication system.

## Architecture

This is a multi-tenant user management API. Key architectural concerns:

- **Shared identity**: One user account across all consumer apps — not per-app siloed accounts
- **App-level isolation**: Different apps may have different permission scopes, feature flags, or role mappings on top of the shared identity
- **API-first design**: Consumer applications integrate via RESTful APIs; no server-rendered UI for this service
- **Authentication**: Issues and validates tokens (e.g., JWT) that consumer apps use to authenticate requests
- **Authorization**: App-level and role-level access control, since different apps may grant different capabilities to the same user

## Development Commands

- `make build` — compile to `bin/server`
- `make run` — run dev server (default :8080)
- `make test` — run tests with race detection and coverage
- `make lint` — run `go vet`
- `make deps` — tidy go.mod
- `make generate-keys` — generate RSA key pair in `keys/`
- `go test -race -run TestFoo ./internal/service/` — run a single test
- Database migration: `psql -d yunhou_users -f migrations/001_init.sql`

## Key Domains

- **User**: Core identity — email, phone, password hash, profile, status
- **App (Client)**: Registered consumer application — app_id, app_secret, redirect URIs, allowed scopes
- **Authentication**: Login, register, password reset, MFA, token issuance/refresh
- **Authorization**: Scopes, roles, permissions per app per user
- **Session**: Active tokens/sessions, device tracking, revocation

## Design Principles

- API responses follow a consistent envelope format (status, data, error message, metadata)
- Immutable data patterns — create new records rather than mutate existing ones where appropriate
- Input validation at all system boundaries (API endpoints, external callbacks)
- No hardcoded secrets — use environment variables or secret management
- Parameterized queries only — no raw SQL string interpolation
