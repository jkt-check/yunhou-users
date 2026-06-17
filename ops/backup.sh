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
