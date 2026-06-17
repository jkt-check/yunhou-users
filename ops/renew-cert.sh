#!/usr/bin/env bash
# Wraps `certbot renew` to also reload Nginx if any cert was renewed.
# No-op when no certs are configured (i.e., pre-domain). Run via cron.
set -euo pipefail

if certbot renew --quiet; then
  # reload only if needed; certbot prints nothing on no-op
  systemctl reload nginx || true
fi
