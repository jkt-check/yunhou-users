#!/usr/bin/env bash
# Dumps the Postgres database to /var/backups/yunhou-users/, gzipped.
# Keeps the most recent 14 days of dumps. Run via cron (see docs/deployment.md).
set -euo pipefail

# Dumps contain full user PII (emails, identities, subscriptions, payments) —
# every file this script creates must be owner-only.
umask 077

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
chmod 700 "$DIR"

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "ERROR: DATABASE_URL is not set after sourcing $ENV_FILE" >&2
  exit 1
fi

# Pass credentials via libpq env vars instead of the command line — a
# connection-string argv (pg_dump "$DATABASE_URL") exposes the DB password
# to any local user via ps / /proc for the duration of the dump. Applies to
# the standard postgres://user:pass@host[:port]/db form; anything else
# (keyword conninfo, no password) falls back to the argv form.
if [[ "$DATABASE_URL" =~ ^postgres(ql)?://([^:]+):([^@]+)@([^/:]+)(:([0-9]+))?/([^?]+) ]]; then
  export PGUSER="${BASH_REMATCH[2]}"
  export PGPASSWORD="${BASH_REMATCH[3]}"
  export PGHOST="${BASH_REMATCH[4]}"
  export PGDATABASE="${BASH_REMATCH[7]}"
  if [[ -n "${BASH_REMATCH[6]:-}" ]]; then
    export PGPORT="${BASH_REMATCH[6]}"
  fi
  pg_dump | gzip > "$DIR/db-$TS.sql.gz"
else
  pg_dump "$DATABASE_URL" | gzip > "$DIR/db-$TS.sql.gz"
fi
echo "wrote $DIR/db-$TS.sql.gz"

# Retain 14 days
find "$DIR" -name 'db-*.sql.gz' -mtime +14 -delete
