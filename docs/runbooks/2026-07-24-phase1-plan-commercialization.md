# Phase 1 Plan Commercialization Deploy Runbook

## Overview

Phase 1 deploys the following changes to `yunhou-users`:

- `migrations/012_plan_commercial_fields.sql`: adds the seven commercial plan fields (`is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, `description`, `display_order`, `updated_at`), constraints, the `updated_at` trigger, seed backfill, and `plan_change_log`.
- `migrations/013_plan_change_log_fk_set_null.sql`: makes `plan_change_log.plan_id` nullable and changes its foreign key to `ON DELETE SET NULL`, so audit rows survive plan deletion.
- Service changes from Tasks 1–15: commercial plan CRUD and validation, plan audit logging, subscription acceptance guard, payment currency guard, plan-sourced quote/order fields, `subscription.is_accepting_new`, dev-login plan selection, tests, and API documentation.

Run staging first. Do not promote to production until every required release gate passes or the release owner explicitly resolves the failures in [Known issues](#known-issues).

All commands below assume Bash, `psql`, `curl`, and `jq` are installed and the checkout is at the Phase 1 release revision.

```bash
set -euo pipefail
cd /opt/yunhou-users

: "${STAGING_DATABASE_URL:?export STAGING_DATABASE_URL first}"
export API_BASE="https://<staging-api-host>"
export APP_ID="yundian"               # or another active seed app
export PHASE1_RELEASE_SHA="<release-commit-sha>"

test "$(git rev-parse HEAD)" = "$PHASE1_RELEASE_SHA"
```

## Pre-deploy checklist

### 1. Confirm there are no active `free` subscriptions

This query **must return `0`**. Stop the deploy if it does not.

```bash
FREE_ACTIVE_COUNT="$({
  psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 -Atqc \
    "SELECT COUNT(*) FROM subscriptions WHERE plan_id='free' AND status='active'"
})"
printf 'active free subscriptions: %s\n' "$FREE_ACTIVE_COUNT"
test "$FREE_ACTIVE_COUNT" = "0"
```

If the count is non-zero, cancel those subscriptions or migrate them to a known paid plan before continuing. Do not bypass this check.

### 2. Dry-run migrations 012 and 013 on staging

Run both migrations in a transaction and roll the transaction back. Both files are transactional and idempotent. The command must finish without an error.

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
\i migrations/012_plan_commercial_fields.sql
\i migrations/013_plan_change_log_fk_set_null.sql
ROLLBACK;
SQL
```

Record the release SHA, migration checksums, operator, timestamp, and dry-run result in the deployment ticket.

```bash
sha256sum \
  migrations/012_plan_commercial_fields.sql \
  migrations/013_plan_change_log_fk_set_null.sql
git rev-parse HEAD
date -u '+%Y-%m-%dT%H:%M:%SZ'
```

### 3. Confirm all in-flight records reference known plans

Both counts must be `0`.

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT 'pending_orders_with_unknown_plan' AS check_name, COUNT(*) AS failures
  FROM orders o
  LEFT JOIN plans p ON p.id = o.plan_id
 WHERE o.status = 'pending'
   AND p.id IS NULL
UNION ALL
SELECT 'pending_subscriptions_with_unknown_plan', COUNT(*)
  FROM subscriptions s
  LEFT JOIN plans p ON p.id = s.plan_id
 WHERE s.status = 'pending'
   AND p.id IS NULL;
SQL
```

Also inspect the actual in-flight plan distribution before deploying:

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT 'order' AS record_type, status, plan_id, COUNT(*)
  FROM orders
 WHERE status = 'pending'
 GROUP BY status, plan_id
UNION ALL
SELECT 'subscription', status, plan_id, COUNT(*)
  FROM subscriptions
 WHERE status = 'pending'
 GROUP BY status, plan_id
ORDER BY record_type, plan_id;
SQL
```

### 4. Confirm the release artifact builds

```bash
make build
```

## Migration apply

Apply 012, then 013, in that order. `ON_ERROR_STOP=1` prevents a partial script from being mistaken for success.

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f migrations/012_plan_commercial_fields.sql
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f migrations/013_plan_change_log_fk_set_null.sql
```

Both migrations are idempotent and safe to re-run. A second pass is an optional explicit idempotency check:

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f migrations/012_plan_commercial_fields.sql
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f migrations/013_plan_change_log_fk_set_null.sql
```

Verify the resulting schema and seed values:

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT id, is_active, is_listed, accepting_new_subscriptions,
       currency, trial_days, display_order, updated_at
  FROM plans
 ORDER BY display_order, id;

