-- 047_kaya_usage_token_stats.sql
-- Description: kaya token 用量统计（云端摄入 + 用户自助查询）— 契约见
-- yunhou-terminal docs/api/usage-stats.md v1.2（2026-10-22）。
--
-- 【边界：本仓第四套用量表】不要与既有三套混用：
--   - usage_events（021）：kaya 登录心跳，支撑 /admin/stats/* DAU/WAU/MAU
--     与时长统计，不含任何 token 计数；
--   - llm_usage_events（022）：/chat 代理（DeepSeek 直通/网关 facade）的
--     服务端计量，按模型聚合，是服务端实测值；
--   - inference 计量表组（026 起：inference_usage_records / ledger_entries /
--     quota_windows 等）：网关计费依据，承载额度闸门与账本；
--   - 本组（047）：kaya 客户端本地聚合后自报的小时/天桶，仅供用户登录后
--     自助查询自己的多设备用量。客户端自报数据，【不是计费依据】（契约 §6
--     第 11 条：网关路由黑名单等场景下数字可能整块缺失，不得用于对账）。
--
-- 【v1 无 RLS 是显式的已知风险决策】（契约 §5 建议）：本栈无 RLS 先例，
-- v1 补偿措施 = 本 handler 家族（internal/handler/usage_tokens.go）所有
-- 查询强制 WHERE user_id = JWT sub + 固定三过滤（user_id / bucket_size =
-- 部署配置 / 排除已撤销设备），并由以下测试覆盖跨账号 IDOR 路径：
--   - TestUsageTokenIntegration_CrossUserIDOR（internal/service/
--     usage_token_integration_test.go：A 的桶/设备对 B 不可见、不可操作）
--   - TestUsageTokenIntegration_RevokedDeviceExcludedFromStats
--   - TestUsageTokenIntegration_DeleteAllThenReuploadDiscarded
--   - TestUsageTokenIntegration_WindowTombstone
-- 后续若要上 RLS，作为纵深防御另起专项，不作为本契约阻塞项。
--
-- 【bucket_size 混存防双计的三条 MUST 落点】（契约 §6 第 1 条）：
--   1. 摄入侧硬拒绝 bucket_size 与部署配置（USAGE_TOKEN_BUCKET_SIZE）不一致
--      —— handler 返回 bucket_size_mismatch（本表 CHECK 只限 hour/day，
--      不限制部署值）；
--   2. 所有读查询固定 WHERE bucket_size = 部署配置（service 层注入，唯一键
--      里的 bucket_size 只防写重，不防读双计）；
--   3. hour ↔ day 配置变更必须执行一次性清桶/迁移 runbook，切换后旧桶不得
--      参与任何查询（靠第 2 条的固定过滤天然满足）。
--
-- 幂等：全部 IF NOT EXISTS；本文件不含 BEGIN/COMMIT（cmd/migrate 单事务
-- 包裹），不含 CREATE INDEX CONCURRENTLY。

CREATE TABLE IF NOT EXISTS usage_token_buckets (
    id                BIGSERIAL PRIMARY KEY,
    -- 删用户即删其全部用量桶（隐私友好，与 usage_events 一致）
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id         TEXT NOT NULL,
    bucket_start      TIMESTAMPTZ NOT NULL,
    bucket_size       TEXT NOT NULL CHECK (bucket_size IN ('hour','day')),
    source            TEXT NOT NULL CHECK (source IN ('chat','gateway')),
    agent_key         TEXT NOT NULL,
    provider_key      TEXT NOT NULL,
    model_key         TEXT NOT NULL,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens BIGINT NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    total_tokens      BIGINT NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
    requests          BIGINT NOT NULL DEFAULT 0 CHECK (requests >= 0),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 唯一键 = 幂等键：客户端重发同一桶的完整累计值时 ON CONFLICT DO UPDATE
    -- 覆盖，不累加（契约 §3.3 重试安全/不发 delta）
    UNIQUE (user_id, device_id, bucket_start, bucket_size, source, agent_key, provider_key, model_key)
);

CREATE INDEX IF NOT EXISTS idx_usage_token_user_time
    ON usage_token_buckets (user_id, bucket_start);

-- 不建 (user_id, device_id, bucket_start) 冗余索引：
-- UNIQUE 约束的最左前缀已覆盖该查询，额外索引只增加写放大。

CREATE TABLE IF NOT EXISTS usage_devices (
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id        TEXT NOT NULL,
    -- label 是用户手动输入；不自动采集 hostname（契约 §4.8）
    label            TEXT,
    platform         TEXT CHECK (platform IN ('macos','windows','linux')),
    app_version      TEXT,
    consented_at     TIMESTAMPTZ,
    consent_version  TEXT,
    -- 设备级删除水位：摄入丢弃 bucket_start < cleared_before 的该设备桶
    cleared_before   TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at     TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    PRIMARY KEY (user_id, device_id)
);

-- 全量删除的用户级水位（对所有设备生效，含未来新设备）
CREATE TABLE IF NOT EXISTS usage_token_watermarks (
    user_id        UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    cleared_before TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 窗口删除的墓碑；device_id 为空 = 该用户全部设备。
-- 水位只能表达"某时刻之前"，表达不了任意中间窗口，因此窗口删除必须用
-- 墓碑（契约 §3.4 边界条）。
CREATE TABLE IF NOT EXISTS usage_token_tombstones (
    id          BIGSERIAL PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id   TEXT,
    from_bucket TIMESTAMPTZ NOT NULL,
    to_bucket   TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_bucket <= to_bucket)
);

CREATE INDEX IF NOT EXISTS idx_usage_token_tombstone_lookup
    ON usage_token_tombstones (user_id, device_id, from_bucket);
