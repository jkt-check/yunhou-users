# Login / Subscription Decouple — Design Spec

**Date:** 2026-07-23
**Status:** Draft
**Author:** Claude (Yunhou Users)
**Related incidents:** cn-staging 2026-07-23 WeChat-login → "订阅已过期" banner → resubscribe blocked → permanent loop
**Companion spec:** `2026-07-23-sub-expires-at-end-to-end-design.md` (separately tracks the sub_expires_at data-channel fix; this spec is orthogonal — identity layer vs. data layer)

> **Superseded by 2026-07-24 commercialization design (Phase 2).** The default-plan
> fallback described in this spec was retired by `migrations/014_remove_default_plan.sql`
> and the `resolvePlanForTokenIssuance` rewrite (Tasks T17–T21). In the current contract:
> - `sub=nil` → `JWT.scope=[]`, `HasAccess=false` (no default plan)
> - `expired=true, sub!=nil` → preserves `sub.PlanID`, `scope=[]`, `HasAccess=false`
>
> The login/subscription decoupling itself (deleting `ErrSubscriptionExpired`,
> `findUsableSubscription → peekSubscription`) remains the standing contract; only
> the plan-selection branch is obsolete.

## 1. Problem statement

`yunhou-users` today fuses **authentication** (who you are) with **subscription enforcement** (whether you can use the app) inside OAuth callbacks and the access-token-issuance tail:

- `service.ErrSubscriptionExpired` and `service.ErrSubscriptionNotActive` are sentinels returned from `findUsableSubscription` during **token issuance**.
- `handler.AuthHandler.authErrReason` (handler/auth.go:62-63) maps them to `reason=subscription_expired` on the URL the user is redirected to.
- The BFF (yunhou-website) renders `auth.error.subscription_expired` as a red banner that **replaces the login form's interactive elements**.
- `useBilling` then refuses a fresh checkout because the user's subscription row is still `status='active'` from the auth view.

Net effect: when a paid-but-past-`expires_at` row exists in `subscriptions`, **the user cannot log in, cannot resubscribe, and gets stuck in a loop.** Verified live 2026-07-23 on cn-staging.

The user's mental model — *登录跟订阅有什么关系？订阅管的是能不能用应用？* — is correct: this split is wrong. SaaS-standard pattern is:

- **Login** issues a token unconditionally (you are who you say you are).
- **Subscription** gates API/UI access (you can or cannot use the app right now).
- A user with an expired subscription is allowed in, sees "your plan has expired" inside the console, and is sent through the renewal flow from there.

## 2. Goal

Remove subscription-state checks from the login / refresh / token-issuance code path. A user authenticates and gets a session token **regardless** of subscription state. The token's plan scope and the login response's `subscription` block reflect the user's current entitlement (active, expired-but-recoverable, none), but they do **not** abort login.

## 3. Non-goals

- **No API-level 402 enforcement** in this PR. There is no `RequireSubscription` middleware anywhere in the codebase today and we are not adding one. The console FE already gates protected actions via `Subscription.HasAccess` (read-only) — that pattern stays unchanged.
- **No `sub_expires_at` end-to-end plumbing** in this PR — see `2026-07-23-sub-expires-at-end-to-end-design.md` for that work.
- **No sweeper** that periodically flips `status=active` rows with `expires_at < now()` to `status=expired`. Leaving rows as-is and letting `peekSubscription` do the right thing at read time is sufficient; adding cron machinery is outside the agreed scope. (Cf. the data-backfill migration in §7.)

## 4. Design decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Delete `service.ErrSubscriptionExpired`** | Login/refresh should never fail because of subscription state. Sentinel exists solely to translate sub state into a login error; with the mapping removed, the sentinel is dead weight. |
| 2 | **Delete `service.ErrSubscriptionNotActive`** | Same reason. |
| 3 | **`findUsableSubscription` → `peekSubscription`** returning `(sub *model.Subscription, expired bool, err error)` | The function's job becomes "tell me what's there." Login-time expiry checks are policy decisions moved to the caller (which always wants login to succeed). |
| 4 | **`issueTokensForUser` falls back to default plan when `expired=true`** | Plan selection at token-issuance time is still valid — but the plan choice must not depend on whether the sub is *current*. If the sub's `expires_at` is past, pick the default plan and tag `HasAccess=false` on the response so the FE can render the renewal banner in console. |
| 5 | **Expired subscription's `PlanID` is preserved in the response** | Returning `PlanID = "free"` would mislead the FE into showing a downgrade or hiding the "renew" CTA. Preserve the user's intended plan (e.g. `"monthly"`), set `HasAccess = false` — honest state: "still on monthly, but access has lapsed". |
| 6 | **Response shape unchanged** | `SubscriptionInfo{PlanID, PlanName, HasAccess, ExpiresAt}` is the contract the BFF reads today; the only difference is that the values for an expired-sub user now read `HasAccess=false, ExpiresAt=<original>`. No BFF schema change required. |
| 7 | **`authErrReason` and `expectedAuthErrors` lists in `handler/auth.go` lose the two subscription entries** | Follows from (1) and (2). |
| 8 | **SPA `AuthLoginPage`'s subscription_expired guard is preserved** as defence-in-depth | The BFF shouldn't emit this reason after this PR ships, but future code may bring it back. The 30-line guard and 9 unit tests are cheap insurance; we update the comment to reflect that it's a historical carry-over rather than the expected path. |
| 9 | **No new failure mode on `/auth/me`** | `/auth/me` (and any JWT-protected endpoint that already runs) doesn't go through `issueTokensForUser` — it just decodes an existing token. Today its `_/user.GET subscription` field is whatever was stamped at issuance. With this PR the field for an expired-but-logged-in user reads `HasAccess=false`, which is the correct semantic. We don't read subscription at request time. |