SELECT c.is_nullable, rc.delete_rule
  FROM information_schema.columns c
  JOIN information_schema.referential_constraints rc
    ON rc.constraint_name = 'plan_change_log_plan_id_fkey'
 WHERE c.table_schema = 'public'
   AND c.table_name = 'plan_change_log'
   AND c.column_name = 'plan_id';
SQL
```

Expected for migration 013: `plan_change_log.plan_id` is nullable and the foreign-key delete rule is `SET NULL`.

## Service deploy

Use the standard `yunhou-users` deployment pipeline documented in `docs/deployment.md`:

```bash
cd /opt/yunhou-users
./deploy/deploy.sh
```

The script pulls the release, builds the Docker image, replaces the app container, and checks `/healthz`. Confirm the intended revision and container state after it finishes:

```bash
test "$(git rev-parse HEAD)" = "$PHASE1_RELEASE_SHA"
docker compose ps
docker compose logs --since=10m app
```

## Smoke test

### Smoke-test setup

Use a staging-only internal app secret. Do not put it in shell history or the runbook.

```bash
printf 'X-App-Secret for %s: ' "$APP_ID" >&2
IFS= read -r -s APP_SECRET
printf '\n' >&2
export APP_SECRET

SMOKE_PLAN_ID="phase1-smoke-$(date +%s)"
SMOKE_EMAIL="phase1-smoke-$(date +%s)@example.invalid"
TMP_DIR="$(mktemp -d)"
```

### 1. Health check returns 200

```bash
HTTP_STATUS="$(curl -sS -o "$TMP_DIR/health.json" -w '%{http_code}' \
  "$API_BASE/healthz")"
test "$HTTP_STATUS" = "200"
jq -e '.code == 0 and .data.status == "ok"' "$TMP_DIR/health.json"
```

### 2. Public plans contain the paid seed plans (and the not-yet-retired `free` plan)

Phase 1 does **not** retire `free`. Migration 012 leaves `free.is_active=true` and `free` is still listed in `GET /apps/:id/plans`. The "no retired free plan" assertion belongs to the Phase 2 runbook; here we only assert that the commercial fields are observable and the paid seed plans are present. Operators may promote Phase 1 with `free` still listed — the retirement is the Phase 2 deployment's responsibility.

```bash
HTTP_STATUS="$(curl -sS -o "$TMP_DIR/plans.json" -w '%{http_code}' \
  "$API_BASE/apps/$APP_ID/plans")"
test "$HTTP_STATUS" = "200"
jq . "$TMP_DIR/plans.json"

jq -e '
  [.data[].id] as $ids
  | ($ids | index("monthly") != null)
    and ($ids | index("quarterly") != null)
    and ($ids | index("yearly") != null)
    and ($ids | index("free") == null)
    and all(.data[]; .is_listed == true)
' "$TMP_DIR/plans.json"
```

The `PublicPlan` DTO exposes `is_listed` as a top-level JSON field. The repo filters by `is_listed=true`, so a plan with `is_listed=false` will be absent from this response — verify by absence. To verify a returned plan carries the flag, assert `.data[].is_listed == true`.

### 3. Admin plan creation accepts all commercial fields

Six fields are client-writable. The seventh field, `updated_at`, is server-managed and must be present in the response/read-back.

```bash
jq -n \
  --arg id "$SMOKE_PLAN_ID" \
  --arg app_id "$APP_ID" \
  '{
    id: $id,
    name: "Phase 1 Smoke USD",
    price: 19.9,
    interval_days: 30,
    apps: [$app_id],
    is_active: true,
    is_listed: true,
    accepting_new_subscriptions: true,
    currency: "USD",
    trial_days: 7,
    description: "Phase 1 deploy smoke plan",
    display_order: 999
  }' > "$TMP_DIR/create-plan-request.json"

HTTP_STATUS="$(curl -sS -o "$TMP_DIR/create-plan.json" -w '%{http_code}' \
  -X POST "$API_BASE/admin/plans" \
  -H "X-App-ID: $APP_ID" \
  -H "X-App-Secret: $APP_SECRET" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP_DIR/create-plan-request.json")"
test "$HTTP_STATUS" = "201"

jq -e --arg id "$SMOKE_PLAN_ID" '
  .code == 0
  and .data.id == $id
  and .data.is_listed == true
  and .data.accepting_new_subscriptions == true
  and .data.currency == "USD"
  and .data.trial_days == 7
  and .data.description == "Phase 1 deploy smoke plan"
  and .data.display_order == 999
  and (.data | has("updated_at"))
