-- 025_inference_accounts.sql
-- Description: inference 模块账号表组 — 上游凭据/上游账号/客户计费账户/客户 API Key
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §4.2、§5、§8、§7.3
--
-- 口径:
--   - 凭据只存密文（AEAD，Task 4 实现加解密）；任何 SELECT 默认不返回可用秘密。
--   - billing_accounts 对客户身份 users(id) 不做级联删除：账本/计费相关数据
--     随客户删除走去标识化策略（后续任务实现），此处保留行。
--   - API Key 只存查找前缀与验证摘要，明文仅创建时返回一次。

CREATE TABLE IF NOT EXISTS inference_credentials (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id UUID NOT NULL REFERENCES inference_providers (id),
    label       TEXT NOT NULL DEFAULT '',
    -- 认证方式与模型协议分别建模（设计 §8）
    auth_type   TEXT NOT NULL CHECK (auth_type IN ('api_key', 'oauth', 'service')),
    -- AEAD 密文；配套 key_version 支持轮换后解密旧版本
    ciphertext  BYTEA NOT NULL,
    key_version INT NOT NULL CHECK (key_version > 0),
    -- refresh generation CAS（设计 §8：旧 refresh token 不得覆盖新值）
    generation  BIGINT NOT NULL DEFAULT 1 CHECK (generation > 0),
    expires_at  TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'rotating', 'revoked')),
    last_rotated_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_credentials_provider
    ON inference_credentials (provider_id);

CREATE TABLE IF NOT EXISTS inference_upstream_accounts (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id   UUID NOT NULL REFERENCES inference_providers (id),
    credential_id UUID NOT NULL REFERENCES inference_credentials (id),
    -- 上游侧账号标识（连接器/官方 API 返回的 account id；未知时为空串）
    external_account_id TEXT NOT NULL DEFAULT '',
    display_name  TEXT NOT NULL DEFAULT '',
    -- 账号状态机（设计 §8）
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'refreshing', 'cooldown', 'reauth_required', 'disabled')),
    -- 每账号并发上限；0 = 不允许调度
    concurrency_limit INT NOT NULL DEFAULT 1 CHECK (concurrency_limit >= 0),
    -- 上游额度缓存（设计 §8：observed_at/source/reset_at，未知保持未知 → 全部可空）
    quota_limit_micros     BIGINT CHECK (quota_limit_micros >= 0),
    quota_remaining_micros BIGINT CHECK (quota_remaining_micros >= 0),
    quota_observed_at TIMESTAMPTZ,
    quota_source      TEXT CHECK (quota_source IS NULL OR quota_source IN ('reported', 'estimated')),
    quota_reset_at    TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, credential_id)
);

CREATE INDEX IF NOT EXISTS idx_inference_upstream_accounts_status
    ON inference_upstream_accounts (status) WHERE status IN ('active', 'refreshing', 'cooldown');

CREATE TABLE IF NOT EXISTS inference_billing_accounts (
    id      UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 首期每用户一个个人计费账户；所有权来自服务端身份。
    -- 不级联删除（设计 §7.3：账本保留去标识化主体）。
    user_id UUID NOT NULL REFERENCES users (id),
    -- 预留账户主体字段（组织/席位后续任务，首期固定 'user'）
    subject_type TEXT NOT NULL DEFAULT 'user' CHECK (subject_type IN ('user', 'organization')),
    status  TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id)
);

CREATE TABLE IF NOT EXISTS inference_api_keys (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    billing_account_id UUID NOT NULL REFERENCES inference_billing_accounts (id),
    name               TEXT NOT NULL DEFAULT '',
    -- 查找前缀（明文前缀，索引定位）与验证摘要（哈希，不存明文）
    key_prefix         TEXT NOT NULL UNIQUE,
    key_hash           TEXT NOT NULL UNIQUE,
    -- Key 级模型收窄：只能在所属账户权益范围内缩小，永不能扩大；
    -- NULL = 不做 Key 级收窄（权益集合仍是唯一授权来源，设计 §4.2）
    model_allow        TEXT[],
    -- Key 预算（微额度）；NULL = 不设预算。used 由 Task 7 的原子预占维护，
    -- 与三窗口在同一事务内更新。
    budget_limit_micros BIGINT CHECK (budget_limit_micros IS NULL OR budget_limit_micros >= 0),
    budget_used_micros  BIGINT NOT NULL DEFAULT 0 CHECK (budget_used_micros >= 0),
    -- Key 级速率/并发上限；NULL = 沿用策略
    rpm_limit         INT CHECK (rpm_limit IS NULL OR rpm_limit > 0),
    concurrency_limit INT CHECK (concurrency_limit IS NULL OR concurrency_limit > 0),
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked', 'expired')),
    expires_at  TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_api_keys_account
    ON inference_api_keys (billing_account_id);