## 5. Architecture changes (file map)

| # | File | Change |
|---|---|---|
| 1 | `internal/service/errors.go` | Delete `ErrSubscriptionExpired`, `ErrSubscriptionNotActive`. Update doc-comments listing which calls remain. |
| 2 | `internal/service/auth.go` | `findUsableSubscription` → `peekSubscription(ctx, userID)` returning `(*model.Subscription, bool /*expired*/, error)`. `issueTokensForUser` (used by `RefreshToken` and `TestLogin`) and `LoginWithProfile` use `expired` to decide between sub plan vs default plan. |
| 3 | `internal/handler/auth.go` | Remove the two subscription sentinels from `expectedAuthErrors` (line 25-36) and from `authErrReason` (line 52-71). Delete the in-package test for those branches. |
| 4 | `internal/handler/auth_github.go`, `internal/handler/auth_wechat.go` | No direct changes — they call `isExpectedAuthErr(authErrReason(err))`. With the sentinel deleted, those code paths stop mapping subscription errors to URL reasons. |
| 5 | `internal/service/auth_test.go` | Existing tests reference `findUsableSubscription`; rename to `peekSubscription`. Add unit coverage for: (a) `peekSubscription` with past `expires_at` returns `(sub, true, nil)`; (b) `issueTokensForUser` with `expired=true` returns `LoginResponse{Subscription.HasAccess=false, Subscription.PlanID=<sub plan>, Subscription.ExpiresAt=<sub.ExpiresAt>}` and no error. |
| 6 | `internal/service/payment_test.go` | `TestBuildReconcileWebhookEvent_*` already locks `SubExpiresAt == nil` for the real bug. No additions here. |
| 7 | `internal/handler/handler_test.go` | Update `authErrReason` table test to drop rows whose err wraps the deleted sentinels (the rest of the table is unchanged). |
| 8 | `migrations/012_backfill_expired_active_subscriptions.sql` | NEW. One-shot backfill for the cn-staging pollution: `UPDATE subscriptions SET expires_at = NOW() + (SELECT interval_days * INTERVAL '1 day' FROM plans WHERE plans.id = subscriptions.plan_id) WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW();`. Guarded with a `_migrations` entry. Idempotent: running twice does nothing the second time. |
| 9 | `migrations/README.md` | Note that `012_*` is **not** rolled back on environmental reset (intentional; once the rows are fixed, the migration is no-op). |
| 10 | `yunhou-users/bin/backfill-active-sub-expirations.sh` | NEW. Wraps the SQL with `--dry-run` (default) printing the rows that would change and `--apply` flag to actually run. Connects via `docker exec yunhou-deploy-postgres-1` using creds from `yunhou-deploy/env/cn.staging.env`. Manual run only; not part of the deploy pipeline. |
| 11 | `yunhou-website/.claude/worktrees/kaya-only/src/site/pages/AuthLoginPage.tsx` | Update the file-header doc comment: change *"`Yunhou's WeChat/GitHub callback refused to issue tokens because `findUsableSubscription` saw an expired row`"* to *"Historical carry-over: in 2026-07-23 the BFF used to do this; we now expect the BFF to never emit `reason=subscription_expired`, but the guard stays as defence-in-depth."* No code change. |
| 12 | `internal/middleware/*` | Unchanged. There is no `RequireSubscription` middleware today and none added. |

## 6. Data flow (after fix)

```
Browser → /auth/wechat/callback
   ↓
auth_wechat.go/LoginWithProfile(profile)
   ↓
peekSubscription(userID) → (sub, expired=false, nil)
   ↓                       (or (sub, expired=true) — no error either way)
issueTokensForUser(user, appID)
   - expired=false, sub!=nil → plan = sub.plan, HasAccess = plan.Apps contains appID
   - expired=true,  sub!=nil → plan = default plan, HasAccess=false, ExpiresAt = sub.ExpiresAt
   - sub=nil                 → plan = default plan, HasAccess = default.Apps contains appID
   ↓
access_token{apps = plan.Apps} + SubscriptionInfo{PlanID, PlanName, HasAccess, ExpiresAt}
   ↓
BFF stores in /auth/me; SPA renders console with banner if HasAccess=false
```

