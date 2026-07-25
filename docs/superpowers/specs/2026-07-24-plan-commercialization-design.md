# Plan Commercialization — Design Spec

**Date:** 2026-07-24
**Status:** Implemented / Current (Phase 1 + Phase 2 shipped; see `2026-07-24-plan-commercialization-COMPLETION.md`)
**Author:** Claude (Yunhou Users)
**Related specs:**
- `2026-07-23-login-subscription-decouple-design.md` (login/subscription already decoupled — this spec layers the commercial surface on top)
- `2026-07-23-sub-expires-at-end-to-end-design.md` (sub_expires_at end-to-end plumbing; **out of scope** for this PR)

## 1. Problem statement

`yunhou-users` is in closed beta. Login and payments work end-to-end. The current `plans` table is a flat internal catalog:

```
plans(id PK, name, price, interval_days, apps TEXT[], is_active, is_default, created_at)
```

with four seed rows: `free` (default, apps=[yundian]), `monthly` / `quarterly` / `yearly` (CNY 29.9/79.9/299, apps=[yundian,yundash]).

This catalog is **not** a commercial surface yet. It is missing:

- **Visibility / accept-new controls.** The BFF cannot ask "is this plan still being sold?" without inspecting `is_active`, and even then there is no way to keep a plan active for existing subscribers while hiding it from new signups.
- **Currency field.** Currency lives in `orders` and `quote` (both hardcoded — CNY and USD respectively) and not on the plan itself, so the source of truth is split.
- **Trial support beyond PayPal.** Only PayPal has `trial_days` via `apps.config.payment_providers.paypal.plans[id]`; WeChat / Alipay / Stripe have no trial.
- **Marketing text and ordering.** No `description` for BFF to display, no `display_order` for ops to control list ordering; everything is sorted by `created_at`.
- **Audit timestamp.** `updated_at` is missing; admin edits to plans are not auditable.
- **App-list validation.** `apps` is a free-form `TEXT[]` with no FK and no service-layer check — typos silently land in the DB.

The bigger gap is the `default plan` mechanism itself. `peekSubscription → issueTokensForUser` falls back to the default plan (`free`) when the user has no subscription. This made sense while `free` was the always-on tier for `yundian`, but the commercial direction is now **paid-only** — every app requires an active subscription. The default-plan fallback has to disappear, and `free` has to retire.

This spec turns the plan table from a development convenience into a commercial surface, retires `free`, and removes the default-plan concept.

## 2. Goal

Make `plans` a commercial product surface and remove the `default plan` mechanism:

