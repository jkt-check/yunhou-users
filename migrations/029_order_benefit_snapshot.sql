-- Migration: 029_order_benefit_snapshot
-- Description: 下单权益快照 + 套餐权益配置 + 跨档升级规则（Kaya Coding Plan Task 10）
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §4.1–§4.3、§7.2
--
-- 口径:
--   - 下单即冻结：订单落库时把产品归属、计费周期、套餐权益版本（配额策略版本 +
--     显式模型集合）、发放形态与升级规则结果快照到 orders 行。支付回调/webhook
--     只按订单快照兑现，不再读可能被运营改写的 plans / plan_benefit_configs
--     当前值（调价、撤售、改配额不影响已下单的订单）。
--   - orders.product_code / plan_interval_days 为 NULL 的是 029 前的历史订单：
--     回调路径对它们保持旧的"读 plan 当前行"行为，逐字保留。存量行由本迁移
--     按 plan 当前值一次性回填（等价于旧路径当时会读到的值，行为不变）。
--   - 跨域引用用弱引用（设计 §3 模块边界）：orders.benefit_policy_version_id 与
--     orders.upgrade_from_plan_id 不加 FK（与 003 orders/payments 的风格一致）；
--     plan_benefit_configs 是商品域到 inference 域的绑定表，其引用加 FK 防止
--     运营配出悬空指针（policy version 不可变、只退休不删除，RESTRICT 无副作用）。
--   - 幂等：全部 ADD COLUMN IF NOT EXISTS / CREATE TABLE IF NOT EXISTS /
--     pg_constraint 门控的 DO 块；回填 UPDATE 以 product_code IS NULL 为哨兵。

-- ============================================================================
-- 1. orders —— 权益快照列
-- ============================================================================
-- product_code        : 商业产品归属快照（kaya-membership / coding-plan / ...）
-- plan_interval_days  : 计费周期快照（回调的 expires_at 推导不再读 plan 当前值）
-- benefit_policy_version_id / benefit_model_ids : 套餐权益版本快照
--                       （inference_policy_versions 是不可变发布版本 + 显式模型集合）
-- benefit_grant_mode  : 发放形态快照：subscription=显式套餐权益（coding-plan 商品），
--                       gift=捆绑赠送权益（如会员捆绑包，默认不叠加，设计 §4.2）
-- order_kind          : 下单时判定的订单种类：new=新购 / renewal=同套餐续费 /
--                       upgrade=已配置规则的跨档升级；仅 coding-plan 订单填写，
--                       kaya 订单保持 NULL（旧 IntervalDays 比较规则原样保留）
-- upgrade_from_plan_id: order_kind='upgrade' 时下单时固化的"被升级"套餐
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS product_code              TEXT,
    ADD COLUMN IF NOT EXISTS plan_interval_days        INT,
    ADD COLUMN IF NOT EXISTS benefit_policy_version_id UUID,
    ADD COLUMN IF NOT EXISTS benefit_model_ids         TEXT[],
    ADD COLUMN IF NOT EXISTS benefit_grant_mode        TEXT,
    ADD COLUMN IF NOT EXISTS order_kind                TEXT,
    ADD COLUMN IF NOT EXISTS upgrade_from_plan_id      TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'orders_benefit_grant_mode_check' AND conrelid = 'orders'::regclass
    ) THEN
        ALTER TABLE orders ADD CONSTRAINT orders_benefit_grant_mode_check
            CHECK (benefit_grant_mode IS NULL OR benefit_grant_mode IN ('subscription', 'gift'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'orders_order_kind_check' AND conrelid = 'orders'::regclass
    ) THEN
        ALTER TABLE orders ADD CONSTRAINT orders_order_kind_check
            CHECK (order_kind IS NULL OR order_kind IN ('new', 'renewal', 'upgrade'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'orders_benefit_snapshot_consistent' AND conrelid = 'orders'::regclass
    ) THEN
        -- 快照要么整组为空（无权益映射的商品 / 029 前历史单），要么成组完整：
        -- 有策略版本必有发放形态与显式模型集合（空数组 = 不授权任何模型，
        -- 但列本身不得 NULL，杜绝 NULL-means-all 的歧义）。
        ALTER TABLE orders ADD CONSTRAINT orders_benefit_snapshot_consistent
            CHECK (
                (benefit_policy_version_id IS NULL AND benefit_grant_mode IS NULL)
                OR
                (benefit_policy_version_id IS NOT NULL AND benefit_grant_mode IS NOT NULL
                 AND benefit_model_ids IS NOT NULL)
            );
    END IF;
END $$;

-- 存量订单回填：按 plan 当前值冻结产品归属与周期。027 已保证 plans 行存在
-- （orders.plan_id FK RESTRICT），join 不会丢行。哨兵 product_code IS NULL
-- 使重跑成为 no-op。
UPDATE orders o
   SET product_code = p.product_code,
       plan_interval_days = p.interval_days
  FROM plans p
 WHERE o.plan_id = p.id
   AND o.product_code IS NULL;

-- 注：product_code / plan_interval_days 不加 NOT NULL 约束——合成续费订单
-- （PayPal renewal，无下单时刻）与历史测试夹具允许留空；029 起新订单由
-- 服务层 CreateOrder 强制填写（空值 = 历史单，回调走旧的 live-read 兼容路径）。

-- ============================================================================
-- 2. plan_benefit_configs —— 套餐 → 权益版本的发布配置（商品的"支付配置"）
-- ============================================================================
-- 没有配置的商品不可购买（设计 §4.3：没有配置的商品保持草稿或不可购买）：
-- coding-plan 商品的套餐必须有一行 grant_mode='subscription' 的配置才能下单；
-- kaya-membership 套餐可选配置 grant_mode='gift' 行，即捆绑赠送（bundle）——
-- 支付成功后按订单快照显式发放一份赠送权益，默认不与显式套餐叠加。
CREATE TABLE IF NOT EXISTS plan_benefit_configs (
    plan_id           TEXT PRIMARY KEY REFERENCES plans (id) ON DELETE RESTRICT,
    policy_version_id UUID NOT NULL REFERENCES inference_policy_versions (id),
    -- 显式模型集合（设计 §4.2：无 NULL 全放行语义；空数组 = 不授权任何模型）
    model_ids         TEXT[] NOT NULL,
    grant_mode        TEXT NOT NULL DEFAULT 'subscription'
                      CHECK (grant_mode IN ('subscription', 'gift')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================================
-- 3. plan_upgrade_rules —— 跨档升级的显式配置（设计 §4.2）
-- ============================================================================
-- 商品未配置升级报价规则时不开放即时跨档升级。Coding Plan 的跨档订单必须存在
-- (from_plan_id, to_plan_id) 规则行，否则下单拒绝；同套餐续费不需要规则行。
-- kaya-membership 不使用本表（旧的"周期更长即可升级"规则仅保留给原会员商品）。
CREATE TABLE IF NOT EXISTS plan_upgrade_rules (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_plan_id TEXT NOT NULL REFERENCES plans (id) ON DELETE RESTRICT,
    to_plan_id   TEXT NOT NULL REFERENCES plans (id) ON DELETE RESTRICT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_plan_id <> to_plan_id),
    UNIQUE (from_plan_id, to_plan_id)
);