No sentinel bubbles up to the callback; both branches succeed.

## 7. Backfill migration (rationale)

**Why this exists even though we no longer need the sub state for login:**
The cn-staging DB currently has at least one polluted subscription row:
```
user_id    = 018103d4-4b20-4f6f-8190-6d6a9944ff59
plan_id    = monthly
status     = active
expires_at = 2026-07-23 07:01:38+00   (≈6h in the past)
```
This row was written by pre-`c9ab516` reconcile code that aliased `res.SuccessTime → SubExpiresAt`. With that code gone, no new rows get this wrong, but the existing one is still latent.

Even after this PR's login-decouple work, **that user is still out of `HasAccess` because `expires_at < now()`**. That's the intended semantic ("subscription expired, go renew"). They can now log in, see a renewal banner in console, and click through to checkout. Whether the renewal extends *from today* or *from a date that had it remained active* is the backfill's call.

Decision: **extend from today**, not retroactively. The user already lived through `expires_at` as a date they could have used; retro-computing would mean giving them access time they didn't pay for. `NOW() + interval_days` matches what `activateSubscriptionOnTx` will compute for any new top-up anyway (see companion spec §4), so the math is consistent.

The backfill is a single UPDATE that reads `interval_days` from the `plans` table. Dry-run by default; `--apply` to commit. Idempotent because the WHERE clause excludes already-extended rows (their new `expires_at` will be in the future).

## 8. Tests

### 8.1 Required unit
- `TestPeekSubscription_Active_NotExpired` — happy path: `expires_at` in future → `(sub, false, nil)`
- `TestPeekSubscription_Active_PastExpiry` — `expires_at` past → `(sub, true, nil)`, **no error**
- `TestPeekSubscription_NoRow` — `sql.ErrNoRows` → `(nil, false, nil)` (not an error)
- `TestPeekSubscription_ExpiredDBError` — non-NoRows DB error → bubbles up
- `TestIssueTokensForUser_ExpiredSub_FallsBackToDefaultPlan` — input: a session whose user's row has `expires_at` past. Output: `(LoginResponse.Subscription.PlanID = defaultPlan.ID, HasAccess=false, ExpiresAt=&sub.ExpiresAt)`. No error.
- `TestIssueTokensForUser_NilSub_NoSubRow` — uses default plan; asserts no `expired` flag.
- `TestAuthErrReason_NoSubscriptionBranches` — table test that the function still returns `auth_failed` for any of the now-removed wrapped errors (proves the dead branch is gone).
- `TestBuildReconcileWebhookEvent_DoesNotSetSubExpiresAt` (existing) — unchanged.
- `TestReconcilePreCheck` (existing) — unchanged.

### 8.2 Required E2E
- `tests/e2e-ui`: a regression Playwright scenario where a user with `status=active, expires_at=past` clicks WeChat-login on cn-staging. **Expected**: lands on `/console`, not on `/auth/login`. Sees banner prompting renewal. No redirect loop.
- If `tests/e2e-ui` doesn't have such a fixture, add one to `seeds/users.go` (smoke-only user with a past-`expires_at` subscription row) and a Playwright `spec/auth-login-with-expired-sub.test.ts`.

### 8.3 Removed assertions
- The `expectedAuthErrors` and `authErrReason` table-test rows whose err was `service.ErrSubscriptionExpired` / `service.ErrSubscriptionNotActive`. These rows tested (a) → URL reason; with the sentinel gone there's nothing to test.

## 9. Rollout

1. Land this PR with backfill migration NOT applied (default `--dry-run`).
2. Run `bin/backfill_active_sub_expirations.sh --dry-run` on cn-staging. Capture output of how many rows would change. Manual review by operator.
3. Run `bin/backfill_active_sub_expirations.sh --apply` only after manual OK. (User has confirmed they will apply manually — this is not in the deploy pipeline.)
4. Rebuild users container, push to staging, deploy. Watch for the smoke run + console smoke that this specific scenario (login with past-`expires_at` user) passes.
5. Once cn-staging has been quiet for 24h with the new login behaviour, promote to prod.

## 10. Out of scope (acknowledged)

- **Sweeper for `status=active, expires_at < now()`**: agreed not in scope. Without it, future regrowth of these rows remains possible. Mitigation: the data-backfill script is reusable. If we ever want to add a sweeper it's a separate PR (per `yunhou-users/internal/service/sweeper.go` — doesn't exist yet, would need a worker entry point).
- **`sub_expires_at` E2E plumbing**: see companion spec.
- **Removing the SPA `AuthLoginPage` guard**: explicit decision (Decision #8) to keep as defence-in-depth. Removing it would let a future regression break login UX without test coverage.
