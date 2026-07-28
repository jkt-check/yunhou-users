# 7 天试用发放（trial grant）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新用户首次 OAuth 登录时，由 yunhou-users 真正发放 7 天试用订阅行（`plan_id='trial'`），前端三处把 `'trial'` 识别为试用态，替换掉现在的"UI 壳"假试用。

**Architecture:** 方案 B（spec 已确认）——新 migration 018 种 `trial` plan 行（active、不可购买、不上架、trial_days=7）；`AuthService.getOrCreateUser` 新建用户分支 best-effort 发放 active 订阅行（失败不阻塞登录）；支付链路零改动（结转/降级守卫/购买规则已验证自动成立）；前端收敛 `plan_id === 'free'` 字面量为 `isTrialPlanId()` helper。

**Spec:** `docs/superpowers/specs/2026-07-28-trial-grant-design.md`（yunhou-users 仓，`feat/trial-grant` 分支）

**Tech Stack:** Go (sqlx + postgres migrations)、React + TypeScript + vitest（happy-dom）

**两个仓库：**
- yunhou-users：`/Users/lili/Downloads/github/yunhou-users`（分支 `feat/trial-grant`，spec 已提交于此）
- yunhou-website：`/Users/lili/Downloads/github/yunhou-website`（先切出 `feat/trial-grant` 分支再开工）

**关键约束（来自项目记忆，执行时必须遵守）：**
- users 仓 Go 测试多包共用本地 postgres，必须 `go test -p 1` 串行跑；`DATABASE_URL` 可覆盖，默认 `postgres://postgres@localhost/yunhou_users?sslmode=disable`
- users 仓预置失败（与本改动无关，见到不要修）：repo 包测试 panic、integration 404、`TestWeChat_OAuth_MockMode_FullRoundTrip`——在干净 worktree 上可复现的才算预置
- website pr-ci 有全局分支覆盖率阈值 90%——新增代码必须带测试
- 不碰 yunhou-deploy 的 env/ENV_BLOB；migration 随 deploy 流水线自动应用

---

## Part A：yunhou-users

### Task 1: migration 018 种 trial plan 行

**Files:**
- Create: `migrations/018_trial_plan.sql`
- Modify: `migrations/README.md`（Files 表加一行）

背景：`cmd/migrate` 按文件名词典序应用，每个文件包在单事务里并记 `_migrations` ledger。README 要求所有 DDL 幂等（seed 用 `INSERT ... ON CONFLICT DO NOTHING`）。幂等性由 `internal/migrate/migrate_test.go` 的 `TestApply_RealMigrationsFromRepo` 对新库全量重放保证。plans 表当前列（002 + 012 + 014 演化后）：`id, name, price, interval_days, apps, is_active, created_at, is_listed, accepting_new_subscriptions, currency, trial_days, description, display_order, updated_at`（`is_default` 已被 014 drop）。

- [ ] **Step 1: 写 migration 文件**

创建 `migrations/018_trial_plan.sql`：

```sql
-- 2026-07-28: 7-day free trial grant (docs/superpowers/specs/2026-07-28-trial-grant-design.md).
--
-- Registers the 'trial' plan row consumed by AuthService.grantTrialSubscription
-- (internal/service/auth.go) on a user's first-ever login. Properties:
--   is_active=true                    — has_access requires plan.is_active
--                                       (resolvePlanForTokenIssuanceWithPlan)
--   accepting_new_subscriptions=false — CreateOrder rejects it (409); trial
--                                       can only be granted, never bought
--   is_listed=false                   — stays out of the public catalog
--   trial_days=7                      — grant reads this; ops can tune via the
--                                       plan admin API without a deploy
--   interval_days=0                   — smallest interval, so repurchaseAllowed
--                                       lets a trial user buy any paid plan and
--                                       the activation-time downgrade guard
--                                       never fires from a trial row
--   apps={yundian,yundash}            — trial = full functionality, same app
--                                       set as the paid plans
INSERT INTO plans (id, name, price, interval_days, apps, is_active,
                   accepting_new_subscriptions, is_listed, trial_days)
VALUES ('trial', 'Free Trial', 0, 0, '{yundian,yundash}', true,
        false, false, 7)
ON CONFLICT (id) DO NOTHING;
```

