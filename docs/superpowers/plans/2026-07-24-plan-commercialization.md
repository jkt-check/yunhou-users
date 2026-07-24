# Plan Commercialization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Convert the `plans` table from an internal catalog into a commercial product surface — retire `free`, remove the default-plan concept, add 7 plan fields (is_listed / accepting_new_subscriptions / currency / trial_days / description / display_order / updated_at), and switch `quote`/`order`/`subscription` to read currency + trial_days from `plan` rather than from `orders` or `AppConfig`.

**Architecture:** Two-phase deploy. Phase 1 applies `migrations/012_plan_commercial_fields.sql` (purely additive: new columns + new audit table + data backfill) and deploys service code that writes/reads those new fields without changing any default-plan behavior. Phase 2 applies `migrations/013_remove_default_plan.sql` (drop `is_default`, retire `free`, drop partial unique index) and deploys the auth-service rewrite that removes the default-plan fallback. Phase 1 is independently deployable and rollback-friendly; Phase 2 must deploy together with the auth-service change because the code can no longer reference `model.Plan.IsDefault` after 013.

**Tech Stack:** Go 1.22, PostgreSQL, sqlx, Echo, JWT (RS256), testify (mock in service tests). Tests are unit (`internal/service`, `internal/handler`) + E2E (`tests/e2e/`) + integration (`tests/integration/`).

**Spec:** `docs/superpowers/specs/2026-07-24-plan-commercialization-design.md`

---

## File map

### New files

| Path | Purpose |
|---|---|
| `migrations/012_plan_commercial_fields.sql` | Add 7 columns + CHECKs + trigger + plan_change_log + backfill |
| `migrations/013_remove_default_plan.sql` | Pre-check + retire free + drop plans_one_default + drop is_default |
| `tests/e2e/plan_commercial_test.go` | E2E tests per spec §10.2 (Phase 1 subset) |
| `tests/e2e/plan_commercial_phase2_test.go` | E2E tests for retired-free + no-default (Phase 2 subset) |

### Modified files

| Path | Phase | Change |
|---|---|---|
| `internal/model/plan.go` | 1 | Add 7 fields; remove `IsDefault` in Phase 2; expand `PublicPlan` |
| `internal/model/auth.go` | 1 | Add `IsAcceptingNew` to `LoginSubscriptionInfo` |
| `internal/repo/repo.go` | 1 | PlanRepo Create/Update new columns; FindByApp ORDER BY |
| `internal/service/plan.go` | 1+2 | ValidateApps; audit log; Create/Update/Delete with new fields; FindDefault deprecated |
| `internal/service/subscription.go` | 1 | Create — accepting_new_subscriptions guard |
| `internal/service/payment.go` | 1 | CreateOrder — accepting_new_subscriptions + currency mismatch guard; plan.Currency source |
| `internal/service/auth.go` | 2 | resolvePlanForTokenIssuance rewrite (no default fallback) |
| `internal/service/quote.go` | 1 | CycleConfig.TrialDays + Currency from plan |
| `internal/service/errors.go` | 1 | New sentinels |
| `internal/service/*_test.go` | 1+2 | Unit tests per spec §10.1 |
| `internal/handler/app.go` | 1 | Admin plan CRUD validation upgrade; reject is_default |
| `internal/handler/handler_test.go` | 1 | New handler tests |
| `cmd/server/main.go` | 1 | POST /test/login requires ?plan_id |
| `tests/e2e/testhelpers.go` | 1 | Drop is_default update; expand plan seed slice |
| `tests/e2e/extra_test.go` | 1 | Add accepting_new_subscriptions=false to quarterly fixture |
| `tests/integration/integration_test.go` | 1 | Update plan INSERTs |
| `docs/api-integration-guide.md` | 1 | §Plans, §Subscriptions updates |
| `CLAUDE.md` | 1 | Plan-based access paragraph; Endpoints table |

---

## Phase 1 — Additive deploy (migration 012 + service fields)

### Task 1: Migration 012 — additive plan fields

**Files:**
- Create: `migrations/012_plan_commercial_fields.sql`

- [ ] **Step 1: Write the migration file**

Copy the SQL block verbatim from spec §5.1 (`migrations/012_plan_commercial_fields.sql`). Five sections in order: (a) ADD COLUMN 7 new fields with `DEFAULT true / 0 / 'CNY' / now()`; (b) ADD CONSTRAINT `plans_currency_supported CHECK (currency IN ('CNY','USD','EUR'))` and `plans_trial_nonneg CHECK (trial_days >= 0)`; (c) CREATE FUNCTION `plans_touch_updated_at` + CREATE TRIGGER `plans_touch_updated_at BEFORE UPDATE`; (d) UPDATE existing 4 rows (monthly/quarterly/yearly/free) with description / accepting_new_subscriptions / display_order; (e) CREATE TABLE `plan_change_log` with `id BIGSERIAL PK, plan_id TEXT FK plans ON DELETE CASCADE, actor_id TEXT, change_type TEXT CHECK IN ('apps_update','plan_create','plan_update','plan_deactivate','plan_archive'), before JSONB, after JSONB, changed_at TIMESTAMPTZ DEFAULT now()` + CREATE INDEX on `(plan_id, changed_at DESC)`.

- [ ] **Step 2: Verify migration parses**

Run: `grep -c "ALTER TABLE plans" migrations/012_plan_commercial_fields.sql`
Expected: `2` (one ADD COLUMN, one ADD CONSTRAINT)

Run: `grep -c "CREATE TABLE" migrations/012_plan_commercial_fields.sql`
Expected: `1` (just plan_change_log; the trigger uses CREATE TRIGGER not CREATE TABLE)

- [ ] **Step 3: Apply locally**

Run: `make e2e-up` (or whatever brings up the local Postgres used by `tests/integration`; check Makefile target).