' "$TMP_DIR/create-plan.json"

curl -fsS "$API_BASE/admin/plans/$SMOKE_PLAN_ID" \
  -H "X-App-ID: $APP_ID" \
  -H "X-App-Secret: $APP_SECRET" \
  > "$TMP_DIR/get-plan.json"

jq -e '
  .data.currency == "USD"
  and .data.trial_days == 7
  and (.data.updated_at | startswith("0001-") | not)
' "$TMP_DIR/get-plan.json"
```

### 4. Admin plan creation rejects `is_default`

```bash
jq -n \
  --arg id "${SMOKE_PLAN_ID}-default" \
  --arg app_id "$APP_ID" \
  '{
    id: $id,
    name: "Must Reject",
    price: 0,
    interval_days: 0,
    apps: [$app_id],
    is_default: true,
    is_listed: true,
    accepting_new_subscriptions: true,
    currency: "CNY",
    trial_days: 0,
    description: "must reject",
    display_order: 1000
  }' > "$TMP_DIR/reject-default-request.json"

HTTP_STATUS="$(curl -sS -o "$TMP_DIR/reject-default.json" -w '%{http_code}' \
  -X POST "$API_BASE/admin/plans" \
  -H "X-App-ID: $APP_ID" \
  -H "X-App-Secret: $APP_SECRET" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP_DIR/reject-default-request.json")"
test "$HTTP_STATUS" = "400"
jq -e '.code == 400 and (.message | contains("is_default"))' \
  "$TMP_DIR/reject-default.json"
```

### 5. Login response exposes `subscription.is_accepting_new`

On a development-only environment with `PAYPAL_L3_E2E_MODE=1`:

```bash
jq -n --arg email "$SMOKE_EMAIL" --arg app_id "$APP_ID" \
  '{email: $email, app_id: $app_id}' > "$TMP_DIR/login-request.json"

HTTP_STATUS="$(curl -sS -o "$TMP_DIR/login.json" -w '%{http_code}' \
  -X POST "$API_BASE/test/login?plan_id=monthly" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP_DIR/login-request.json")"
test "$HTTP_STATUS" = "200"

jq -e '
  .code == 0
  and .data.subscription.plan_id == "monthly"
  and .data.subscription.has_access == true
  and .data.subscription.is_accepting_new == true
' "$TMP_DIR/login.json"

ACCESS_TOKEN="$(jq -er '.data.access_token' "$TMP_DIR/login.json")"
REFRESH_TOKEN="$(jq -er '.data.refresh_token' "$TMP_DIR/login.json")"
SMOKE_USER_ID="$(jq -er '.data.user.id' "$TMP_DIR/login.json")"
export ACCESS_TOKEN REFRESH_TOKEN SMOKE_USER_ID
```

If `/test/login` returns 404, that is correct for a shared/production-like staging environment. Do not enable the dev endpoint there; complete the equivalent check through the configured GitHub or WeChat OAuth flow and inspect the BFF login result instead.

### 6. A user with no subscription still resolves through the Phase 1 default-plan fallback

Phase 1 keeps the `default-plan` fallback in `resolvePlanForTokenIssuance`. With no persisted subscription, `/auth/refresh` and `/auth/{github,wechat}/callback` still issue a token whose `subscription.plan_id` falls back to the default plan (the historical `free` row). This is the expected Phase 1 behaviour; the "no-subscription ⇒ `plan_id=null` / `has_access=false`" assertion belongs to the Phase 2 runbook. Here we only verify the token still issues and `is_accepting_new` reflects the chosen default plan.

For a dedicated smoke identity only, ensure it has no subscription, then rotate its refresh token. Refresh and OAuth login share the token-issuance resolution path; staging must still receive one real OAuth pass before promotion.

```bash
psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -v smoke_user_id="$SMOKE_USER_ID" <<'SQL'
DELETE FROM subscriptions WHERE user_id = :'smoke_user_id';
SQL

jq -n --arg refresh_token "$REFRESH_TOKEN" \
  '{refresh_token: $refresh_token}' > "$TMP_DIR/refresh-request.json"

HTTP_STATUS="$(curl -sS -o "$TMP_DIR/no-subscription.json" -w '%{http_code}' \
  -X POST "$API_BASE/auth/refresh" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP_DIR/refresh-request.json")"
test "$HTTP_STATUS" = "200"