- [ ] **Step 2: README Files 表加一行**

`migrations/README.md` 表格末尾（017 行之后）加：

```markdown
| `018_trial_plan.sql` | adds the `trial` plan row (active, not purchasable, not listed, trial_days=7) backing `AuthService.grantTrialSubscription` — 7-day free trial granted at first login |
```

- [ ] **Step 3: 跑 migration 幂等性测试**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/migrate/ -run TestApply_RealMigrationsFromRepo -v
```

Expected: PASS（新库全量重放所有 migration 含 018；第二次应用 ON CONFLICT 空转）。无本地 postgres 时确认数据库先在跑（`pg_isready` 或 `psql postgres://postgres@localhost/yunhou_users -c 'select 1'`）。

- [ ] **Step 4: 本地库应用一次 + 目检**

```bash
cd /Users/lili/Downloads/github/yunhou-users && make migrate && psql postgres://postgres@localhost/yunhou_users -c "SELECT id, is_active, accepting_new_subscriptions, is_listed, trial_days FROM plans WHERE id='trial';"
```

Expected: 一行 `trial | t | f | f | 7`。（若 `make migrate` 不存在，查看 Makefile 用对应的 migrate 目标。）

- [ ] **Step 5: Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-users
git add migrations/018_trial_plan.sql migrations/README.md
git commit -m "feat(plans): seed trial plan row for first-login grant (migration 018)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: 试用发放——先写失败测试（auth_test.go）

**Files:**
- Test: `internal/service/auth_test.go`（追加到文件末尾；模式参考既有 `TestAuthService_LoginWithProfile`，约 line 611）

测试基建（全部已存在于 `internal/service/mock_test.go` / `auth_test.go`）：`newAuthMocks()` 返回 `(ur, sir, pr, sr, ssr, ar)`；`ar.seedActive("yundian", "云店")`；`pr.plans[id] = &model.Plan{...}`；`sr.byUserID[userID]` 保存 active 订阅、`sr.createErr` 注入 Create 失败；`tokenSvc := newTokenServiceWithMocks(ssr, sr)`；`svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)`。`model.Plan` 有 `TrialDays int` 字段。

- [ ] **Step 1: 写失败测试**

`internal/service/auth_test.go` 末尾追加：

