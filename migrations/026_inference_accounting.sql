-- 026_inference_accounting.sql
-- Description: inference 模块计量/配额/账本表组 — 价格版本/配额策略/权益/请求/尝试/
--   计量事实/配额窗口/预占/并发租约/账本/调整/outbox/核对任务
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §6、§7.1–§7.3
--
-- 口径:
--   - 额度一律整数微额度（microcredit，BIGINT）；金额一律带币种的定点微金额
--     （BIGINT micros + CHAR(3) 币种）；任何路径禁止 float（设计 §7.1）。
--   - 账本追加 + 冲正，不 UPDATE 已结算事实；账本/计量相关表一律不对
--     用户/计费账户级联删除（去标识化策略由后续任务实现）。
--   - 请求/尝试/结算的唯一键是本文件的核心约束（设计 §7.2：幂等防重复结算）。
--   - 窗口区间 [window_start, window_end)，到边界属新窗口；同权益同 kind
--     的活跃窗口不得重叠（EXCLUDE 约束保证）。

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ---------------------------------------------------------------------------
-- 价格与策略（不可变版本，设计 §5/§7.1）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inference_price_versions (
    id       UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    model_id TEXT NOT NULL REFERENCES inference_models (id),
    -- sale_credit = 套餐额度消耗表（microcredit）；
    -- sale_money  = 按量售价（带币种微金额）；
    -- upstream_cost = 采购成本（带币种微金额）
    kind     TEXT NOT NULL CHECK (kind IN ('sale_credit', 'sale_money', 'upstream_cost')),
    -- 微单位/百万 token；额度口径 unit='microcredit' 且 currency 为空，
    -- 金额口径 unit='micromoney' 且 currency 必填 ISO-4217 大写三字母
    unit     TEXT NOT NULL CHECK (unit IN ('microcredit', 'micromoney')),
    currency TEXT CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    input_micros_per_mtok       BIGINT NOT NULL DEFAULT 0 CHECK (input_micros_per_mtok >= 0),
    cache_read_micros_per_mtok  BIGINT NOT NULL DEFAULT 0 CHECK (cache_read_micros_per_mtok >= 0),
    cache_write_micros_per_mtok BIGINT NOT NULL DEFAULT 0 CHECK (cache_write_micros_per_mtok >= 0),
    output_micros_per_mtok      BIGINT NOT NULL DEFAULT 0 CHECK (output_micros_per_mtok >= 0),
    -- 其他可计费项目（工具等）的扩展价目，带 schema 版本：{"schema_version":1,...}
    extra_rates JSONB NOT NULL DEFAULT '{"schema_version":1}',
    revision      INT NOT NULL CHECK (revision > 0),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, kind, revision),
    CHECK ((unit = 'microcredit') = (currency IS NULL)),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_inference_price_versions_model
    ON inference_price_versions (model_id, kind, effective_from);

CREATE TABLE IF NOT EXISTS inference_policy_versions (
    id   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name TEXT NOT NULL,
    revision INT NOT NULL CHECK (revision > 0),
    -- 显式模型集合（设计 §4.2：新模型默认无授权，不存在 NULL 全放行语义）
    model_ids TEXT[] NOT NULL,
    -- 三个窗口限额（microcredit）；NULL = 该窗口禁用（显式标识，≠ 无限）
    five_hour_limit_micros BIGINT CHECK (five_hour_limit_micros IS NULL OR five_hour_limit_micros >= 0),
    weekly_limit_micros    BIGINT CHECK (weekly_limit_micros IS NULL OR weekly_limit_micros >= 0),
    monthly_limit_micros   BIGINT CHECK (monthly_limit_micros IS NULL OR monthly_limit_micros >= 0),
    rpm_limit         INT CHECK (rpm_limit IS NULL OR rpm_limit > 0),
    tpm_limit         BIGINT CHECK (tpm_limit IS NULL OR tpm_limit > 0),
    concurrency_limit INT CHECK (concurrency_limit IS NULL OR concurrency_limit > 0),
    -- 超额策略：reject（默认停止）/ clamp_if_declared（客户端显式声明允许
    -- 缩减输出上限时按剩余额度下调，设计 §7.2）/ allow_overage（Task 14）
    overage_policy TEXT NOT NULL DEFAULT 'reject'
                   CHECK (overage_policy IN ('reject', 'clamp_if_declared', 'allow_overage')),
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    UNIQUE (name, revision)
);

