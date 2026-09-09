-- 030_inference_wallet.sql
-- Description: 预付按量钱包与显式套餐外消费（Kaya Coding Plan Task 14）
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §4.2–§4.3、§7.1–§7.3
--
-- 口径（控制者裁决 + 设计 §7.1/§7.3）:
--   - 金额一律带币种的定点整数微金额（BIGINT micros + ISO-4217 币种）；按币种
--     隔离钱包：同一账户每币种一个钱包。任何路径禁止 float 累计余额。
--   - 钱包没有缓存余额列：客户展示一律由 inference_wallet_entries 借贷分录
--     派生（credit − debit，按 source 分现金/赠送），冻结额由
--      inference_wallet_holds 的 held 行派生——禁止独立缓存余额。
--   - 冻结/释放是 hold 行的状态迁移，与额度预占 inference_reservations 同一
--     口径（预占不是账本行）；消费/退款/调整/冲正写账本行，全部带幂等
--     business_key（充值 wallet:topup:{payment_id}、消费
--     wallet:consume:{request_id}:{source}、退款 wallet:refund:{refund_id}、
--     调整 wallet:adjustment:{idempotency_key}、冲正 wallet:reversal:{entry_id}）。
--   - 分录区分现金(cash)/赠送(bonus)来源；退款只允许现金来源原路退
--     （CHECK 兜底：refund 必为 debit+cash），赠送余额不得伪装现金退款。
--   - 套餐外消费默认关：overage_enabled 默认 false；开启必须同时设置
--     UTC 自然月支出上限（CHECK 强制）；开启/关闭/改上限都写
--     inference_wallet_audits 审计行。
--   - PAYG（无套餐按量）：权益来源扩 'payg'，显式记录才授权；运营发布
--     的默认 PAYG 策略/模型集合存 inference_payg_config（单行）。
--   - 并发双花守卫：最后一份余额的并发扣减在 DB 层原子——所有钱包
--     变动先锁账户锚行再锁钱包行（固定锁序），冻结在锁内按派生余额
--     检查，不超扣。
--   - 幂等：全 IF NOT EXISTS / DROP IF EXISTS + duplicate_object 门控，重跑
--     为 no-op。

