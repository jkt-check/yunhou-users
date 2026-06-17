# Deployment Design — Yunhou Users

**Date:** 2026-06-17
**Status:** Approved (pending written review)
**Author:** brainstorming session with user

## 1. Context and Goals

Yunhou Users is a Go + Gin shared user management API. The codebase has zero
deployment artifacts today (no Dockerfile, no compose, no CI, no Nginx config).
This design covers the **first production-shaped deployment** for a single
Ubuntu 24.04 VPS that already has PostgreSQL 16 running on `127.0.0.1:5432`.

Goals:

- A reproducible Docker image that runs the existing `cmd/server` binary
- A single-command deploy that any teammate with SSH key can run
- HTTPS termination in front of the API via Nginx + Certbot
- Minimal-but-defensible backups, log rotation, and cert renewal
- Room to add a real domain later without re-architecting

Non-goals (out of scope for this design):

- Kubernetes, multi-host HA, autoscaling
- Push-to-deploy CI (manual SSH for now)
- S3/off-host backups (local retention only)
- Centralized logging / metrics / alerting
- Auto-rollback (deploy script fails loudly, manual `git checkout` to undo)

## 2. Architecture / Topology

```
                    Internet
                       │
            TLS (443)  HTTP→HTTPS 301
                       │
        ┌──────────────▼──────────────┐
        │  Nginx (host, systemd)      │  ← Certbot issues/renews
        │  /etc/letsencrypt mounted   │  ← proxy_pass 127.0.0.1:8080
        └──────────────┬──────────────┘
                       │  HTTP (loopback)
        ┌──────────────▼──────────────┐
        │  Docker daemon (host)       │
        │  ┌────────────────────────┐ │
        │  │  yunhou-users (app)    │ │  ← distroless, ~25MB
        │  │  Go + Gin, port 8080   │ │
        │  └─┬──────────────────┬───┘ │
        └────┼──────────────────┼─────┘
   bind     │                  │  DATABASE_URL
   mount    │                  │  (host.docker.internal
   (ro)     │                  │   → 172.17.0.1)
        ┌────▼─────┐    ┌───────▼─────────┐
        │ keys/    │    │  PostgreSQL     │
        │ .env     │    │  host 127.0.0.1 │
        │ on host  │    │  :5432          │
        └──────────┘    └─────────────────┘
```

Key invariants:

- Only ports **22, 80, 443** are exposed on the VPS (UFW)
- App container binds `127.0.0.1:8080` only (no public exposure of API port)
- PostgreSQL stays on `127.0.0.1:5432` and is **not** put in compose
- Container reaches host Postgres via `host.docker.internal` (added via
  `extra_hosts` in compose, mapped to the docker bridge gateway)
- All secrets live in `/opt/yunhou-users/.env` and `/opt/yunhou-users/keys/`,
  both `chmod 600`, owner `ubuntu:ubuntu`

## 3. Components

### 3.1 Files added to the repository

```
yunhou-users/
├── Dockerfile                          multi-stage, distroless final
├── .dockerignore                       exclude .git, keys/, bin/
├── docker-compose.yml                  one app service + extra_hosts
├── .env.example                        env var template (no real values)
├── deploy/
│   ├── deploy.sh                       git pull → build → up → healthcheck
│   └── nginx.conf                      server blocks, security headers, rate limit
├── ops/
│   ├── backup.sh                       pg_dump + 14-day retention
│   ├── renew-cert.sh                   certbot renew wrapper (no-op until DOMAIN set)
│   └── logrotate.conf                  docker log rotation
├── docs/deployment.md                  human-facing deploy runbook
└── internal/handler/health.go          /healthz endpoint (DB ping)
```

`.gitignore` additions: `.env`, `bin/`, `keys/`.

### 3.2 Dockerfile (multi-stage)

Stage 1 — `golang:1.25-alpine`:

- `WORKDIR /src`
- Copy `go.mod`, `go.sum`, run `go mod download`
- Copy source, `go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server`
- Run `go test ./...` (fail build on test failure) — this gates image quality

Stage 2 — `gcr.io/distroless/static-debian12:nonroot`:

- `COPY --from=builder /out/server /server`
- `EXPOSE 8080`
- `USER 65532:65532`
- `HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
   CMD ["/server", "-healthcheck"]`  *(see §6.2 for how /healthz is wired)*

Final image: ~20-30 MB, no shell, no package manager, runs as nonroot UID 65532.

### 3.3 docker-compose.yml