1. Add six columns to `plans`: `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, `description`, `display_order`, `updated_at`.
2. Delete `is_default` and the `plans_one_default` partial unique index; retire `free` (soft-delete, hard-delete deferred to a later migration).
3. Change `resolvePlanForTokenIssuance` so that users with **no subscription** get `JWT.scope=[]` and `HasAccess=false` (no default-plan fallback).
4. Change the expired-subscription branch so `JWT.scope=[]` (security) while `LoginResponse.subscription.plan_id` preserves the user's historical plan (so the BFF can render the renewal CTA).
5. Add a new `plan_change_log` table to audit plan mutations (especially `apps` changes that immediately affect every subscriber's `JWT.scope`).
6. Make `currency` the source of truth — `quote.currency` reads `plan.currency`, `orders.currency` reads `plan.currency`, the existing USD/CNY hardcodes disappear.
7. Pull `trial_days` out of `apps.config.payment_providers.paypal.plans[id]` and onto `plan.trial_days`.
8. Validate `plan.apps` at the service layer (every app_id must exist and `is_active=true`).
9. Upgrade admin-plan CRUD validation so negative / wrong-currency / wrong-trial values are rejected at the handler with `400` rather than blowing up at the DB CHECK as `500`.

## 3. Non-goals

- **No upgrade / downgrade service endpoints.** The BFF composes `cancel` + `subscribe` itself. yunhou-users stays a primitive. (Per `CLAUDE.md`: *business policy lives in the frontend.*)
- **No `sub_expires_at` end-to-end plumbing.** That lives in `2026-07-23-sub-expires-at-end-to-end-design.md` (still Draft).
- **No breaking up `apps TEXT[]` into a `plan_apps` join table.** The decision is to retain `TEXT[]` and add service-layer validation + audit logging. SQL stays unchanged.
- **No currency conversion.** We add the `currency` column but do not implement FX. Each plan is single-currency; PayPal sandbox may use USD while production uses CNY.
- **No plan-tier reordering mechanics beyond `display_order`.** Tier comparison (e.g., "is `yearly` higher than `monthly`?") is not modeled — the BFF decides.
- **No re-pricing.** Existing CNY 29.9 / 79.9 / 299 prices stay as-is. The fact that quarterly's per-day cost (¥0.89) is higher than yearly's (¥0.82) is a **pricing-strategy follow-up** flagged in §11.
- **No change to the OAuth redirect flow.** `/auth/{github,wechat}/redirect` and `/auth/{github,wechat}/callback` are unchanged.
- **No new `RequireSubscription` middleware.** API-level 402 stays the BFF's responsibility via `subscription.has_access`.

## 4. Design decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Add `is_listed` and `accepting_new_subscriptions` as two separate booleans | Lets ops express both "visible in BFF list, accepting new subs" and "hidden but subscribable from admin" and "legacy (visible, not accepting new subs)" without an enum. The latter is what `quarterly` becomes. |
| 2 | Retire `free` by setting `is_active=false, accepting_new_subscriptions=false`, keep the row | Hard-delete is deferred to a follow-up migration so historical `subscriptions.plan_id='free'` rows can still resolve their FK. The pre-check in migration 014 aborts if any active free subscription exists. |
| 3 | Delete `is_default` column + `plans_one_default` partial unique index | The default-plan concept is the user-facing "what does an anonymous user get?" question. The answer becomes "nothing — they need to subscribe." Column removal forces every caller to make an explicit choice. |
| 4 | `resolvePlanForTokenIssuance` becomes a three-state decision without a default-plan fallback | The three observable states are still `sub=nil`, `sub≠nil && expired=true`, and `sub≠nil && expired=false`. What disappears is the `defaultPlan` variable and the branches that re-pointed `chosenPlan` at it. `chosenPlan` may now be `nil` when `sub=nil`. |
| 5 | Expired-subscription users get `JWT.scope=[]` (not the plan's apps) | Security: a 15-minute access token with the previous scope lets a lapsed user keep calling APIs until expiry. The renewal CTA comes from `LoginResponse.subscription.plan_id`, not from the token. |
| 6 | `LoginResponse.subscription.plan_id` preserves the historical plan id on expired subs | Lets the BFF render *按月订阅 已过期，请续费* with the correct plan name and price. |
| 7 | `LoginResponse.subscription.is_accepting_new` is a new boolean field | Lets the BFF distinguish "your sub is fine" from "your sub is fine, but if you wanted to switch, that plan isn't sold anymore." |
| 8 | `currency` lives on `plan`, not on `orders` / `quote` | One source of truth. `orders.currency` and `quote.currency` are derived. The hardcoded `"CNY"` (in `payment.go:222`) and `"USD"` (in `quote.go:82`) disappear. |
| 9 | `trial_days` lives on `plan`, not on `apps.config.payment_providers.paypal.plans[id]` | One source of truth. `Paypal.Plans[id].trial_days` is parsed-but-deprecated; new writes go to `plan.trial_days`. |
| 10 | `updated_at` maintained by a DB trigger, not by service code | Defense against any caller forgetting to write it. The `BEFORE UPDATE` trigger reassigns the value unconditionally. |
| 11 | New `plan_change_log` table (separate from `audit_log`) | `audit_log` is structured around `entity_type='subscription'` etc.; plan-mutation events have different fields (no `user_id`, instead `actor_id` + `before/after` snapshots) and a different read pattern (per-plan timeline). |
| 12 | Service-layer `ValidateApps` for plan CRUD | Defense against typo'd `app_id` reaching the DB. The check uses `appRepo.FindByID` + `is_active=true`. |
| 13 | `SubscriptionService.Create` and `PaymentService.CreateOrder` reject `!plan.AcceptingNewSubscriptions` | Without this guard, `quarterly` could still be self-subscribed via `/user/subscriptions` or paid via `/payments/orders`. |
| 14 | `SubscriptionService.Create` keeps the `plan.Price > 0 → ErrPaidPlanForbidden` guard | Defense against an ops mistake that flips a paid plan to `price=0` to enable a self-subscribe bypass. Removed only when payment-driven self-subscribe lands (out of scope). |
| 15 | Admin `POST /admin/plans` and `PATCH /admin/plans/:id` reject `is_default` input | Once the column is gone, accepting the field silently would mask migration ordering bugs. Reject with `400` so any stale client surfaces the issue immediately. |
| 16 | `POST /test/login` (dev-only) requires `?plan_id=xxx` | Without a default plan, the dev/test login needs an explicit target. Reject 400 if missing, 404 if plan doesn't exist, 400 if plan is not active. |
| 17 | Two-phase migration (012 then 014) | 012 is purely additive and backwards compatible — service can be deployed before, after, or simultaneously. 014 is the behavioral switch (drop default-plan fallback, retire free) and must deploy together with the code that no longer references `is_default`. |
| 18 | `display_order` defaults to 0; seed assigns 10/20/30 to monthly/quarterly/yearly | BFF sorts `ORDER BY display_order ASC, created_at ASC, id ASC`. Ties break deterministically. |
| 19 | `currency` CHECK constraint allows `('CNY','USD','EUR')` | We're not adding more, but the constraint keeps the column tight; adding a new currency is a one-line `ALTER TABLE` later. |
| 20 | `PlanService.FindDefault` and `PlanRepo.FindDefault` are removed entirely | The T20 cleanup deleted the method bodies, the interface declarations, and the mock plumbing. `ErrDeprecatedDefaultPlan` is now unused dead code kept only because removing it would touch call sites that are already being deleted as part of T17/T18/T19; it is a follow-up to drop the sentinel once the interface signatures are verified cold. |

## 5. Schema changes

### 5.1 `migrations/012_plan_commercial_fields.sql` — additive, backwards-compatible

```sql
-- (a) Add new columns
ALTER TABLE plans
    ADD COLUMN IF NOT EXISTS is_listed                   BOOLEAN     NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS accepting_new_subscriptions BOOLEAN     NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS currency                    TEXT        NOT NULL DEFAULT 'CNY',
    ADD COLUMN IF NOT EXISTS trial_days                  INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS description                 TEXT,
    ADD COLUMN IF NOT EXISTS display_order               INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now();

