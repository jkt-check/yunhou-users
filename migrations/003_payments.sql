-- Migration: 003_payments
-- Description: 支付数据存储层 — orders / payments / refunds / webhook_events / audit_log
-- 设计文档: docs/plans/2026-06-16-user-system-design.md
-- webhook 机制: docs/plans/2026-06-23-payment-webhook-mechanism.md
--
-- 职责边界:
--   yunhou-users 是原始操作层 — 支付执行在前端，钱在 Stripe/WeChat/Alipay，
--   本服务只存储订单/支付/退款/webhook/审计事实，不写业务策略（退款窗口、审批流等）。
--
-- 设计原则:
--   - TEXT + CHECK (col IN (...)) 表达 enum（与 001/002 一致，加状态便宜）
--   - DECIMAL(10,2) major currency units（边界归一化在 webhook doc §"Amount unit convention"）
--   - TIMESTAMPTZ NOT NULL DEFAULT now()
--   - 部分唯一索引用 WHERE 子句
--   - cross-table 引用不加 FK（service 层保证 referential integrity，
--     避免 cascade 链路上 refund 测试被锁死）
-- ============================================================================
-- 1. orders — 支付前的意图（user 选了 plan，server mint 一行）
-- ============================================================================
CREATE TABLE IF NOT EXISTS orders (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    plan_id     TEXT NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
    amount      DECIMAL(10,2) NOT NULL CHECK (amount >= 0),
    currency    TEXT NOT NULL DEFAULT 'CNY' CHECK (length(currency) = 3),
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'paid', 'failed', 'refunded', 'cancelled', 'expired')),
    -- 30 分钟默认过期（设计文档 §"v1 decisions on order lifecycle"），
    -- 配置可改但 SQL 默认值固化 30 分钟；service 层 INSERT 时可显式覆盖
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '30 minutes'),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- user 历史订单查询（GET /payments）
CREATE INDEX IF NOT EXISTS idx_orders_user_id ON orders(user_id);
-- sweeper 扫描路径：status='pending' AND expires_at < now()
CREATE INDEX IF NOT EXISTS idx_orders_status_expires ON orders(status, expires_at);
-- 统计/对账（按 plan 看 GMV 等）
CREATE INDEX IF NOT EXISTS idx_orders_plan_id ON orders(plan_id);
-- 按状态过滤（admin 列表）
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);

-- ============================================================================
-- 2. payments — 通道侧的事务，order 最多有一个 paid 行
-- ============================================================================
CREATE TABLE IF NOT EXISTS payments (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- order_id 不加 FK：与 webhook 边界设计一致（webhook payload 携带 order_id，
    -- handler 在事务里读 order 表；order 被删时 payment 不应该 cascade）
    order_id        UUID NOT NULL,
    channel         TEXT NOT NULL CHECK (channel IN ('stripe', 'wechat_pay', 'alipay')),
    external_txn_id TEXT NOT NULL,
    amount          DECIMAL(10,2) NOT NULL CHECK (amount >= 0),
    currency        TEXT NOT NULL DEFAULT 'CNY' CHECK (length(currency) = 3),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'paid', 'failed', 'refunded')),
    paid_at         TIMESTAMPTZ,
    failed_reason   TEXT,
    disputed        BOOLEAN NOT NULL DEFAULT false,
    disputed_at     TIMESTAMPTZ,
    -- 原始 webhook body（webhook doc §"raw_payload" 永不删，forensic 用）。
    -- JSONB 列要求 valid JSON；handler 必须保证写入值合法。
    -- 存储策略：
    --   * Stripe / WeChat：body 本身就是 JSON，作为 JSON 字符串存储
    --     (`"<escaped json>"` — 由 handler.wrapRawPayload 统一处理)。
    --   * Alipay：body 是 form-encoded key=value&... 文本，handler 把
    --     它 escape 成 JSON 字符串后存入本列：
    --     `{"raw":"<escaped form body>"}`。
    -- 任何后续 forensic / 审计查询要按 channel 取对应的反序列化方式。
    raw_payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 业务级幂等键（webhook doc §5.2）
    UNIQUE (channel, external_txn_id)
);

-- order 维度查询（GET /payments/:order_id/refunds 等）
CREATE INDEX IF NOT EXISTS idx_payments_order_id ON payments(order_id);
-- 状态过滤（admin 列表 / 对账）
CREATE INDEX IF NOT EXISTS idx_payments_status ON payments(status);
-- 争议支付查询（chargeback 跟踪）
CREATE INDEX IF NOT EXISTS idx_payments_disputed ON payments(disputed) WHERE disputed = true;

-- 关键的部分唯一索引：一个 order 最多一个 paid 支付（设计文档 §Payment + webhook doc §5.2）。
-- 跨通道重试（Stripe failed → WeChat succeeded）允许，但只能有一个 paid。
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_one_paid_per_order
    ON payments(order_id) WHERE status = 'paid';