```yaml
services:
  app:
    build: .
    image: yunhou-users:latest
    container_name: yunhou-users
    restart: unless-stopped
    env_file: .env
    ports:
      - "127.0.0.1:8080:8080"   # loopback only
    extra_hosts:
      - "host.docker.internal:host-gateway"
    volumes:
      - ./keys:/keys:ro         # RSA keys, read-only
    healthcheck:
      test: ["CMD", "/server", "-healthcheck"]
      interval: 30s
      timeout: 3s
      retries: 3
      start_period: 10s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

> Note: `/healthz` is invoked via a flag `-healthcheck` because distroless has
> no `wget`/`curl`. The handler returns exit 0 on success, exit 1 on DB
> failure. (Implementation in §6.2.)

### 3.4 .env.example (template)

```
PORT=8080
STATE_HMAC_KEY=                     # required, ≥32 random chars
DATABASE_URL=postgres://yunhou:CHANGEME@host.docker.internal:5432/yunhou_users?sslmode=disable
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
GITHUB_CALLBACK_URL=https://CHANGEME/callback/github
RSA_PRIVATE_KEY_PATH=/keys/private.pem
RSA_PUBLIC_KEY_PATH=/keys/public.pem
JWT_ACCESS_TTL=15m
JWT_REFRESH_TTL=168h
DOMAIN=                             # optional, set when real domain is in use
```

`.env` is **not** committed; the real file is generated on the host by
copying `.env.example` and editing.

### 3.5 deploy/deploy.sh (no rollback)

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

echo "[1/4] git pull"
git pull --ff-only

echo "[2/4] build image"
docker compose build --no-cache

echo "[3/4] restart"
docker compose up -d
sleep 5
if ! docker compose ps --format json | grep -q '"State":"running"'; then
  echo "!! container not running, recent logs:"
  docker compose logs --tail=100 app
  exit 1
fi

echo "[4/4] healthcheck"
curl -fsS http://127.0.0.1:8080/healthz
echo
echo "deploy OK"
```

Run from `/opt/yunhou-users` after `cd`-ing in.

### 3.6 deploy/nginx.conf (template)

```nginx
server_tokens off;

# Rate limit at the edge (app has its own IP bucket too)
limit_req_zone $binary_remote_addr zone=api:10m rate=30r/s;

# --- HTTP: redirect to HTTPS only when DOMAIN is set, else serve directly ---
server {
    listen 80 default_server;
    server_name _;

    # ACME http-01 challenge passthrough (Certbot)
    location /.well-known/acme-challenge/ {
        root /var/www/certbot;
    }

    location / {
        # If DOMAIN env is set on the host, redirect to https; else proxy directly
        if ($http_x_no_redirect = "1") { return 200 "ok\n"; }
        return 301 https://$host$request_uri;
    }
}

# --- HTTPS: only meaningful once a real domain + cert exist ---
server {
    listen 443 ssl http2;
    server_name api.yh.com;          # placeholder, replaced by sed at deploy

    ssl_certificate     /etc/letsencrypt/live/api.yh.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.yh.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    add_header X-Content-Type-Options "nosniff" always;
    add_header X-Frame-Options        "DENY"   always;
    add_header Referrer-Policy        "no-referrer" always;

    location / {
        limit_req zone=api burst=60 nodelay;
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 30s;
    }
}
```

The 443 server block is **commented out** in the in-repo template. It is
uncommented (and `server_name` / cert paths templated) by the operator on
the host after a real domain is pointed at the VPS. Until then, port 80
serves the API directly — fine for staging and pre-launch.

### 3.7 ops/backup.sh

```bash
#!/usr/bin/env bash
set -euo pipefail
source /opt/yunhou-users/.env
TS=$(date -u +%Y%m%dT%H%M%SZ)
DIR=/var/backups/yunhou-users
mkdir -p "$DIR"
pg_dump "$DATABASE_URL" | gzip > "$DIR/db-$TS.sql.gz"
find "$DIR" -name 'db-*.sql.gz' -mtime +14 -delete
```

Installed as `/etc/cron.d/yunhou-users-backup`:

```
0 3 * * * ubuntu /opt/yunhou-users/ops/backup.sh >> /var/log/yunhou-users-backup.log 2>&1
```

### 3.8 ops/renew-cert.sh

```bash
#!/usr/bin/env bash
set -euo pipefail
certbot renew --quiet
systemctl reload nginx
```

Installed as `/etc/cron.d/yunhou-users-cert` (only after a real domain is
in use):