# Phase 1 invariant: refresh still succeeds and the response carries the
# default-plan fallback (subscription.plan_id non-null). Do NOT assert
# plan_id == null here -- that gate is a Phase 2 behaviour.
jq -e '
  .code == 0
  and (.data.subscription.plan_id != null)
  and (.data.subscription.is_accepting_new != null)
' "$TMP_DIR/no-subscription.json"
```

### 7. `quarterly` rejects new self-subscriptions

```bash
HTTP_STATUS="$(curl -sS -o "$TMP_DIR/quarterly.json" -w '%{http_code}' \
  -X POST "$API_BASE/user/subscriptions" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary '{"plan_id":"quarterly"}')"
test "$HTTP_STATUS" = "409"
jq -e '.code == 409 and (.message | contains("not accepting"))' \
  "$TMP_DIR/quarterly.json"
```

### 8. PayPal rejects a CNY plan

PayPal requires USD; `monthly` is the seeded CNY plan.

```bash
HTTP_STATUS="$(curl -sS -o "$TMP_DIR/paypal-cny.json" -w '%{http_code}' \
  -X POST "$API_BASE/payments/orders" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary '{"plan_id":"monthly","channel":"paypal"}')"
test "$HTTP_STATUS" = "400"
jq -e '.code == 400 and (.message | contains("currency"))' \
  "$TMP_DIR/paypal-cny.json"
```

### 9. PayPal accepts a USD plan

The temporary smoke plan created above is USD and accepting new subscriptions.

```bash
jq -n --arg plan_id "$SMOKE_PLAN_ID" \
  '{plan_id: $plan_id, channel: "paypal"}' > "$TMP_DIR/paypal-usd-request.json"

HTTP_STATUS="$(curl -sS -o "$TMP_DIR/paypal-usd.json" -w '%{http_code}' \
  -X POST "$API_BASE/payments/orders" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @"$TMP_DIR/paypal-usd-request.json")"
test "$HTTP_STATUS" = "201"
jq -e --arg plan_id "$SMOKE_PLAN_ID" '
  .code == 0
  and .data.plan_id == $plan_id
  and .data.currency == "USD"
  and .data.status == "pending"
' "$TMP_DIR/paypal-usd.json"

SMOKE_ORDER_ID="$(jq -er '.data.id' "$TMP_DIR/paypal-usd.json")"
export SMOKE_ORDER_ID
```

### Smoke-test cleanup

`DELETE /payments/orders/:id` cancels a pending order but retains its row. Because `orders.plan_id` is restrictive, remove the dedicated smoke order row before deleting its temporary plan.

```bash
curl -fsS -X DELETE "$API_BASE/payments/orders/$SMOKE_ORDER_ID" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .

psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -v smoke_order_id="$SMOKE_ORDER_ID" \
  -v smoke_user_id="$SMOKE_USER_ID" <<'SQL'
DELETE FROM orders
 WHERE id = :'smoke_order_id'
   AND user_id = :'smoke_user_id'
   AND status = 'cancelled';
SQL

curl -fsS -X DELETE "$API_BASE/admin/plans/$SMOKE_PLAN_ID" \
  -H "X-App-ID: $APP_ID" \
  -H "X-App-Secret: $APP_SECRET" | jq .

psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -v smoke_user_id="$SMOKE_USER_ID" <<'SQL'
DELETE FROM users WHERE id = :'smoke_user_id';
SQL

rm -rf "$TMP_DIR"
unset APP_SECRET ACCESS_TOKEN REFRESH_TOKEN SMOKE_USER_ID SMOKE_ORDER_ID
```

## Rollback

Rollback loses the commercial-field values and `plan_change_log`. Capture audit data first and use a maintenance window so old service code and the reverted schema do not run in a mismatched state.

```bash
cd /opt/yunhou-users
export PRE_PHASE1_REF="<previous-tag-or-sha>"
export ROLLBACK_DATABASE_URL="$STAGING_DATABASE_URL"   # use the correct target

pg_dump "$ROLLBACK_DATABASE_URL" \
  --data-only --table=plan_change_log \
  > "/tmp/plan_change_log-before-phase1-rollback-$(date -u +%Y%m%dT%H%M%SZ).sql"

docker compose stop app

psql "$ROLLBACK_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
DROP TRIGGER IF EXISTS plans_touch_updated_at ON plans;
DROP FUNCTION IF EXISTS plans_touch_updated_at();
DROP TABLE IF EXISTS plan_change_log;
ALTER TABLE plans
    DROP CONSTRAINT IF EXISTS plans_currency_supported,
    DROP CONSTRAINT IF EXISTS plans_trial_nonneg,
    DROP COLUMN IF EXISTS is_listed,
    DROP COLUMN IF EXISTS accepting_new_subscriptions,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS trial_days,
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS display_order,
    DROP COLUMN IF EXISTS updated_at;
