# Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a reproducible single-VPS deployment for Yunhou Users using Docker + docker-compose + Nginx + Certbot, with a `/healthz` endpoint, deploy script, backup cron, and runbook.

**Architecture:** Multi-stage Dockerfile (distroless final), one `app` service in `docker-compose.yml`, Nginx on the host for TLS termination, PostgreSQL on the host (already deployed), bind-mounted RSA keys, manual SSH deploy via `deploy/deploy.sh`. No rollback, no auto-rollback, local-only backups.

**Tech Stack:** Go 1.25, Gin, distroless static, docker compose v2, Nginx, Certbot, PostgreSQL 16 (external), cron.

**Spec:** `docs/superpowers/specs/2026-06-17-deployment-design.md`

**Working directory for all commands:** repo root (`/Users/lili/Downloads/github/yunhou-users`).

---

## File map

| File | Action | Purpose |
|---|---|---|
| `internal/handler/health.go` | create | `/healthz` HTTP handler |
| `internal/handler/health_test.go` | create | unit tests for health handler |
| `internal/router/router.go` | modify | register `/healthz` before public rate limit |
| `cmd/server/main.go` | modify | handle `-healthcheck` flag for Docker HEALTHCHECK |
| `.env.example` | create | env var template |
| `.dockerignore` | create | exclude .git, keys/, bin/ from build context |
| `Dockerfile` | create | multi-stage, distroless final |
| `docker-compose.yml` | create | single `app` service |
| `deploy/deploy.sh` | create | git pull + build + up + healthcheck |
| `deploy/nginx.conf` | create | no-domain template (80 → app) |
| `ops/backup.sh` | create | pg_dump + 14-day retention |
| `ops/renew-cert.sh` | create | certbot renew wrapper |
| `ops/logrotate.conf` | create | docker log rotation |
| `docs/deployment.md` | create | human-facing runbook |

`.gitignore` already has `keys/`, `bin/`, `*.exe`, `*.test`, `.env` — **no change needed**.

---

## Task 1: Add health handler — test first (TDD)

**Files:**
- Create: `internal/handler/health_test.go`
- Create: `internal/handler/health.go`

- [ ] **Step 1: Write the failing test**

Create `internal/handler/health_test.go`:

```go
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// stubPinger implements the Pinger interface without touching a real DB.
type stubPinger struct {
	err error
}

func (s *stubPinger) PingContext(ctx context.Context) error { return s.err }

func TestHealth_HealthyReturns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &HealthHandler{Pinger: &stubPinger{err: nil}}
	r.GET("/healthz", h.Handle)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["code"].(float64) != 0 {
		t.Fatalf("expected code=0, got %v", body["code"])
	}
	data, ok := body["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data object, got %T", body["data"])
	}
	if data["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", data["status"])
	}
}

func TestHealth_DBDownReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &HealthHandler{Pinger: &stubPinger{err: errors.New("db down")}}
	r.GET("/healthz", h.Handle)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails to compile (no HealthHandler yet)**

Run: `go test -race -run TestHealth ./internal/handler/`
Expected: compile error `undefined: HealthHandler`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/handler/health.go`:

