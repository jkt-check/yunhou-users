#!/usr/bin/env bash
# Rebuilds and restarts the app container, then verifies it's healthy.
# No auto-rollback: if anything fails, the script exits non-zero and the
# previous container (still running) is left alone. To roll back manually:
#   git checkout HEAD~1 && ./deploy/deploy.sh
set -euo pipefail

cd "$(dirname "$0")/.."

# Source env so DATABASE_URL is available for the migration + backup steps.
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

echo "[1/6] git pull"
git pull --ff-only

echo "[2/6] pre-deploy backup"
if [[ -n "${DATABASE_URL:-}" && -x ./ops/backup.sh ]]; then
  ./ops/backup.sh || echo "!! backup failed (continuing; DB unchanged)"
else
  echo "(skipping backup — DATABASE_URL or ops/backup.sh unavailable)"
fi

echo "[3/6] build image"
# Build only. We do NOT `docker compose up -d` here — that would start the
# new container against the OLD schema (migrations haven't run yet). See
# the step ordering note below.
docker compose build

echo "[4/6] run migrations"
if [[ -n "${DATABASE_URL:-}" ]]; then
  # Run the standalone migrate binary; it owns the _migrations ledger
  # and re-applies nothing that's already recorded. See
  # internal/migrate/migrate.go for the contract and migrations/README.md
  # for the file naming + DDL rules.
  #
  # Ordering: build → migrate → up. If migrate fails, we abort BEFORE
  # starting the new container, so the previous binary keeps serving
  # against the unchanged schema. (Doing `up` first then `migrate`
  # would risk the new binary crashing against the new column it added
  # but couldn't yet write to — the order here is the safe one.)
  docker compose run --rm migrate || {
    echo "!! migrate failed — aborting deploy"
    exit 1
  }
else
  echo "(skipping migrations — DATABASE_URL not set)"
fi

echo "[5/6] restart + healthcheck"
docker compose up -d
# Poll for the container to reach 'running' state with a 60s ceiling.
running=false
for _ in $(seq 1 60); do
  if docker compose ps --format json 2>/dev/null | grep -q '"State":"running"'; then
    running=true
    break
  fi
  sleep 1
done
if [[ "$running" != "true" ]]; then
  echo "!! container not running, recent logs:"
  docker compose logs --tail=200 app
  exit 1
fi

echo "[6/6] healthcheck"
if ! curl -fsS --max-time 5 http://127.0.0.1:8080/healthz; then
  echo "!! healthcheck failed, recent logs:"
  docker compose logs --tail=200 app
  exit 1
fi
echo
echo "deploy OK"