Run: `psql $DATABASE_URL -f migrations/012_plan_commercial_fields.sql`
Expected: applies without error; existing 4 plan rows have `currency='CNY'`, `trial_days=0`, non-null `description`.

Verify with: `psql $DATABASE_URL -c "SELECT id, currency, trial_days, display_order, accepting_new_subscriptions FROM plans ORDER BY id"`
Expected: 4 rows; `monthly=true/10/true`, `quarterly=true/20/false`, `yearly=true/30/true`, `free=false/0/false`.

- [ ] **Step 4: Verify trigger fires**

```sql
UPDATE plans SET name = name || '!' WHERE id = 'monthly';
SELECT updated_at FROM plans WHERE id = 'monthly';
```

Expected: `updated_at` is later than the row's previous value (or `now()`).

Revert: `UPDATE plans SET name = REPLACE(name, '!', '') WHERE id = 'monthly';`

- [ ] **Step 5: Commit**

```bash
git add migrations/012_plan_commercial_fields.sql
git commit -m "feat(migration): add 7 commercial fields + plan_change_log"
```

---

### Task 2: Model — extend `model.Plan` + `PublicPlan` + add new errors

**Files:**
- Modify: `internal/model/plan.go`
- Modify: `internal/service/errors.go` (or wherever sentinels live)

- [ ] **Step 1: Add fields to `model.Plan`**

In `internal/model/plan.go`, add to the `Plan` struct (keep existing field order; append new fields before `CreatedAt`):

```go
IsListed                   bool           `db:"is_listed"                    json:"is_listed"`
AcceptingNewSubscriptions  bool           `db:"accepting_new_subscriptions"  json:"accepting_new_subscriptions"`
Currency                   string         `db:"currency"                     json:"currency"`
TrialDays                  int            `db:"trial_days"                   json:"trial_days"`
Description                *string        `db:"description"                  json:"description"`
DisplayOrder               int            `db:"display_order"                json:"display_order"`
UpdatedAt                  time.Time      `db:"updated_at"                   json:"updated_at"`
```

Keep `IsDefault bool` for now — it will be removed in Task 17 (Phase 2). The DB still has the column after 012.

- [ ] **Step 2: Extend `PublicPlan` DTO**

In the same file, modify `PublicPlan` (currently in `plan.go` around line 60):

```go
type PublicPlan struct {
    ID           string             `json:"id"`
    Name         string             `json:"name"`
    Price        float64            `json:"price"`
    IntervalDays int                `json:"interval_days"`
    Currency     string             `json:"currency"`
    TrialDays    int                `json:"trial_days"`
    Description  *string            `json:"description"`
    Apps         []string           `json:"apps"`
    DisplayOrder int                `json:"display_order"`
    ProviderIDs  map[string]string  `json:"provider_ids"`
    Cycle        *CycleSummary      `json:"cycle"`
}
```

`CycleSummary` stays the same shape (`{trial_days, billing_cycle_days}`). `IsActive` stays excluded.

- [ ] **Step 3: Add new error sentinels**

In `internal/service/errors.go`, append:

```go
var (
    ErrPlanNotAcceptingNew      = errors.New("plan is not accepting new subscriptions")
    ErrPlanCurrencyMismatch     = errors.New("plan currency does not match order currency")
    ErrInvalidAppID             = errors.New("plan apps contains unknown or inactive app_id")
    ErrDeprecatedDefaultPlan    = errors.New("default plan concept is deprecated; supply plan_id explicitly")
)
```

Check the existing file uses `errors.New` (not `fmt.Errorf`) for sentinels — match the existing pattern.

- [ ] **Step 4: Verify build**

Run: `make build`
Expected: succeeds. The new fields exist in the struct but no code path uses them yet.

- [ ] **Step 5: Commit**

```bash
git add internal/model/plan.go internal/service/errors.go
git commit -m "feat(model): add 7 plan fields + new commercial DTO"
```

---

### Task 3: Model — add `IsAcceptingNew` to `LoginSubscriptionInfo`

**Files:**
- Modify: `internal/model/auth.go`

- [ ] **Step 1: Locate `LoginSubscriptionInfo` struct**

In `internal/model/auth.go`, find the `LoginSubscriptionInfo` struct. It currently has fields like `PlanID *string`, `PlanName *string`, `HasAccess bool`, `ExpiresAt *time.Time`.

- [ ] **Step 2: Add the new field**

```go
type LoginSubscriptionInfo struct {
    PlanID         *string    `json:"plan_id"`
    PlanName       *string    `json:"plan_name"`
    HasAccess      bool       `json:"has_access"`
    ExpiresAt      *time.Time `json:"expires_at"`
    IsAcceptingNew bool       `json:"is_accepting_new"`
}
```

- [ ] **Step 3: Verify build**

Run: `make build`
Expected: succeeds. The new field is just added — no code sets it yet.

- [ ] **Step 4: Commit**

```bash
git add internal/model/auth.go
git commit -m "feat(model): add is_accepting_new to LoginSubscriptionInfo"
```

---

### Task 4: Repo — extend PlanRepo SQL for new columns + ORDER BY change

**Files:**
- Modify: `internal/repo/repo.go` (`PlanRepo` Create / Update / FindByApp)

- [ ] **Step 1: Write the failing test**

In `internal/repo/repo_test.go` (or whichever file contains `TestPlanRepo_*`):

```go
func TestPlanRepo_FindByApp_SortsByDisplayOrder(t *testing.T) {
    // assume helper `setupTestDB(t)` and seeded plans with display_order 30/10/20
    plans, err := repo.FindByApp(ctx, "yundian")
    require.NoError(t, err)
    require.Len(t, plans, 3)
    require.Equal(t, "monthly", plans[0].ID)   // display_order=10
    require.Equal(t, "quarterly", plans[1].ID) // display_order=20
    require.Equal(t, "yearly", plans[2].ID)    // display_order=30
}
```

Run: `go test -race -run TestPlanRepo_FindByApp_SortsByDisplayOrder ./internal/repo/`
Expected: FAIL — current ORDER BY is `created_at, id`.