COMMIT;
SQL

git checkout "$PRE_PHASE1_REF"
./deploy/deploy.sh
curl -fsS http://127.0.0.1:8080/healthz | jq .
```

Use the same sequence with the production database URL only after staging rollback has been proven. The service code must return to the previous release version as part of the same rollback window.

## Post-deploy monitoring

Watch HTTP 4xx/5xx rates, panics, database errors, plan validation failures, currency mismatch volume, and payment order creation for at least one normal traffic window.

```bash
docker compose ps
docker compose logs --since=30m app \
  | grep -Ei 'panic|fatal|error|status=(400|409|500|503)' || true

psql "$STAGING_DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT id, plan_id, actor_id, change_type, changed_at
  FROM plan_change_log
 ORDER BY changed_at DESC
 LIMIT 100;

SELECT change_type, COUNT(*)
  FROM plan_change_log
 WHERE changed_at >= now() - interval '24 hours'
 GROUP BY change_type
 ORDER BY change_type;
SQL
```

Record smoke-test outputs, monitoring links, operator, release SHA, and the go/no-go decision in the deployment ticket.

## Known issues

The Task 16 local smoke test found three release-gate mismatches in the current Phase 1 implementation:

1. **`free` is not retired by migrations 012/013.** Migration 012 leaves `free.is_active=true` and backfills `free.is_listed=true`; migration 013 only changes the audit foreign key. The local `GET /apps/yundian/plans` call returned `free`, so the “no retired free plan” smoke assertion fails. If retiring `free` is a Phase 1 release requirement, do not promote until the migration/service scope is corrected; otherwise move that assertion to the coordinated default-plan-removal phase. (Historical note: `is_listed` was already an observable filter on the public catalog by the time of the Phase 1 code review — the post-D2 `is_listed` repo filter and `PublicPlan.IsListed` exposure fixed this; the original Task 16 observation is recorded here for traceability only.)
2. **No-subscription token issuance still falls back to `free`.** After a dev login for a user with no persisted subscription, local `POST /auth/refresh` returned `subscription.plan_id="free"` and `has_access=true`, rather than `plan_id=null` and `has_access=false`. This is consistent with the default-plan fallback remaining in the current Phase 1 auth service. If the no-subscription behavior is a Phase 1 release gate, it must be fixed before promotion; otherwise the smoke gate belongs to the later auth/default-plan-removal deploy.
3. **Admin create returns zero-value timestamps in its immediate 201 payload.** Local `POST /admin/plans` returned all commercial values but reported `created_at`/`updated_at` as Go zero time. A following `GET /admin/plans/:id` returned the correct database timestamps. The smoke procedure therefore validates persisted `updated_at` through GET, but the create-response timestamp should be tracked as an API defect.

Local-only fixture note: the developer database initially contained only `free` and `monthly`. Temporary `quarterly` and USD plans were created to exercise the 409 and PayPal currency paths, then removed after the smoke test. A real staging seed must already contain `monthly`, `quarterly`, and `yearly` before promotion.

## Task 16 local verification record

Executed locally against PostgreSQL `yunhou_users` and the service on `127.0.0.1:18080`:

| Check | Result |
|---|---|
| `make build` | PASS |
| Re-run migrations 012 + 013 with `ON_ERROR_STOP=1` | PASS; idempotent notices only |
| Server startup | PASS |
| `GET /healthz` | PASS — 200 |
| `GET /apps/yundian/plans` | HTTP PASS — 200; release assertion FAIL — `free` was returned and local seed lacked quarterly/yearly |
| `POST /admin/plans` with commercial fields | PASS — 201; GET round-trip returned correct persisted fields |
| `POST /admin/plans` with `is_default:true` | PASS — 400 |
| `POST /test/login?plan_id=monthly` | PASS — 200; `is_accepting_new=true` |
| No-subscription refresh path | HTTP PASS — 200; semantic assertion FAIL — returned `free`/`has_access=true` |
| `POST /user/subscriptions` with non-accepting quarterly fixture | PASS — 409 |
| `POST /payments/orders` with PayPal + CNY monthly | PASS — 400 |
| `POST /payments/orders` with PayPal + temporary USD plan | PASS — 201 |
| Service shutdown and temporary fixture cleanup | PASS |