```
0 4 * * 1 root /opt/yunhou-users/ops/renew-cert.sh >> /var/log/yunhou-users-cert.log 2>&1
```

### 3.9 ops/logrotate.conf

Snippets that limit Docker's json-file logs (complements the per-container
`logging` config in compose; belt + suspenders).

## 4. Request Lifecycle

OAuth login as the canonical example:

1. `GET https://api.yh.com/authorize?app_id=...&provider=github&...`
2. Nginx terminates TLS, forwards to `127.0.0.1:8080`
3. Gin handler validates params, HMAC-signs `state`, 302 to GitHub
4. User consents at GitHub, browser returns to
   `https://api.yh.com/callback/github?code=...&state=...`
5. Nginx → Gin callback:
   - Verifies state signature (CSRF)
   - Re-validates `redirect_uri` (open-redirect defense)
   - Exchanges code for GitHub token
   - Fetches user info
   - Find-or-creates `users` + `identities` rows
   - Inserts `auth_codes` row (single use, short TTL)
   - 302 to `redirect_uri?code=...`
6. Consumer app `POST /token {code, app_id, app_secret}`
7. Gin token handler:
   - bcrypt-verify `app_secret`
   - Check active subscription
   - In a transaction: revoke `auth_codes` row, mint access (JWT, RSA256) and
     refresh (random, SHA-256 stored)
   - Return `{access_token, refresh_token, expires_in}`
8. Subsequent `GET /user/profile` with `Authorization: Bearer ...`:
   - Gin parses JWT, verifies signature against JWKS public key
   - Business logic

Persistence: all state in PostgreSQL (`users`, `identities`, `apps`,
`subscriptions`, `auth_codes`, `refresh_tokens`). No Redis, no in-memory
caches → restart loses only in-flight requests.

## 5. Failure Modes

| Failure | Symptom | Recovery |
|---|---|---|
| Nginx down | 5xx / connection refused on 80/443 | systemd auto-restart |
| App container down | 502 from Nginx | Docker `restart: unless-stopped` |
| PostgreSQL down | `/healthz` returns 503, all writes fail | External dependency, manual start |
| Cert expired | Browser `NET::ERR_CERT_DATE_INVALID` | cron renews; needs monitoring later |
| Disk full | Docker / Postgres write errors | logrotate + 14-day backup retention |
| RSA key lost | All token issuance / refresh fails | `keys/` cold backup off-host (operator) |

## 6. Verification

### 6.1 Post-deploy checks

```bash
docker compose ps                          # running + healthy
curl -fsS http://127.0.0.1:8080/healthz    # 200, status:ok
curl -fsS http://127.0.0.1:8080/.well-known/jwks.json | jq .
curl -fsS -I http://<VPS-IP>/              # 80 reachable
curl -fsS -I https://api.yh.com/healthz    # (after domain) 200
PGPASSWORD=... psql -h 127.0.0.1 -U yunhou -d yunhou_users -c '\dt'
DATABASE_URL=... make e2e                  # full suite green
bash ops/backup.sh && ls /var/backups/yunhou-users/   # db-*.sql.gz present
sudo certbot renew --dry-run               # (after domain) success
```

### 6.2 /healthz endpoint (new code)

`internal/handler/health.go`:

```go
package handler

import (
    "context"
    "net/http"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/jmoiron/sqlx"
)

type HealthHandler struct {
    DB *sqlx.DB
}

func (h *HealthHandler) Register(r *gin.Engine) {
    r.GET("/healthz", h.handle)
}

// handle returns 200 when the process is alive and DB responds to ping.
// 503 when DB is unreachable. Bypasses rate limiting (route registered
// before public rate-limit middleware is applied).
func (h *HealthHandler) handle(c *gin.Context) {
    ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
    defer cancel()
    if err := h.DB.PingContext(ctx); err != nil {
        c.JSON(http.StatusServiceUnavailable, gin.H{
            "code":    http.StatusServiceUnavailable,
            "message": "db unavailable",
        })
        return
    }
    c.JSON(http.StatusOK, gin.H{
        "code": 0,
        "data": gin.H{"status": "ok"},
    })
}
```

Wired in `internal/router` (or wherever the public routes are mounted):
register `/healthz` **before** the IP rate-limit middleware so health checks
from monitors don't get throttled.

For the Docker `HEALTHCHECK` (distroless has no `wget`/`curl`), add a tiny
flag to the binary:

```go
// in cmd/server/main.go, very early:
if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
    // quick DB ping
    if err := db.PingContext(ctx); err != nil {
        os.Exit(1)
    }
    os.Exit(0)
}
```