- [ ] **Step 2: Update `FindByApp` SQL**

In `internal/repo/repo.go`, change the `FindByApp` SQL to:

```go
SELECT * FROM plans WHERE $1 = ANY(apps) AND is_active = true ORDER BY display_order ASC, created_at ASC, id ASC
```

- [ ] **Step 3: Update `Create` SQL columns**

Update the INSERT column list to include all 12 columns (drop nothing; just add the 7 new ones):

```go
INSERT INTO plans (id, name, price, interval_days, apps, is_active, is_default,
                   is_listed, accepting_new_subscriptions, currency, trial_days,
                   description, display_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
```

Use `sqlx.NamedExec` or a positional args wrapper. Match the existing repo style (likely positional `$N`).

- [ ] **Step 4: Update `Update` SQL**

Update the UPDATE SET clause to set all 12 settable columns. Keep `id = id` style (or pass `id` as a separate arg) to mirror the existing pattern.

- [ ] **Step 5: Verify build + tests**

Run: `make build && go test -race ./internal/repo/`
Expected: build succeeds; all repo tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/repo/repo.go internal/repo/repo_test.go
git commit -m "feat(repo): write all plan fields + sort by display_order"
```

---

### Task 5: PlanService — `ValidateApps` helper

**Files:**
- Modify: `internal/service/plan.go`
- Modify: `internal/service/plan_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestPlanService_ValidateApps_UnknownApp(t *testing.T) {
    mockApp := &mockAppRepo{
        findByID: func(_ context.Context, id string) (*model.App, error) {
            if id == "missing" {
                return nil, sql.ErrNoRows
            }
            return &model.App{ID: id, IsActive: true}, nil
        },
    }
    svc := NewPlanService(mockRepo, mockApp)
    err := svc.ValidateApps(ctx, []string{"yundian", "missing"})
    require.ErrorIs(t, err, ErrInvalidAppID)
}

func TestPlanService_ValidateApps_InactiveApp(t *testing.T) {
    mockApp := &mockAppRepo{
        findByID: func(_ context.Context, id string) (*model.App, error) {
            return &model.App{ID: id, IsActive: id == "yundian"}, nil
        },
    }
    svc := NewPlanService(mockRepo, mockApp)
    err := svc.ValidateApps(ctx, []string{"yundian", "yundash"})
    require.ErrorIs(t, err, ErrInvalidAppID)
}

func TestPlanService_ValidateApps_AllValid(t *testing.T) {
    mockApp := &mockAppRepo{
        findByID: func(_ context.Context, id string) (*model.App, error) {
            return &model.App{ID: id, IsActive: true}, nil
        },
    }
    svc := NewPlanService(mockRepo, mockApp)
    require.NoError(t, svc.ValidateApps(ctx, []string{"yundian", "yundash"}))
}
```

Check `mockAppRepo` shape — match existing `mockPlanRepo` pattern in `plan_test.go` (function fields for each method).

Run: `go test -race -run TestPlanService_ValidateApps ./internal/service/`
Expected: FAIL — `ValidateApps` not defined.

- [ ] **Step 2: Implement `ValidateApps`**

In `internal/service/plan.go`, add to `PlanService`:

```go
func (s *PlanService) ValidateApps(ctx context.Context, apps []string) error {
    for _, id := range apps {
        app, err := s.appRepo.FindByID(ctx, id)
        if err != nil {
            if errors.Is(err, sql.ErrNoRows) {
                return fmt.Errorf("%w: %s", ErrInvalidAppID, id)
            }
            return err
        }
        if !app.IsActive {
            return fmt.Errorf("%w: %s is inactive", ErrInvalidAppID, id)
        }
    }
    return nil
}
```

If `appRepo` is not yet a dependency of `PlanService`, inject it. Check `NewPlanService` constructor signature in `plan.go` — likely takes `(planRepo, ...)` and you add `appRepo` as a new parameter. Update the constructor and any call sites in `cmd/server/main.go`.

- [ ] **Step 3: Verify tests pass**

Run: `go test -race -run TestPlanService_ValidateApps ./internal/service/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service/plan.go internal/service/plan_test.go cmd/server/main.go
git commit -m "feat(plan): ValidateApps rejects unknown/inactive app_id"
```

---

### Task 6: PlanService — audit logging on Create / Update / Delete

**Files:**
- Modify: `internal/service/plan.go`
- Modify: `internal/repo/repo.go` (new `PlanChangeLogRepo` interface or `ChangeLogRepo`)
- Modify: `internal/service/plan_test.go`

- [ ] **Step 1: Define the change-log interface**

In `internal/repo/repo.go`, add:

```go
type PlanChangeLogRepo interface {
    Insert(ctx context.Context, planID, actorID, changeType string, before, after *model.Plan) error
}
```

`before` and `after` are nullable (`*model.Plan`); the repo marshals to JSONB internally. Implement the method in the same file (look up the existing pattern for `audit_log` if there's a similar repo).

- [ ] **Step 2: Write failing test**

In `internal/service/plan_test.go`:

```go
func TestPlanService_UpdatePlan_WritesChangeLog(t *testing.T) {
    mockChangeLog := &mockPlanChangeLogRepo{}
    svc := NewPlanService(mockRepo, mockApp, mockChangeLog)

    _, err := svc.UpdatePlan(ctx, "monthly", &model.PlanPatch{
        Apps: &[]string{"yundian", "yundash", "newapp"},
    })
    require.NoError(t, err)

    require.Equal(t, 1, mockChangeLog.calls)
    require.Equal(t, "monthly", mockChangeLog.lastPlanID)
    require.Equal(t, "apps_update", mockChangeLog.lastChangeType) // or "plan_update" — pick one
    require.NotNil(t, mockChangeLog.lastBefore)
    require.NotNil(t, mockChangeLog.lastAfter)
}
```

(The change_type can be a single `'plan_update'` that records both apps and other field changes — simpler than distinguishing `'apps_update'`. Pick one; update the migration CHECK constraint if you go with a single value.)

Run: `go test -race -run TestPlanService_UpdatePlan_WritesChangeLog ./internal/service/`
Expected: FAIL — change log not wired.

- [ ] **Step 3: Wire `PlanChangeLogRepo` into `PlanService`**

Update `NewPlanService` signature. In `UpdatePlan`:
1. Read current plan (`planRepo.FindByID`).
2. Apply patch.
3. Call `ValidateApps` if `apps` changed.
4. Persist update.
5. Call `changeLogRepo.Insert(ctx, planID, "admin:<appID>", "plan_update", beforeSnapshot, afterSnapshot)`.

`actorID` is read from a request context (see how the existing handler threads it — likely `ctx` value `"actor_id"`). If the existing handler doesn't thread actor_id, use a placeholder `"admin"` for Phase 1 and improve in a follow-up.

- [ ] **Step 4: Mirror in Create and Delete**

- `CreatePlan`: write change log with `before=nil, after=createdPlan, change_type="plan_create"`.
- `DeletePlan`: write change log with `before=planSnapshot, after=nil, change_type="plan_archive"`.

- [ ] **Step 5: Update mock + call sites**

The `PlanChangeLogRepo` needs a real implementation (sqlx) for production. Add the constructor call in `cmd/server/main.go`. Update the PlanService constructor invocation everywhere.

- [ ] **Step 6: Verify build + tests**

Run: `make build && go test -race ./internal/service/`
Expected: build succeeds; existing tests still pass; new test passes.

- [ ] **Step 7: Commit**

```bash
git add internal/repo/repo.go internal/service/plan.go internal/service/plan_test.go cmd/server/main.go
git commit -m "feat(plan): audit log on plan create/update/delete"
```

---

### Task 7: SubscriptionService — `accepting_new_subscriptions` guard

**Files:**
- Modify: `internal/service/subscription.go`
- Modify: `internal/service/subscription_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestSubscriptionService_Create_RejectsNotAcceptingNew(t *testing.T) {
    mockPlanRepo := &mockPlanRepo{
        findByID: func(_ context.Context, id string) (*model.Plan, error) {
            return &model.Plan{ID: id, IsActive: true, AcceptingNewSubscriptions: false, Price: 0}, nil
        },
    }
    svc := NewSubscriptionService(mockPlanRepo, mockSubRepo)
    _, err := svc.Create(ctx, userID, "quarterly")
    require.ErrorIs(t, err, ErrPlanNotAcceptingNew)
}

