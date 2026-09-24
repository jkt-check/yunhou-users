# Migrations

This directory holds the SQL files that the `cmd/migrate` binary applies
in lexicographic filename order. Each successfully applied file is
recorded in the `_migrations` ledger table inside the same transaction
as its SQL — re-running `make migrate` on a clean DB is a no-op.

## Naming

- Format: `NNN_short_description.sql` where `NNN` is a zero-padded
  sequence number. Example: `001_init.sql`, `009_add_user_locale.sql`.
- Numbers don't need to be contiguous — leave gaps so a hot-fix can
  slip in between planned ones without renumbering.
- Once a migration file has been deployed to **any** environment, **do
  not edit it**. Fix forward with a new file. The ledger keeps the
  original's id recorded, so an in-place edit would diverge from what
  every running database actually has.

## Idempotent DDL — required

The cmd/migrate binary wraps each file in a single transaction, but it
**does not** retry a failed migration. If `001_init.sql` ever runs twice
on the same database the non-idempotent DDL will fail on the second
run. The fix is the same one you'd use for any hand-applied migration:

| Operation | Idempotent form |
|---|---|
| Create table | `CREATE TABLE IF NOT EXISTS ...` |
| Add column | `ALTER TABLE ... ADD COLUMN IF NOT EXISTS ...` |
| Drop constraint | `ALTER TABLE ... DROP CONSTRAINT IF EXISTS ...` |
| Add constraint | wrap in `DO $$ ... EXCEPTION WHEN duplicate_object THEN NULL; END $$;` |
| Create index | `CREATE INDEX IF NOT EXISTS ...` |
| Insert seed | `INSERT ... ON CONFLICT DO NOTHING` |

The integration test `TestApply_RealMigrationsFromRepo` (in
`internal/migrate/migrate_test.go`) loads this directory and runs every
file against a fresh database; a non-idempotent DDL anywhere here will
break that test, which is the safety net the deploy script depends on.

## Transaction control statements

Don't put `BEGIN` / `COMMIT` / `ROLLBACK` at the top level of a
migration file. The cmd/migrate binary already wraps each file in its
own transaction — adding your own breaks the nested-tx model (PG has
no real nested transactions; the inner commit silently ends the outer
tx and the ledger INSERT then fails). The `DO $$ BEGIN ... EXCEPTION
... END $$;` blocks used in 005_paypal_channel.sql are PL/pgSQL block
syntax, **not** transaction control — those are fine.

## Files