```go
// 2026-07-28 trial grant (spec: docs/superpowers/specs/2026-07-28-trial-grant-design.md):
// a brand-new user's first login receives an active 'trial' subscription
// row expiring in trial.trial_days. Grant is best-effort — failures must
// never block login (subscription is a capability layer, not identity).
func TestAuthService_LoginWithProfile_TrialGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	seedTrialPlan := func(pr *mockPlanRepo) {
		pr.plans["trial"] = &model.Plan{
			ID: "trial", Name: "Free Trial", Apps: []string{"yundian", "yundash"},
			IsActive: true, AcceptingNewSubscriptions: false, TrialDays: 7,
		}
	}
	login := func(svc *AuthService, uid string) (*LoginResponse, error) {
		return svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: uid, Email: uid + "@x.com"},
			AppID:   "yundian",
		})
	}

	t.Run("new user gets active trial sub and trial-shaped response", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlan(pr)
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		before := time.Now()
		resp, err := login(svc, "gh-trial-new")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		sub := sr.byUserID[resp.User.ID]
		if sub == nil {
			t.Fatal("expected a trial subscription row for the new user")
		}
		if sub.PlanID != "trial" {
			t.Errorf("plan_id = %q, want trial", sub.PlanID)
		}
		if sub.Status != "active" {
			t.Errorf("status = %q, want active", sub.Status)
		}
		if sub.ExpiresAt == nil {
			t.Fatal("expires_at must be set (nil = never expires)")
		}
		want := before.Add(7 * 24 * time.Hour)
		if diff := sub.ExpiresAt.Sub(want); diff < -time.Minute || diff > time.Minute {
			t.Errorf("expires_at = %v, want %v ±1m", *sub.ExpiresAt, want)
		}

		// The very first login response already surfaces the trial —
		// first paint shows the trial countdown, no second fetch needed.
		if resp.Subscription == nil {
			t.Fatal("expected subscription in response")
		}
		if resp.Subscription.PlanID != "trial" {
			t.Errorf("response plan_id = %q, want trial", resp.Subscription.PlanID)
		}
		if !resp.Subscription.HasAccess {
			t.Error("response has_access = false, want true during trial")
		}
		if resp.Subscription.ExpiresAt == nil {
			t.Error("response expires_at must carry the trial expiry")
		}
	})

	t.Run("existing user (identity match) gets no grant", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlan(pr)
		ur.users["user-old"] = &model.User{ID: "user-old", Status: "active"}
		sir.identities["github:gh-old"] = &model.SocialIdentity{
			ID: "ident-old", UserID: "user-old", Provider: "github", ProviderUID: "gh-old",
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := login(svc, "gh-old")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sr.subs) != 0 {
			t.Errorf("expected no subscription rows for an existing user, got %d", len(sr.subs))
		}
		if resp.Subscription != nil && resp.Subscription.PlanID == "trial" {
			t.Error("existing user must not receive a trial")
		}
	})

	t.Run("email-merge into an existing user gets no grant", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlan(pr)
		ur.users["user-merge"] = &model.User{ID: "user-merge", Status: "active"}
		email := "merge@x.com"
		sir.byEmail[email] = []model.SocialIdentity{
			{ID: "ident-m", UserID: "user-merge", Provider: "google", ProviderUID: "g-1", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		// A brand-new GitHub identity whose email matches an existing
		// google identity merges into that user — created=false, no trial.
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-merge", Email: email},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sr.subs) != 0 {
			t.Errorf("email-merged user must not receive a trial, got %d rows", len(sr.subs))
		}
	})

	t.Run("trial plan row missing: login succeeds without a grant", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// deliberately no seedTrialPlan — migration not applied yet
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := login(svc, "gh-noplan")
		if err != nil {
			t.Fatalf("login must not fail when the trial plan is missing: %v", err)
		}
		if resp.AccessToken == "" {
			t.Error("expected tokens even without the trial grant")
		}
		if len(sr.subs) != 0 {
			t.Errorf("expected no subscription rows, got %d", len(sr.subs))
		}
	})

	t.Run("trial_days=0 skips the grant but login succeeds", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.plans["trial"] = &model.Plan{
			ID: "trial", Name: "Free Trial", Apps: []string{"yundian"},
			IsActive: true, AcceptingNewSubscriptions: false, TrialDays: 0,
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		if _, err := login(svc, "gh-zero"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sr.subs) != 0 {
			t.Errorf("trial_days=0 must not grant (would be instantly expired), got %d rows", len(sr.subs))
		}
	})

	t.Run("subscription insert failure does not block login", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlan(pr)
		sr.createErr = errors.New("db down")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := login(svc, "gh-grantfail")
		if err != nil {
			t.Fatalf("grant failure must not block login: %v", err)
		}
		if resp.AccessToken == "" || resp.RefreshToken == "" {
			t.Error("expected tokens even when the trial grant fails")
		}
	})
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/service/ -run TestAuthService_LoginWithProfile_TrialGrant -v
```

Expected: FAIL——"new user gets active trial sub..." 子测试在 `sr.byUserID[resp.User.ID] == nil` 处挂（`expected a trial subscription row`）；其余子测试应该已经通过（它们断言的是"不发放"的现状）。如果有子测试意外失败（比如 email-merge 的 mock 行为与预期不符），先读 mock 实现修正测试，不要改实现。

---

### Task 3: 实现 grantTrialSubscription 并接入 getOrCreateUser

**Files:**
- Modify: `internal/service/auth.go`（`getOrCreateUser` 约 line 228-273；helper 放在其后）

- [ ] **Step 1: 实现发放逻辑**