func TestSubscriptionService_Create_RejectsPaidSelfSubscribe(t *testing.T) {
    // preserved guard
    mockPlanRepo := &mockPlanRepo{
        findByID: func(_ context.Context, id string) (*model.Plan, error) {
            return &model.Plan{ID: id, IsActive: true, AcceptingNewSubscriptions: true, Price: 29.9}, nil
        },
    }
    svc := NewSubscriptionService(mockPlanRepo, mockSubRepo)
    _, err := svc.Create(ctx, userID, "monthly")
    require.ErrorIs(t, err, ErrPaidPlanForbidden)
}
```

Run: `go test -race -run TestSubscriptionService_Create_ ./internal/service/`
Expected: first test FAILS (guard missing); second test PASSES (existing guard).

- [ ] **Step 2: Add the guard in `SubscriptionService.Create`**

In `internal/service/subscription.go`, in `Create`, after `plan.IsActive` check:

```go
if !plan.AcceptingNewSubscriptions {
    return nil, ErrPlanNotAcceptingNew
}
```

- [ ] **Step 3: Verify tests pass**

Run: `go test -race -run TestSubscriptionService_Create_ ./internal/service/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service/subscription.go internal/service/subscription_test.go
git commit -m "feat(sub): reject self-subscribe on plans not accepting new"
```

---

### Task 8: PaymentService — accepting_new + currency guard + currency source

**Files:**
- Modify: `internal/service/payment.go`
- Modify: `internal/service/payment_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestPaymentService_CreateOrder_RejectsNotAcceptingNew(t *testing.T) {
    // setup mock plan with AcceptingNewSubscriptions=false
    // expect ErrPlanNotAcceptingNew
}

func TestPaymentService_CreateOrder_RejectsCurrencyMismatch(t *testing.T) {
    // setup mock plan with Currency="CNY" and order currency="USD"
    // expect ErrPlanCurrencyMismatch
}

func TestPaymentService_CreateOrder_ReadsCurrencyFromPlan(t *testing.T) {
    // setup mock plan with Currency="USD"
    // assert created order.Currency == "USD" (not the hardcoded "CNY")
}
```

Run: `go test -race -run TestPaymentService_CreateOrder_ ./internal/service/`
Expected: all FAIL.

- [ ] **Step 2: Replace hardcoded `"CNY"` with `plan.Currency`**

In `internal/service/payment.go` around line 222, find the literal `"CNY"` and replace:

```go
Currency: plan.Currency,
```

(Was probably `Currency: "CNY"`.)

- [ ] **Step 3: Add guards in `CreateOrder`**

After the existing `plan.IsActive` check:

```go
if !plan.AcceptingNewSubscriptions {
    return nil, ErrPlanNotAcceptingNew
}
if plan.Currency != orderCurrency {
    return nil, ErrPlanCurrencyMismatch
}
```

`orderCurrency` should be derived from `plan.Currency` (single source of truth). Find how `orderCurrency` is currently set — it's likely a constant `"CNY"` or derived from the channel. Update accordingly. If channel ever overrides (e.g., PayPal sandbox uses USD), add a `currency` field on the channel.

- [ ] **Step 4: Verify tests pass**

Run: `go test -race -run TestPaymentService_CreateOrder_ ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/payment.go internal/service/payment_test.go
git commit -m "feat(pay): reject not-accepting-new + currency mismatch + plan-currency source"
```

---

### Task 9: QuoteService — trial + currency from plan

**Files:**
- Modify: `internal/service/quote.go`
- Modify: `internal/service/quote_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestQuoteService_Get_PlanTrialDaysOverride(t *testing.T) {
    // plan with TrialDays=14, BillingCycleDays=30
    // expect Quote.CycleConfig.TrialDays == 14 (not AppConfig.Paypal.Plans[id].TrialDays=7)
}