```go
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger is the minimal interface HealthHandler needs. *sqlx.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// HealthHandler exposes /healthz for liveness/readiness checks.
// Returns 200 when the process is alive and the dependency is reachable;
// 503 when the dependency is not. Intentionally bypasses rate limiting
// (route is registered before the public rate-limit middleware).
type HealthHandler struct {
	Pinger Pinger
}

func (h *HealthHandler) Handle(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
	defer cancel()
	if err := h.Pinger.PingContext(ctx); err != nil {
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

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -run TestHealth -v ./internal/handler/`
Expected: 2 tests PASS, 0 FAIL.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/health.go internal/handler/health_test.go
git commit -m "feat(handler): add /healthz endpoint with DB ping"
```

---

## Task 2: Wire /healthz into the router (before public rate limit)

**Files:**
- Modify: `internal/handler/health.go` (add constructor)
- Modify: `internal/router/router.go` (add route + new param)
- Modify: `cmd/server/main.go` (pass `db` as pinger)

- [ ] **Step 1: Add the constructor in health.go**

Append to `internal/handler/health.go`:

```go
func NewHealthHandler(p Pinger) *HealthHandler {
	return &HealthHandler{Pinger: p}
}
```

- [ ] **Step 2: Update router.Setup to accept the pinger**

In `internal/router/router.go`, change the signature of `Setup` to add a new
`healthPinger handler.Pinger` parameter right after `engine *gin.Engine`:

```go
func Setup(
	ctx context.Context,
	engine *gin.Engine,
	healthPinger handler.Pinger,
	appRepo repo.AppRepo,
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *service.TokenService,
	authSvc *service.AuthService,
	subSvc *service.SubscriptionService,
	oauth *service.OAuthProvider,
	stateHMACKey string,
) {
	// Health check — registered before the public rate limit so monitors
	// are never throttled.
	healthHandler := handler.NewHealthHandler(healthPinger)
	engine.GET("/healthz", healthHandler.Handle)

	authHandler := handler.NewAuthHandler(authSvc, tokenSvc, oauth, stateHMACKey)
	// ...rest of the function unchanged...
```

- [ ] **Step 3: Update main.go to pass *sqlx.DB**

In `cmd/server/main.go`, change the `router.Setup(...)` call to pass `db` as
the new second argument:

```go
	router.Setup(context.Background(), engine, db, appRepo, userRepo, identityRepo, subRepo, sessionRepo, tokenSvc, authSvc, subSvc, oauth, cfg.StateHMACKey)
```

- [ ] **Step 4: Build to verify compile**

Run: `go build -o /tmp/server-build-check ./cmd/server && echo OK && rm /tmp/server-build-check`
Expected: `OK` printed, no errors.

- [ ] **Step 5: Run the full test suite**

Run: `go test -race ./internal/...`
Expected: all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/router/router.go internal/handler/health.go cmd/server/main.go
git commit -m "feat(router): wire /healthz with pinger, bypass rate limit"
```

---

## Task 3: Add -healthcheck flag to main.go (Docker HEALTHCHECK)

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add a small helper file**

Create `cmd/server/healthcheck.go`:

```go
package main

import (
	"context"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
)

// runHealthcheck is invoked by the Docker HEALTHCHECK instruction via
// the binary's `-healthcheck` flag. Distroless has no wget/curl, so the
// binary itself does the DB ping and exits 0 (ok) or 1 (degraded).
func runHealthcheck(db *sqlx.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
```

- [ ] **Step 2: Wire the flag in main.go**

In `cmd/server/main.go`, **after** the successful `db.Ping()` call (i.e., after line 39, before the repos are constructed), add:

```go
	// Docker HEALTHCHECK calls the binary with `-healthcheck`. We handle it
	// here, after DB is ready, and exit before the HTTP server starts.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		runHealthcheck(db)
	}
```

Add `"os"` to the imports.

- [ ] **Step 3: Build and run the flag locally**

Run:
```bash
go build -o /tmp/srv ./cmd/server
/tmp/srv -healthcheck; echo "exit=$?"
```
Expected: with no DB available, `exit=1`. (This proves the flag is wired — it fails fast. To see `exit=0` you need a running DB; we'll do that in a later task.)

- [ ] **Step 4: Clean up and commit**

```bash
rm /tmp/srv
git add cmd/server/healthcheck.go cmd/server/main.go
git commit -m "feat(cmd): add -healthcheck flag for Docker HEALTHCHECK"
```

---

## Task 4: Add .env.example

**Files:**
- Create: `.env.example`

- [ ] **Step 1: Write the file**

Create `.env.example` with this content (the spec's field list, with placeholders, no real values):

```bash
# Yunhou Users — environment variable template
# Copy to .env on the host and fill in real values. NEVER commit .env.

PORT=8080

# Required. ≥32 random characters. Generate with: openssl rand -hex 32
STATE_HMAC_KEY=

# App reaches host Postgres via host.docker.internal (added in docker-compose)
DATABASE_URL=postgres://yunhou:CHANGE_ME@host.docker.internal:5432/yunhou_users?sslmode=disable

# GitHub OAuth (https://github.com/settings/developers)
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
GITHUB_CALLBACK_URL=https://CHANGE_ME/callback/github

# Google OAuth (no longer needed — Google direct-token login was removed)
# (kept here as a placeholder so operators know not to add it back)

# WeChat OAuth
WECHAT_CLIENT_ID=
WECHAT_CLIENT_SECRET=

# RSA keys are bind-mounted at /keys inside the container
RSA_PRIVATE_KEY_PATH=/keys/private.pem
RSA_PUBLIC_KEY_PATH=/keys/public.pem

JWT_ACCESS_TTL=15m
JWT_REFRESH_TTL=168h

# Optional. Set when a real domain is in use; used to decide Nginx 80→301 behavior.
DOMAIN=
```

- [ ] **Step 2: Verify it is ignored by git**

Run: `git check-ignore -v .env || echo "NOT IGNORED — fix .gitignore"`
Expected: path printed with `.gitignore:` line. If `NOT IGNORED`, append `.env.example` is **not** the same as `.env` — `.env` is already in `.gitignore` (line 5 of `.gitignore`). Good.

- [ ] **Step 3: Commit**

```bash
git add .env.example
git commit -m "docs: add .env.example template"
```

---

## Task 5: Add .dockerignore

**Files:**
- Create: `.dockerignore`

- [ ] **Step 1: Write the file**

Create `.dockerignore`:

```
.git
.gitignore
keys/
bin/
*.exe
*.test
*.out
.env
.env.example
docs/
tests/e2e/
deploy/
ops/
*.md
!README.md
.dockerignore
Dockerfile
docker-compose.yml
```

Note: `docs/` is excluded because it's not needed in the runtime image. `tests/e2e/` is excluded because the build only needs `internal/` and `cmd/`. The deploy/ops scripts and human-facing docs are not needed in the image either.

- [ ] **Step 2: Commit**

```bash
git add .dockerignore
git commit -m "build: add .dockerignore to keep image small"
```

---

## Task 6: Add Dockerfile (multi-stage, distroless)

**Files:**
- Create: `Dockerfile`

- [ ] **Step 1: Write the file**

Create `Dockerfile`:

```dockerfile
# syntax=docker/dockerfile:1.7

# ---- Stage 1: build ----
FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache module download
COPY go.mod go.sum ./
RUN go mod download

# Copy source and run unit tests (e2e excluded — needs a live DB)
COPY . .
RUN go test -race ./internal/...

# Build a static, stripped binary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 2: runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/server /server
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/server"]
```

Note on `go test` in the build: it runs `./internal/...` (unit tests only). The project's `tests/e2e/` is excluded via `.dockerignore` so it won't be in the build context at all, but the explicit `./internal/...` is a belt-and-suspenders guard.

- [ ] **Step 2: Build the image**

Run: `docker build -t yunhou-users:dev .`
Expected: ends with `Successfully tagged yunhou-users:dev` and no errors. The build step runs `go test` — must pass.

- [ ] **Step 3: Smoke-test the image (no DB → -healthcheck exits 1)**

Run:
```bash
docker run --rm yunhou-users:dev -healthcheck; echo "exit=$?"
```
Expected: process exits 1 with some error (DB unreachable). This proves the binary inside the image runs the healthcheck path.

- [ ] **Step 4: Commit**

```bash
git add Dockerfile
git commit -m "build: add multi-stage Dockerfile with distroless final stage"
```

---

## Task 7: Add docker-compose.yml

**Files:**
- Create: `docker-compose.yml`

- [ ] **Step 1: Write the file**

Create `docker-compose.yml`:

```yaml
services:
  app:
    build: .
    image: yunhou-users:latest
    container_name: yunhou-users
    restart: unless-stopped
    env_file: .env
    ports:
      - "127.0.0.1:8080:8080"   # loopback only; Nginx reaches it via 127.0.0.1
    extra_hosts:
      - "host.docker.internal:host-gateway"   # resolves to host's docker bridge gateway
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

- [ ] **Step 2: Validate compose syntax**

Run: `docker compose config -q && echo OK`
Expected: `OK`. (May warn about missing `.env` and `keys/`, which is fine — the file is created on the host, not in the repo.)

- [ ] **Step 3: Commit**

```bash
git add docker-compose.yml
git commit -m "build: add docker-compose.yml with /keys bind-mount and host.docker.internal"
```

---

## Task 8: Add deploy/deploy.sh

**Files:**
- Create: `deploy/deploy.sh`

- [ ] **Step 1: Write the file**

Create `deploy/deploy.sh`:

```bash
#!/usr/bin/env bash
# Rebuilds and restarts the app container, then verifies it's healthy.
# No auto-rollback: if anything fails, the script exits non-zero and the
# previous container (still running) is left alone. To roll back manually:
#   git checkout HEAD~1 && ./deploy/deploy.sh
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[1/4] git pull"
git pull --ff-only

echo "[2/4] build image"
docker compose build

echo "[3/4] restart container"
docker compose up -d
sleep 5
if ! docker compose ps --format json | grep -q '"State":"running"'; then
  echo "!! container not running, recent logs:"
  docker compose logs --tail=200 app
  exit 1
fi

echo "[4/4] healthcheck"
if ! curl -fsS --max-time 5 http://127.0.0.1:8080/healthz; then
  echo "!! healthcheck failed, recent logs:"
  docker compose logs --tail=200 app
  exit 1
fi
echo
echo "deploy OK"
```

- [ ] **Step 2: Make executable and syntax-check**

Run:
```bash
chmod +x deploy/deploy.sh
bash -n deploy/deploy.sh && echo "syntax OK"
```
Expected: `syntax OK`.

- [ ] **Step 3: Commit**

```bash
git add deploy/deploy.sh
git commit -m "deploy: add deploy.sh with no-rollback update flow"
```

---

## Task 9: Add deploy/nginx.conf (no-domain template)

**Files:**
- Create: `deploy/nginx.conf`

- [ ] **Step 1: Write the file**

Create `deploy/nginx.conf`:

```nginx
# Yunhou Users — Nginx config (no-domain form).
# Drop into /etc/nginx/sites-available/yunhou-users and symlink into sites-enabled.
# When a real domain is added, see docs/deployment.md §"Domain upgrade".

http {
    server_tokens off;
    limit_req_zone $binary_remote_addr zone=api:10m rate=30r/s;

    # Pre-domain: serve the API over plain HTTP on port 80.
    server {
        listen 80 default_server;
        server_name _;

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

    # Post-domain: uncomment and set server_name to the real domain.
    # Requires running `sudo certbot --nginx -d <domain>` first.
    #
    # server {
    #     listen 443 ssl http2;
    #     server_name api.yh.com;
    #
    #     ssl_certificate     /etc/letsencrypt/live/api.yh.com/fullchain.pem;
    #     ssl_certificate_key /etc/letsencrypt/live/api.yh.com/privkey.pem;
    #     ssl_protocols       TLSv1.2 TLSv1.3;
    #
    #     add_header X-Content-Type-Options "nosniff" always;
    #     add_header X-Frame-Options        "DENY"   always;
    #     add_header Referrer-Policy        "no-referrer" always;
    #
    #     location / {
    #         limit_req zone=api burst=60 nodelay;
    #         proxy_pass http://127.0.0.1:8080;
    #         proxy_set_header Host              $host;
    #         proxy_set_header X-Real-IP         $remote_addr;
    #         proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    #         proxy_set_header X-Forwarded-Proto $scheme;
    #         proxy_read_timeout 30s;
    #     }
    # }
}
```

- [ ] **Step 2: Syntax-check (host nginx not required)**

Run: `which nginx || echo "nginx not installed locally; will be tested on the host VPS"`
Then if installed: `sudo nginx -t -c "$PWD/deploy/nginx.conf" || echo "(sudo may not be available; skip on dev box)"`
Expected: either `nginx -t` passes, or the "skip on dev box" message.

- [ ] **Step 3: Commit**

```bash
git add deploy/nginx.conf
git commit -m "deploy: add nginx config (no-domain template)"
```

---

## Task 10: Add ops/backup.sh

**Files:**
- Create: `ops/backup.sh`

- [ ] **Step 1: Write the file**

Create `ops/backup.sh`:

```bash
#!/usr/bin/env bash
# Dumps the Postgres database to /var/backups/yunhou-users/, gzipped.
# Keeps the most recent 14 days of dumps. Run via cron (see docs/deployment.md).
set -euo pipefail

# Source env (DATABASE_URL lives there)
ENV_FILE=/opt/yunhou-users/.env
if [[ ! -f "$ENV_FILE" ]]; then
  echo "ERROR: $ENV_FILE not found" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

TS=$(date -u +%Y%m%dT%H%M%SZ)
DIR=/var/backups/yunhou-users
mkdir -p "$DIR"

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "ERROR: DATABASE_URL is not set after sourcing $ENV_FILE" >&2
  exit 1
fi

pg_dump "$DATABASE_URL" | gzip > "$DIR/db-$TS.sql.gz"
echo "wrote $DIR/db-$TS.sql.gz"

# Retain 14 days
find "$DIR" -name 'db-*.sql.gz' -mtime +14 -delete
```

- [ ] **Step 2: Make executable and syntax-check**

Run:
```bash
chmod +x ops/backup.sh
bash -n ops/backup.sh && echo "syntax OK"
```
Expected: `syntax OK`.

- [ ] **Step 3: Commit**

```bash
git add ops/backup.sh
git commit -m "ops: add pg_dump backup script with 14-day retention"
```

---

## Task 11: Add ops/renew-cert.sh

**Files:**
- Create: `ops/renew-cert.sh`

- [ ] **Step 1: Write the file**

Create `ops/renew-cert.sh`:

```bash
#!/usr/bin/env bash
# Wraps `certbot renew` to also reload Nginx if any cert was renewed.
# No-op when no certs are configured (i.e., pre-domain). Run via cron.
set -euo pipefail

if certbot renew --quiet; then
  # reload only if needed; certbot prints nothing on no-op
  systemctl reload nginx || true
fi
```

- [ ] **Step 2: Make executable and syntax-check**

Run:
```bash
chmod +x ops/renew-cert.sh
bash -n ops/renew-cert.sh && echo "syntax OK"
```
Expected: `syntax OK`.

- [ ] **Step 3: Commit**

```bash
git add ops/renew-cert.sh
git commit -m "ops: add certbot renew wrapper for cron"
```

---

## Task 12: Add ops/logrotate.conf

**Files:**
- Create: `ops/logrotate.conf`

- [ ] **Step 1: Write the file**

Create `ops/logrotate.conf`:

```
/var/lib/docker/containers/*/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

This is a belt-and-suspenders complement to the per-container `logging` config
in `docker-compose.yml` (which already caps each container at 3 × 10MB). It
guards against the daemon's default of unlimited growth for any future
container started without explicit logging config.

- [ ] **Step 2: Validate config syntax (if logrotate installed)**

Run: `logrotate -d ops/logrotate.conf 2>&1 | head -20 || echo "(logrotate not on dev box; will be tested on the host VPS)"`
Expected: dry-run prints planned actions, no errors.

- [ ] **Step 3: Commit**

```bash
git add ops/logrotate.conf
git commit -m "ops: add logrotate config for docker json logs"
```

---

## Task 13: Add docs/deployment.md (human runbook)

**Files:**
- Create: `docs/deployment.md`

- [ ] **Step 1: Write the file**

Create `docs/deployment.md`:

```markdown
# Deployment Runbook

Single-VPS deployment for Yunhou Users. Target: Ubuntu 24.04 with PostgreSQL
already running on `127.0.0.1:5432`. The app runs in Docker, Nginx + Certbot
on the host terminate TLS.

## First-time setup

1. **Install host packages**

   ```bash
   sudo apt update
   sudo apt install -y nginx certbot python3-certbot-nginx postgresql-client
   sudo systemctl enable --now nginx
   ```

2. **Set up the deploy directory**

   ```bash
   sudo mkdir -p /opt/yunhou-users
   sudo chown -R "$USER":"$USER" /opt/yunhou-users
   cd /opt/yunhou-users
   git clone git@github.com:yunhou/users.git .
   ```

3. **Generate RSA keys**

   ```bash
   mkdir -p keys
   openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
   openssl rsa -pubout -in keys/private.pem -out keys/public.pem
   chmod 600 keys/private.pem
   ```

4. **Configure environment**

   ```bash
   cp .env.example .env
   $EDITOR .env   # set STATE_HMAC_KEY, DATABASE_URL, GITHUB_*, etc.
   chmod 600 .env
   ```

5. **Create the Postgres role and database** (one-time, as `postgres` superuser)

   ```bash
   sudo -u postgres psql <<'SQL'
   CREATE DATABASE yunhou_users;
   CREATE USER yunhou WITH PASSWORD '<strong-password>';
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
   ```

6. **Apply migrations**

   ```bash
   psql "$(grep ^DATABASE_URL .env | cut -d= -f2-)" -f migrations/001_init.sql
   ```

7. **Install Nginx config**

   ```bash
   sudo cp deploy/nginx.conf /etc/nginx/sites-available/yunhou-users
   sudo ln -sf /etc/nginx/sites-available/yunhou-users /etc/nginx/sites-enabled/
   sudo rm -f /etc/nginx/sites-enabled/default
   sudo nginx -t && sudo systemctl reload nginx
   ```

8. **Open firewall**

   ```bash
   sudo ufw allow 22/tcp
   sudo ufw allow 80/tcp
   sudo ufw allow 443/tcp
   sudo ufw enable   # if not already enabled
   ```

9. **Start the app**

   ```bash
   docker compose up -d --build
   curl -fsS http://127.0.0.1:8080/healthz
   ```

10. **Install cron jobs**

    ```bash
    sudo install -m 644 ops/logrotate.conf /etc/logrotate.d/docker-yunhou-users
    echo '0 3 * * * ubuntu /opt/yunhou-users/ops/backup.sh >> /var/log/yunhou-users-backup.log 2>&1' \
      | sudo tee /etc/cron.d/yunhou-users-backup
    ```

## Daily operations

### Deploy a new version

```bash
cd /opt/yunhou-users
./deploy/deploy.sh
```

The script: `git pull` → `docker compose build` → `docker compose up -d` →
5-second wait → check container is `running` → `curl /healthz`. Any failure
exits non-zero; the previous container stays up.

To roll back: `git checkout <previous-tag-or-sha> && ./deploy/deploy.sh`.

### View logs

```bash
docker compose logs -f app               # app stdout/stderr
sudo tail -f /var/log/nginx/access.log   # Nginx access
sudo tail -f /var/log/nginx/error.log    # Nginx errors
tail -f /var/log/yunhou-users-backup.log # backup run history
```

### Check status

```bash
docker compose ps                 # container health
curl -s http://127.0.0.1:8080/healthz | jq .
sudo systemctl status nginx
```

### Restore from backup

```bash
gunzip -c /var/backups/yunhou-users/db-20260617T030000Z.sql.gz \
  | psql "$(grep ^DATABASE_URL /opt/yunhou-users/.env | cut -d= -f2-)"
```

## Domain upgrade (later)

1. Buy a domain, point an A record at the VPS IP, wait for DNS to propagate.
2. Edit `/opt/yunhou-users/.env`: set `DOMAIN=api.yh.com`, update
   `GITHUB_CALLBACK_URL=https://api.yh.com/callback/github` (and any other
   provider callback URLs).
3. Edit `/etc/nginx/sites-available/yunhou-users`:
   - Replace the 80 `server` block with:
     ```nginx
     server {
         listen 80;
         server_name api.yh.com;
         location /.well-known/acme-challenge/ { root /var/www/certbot; }
         location / { return 301 https://$host$request_uri; }
     }
     ```
   - Uncomment the 443 `server` block, set `server_name` to `api.yh.com`.
4. Issue the cert (Certbot will edit Nginx in place):
   ```bash
   sudo certbot --nginx -d api.yh.com
   ```
5. Install the cert renewal cron:
   ```bash
   echo '0 4 * * 1 root /opt/yunhou-users/ops/renew-cert.sh >> /var/log/yunhou-users-cert.log 2>&1' \
     | sudo tee /etc/cron.d/yunhou-users-cert
   ```
6. Redeploy to pick up the new env: `./deploy/deploy.sh`.

## Troubleshooting

| Symptom | First check |
|---|---|
| 502 from Nginx | `docker compose ps` — container not running? `docker compose logs --tail=200 app` |
| `/healthz` 503 | Postgres down. `psql "$(grep ^DATABASE_URL /opt/younhou-users/.env \| cut -d= -f2-)" -c 'select 1'` |
| Cert expired | `sudo certbot certificates` then `sudo certbot renew --dry-run` |
| Disk full | `df -h` and `du -sh /var/lib/docker /var/backups/yunhou-users /var/log` |
| OAuth callback fails | Check `GITHUB_CALLBACK_URL` matches the registered GitHub OAuth app callback |
```

- [ ] **Step 2: Commit**

```bash
git add docs/deployment.md
git commit -m "docs: add human-facing deployment runbook"
```

---

## Task 14: End-to-end local verification

This is the final sanity check before declaring done. The dev box needs Docker
and (for the e2e step) a reachable Postgres — most likely skip the e2e step
locally and rely on CI / VPS-side verification.

- [ ] **Step 1: Confirm everything builds**

Run: `docker compose build`
Expected: succeeds.

- [ ] **Step 2: Start the stack**

Run: `docker compose up -d`
Expected: container starts, `docker compose ps` shows `running`. Without a
real DB and `.env`, `/healthz` will return 503 — that's fine, the rest of
the test confirms wiring.

- [ ] **Step 3: Confirm /healthz is wired**

Run:
```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz
```
Expected: `503` (DB unavailable in dev box) **or** `200` (if a DB is
reachable). Either response means the route is wired.

- [ ] **Step 4: Confirm JWKS is reachable through the container**

Run: `curl -s http://127.0.0.1:8080/.well-known/jwks.json | head -c 200`
Expected: JSON with `"keys": [...]`. This proves the container's HTTP server
is up and RSA keys are loaded (or it 500s if keys missing — the JSON response
is the success signal).

- [ ] **Step 5: Stop the stack**

Run: `docker compose down`
Expected: clean exit.

- [ ] **Step 6: Run the full test suite one more time**

Run: `go test -race ./internal/...`
Expected: all PASS.

- [ ] **Step 7: Commit any stray files (if needed)**

If you created `.env` or `keys/` locally for testing, they are gitignored.
No commit needed. If you created any untracked files, decide whether they
belong in the repo.

---

## Done criteria

All 14 tasks completed and committed. Running the deploy runbook
(`docs/deployment.md` §"First-time setup") on a fresh Ubuntu 24.04 VPS with
PostgreSQL reachable on `127.0.0.1:5432` produces:

- `docker compose ps` → running, healthy
- `curl /healthz` → 200 (with a real DB) or 503 (without)
- `curl /.well-known/jwks.json` → 200, contains 1 key
- `curl http://<VPS-IP>/` → 200 (Nginx proxies to app on 80)
- `psql -c '\dt'` → 6 tables
- Daily `ops/backup.sh` produces a gzipped dump in `/var/backups/yunhou-users/`
- (post-domain) `sudo certbot renew --dry-run` → success
