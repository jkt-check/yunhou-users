# 7 天试用发放设计（trial grant）

日期：2026-07-28
状态：已与用户确认（方案 B / 结转 / 所有新用户 / 只发新用户）
涉及仓库：yunhou-users（发放 + plan 数据）、yunhou-website（前端识别 trial plan）

## 背景

营销页与 console 承诺"微信登录即开通 7 天试用"，但后端从未发放任何订阅：新用户注册只写 `users` + `social_identities`（auth.go 的 `LoginWithProfile` → `getOrCreateUser`），`/auth/refresh`（BFF `/auth/me` 代理）对新用户返回 `plan_id=""`、`has_access=false`。前端把空 plan_id 渲染成试用态（plan.ts `resolvePlan` 回落 `PLAN_SNAPSHOTS.free`），但 `has_access=false` 使其落到 `isExpired=true`——显示有试用，实际无权限（"UI 壳"）。

`plans.trial_days` 字段已存在（migration 012），但只被 Quote 报价消费，激活路径不读。当前 plans 表 4 行：monthly/yearly（在售）、quarterly（全退）、free（014 停用+下架）。

## 已确认的产品决策

| 决策点 | 结论 |
|---|---|
| 发放机制 | **方案 B**：新建 `trial` plan 行（不复用 free，不做 users 表虚拟字段） |
| 试用期付费 | **结转**：试用剩余天数累加到付费订阅（resolveSubExpiry rollover 自动成立） |
| 发放范围 | **所有新用户**（cn 微信 + intl，发放点在 users 服务不区分区域） |
| 存量用户 | **只发新用户**：不做补发分支；上线前注册的未付费用户保持现状 |
| 发放失败 | 不阻塞登录（订阅是能力层不是身份层），记 error 日志，无自动重试 |

## 设计

### 1. 数据层（yunhou-users，新 migration 017）

```sql
-- 预检查：已存在 id='trial' 行则 abort（风格对齐 014）
INSERT INTO plans (id, name, price, interval_days, apps, is_active,
                   accepting_new_subscriptions, is_listed, trial_days)
VALUES ('trial', 'Free Trial', 0, 0, '{yundian,yundash}',
        true,   -- has_access 判定要求 plan.is_active（auth.go resolvePlanForTokenIssuance）
        false,  -- 不可购买/下单
        false,  -- 不进公开目录（016 引入的 is_listed）
        7);
```

- `apps` 与在售 plan 一致（`{yundian,yundash}`），试用=全功能。
- `interval_days=0`：与 free 同形态，对购买规则是"最小周期"。
- 试用天数放 `trial.plans.trial_days`（初始 7），代码读行不写死，管理 API 可调。

### 2. 发放点（yunhou-users `internal/service/auth.go`）

`LoginWithProfile` 中 `getOrCreateUser` 返回"新建"时（仅新建分支，不发存量用户）：

1. 用户行创建成功后，作为**独立步骤**（不与用户创建同事务）发放：
   `INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at)
    VALUES (?, 'trial', 'active', now, now + trial.trial_days)`
   - 天数从 trial plan 行读（`planRepo.FindByID("trial")`）。
   - 并发双首次登录撞 `idx_subscriptions_user_active` 部分唯一索引：吞唯一冲突，后到请求走正常读取。
2. 失败处理：trial plan 缺失 → warn 跳过；INSERT 失败 → error 日志。两者都**不影响登录返回**（此时响应里的 subscription 仍按现状组装，新用户本轮拿到无订阅响应，下次登录……也无订阅——接受此现状，staging 体量下靠日志发现）。
   - 注：若实现在用户创建后、token 组装前完成发放，则本轮登录响应即带 `plan_id='trial', has_access=true, expires_at`，前端首屏就是试用倒计时。实现时优先保证这一点。
3. `TestLogin`（dev-only）不发放（保持测试登录行为可控）。

### 3. 与支付链路的交互（无需改代码，已逐条验证）

