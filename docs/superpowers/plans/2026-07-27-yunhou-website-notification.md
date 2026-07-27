# Yunhou Users → yunhou-website 集成通知（订阅过期回退）

> 发送对象：yunhou-website 团队
> 发送时机：yunhou-users 推到 origin 后（commit 链 ad7073d..75d448f，共 23 个 commits）
> 涉及变更：**wire contract**，yunhou-website 必须改 BFF 代码适配；非兼容升级

---

## 1. ⚠️ 必看：`subscription.expires_at` 字段从「可选」变成「必存在」

### 变更前后对比

| | 旧（≤2026-07-26 部署） | 新（2026-07-27+ 部署） |
|---|---|---|
| 有有效订阅的用户 | `"expires_at": "2026-08-04T..."` | 同上，无变化 |
| 没有订阅 / 订阅过期 | 字段**缺席**（omitempty） | `"expires_at": null`（字段存在，值为 null） |
| WeChat 新激活的订阅 | 字段**缺席**（bug，写了 NULL） | `"expires_at": <now + plan.interval_days*24h>` |

### BFF 影响

**所有**反序列化 `subscription.expires_at` 的代码都必须把"字段缺席 → 永不过期"分支改成"字段存在但值是 null → 永不过期"。具体场景：

```ts
// ❌ 旧代码
const exp = subscription?.expires_at;
if (exp === undefined) { /* 永不过期 */ }
if (new Date(exp) < new Date()) { /* 已过期 */ }

// ✅ 新代码
const exp = subscription?.expires_at ?? null;
if (exp === null) { /* 永不过期：预 2026-07-27 激活 + 没有订阅 的历史行 */ }
if (new Date(exp) < new Date()) { /* 已过期 */ }
```

yunhou-users 侧代码（`internal/service/auth.go:155`）已经把 JSON tag 改成无 `omitempty`：`ExpiresAt *time.Time \`json:"expires_at"\``。配套测试见 `internal/handler/auth_common_test.go:TestLoginResponse_SubscriptionExpiresAt_AlwaysPresent` / `_RFC3339Serialization`。

### 历史 NULL 行（2026-07-27 之前由 WeChat 激活的订阅）

**没有回填**。决定见 `migrations/017_sub_expiry_does_not_backfill.sql`，原因：

1. 这些行的 `started_at` 是真实过去的某天，如果用 `now() + interval_days` 回填，会让客户实际获得的订阅时长超过他们付的钱
2. 用 `started_at + interval_days` 回填，绝大多数行会立即显示过期

**处理机制**：这些客户下次任何渠道（renewal / webhook 重投 / Confirm）触发激活时，会走新的 `resolveSubExpiry` helper 写入新的 `now() + interval_days`。运维侧如需手动修复：让用户 DELETE subscription 后重新购买即可。

---

## 2. WeChat sub_expires_at 回退路径

### 现状（之前没说清楚）

yunhou-users 的 `POST /apps/:id/quote` 返回 `sub_expires_at`，建议 BFF 在 `POST /payments/orders/:id/confirm` body 里也带上 `expires_at`（沿用同字段名）。**WeChat v3 NATIVE 协议本身不携带 `sub_expires_at`**，所以 yunhou-users 的 webhook 路径永远拿不到这个值。

之前（pre-fix）的 BFF 行为风险：
- BFF 不传 `expires_at` → webhook 写 `subscriptions.expires_at = NULL` → auth 路径的 `isExpiredAt` 把 NULL 当"永不过期" → 这就是 2026-07-23 cn-staging 事件的根因（每个 WeChat 订阅都变成终身会员）

### 修复后

yunhou-users 新增 `internal/service/payment.go` 的 `resolveSubExpiry` 助手，三路优先级：

1. **hint**：BFF 在 Confirm body 里传 `expires_at` → 直接采用（最快路径，Stripe / Alipay / PayPal 一直就这么走）
2. **preservedExpiry**：retry 同一笔 payment 时保留旧值，避免 retry 把到期日往后推
3. **plan.interval_days fallback**：`time.Now() + plan.interval_days*24h`，对 WeChat v3 是兜底默认值

### BFF 接入策略