func TestQuoteService_Get_CurrencyFromPlan(t *testing.T) {
    // plan with Currency="USD"
    // expect Quote.Currency == "USD" (not hardcoded "USD")
}
```

Run: `go test -race -run TestQuoteService_Get_ ./internal/service/`
Expected: FAIL — currency is hardcoded; trial comes from AppConfig.

- [ ] **Step 2: Replace hardcoded `"USD"` with `plan.Currency`**

In `internal/service/quote.go` around line 82, change:

```go
Currency: plan.Currency,
```

- [ ] **Step 3: Replace AppConfig trial source with plan.TrialDays**

In `internal/service/quote.go` around line 35 (the `CycleConfig` builder):

```go
cycle := model.CycleConfig{
    TrialDays:        plan.TrialDays,
    BillingCycleDays: plan.IntervalDays,
}
```

Stop reading `appConfig.PaymentProviders.Paypal.Plans[id].TrialDays`. If `plan.TrialDays == 0`, leave it 0 — no fallback.

- [ ] **Step 4: Verify tests pass**

Run: `go test -race -run TestQuoteService_Get_ ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/quote.go internal/service/quote_test.go
git commit -m "feat(quote): trial + currency from plan; drop AppConfig override"
```

---

### Task 10: Handler — admin plan CRUD validation upgrade

**Files:**
- Modify: `internal/handler/app.go` (`POST /admin/plans`, `PATCH /admin/plans/:id`)
- Modify: `internal/handler/handler_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestPlanHandler_Create_RejectsIsDefault(t *testing.T) {
    body := `{"id":"x","name":"x","is_default":true}`
    // POST /admin/plans
    // expect 400 with message containing "is_default is no longer supported"
}

func TestPlanHandler_Create_RejectsNegativePrice(t *testing.T) {
    body := `{"id":"x","name":"x","price":-1}`
    // expect 400
}

func TestPlanHandler_Create_RejectsBadCurrency(t *testing.T) {
    body := `{"id":"x","name":"x","currency":"JPY"}`
    // expect 400
}

func TestPlanHandler_Create_AcceptsNewFields(t *testing.T) {
    body := `{"id":"q1","name":"Q1","price":99,"interval_days":30,"apps":["yundian"],
              "is_listed":true,"accepting_new_subscriptions":true,"currency":"CNY",
              "trial_days":7,"description":"Quarterly Pro","display_order":15}`
    // expect 201; GET returns the same fields
}
```

Run: `go test -race -run TestPlanHandler_ ./internal/handler/`
Expected: FAIL.

- [ ] **Step 2: Reject `is_default` field**

In `internal/handler/app.go`, in the admin plan create handler (around line 575), after decoding the body:

```go
if _, present := raw["is_default"]; present {
    return echo.NewHTTPError(http.StatusBadRequest, "is_default is no longer supported; use plan selection logic in BFF")
}
```

Use the `json.RawMessage` decode pattern (decode to `map[string]json.RawMessage` first, check for `"is_default"`, then re-decode to struct) — or define a `CreatePlanRequest` struct that explicitly omits `is_default` and rejects requests where the raw body contains the field.

- [ ] **Step 3: Add field validation**

In the same handler, after parsing the struct:

```go
if req.Price < 0 { return echo.NewHTTPError(http.StatusBadRequest, "price must be non-negative") }
if req.IntervalDays < 0 { return echo.NewHTTPError(http.StatusBadRequest, "interval_days must be non-negative") }
if req.TrialDays < 0 { return echo.NewHTTPError(http.StatusBadRequest, "trial_days must be non-negative") }
if req.Currency != "" {
    switch req.Currency {
    case "CNY", "USD", "EUR":
    default:
        return echo.NewHTTPError(http.StatusBadRequest, "currency must be one of CNY/USD/EUR")
    }
}
```

- [ ] **Step 4: Mirror in PATCH handler**

The PATCH handler (around line 638) repeats the same checks. Apply the same logic to the PATCH request DTO.

- [ ] **Step 5: Verify tests pass**

Run: `go test -race -run TestPlanHandler_ ./internal/handler/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/app.go internal/handler/handler_test.go
git commit -m "feat(handler): admin plan CRUD validates new fields, rejects is_default"
```

---

### Task 11: cmd/server — `POST /test/login` requires `?plan_id`

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Locate the test-login handler**

Find the handler that responds to `POST /test/login` (dev-only, gated by `PAYPAL_L3_E2E_MODE=1`).

- [ ] **Step 2: Read `plan_id` from query, validate**

```go
planID := c.QueryParam("plan_id")
if planID == "" {
    return c.JSON(http.StatusBadRequest, map[string]string{"message": "plan_id is required"})
}
plan, err := planRepo.FindByID(ctx, planID)
if errors.Is(err, sql.ErrNoRows) {
    return c.JSON(http.StatusNotFound, map[string]string{"message": "plan not found"})
}
if err != nil { return c.JSON(http.StatusInternalServerError, ...) }
if !plan.IsActive || !plan.AcceptingNewSubscriptions {
    return c.JSON(http.StatusBadRequest, map[string]string{"message": "plan is not available for test login"})
}
```

Remove the previous default-plan lookup. Use `plan` for the rest of the test-login flow (set `chosenPlan = plan` for the JWT issuance path).

- [ ] **Step 3: Update e2e tests calling `/test/login`**

In `tests/integration/integration_test.go` and `tests/e2e/e2e_test.go`, find all `POST /test/login` calls and add `?plan_id=monthly` (or `?plan_id=free` for legacy tests — those need to migrate to `monthly`).

- [ ] **Step 4: Verify build + integration tests**

Run: `make build && make test`
Expected: succeeds; integration tests pass with `?plan_id` query.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/main.go tests/integration/integration_test.go tests/e2e/e2e_test.go
git commit -m "feat(test-login): require ?plan_id query; remove default-plan fallback"
```

