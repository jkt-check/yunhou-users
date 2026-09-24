-- 037_admin_idempotency.sql
-- Description: dashboard 运营 admin API（POST /admin/users/:id/vip）的幂等键表
-- 规格: dashboard-admin-api-spec.md §5.3
--
-- 口径:
--   - (app_id, key) 唯一：不同 app 的相同 key 互不干扰，同一 app 重放返回
--     首次成功的 response 载荷（照 inference_bulk_imports.task_id 撞键重放
--     与 refunds UNIQUE(user_id, idempotency_key) 模式）。
--   - 只记录成功（vip.grant / vip.extend）；拒绝（vip.reject）只落 audit_log，
--     不占幂等键——拒绝没有副作用，重试应重新评估。
--   - 幂等行与订阅变更、audit_log 行在同一事务提交，不存在半提交窗口。
--   - 全部 IF NOT EXISTS，重跑为 no-op。

CREATE TABLE IF NOT EXISTS admin_idempotency_keys (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    app_id     TEXT NOT NULL,
    key        TEXT NOT NULL,
    -- 'vip.grant' / 'vip.extend'
    action     TEXT NOT NULL,
    -- 'user:<uuid>'
    target     TEXT NOT NULL,
    -- 首次成功的 data 载荷，重放时原样返回
    response   JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (app_id, key)
);