`internal/service/auth.go` 中：

1) `getOrCreateUser` 末尾（identity Create 成功之后、`return s.userRepo.FindByID(ctx, userID)` 之前）插入：

```go
	// 4. First-ever login: best-effort grant of the free trial. Grant
	//    failures never block login — subscription is a capability
	//    layer, not an identity layer (cn-staging 2026-07-23 incident).
	//    Only the created=true branch grants: email-merge and
	//    existing-identity logins are not new users (spec: 只发新用户).
	if isNew {
		s.grantTrialSubscription(ctx, userID)
	}

	return s.userRepo.FindByID(ctx, userID)
```

2) 同一函数 dup-key race 分支里的 `_ = isNew // orphan cleanup is a sweeper concern`（约 line 267）：`isNew` 现在有真实用途，删掉这行丢弃语句，保留其上方注释块不动。

3) `getOrCreateUser` 之后新增 helper 与常量：

```go
// TrialPlanID is the catalog id of the free-trial plan row seeded by
// migration 018_trial_plan.sql. The row is is_active (so has_access
// computes true) but accepting_new_subscriptions=false — the trial can
// only be granted here, never bought.
const TrialPlanID = "trial"

// grantTrialSubscription inserts the active trial subscription row for a
// brand-new user. Best-effort by design: every failure is logged and
// swallowed so a catalog hiccup can never bounce a first login. The
// partial unique index idx_subscriptions_user_active turns a concurrent
// duplicate grant into a DB-level no-op (unique violation, logged here).
// There is deliberately no backfill for pre-existing users (spec: 只发新用户).
func (s *AuthService) grantTrialSubscription(ctx context.Context, userID string) {
	plan, err := s.planRepo.FindByID(ctx, TrialPlanID)
	if err != nil {
		log.Printf("trial grant: find plan %q: %v (user %s)", TrialPlanID, err, userID)
		return
	}
	if plan.TrialDays <= 0 {
		log.Printf("trial grant: plan %q has trial_days=%d, skipping (user %s)", TrialPlanID, plan.TrialDays, userID)
		return
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
	if err := s.subRepo.Create(ctx, &model.Subscription{
		ID:        GenerateUUID(),
		UserID:    userID,
		PlanID:    plan.ID,
		Status:    "active",
		StartedAt: now,
		ExpiresAt: &expiresAt,
	}); err != nil {
		log.Printf("trial grant: create subscription: %v (user %s)", err, userID)
	}
}
```

（`log`、`time`、`model`、`GenerateUUID` 在 auth.go 均已使用，无需新 import。）

- [ ] **Step 2: 跑 Task 2 的测试确认全部通过**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/service/ -run TestAuthService_LoginWithProfile_TrialGrant -v
```

Expected: 6 个子测试全 PASS。

- [ ] **Step 3: 跑既有 auth 测试防回归**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/service/ -run 'TestAuthService' -v
```

Expected: 全 PASS。特别注意既有 `TestAuthService_LoginWithProfile`/`TestAuthService_TestLogin` 不能挂——TestLogin 不走 `getOrCreateUser`（它自己建 user + synthetic identity），不受影响。

- [ ] **Step 4: Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-users
git add internal/service/auth.go internal/service/auth_test.go
git commit -m "feat(auth): grant 7-day trial subscription on first login

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: 支付链路 DB 测试——试用结转 + trial 不可购买

**Files:**
- Test: `internal/service/payment_db_test.go`

支付链路**零实现改动**——本任务用测试钉住 spec §3 的两条关键交互。先读既有测试 `TestConfirm_RolloverUpgrade`（monthly→yearly 结转，2026-07-28 加的）与 `TestCreateOrder_*` 系列作为模式模板。

- [ ] **Step 1: setupPaymentDB 种子加 trial 行**

`setupPaymentDB`（payment_db_test.go 约 line 31-78）的 plans 种子 slice 里加一行（插在 yearly 之后）：

```go
		{"trial", "Free Trial", 0, 0, []string{"yundian", "yundash"}},
```

