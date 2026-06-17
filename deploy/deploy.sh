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
