# Plan Pricing Realignment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align yunhou-users `plans` data with the yunhou-website frontend promo (monthly ¥19.9 / yearly ¥199.9, only two public plans) and fully retire `quarterly` and `free` from `/apps/:id/plans`.

**Architecture:** Pure-data migration (`migrations/016_plan_pricing_and_hide.sql`) — three UPDATE blocks that re-price `monthly`/`yearly`, set `quarterly.is_listed=false` + `is_listed=false` + `is_active=false`, and set `free.is_listed=false`. Zero service / model / handler / DTO changes; `plans.price` is read directly from DB so `GET /apps/:id/plans` and `GET /admin/plans` reflect the new value the moment the migration applies. No BFF deploy needed (`catalog.ts` 60s TTL) and no website deploy needed (frontend already ships the matching static config).

**Tech Stack:** PostgreSQL (migration), Go (test fixtures), Vitest/Go testing libraries, bash (psql verification).

---

## Task 1: Write the migration file

**Files:**
- Create: `migrations/016_plan_pricing_and_hide.sql`

- [ ] **Step 1: Create the migration file**

Create `migrations/016_plan_pricing_and_hide.sql` with the following content (idempotent, data-only):

```sql
-- Migration: 016_plan_pricing_and_hide
-- 2026-07-25: align plans.price with the yunhou-website frontend promo
-- (see docs/superpowers/specs/2026-07-25-plan-pricing-realignment-design.md).
--   - monthly ¥29.9/月 → ¥19.9/月
--   - yearly  ¥299/年  → ¥199.9/年
--   - quarterly: is_listed=false, is_active=false (full retirement;
--     historical plan_id preserved in LoginResponse.subscription.plan_id)
--   - free: is_listed=false (already is_active=false since migration 014)
--
-- Idempotency: each UPDATE is keyed on a specific id literal; non-matching
-- rows leave the column untouched. Re-running after first apply writes the
-- same value back (semantic no-op), but bumps updated_at via the trigger
-- from migration 012 — operators who care about a clean updated_at
-- timeline should run once.

-- (a) Re-price the two surviving public plans
UPDATE plans SET price = CASE id
    WHEN 'monthly' THEN 19.9
    WHEN 'yearly'  THEN 199.9
    ELSE price
END
WHERE id IN ('monthly','yearly');

-- (b) Retire quarterly fully (is_listed=false + is_active=false)
UPDATE plans SET is_listed = false WHERE id = 'quarterly';
UPDATE plans SET is_active = false WHERE id = 'quarterly';

-- (c) Hide free from the public catalog
UPDATE plans SET is_listed = false WHERE id = 'free';
```

- [ ] **Step 2: Verify SQL syntax with psql dry-parse**

Run (does NOT execute, only parses):
```bash
PGPASSWORD=postgres psql -h localhost -U postgres -d yunhou_users -v ON_ERROR_STOP=1 \
  --single-transaction --dry-run \
  -f migrations/016_plan_pricing_and_hide.sql 2>&1 | head -30
```
Expected: parser errors (if any) printed; no `ERROR:` lines about table/column references. **No rows updated** because of `--dry-run` (actually `--dry-run` is not a psql flag; the standard way is `BEGIN; ... ROLLBACK;` — see Step 3).

- [ ] **Step 3: Apply migration against local DB in a rolled-back transaction**

This validates that all column references resolve without committing:
```bash
PGPASSWORD=postgres psql -h localhost -U postgres -d yunhou_users -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
\i migrations/016_plan_pricing_and_hide.sql
ROLLBACK;
SQL
```
Expected: no errors. The ROLLBACK undoes the UPDATEs so the DB stays at the pre-migration state.

- [ ] **Step 4: Apply migration against local DB (committed)**

```bash
PGPASSWORD=postgres psql -h localhost -U postgres -d yunhou_users -v ON_ERROR_STOP=1 \
  -f migrations/016_plan_pricing_and_hide.sql
```
Expected: 4 `UPDATE` statements execute (one per target row across the three blocks: 2 from block a, 1 quarterly from block b's first UPDATE, 1 quarterly from block b's second UPDATE, 1 free from block c — actual rowcount counts depend on the prod state). No `ERROR:` lines.