-- ============================================================================
-- 3. refunds — 每行一次退款事件，部分退款可多次
-- ============================================================================
CREATE TABLE IF NOT EXISTS refunds (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- payment_id 不加 FK（同 payments.order_id 原则）
    payment_id        UUID NOT NULL,
    -- channel denormalized from payments.channel（设计文档 Refund 表注），
    -- 不 denorm 就没法在 DB 层 enforce UNIQUE(channel, external_refund_id)
    channel           TEXT NOT NULL CHECK (channel IN ('stripe', 'wechat_pay', 'alipay')),
    amount            DECIMAL(10,2) NOT NULL CHECK (amount > 0),
    reason            TEXT,
    -- Denormalized from orders.user_id via payment_id → order_id → user_id
    -- join at INSERT time. Required so Idempotency-Key can be scoped per
    -- user (UNIQUE(user_id, idempotency_key)) — without this, two users
    -- picking the same key would collide and one would see the other's
    -- refund response (IDOR / data exposure).
    user_id           UUID NOT NULL,
    -- 调用方 HTTP header Idempotency-Key（设计文档 POST /refunds）
    idempotency_key   TEXT NOT NULL,
    -- channel 返回前为 NULL；PG 的 UNIQUE 默认把 NULL 视为 distinct，
    -- 所以同一 payment 的多次 channel-call（异常路径下）可以各自有 NULL external_refund_id
    external_refund_id TEXT,
    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'paid', 'failed')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 调用方幂等：相同 (user, key) 的 POST /refunds 命中已有行。
    -- 范围是 (user_id, idempotency_key) 而非 (idempotency_key) 全局，
    -- 防止 cross-user IDOR；handler 同时会做长度 / 字符集校验。
    UNIQUE (user_id, idempotency_key),
    -- 业务幂等：同一通道不会给同一个退款事件两个外部 ID
    UNIQUE (channel, external_refund_id)
);

-- payment 维度查询（GET /payments/:id/refunds）
CREATE INDEX IF NOT EXISTS idx_refunds_payment_id ON refunds(payment_id);
-- 状态过滤（admin 列表）
CREATE INDEX IF NOT EXISTS idx_refunds_status ON refunds(status);
-- user 历史退款查询（GET /user/refunds 等）；也是 scoped idempotency lookup 的支撑索引
CREATE INDEX IF NOT EXISTS idx_refunds_user_id ON refunds(user_id);

-- ============================================================================
-- 4. webhook_events — 每个事件的审计行，handler 先 INSERT 再业务操作（webhook doc §5.1）
-- ============================================================================
CREATE TABLE IF NOT EXISTS webhook_events (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel      TEXT NOT NULL CHECK (channel IN ('stripe', 'wechat_pay', 'alipay')),
    event_id     TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL = queued (handler 还没处理完)；NOT NULL = 已处理（无论业务 action 是否执行）
    processed_at TIMESTAMPTZ,
    raw_payload  JSONB NOT NULL,
    UNIQUE (channel, event_id)
);

-- channel 维度查询（ops 排查某通道最近事件）
CREATE INDEX IF NOT EXISTS idx_webhook_events_channel ON webhook_events(channel);
-- 未处理事件监控（alert on count > threshold）
CREATE INDEX IF NOT EXISTS idx_webhook_events_unprocessed
    ON webhook_events(received_at) WHERE processed_at IS NULL;

-- ============================================================================
-- 5. audit_log — 服务层写的事实日志（webhook 不写这里，webhook 写 webhook_events）
-- ============================================================================
CREATE TABLE IF NOT EXISTS audit_log (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- actor 形式：'sweeper' / 'service' / 'user:<user_id>' / 'admin:<app_id>'
    actor       TEXT NOT NULL,
    -- 动词-名词，如 'late_payment_post_expiry' / 'cancel_order' / 'unexpected_state_transition'
    action      TEXT NOT NULL,
    -- 资源引用，如 'order:<order_id>'
    target      TEXT,
    tags        TEXT[] NOT NULL DEFAULT '{}',
    -- 结构化 payload：old state, new state, ids involved
    context     JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- retention unbounded in v1（设计文档 §AuditLog），但 ops 查询不能全表扫
CREATE INDEX IF NOT EXISTS idx_audit_log_occurred_at ON audit_log(occurred_at);
-- 按 action 查（如所有 late_payment_post_expiry）
CREATE INDEX IF NOT EXISTS idx_audit_log_action ON audit_log(action);
-- 按 actor 查（如某用户的所有动作）
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(actor);
-- GIN 支持 tags 包含查询（如 action IN (...,...) OR tags && ARRAY[...]）
CREATE INDEX IF NOT EXISTS idx_audit_log_tags ON audit_log USING GIN (tags);