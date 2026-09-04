-- 021_usage_events.sql
-- Description: 使用统计心跳流水表 — kaya 桌面端登录用户每 5min 上报一拍
-- 设计文档: yunhou-terminal docs/superpowers/specs/2026-09-04-usage-analytics-design.md
--
-- 口径:
--   - 活跃用户(DAU):当天(local_date)有 ≥1 条心跳落库的登录用户
--   - 使用时长:active_seconds 增量求和(应用存活时长,客户端每拍封顶 300s)
--   - local_date 为客户端本地日期(YYYY-MM-DD),服务端不重新推导日界
--
-- 隐私边界(路线 A):不含 nickname/email/会话内容;身份仅经 JWT 关联 user_id。
--
-- 幂等:(user_id, client_event_id) 唯一约束;客户端断网缓冲后批量补发,
-- 重试/补发由 ON CONFLICT DO NOTHING 吞掉,不产生重复数据。
--
-- 数据量说明:每登录用户每天最多 288 条。MVP 直接对流水表 SQL 聚合;
-- 原始数据保留期/usage_daily 预聚合表随量级增长后另起 migration(首版不做)。

CREATE TABLE IF NOT EXISTS usage_events (
    id              BIGSERIAL PRIMARY KEY,
    -- ON DELETE CASCADE 与 social_identities/sessions/subscriptions 一致:
    -- 删用户即删其心跳数据(隐私友好)
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- app_id 即 apps.app_id(JWT claims.app_id,如 'yunhou-website'),
    -- 统计按应用过滤时受 FK 保护
    app_id          TEXT NOT NULL REFERENCES apps(app_id),
    client_event_id UUID NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    local_date      DATE NOT NULL,
    -- 单拍存活秒数;客户端每拍封顶为心跳间隔(300s),3600 为防御性上限
    active_seconds  INT NOT NULL CHECK (active_seconds BETWEEN 0 AND 3600),
    platform        TEXT NOT NULL CHECK (platform IN ('macos', 'windows', 'linux')),
    app_version     TEXT NOT NULL CHECK (length(app_version) <= 64),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 幂等键:重试/补发不产生重复数据(写入路径 ON CONFLICT DO NOTHING)
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_user_event
    ON usage_events (user_id, client_event_id);

-- 按用户+日期聚合(单用户时长明细)
CREATE INDEX IF NOT EXISTS idx_usage_events_user_date
    ON usage_events (user_id, local_date);

-- 按日期聚合(DAU/WAU/MAU、总时长;WAU/MAU 为 local_date 范围扫描)
CREATE INDEX IF NOT EXISTS idx_usage_events_date
    ON usage_events (local_date);