并在种子循环之后加 trial 的商用属性（与 migration 018 对齐——种子 INSERT 只写 5 列，其余靠默认值，需显式 UPDATE）：

```go
	// trial mirrors migration 018: grantable by auth, never purchasable,
	// never listed. The rollover tests only need the row to exist with
	// interval_days=0; the 409 test needs accepting_new_subscriptions=false.
	if _, err := db.ExecContext(context.Background(), `
		UPDATE plans SET accepting_new_subscriptions = false, is_listed = false, trial_days = 7
		WHERE id = 'trial'
	`); err != nil {
		t.Fatalf("seed trial plan flags: %v", err)
	}
```

- [ ] **Step 2: 写结转测试（先确认失败）**

复制 `TestConfirm_RolloverUpgrade` 整段为 `TestConfirm_TrialRolloverOnFirstPurchase`，做两处改动：

1. 既有活跃订阅的种子从 monthly 改为 trial：`seedActiveSub(t, db, uid, "trial", trialExpiry)`，其中 `trialExpiry := time.Now().Add(72 * time.Hour)`（试用剩 3 天）。
2. 期望到期从"旧到期 +365 天"改为**试用到期日 +30 天**：

```go
	withinSeconds(t, gotExpiry, trialExpiry.Add(30*24*time.Hour), 2*time.Minute)
```

（具体变量名沿用被复制测试的现有写法；`readSub`/`withinSeconds` 助手已存在于同文件。）

跑一次确认它**通过**——注意：这个测试是对既有 rollover 行为的 characterization test，resolveSubExpiry 对 trial（interval_days=0 的旧订阅）天然走 rollover 分支，所以应当直接 PASS。如果 FAIL，说明对现有逻辑的理解有误，停下来重读 resolveSubExpiry 再修测试。

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/service/ -run TestConfirm_TrialRolloverOnFirstPurchase -v
```

- [ ] **Step 3: 写 trial 不可下单测试（先确认通过）**

复制 `TestCreateOrder_DowngradeRejected` 为 `TestCreateOrder_TrialPlanNotPurchasable`：改为对一个**无任何订阅**的用户下单 `planID = "trial"`，期望返回 `ErrPlanNotAcceptingNew`（常量定义于 `internal/service/errors.go`，`TestLogin` 已在用）：

```go
	if !errors.Is(err, ErrPlanNotAcceptingNew) {
		t.Fatalf("expected ErrPlanNotAcceptingNew, got %v", err)
	}
```

（下单入口是 `svc.CreateOrder(ctx, ...)`——参数列表照抄被复制的测试。）这个测试也应直接 PASS（eligibilityAndInsertOrderTx 已检查 AcceptingNewSubscriptions）；它是行为钉住，防未来回归。

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test ./internal/service/ -run TestCreateOrder_TrialPlanNotPurchasable -v
```

- [ ] **Step 4: 串行跑整个 service 包**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go test -p 1 ./internal/service/
```

Expected: PASS（DB 测试需本地 postgres；跨包必须 `-p 1`，单包内也建议串行确认无 TRUNCATE 竞争）。

- [ ] **Step 5: Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-users
git add internal/service/payment_db_test.go
git commit -m "test(payments): pin trial rollover on first purchase + trial not purchasable

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Part B：yunhou-website

先切分支：`cd /Users/lili/Downloads/github/yunhou-website && git checkout -b feat/trial-grant`

### Task 5: plan.ts——先写失败测试

**Files:**
- Test: `src/site/components/console/plan.test.ts`（文件顶部已 mock `../../lib/siteConfig`，mock 里 `trialDays: 7` 已有；追加 describe 即可）

- [ ] **Step 1: 写失败测试**

`src/site/components/console/plan.test.ts` 末尾追加（import 行加 `isTrialPlanId`——现在还不存在，这正是失败点）：

```ts
import { deriveSubscriptionState, isTrialPlanId, resolvePlan } from './plan';

// ...既有测试保持不动...