- **推荐**：BFF 拿到 `quote.sub_expires_at` 后继续在 Confirm body 里带上 `expires_at`。优点是到期日基于 quote 时刻确定，下游有可见的"何时起的 30 天"。
- **可省略**：如果 BFF 想简化，也可以不带。yunhou-users 会按 webhook 到达时刻计算，到期日会有几秒差异，对业务无影响。
- **不要试图自己计算**：BFF 不要重新从 `plan.interval_days` 算 sub_expires_at 传给 yunhou-users — 让 server-side 单一来源（`plan.interval_days` 来自 `plans` 表的 DB 真值），BFF 的 `quote.sub_expires_at` 已经是这个值。

### PayPal 续费（renewal）是例外

PayPal 的 webhook renewal path 用的是 `resource.billing_info.next_billing_time`（不是 `custom_data.sub_expires_at`），缺失时**不**走 fallback，而是写 audit log `paypal_renewal_no_expiry_hint` 并拒绝续期。这是**故意的**：renewal 应该来自 PayPal 自身的计费节奏，缺失说明产品定义 contract drift，需要 ops 介入。BFF 侧不需要改，但知道这条约定有用。

---

## 3. 迁移列表更新

yunhou-users 新增两个 migration：

- **016_plan_pricing_and_hide.sql**：`monthly` ¥19.9、`yearly` ¥199.9；`quarterly` 完全退役（`is_listed=false`、`is_active=false`）；`free` 从公开目录隐藏。**这条不需要 BFF 改动**（公开 catalog `GET /apps/:id/plans` 现在只返回 `monthly` + `yearly`）。
- **017_sub_expiry_does_not_backfill.sql**：no-op marker，记录 §1 的不回填决定。**BFF 不需要执行任何 SQL**，但运维部署顺序要带上。

部署命令（参考 `README.md`）：
```bash
psql -d yunhou_users -f migrations/016_plan_pricing_and_hide.sql
psql -d yunhou_users -f migrations/017_sub_expiry_does_not_backfill.sql
```

或用 `cmd/migrate` 二进制（推荐）：`make migrate`

---

## 4. 验证清单

部署 yunhou-users 后，BFF 团队请验证：

1. **新用户走 WeChat 完整支付流程**，完成后 `GET /user/subscriptions` 返回的 `expires_at` 是**未来 30 天**左右（不是 null，不是过去时间）
2. **老用户（修复前激活的 WeChat 订阅）**：`subscription.expires_at` 是 `null`，UI "永不过期" 分支正常工作
3. **代码里所有 `subscription?.expires_at` 检查**改成 `=== null` 判断而不是 `=== undefined`
4. **没装新代码的 yunhou-website**：用户登录后的 `subscription.expires_at` 字段会**突然变成 null**（之前是字段缺席），任何 TypeScript 类型上把它标成 `string` 的代码会编译失败或 runtime undefined

---

## 5. 改动文件一览（yunhou-users 侧）

仅供 BFF 团队理解整体范围，不必逐文件看：

- `internal/service/payment.go` — 新增 `resolveSubExpiry` 助手、wire 进 `onPaymentSucceeded` / `Confirm` / `buildReconcileWebhookEvent`；`activateSubscriptionOnTx` 的 `started_at` 改用 `COALESCE(started_at, now())`（retry 不再覆盖首次激活时间）
- `internal/service/auth.go` — `SubscriptionInfo.ExpiresAt` JSON tag 去掉 `omitempty`
- `internal/repo/repo.go` — 新增 `SubscriptionRepo.FindActiveByUserIDTx`（避免 tx 内连接池死锁）
- `internal/repo/payments_repos.go` — 新增 `PaymentRepo.FindByChannelTxnIDTx`（同上）
- `migrations/017_sub_expiry_does_not_backfill.sql` — no-op marker
- `docs/api-integration-guide.md` — §1 的 `expires_at` 契约说明、`sub_expires_at` 回退说明
- `README.md` / `CLAUDE.md` — 迁移列表更新到 017，webhook 描述加上 WeChat 例外说明
- `PROGRESS.md` — 2026-07-27 contract change 公告条目

---

## 6. 回滚预案

如果 BFF 端遇到兼容问题无法立刻上线新代码：

- yunhou-users 这边的 contract 变化**不能**回滚（不回滚等于把 WeChat 终身会员 bug 加回来）
- BFF 应该临时打 patch 处理 `null` → undefined 转换，或者快速发版适配新契约
- 不推荐在 yunhou-users 侧加 dual-mode（同时输出 `expires_at` 和 `expires_at_nullable`），会让接口长期不干净

---

发送方（yunhou-users）commit 链已就绪，待 push。push 后请知会 yunhou-website 部署团队按 §4 验证。