-- (b) Constraints
ALTER TABLE plans ADD CONSTRAINT IF NOT EXISTS plans_currency_supported
    CHECK (currency IN ('CNY','USD','EUR'));
ALTER TABLE plans ADD CONSTRAINT IF NOT EXISTS plans_trial_nonneg
    CHECK (trial_days >= 0);

-- (c) updated_at trigger
CREATE OR REPLACE FUNCTION plans_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS plans_touch_updated_at ON plans;
CREATE TRIGGER plans_touch_updated_at
    BEFORE UPDATE ON plans
    FOR EACH ROW EXECUTE FUNCTION plans_touch_updated_at();

-- (d) Backfill the existing 4 rows
UPDATE plans SET
    currency = 'CNY',
    trial_days = 0,
    description = CASE id
        WHEN 'monthly'   THEN '按月订阅 ¥29.9，自动续费，可随时取消'
        WHEN 'quarterly' THEN '按季订阅 ¥79.9，暂不开放新订阅，已有订阅保留'
        WHEN 'yearly'    THEN '按年订阅 ¥299（约 83 折，比月付年省 ¥59.8）'
        WHEN 'free'      THEN '免费版（已下线）'
    END,
    is_listed = true,
    accepting_new_subscriptions = CASE id WHEN 'free' THEN false WHEN 'quarterly' THEN false ELSE true END,
    display_order = CASE id
        WHEN 'monthly' THEN 10
        WHEN 'quarterly' THEN 20
        WHEN 'yearly' THEN 30
        ELSE 0
    END
WHERE id IN ('monthly','quarterly','yearly','free');