-- ---------------------------------------------------------------------------
-- 钱包主表：每账户每币种一行；无余额缓存列（余额 = 分录派生）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inference_wallets (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    currency           TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- 客户显式开启的套餐外/按量消费开关（默认关：套餐耗尽即停，不动余额）
    overage_enabled    BOOLEAN NOT NULL DEFAULT false,
    -- UTC 自然月支出上限（微金额）；开启时必填（CHECK 兜底）
    monthly_spend_limit_micros BIGINT
        CHECK (monthly_spend_limit_micros IS NULL OR monthly_spend_limit_micros >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (billing_account_id, currency),
    CHECK (NOT overage_enabled OR monthly_spend_limit_micros IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- 钱包冻结（预占）：一次请求至多一条；现金/赠送拆分在冻结时固定，
-- 结算/释放按此拆分回落；价格版本在冻结时钉住（调价不影响在途冻结）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inference_wallet_holds (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    wallet_id   UUID NOT NULL REFERENCES inference_wallets (id),
    -- 幂等业务键：一次请求至多一条钱包冻结
    request_id  UUID NOT NULL UNIQUE REFERENCES inference_requests (id),
    amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
    -- 赠送先扣的冻结拆分（cash + bonus = amount）
    cash_micros  BIGINT NOT NULL CHECK (cash_micros >= 0),
    bonus_micros BIGINT NOT NULL CHECK (bonus_micros >= 0),
    -- 入场钉住的 sale_money 价格版本（冗余自请求行，便于钱包侧审计）
    price_version_id UUID REFERENCES inference_price_versions (id),
    state TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'settled', 'released')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at  TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    CHECK (cash_micros + bonus_micros = amount_micros)
);

CREATE INDEX IF NOT EXISTS idx_inference_wallet_holds_wallet_held
    ON inference_wallet_holds (wallet_id) WHERE state = 'held';

-- ---------------------------------------------------------------------------
-- 钱包账本：追加 + 冲正，不 UPDATE 已入账分录
-- direction: credit=入（充值/赠送/调整贷方/冲正借方分录）；
--            debit=出（消费/退款/调整借方/冲正贷方分录）
-- source:    cash=现金（可退）/ bonus=赠送（不可现金退款）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inference_wallet_entries (
    id BIGSERIAL PRIMARY KEY,
    wallet_id  UUID NOT NULL REFERENCES inference_wallets (id),
    entry_type TEXT NOT NULL
               CHECK (entry_type IN ('topup', 'bonus', 'consume', 'refund', 'adjustment', 'reversal')),
    direction  TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    source     TEXT NOT NULL CHECK (source IN ('cash', 'bonus')),
    amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
    -- 冗余自钱包行：按币种隔离的核对在分录层自洽
    currency   TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- 消费分录关联请求与冻结；账户级分录（充值/退款/调整）为 NULL
    request_id UUID REFERENCES inference_requests (id),
    hold_id    UUID REFERENCES inference_wallet_holds (id),
    -- 跨域弱引用（支付域，设计 §3 模块边界）：充值/退款对账凭证
    payment_id TEXT,
    refund_id  TEXT,
    -- 运营调整关联 inference_adjustments（unit='micromoney' 行）
    adjustment_id UUID REFERENCES inference_adjustments (id),
    -- 冲正指向原分录；一条分录至多被冲正一次（下方部分唯一索引）
    reverses_entry_id BIGINT REFERENCES inference_wallet_entries (id),
    -- 幂等业务键：重复投递/回调撞唯一键
    business_key TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 类型-方向-来源一致性（DB 兜底；应用层 accounting.ValidateWalletEntry 同规则）
    CHECK (entry_type <> 'topup'   OR (direction = 'credit' AND source = 'cash')),
    CHECK (entry_type <> 'bonus'   OR (direction = 'credit' AND source = 'bonus')),
    CHECK (entry_type <> 'consume' OR direction = 'debit'),
    -- 退款只允许现金来源原路退：赠送余额不得伪装现金退款
    CHECK (entry_type <> 'refund'  OR (direction = 'debit' AND source = 'cash'))
);

-- 一条分录至多一条冲正
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_wallet_entries_reversal_once
    ON inference_wallet_entries (reverses_entry_id) WHERE reverses_entry_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_inference_wallet_entries_wallet
    ON inference_wallet_entries (wallet_id, id DESC);

-- ---------------------------------------------------------------------------
-- 钱包设置审计：开启/关闭套餐外、改支出上限、建钱包，全部留痕
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inference_wallet_audits (
    id BIGSERIAL PRIMARY KEY,
    wallet_id  UUID NOT NULL REFERENCES inference_wallets (id),
    action     TEXT NOT NULL
               CHECK (action IN ('create', 'enable_overage', 'disable_overage', 'set_spend_limit')),
    changed_by TEXT NOT NULL,
    old_overage_enabled BOOLEAN,
    new_overage_enabled BOOLEAN,
    old_monthly_spend_limit_micros BIGINT,
    new_monthly_spend_limit_micros BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_wallet_audits_wallet
    ON inference_wallet_audits (wallet_id, id DESC);

-- ---------------------------------------------------------------------------
-- PAYG 发布配置（单行）：运营发布的默认按量策略版本 + 显式模型集合。
-- 客户开启 PAYG 时按此配置发放 source_type='payg' 的显式权益记录；
-- 未配置 = 不可开启（没有配置的商品保持不可购买，设计 §4.3）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inference_payg_config (
    id TEXT PRIMARY KEY CHECK (id = 'default'),
    policy_version_id UUID NOT NULL REFERENCES inference_policy_versions (id),
    -- 显式模型集合（无 NULL 全放行语义；空数组 = 不授权任何模型）
    model_ids  TEXT[] NOT NULL,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 权益来源扩 'payg'（无套餐按量的显式权益记录）
-- ---------------------------------------------------------------------------
ALTER TABLE inference_entitlements DROP CONSTRAINT IF EXISTS inference_entitlements_source_type_check;
DO $$
BEGIN
    ALTER TABLE inference_entitlements
        ADD CONSTRAINT inference_entitlements_source_type_check
        CHECK (source_type IN ('subscription', 'order', 'grant', 'payg'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- ---------------------------------------------------------------------------
-- 请求行：入场固定扣费来源（首版不中途切换套餐/余额）
-- ---------------------------------------------------------------------------
ALTER TABLE inference_requests
    ADD COLUMN IF NOT EXISTS charge_source TEXT NOT NULL DEFAULT 'plan';
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'inference_requests_charge_source_check'
          AND conrelid = 'inference_requests'::regclass
    ) THEN
        ALTER TABLE inference_requests
            ADD CONSTRAINT inference_requests_charge_source_check
            CHECK (charge_source IN ('plan', 'wallet'));
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 预占目标扩 'wallet'：钱包冻结在 inference_reservations 镜像一行
-- （统一预占审计面与唯一键防重复预占）；wallet 目标不挂窗口/Key。
-- ---------------------------------------------------------------------------
ALTER TABLE inference_reservations DROP CONSTRAINT IF EXISTS inference_reservations_target_kind_check;
ALTER TABLE inference_reservations DROP CONSTRAINT IF EXISTS inference_reservations_check;
ALTER TABLE inference_reservations DROP CONSTRAINT IF EXISTS inference_reservations_check1;
DO $$
BEGIN
    ALTER TABLE inference_reservations
        ADD CONSTRAINT inference_reservations_target_kind_check
        CHECK (target_kind IN ('window_five_hour', 'window_weekly', 'window_monthly', 'key_budget', 'wallet'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE inference_reservations
        ADD CONSTRAINT inference_reservations_target_refs_check
        CHECK (
            (target_kind IN ('window_five_hour', 'window_weekly', 'window_monthly')
             AND window_id IS NOT NULL AND api_key_id IS NULL)
            OR (target_kind = 'key_budget'
             AND api_key_id IS NOT NULL AND window_id IS NULL)
            OR (target_kind = 'wallet'
             AND window_id IS NULL AND api_key_id IS NULL)
        );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