-- 权益：调用授权来源（设计 §4.2）
CREATE TABLE IF NOT EXISTS inference_entitlements (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    -- 来源订阅/订单/赠送规则；跨域对象用弱引用（模块边界，设计 §3）
    source_type TEXT NOT NULL CHECK (source_type IN ('subscription', 'order', 'grant')),
    source_id   TEXT NOT NULL,
    -- 显式模型集合；空数组 = 不授权任何模型
    model_ids  TEXT[] NOT NULL,
    policy_version_id UUID NOT NULL REFERENCES inference_policy_versions (id),
    -- 权益原始生效锚点：weekly/monthly 窗口以此推进，升级/续费不改变它
    anchor_at       TIMESTAMPTZ NOT NULL,
    effective_from  TIMESTAMPTZ NOT NULL,
    effective_to    TIMESTAMPTZ,
    -- 权益修订版本（升级不清空已用额度，只升 revision）
    revision INT NOT NULL DEFAULT 1 CHECK (revision > 0),
    -- 赠送权益默认不与显式套餐叠加（设计 §4.2）
    stackable BOOLEAN NOT NULL DEFAULT false,
    status    TEXT NOT NULL DEFAULT 'active'
              CHECK (status IN ('active', 'expired', 'revoked', 'superseded')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_id, revision),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_inference_entitlements_account
    ON inference_entitlements (billing_account_id, status);

-- ---------------------------------------------------------------------------
-- 逻辑请求 / 上游尝试 / 计量事实（设计 §7.2）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inference_requests (
    -- request_id：一次逻辑调用；由网关在鉴权后生成，重复投递撞唯一键
    id                 UUID PRIMARY KEY,
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    api_key_id         UUID REFERENCES inference_api_keys (id),
    entitlement_id     UUID NOT NULL REFERENCES inference_entitlements (id),
    model_id           TEXT NOT NULL REFERENCES inference_models (id),
    protocol           TEXT NOT NULL
                       CHECK (protocol IN ('openai_chat', 'openai_responses', 'anthropic_messages', 'kaya_chat')),
    stream             BOOLEAN NOT NULL DEFAULT false,
    status             TEXT NOT NULL DEFAULT 'authenticated'
                       CHECK (status IN ('authenticated', 'reserved', 'dispatching',
                                         'streaming', 'non_streaming', 'settling',
                                         'settled', 'released', 'reconciliation_required',
                                         'failed')),
    -- admitted_at：预占成功时刻；窗口与价格按它绑定，跨重置时刻不迁移（§6）
    admitted_at        TIMESTAMPTZ,
    -- 固定 price/policy revision（新价格只影响生效后的请求）
    price_version_id   UUID REFERENCES inference_price_versions (id),
    policy_version_id  UUID NOT NULL REFERENCES inference_policy_versions (id),
    -- 入场时绑定的三个窗口（窗口在预占同事务创建，故可空）
    window_five_hour_id UUID,
    window_weekly_id    UUID,
    window_monthly_id   UUID,
    -- 预占/结算额度（microcredit）
    reserved_micros BIGINT CHECK (reserved_micros IS NULL OR reserved_micros >= 0),
    settled_micros  BIGINT CHECK (settled_micros IS NULL OR settled_micros >= 0),
    -- 计量来源状态：pending → reported/estimated/unknown（§7.1，不记零）
    usage_status TEXT NOT NULL DEFAULT 'pending'
                 CHECK (usage_status IN ('pending', 'reported', 'estimated', 'unknown')),
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

-- 客户用量分页：按账户/模型/Key 分组与时间倒序（设计 §9.2 /user/model-usage）
CREATE INDEX IF NOT EXISTS idx_inference_requests_account_time
    ON inference_requests (billing_account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inference_requests_model_time
    ON inference_requests (model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inference_requests_key_time
    ON inference_requests (api_key_id, created_at DESC) WHERE api_key_id IS NOT NULL;
-- 恢复任务扫描：未终态请求
CREATE INDEX IF NOT EXISTS idx_inference_requests_open
    ON inference_requests (status, created_at)
    WHERE status IN ('reserved', 'dispatching', 'streaming', 'non_streaming', 'settling', 'reconciliation_required');

CREATE TABLE IF NOT EXISTS inference_attempts (
    -- attempt_id：每次上游尝试独立 ID
    id          UUID PRIMARY KEY,
    request_id  UUID NOT NULL REFERENCES inference_requests (id),
    attempt_no  INT NOT NULL CHECK (attempt_no > 0),
    deployment_id       UUID REFERENCES inference_deployments (id),
    upstream_account_id UUID REFERENCES inference_upstream_accounts (id),
    status      TEXT NOT NULL DEFAULT 'dispatching'
                CHECK (status IN ('dispatching', 'streaming', 'completed',
                                  'failed', 'cancelled', 'unknown')),
    -- 上游请求 ID（用于按供应商能力逐笔核对，设计 §7.2）
    upstream_request_id TEXT NOT NULL DEFAULT '',
    -- 上游成本（微金额 + 币种）；cost_basis 区分报告/估算/分摊（§7.1）
    cost_micros   BIGINT CHECK (cost_micros IS NULL OR cost_micros >= 0),
    cost_currency TEXT CHECK (cost_currency IS NULL OR cost_currency ~ '^[A-Z]{3}$'),
    cost_basis    TEXT CHECK (cost_basis IS NULL OR cost_basis IN ('reported', 'estimated', 'allocated')),
    error_kind TEXT NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, attempt_no),
    CHECK ((cost_micros IS NULL) = (cost_currency IS NULL)),
    CHECK (cost_micros IS NULL OR cost_basis IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_inference_attempts_request
    ON inference_attempts (request_id);

CREATE TABLE IF NOT EXISTS inference_usage_records (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    request_id UUID NOT NULL REFERENCES inference_requests (id),
    attempt_id UUID NOT NULL REFERENCES inference_attempts (id),
    -- 计量来源（设计 §7.1：reported/estimated/unknown；缺失不记 0 → 全列 NULL）
    source  TEXT NOT NULL CHECK (source IN ('reported', 'estimated', 'unknown')),
    -- 规范化用量桶（token）；重叠语义由适配器归一（推理 token 不重复加算）。
    -- source='unknown' 时全部为 NULL；NULL ≠ 0。
    input_tokens       BIGINT CHECK (input_tokens IS NULL OR input_tokens >= 0),
    cache_read_tokens  BIGINT CHECK (cache_read_tokens IS NULL OR cache_read_tokens >= 0),
    cache_write_tokens BIGINT CHECK (cache_write_tokens IS NULL OR cache_write_tokens >= 0),
    output_tokens      BIGINT CHECK (output_tokens IS NULL OR output_tokens >= 0),
    reasoning_tokens   BIGINT CHECK (reasoning_tokens IS NULL OR reasoning_tokens >= 0),
    -- 原始 usage 结构（带 schema 版本），不存完整提示词/回答（§7.1）
    raw_usage JSONB NOT NULL DEFAULT '{"schema_version":1}',
    -- usage revision：同一 attempt 的修正记录递增，保留历史（§7.2 冲正）
    revision    INT NOT NULL DEFAULT 1 CHECK (revision > 0),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (attempt_id, revision),
    CHECK (source <> 'unknown' OR input_tokens IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_inference_usage_records_request
    ON inference_usage_records (request_id);

-- ---------------------------------------------------------------------------
-- 配额闸门：窗口 / 预占 / 并发租约（设计 §6、§7.2）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inference_quota_windows (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 窗口属于消费主体（权益）；升级沿用同一权益的窗口，不清 used/reserved
    entitlement_id UUID NOT NULL REFERENCES inference_entitlements (id),
    kind         TEXT NOT NULL CHECK (kind IN ('five_hour', 'weekly', 'monthly')),
    window_start TIMESTAMPTZ NOT NULL,
    window_end   TIMESTAMPTZ NOT NULL,
    limit_micros    BIGINT NOT NULL CHECK (limit_micros >= 0),
    used_micros     BIGINT NOT NULL DEFAULT 0 CHECK (used_micros >= 0),
    reserved_micros BIGINT NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
    -- voided：五小时窗口"全部请求确认无消费"时事务内撤销（设计 §6）；
    -- 被撤销窗口不参与重叠排他，但行保留可查
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'voided')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (window_end > window_start),
    CHECK (used_micros + reserved_micros >= 0)
);

-- 同权益同 kind 的活跃窗口区间不得重叠（[start, end) 语义）
DO $$
BEGIN
    ALTER TABLE inference_quota_windows
        ADD CONSTRAINT inference_quota_windows_no_overlap
        EXCLUDE USING gist (
            entitlement_id WITH =,
            kind WITH =,
            tstzrange(window_start, window_end) WITH &&
        ) WHERE (state = 'active');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- 首次消费并发创建窗口时以此去重
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_quota_windows_unique
    ON inference_quota_windows (entitlement_id, kind, window_start);

CREATE INDEX IF NOT EXISTS idx_inference_quota_windows_lookup
    ON inference_quota_windows (entitlement_id, kind, window_end) WHERE state = 'active';

-- 请求 → 窗口 FK 后置（quota_windows 在 requests 之后创建；窗口在预占同事务创建）
DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_window_five_hour_fk
        FOREIGN KEY (window_five_hour_id) REFERENCES inference_quota_windows (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_window_weekly_fk
        FOREIGN KEY (window_weekly_id) REFERENCES inference_quota_windows (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_window_monthly_fk
        FOREIGN KEY (window_monthly_id) REFERENCES inference_quota_windows (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS inference_reservations (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    request_id UUID NOT NULL REFERENCES inference_requests (id),
    -- 预占对象：三个窗口之一，或 Key 预算；同一事务内全部成功才提交（§7.2）
    target_kind TEXT NOT NULL
                CHECK (target_kind IN ('window_five_hour', 'window_weekly', 'window_monthly', 'key_budget')),
    window_id  UUID REFERENCES inference_quota_windows (id),
    api_key_id UUID REFERENCES inference_api_keys (id),
    amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
    state TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'settled', 'released')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at  TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    -- 一次请求对每类对象至多一条预占；重复预占撞唯一键
    UNIQUE (request_id, target_kind),
    CHECK ((target_kind = 'key_budget') = (api_key_id IS NOT NULL)),
    CHECK ((target_kind = 'key_budget') = (window_id IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_inference_reservations_window
    ON inference_reservations (window_id) WHERE state = 'held';
CREATE INDEX IF NOT EXISTS idx_inference_reservations_held
    ON inference_reservations (state, created_at) WHERE state = 'held';

CREATE TABLE IF NOT EXISTS inference_concurrency_leases (
    id       UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 协调范围：客户计费账户 / 上游账号 / 部署 / 客户 Key
    scope    TEXT NOT NULL CHECK (scope IN ('billing_account', 'upstream_account', 'deployment', 'api_key')),
    scope_id UUID NOT NULL,
    request_id UUID NOT NULL REFERENCES inference_requests (id),
    -- 所有权与 fencing token：超时回收不得与仍活跃请求重叠授权（Task 7）
    owner_token   TEXT NOT NULL,
    fencing_token BIGINT NOT NULL CHECK (fencing_token > 0),
    state      TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'released', 'expired')),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    UNIQUE (scope, scope_id, request_id),
    CHECK (expires_at > acquired_at)
);

CREATE INDEX IF NOT EXISTS idx_inference_concurrency_leases_scope
    ON inference_concurrency_leases (scope, scope_id, state) WHERE state = 'held';

-- ---------------------------------------------------------------------------
-- 客户账本 / 调整（追加 + 冲正，不重写已结算事实；设计 §7.3）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inference_ledger_entries (
    -- BIGSERIAL 提供全局追加顺序
    id BIGSERIAL PRIMARY KEY,
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    -- 请求级分录关联请求；账户级调整（无请求）为 NULL
    request_id UUID REFERENCES inference_requests (id),
    -- charge=结算扣额度；reversal=冲正；adjustment=运营补偿（关联 adjustments）
    entry_type TEXT NOT NULL CHECK (entry_type IN ('charge', 'reversal', 'adjustment')),
    amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
    unit     TEXT NOT NULL DEFAULT 'microcredit' CHECK (unit IN ('microcredit', 'micromoney')),
    currency TEXT CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    price_version_id UUID REFERENCES inference_price_versions (id),
    -- 冲正/补偿指向原分录
    reverses_entry_id BIGINT REFERENCES inference_ledger_entries (id),
    adjustment_id     UUID,
    created_by TEXT NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((unit = 'microcredit') = (currency IS NULL))
);

-- 结算唯一键：同一请求至多一条 charge（重复结算撞唯一键，设计 §7.2）
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_ledger_charge_once
    ON inference_ledger_entries (request_id) WHERE entry_type = 'charge';

CREATE INDEX IF NOT EXISTS idx_inference_ledger_account_time
    ON inference_ledger_entries (billing_account_id, id DESC);

CREATE TABLE IF NOT EXISTS inference_adjustments (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    request_id UUID REFERENCES inference_requests (id),
    reason   TEXT NOT NULL,
    amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
    direction TEXT NOT NULL CHECK (direction IN ('credit', 'debit')),
    unit     TEXT NOT NULL DEFAULT 'microcredit' CHECK (unit IN ('microcredit', 'micromoney')),
    currency TEXT CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    -- 人员及服务双重归因（设计 §9.2）
    operator_subject TEXT NOT NULL,
    service_subject  TEXT NOT NULL DEFAULT '',
    -- 幂等键：重复补偿投递只生效一次
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((unit = 'microcredit') = (currency IS NULL))
);

DO $$
BEGIN
    ALTER TABLE inference_ledger_entries
        ADD CONSTRAINT inference_ledger_adjustment_fk
        FOREIGN KEY (adjustment_id) REFERENCES inference_adjustments (id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_inference_adjustments_account
    ON inference_adjustments (billing_account_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 持久任务：事务 outbox 与核对队列（设计 §3、§7.2）
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inference_outbox (
    id BIGSERIAL PRIMARY KEY,
    topic   TEXT NOT NULL,
    payload JSONB NOT NULL,
    -- 幂等消费键（支付发放等场景）
    dedup_key TEXT UNIQUE,
    status   TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_retry_at TIMESTAMPTZ,
    delivered_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_outbox_pending
    ON inference_outbox (next_retry_at) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS inference_reconciliation_jobs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 一次请求至多一个核对任务
    request_id UUID NOT NULL UNIQUE REFERENCES inference_requests (id),
    reason TEXT NOT NULL
           CHECK (reason IN ('unknown_usage', 'crash_recovery', 'ledger_mismatch', 'manual')),
    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'running', 'resolved', 'escalated')),
    detail JSONB NOT NULL DEFAULT '{"schema_version":1}',
    -- 恢复时限：超时升级告警，禁止仅凭 TTL 释放预占（设计 §7.2）
    deadline_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_inference_reconciliation_open
    ON inference_reconciliation_jobs (deadline_at) WHERE status IN ('pending', 'running');