// 2026-07-28 trial grant: yunhou-users migration 018 + first-login grant
// now issues a real subscription row with plan_id='trial'. The console
// must render it as the trial state (same chrome as the legacy 'free').
describe('trial plan (first-login grant)', () => {
  it('resolvePlan returns the trial snapshot for plan_id "trial"', () => {
    const snap = resolvePlan('trial');
    expect(snap.key).toBe('trial');
    expect(snap.labelKey).toBe('app.console.subscription.trialBadge');
    expect(snap.priceLabel('zh')).toBeNull();
    expect(snap.priceUsd).toBe(0);
    expect(snap.periodDays).toBe(7);
  });

  it('isTrialPlanId recognises only free and trial', () => {
    expect(isTrialPlanId('free')).toBe(true);
    expect(isTrialPlanId('trial')).toBe(true);
    expect(isTrialPlanId('monthly')).toBe(false);
    expect(isTrialPlanId('yearly')).toBe(false);
    expect(isTrialPlanId(null)).toBe(false);
    expect(isTrialPlanId(undefined)).toBe(false);
    expect(isTrialPlanId('')).toBe(false);
  });

  it('active trial: isTrial + isActive with days remaining', () => {
    const state = deriveSubscriptionState({
      plan_id: 'trial',
      plan_name: 'Free Trial',
      has_access: true,
      expires_at: new Date(Date.now() + 3 * 86400000).toISOString(),
    });
    expect(state.isTrial).toBe(true);
    expect(state.isActive).toBe(true);
    expect(state.isExpired).toBe(false);
    expect(state.daysRemaining).toBe(3);
  });

  it('expired trial: isTrial + isExpired, not active', () => {
    const state = deriveSubscriptionState({
      plan_id: 'trial',
      plan_name: 'Free Trial',
      has_access: false,
      expires_at: new Date(Date.now() - 86400000).toISOString(),
    });
    expect(state.isTrial).toBe(true);
    expect(state.isExpired).toBe(true);
    expect(state.isActive).toBe(false);
  });
});
```

（检查文件顶部的既有 import 行——若已有 `import { deriveSubscriptionState, resolvePlan } from './plan'`，只把 `isTrialPlanId` 加进去，不要重复 import。）

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/components/console/plan.test.ts
```

Expected: FAIL（`isTrialPlanId is not a function` / 导出不存在）。

---

### Task 6: plan.ts 实现 trial 快照 + isTrialPlanId

**Files:**
- Modify: `src/site/components/console/plan.ts`

- [ ] **Step 1: 实现**

按顺序改 5 处：

1) `PlanKey` 加 `"trial"`，并引入 `PaidPlanKey`（line 13）：

```ts
export type PlanKey = "free" | "trial" | "monthly" | "yearly";

/** Plans that cost money and live in PRICING.plans — the SSOT lookup
 *  below only covers these two; trial/free have no price row. */
export type PaidPlanKey = "monthly" | "yearly";
```

2) `PERIOD_KEY`（line 29-33）加 trial 项：

```ts
const PERIOD_KEY: Record<PlanKey, "month" | "year" | null> = {
  free: null,
  trial: null,
  monthly: "month",
  yearly: "year",
};
```

3) `LABEL_KEY`、`PRICING_BY_ID`、`buildPriceLabel` 三处的 `Exclude<PlanKey, "free">` 全部换成 `PaidPlanKey`（line 35、44-45、67）——否则把 `"trial"` 加进 PlanKey 后，SSOT 启动检查会要求 PRICING.plans 里存在 trial 价格行而 throw。

4) `PLAN_SNAPSHOTS` 在 `free` 之后加 trial 快照（line 89 起）：

```ts
  trial: {
    key: "trial",
    labelKey: "app.console.subscription.trialBadge",
    priceLabel: () => null,
    priceUsd: 0,
    periodDays: PRICING.trialDays,
  },
```

5) `deriveSubscriptionState` 的 isTrial（line 154）改为覆盖两个 key，并在文件导出区加 helper：