-- (e) plan_change_log (new audit table)
CREATE TABLE IF NOT EXISTS plan_change_log (
    id          BIGSERIAL PRIMARY KEY,
    plan_id     TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    actor_id    TEXT NOT NULL,                              -- 'admin:<appID>' or 'system:<job>'
    change_type TEXT NOT NULL CHECK (change_type IN ('apps_update','plan_create','plan_update','plan_deactivate','plan_archive')),
    before      JSONB NOT NULL,
    after       JSONB NOT NULL,
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_plan_change_log_plan_changed_at
    ON plan_change_log (plan_id, changed_at DESC);
```

**012 deliberately does not:**
- Drop `is_default`
- Drop `plans_one_default`
- Touch `free` semantics beyond writing the new fields
- Touch token issuance or subscription code paths

### 5.2 `migrations/014_remove_default_plan.sql` — behavioral switch, requires coordinated deploy

```sql
-- Pre-check: abort if any active subscription still references plan_id='free'.
DO $$
DECLARE cnt INT;
BEGIN
    SELECT COUNT(*) INTO cnt FROM subscriptions WHERE plan_id = 'free' AND status = 'active';
    IF cnt > 0 THEN
        RAISE EXCEPTION 'migration aborted: % active subscriptions still reference plan_id=free; cancel them first', cnt;
    END IF;
END $$;

-- Retire 'free' (keep row so historical cancelled/expired FK still resolves).
UPDATE plans SET is_active = false, accepting_new_subscriptions = false WHERE id = 'free';

-- Drop the partial unique index (no longer relevant once is_default is gone).
DROP INDEX IF EXISTS plans_one_default;

-- Drop is_default column.
ALTER TABLE plans DROP COLUMN IF EXISTS is_default;
```

**014 must deploy together with the code changes** in §6/§7/§8 because:
- `model.Plan.IsDefault` is being removed (compilation breaks if column removed first)
- `PlanService` stops calling `FindDefault` (still safe even before 014, but the cleanup is a single logical step)

### 5.3 Migration ordering

```
... 011 (existing order_reconcile)
012 plan_commercial_fields           ← additive
013 plan_change_log_fk_set_null       ← audit FK relax (added in code review; ships with Phase 1)
014 remove_default_plan               ← switch
015 plan_change_log_nullable_snapshots ← audit snapshot nullability (added in code review; ships with Phase 1)
016 (future, out of scope) hard_delete_free
```

### 5.4 Pre-deploy checklist for ops

1. Run on staging first; `SELECT COUNT(*) FROM subscriptions WHERE plan_id='free' AND status='active'` returns 0.
2. If non-zero, manually cancel those subscriptions (or migrate them to `monthly` first).
3. Apply 012 + 013 + 015, redeploy service (no behavior change yet).
4. Apply 014 and deploy the new service in the same maintenance window.

## 6. Service-layer changes

### 6.1 `internal/service/plan.go`

**New helper**
```go
// ValidateApps returns ErrInvalidAppID if any app_id in apps does not exist
// or is_active=false. Duplicates tolerated.
func (s *PlanService) ValidateApps(ctx context.Context, apps []string) error
```

**CreatePlan / UpdatePlan**
- Run `ValidateApps` before INSERT / UPDATE.
- After successful write, insert into `plan_change_log` with the `before` (null for create) / `after` snapshot.

**UpdatePlan diff payload**
- The `before` / `after` JSON contains the union of: `apps`, `currency`, `trial_days`, `description`, `display_order`, `is_listed`, `accepting_new_subscriptions`, `is_active`. Other fields (`name`, `price`, `interval_days`, `id`) are excluded from the audit payload but still updated.

**DeletePlan**
- After successful DELETE, insert `plan_change_log` with `change_type='plan_archive'`, `before=<last-known plan snapshot>`, `after=null`.
- FK 23503 is still surfaced by the handler as `409 Conflict`.

**FindDefault**
- Removed entirely by T20. The interface declaration, the `PlanService` method, and the `PlanRepo.FindDefault` SQL are all gone; `grep` for `FindDefault` in `internal/` returns zero matches. `ErrDeprecatedDefaultPlan` is now an unused sentinel — see follow-up note in decision row 20.

### 6.2 `internal/service/subscription.go`

**Create — new validation order**
```go
plan, err := s.planRepo.FindByID(ctx, planID)
if err != nil { return ErrPlanNotFound }
if !plan.IsActive                  { return ErrPlanInactive }
if !plan.AcceptingNewSubscriptions { return ErrPlanNotAcceptingNew }   // ← new
if plan.Price > 0                  { return ErrPaidPlanForbidden }     // preserved
// existing active-sub check (unchanged)
```

**Renew / Cancel** — unchanged.

### 6.3 `internal/service/payment.go`

**CreateOrder — new validation order**
```go
plan, err := s.planRepo.FindByID(ctx, planID)
if err != nil { return ErrPlanNotFound }
if !plan.IsActive                  { return ErrPlanInactive }
if !plan.AcceptingNewSubscriptions { return ErrPlanNotAcceptingNew }   // ← new
if plan.Currency != order.Currency { return ErrPlanCurrencyMismatch }  // ← new
```

**Currency source-of-truth switch**
- The literal `"CNY"` at `payment.go:222` (the current order-creation hardcode) is replaced with `plan.Currency`.
- The literal `"USD"` at `quote.go:82` is replaced with `plan.Currency`.

**onRefundSucceeded** — unchanged. (Already narrows by `orders.plan_id`.)

### 6.4 `internal/service/auth.go` — `resolvePlanForTokenIssuance`

**Before** (existing three-state decision with default-plan fallback)
```go
sub, expired, err := peekSubscription(ctx, user.ID)
defaultPlan, _ := s.planRepo.FindDefault(ctx)
switch {
case sub == nil:           chosenPlan = defaultPlan; hasAccess = defaultPlan.Apps ⊇ appID
case expired:              chosenPlan = defaultPlan; hasAccess = false  // WRONG: scope leak via sub.plan.Apps
default:                   chosenPlan = sub.plan;    hasAccess = sub.plan.Apps ⊇ appID
}
```

**After** (three-state decision; default-plan variable gone)
```go
sub, expired, err := peekSubscription(ctx, user.ID)
if err != nil { return nil, false, err }

switch {
case sub == nil:
    // Unauthenticated-equivalent: scope=[], has_access=false, chosenPlan=nil.
    return nil, false, nil
case expired:
    // Scope is forced to [] for security; chosenPlan is preserved so that
    // LoginResponse.subscription.plan_id can render the renewal CTA.
    return sub.plan, false, nil
default:
    // Active sub. Deactivated plans (P1 fix) must still narrow scope to [].
    hasAccess := sub.plan.IsActive && containsApp(sub.plan.Apps, appID)
    return sub.plan, hasAccess, nil
}
```

**peekSubscription** — unchanged. Still returns `(sub, expired, err)` with `expired = sub != nil && sub.ExpiresAt != nil && *sub.ExpiresAt < time.Now()`. The `expired` boolean is consumed by the new logic; no caller change required beyond the switch above.

### 6.5 `internal/service/quote.go`

**CycleConfig source switch**
```go
cycle := model.CycleConfig{
    TrialDays:        plan.TrialDays,        // ← was AppConfig.Paypal.Plans[id].TrialDays
    BillingCycleDays: plan.IntervalDays,     // unchanged source
}
```

**Currency source switch**
```go
quote.Currency = plan.Currency  // ← was literal "USD"
```

**Backwards-compat read of `AppConfig.Paypal.Plans[id].trial_days` is dropped.** The field is still parsed for any external consumer that reads `apps.config`, but `quote` no longer reads it.

### 6.6 `cmd/server/main.go` — `POST /test/login`

- Now requires `?plan_id=xxx` query parameter.
- Missing → `400 {"message":"plan_id is required"}`.
- Plan not found → `404`.
- Plan `is_active=false` or `accepting_new_subscriptions=false` → `400`.
- No fallback to `defaultPlan`; the test endpoint assumes the caller knows which plan it wants to mint a token for.

### 6.7 New error sentinels

| Sentinel | HTTP | Used in |
|---|---|---|
| `service.ErrPlanNotAcceptingNew` | 409 | `SubscriptionService.Create`, `PaymentService.CreateOrder`, `POST /test/login` |
| `service.ErrPlanCurrencyMismatch` | 400 | `PaymentService.CreateOrder` |
| `service.ErrInvalidAppID` | 400 | `PlanService.ValidateApps` |
| `service.ErrDeprecatedDefaultPlan` | 410 | (unused — `PlanService.FindDefault` removed entirely; sentinel kept as dead code) |

## 7. Handler / API changes

### 7.1 `internal/handler/app.go` — admin plan CRUD

**`POST /admin/plans` request body** (added fields)
```json
{
  "id": "string",
  "name": "string",
  "price": 0.0,
  "interval_days": 0,
  "apps": ["app_id", ...],
  "is_listed": true,
  "accepting_new_subscriptions": true,
  "currency": "CNY",
  "trial_days": 0,
  "description": "...",
  "display_order": 0
}
```

**Rejected fields**
- `is_default` → `400 {"message":"is_default is no longer supported; use plan selection logic in BFF"}`.

**Validation upgrade** (handler runs these before calling service)
- `price >= 0` → `400 {"message":"price must be non-negative"}`
- `interval_days >= 0` → `400 {"message":"interval_days must be non-negative"}`
- `currency ∈ {CNY,USD,EUR}` → `400 {"message":"currency must be one of CNY/USD/EUR"}`
- `trial_days >= 0` → `400 {"message":"trial_days must be non-negative"}`
- `apps` → forwarded to `PlanService.ValidateApps`, which returns `400 ErrInvalidAppID`.

**`PATCH /admin/plans/:id`**
- Same rejected field (`is_default`) and same validations.
- All fields are `*T` for partial update (existing pattern retained).
- Audit row written with the diff per §6.1.

**`DELETE /admin/plans/:id`**
- FK 23503 still maps to `409 {"message":"plan is in use by existing subscriptions"}`.
- Audit row written with `change_type='plan_archive'`, `after=null`.

**`GET /admin/plans[/:id]`**
- Returns the new fields. JSON shape is additive; existing BFF readers ignore unknown fields.

### 7.2 `internal/model/plan.go` — `PublicPlan` DTO

```go
type PublicPlan struct {
    ID                       string             `json:"id"`
    Name                     string             `json:"name"`
    Price                    float64            `json:"price"`
    IntervalDays             int                `json:"interval_days"`
    Currency                 string             `json:"currency"`
    TrialDays                int                `json:"trial_days"`
    Description              *string            `json:"description"`     // pointer for nullable
    IsListed                 bool               `json:"is_listed"`
    Apps                     []string           `json:"apps"`
    DisplayOrder             int                `json:"display_order"`
    ProviderIDs              map[string]string  `json:"provider_ids"`
    Cycle                    *CycleSummary      `json:"cycle"`           // {trial_days, billing_cycle_days}
}
```

`IsActive` remains excluded (was already excluded). `IsDefault` is gone.

### 7.3 `internal/router/router.go` — `/apps/:id/plans` ordering

`PlanRepo.FindByApp` SQL change:
```sql
SELECT * FROM plans
WHERE $1 = ANY(apps) AND is_active = true AND is_listed = true
ORDER BY display_order ASC, created_at ASC, id ASC
```

### 7.4 `internal/service/auth.go` — `LoginResponse.Subscription` (additive)

```go
type SubscriptionInfo struct {
    PlanID         string     `json:"plan_id"`
    PlanName       string     `json:"plan_name"`
    HasAccess      bool       `json:"has_access"`
    ExpiresAt      *time.Time `json:"expires_at,omitempty"`
    IsAcceptingNew bool       `json:"is_accepting_new"`  // ← new
}
```

> **Implementation note:** the struct lives in `internal/service/auth.go` (not `internal/model/`); `PlanID` / `PlanName` are value types (not `*string`); `ExpiresAt` keeps `omitempty`. The new field is additive and does not change existing wire behaviour for callers that omit it from their decoders.

- `IsAcceptingNew = plan != nil && plan.IsActive && plan.AcceptingNewSubscriptions`
- For un-subscribed users: all pointer fields nil, `HasAccess=false`, `IsAcceptingNew=false`.

### 7.5 `/apps/:id/quote`

- Request unchanged.
- Response `Quote.Currency` reflects `plan.Currency` (was `"USD"`).
- Response `Quote.CycleConfig.TrialDays` reflects `plan.TrialDays` (was `AppConfig.Paypal.Plans[id].TrialDays`).

### 7.6 `/payments/orders`

- Request unchanged.
- New `409 ErrPlanNotAcceptingNew` and `400 ErrPlanCurrencyMismatch` rejection paths.
- `Order.Currency` written from `plan.Currency` (was hardcoded `"CNY"`).

## 8. JWT scope path

### 8.1 Decision matrix (after this spec)

| User state | `JWT.scope` | `JWT.aud` | `JWT.app_id` | `LoginResponse.subscription.plan_id` | `LoginResponse.subscription.has_access` | `is_accepting_new` |
|---|---|---|---|---|---|---|
| Unauthenticated-equivalent (no sub) | `[]` | `[appID]` | `appID` | `null` | `false` | `false` |
| Expired sub (any plan) | `[]` | `[appID]` | `appID` | `sub.plan.ID` (preserved) | `false` | `sub.plan.IsActive && sub.plan.AcceptingNewSubscriptions` |
| Active sub, `sub.plan.IsActive=true`, `apps ⊇ appID` | `sub.plan.Apps` | `[appID]` | `appID` | `sub.plan.ID` | `true` | `sub.plan.IsActive && sub.plan.AcceptingNewSubscriptions` |
| Active sub, `sub.plan.IsActive=true`, `apps ⊄ appID` | `sub.plan.Apps` | `[appID]` | `appID` | `sub.plan.ID` | `false` | `sub.plan.IsActive && sub.plan.AcceptingNewSubscriptions` |
| Active sub, `sub.plan.IsActive=false` (P1 fix) | `[]` | `[appID]` | `appID` | `sub.plan.ID` | `false` | `false` |

### 8.2 Why `aud` and `app_id` stay populated

`aud=[appID]` and `app_id=appID` mean *the token is intended for appID*. `scope` carries the actual authorization list. Per RFC 7519 they are independent. Decoupling lets the BFF:

1. Read `aud` to know *which app requested the token*.
2. Read `scope` to know *what the user can do*.
3. Read `subscription.has_access` to decide whether to render protected UI.

The BFF never has to introspect the URL the token was issued from.

## 9. Data flow (after fix)

```
Browser → /auth/wechat/callback
   ↓
LoginWithProfile(profile)
   ↓
peekSubscription(userID) → (sub, expired, nil)
   ↓
resolvePlanForTokenIssuance(user, appID)
   - sub=nil              → chosenPlan=nil,    HasAccess=false
   - expired=true         → chosenPlan=sub.plan, HasAccess=false (scope forced to [])
   - sub!=nil, !expired   → chosenPlan=sub.plan, HasAccess = sub.plan.IsActive && apps ⊇ appID
   ↓
issueTokensForUser(user, appID)
   - scope = chosenPlan.Apps (or [] when chosenPlan is nil / expired)
   - aud = [appID], app_id = appID
   ↓
302 to BFF with #access_token=...&refresh_token=...
   ↓
BFF stores in /auth/me; renders LoginResponse.subscription.*
```

No sentinel bubbles up. `JWT.scope` correctly reflects active authorization; the `subscription` block in the response carries the user's intent (so the renewal CTA still has a target).

## 10. Testing strategy

### 10.1 Unit (`internal/service/`, `internal/handler/`)

| Test | Asserts |
|---|---|
| `TestPlanService_CreatePlan_InvalidApps` | `ValidateApps` returns `ErrInvalidAppID` when one app_id is unknown / inactive |
| `TestPlanService_UpdatePlan_WritesChangeLog` | `UpdatePlan` writes a `plan_change_log` row with the correct `before/after` |
| `TestPlanService_FindByApp_SortsByDisplayOrder` | Plans returned in `display_order ASC, created_at ASC, id ASC` order |
| `TestSubscriptionService_Create_NotAcceptingNew` | `quarterly` (accepting_new=false) → `ErrPlanNotAcceptingNew` |
| `TestSubscriptionService_Create_PaidPlanForbidden` | `monthly` (price>0) self-subscribe → `ErrPaidPlanForbidden` (guard preserved) |
| `TestPaymentService_CreateOrder_CurrencyMismatch` | order currency doesn't match plan currency → `ErrPlanCurrencyMismatch` |
| `TestAuthService_ResolvePlanForTokenIssuance_NoSub` | chosenPlan=nil, hasAccess=false |
| `TestAuthService_ResolvePlanForTokenIssuance_ExpiredSub` | chosenPlan=sub.plan, hasAccess=false, scope forced to `[]` |
| `TestAuthService_ResolvePlanForTokenIssuance_ActiveSub_PlanDeactivated` | chosenPlan=sub.plan, hasAccess=false (P1 fix) |
| `TestHandler_AdminPlans_RejectsIsDefault` | POST/PATCH `/admin/plans[/:id]` with `is_default` → 400 |

### 10.2 E2E (`tests/e2e/`)

| Test | Asserts |
|---|---|
| `TestE2E_PlanCommercial_CreateWithNewFields` | All seven new fields round-trip through POST/GET/PATCH |
| `TestE2E_LoginNoSubscription_HasAccessFalse` | Login without any sub → response `subscription.plan_id=nil, has_access=false`, JWT scope empty |
| `TestE2E_LoginExpiredSubscription_PreservesPlanID` | Expired sub user → response `plan_id=<historical>`, has_access=false, JWT scope empty |
| `TestE2E_PlanNotAcceptingNew_Quarterly` | POST `/user/subscriptions` with `quarterly` → 409 |
| `TestE2E_PlanCurrencyMismatch_Order` | Plan CNY + channel USD → 400 |
| `TestE2E_AppsValidation_TypoAppID` | POST `/admin/plans` with `apps=["nope"]` → 400 |
| `TestE2E_PostQuote_ReadsPlanTrialAndCurrency` | Plan trial_days=7 + USD; quote returns those values, not `AppConfig` overrides |

### 10.3 Test fixture cleanup

- `tests/e2e/testhelpers.go:121` — drop the `UPDATE plans SET is_default = false` step.
- `tests/e2e/testhelpers.go:131-149` — extend the seed slice with `currency`, `trial_days`, `description`, `display_order`, `is_listed`, `accepting_new_subscriptions` fields.
- `tests/e2e/extra_test.go:399` — update the `quarterly` seed to include `accepting_new_subscriptions=false`.
- `tests/integration/integration_test.go:78, :561` — update to the new column set.

## 11. Risks and rollback

### 11.1 Known follow-up (pricing)

The seeded prices make `quarterly` (¥79.9 / 90 days = ¥0.89/day) more expensive per day than `yearly` (¥299 / 365 days = ¥0.82/day). This contradicts the standard SaaS discount gradient. Flagged as a **pricing-strategy follow-up**: product decides whether to re-price `quarterly` (e.g., `¥69.9`) or `yearly` (e.g., `¥249`). The schema (`price DECIMAL(10,2)`) supports either; no spec change required.

### 11.2 Risk table

| Risk | Impact | Mitigation |
|---|---|---|
| 014 deploys without the code change (or vice versa) | Build failure or runtime `Plan.IsDefault` zero-value mishaps | Document single deploy window; CI lint to flag `model.Plan.IsDefault` / `FindDefault` references |
| Pre-check fails: active `plan_id='free'` subscription exists | Migration 014 aborts | Documented operator runbook: cancel those rows, or migrate them to `monthly` first |
| BFF sees new `subscription.is_accepting_new=false` and silently breaks | Renewal CTA fails open | New field is **additive**; old BFF clients ignore it. The renewal CTA still works because `plan_id` and `has_access` are unchanged |
| Quarterly existing subscribers hit `accepting_new=false` on renewal | They can't renew | `SubscriptionService.Renew` does **not** check `accepting_new_subscriptions` (only `plan.IsActive`). Quarterly renews are still allowed |
| `POST /test/login` breaks in e2e | Local CI red | E2E fixtures updated in §10.3 |
| Trial semantics change for PayPal users | Quote response `trial_days` differs from before | BFF receives the new value (single source of truth on `plan`). One-time reconciliation: BFF reloads quote endpoints to learn the new value |

### 11.3 Rollback

| Phase | Rollback |
|---|---|
| After 012 only | `ALTER TABLE plans DROP COLUMN ...` (lose data, no behavior impact since defaults are conservative). Trigger can stay or drop. |
| After 014 | Re-add `is_default BOOLEAN DEFAULT false`, recreate `plans_one_default` partial unique index, set `free` to `is_default=true` again, restore the `defaultPlan` branch in `resolvePlanForTokenIssuance`. Service reverts cleanly because `model.Plan.IsDefault` is reintroduced and `FindDefault` body is intact. |
| Operational emergency | PATCH `is_listed=false` on any plan to hide from BFF without code change. PATCH `accepting_new_subscriptions=false` to stop new signups. |

## 12. Open follow-ups (out of scope)

- `016_hard_delete_free.sql` — actually `DELETE FROM plans WHERE id='free'` once we are confident no historical cancelled/expired FKs rely on it. (The 014 / 015 follow-ups raised this number from the originally-planned 014.)
- `2026-07-23-sub-expires-at-end-to-end-design.md` — `sub_expires_at` end-to-end plumbing (separate spec, separate PR).
- Pricing re-evaluation per §11.1.
- `apps` join table — revisited only if `TEXT[]` validation proves insufficient at scale.

---

## Appendix A — File map (deltas)

| # | File | Change |
|---|---|---|
| 1 | `migrations/012_plan_commercial_fields.sql` | NEW. Adds 7 columns + 2 CHECKs + trigger + plan_change_log + data backfill |
| 2 | `migrations/014_remove_default_plan.sql` | NEW. Pre-check + retire `free` + drop `plans_one_default` + drop `is_default` |
| 3 | `internal/model/plan.go` | Add 7 fields; remove `IsDefault`; expand `PublicPlan` |
| 4 | `internal/model/app_test.go`, `internal/model/app.go` | (If `app_test.go` references removed fields; TBD during implementation) |
| 5 | `internal/service/auth.go` | Add `IsAcceptingNew` to `SubscriptionInfo` (struct already lives in this file with value-type `PlanID`/`PlanName` and `ExpiresAt *time.Time` with `omitempty`) |
| 6 | `internal/model/quote.go` | (No schema change; `Currency` already a string) |
| 7 | `internal/repo/repo.go` | `PlanRepo.Create/Update` columns updated; `FindByApp` ORDER BY changed; `FindDefault` removed entirely |
| 8 | `internal/service/plan.go` | `ValidateApps` helper; `CreatePlan`/`UpdatePlan`/`DeletePlan` audit writes; remove `IsDefault` references |
| 9 | `internal/service/subscription.go` | New validation order in `Create` |
| 10 | `internal/service/payment.go` | `CreateOrder` reads `plan.Currency`; new validation order |
| 11 | `internal/service/auth.go` | `resolvePlanForTokenIssuance` rewrite (three-state, no default-plan fallback) |
| 12 | `internal/service/quote.go` | `CycleConfig.TrialDays` from `plan.TrialDays`; `Currency` from `plan.Currency` |
| 13 | `internal/service/errors.go` | Add `ErrPlanNotAcceptingNew`, `ErrPlanCurrencyMismatch`, `ErrInvalidAppID`, `ErrDeprecatedDefaultPlan` |
| 14 | `internal/service/*_test.go` | New unit tests per §10.1 |
| 15 | `internal/handler/app.go` | Admin plan CRUD validation upgrade; reject `is_default` |
| 16 | `internal/handler/auth.go` | No direct change; new field surfaces via `LoginResponse` shape |
| 17 | `cmd/server/main.go` | `POST /test/login` requires `?plan_id` |
| 18 | `tests/e2e/testhelpers.go` | Drop `is_default` update; expand plan seed slice |
| 19 | `tests/e2e/extra_test.go` | Add `accepting_new_subscriptions=false` to `quarterly` fixture |
| 20 | `tests/integration/integration_test.go` | Update plan INSERTs |
| 21 | `tests/e2e/plans_quote_test.go`, `tests/e2e/e2e_test.go` | New e2e tests per §10.2 |
| 22 | `docs/api-integration-guide.md` | Update `§Plans` (new fields); update `§Subscriptions` (no default plan) |
| 23 | `CLAUDE.md` | "Plan-based access" paragraph; "Endpoints" table; add "Commercial plans" note |