---

### Task 12: Test fixtures — update seed for new fields

**Files:**
- Modify: `tests/e2e/testhelpers.go`
- Modify: `tests/e2e/extra_test.go`
- Modify: `tests/integration/integration_test.go`

- [ ] **Step 1: Drop `is_default` from testhelpers seed**

In `tests/e2e/testhelpers.go` around line 121, remove the `UPDATE plans SET is_default = false` line and the `isDef bool` field. Drop the `is_default` column from the `INSERT INTO plans` statement.

- [ ] **Step 2: Add new fields to testhelpers seed**

Extend the plan seed slice to include:

```go
{"monthly", "按月订阅", 29.9, 30, "{yundian,yundash}", true, true, "CNY", 0, "按月订阅...", 10},
```

(match the spec's data backfill defaults).

- [ ] **Step 3: Update `extra_test.go` quarterly fixture**

In `tests/e2e/extra_test.go` around line 399, extend the `quarterly` INSERT to include `accepting_new_subscriptions=false`.

- [ ] **Step 4: Update `integration_test.go` plan INSERTs**

Same as Step 2 — extend INSERTs at lines 78 and 561.

- [ ] **Step 5: Verify**

Run: `make e2e && make test`
Expected: all tests green.

- [ ] **Step 6: Commit**

```bash
git add tests/e2e/testhelpers.go tests/e2e/extra_test.go tests/integration/integration_test.go
git commit -m "test: update fixtures for plan commercial fields"
```

---

### Task 13: AuthService — `is_accepting_new` on LoginSubscriptionInfo

**Files:**
- Modify: `internal/service/auth.go`
- Modify: `internal/service/auth_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestAuthService_IssueTokensForUser_IsAcceptingNew(t *testing.T) {
    // sub with plan.IsActive=true, AcceptingNewSubscriptions=true → IsAcceptingNew=true
    // sub with plan.IsActive=false → IsAcceptingNew=false
    // no sub → IsAcceptingNew=false
}
```

Run: `go test -race -run TestAuthService_IssueTokensForUser_IsAcceptingNew ./internal/service/`
Expected: FAIL — field not set.

- [ ] **Step 2: Set the field in `issueTokensForUser`**

In `internal/service/auth.go`, find the place where `LoginSubscriptionInfo` is populated. After setting `HasAccess`, add:

```go
isAcceptingNew := chosenPlan != nil && chosenPlan.IsActive && chosenPlan.AcceptingNewSubscriptions
subInfo := model.LoginSubscriptionInfo{
    // ... existing fields ...
    IsAcceptingNew: isAcceptingNew,
}
```

(Don't rewrite `resolvePlanForTokenIssuance` yet — that's Task 17 in Phase 2. Phase 1 only sets the new field; behavior for the default-plan fallback stays as-is.)

- [ ] **Step 3: Verify**

Run: `go test -race -run TestAuthService_ ./internal/service/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service/auth.go internal/service/auth.go
git commit -m "feat(auth): LoginResponse includes is_accepting_new"
```

---

### Task 14: E2E tests — Phase 1 subset

**Files:**
- Create: `tests/e2e/plan_commercial_test.go`

- [ ] **Step 1: Write `TestE2E_PlanCommercial_CreateWithNewFields`**

End-to-end test that POSTs a plan with all 7 new fields, then GETs it and asserts each field round-trips.

- [ ] **Step 2: Write `TestE2E_PlanCommercial_AppsValidation`**

POST a plan with `apps=["nonexistent"]` — assert 400 with the validation message.

- [ ] **Step 3: Write `TestE2E_PlanCommercial_QuarterlyNotAcceptingNew`**

POST `/user/subscriptions` with `quarterly` (after fixture sets `accepting_new_subscriptions=false`) — assert 409.

- [ ] **Step 4: Write `TestE2E_PlanCommercial_OrderCurrencyMismatch`**

Set plan currency to CNY but attempt to create an order with channel that forces USD — assert 400.

- [ ] **Step 5: Run E2E**

Run: `make e2e`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add tests/e2e/plan_commercial_test.go
git commit -m "test(e2e): plan commercial fields + validation guards"
```

---

### Task 15: Docs updates

**Files:**
- Modify: `docs/api-integration-guide.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update `docs/api-integration-guide.md` §Plans**

Replace the "Plan object" example JSON to include the 7 new fields. Note the new validation: `currency IN (CNY/USD/EUR)`, `trial_days >= 0`, `is_default` is rejected.

- [ ] **Step 2: Update `docs/api-integration-guide.md` §Subscriptions**

Add the `is_accepting_new` field on `LoginResponse.subscription`. Add the new error codes (`ErrPlanNotAcceptingNew`, `ErrPlanCurrencyMismatch`, `ErrInvalidAppID`).

- [ ] **Step 3: Update `CLAUDE.md`**

Replace the "Plan-based access" paragraph with the updated decision matrix from spec §8.1. Add `POST /test/login` requiring `?plan_id`.

- [ ] **Step 4: Commit**

```bash
git add docs/api-integration-guide.md CLAUDE.md
git commit -m "docs: plan commercial surface + LoginResponse.is_accepting_new"
```

---

### Task 16: Deploy Phase 1

**Files:** none

- [ ] **Step 1: Apply migration 012 to staging**

Run: `psql $STAGING_DATABASE_URL -f migrations/012_plan_commercial_fields.sql`
Expected: applies without error.

- [ ] **Step 2: Deploy service to staging**

Run the project's normal deploy pipeline. Service now reads/writes the 7 new fields; no behavior change for default-plan fallback (still present until Phase 2).

- [ ] **Step 3: Smoke test**

Verify on staging:
- Admin POST `/admin/plans` accepts new fields.
- Admin POST `/admin/plans` with `is_default:true` returns 400.
- `GET /apps/:id/plans` returns plans sorted by `display_order`.
- Login response includes `subscription.is_accepting_new`.

- [ ] **Step 4: Tag release**

```bash
git tag v0.X.Y-phase1
```

- [ ] **Step 5: Deploy to production**

Same steps on production. The default-plan fallback is still in effect; Phase 1 is fully backwards compatible.

---

## Phase 2 — Behavioral switch (migration 013 + default-plan removal)

### Task 17: Migration 013 — drop `is_default` + retire `free`

**Files:**
- Create: `migrations/013_remove_default_plan.sql`

- [ ] **Step 1: Write the migration file**

Copy from spec §5.2:

```sql
DO $$
DECLARE cnt INT;
BEGIN
    SELECT COUNT(*) INTO cnt FROM subscriptions WHERE plan_id = 'free' AND status = 'active';
    IF cnt > 0 THEN
        RAISE EXCEPTION 'migration aborted: % active subscriptions still reference plan_id=free; cancel them first', cnt;
    END IF;
END $$;

UPDATE plans SET is_active = false, accepting_new_subscriptions = false WHERE id = 'free';
DROP INDEX IF EXISTS plans_one_default;
ALTER TABLE plans DROP COLUMN IF EXISTS is_default;
```

- [ ] **Step 2: Verify pre-check locally**

With Phase 1 deployed, run:
```sql
SELECT COUNT(*) FROM subscriptions WHERE plan_id = 'free' AND status = 'active';
```

Expected: 0.

If non-zero: cancel those subscriptions first (`UPDATE subscriptions SET status='cancelled' WHERE ...`), then re-run.

- [ ] **Step 3: Apply locally**

Run: `psql $DATABASE_URL -f migrations/013_remove_default_plan.sql`
Expected: applies without error.

Verify:
```sql
SELECT id, is_default FROM plans; -- should fail because column is gone
SELECT id, is_active FROM plans WHERE id = 'free'; -- is_active=false
```

- [ ] **Step 4: Commit**

```bash
git add migrations/013_remove_default_plan.sql
git commit -m "feat(migration): drop is_default + retire free"
```

---

### Task 18: Model — remove `IsDefault` field

**Files:**
- Modify: `internal/model/plan.go`
- Modify: `internal/service/*` (any code reading `model.Plan.IsDefault`)

- [ ] **Step 1: Find all references**

Run: `grep -rn "IsDefault\|is_default" internal/`
Expected: only references that are about to be deleted.

- [ ] **Step 2: Remove the field**

In `internal/model/plan.go`, delete:

```go
IsDefault bool `db:"is_default" json:"is_default"`
```

- [ ] **Step 3: Fix compile errors**

The migration in Task 17 already dropped the column. Build will fail until all references are removed. Touch each call site:
- If a service sets `plan.IsDefault = true` on insert: drop the assignment.
- If a test reads `plan.IsDefault`: delete the assertion or update it.
- If `repo.FindDefault` reads `WHERE is_default = true LIMIT 1`: keep the method but update body (next task).

- [ ] **Step 4: Verify build**

Run: `make build`
Expected: succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/model/plan.go internal/...
git commit -m "refactor(model): drop IsDefault field"
```

---

### Task 19: AuthService — `resolvePlanForTokenIssuance` rewrite (no default fallback)

**Files:**
- Modify: `internal/service/auth.go`
- Modify: `internal/service/auth_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestResolvePlanForTokenIssuance_NoSub_ReturnsNilChosenPlan(t *testing.T) {
    // peekSubscription returns (nil, false, nil)
    // chosenPlan = nil, hasAccess = false
}

func TestResolvePlanForTokenIssuance_ExpiredSub_ScopeEmpty(t *testing.T) {
    // peekSubscription returns (sub, true, nil)
    // chosenPlan = sub.plan, hasAccess = false
    // AND signAccessToken is called with empty scope (verify via token claims)
}

func TestResolvePlanForTokenIssuance_ActiveSub_PlanDeactivated(t *testing.T) {
    // peekSubscription returns (sub, false, nil)
    // plan.IsActive = false → hasAccess = false
}

func TestIssueTokensForUser_NoSub_SignWithEmptyScope(t *testing.T) {
    // issueTokensForUser with no sub → JWT.scope = []
}
```

Run: `go test -race -run TestResolvePlanForTokenIssuance_ ./internal/service/`
Expected: FAIL — current behavior includes default-plan fallback.

- [ ] **Step 2: Rewrite `resolvePlanForTokenIssuance`**

In `internal/service/auth.go`, replace the body:

```go
func (s *AuthService) resolvePlanForTokenIssuance(ctx context.Context, user *model.User, appID string) (*model.Plan, bool, error) {
    sub, expired, err := s.peekSubscription(ctx, user.ID)
    if err != nil {
        return nil, false, err
    }

    switch {
    case sub == nil:
        return nil, false, nil
    case expired:
        // Scope is forced to [] for security; chosenPlan preserved for the
        // LoginResponse.subscription.plan_id renewal CTA.
        return sub.Plan, false, nil
    default:
        // Active sub. Deactivated plans (P1 fix) force hasAccess=false.
        hasAccess := sub.Plan.IsActive && containsApp(sub.Plan.Apps, appID)
        return sub.Plan, hasAccess, nil
    }
}
```

Remove the `defaultPlan, _ := s.planRepo.FindDefault(ctx)` line. Remove the `defaultPlan`-related branches from the previous switch.

- [ ] **Step 3: Update `issueTokensForUser`**

When `chosenPlan` is `nil`, sign the access token with `scope = []`. The existing code likely does `tokenSvc.SignAccessToken(userID, appID, chosenPlan.Apps)`. Change to handle the nil case:

```go
scope := []string{}
if chosenPlan != nil {
    scope = chosenPlan.Apps
}
accessToken, err := s.tokenSvc.SignAccessToken(user.ID, appID, scope)
```

- [ ] **Step 4: Verify tests pass**

Run: `go test -race -run TestResolvePlanForTokenIssuance_ ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/auth.go internal/service/auth_test.go
git commit -m "feat(auth): resolvePlanForTokenIssuance three-state without default fallback"
```

---

### Task 20: PlanService — `FindDefault` deprecated

**Files:**
- Modify: `internal/service/plan.go`

- [ ] **Step 1: Locate `FindDefault`**

In `internal/service/plan.go`, find `FindDefault` (or whatever the default-plan-lookup method is named).

- [ ] **Step 2: Update method body**

```go
// FindDefault is deprecated: the default plan concept is removed (migration 013).
// It now returns ErrDeprecatedDefaultPlan. Callers must supply an explicit plan_id.
func (s *PlanService) FindDefault(ctx context.Context) (*model.Plan, error) {
    return nil, ErrDeprecatedDefaultPlan
}
```

- [ ] **Step 3: Search for call sites**

Run: `grep -rn "FindDefault\|\.FindDefault(" internal/ cmd/`
Expected: zero remaining call sites (Phase 1's `resolvePlanForTokenIssuance` no longer calls it after Task 19; `cmd/server/main.go` test-login uses explicit plan_id after Task 11).

If any remain, replace them with explicit `FindByID(planID)`.

- [ ] **Step 4: Verify build + tests**

Run: `make build && make test`
Expected: green.

- [ ] **Step 5: Commit**

```bash
git add internal/service/plan.go internal/...
git commit -m "refactor(plan): FindDefault returns ErrDeprecatedDefaultPlan"
```

---

### Task 21: E2E tests — Phase 2 subset

**Files:**
- Create: `tests/e2e/plan_commercial_phase2_test.go`

- [ ] **Step 1: Write `TestE2E_LoginNoSubscription_HasAccessFalse`**

Login a user with no subscription. Assert response has `subscription.plan_id=null`, `has_access=false`, and decode the JWT to assert `scope=[]`.

- [ ] **Step 2: Write `TestE2E_LoginExpiredSubscription_PreservesPlanID`**

Seed an expired sub. Login. Assert response has `subscription.plan_id="quarterly"` (or whatever the historical plan is), `has_access=false`, JWT scope=`[]`.

- [ ] **Step 3: Run E2E**

Run: `make e2e`
Expected: green.

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/plan_commercial_phase2_test.go
git commit -m "test(e2e): retired free + no-default token behavior"
```

---

### Task 22: Deploy Phase 2

**Files:** none

- [ ] **Step 1: Pre-deploy: pre-check active free subs**

Run: `psql $PROD_DATABASE_URL -c "SELECT COUNT(*) FROM subscriptions WHERE plan_id = 'free' AND status = 'active'"`

Expected: 0. If non-zero, cancel those rows first.

- [ ] **Step 2: Coordinate deploy**

Apply migration 013 and deploy the new service **in the same maintenance window**. Migration 013 drops `is_default`; if the service is still on the old code referencing `model.Plan.IsDefault`, the build was already broken — so this is a coordinated atomic step.

Run:
```bash
psql $PROD_DATABASE_URL -f migrations/013_remove_default_plan.sql
# then trigger service deploy
```

- [ ] **Step 3: Smoke test**

Verify on production:
- Login without subscription: response has `subscription.plan_id=null`, `has_access=false`. JWT decodes to `scope=[]`.
- Login with expired sub: response has `subscription.plan_id="<historical>"`, `has_access=false`, JWT `scope=[]`.
- Login with active sub: response has `subscription.plan_id=<active>`, `has_access=true` (if apps ⊇ appID).
- `POST /admin/plans` with `is_default:true` → 400.
- `GET /admin/apps` and `GET /apps/:id/plans` work.

- [ ] **Step 4: Tag release**

```bash
git tag v0.X.Y-phase2
```

---

## Self-review checklist

**Spec coverage:**

| Spec section | Task |
|---|---|
| §2.1 Add 7 columns | T1, T2, T3 |
| §2.2 Delete is_default + retire free | T17, T18 |
| §2.3 resolvePlanForTokenIssuance change | T19 |
| §2.4 expired-sub scope=[] + LoginResponse.plan_id preserved | T19 |
| §2.5 plan_change_log | T6 |
| §2.6 currency source-of-truth | T8, T9 |
| §2.7 trial source-of-truth | T9 |
| §2.8 apps validation | T5 |
| §2.9 admin CRUD validation | T10 |
| §5.1 migration 012 | T1 |
| §5.2 migration 013 | T17 |
| §6.1 PlanService | T5, T6, T20 |
| §6.2 SubscriptionService | T7 |
| §6.3 PaymentService | T8 |
| §6.4 AuthService | T13, T19 |
| §6.5 QuoteService | T9 |
| §6.6 POST /test/login | T11 |
| §6.7 new error sentinels | T2 |
| §7.1 admin plan CRUD | T10 |
| §7.2 PublicPlan DTO | T2 |
| §7.3 /apps/:id/plans ordering | T4 |
| §7.4 LoginSubscriptionInfo | T3, T13 |
| §7.5 /apps/:id/quote | T9 |
| §7.6 /payments/orders | T8 |
| §8 decision matrix | T19 |
| §9 data flow | implicit |
| §10.1 unit tests | T5, T6, T7, T8, T9, T10, T13, T19 |
| §10.2 e2e tests | T14, T21 |
| §10.3 test fixtures | T12 |
| §11.3 rollback | implicit (ALTER TABLE DROP COLUMN) |

All spec sections map to at least one task. No gaps.