| File | Purpose |
|---|---|
| `001_init.sql` | Core users / identities / apps / plans / subscriptions tables |
| `002_simplify_plans.sql` | Plan has apps[]; seeded free/monthly/quarterly/yearly rows (default-plan concept later dropped by 014) |
| `003_payments.sql` | orders / payments / refunds / webhook_events / audit_log |
| `004_ls_channel.sql` | (historical) added lemonsqueezy to channel CHECK — kept for installs that ran it before 008 |
| `005_paypal_channel.sql` | extends channel CHECK to include 'paypal' |
| `006_paypal_sub_mapping.sql` | subscriptions.external_subscription_id for PayPal renewals |
| `007_app_secret.sql` | apps.secret_hash column (backfilled by server startup) |
| `008_drop_lemonsqueezy.sql` | removes lemonsqueezy from channel CHECK (LS code was removed in d8f333d) |
| `009_wechat_pay_intent.sql` | adds orders.provider_intent JSONB for wechat_pay pre-auth metadata |
| `010_provider_intent_nullable.sql` | change provider_intent default from `'{}'` to NULL so omitempty works |
| `011_order_reconcile.sql` | adds orders.last_reconciled_at (defaulted to `now()`, indexed) so GetOrder's wechat_pay QueryOrder reconcile path can throttle re-polls |
| `012_plan_commercial_fields.sql` | commercializes the plans surface: `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, `description`, `display_order`, `updated_at` (trigger-maintained) + new `plan_change_log` audit table |
| `013_plan_change_log_fk_set_null.sql` | relaxes `plan_change_log.plan_id` FK to `ON DELETE SET NULL` so hard-deleted plans keep their audit history; pairs with `PlanService.DeletePlan` reorder to INSERT-then-DELETE |
| `014_remove_default_plan.sql` | retires the default-plan concept: marks `free` inactive, drops `plans_one_default` partial unique index, drops `plans.is_default` column (gated on no active `free` subscriptions) |
| `015_plan_change_log_nullable_snapshots.sql` | plan_change_log.before / .after become nullable so CreatePlan (`before=NULL`) and DeletePlan (`after=NULL`) can write audit rows (spec §6.1) |
| `016_plan_pricing_and_hide.sql` | re-prices `monthly` (¥19.9/mo) and `yearly` (¥199.9/yr) to match the yunhou-website frontend promo; fully retires `quarterly` (`is_listed=false`, `is_active=false`) and hides `free` from the public catalog |
| `017_sub_expiry_does_not_backfill.sql` | no-op marker documenting the decision NOT to backfill `subscriptions.expires_at = NULL` rows from the pre-2026-07-27 WeChat NATIVE v3 bug (see `docs/superpowers/plans/2026-07-27-subscription-expiry-fallback.md` for rationale) |
| `018_trial_plan.sql` | adds the `trial` plan row (active, not purchasable, not listed, trial_days=7) backing `AuthService.grantTrialSubscription` — 7-day free trial granted at first login |
| `019_kaya_bridge.sql` | kaya desktop OAuth (yunhou-terminal round-137): appends `yunhou-website` to full-feature plans' `apps[]` (has_access requires plan.Apps contains the appID), and appends the staging+prod `…/auth/kaya-bridge` URLs to `yunhou-website`'s wechat `callback_urls` (https bridge page → OS scheme bounce; both idempotent, no-op when app row absent) |
| `020_refresh_reuse_grace.sql` | sessions gains `revoked_at` + `rotated_to` (self-FK) so `AuthService.RefreshToken` can distinguish a lost-response retry (retry with the old token within the grace window → follow `rotated_to` and rotate the successor) from a real token replay (outside the window → chain revoke + 401; chain-scoped since 2026-09-16, previously family-wide) |
| `021_usage_events.sql` | usage-analytics 心跳流水表 `(user_id, client_event_id)` 幂等键 + `(user_id, local_date)` / `(local_date)` 索引;支撑 `POST /user/usage/heartbeat` 与 `/admin/stats/*`(DAU/WAU/MAU、时长、新增用户;设计文档在 yunhou-terminal 仓库 `docs/superpowers/specs/2026-09-04-usage-analytics-design.md`) |
| `024_inference_catalog.sql` | inference 模型目录表组：models/providers/deployments/model_routes/config_revisions（每 scope 一个 active 发布的部分唯一索引）。022/023 保留给未合入的 feat/multi-model-gateway 分支，不得占用 |
| `025_inference_accounts.sql` | inference 账号表组：credentials（AEAD 密文 + key_version + generation）/upstream_accounts（含上游额度缓存）/billing_accounts（每用户一个，不级联删除）/api_keys（前缀+摘要，预算） |
| `026_inference_accounting.sql` | inference 计量/账本表组：price_versions/policy_versions/entitlements/requests/attempts/usage_records/quota_windows（区间不重叠 EXCLUDE）/reservations/concurrency_leases/ledger_entries（每请求一条 charge 的部分唯一索引）/adjustments/outbox/reconciliation_jobs；全部整数微额度/微金额，账本不对用户级联删除 |
| `027_subscription_product_scope.sql` | 套餐/订阅增加 `product_code`（默认回填 `kaya-membership`）；002 的每用户全局活跃唯一索引替换为 `(user_id, product_code)` 部分唯一索引；迁移前 DO 块对同 (user,product) 多活跃订阅输出可读诊断并中止；触发器保证订阅 product_code 与套餐一致 |
| `028_operator_permissions.sql` | inference 运营授权表组：operator_roles（角色→权限映射）+ inference_audit_log（递归脱敏审计）（Task 4） |
| `029_order_benefit_snapshot.sql` | 下单权益快照：orders 增加 product_code/plan_interval_days/benefit_policy_version_id/benefit_model_ids/benefit_grant_mode/order_kind/upgrade_from_plan_id（支付回调只按快照兑现；存量订单按 plan 当前值回填）；新增 plan_benefit_configs（套餐→权益版本发布配置，无配置的商品不可购买）与 plan_upgrade_rules（跨档升级显式规则）（Task 10） |
| `030_inference_wallet.sql` | 预付按量钱包：wallets（每账户每币种一行、无缓存余额列、套餐外开关+UTC 自然月支出上限）/wallet_entries（借贷分录追加+冲正、cash/bonus 来源隔离、refund 仅 cash 的 CHECK、幂等 business_key、一条分录至多一条冲正）/wallet_holds（每请求一条冻结、赠送先扣拆分、价格版本钉住）/wallet_audits（开关与上限修改审计）/payg_config（单行发布配置）；权益来源扩 'payg'、请求行增 charge_source、预占目标扩 'wallet'（Task 14；030 为 Task 0 预留号） |
| `031_inference_settlement_recovery.sql` | 幂等结算与崩溃恢复：charge 允许零额（零消费恒落行）、reconciliation reason 增 settlement_overage、窗口级任务部分唯一索引、请求行持久化准入上界（Task 9；030 起空号经控制者裁决，后由 Task 14 占用 030） |
| `032_inference_oauth_sessions.sql` | inference OAuth 授权状态（一次性 state + PKCE，独立于社交登录）与粘性会话绑定表组（Task 12；控制者裁决确需 DDL 从 032 起） |
| `033_inference_response_chains.sql` | OpenAI Responses 会话链持久化：previous_response_id 接续的 transcript 回放 + 账户/上游账号归属（Task 13） |
| `034_inference_operations.sql` | 运营面支撑：inference_bulk_imports 批量导入幂等任务表（commit 与目录写入同事务、重复提交重放已记录结果）+ requests/attempts 的 created_at 范围扫描索引（运营统计/异常筛选的真实查询计划；只读派生路径，不引入外部分析库）（Task 15） |
| `035_lease_indexes_reaper.sql` | 租约热路径索引与收割支撑：concurrency_leases 增 (scope, scope_id, fencing_token DESC) 索引（MAX(fencing) 不再扫 scope 全历史）与终态行部分索引（DeleteTerminalLeases 收割 released/expired 且早于 7 天保留窗口的行，挂 upstream_health 轮次）；wallet_entries 增 (wallet_id, created_at) 索引（支撑月度支出派生）（评审修复批次8） |
| `036_adjustments_source.sql` | inference_adjustments 增 source 列（cash|bonus，CHECK 约束）：幂等重放载荷比对纳入资金路由来源，同键异 source → 409；micromoney 存量行经 adjustment_id 联 wallet_entries 的 adjustment 分录回填真实来源，microcredit 行无现金/赠送概念保持 NULL（评审轮2 C-I1） |
| `037_admin_idempotency.sql` | dashboard 运营 admin API 幂等键表 admin_idempotency_keys（UNIQUE(app_id,key)，撞键重放首次 response；与 VIP 订阅变更 + audit_log 同事务提交，只记录成功） |
