-- 028_operator_permissions.sql
-- Description: inference 模块运营授权与审计 — 运营角色表 + 管理审计日志
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §9.2
--
-- 口径:
--   - 运营身份 = 服务端验证的用户 JWT（operator_roles.user_id）+ 受验证的
--     服务身份（X-App-ID/X-App-Secret，应用层中间件保证）。角色只来自本表，
--     请求体自报的 role/actor 一律无效。
--   - 角色 → 权限映射在代码层（management.PermissionsOf）：admin 拥有
--     models:manage / credentials:manage / billing:adjust / usage:read 全部；
--     operator 拥有 models:manage / credentials:manage / usage:read；
--     auditor 只读 usage:read（后台读权限与秘密写权限分离）。
--   - 权限变更（grant/revoke）与凭据/目录写操作都落 inference_audit_log，
--     记录人员 + 服务双重归因、对象、原因；detail JSONB 经服务端脱敏，
--     任何响应/日志/审计均不含可用秘密。
--   - bootstrap 命令（cmd/admin-bootstrap）离线授予首个 admin，幂等。

CREATE TABLE IF NOT EXISTS operator_roles (
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'auditor')),
    granted_by UUID REFERENCES users (id),
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, role)
);

CREATE INDEX IF NOT EXISTS idx_operator_roles_user
    ON operator_roles (user_id);

CREATE TABLE IF NOT EXISTS inference_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 人员归因（服务端验证的用户 JWT subject）；服务调用必非空
    actor_user_id UUID,
    -- 服务归因（受验证的 X-App-ID）
    actor_app_id  TEXT NOT NULL DEFAULT '',
    -- 动词-名词，如 credential.create / credential.rotate / credential.test /
    -- credential.disable / model.create / catalog.publish / permission.grant
    action        TEXT NOT NULL,
    object_type   TEXT NOT NULL DEFAULT '',
    object_id     TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    -- 服务端脱敏后的上下文；禁止写入秘密明文/密文
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_inference_audit_log_occurred_at
    ON inference_audit_log (occurred_at);
CREATE INDEX IF NOT EXISTS idx_inference_audit_log_action
    ON inference_audit_log (action);
CREATE INDEX IF NOT EXISTS idx_inference_audit_log_object
    ON inference_audit_log (object_type, object_id);
CREATE INDEX IF NOT EXISTS idx_inference_audit_log_actor
    ON inference_audit_log (actor_user_id);