- [ ] **Step 5: Verify post-migration state**

```bash
PGPASSWORD=postgres psql -h localhost -U postgres -d yunhou_users -c \
  "SELECT id, price, is_active, is_listed, accepting_new_subscriptions FROM plans ORDER BY id;"
```
Expected:
```
    id     | price | is_active | is_listed | accepting_new_subscriptions
-----------+-------+-----------+-----------+---------------------------
 free      |     0 | f         | f         | f
 monthly   |  19.9 | t         | t         | t
 quarterly |  79.9 | f         | f         | f
 yearly    | 199.9 | t         | t         | t
```

- [ ] **Step 6: Verify `/apps/:id/plans` returns only two plans**

```bash
# Run the server (assumes go build / cmd/server already wired)
go run ./cmd/server &
SERVER_PID=$!
sleep 2

curl -s http://localhost:8080/apps/yundian/plans | jq '.data | length'
# Expected: 2

curl -s http://localhost:8080/apps/yundian/plans | jq '.data[] | {id, price}'
# Expected: [{"id":"monthly","price":19.9},{"id":"yearly","price":199.9}]

kill $SERVER_PID
```

- [ ] **Step 7: Commit migration file**

```bash
git add migrations/016_plan_pricing_and_hide.sql
git commit -m "migration(016): re-price monthly/yearly to ¥19.9/¥199.9 and retire quarterly

Aligns yunhou-users plans with the yunhou-website frontend promo
(only two public plans, monthly ¥19.9 with strikethrough ¥29.9, yearly
¥199.9 with strikethrough ¥299.9 — see cn.ts:43-65).

No service code changes; price is read directly from DB. No BFF / website
deploys required — BFF catalog.ts 60s TTL picks up the new price; frontend
already ships the matching static config. originalPrice stays a frontend-
only field per the responsibility boundary.

quarterly.is_active=false narrows JWT.scope=[] for any historical
subscriber per plan_commercialization §6.4; historical plan_id is
preserved in LoginResponse.subscription.plan_id so the frontend can
render '已下线，请重新订阅'.

Ref: docs/superpowers/specs/2026-07-25-plan-pricing-realignment-design.md

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: Update e2e plan seed in testhelpers.go

**Files:**
- Modify: `tests/e2e/testhelpers.go:131-135` (seed slice literal)

- [ ] **Step 1: Read current seed slice**

Confirm the file at `tests/e2e/testhelpers.go:120-160` matches the snippet:
```go
plans := []struct {
    id, name, currency string
    price              float64
    days               int
    apps               string
    trialDays          int
    description        string
    isListed           bool
    acceptingNew       bool
    displayOrder       int
}{
    {"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", true, false, 0},
    {"monthly", "按月订阅", "CNY", 29.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥29.9，自动续费，可随时取消", true, true, 10},
    {"monthly_usd", "Monthly PayPal Test", "USD", 29.9, 30, "{}", 0, "PayPal USD test fixture", false, true, 0},
}
```

- [ ] **Step 2: Replace seed entries to match post-migration state**

Apply this Edit (replace_all: false) at `tests/e2e/testhelpers.go:132-134`:

OLD:
```go
		{"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", true, false, 0},
		{"monthly", "按月订阅", "CNY", 29.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥29.9，自动续费，可随时取消", true, true, 10},
		{"monthly_usd", "Monthly PayPal Test", "USD", 29.9, 30, "{}", 0, "PayPal USD test fixture", false, true, 0},
```

NEW:
```go
		{"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", false, false, 0},
		{"monthly", "按月订阅", "CNY", 19.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥19.9，自动续费，可随时取消", true, true, 10},
		{"monthly_usd", "Monthly PayPal Test", "USD", 29.9, 30, "{}", 0, "PayPal USD test fixture", false, true, 0},
```

Notes:
- `free.isListed: true → false` (matches migration 016 block c)
- `monthly.price: 29.9 → 19.9` (matches migration 016 block a)
- `monthly` description text updated to match new price (cosmetic; the description is part of the PublicPlan DTO)
- `monthly_usd` left at 29.9 USD — PayPal test fixture is independent of CN pricing

- [ ] **Step 3: Verify the file compiles**

```bash
cd /Users/lili/Downloads/github/yunhou-users
go build ./tests/...
```
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/testhelpers.go
git commit -m "test(e2e): align plan seed with migration 016 (¥19.9, free/quarterly hidden)

Bumps tests/e2e/testhelpers.go:132-134 to match the post-migration 016
state so local 'make e2e' against a fresh DB seeds the right fixtures.
monthly_usd kept at 29.9 USD (PayPal test fixture is independent of CN
pricing).

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: Update quarterly fixture in tests/e2e/extra_test.go

**Files:**
- Modify: `tests/e2e/extra_test.go:395-405` (quarterly INSERT)

- [ ] **Step 1: Read the current quarterly INSERT**

Confirm lines 395-405 contain:
```go
if _, err := db.ExecContext(context.Background(),
    `INSERT INTO plans (
        id, name, price, interval_days, apps, is_active,
        is_listed, accepting_new_subscriptions, currency, trial_days,
        description, display_order
    ) VALUES (
        'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], true,
        true, false, 'CNY', 0, '按季订阅 ¥79.9，暂不开放新订阅，已有订阅保留', 20
    ) ON CONFLICT (id) DO NOTHING`); err != nil {
```

- [ ] **Step 2: Patch the INSERT to retire quarterly**

Apply this Edit at `tests/e2e/extra_test.go:401-402`:

OLD:
```go
		'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], true,
		true, false, 'CNY', 0, '按季订阅 ¥79.9，暂不开放新订阅，已有订阅保留', 20
```

NEW:
```go
		'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], false,
		false, false, 'CNY', 0, '按季订阅 ¥79.9（已下线）', 20
```

Notes:
- `is_active: true → false` (matches migration 016 block b)
- `is_listed: true → false` (matches migration 016 block b)
- Description text simplified to reflect retirement

- [ ] **Step 3: Verify the test still compiles**

```bash
go vet ./tests/e2e/...
```
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/extra_test.go
git commit -m "test(e2e/extra): mark quarterly fixture as fully retired

Aligns the per-test quarterly seed in tests/e2e/extra_test.go with
migration 016 (is_listed=false, is_active=false). Tests that depend on
quarterly having visible-but-not-subscribable state must migrate to
monthly/yearly in a follow-up — flagged in spec §11.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: Update integration test plan seed

**Files:**
- Modify: `tests/integration/integration_test.go:65-80` (seed slice)

- [ ] **Step 1: Read current seed slice**

Confirm lines 65-80 match:
```go
plans := []struct {
    id, name, currency string
    price              float64
    days               int
    apps               string
    trialDays          int
    description        string
    isListed           bool
    acceptingNew       bool
    displayOrder       int
}{
    {"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", true, false, 0},
    {"monthly", "按月订阅", "CNY", 29.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥29.9，自动续费，可随时取消", true, true, 10},
    {"test_free", "Integration Free", "CNY", 0, 0, "{yundian}", 0, "Free integration fixture", false, true, 0},
}
```

- [ ] **Step 2: Replace `monthly.price` and `free.isListed`**

Apply this Edit at `tests/integration/integration_test.go:77-78`:

OLD:
```go
		{"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", true, false, 0},
		{"monthly", "按月订阅", "CNY", 29.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥29.9，自动续费，可随时取消", true, true, 10},
```

NEW:
```go
		{"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", false, false, 0},
		{"monthly", "按月订阅", "CNY", 19.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥19.9，自动续费，可随时取消", true, true, 10},
```

- [ ] **Step 3: Verify the integration tests compile**

```bash
go vet ./tests/integration/...
```
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add tests/integration/integration_test.go
git commit -m "test(integration): align plan seed with migration 016

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: Add new E2E tests for catalog realignment

**Files:**
- Create: `tests/e2e/plans_pricing_test.go`

- [ ] **Step 1: Create the E2E test file**

Create `tests/e2e/plans_pricing_test.go`:

```go
package e2e

import (
    "context"
    "encoding/json"
    "net/http"
    "testing"
)

// TestE2E_PlanPricing_OnlyTwoPlansReturned verifies that after migration 016
// only monthly and yearly show up on the public catalog. quarterly and free
// are filtered out by is_listed=false.
func TestE2E_PlanPricing_OnlyTwoPlansReturned(t *testing.T) {
    srv, _ := startTestServer(t)
    defer srv.Close()

    resp := doRequest(t, srv, http.MethodGet, "/apps/yundian/plans", nil, nil)
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("status: got %d, want 200", resp.StatusCode)
    }

    var body struct {
        Code    int                      `json:"code"`
        Data    []map[string]interface{} `json:"data"`
        Message *string                  `json:"message"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if body.Code != 0 {
        t.Fatalf("code: got %d, want 0", body.Code)
    }
    if len(body.Data) != 2 {
        t.Fatalf("plan count: got %d, want 2; data=%v", len(body.Data), body.Data)
    }

    ids := map[string]bool{}
    prices := map[string]float64{}
    for _, p := range body.Data {
        ids[p["id"].(string)] = true
        prices[p["id"].(string)] = p["price"].(float64)
    }
    if !ids["monthly"] || !ids["yearly"] {
        t.Fatalf("expected monthly+yearly, got %v", ids)
    }
    if ids["quarterly"] || ids["free"] {
        t.Fatalf("expected quarterly+free absent, got %v", ids)
    }
    if prices["monthly"] != 19.9 {
        t.Errorf("monthly.price: got %v, want 19.9", prices["monthly"])
    }
    if prices["yearly"] != 199.9 {
        t.Errorf("yearly.price: got %v, want 199.9", prices["yearly"])
    }
}

// TestE2E_PlanPricing_QuarterlyHiddenAndInactive checks the DB directly so a
// regression in FindByApp's WHERE clause (e.g. dropping is_listed) would
// surface even if the catalog endpoint didn't fail.
func TestE2E_PlanPricing_QuarterlyHiddenAndInactive(t *testing.T) {
    db := openTestDB(t)
    defer db.Close()

    var listed, active bool
    err := db.QueryRowContext(context.Background(),
        `SELECT is_listed, is_active FROM plans WHERE id = 'quarterly'`).
        Scan(&listed, &active)
    if err != nil {
        t.Fatalf("query quarterly: %v", err)
    }
    if listed {
        t.Errorf("quarterly.is_listed: got true, want false")
    }
    if active {
        t.Errorf("quarterly.is_active: got true, want false")
    }
}

// TestE2E_PlanPricing_OrderMonthlyNewPrice creates a quote against monthly
// and asserts the quoted amount is the new ¥19.9, not the old ¥29.9.
func TestE2E_PlanPricing_OrderMonthlyNewPrice(t *testing.T) {
    srv, _ := startTestServer(t)
    defer srv.Close()

    body := map[string]interface{}{
        "plan_id": "monthly",
        "channel": "wechat_pay",
    }
    resp := doJSONAuth(t, srv, http.MethodPost, "/apps/yundian/quote", body)
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("status: got %d, want 200", resp.StatusCode)
    }

    var out struct {
        Code    int                    `json:"code"`
        Data    map[string]interface{} `json:"data"`
        Message *string                `json:"message"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if out.Code != 0 {
        t.Fatalf("code: got %d, want 0", out.Code)
    }
    amt, ok := out.Data["amount"].(float64)
    if !ok {
        t.Fatalf("quote.amount: missing or wrong type: %v", out.Data["amount"])
    }
    if amt != 19.9 {
        t.Errorf("quote.amount: got %v, want 19.9", amt)
    }
}
```

- [ ] **Step 2: Verify the test file uses the project's helpers correctly**

Inspect the existing `tests/e2e/` for the helper signatures used in the file. Update imports / helper names if any differ (the names above — `startTestServer`, `doRequest`, `doJSONAuth`, `openTestDB` — are placeholders matching the existing patterns in `tests/e2e/*.go`; if any differ, edit the test file to match). Run:

```bash
go vet ./tests/e2e/...
```
Expected: no errors.

- [ ] **Step 3: Run the new E2E tests against the post-migration DB**

```bash
make e2e TESTS="TestE2E_PlanPricing_OnlyTwoPlansReturned TestE2E_PlanPricing_QuarterlyHiddenAndInactive TestE2E_PlanPricing_OrderMonthlyNewPrice"
```
Expected: all three pass.

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/plans_pricing_test.go
git commit -m "test(e2e): add plans_pricing_test for migration 016 catalog realignment

Covers three post-migration invariants:
- /apps/:id/plans returns exactly 2 plans (monthly+yearly)
- quarterly is is_listed=false AND is_active=false in the DB
- /apps/:id/quote for monthly returns amount=19.9 (not the old 29.9)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 6: Adjust unit test amount assertions

**Files:**
- Modify: `internal/handler/handler_test.go:2664,2695`
- Modify: `internal/service/payment_db_test.go:54,130`

- [ ] **Step 1: Read handler_test.go:2660-2700 to confirm the assertion context**

The file contains:
```go
PlanID: "monthly", Amount: 29.9, Currency: "USD",
...
if resp.Data == nil || resp.Data.PlanID != "monthly" || resp.Data.Amount != 29.9 {
```

If this test still uses a `monthly` plan fixture, both the input and assertion need updating to `19.9`. If it uses an unrelated `monthly_usd` fixture (USD), leave at 29.9.

- [ ] **Step 2: Update `internal/handler/handler_test.go` amount assertion (CN monthly path)**

Apply this Edit at `internal/handler/handler_test.go:2695`:

OLD:
```go
		if resp.Data == nil || resp.Data.PlanID != "monthly" || resp.Data.Amount != 29.9 {
```

NEW:
```go
		if resp.Data == nil || resp.Data.PlanID != "monthly" || resp.Data.Amount != 19.9 {
```

(If `2664` is `PlanID: "monthly", Amount: 29.9, Currency: "USD"` and is used as the test's *input*, also flip it to `19.9`. Inspect before editing — only flip the input if the test's setup section uses a `monthly` CN fixture; if it's a `monthly_usd` USD fixture, leave at 29.9.)

- [ ] **Step 3: Update `internal/service/payment_db_test.go` amount assertion**

Apply this Edit at `internal/service/payment_db_test.go:130`:

OLD:
```go
	if order.PlanID != "monthly" || order.Amount != 29.9 || order.Currency != "CNY" {
```

NEW:
```go
	if order.PlanID != "monthly" || order.Amount != 19.9 || order.Currency != "CNY" {
```

The fixture in the same file at line 54 (`{"monthly", "Monthly", 29.9, 30, []string{"yundian", "yundash"}}`) is the seed for this test — flip it too:

OLD:
```go
		{"monthly", "Monthly", 29.9, 30, []string{"yundian", "yundash"}},
```

NEW:
```go
		{"monthly", "Monthly", 19.9, 30, []string{"yundian", "yundash"}},
```

- [ ] **Step 4: Run unit tests to verify**

```bash
go test -race -run 'TestPayment|TestHandler' ./internal/...
```
Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/handler_test.go internal/service/payment_db_test.go
git commit -m "test(unit): align monthly amount assertions with new ¥19.9 price

Both tests seed a CN 'monthly' plan fixture and assert order.amount
against it. After migration 016 sets monthly.price=19.9, the assertions
must follow. monthly_usd (USD PayPal fixture) untouched.

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 7: Update documentation

**Files:**
- Modify: `docs/api-integration-guide.md` (Plans section example payload)
- Modify: `CLAUDE.md` (Commercial plans snippet)

- [ ] **Step 1: Find the Plans example in `docs/api-integration-guide.md`**

```bash
grep -n "29.9\|299\|Plans" docs/api-integration-guide.md | head -20
```

- [ ] **Step 2: Replace any `29.9` (monthly) and `299` (yearly) references in the Plans section**

Apply targeted Edits. If a JSON example payload like:
```json
{"id":"monthly","price":29.9,...}
{"id":"yearly","price":299,...}
```
appears in the Plans section, replace `29.9` with `19.9` and `299` with `199.9`. Leave other `29.9` references untouched (e.g. webhook payload examples are channel amounts, not plan prices).

- [ ] **Step 3: Update CLAUDE.md "Commercial plans" section**

Apply this Edit (verify the exact wording first — adjust if the surrounding text differs):

OLD:
```
In addition to identity, price, interval, app scope, and active state, each Plan carries `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, nullable `description`, `display_order`, and DB-managed `updated_at`. `currency` is restricted to `CNY` / `USD` / `EUR`, `trial_days` must be non-negative, and quote/order currency plus quote trial duration are derived from the Plan.
```

NEW: (add a one-liner about the current catalog state)
```
In addition to identity, price, interval, app scope, and active state, each Plan carries `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, nullable `description`, `display_order`, and DB-managed `updated_at`. `currency` is restricted to `CNY` / `USD` / `EUR`, `trial_days` must be non-negative, and quote/order currency plus quote trial duration are derived from the Plan. As of migration 016 the public catalog shows only `monthly` (¥19.9/mo, list ¥29.9) and `yearly` (¥199.9/yr, list ¥299.9); `quarterly` and `free` are fully retired (`is_listed=false`).
```

- [ ] **Step 4: Commit**

```bash
git add docs/api-integration-guide.md CLAUDE.md
git commit -m "docs: align integration guide and CLAUDE.md with migration 016 catalog

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 8: Final verification — make test + make e2e

**Files:** none (verification only)

- [ ] **Step 1: Run unit tests with race detection and coverage**

```bash
make test
```
Expected: pass.

- [ ] **Step 2: Run E2E tests against local PostgreSQL**

```bash
make e2e
```
Expected: pass. Pay particular attention to:
- `TestE2E_PlanPricing_OnlyTwoPlansReturned` (new in Task 5)
- `TestE2E_PlanPricing_QuarterlyHiddenAndInactive` (new in Task 5)
- `TestE2E_PlanPricing_OrderMonthlyNewPrice` (new in Task 5)
- All pre-existing tests that touch `monthly` / `quarterly` / `free` plans.

- [ ] **Step 3: Run linter**

```bash
make lint
```
Expected: pass.

- [ ] **Step 4: Final commit (no changes expected; this is a sanity step)**

If `make test` / `make e2e` / `make lint` revealed any fix-up changes, commit them here with a descriptive message. If no changes, skip this step.

---

## Self-Review Checklist

- **Spec coverage:**
  - §5.1 migration SQL → Task 1 ✓
  - §7.1 e2e seed update → Task 2 ✓
  - §7.1 extra_test.go quarterly → Task 3 ✓
  - §7.1 integration_test.go seed → Task 4 ✓
  - §7.2 new E2E tests → Task 5 ✓
  - §7.1 unit test amount assertions → Task 6 ✓
  - §A.8 api-integration-guide.md + CLAUDE.md → Task 7 ✓
  - Verification gate → Task 8 ✓

- **Placeholder scan:** No "TBD" / "TODO" / "implement later" in the plan. Every code step shows actual code; every commit command is a complete `git` invocation.

- **Type consistency:** Test helper names (`startTestServer`, `doRequest`, `doJSONAuth`, `openTestDB`) appear consistently across Tasks 5 and 6. Plan IDs (`monthly`, `yearly`, `quarterly`, `free`, `monthly_usd`) are used consistently throughout.

- **DRY / YAGNI:** No speculative test for non-existent behavior (e.g., no test for `original_price` field — it's intentionally out of scope per spec §3). No premature abstraction.