```ts
/** True for plan ids that represent an unpaid trial: the migration-018
 *  'trial' plan granted at first login, and legacy 'free' rows. */
export function isTrialPlanId(planId: string | null | undefined): boolean {
  return planId === "free" || planId === "trial";
}
```

```ts
  const isTrial = plan.key === "free" || plan.key === "trial";
```

（用 `plan.key` 而不是 `isTrialPlanId(sub?.plan_id)`：resolvePlan 把未知 planId 回落到 free 快照，既有行为是这些也按试用渲染——保持不变。）

- [ ] **Step 2: 跑测试确认通过**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/components/console/plan.test.ts
```

Expected: 全新 PASS（含既有用例——SubscriptionCard 等消费方不受影响）。

- [ ] **Step 3: Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-website
git add src/site/components/console/plan.ts src/site/components/console/plan.test.ts
git commit -m "feat(console): recognise plan_id 'trial' as the trial state

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: PricingSection——试用用户 CTA 解锁

**Files:**
- Modify: `src/site/components/blocks/PricingSection.tsx:195-200`
- Test: `src/site/components/blocks/PricingSection.test.tsx`

- [ ] **Step 1: 写失败测试**

打开 `PricingSection.test.tsx`，找到 2026-07-28 加的升级 CTA 测试（`mockAuthSub.value` 设为 monthly 订阅、断言 yearly 卡出现"升级到年付"的那个，约 line 306-340）。复制它为 `trial user sees an enabled subscribe CTA on both cards`，改动：

```ts
    mockAuthSub.value = {
      plan_id: 'trial',
      plan_name: 'Free Trial',
      has_access: true,
      expires_at: '2026-08-04T00:00:00Z',
    };
```

断言（沿用该测试既有的查询方式）：两张卡的 CTA 按钮都**不是** disabled 的 `billing.pricing.cta.subscribed` 态——monthly 卡与 yearly 卡均显示可点击的订阅文案（`pricing.cta.subscribed`，即传入的 `ctaSubscribeLabel`；测试里 `t` 是 identity 函数则按 key 断言）。

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/components/blocks/PricingSection.test.tsx
```

Expected: 新用例 FAIL——当前 `onPaidPlan = currentPlan !== 'free'`，trial 被当付费用户，CTA 是 disabled 的已订阅态。

- [ ] **Step 3: 实现**

`PricingSection.tsx`：

1) import 区加：

```ts
import { isTrialPlanId } from "../console/plan";
```

2) line 195-200 替换为：

```ts
  // Trial users (plan_id 'trial' from the first-login grant, or legacy
  // 'free') are intentionally allowed to subscribe early — only an
  // active paid plan locks the Subscribe button.
  const currentPlan = subscription?.plan_id;
  const onPaidPlan = !!currentPlan && !isTrialPlanId(currentPlan);
```

- [ ] **Step 4: 跑测试确认通过 + Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/components/blocks/PricingSection.test.tsx
git add src/site/components/blocks/PricingSection.tsx src/site/components/blocks/PricingSection.test.tsx
git commit -m "feat(pricing): unlock subscribe CTA for trial users

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 8: useBilling——试用用户不被续订门槛拦截

**Files:**
- Modify: `src/site/hooks/useBilling.ts`（docblock 约 line 156-157；门槛约 line 478-480）
- Test: `src/site/hooks/useBilling.test.ts`

- [ ] **Step 1: 写失败测试**

`useBilling.test.ts` 的 `describe('useBilling.startCheckout')` 内追加（模式照抄 "lets a monthly subscriber start the yearly upgrade checkout"）：

```ts
  // 2026-07-28 trial grant: the first-login grant issues plan_id 'trial'
  // with has_access=true. Trial users must pass the repurchase gate —
  // they have no paid plan to renew, only a trial to convert.
  it('lets a trial user (plan_id "trial", has_access) start a checkout', async () => {
    (globalThis as { __authSub?: unknown }).__authSub = {
      plan_id: 'trial', plan_name: 'Free Trial',
      has_access: true, expires_at: '2026-08-04T00:00:00Z',
    };
    mockStartCheckout.mockResolvedValue({
      checkout_url: 'https://paypal.example/approve',
      order_id: 'order_tr',
      expires_at: '2026-08-01T01:00:00Z',
    });
    const { result } = renderHook(() => useBilling());

    await act(async () => {
      await result.current.startCheckout('monthly');
    });

    expect(mockStartCheckout).toHaveBeenCalledWith('monthly', 'paypal');
    expect(mockToast).not.toHaveBeenCalledWith(
      expect.objectContaining({ title: 'billing.pricing.cta.subscribed' }),
    );
  });
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/hooks/useBilling.test.ts
```

