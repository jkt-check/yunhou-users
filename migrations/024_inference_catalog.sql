-- 024_inference_catalog.sql
-- Description: inference 模块模型目录表组 — 模型/供应商/上游部署/模型路由/配置发布版本
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §5、§7.3
--
-- 口径:
--   - inference_models.id 是稳定公开模型 ID（API 契约），不使用自增/UUID。
--   - 新模型默认 lifecycle='draft'（不可售），发布后进入 active。
--   - 核心字段用类型列 + CHECK；仅协议扩展配置（deployments.config、
--     config_revisions.payload）使用带 schema 版本的 JSONB。
--   - 全部时间列 TIMESTAMPTZ，服务端口径 UTC。

CREATE TABLE IF NOT EXISTS inference_models (
    -- 稳定公开 ID（如 'glm-4.6'），即 /v1/models 的 id 字段
    id               TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    display_name     TEXT NOT NULL,
    -- 生命周期: draft(默认不可售) → active → deprecated → retired
    lifecycle        TEXT NOT NULL DEFAULT 'draft'
                     CHECK (lifecycle IN ('draft', 'active', 'deprecated', 'retired')),
    -- 模型版本与别名（'latest' 等别名解析在 catalog 层，不落库重复行）
    model_version    TEXT NOT NULL DEFAULT '',
    aliases          TEXT[] NOT NULL DEFAULT '{}',
    -- 输入/输出模态: text/image/audio/... ，至少 text→text
    input_modalities  TEXT[] NOT NULL DEFAULT '{text}',
    output_modalities TEXT[] NOT NULL DEFAULT '{text}',
    -- 上下文与输出硬上限（token）；设计 §7.2：不允许无限输出
    context_tokens   INT NOT NULL CHECK (context_tokens > 0),
    max_output_tokens INT NOT NULL CHECK (max_output_tokens > 0),
    -- 支持的对外协议与能力
    protocols        TEXT[] NOT NULL DEFAULT '{openai_chat}',
    supports_tools     BOOLEAN NOT NULL DEFAULT false,
    supports_reasoning BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_models_lifecycle
    ON inference_models (lifecycle);

CREATE TABLE IF NOT EXISTS inference_providers (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 供应商标识（'openai'/'anthropic'/...），与接入类型分离
    code         TEXT NOT NULL UNIQUE CHECK (code ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    display_name TEXT NOT NULL,
    -- 接入类型: 官方 API / OAuth 连接器 / 自托管服务（设计 §8）
    access_type  TEXT NOT NULL
                 CHECK (access_type IN ('official_api', 'oauth_connector', 'self_hosted')),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS inference_deployments (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id    UUID NOT NULL REFERENCES inference_providers (id),
    -- 上游侧模型名（与公开模型 ID 不同名；设计 §5）
    upstream_model TEXT NOT NULL,
    base_url       TEXT NOT NULL,
    -- 上游协议（与认证类型分别建模，设计 §8）
    protocol       TEXT NOT NULL
                   CHECK (protocol IN ('openai_chat', 'openai_responses', 'anthropic_messages')),
    region         TEXT NOT NULL DEFAULT '',
    connect_timeout_ms INT NOT NULL DEFAULT 5000  CHECK (connect_timeout_ms > 0),
    request_timeout_ms INT NOT NULL DEFAULT 600000 CHECK (request_timeout_ms > 0),
    -- 配置乐观锁版本号；每次编辑 +1（设计 §5 草稿/发布乐观锁）
    config_version INT NOT NULL DEFAULT 1 CHECK (config_version > 0),
    -- 仅协议扩展配置进 JSONB，须带 schema 版本：
    -- {"schema_version":1, ...}；自定义请求头等受限字段在 Task 4 校验
    config         JSONB NOT NULL DEFAULT '{"schema_version":1}',
    status         TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'disabled')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, upstream_model, base_url)
);

CREATE INDEX IF NOT EXISTS idx_inference_deployments_provider
    ON inference_deployments (provider_id);

-- Model → 多 Deployment 路由（设计 §5 ModelRoute）
CREATE TABLE IF NOT EXISTS inference_model_routes (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    model_id      TEXT NOT NULL REFERENCES inference_models (id),
    deployment_id UUID NOT NULL REFERENCES inference_deployments (id),
    -- 数字小者优先；同优先级按 weight 加权
    priority      INT NOT NULL DEFAULT 0,
    weight        INT NOT NULL DEFAULT 1 CHECK (weight > 0),
    -- 该路由适用的模型能力子集（空 = 全部能力可用）
    capabilities  TEXT[] NOT NULL DEFAULT '{}',
    -- 账号池策略（Task 12 调度语义）
    account_pool_strategy TEXT NOT NULL DEFAULT 'round_robin'
                   CHECK (account_pool_strategy IN ('round_robin', 'least_loaded', 'session_sticky')),
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, deployment_id)
);

CREATE INDEX IF NOT EXISTS idx_inference_model_routes_model
    ON inference_model_routes (model_id) WHERE enabled;

-- 不可变发布版本（设计 §5：草稿 + 不可变发布，发布原子切换 active revision）
CREATE TABLE IF NOT EXISTS inference_config_revisions (
    -- BIGSERIAL 提供全局单调的发布序号，便于快照按序加载
    id           BIGSERIAL PRIMARY KEY,
    -- 发布范围: catalog（模型/部署/路由）、pricing、policy（后续任务按范围扩展）
    scope        TEXT NOT NULL CHECK (scope IN ('catalog', 'pricing', 'policy')),
    revision     INT NOT NULL CHECK (revision > 0),
    -- 完整不可变快照，带 schema 版本：{"schema_version":1, ...}
    payload      JSONB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'published', 'superseded')),
    is_active    BOOLEAN NOT NULL DEFAULT false,
    published_at TIMESTAMPTZ,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope, revision)
);

-- 每个 scope 至多一个 active 发布版本（发布原子切换的数据库保障）
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_config_revisions_active
    ON inference_config_revisions (scope) WHERE is_active;