The route `/healthz` is what `deploy.sh` and humans hit; the flag is what
the Docker daemon's `HEALTHCHECK` calls.

### 6.3 Done criteria (deploy is "complete" when all pass)

- `docker compose ps` → running, healthy
- `curl /healthz` → 200
- `curl /.well-known/jwks.json` → 200, contains one key
- `psql -c '\dt'` → 6 tables
- `make e2e` → green
- `bash ops/backup.sh` → produces a gz dump
- (post-domain) `certbot renew --dry-run` → success

## 7. Security

- `keys/private.pem` `chmod 600`, owner `ubuntu:ubuntu`
- `.env` `chmod 600`
- Container process UID `65532:65532` (distroless nonroot)
- UFW: allow 22, 80, 443 only; 5432 stays on 127.0.0.1
- SSH: password auth disabled, key-only (baseline; verify on VPS)
- Nginx: security headers (`nosniff`, `DENY`, `no-referrer`); `server_tokens off`
- TLS (post-domain): `TLSv1.2 TLSv1.3` only, default Certbot profile
- Postgres role `yunhou`: `SELECT/INSERT/UPDATE/DELETE` only, no DDL
  (privileges set via `ALTER DEFAULT PRIVILEGES`)
- No secrets logged: confirmed app handlers do not log `Authorization`,
  `code`, or `app_secret`

## 8. Initial Bootstrap (one-time, on the VPS)

```bash
ssh ubuntu@1.2.3.4

sudo apt update && sudo apt install -y nginx certbot python3-certbot-nginx \
                                   postgresql-client
sudo systemctl enable --now nginx

sudo mkdir -p /opt/yunhou-users && sudo chown -R $USER:$USER /opt/yunhou-users
cd /opt/yunhou-users
git clone git@github.com:yunhou/users.git .

mkdir -p keys
openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
openssl rsa -pubout -in keys/private.pem -out keys/public.pem
chmod 600 keys/private.pem

cp .env.example .env && $EDITOR .env

# --- DB role + database (one-time, as postgres superuser) ---
sudo -u postgres psql <<'SQL'
CREATE DATABASE yunhou_users;
CREATE USER yunhou WITH PASSWORD '<strong>';
GRANT CONNECT ON DATABASE yunhou_users TO yunhou;
\c yunhou_users
GRANT USAGE ON SCHEMA public TO yunhou;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO yunhou;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO yunhou;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO yunhou;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO yunhou;
SQL

# --- schema ---
psql "$(grep ^DATABASE_URL .env | cut -d= -f2-)" -f migrations/001_init.sql

# --- Nginx ---
sudo cp deploy/nginx.conf /etc/nginx/sites-available/yunhou-users
sudo ln -sf /etc/nginx/sites-available/yunhou-users /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default
sudo nginx -t && sudo systemctl reload nginx

# --- start app ---
docker compose up -d --build

# --- install crons ---
sudo install -m 644 ops/logrotate.conf /etc/logrotate.d/docker-yunhou-users
echo '0 3 * * * ubuntu /opt/yunhou-users/ops/backup.sh >> /var/log/yunhou-users-backup.log 2>&1' \
  | sudo tee /etc/cron.d/yunhou-users-backup
# cert cron only after DOMAIN is set:
# echo '0 4 * * 1 root /opt/yunhou-users/ops/renew-cert.sh >> /var/log/yunhou-users-cert.log 2>&1' \
#   | sudo tee /etc/cron.d/yunhou-users-cert

curl -fsS http://127.0.0.1:8080/healthz
```

## 9. Domain Upgrade (later, when ready)

1. Buy domain, point A record to VPS IP
2. Edit `/opt/yunhou-users/.env`: set `DOMAIN=api.yh.com`,
   `GITHUB_CALLBACK_URL=https://api.yh.com/callback/github`
3. Uncomment the `server { listen 443 ... }` block in
   `/etc/nginx/sites-available/yunhou-users`, set `server_name` to the real
   domain
4. `sudo certbot --nginx -d api.yh.com` (issues cert, edits Nginx in place)
5. Install cert cron
6. Redeploy: `./deploy/deploy.sh` (picks up new env)

## 10. Out of Scope (deferred)

- Auto-rollback in deploy.sh
- Off-host backup (S3 / Backblaze)
- Centralized logs / metrics
- Alerting (Slack / email)
- Multi-host / K8s migration
- Image signing / SBOM
- IDS / WAF / fail2ban

These are tracked as candidates for the next iteration once traffic or team
size justifies them.