Expected: 新用例 FAIL——trial 被 `onPaidPlan` 当付费用户，toast 拦截、`mockStartCheckout` 未被调用。

- [ ] **Step 3: 实现**

`useBilling.ts`：

1) import 区加：

```ts
import { isTrialPlanId } from '../components/console/plan';
```

2) docblock（约 line 156-157）更新：

```
 *  - Already on a *paid* plan -> toast and bail. Trial users
 *    (`plan_id` 'trial' from the first-login grant, or legacy 'free')
 *    are intentionally allowed to subscribe early.
```

3) 门槛（约 line 478-480）：

```ts
      const currentPlan = subscription?.plan_id;
      const onPaidPlan = !!currentPlan && !isTrialPlanId(currentPlan);
```

（其上方 "has_access is also true during the 7-day free trial" 注释块语义不变，保留。）

- [ ] **Step 4: 跑测试确认通过 + Commit**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npx vitest run src/site/hooks/useBilling.test.ts
git add src/site/hooks/useBilling.ts src/site/hooks/useBilling.test.ts
git commit -m "feat(billing): let trial users pass the repurchase gate

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 9: 全量验证（两仓）

**Files:** 无新增

- [ ] **Step 1: users 全量测试（串行）**

```bash
cd /Users/lili/Downloads/github/yunhou-users && go build ./... && go vet ./... && go test -p 1 ./...
```

Expected: PASS。已知预置失败（见到先去干净 master worktree 复现确认，是预置就跳过不管）：repo 包 panic、integration 404、`TestWeChat_OAuth_MockMode_FullRoundTrip`。

- [ ] **Step 2: website 全量测试 + 覆盖率 + 构建**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npm test && npm run coverage && npm run build
```

Expected: 全 PASS；全局分支覆盖率 ≥ 90%（pr-ci 阈值；本计划每个改动都带了测试，若掉下 90% 先查哪个新分支没覆盖）；`tsc -b` 无类型错误（重点：plan.ts 的 `PaidPlanKey` 重构没有漏改 `Exclude<PlanKey,"free">`）。

- [ ] **Step 3: BFF 测试（未改 BFF，确认无回归即可）**

```bash
cd /Users/lili/Downloads/github/yunhou-website && npm run test:bff
```

Expected: PASS。

- [ ] **Step 4: 收尾**

两仓分支推送前的 PR 前置检查（项目记忆）：独立 review 干净 + 本地所有测试通过，缺一不可。review 用 `/goal` 流程另起，不在本计划内。

---

## Self-Review 记录

- **Spec 覆盖**：migration 018 → Task 1；发放点+失败不阻塞+只发新用户 → Task 2/3（6 个子测试一一对应）；spec §3 支付交互（结转、不可下单、降级守卫不触发）→ Task 4（rollover/409 测试钉住；降级守卫由 interval_days=0 的算术保证，既有 `TestConfirm_*` 套件覆盖守卫本身）；前端 3 处 → Task 5-8；测试清单 → 全部落位。
- **有意不覆盖**：spec 说 TestLogin 不发放——由实现位置（`getOrCreateUser` 内，TestLogin 不走该路径）保证，Task 3 Step 3 的既有 TestLogin 测试防回归。
- **类型一致性**：`TrialPlanID`（Go 常量）只在 Task 3 定义、Task 2 测试用字面量 `"trial"`（有意——测试钉住字面量）；前端 `isTrialPlanId`/`PaidPlanKey` 在 Task 6 定义，Task 7/8 引用一致。