| 场景 | 行为 | 依据 |
|---|---|---|
| 试用中买 monthly/yearly | 放行 | `repurchaseAllowed`：`requested.IntervalDays >= current.IntervalDays`，trial=0 最小 |
| 试用剩余天数 | 结转到付费订阅 | `resolveSubExpiry` rollover：`existing.ExpiresAt + p.IntervalDays`，candidate 取 max |
| 降级守卫误拦 | 不会 | `oldPlan.IntervalDays(0) > p.IntervalDays` 恒 false |
| trial 被下单 | 409 | `eligibilityAndInsertOrderTx` 检查 `AcceptingNewSubscriptions=false` |
| trial 出现在目录 | 不会 | `is_listed=false` |
| 试用过期 | `has_access=false`，plan_id 保留 'trial' | 现有过期路径（auth.go:552-554） |
| 试用过期后购买 | 正常新订阅 | 现有逻辑 |

### 4. 前端改动（yunhou-website，3 处 + 测试）

把散落的 `plan_id === 'free'` 字面量特判收敛为 helper（如 `isTrialPlanId(id) = id === 'free' || id === 'trial'`，放 plan.ts 导出）：

1. **`src/site/components/console/plan.ts`**：
   - `PLAN_SNAPSHOTS` 加 `trial` 快照（渲染复用 free 快照：trialBadge 文案、`periodDays: PRICING.trialDays`、price 0）。
   - `deriveSubscriptionState`：`isTrial` 覆盖 `free` 与 `trial` 两个 key（free 保留兼容历史数据）。
2. **`src/site/components/blocks/PricingSection.tsx`**：`onPaidPlan` 排除条件 `currentPlan !== 'free'` → `!isTrialPlanId(currentPlan)`（否则 trial 用户被当付费用户锁 CTA）。
3. **`src/site/hooks/useBilling.ts`**：升级放行 `plan_id === 'free'` → `isTrialPlanId(plan_id)`。

倒计时、到期文案、"升级"CTA、`showRolloverHint` 均走 `expires_at`+`isTrial` 现有逻辑，自动生效。console SubscriptionCard 的试用 CTA 文案（"升级"）无需改。

### 5. 测试

**yunhou-users**（`go test -p 1`，本地 postgres 串行）：
- 新用户注册（LoginWithProfile 新建分支）→ trial 订阅行存在，expires_at ≈ now+7d；响应 subscription `plan_id='trial', has_access=true`
- 存量用户（已有任意订阅行，含 expired）登录 → 不重复发放（每用户最多一行 active 由唯一索引保证，且发放仅走新建分支）
- trial plan 缺失时注册 → 登录成功、无订阅行、日志有 warn
- 试用中付费激活 → expires_at = 试用到期日 + plan.interval_days（结转，复用 payment_db_test 的 seedActiveSub/readSub 基建）
- CreateOrder 买 'trial' → 409

**yunhou-website**（vitest，注意 pr-ci 全局分支覆盖率阈值 90%）：
- plan.ts：`resolvePlan('trial')` → trial 快照；trial+has_access+未来 expires_at → `isTrial && isActive`，daysRemaining 正确；trial+过去 expires_at → isExpired
- PricingSection：trial 用户 → 订阅 CTA 可用（非 subscribed 禁用态）
- useBilling：trial 用户 `start()` 不被"已订阅"门槛拦截

## 不做的事（YAGNI）

- 不补发存量用户（已确认）
- 不做试用到期提醒/转正漏斗（营销自动化，后续单独立项）
- 不改 free plan 现状（保持 014/016 的停用+下架）
- 不动 PayPal 渠道自身的 trial 配置（AppConfig.PayPal TrialDays 属 intl 订阅计费概念，与本功能的"注册即赠订阅行"正交）

## 已知边界

- 发放失败无自动重试（无补发分支的代价，用户已接受）；靠 error 日志发现。
- 用户删号重注册无防护（当前无删号功能；若未来加删号，需考虑 trial 防滥用——同一微信 unionid 重建会再得试用）。
