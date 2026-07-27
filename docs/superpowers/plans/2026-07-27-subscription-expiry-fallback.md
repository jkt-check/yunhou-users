# subscription-expires-at-fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `subscriptions.expires_at` always a meaningful RFC3339 timestamp on activation, fixing the WeChat-onboarding bug where the v3 NATIVE webhook doesn't carry `sub_expires_at` and the row stays NULL (= "never expires" per `isExpiredAt`).

**Architecture:** Add a single helper `resolveSubExpiry(ctx, planID, hint)` that returns:
1. `hint` verbatim if non-nil (BFF or webhook supplied it — current `subExpiresAtFromWebhook` path; future-proof for PayPal/Stripe),
2. otherwise looks up `plan.interval_days` and returns `now() + interval_days * 24h`,
3. otherwise (plan missing or `interval_days == 0`) returns `nil` plus audit-log when the lookup misses.

Wire it into the three activation call sites (`onPaymentSucceeded`, `buildReconcileWebhookEvent`, `Confirm`). Keep `onPaypalRenewalSucceeded`'s fail-loud policy verbatim — divergence is intentional, documented in a comment. Remove `omitempty` on `SubscriptionInfo.ExpiresAt` so the JSON field is always present.

**Tech Stack:** Go 1.22+, sqlx, gin, lib/pq, golang-jwt/jwt v5.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/service/payment.go` | Single source of truth for sub-expiry fallback | Add helper, swap 3 call sites, update 1 comment, add 1 clarifying comment |
| `internal/service/auth.go` | Login response DTO | Remove `omitempty` on `ExpiresAt` |
| `internal/service/payment_db_test.go` | DB integration tests | New tests for webhook path + Confirm path |
| `internal/service/payment_db_tx_test.go` | DB transaction tests | New test for reconcile path |
| `internal/handler/auth_*_test.go` | HTTP-shape tests | New assertion that `expires_at` field is always present |
| `docs/api-integration-guide.md` | Public contract | Document new fallback behavior + JSON shape change |
| `migrations/017_sub_expiry_does_not_backfill.sql` | No-op marker | Mark explicit decision: no backfill, capture rationale |

Plan is intentionally narrow: pure logic +1 SQL no-op. No new endpoints, no new tables, no new webhook types.

---

## Task 1: Add `resolveSubExpiry` helper

**Files:**
- Modify: `internal/service/payment.go` (insert near `subExpiresAtFromWebhook` at line 1901)

- [ ] **Step 1: Write the failing test**

Append to `internal/service/payment_db_test.go` (find the `TestOnPaymentSucceeded_*` block near the end):

```go
func TestResolveSubExpiry_HintForwarded(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()
    s := newTestPaymentService(db)

    hint := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
    got, err := s.resolveSubExpiry(context.Background(), "monthly", &hint)
    if err != nil {
        t.Fatalf("resolveSubExpiry: %v", err)
    }
    if got == nil || !got.Equal(hint) {
        t.Errorf("hint not forwarded: got %v, want %v", got, hint)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestResolveSubExpiry_HintForwarded ./internal/service/`
Expected: compile error (method does not exist yet).

- [ ] **Step 3: Implement the helper**

In `internal/service/payment.go`, replace the existing `subExpiresAtFromWebhook` (line 1901-1905) with:

```go
// ErrPlanMissingForExpiry is returned by resolveSubExpiry when the plan row
// was deleted between order creation and webhook arrival. Callers audit-log
// this and fall back to the existing "NULL = never expires" branch.
var ErrPlanMissingForExpiry = errors.New("plan missing for sub-expiry fallback")

// resolveSubExpiry returns the expires_at to write on a subscription
// activation. Priority:
//
//  1. caller-supplied hint (BFF on Confirm; webhook payload on channels
//     that ship sub_expires_at, e.g. Stripe metadata / PayPal renewal).
//     nil = no hint, fall through.
//
//  2. plan.interval_days fallback. Real WeChat NATIVE v3 doesn't ship
//     sub_expires_at (verified 2026-07-27), so this fires for every fresh
//     WeChat charge unless the BFF forwards one via
//     /payments/orders/:order_id/confirm. Base = now() inside the calling
//     transaction so started_at and expires_at agree on the same reference
//     time. active dormant plans with interval_days > 0 also fall through
//     here — IsActive is intentionally NOT checked because the activation
//     itself is the transition that decides whether the plan still applies.
//
//  3. nil (plan missing OR interval_days == 0). Caller decides: webhook
//     paths audit-log + write NULL; Confirm path mirrors the same shape.
//
// Coexists with the existing subExpiresAtFromWebhook passthrough — the
// hint is just the first arg, no separate method needed.
func (s *PaymentService) resolveSubExpiry(ctx context.Context, planID string, hint *time.Time) (*time.Time, error) {
    if hint != nil {
        return hint, nil
    }
    plan, err := s.planRepo.FindByID(ctx, planID)
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return nil, ErrPlanMissingForExpiry
        }
        return nil, fmt.Errorf("find plan for expiry fallback: %w", err)
    }
    if plan.IntervalDays <= 0 {
        return nil, nil
    }
    t := time.Now().Add(time.Duration(plan.IntervalDays) * 24 * time.Hour)
    return &t, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestResolveSubExpiry_HintForwarded ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/payment.go internal/service/payment_db_test.go
git commit -m "feat(subscription): add resolveSubExpiry helper with plan.interval_days fallback"
```

---

## Task 2: Wire `resolveSubExpiry` into `onPaymentSucceeded`

**Files:**
- Modify: `internal/service/payment.go` (line 1138)
- Modify: `internal/service/payment.go` (replace `subExpiresAtFromWebhook` factory)

- [ ] **Step 1: Write the failing test**

Append to `internal/service/payment_db_test.go`:

```go
func TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()
    s := newTestPaymentService(db)

    userID := "user-wc-no-hint"
    planID := "monthly"
    seedUser(t, db, userID)
    seedPlan(t, db, planID, 30)
    orderID := seedPendingOrder(t, db, userID, planID, 19.9, "CNY")

    // WebhookEvent with no SubExpiresAt — simulates real WeChat v3.
    e := service.WebhookEvent{
        Channel:       "wechat_pay",
        EventID:       "evt-wc-1",
        EventType:     "TRANSACTION.SUCCESS",
        TransactionID: "txn-wc-1",
        OrderID:       orderID,
        Amount:        19.9,
        Currency:      "CNY",
    }

    if err := s.onPaymentSucceeded(context.Background(), e); err != nil {
        t.Fatalf("onPaymentSucceeded: %v", err)
    }

    var exp sql.NullTime
    if err := db.Get(&exp,
        `SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
        t.Fatalf("read sub: %v", err)
    }
    if !exp.Valid {
        t.Fatal("expires_at is NULL — fallback did not fire")
    }
    if exp.Time.Before(time.Now()) {
        t.Error("expires_at is in the past")
    }
    if exp.Time.After(time.Now().Add(31 * 24 * time.Hour)) {
        t.Errorf("expires_at too far in the future: %v", exp.Time)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval ./internal/service/`
Expected: FAIL (currently writes NULL because `subExpiresAtFromWebhook` returns nil).

- [ ] **Step 3: Modify `onPaymentSucceeded` to use the helper**

In `internal/service/payment.go`, replace the call site at line 1138:

```go
// OLD:
if _, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiresAtFromWebhook(e)); err != nil {
    return fmt.Errorf("activate sub: %w", err)
}
```

with:

```go
// NEW:
subExpiry, err := s.resolveSubExpiry(ctx, order.PlanID, e.SubExpiresAt)
if err != nil {
    if errors.Is(err, ErrPlanMissingForExpiry) {
        _ = writeAuditOnTx(ctx, tx, "service", "subscription_expiry_plan_missing",
            fmt.Sprintf("plan:%s", order.PlanID),
            []string{"webhook", "expiry_fallback", "plan_missing"},
            map[string]any{
                "order_id": order.ID,
                "channel":  e.Channel,
                "event_id": e.EventID,
            })
    } else {
        return fmt.Errorf("resolve sub expiry: %w", err)
    }
}
if _, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiry); err != nil {
    return fmt.Errorf("activate sub: %w", err)
}
```

Also delete the now-unused `subExpiresAtFromWebhook` function (lines 1901-1905). Note: the `WebhookEvent.SubExpiresAt` field on line 1617-1619 stays — it's the input to the helper, not the output.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Run the existing related tests to confirm no regression**

Run: `go test -run "TestOnPaymentSucceeded|TestActivateSubscriptionOnTx" ./internal/service/`
Expected: PASS (existing tests use Stripe/PayPal with explicit `SubExpiresAt`, so hint path fires).

- [ ] **Step 6: Commit**

```bash
git add internal/service/payment.go internal/service/payment_db_test.go
git commit -m "fix(subscription): WeChat webhook falls back to plan.interval_days"
```

---

## Task 3: Wire `resolveSubExpiry` into `Confirm` (BFF-confirmed path)

**Files:**
- Modify: `internal/service/payment.go` (line 712)

- [ ] **Step 1: Write the failing test**

Append to `internal/service/payment_db_test.go`:

```go
func TestConfirm_NoHint_UsesPlanInterval(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()
    s := newTestPaymentService(db)

    userID := "user-confirm-no-hint"
    planID := "monthly"
    seedUser(t, db, userID)
    seedPlan(t, db, planID, 30)
    orderID := seedPendingOrder(t, db, userID, planID, 19.9, "CNY")

    res, err := s.Confirm(context.Background(), service.ConfirmInput{
        OrderID:       orderID,
        UserID:        userID,
        Channel:       "wechat_pay",
        ExternalTxnID: "txn-confirm-1",
        ExpiresAt:     nil, // BFF didn't pass one
    })
    if err != nil {
        t.Fatalf("Confirm: %v", err)
    }
    if !res.ActivatedSubscription {
        t.Error("subscription not activated")
    }

    var exp sql.NullTime
    if err := db.Get(&exp,
        `SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
        t.Fatalf("read sub: %v", err)
    }
    if !exp.Valid {
        t.Fatal("expires_at is NULL — Confirm fallback did not fire")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestConfirm_NoHint_UsesPlanInterval ./internal/service/`
Expected: FAIL (currently writes NULL because `in.ExpiresAt` is nil).

- [ ] **Step 3: Modify `Confirm` to use the helper**

In `internal/service/payment.go`, replace the call site at line 712:

```go
// OLD:
activated, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, in.ExpiresAt)
if err != nil {
    return nil, fmt.Errorf("activate sub: %w", err)
}
```

with:

```go
// NEW:
subExpiry, err := s.resolveSubExpiry(ctx, order.PlanID, in.ExpiresAt)
if err != nil {
    if errors.Is(err, ErrPlanMissingForExpiry) {
        _ = writeAuditOnTx(ctx, tx, "service", "subscription_expiry_plan_missing",
            fmt.Sprintf("plan:%s", order.PlanID),
            []string{"confirm", "expiry_fallback", "plan_missing"},
            map[string]any{
                "order_id": order.ID,
                "channel":  in.Channel,
            })
    } else {
        return nil, fmt.Errorf("resolve sub expiry: %w", err)
    }
}
activated, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiry)
if err != nil {
    return nil, fmt.Errorf("activate sub: %w", err)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestConfirm_NoHint_UsesPlanInterval ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Run related Confirm tests**

Run: `go test -run "TestConfirm" ./internal/service/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service/payment.go internal/service/payment_db_test.go
git commit -m "fix(subscription): Confirm path falls back to plan.interval_days when BFF omits expires_at"
```

---

## Task 4: Update `buildReconcileWebhookEvent` comment

**Files:**
- Modify: `internal/service/payment.go` (lines 1907-1951)

- [ ] **Step 1: No code change — comment only**

The reconcile path inherits the fix from Task 2 because it goes through `onPaymentSucceeded`. But the long comment block from line 1907-1928 explicitly says "for now, both paths produce the same subscription shape (never-expires)" — this is now obsolete and contradicts the new behavior.

Replace the comment block (lines 1907-1928) with:

```go
// buildReconcileWebhookEvent builds the WebhookEvent that the active
// reconciliation path (`reconcileFromChannel` in GetOrder) feeds into
// OnWebhook when WeChat's QueryOrder reports the order as SUCCESS.
//
// SubExpiresAt is intentionally LEFT NIL. WeChat's QueryOrder response
// carries `success_time` (when the payment settled — a moment in the
// past), not a subscription expiry. Reusing it as SubExpiresAt would
// write subscriptions.expires_at = <past>, and the auth path's
// `findUsableSubscription` would refuse the next login with
// ErrSubscriptionExpired even though the user just paid (real-world
// observed in cn-staging 2026-07-23).
//
// Sub-expiry is computed downstream by onPaymentSucceeded's
// resolveSubExpiry helper, which falls back to plan.interval_days when
// no hint is provided. Pre-fix behavior was "never expires"; post-fix
// behavior is "now() + plan.interval_days*24h" — same shape as the
// BFF-confirmed Confirm path.
```

The function body (lines 1929-1951) stays the same — SubExpiresAt stays nil, the fallback fires inside `onPaymentSucceeded`.

- [ ] **Step 2: Verify the path still works end-to-end**

Run: `go test -run "TestReconcile" ./internal/service/`
Expected: PASS (no functional change, comment-only).

- [ ] **Step 3: Commit**

```bash
git add internal/service/payment.go
git commit -m "docs(subscription): update reconcile path comment to reflect fallback fix"
```

---

## Task 5: Document the `onPaypalRenewalSucceeded` divergence

**Files:**
- Modify: `internal/service/payment.go` (above line 1421)

- [ ] **Step 1: Add a clarifying comment block**

The PayPal renewal path explicitly does NOT use the fallback (line 1570-1587 audit-logs `paypal_renewal_no_expiry_hint` and refuses to extend). This is intentional — renewal is a known recurring billing context, so missing `next_billing_time` is a sign the upstream contract drifted. Operators need to investigate.

Add a comment block right before the existing `onPaypalRenewalSucceeded` doc comment (line 1421):

```go
// Note: onPaypalRenewalSucceeded does NOT use resolveSubExpiry. The
// webhook path (onPaymentSucceeded → resolveSubExpiry) and the Confirm
// path (Confirm → resolveSubExpiry) both fall back to plan.interval_days
// when no sub_expires_at hint is supplied. PayPal renewal is intentionally
// different:
//
// - WeChat onboarding: sub_expires_at is structurally absent (v3 NATIVE
//   protocol doesn't carry it); the fallback is the only way to write a
//   non-NULL expires_at.
// - PayPal renewal: sub_expires_at is structurally PRESENT (resource.
//   billing_info.next_billing_time); falling back to plan.interval_days
//   when it's missing would silently mask a contract drift between
//   PayPal's product definition and our Plan. The
//   paypal_renewal_no_expiry_hint audit log lets ops reconcile manually.
```

- [ ] **Step 2: Verify no test regression**

Run: `go test -run "TestPaypalRenewal" ./internal/service/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/service/payment.go
git commit -m "docs(subscription): document PayPal renewal divergence from fallback path"
```

---

## Task 6: Remove `omitempty` on `SubscriptionInfo.ExpiresAt`

**Files:**
- Modify: `internal/service/auth.go` (line 155)

- [ ] **Step 1: Write the failing test**

Append to `internal/handler/auth_common_test.go`:

```go
func TestLoginResponse_SubscriptionExpiresAt_AlwaysPresent(t *testing.T) {
    // Build a LoginResponse with ExpiresAt = nil (the buggy / pre-fix shape).
    // After the fix, the JSON tag no longer has omitempty, so the field
    // must serialize as "expires_at": null rather than being absent.
    resp := &service.LoginResponse{
        AccessToken:  "tok",
        RefreshToken: "rt",
        User:         service.UserInfo{ID: "u-1"},
        Subscription: &service.SubscriptionInfo{
            PlanID:         "monthly",
            HasAccess:      true,
            IsAcceptingNew: true,
            ExpiresAt:      nil,
        },
    }
    raw, err := json.Marshal(resp)
    if err != nil {
        t.Fatalf("marshal: %v", err)
    }
    if !strings.Contains(string(raw), `"expires_at":null`) {
        t.Errorf(`expected "expires_at":null in JSON, got: %s`, raw)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestLoginResponse_SubscriptionExpiresAt_AlwaysPresent ./internal/handler/`
Expected: FAIL (current `omitempty` causes the field to be absent).

- [ ] **Step 3: Modify the JSON tag**

In `internal/service/auth.go` line 155, change:

```go
// OLD:
ExpiresAt      *time.Time `json:"expires_at,omitempty"`

// NEW:
ExpiresAt      *time.Time `json:"expires_at"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestLoginResponse_SubscriptionExpiresAt_AlwaysPresent ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Run all auth tests to confirm no regression**

Run: `go test -run "TestAuth" ./internal/handler/ ./internal/service/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service/auth.go internal/handler/auth_common_test.go
git commit -m "feat(api): subscription.expires_at always present in LoginResponse JSON"
```

---

## Task 7: Cross-check existing tests assert new field semantics

**Files:**
- Modify: `internal/handler/auth_common_test.go`, `internal/handler/auth_wechat_test.go`, `internal/handler/auth_github_test.go`

- [ ] **Step 1: Find and update existing snapshots**

These three test files build `service.SubscriptionInfo` literals with only `HasAccess` set. None assert on the JSON output. After Task 6, those literals still pass (ExpiresAt is optional), but for production confidence we should add at least one assertion that a populated `ExpiresAt` round-trips.

Append to `internal/handler/auth_common_test.go`:

```go
func TestLoginResponse_SubscriptionExpiresAt_RFC3339Serialization(t *testing.T) {
    when := time.Date(2027, 1, 15, 12, 30, 45, 0, time.UTC)
    resp := &service.LoginResponse{
        User:         service.UserInfo{ID: "u-1"},
        Subscription: &service.SubscriptionInfo{PlanID: "monthly", HasAccess: true, ExpiresAt: &when},
    }
    raw, _ := json.Marshal(resp)
    if !strings.Contains(string(raw), `"expires_at":"2027-01-15T12:30:45Z"`) {
        t.Errorf("expected RFC3339 expires_at, got: %s", raw)
    }
}
```

- [ ] **Step 2: Run all handler tests**

Run: `go test ./internal/handler/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/handler/auth_common_test.go
git commit -m "test(api): assert expires_at round-trips as RFC3339"
```

---

## Task 8: Add no-op backfill decision migration

**Files:**
- Create: `migrations/017_sub_expiry_does_not_backfill.sql`

- [ ] **Step 1: Write the migration**

The plan intentionally does NOT backfill existing NULL rows. Reasons captured here:

```sql
-- 2026-07-27: subscription expiry fallback fix landed (internal/service/payment.go
-- resolveSubExpiry helper). Pre-existing rows with expires_at IS NULL were
-- activated without a sub_expires_at hint, so the auth path treats them as
-- "never expires" (isExpiredAt: NULL means never expires).
--
-- Decision: do NOT backfill. Two reasons:
--   1. started_at is also written at activation time. For pre-fix rows,
--      started_at is real (e.g. 6 months ago). Issuing a fresh
--      now() + interval_days would silently give a paying customer a
--      longer subscription than they paid for.
--   2. started_at + interval_days is already in the past for any
--      pre-fix row except the most recent activations, so backfilling
--      with that formula would immediately mark them expired.
--
-- Resolution mechanism: any subsequent activation (renewal confirm,
-- webhook re-delivery) goes through the new resolveSubExpiry helper and
-- writes a fresh now() + interval_days. Manual remediation for stuck
-- accounts is via DELETE /user/subscriptions/:id + re-purchase.
--
-- This is a no-op migration deliberately — it documents the decision in
-- the same file so the choice is traceable.
SELECT 1;
```

- [ ] **Step 2: Verify migration applies cleanly**

Run: `make e2e` (uses the test harness that runs migrations in order).
Expected: PASS (the empty SELECT is a no-op).

- [ ] **Step 3: Commit**

```bash
git add migrations/017_sub_expiry_does_not_backfill.sql
git commit -m "docs(migration): mark explicit no-backfill decision for sub expiry fix"
```

---

## Task 9: Update public API contract docs

**Files:**
- Modify: `docs/api-integration-guide.md`

- [ ] **Step 1: Update the `subscription` shape description**

Find the `subscription` JSON sample in the LoginResponse section (around line 152) and update the surrounding text to reflect that `expires_at` is always present:

After line 156 (`"subscription": {...}`), add a paragraph:

```markdown
**`subscription.expires_at` 契约**: 自 2026-07-27 起，`expires_at` 字段在 JSON 响应中**始终存在**（不再 `omitempty`）。新激活的订阅固定为 RFC3339 时间戳（优先 `plan.interval_days` 推算，对 WeChat v3 而言是 webhook 路径上唯一可得的产物，因为 v3 NATIVE 不携带 `sub_expires_at`）。历史 NULL 行（修复前由 WeChat 激活）保持 `null`——它们在 `subscriptions.expires_at` 列上就是 NULL，isExpiredAt 仍按 "never expires" 处理。如果 BFF 想要 UI 上的"永不过期"分支，应仅在 `expires_at: null` 时显示，不再用「字段缺席」做存在性判断。
```

- [ ] **Step 2: Update the `POST /apps/:id/quote` description**

Find the `sub_expires_at` row in the `quote` endpoint table (around line 650) and update the description. The current text says:

> yunhou-users 不二次推导 webhook payload 里的值

Add a sentence:

```markdown
注：yunhou-users webhook / Confirm 路径在 webhook payload / BFF 入参均不含 `sub_expires_at` 时，会回退到 `plan.interval_days`（`time.Now() + interval_days*24h`）作为 contract 兜底。PayPal 续费路径是例外——`next_billing_time` 缺失会被审计并拒绝延期，`sub_expires_at` 不参与。
```

- [ ] **Step 3: Commit**

```bash
git add docs/api-integration-guide.md
git commit -m "docs(api): document subscription.expires_at fallback contract"
```

---

## Task 10: Announce contract change to BFF team

**Files:**
- Modify: `PROGRESS.md` (write a note about the contract change)
- Modify: `docs/superpowers/specs/` (NO new spec — this is a fix, not a new design)

- [ ] **Step 1: Add a PROGRESS.md entry**

Append to `PROGRESS.md`:

```markdown
## 2026-07-27: subscription.expires_at 现在始终是 RFC3339

**Breaking-ish contract change** (consumer behavior, not protocol):

- `LoginResponse.subscription.expires_at` 和 `GET /user/subscriptions[]` 中该字段从可选（`omitempty`）变为必存在。WeChat 通道因 v3 NATIVE 不携带 `sub_expires_at`，以前会写 NULL → 现在写 `now() + plan.interval_days*24h`。
- BFF 接入侧需要：删除「`expires_at` 字段缺席 → 永不过期」的 UI 分支；现在 `expires_at: null` 才表示真正的"永不过期"（仅出现在修复前激活的行上）。
- PayPal 续费路径策略不变：`next_billing_time` 缺失仍 fail-loud（audit-log `paypal_renewal_no_expiry_hint`），不参与 fallback。
- 历史 NULL 行不补：见 `migrations/017_sub_expiry_does_not_backfill.sql`。
- 修复点：`internal/service/payment.go` 新增 `resolveSubExpiry` 助手；`webhook` / `Confirm` / `reconcile` 三个入口均接入；`internal/service/auth.go` 上 `SubscriptionInfo.ExpiresAt` 去掉 `omitempty`。
```

- [ ] **Step 2: Commit**

```bash
git add PROGRESS.md
git commit -m "docs: announce subscription.expires_at contract change"
```

---

## Self-Review

### Spec coverage

- ✅ Field rename interval_days: called out in Task 1 (helper uses `plan.IntervalDays`) and self-review.
- ✅ Reference time = now(): Task 1 step 3 hardcodes `time.Now()`; Task 2 step 3 inherits.
- ✅ started_at stays now(): no change in Tasks 2/3, `activateSubscriptionOnTx` unchanged.
- ✅ Reuse across Confirm / webhook / reconcile: Tasks 2, 3, 4.
- ✅ PayPal renewal divergence: Task 5 (comment only).
- ✅ JSON tag change: Task 6.
- ✅ Plan missing audit log: Tasks 2/3 step 3.
- ✅ Backfill decision: Task 8.
- ✅ API integration docs: Task 9.
- ✅ BFF notification: Task 10.

### Placeholder scan

No "TBD / TODO / implement later" in the plan. All code blocks are complete. No "similar to Task N" references — every test and code snippet is self-contained.

### Type consistency

- `resolveSubExpiry(ctx, planID, hint)` signature used identically in Tasks 1, 2, 3.
- `ErrPlanMissingForExpiry` referenced identically in Tasks 1, 2, 3.
- `service.ConfirmInput` and `service.WebhookEvent` field names match the existing codebase.
- JSON tag change in Task 6 matches the test assertions in Tasks 6, 7.

### Test coverage matrix

| Path | Hint present | Hint absent, plan OK | Plan missing |
|---|---|---|---|
| webhook (WeChat) | Task 2 step 5 (existing) | Task 2 step 1 | (implicit via Task 1 step 1's nil branch) |
| Confirm | Task 3 step 5 (existing) | Task 3 step 1 | (implicit via Task 1) |
| reconcile | Task 4 (review only) | inherited | inherited |
| PayPal renewal | unchanged | unchanged | unchanged |
| JSON tag | Task 7 | Task 6 | — |

### Out-of-scope (deliberately)

- New endpoints / new columns / new webhook types — none added.
- PayPal renewal policy change — explicit decision to keep fail-loud.
- Backfill of NULL rows — explicit no-backfill decision.
- BFF-side code changes — only documented; not implemented in this repo.
