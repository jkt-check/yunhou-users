-- 052_inference_rate_reservations.sql
--
-- T4：PostgreSQL 统一业务速率协调（账户/Key 级 RPM + 账户级策略 TPM）。
--
-- 为什么需要这张表：进程内的滑动窗口计数器（access.RPMCounter）既不跨
-- 实例共享，也把「认证」和「业务额度」混在一起。速率上限是**账户级业务
-- 事实**：多个实例、多个 Key、/chat 与 /v1 共用同一个账户计数，且切套餐
-- 不得重置这一分钟的消费。唯一能同时满足这些语义的地方是数据库。
--
-- 计数口径（设计 §7.2 速率闸门）：
--   - scope='account'：账户级行。counted_at 是该逻辑请求成功准入的时刻
--     （DB 时间），滚动 60 秒内的行数 = 账户 RPM 计数（全局账户 RPM 与
--     选中权益策略 RPM 共用这一个账户级计数 —— 两者都是账户级上限）。
--     reserved_tokens / settled_tokens 承载账户级 TPM：在途预占与结算后
--     的归一化 token 消耗。
--   - scope='key'：Key 级行，只承载该 Key 的滚动 60 秒计数。
--   - 计数键**不含 policy version**：换套餐不重置一分钟消费。
--   - 每个逻辑请求每个 scope 至多一行（UNIQUE(request_id, scope)）：同一个
--     逻辑请求 ID 重复预占撞唯一键（幂等身份）。
--
-- 生命周期：准入（reserved）→ 结算（settled，token 换成实际值）或释放
-- （released，确认零消费）。清理**只删已终结的行**：state='reserved' 的
-- 未结算预占永不删除（长流跨 60 秒仍占额度，不能靠 TTL 释放）。

CREATE TABLE IF NOT EXISTS inference_rate_reservations (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 逻辑请求 ID：幂等身份（一个请求每个 scope 至多一行）。
    request_id         UUID NOT NULL REFERENCES inference_requests (id),
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    api_key_id         UUID REFERENCES inference_api_keys (id),
    scope              TEXT NOT NULL CHECK (scope IN ('account','key')),
    -- 成功准入的时刻（DB 时间口径，多实例一致）：RPM 的滚动窗口归属。
    counted_at         TIMESTAMPTZ NOT NULL,
    -- TPM（账户级）：在途预占 token 与结算后的实际 token。
    reserved_tokens    BIGINT NOT NULL DEFAULT 0 CHECK (reserved_tokens >= 0),
    settled_tokens     BIGINT NOT NULL DEFAULT 0 CHECK (settled_tokens >= 0),
    -- reserved=在途（占 TPM）/ settled=已结算（计实际 token）/ released=
    -- 确认零消费（释放 TPM 预占，但 RPM 计数仍然保留 —— 准入成功即计 1 次）。
    state              TEXT NOT NULL DEFAULT 'reserved'
                       CHECK (state IN ('reserved','settled','released')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at         TIMESTAMPTZ,
    released_at        TIMESTAMPTZ,
    UNIQUE (request_id, scope),
    CHECK ((scope = 'key') = (api_key_id IS NOT NULL))
);

-- 账户级 RPM 计数窗口（billing_account_id + counted_at）。
CREATE INDEX IF NOT EXISTS idx_inference_rate_reservations_account_rpm
    ON inference_rate_reservations (billing_account_id, counted_at DESC)
    WHERE scope = 'account';

-- Key 级 RPM 计数窗口。
CREATE INDEX IF NOT EXISTS idx_inference_rate_reservations_key_rpm
    ON inference_rate_reservations (api_key_id, counted_at DESC)
    WHERE scope = 'key';

-- TPM 在途预占求和（state='reserved'；与年龄无关 —— 长流跨窗口仍占额度）。
CREATE INDEX IF NOT EXISTS idx_inference_rate_reservations_open
    ON inference_rate_reservations (billing_account_id)
    WHERE state = 'reserved';

-- TPM 窗口内已结算 token 求和（结算时刻落在滚动 60 秒内）。
CREATE INDEX IF NOT EXISTS idx_inference_rate_reservations_settled
    ON inference_rate_reservations (billing_account_id, settled_at DESC)
    WHERE state = 'settled';

-- 保留期清理用（只删已终结行；未结算预占永不删除）。
CREATE INDEX IF NOT EXISTS idx_inference_rate_reservations_terminal
    ON inference_rate_reservations (counted_at)
    WHERE state <> 'reserved';
