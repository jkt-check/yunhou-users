# Plan Pricing Realignment — Design Spec

**Date:** 2026-07-25
**Status:** Draft (pending review)
**Author:** Claude (Yunhou Users)
**Related specs:**
- `2026-07-24-plan-commercialization-design.md` (this spec layers on top of the commercial surface: re-prices `monthly` / `yearly` and fully retires `quarterly`)
- `2026-07-23-login-subscription-decouple-design.md` (login/subscription already decoupled — the `quarterly.is_active=false` here reuses the §6.4 / §8.1 deactivated-plan branch)

## 1. Problem statement

The yunhou-website marketing site (`src/site/config/regions/cn.ts:43-65`) has been redesigned to show **only two plans** (`monthly` and `yearly`) with a promotional strikethrough price:

- monthly: actual ¥19.9 / month, strikethrough ¥29.9
- yearly: actual ¥199.9 / year, strikethrough ¥299.9

The `originalPrice` field is **frontend-static** — the cn.ts comment makes this explicit: *"it does NOT flow to the BFF and is not the actual charge."* Production pricing lives in the Yunhou admin. The frontend promo shape is already implemented and shipped; **only the yunhou-users side needs to align**.

Today yunhou-users still has the old prices and a stale `quarterly` row that's `accepting_new_subscriptions=false` but still `is_listed=true, is_active=true`. `/apps/:id/plans` therefore returns 3 rows (monthly=29.9, quarterly=79.9, yearly=299) — which conflicts with the frontend promise of "only two plans."

Goal: align yunhou-users with the frontend promo shape and retire `quarterly` cleanly.

## 2. Goal

Three concrete outcomes:

1. `plans.price` for `monthly` is `19.9` and for `yearly` is `199.9` (so `/apps/:id/plans` returns the new actual prices and BFF's `catalog.ts` 60s cache picks them up).
2. `quarterly` is fully retired: `is_listed=false` AND `is_active=false` — so it disappears from `/apps/:id/plans`, can never be subscribed (already true via `accepting_new_subscriptions=false`), and any existing subscription immediately narrows to `JWT.scope=[]` per plan_commercialization §6.4/§8.1.
3. `free` is also hidden from `/apps/:id/plans` (`is_listed=false`) for catalog hygiene — `is_active=false` was already set in migration 014.

## 3. Non-goals

- **No new `list_price` / `original_price` field on `plans`.** yunhou-website owns `originalPrice` as a static config (cn.ts:43-65) and the comment is explicit that it does not flow to the BFF. yunhou-users stays a primitive: `price` is the source of truth for the actual charge; the strikethrough is a frontend display strategy.
- **No service code changes.** The price column is read directly from DB; both `GET /admin/plans` and `GET /apps/:id/plans` reflect the new value the moment the migration applies. No model / DTO / handler / service edits.
- **No BFF changes.** `server/src/catalog.ts` 60s TTL cache picks up the new price on next refresh — no deploy required.
- **No yunhou-website changes.** The frontend already ships the new promo shape; this spec makes yunhou-users catch up to it.
- **No hard-delete of `quarterly` / `free`.** Both rows stay so historical `subscriptions.plan_id` FKs continue to resolve. `free` is already retired (is_active=false) per migration 014.
- **No new audit-log writes.** This is a one-shot data initialization (same pattern as 012's seed-row backfill: idempotent on re-run, not a per-row operator edit).

## 4. Design decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Single migration `016_plan_pricing_and_hide.sql` with three UPDATE blocks | Each block is idempotent on re-run (CASE expression matches only the targeted id; non-matching rows leave price / flags untouched). |
| 2 | Re-price only `monthly` / `yearly`; leave `quarterly.price=79.9` as historical | The price is frozen at retirement for audit — if any cancelled/expired `subscriptions` row still references `plan_id='quarterly'`, `price` is preserved for reporting/display. |
| 3 | `quarterly.is_active=false` (not just `is_listed=false`) | Confirms user intent: there are zero active quarterly subscribers (verified), so deactivating is a clean cut. Historical `plan_id` is preserved via `LoginResponse.subscription.plan_id` (plan_commercialization §7.4) so the frontend can render "季订阅已下线，请重新选择月/年套餐." |
| 4 | `free.is_listed=false` (in addition to the existing `is_active=false`) | Without this, `/apps/:id/plans` would still return `free` because `is_listed=true` is the `FindByApp` filter (repo.go:268). The frontend's "only two plans" promise requires hiding it. The row is kept so historical FKs resolve. |
| 5 | No `plan_change_log` rows from this migration | This is a seed-style data initialization (analogous to migration 012's backfill). Operator-driven edits — the kind a human wants to audit — go through `POST /admin/plans` / `PATCH /admin/plans/:id` and naturally write `plan_change_log` rows. |
| 6 | No new error sentinel | `quarterly` was already rejected at `POST /user/subscriptions` by `ErrPlanNotAcceptingNew` (plan_commercialization §6.2). Deactivating it adds no new rejection path. |
| 7 | No `list_price` field | Verified against `yunhou-website/src/site/config/regions/cn.ts:43-65` — `originalPrice` is a frontend-only static field. Adding `list_price` to yunhou API would be unused data and contradict the responsibility boundary ("business policy lives in the frontend"). |

## 5. Schema changes

### 5.1 `migrations/016_plan_pricing_and_hide.sql` — data-only, idempotent

```sql
-- (a) Re-price the two surviving public plans.
--     CASE expression matches only the targeted id; non-matching rows
--     leave price untouched. Re-running on a DB that already has the
--     new price is a no-op (idempotent).
UPDATE plans SET price = CASE id
    WHEN 'monthly' THEN 19.9
    WHEN 'yearly'  THEN 199.9
    ELSE price
END
WHERE id IN ('monthly','yearly');

-- (b) Retire quarterly fully.
--     is_listed=false hides it from /apps/:id/plans.
--     is_active=false narrows JWT.scope=[] for any (cancelled/expired)
--     historical subscriber per plan_commercialization §6.4.
--     Historical plan_id is still surfaced via LoginResponse so the
--     frontend can render "已下线，请重新订阅" copy.
UPDATE plans SET is_listed = false WHERE id = 'quarterly';
UPDATE plans SET is_active = false WHERE id = 'quarterly';

-- (c) Hide free from /apps/:id/plans.
--     is_active=false was set by migration 014; this aligns is_listed.
--     Row is kept so historical subscriptions.plan_id='free' FK still
--     resolves.
UPDATE plans SET is_listed = false WHERE id = 'free';
```

**Idempotency note:** each UPDATE is keyed on a specific `id` literal and uses `CASE` to leave non-matching rows untouched. Re-running the migration after the first apply writes the same value back, which is a no-op semantically but bumps `updated_at` (the trigger from migration 012 fires). Operators who care about a clean `updated_at` timeline should run once.

### 5.2 Post-migration state of the `plans` table

| id | price | is_active | is_listed | accepting_new_subscriptions |
|---|---|---|---|---|
| `monthly` | **19.9** (was 29.9) | true | true | true |
| `yearly` | **199.9** (was 299) | true | true | true |
| `quarterly` | 79.9 (unchanged) | **false** (was true) | **false** (was true) | false (unchanged) |
| `free` | unchanged | false (unchanged since 014) | **false** (was true) | false (unchanged since 014) |

### 5.3 Migration ordering

```
... 015 plan_change_log_nullable_snapshots
016 plan_pricing_and_hide              ← this spec
```

No future migrations depend on 016. `017_hard_delete_free.sql` (a plan_commercialization §12 follow-up) is still gated on verifying no historical cancelled/expired FK rows need `free`.

## 6. API / DTO changes

**None.** The `PublicPlan` DTO (`internal/model/plan.go:68-81`) and `Plan` struct (`internal/model/plan.go:10-25`) keep their current shape. The migration's effect is observable purely through existing endpoints:

| Endpoint | Observable change |
|---|---|
| `GET /apps/:id/plans` | Returns **exactly 2 plans**: `monthly` (price=19.9), `yearly` (price=199.9). `quarterly` and `free` filtered by `is_listed=false`. |
| `GET /admin/plans` | Returns 4 rows with the new prices / flags. Admin UI (operator-only) keeps full visibility for reporting. |
| `POST /payments/orders` | `order.amount = plan.price` → monthly orders = ¥19.9, yearly orders = ¥199.9. |
| `POST /apps/:id/quote` | `quote.amount = plan.price` → same. |
| `POST /user/subscriptions` | Already rejects `quarterly` with `ErrPlanNotAcceptingNew` (plan_commercialization §6.2). `quarterly.is_active=false` adds a second safety net. |
| `/auth/{github,wechat}/callback` | `resolvePlanForTokenIssuance` (plan_commercialization §6.4) sees `sub.plan.IsActive=false` → returns `chosenPlan=sub.plan, hasAccess=false`; `JWT.scope=[]`. |

## 7. Test fixture changes

| File | Line(s) | Change |
|---|---|---|
| `tests/e2e/testhelpers.go` | 131-149 | Update seed slice: `monthly.price=19.9`, `yearly.price=199.9`; `quarterly.is_listed=false, is_active=false`; `free.is_listed=false`. |
| `tests/e2e/extra_test.go` | 399 | Same `quarterly` fixture update. |
| `tests/integration/integration_test.go` | 78 | Same seed slice update. |
| `tests/integration/integration_test.go` | 561 | Same. |

### 7.1 Unit tests — minimum touch

| File | Decision |
|---|---|
| `internal/handler/handler_test.go:3032,3089` | Keep `Price: 29.9`. These are self-contained mock plans; the test asserts the handler's general behaviour, not "monthly equals ¥29.9." |
| `internal/service/quote_test.go:42,91,124` | Keep `Price: 29.9`. Same rationale — the test mocks a `monthly` plan and exercises the quote math; the value is arbitrary. |
| `internal/service/payment_db_test.go:54` | Keep `Price: 29.9`. Same. |
| `internal/handler/handler_test.go:2695` | **Update** to assert `Amount != 19.9` if this test still uses a monthly-plan fixture. The order-amount assertion must match the seeded plan's price or the test will silently drift. |
| `internal/service/payment_db_test.go:130` | **Update** to assert `Amount != 19.9` if it depends on the monthly seed. |
| `internal/handler/webhook_test.go` | Keep `29.90` / `2990` in webhook payloads — these are channel-side `amount` values from upstream, unrelated to `plan.price`. The webhook handler parses amount directly from the payload. |

### 7.2 New E2E test

Add to `tests/e2e/`:
- `TestE2E_PlanPricing_OnlyTwoPlansReturned` — `GET /apps/yundian/plans` returns exactly 2 rows; monthly.price=19.9, yearly.price=199.9; quarterly/free absent.
- `TestE2E_PlanPricing_QuarterlyHiddenAndInactive` — direct DB query shows `quarterly.is_listed=false AND is_active=false`.
- `TestE2E_OrderMonthly_NewPrice` — `POST /payments/orders` with monthly plan returns `amount=19.9`.

## 8. Deploy

### 8.1 Steps

1. Apply `016_plan_pricing_and_hide.sql` to staging, then prod. Pure-data UPDATE; safe to run during business hours but recommend off-peak to minimize the BFF 60s-cache window where orders quote the old price.
2. **No yunhou-users service deploy required.** `price` is read directly from DB.
3. **No BFF deploy required.** `server/src/catalog.ts` 60s TTL cache refreshes within 60s of the migration; new orders quote the new price automatically.
4. **No yunhou-website deploy required.** The frontend promo shape (cn.ts:43-65) is already shipped.

### 8.2 Verification

```
SELECT id, price, is_active, is_listed, accepting_new_subscriptions
FROM plans ORDER BY id;
```
Expected:
```
 free      | 0    | f | f | f
 monthly   | 19.9 | t | t | t
 quarterly | 79.9 | f | f | f
 yearly    | 199.9| t | t | t
```

```
curl /apps/yundian/plans | jq '.data | length'   # → 2
curl /apps/yundian/plans | jq '.data[].id'       # → ["monthly","yearly"]
```

## 9. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| BFF cache window (≤60s post-deploy) quotes old price | Order amount = ¥29.9 or ¥299 for at most 60s | Deploy off-peak; cache auto-expires. |
| Existing `quarterly` subscribers lose access immediately | `JWT.scope=[]`, frontend shows "no access" | User verified **zero active quarterly subscriptions** before spec finalization. `LoginResponse.subscription.plan_id='quarterly'` still surfaces, frontend can render "季订阅已下线，请重新订阅" CTA. |
| Test fixture drifts from prod after deploy | E2E red | Fixture update is bundled with migration in same PR; CI catches mismatch. |
| `updated_at` on `monthly`/`yearly` bumps twice if migration re-runs | Audit timeline noise | Re-running the migration writes the same `price` back, firing the `plans_touch_updated_at` trigger. Cosmetic only; if it matters, document "run once." |
| Future operator wants to restore `quarterly` | Would need to manually flip both `is_listed` and `is_active` back to `true` | Documented in operator runbook. `accepting_new_subscriptions=false` (unchanged) is the existing guard against accidental re-activation. |
| Hard-delete `free` follow-up (017) needs this spec to ship first | Sequencing risk | Migration 016 does NOT touch the `free` row's data beyond `is_listed`; 017 can proceed independently per plan_commercialization §12. |

## 10. Rollback

| Step | Rollback |
|---|---|
| Post-016 | `UPDATE plans SET price=29.9 WHERE id='monthly'; UPDATE plans SET price=299 WHERE id='yearly';` + flip `is_listed`/`is_active` back. Single-statement rollback; idempotent. |
| Operational emergency mid-deploy | Migration is a single transaction's worth of UPDATEs; rollback is symmetric. |

## 11. Open follow-ups (out of scope)

- `017_hard_delete_free.sql` (plan_commercialization §12): hard-delete `free` once confident no historical FK needs it.
- Quarterly hard-delete: deferred until historical `subscriptions.plan_id='quarterly'` rows are confirmed absent across all environments (pre-check at deployment showed zero on this env, but other envs may differ).
- `originalPrice` field: remains a frontend-only static config. If yunhou ever exposes it via API, that decision belongs to a future spec that re-evaluates the responsibility boundary.

---

## Appendix A — File map (deltas)

| # | File | Change |
|---|---|---|
| 1 | `migrations/016_plan_pricing_and_hide.sql` | NEW. 3 UPDATE blocks; idempotent. |
| 2 | `tests/e2e/testhelpers.go` | Seed slice: monthly=19.9, yearly=199.9; quarterly.is_listed=false, is_active=false; free.is_listed=false. |
| 3 | `tests/e2e/extra_test.go` | `quarterly` fixture update. |
| 4 | `tests/integration/integration_test.go` | Seed slice + `quarterly` fixture update. |
| 5 | `tests/e2e/plans_pricing_test.go` (NEW) | 3 E2E tests per §7.2. |
| 6 | `internal/handler/handler_test.go:2695` | If asserts `Amount=29.9` for monthly seed, change to `19.9`. |
| 7 | `internal/service/payment_db_test.go:130` | If asserts `Amount=29.9` for monthly seed, change to `19.9`. |
| 8 | `docs/api-integration-guide.md` | Update §Plans example payload: `monthly.price=19.9, yearly.price=199.9`. |
| 9 | `CLAUDE.md` | Update "Commercial plans" snippet to reflect ¥19.9 / ¥199.9 prices; note quarterly is retired. |