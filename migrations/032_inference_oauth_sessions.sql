-- 032_inference_oauth_sessions.sql
-- Description: inference OAuth 授权状态与会话绑定表组（Task 12）
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §8
--
-- 口径:
--   - OAuth 授权与社交登录完全分离：不引用 apps.config/oauth_providers，
--     state 绑定发起运营人员（operator user + app）、目标供应商与有效期，
--     回调一次性消费（consumed_at 原子翻转）。
--   - PKCE code_verifier 是短期服务端秘密：仅本表暂存，消费后不清除行
--     （审计可追溯），但任何 API 响应永不返回它。
--   - 会话绑定（session_binding）实现设计 §8 粘性会话：需要黏性时把
--     (session_key, model_id) 绑定到具体上游账号；账号失效后绑定显式终止
--     （ended），不允许无条件切账号续接；重新绑定按新会话语义显式发起。
--   - 刷新分布式互斥使用 pg_advisory_xact_lock（无新表）；轮换写库走
--     inference_credentials.generation CAS（025 已有列）。

-- OAuth 凭据记录其连接器注册表键（回调创建时写入）：刷新 worker 据此找到
-- 对应厂商的 token 端点。api_key/service 凭据保持空串。
ALTER TABLE inference_credentials
    ADD COLUMN IF NOT EXISTS connector TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS inference_oauth_grants (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 一次性 state（密码学随机）；消费后行保留作审计
    state        TEXT NOT NULL UNIQUE,
    -- 连接器注册表键（INFERENCE_OAUTH_CONNECTORS_JSON 中的 key），
    -- 决定 authorize/token 端点；与供应商行分离建模
    connector    TEXT NOT NULL,
    provider_id  UUID NOT NULL REFERENCES inference_providers (id),
    -- 目标账号展示名（回调成功后创建的 upstream account 使用）
    account_label TEXT NOT NULL DEFAULT '',
    -- PKCE S256 verifier（服务器端暂存，绝不外发）
    code_verifier TEXT NOT NULL,
    -- state 绑定发起运营人员（设计 §8: state 绑定发起运营人员、目标账号/
    -- 供应商及有效期）；回调消费时校验发起身份仍在场
    operator_user_id UUID NOT NULL REFERENCES users (id),
    operator_app_id  TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_oauth_grants_expiry
    ON inference_oauth_grants (expires_at) WHERE consumed_at IS NULL;

CREATE TABLE IF NOT EXISTS inference_session_bindings (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 粘性会话键（客户端会话标识）+ 公开模型 ID 共同定位一条绑定
    session_key TEXT NOT NULL,
    model_id    TEXT NOT NULL REFERENCES inference_models (id),
    account_id  UUID NOT NULL REFERENCES inference_upstream_accounts (id),
    -- active: 调度可用；ended: 已终止（账号失效/显式迁移/到期），
    -- 终止后同 (session_key, model_id) 只能按新会话语义显式重建绑定
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
    ended_reason TEXT,
    bound_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 同一 (session_key, model_id) 至多一条 active 绑定
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_session_bindings_active
    ON inference_session_bindings (session_key, model_id) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_inference_session_bindings_account
    ON inference_session_bindings (account_id) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_inference_session_bindings_expiry
    ON inference_session_bindings (expires_at) WHERE status = 'active';
