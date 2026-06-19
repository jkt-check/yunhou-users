-- Migration: 002_simplify_plans
-- Description: 简化订阅系统，Plan 包含可访问的 App 列表，统一账号订阅

BEGIN;

-- 1. 创建 plans 表
CREATE TABLE IF NOT EXISTS plans (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    price         DECIMAL(10,2) DEFAULT 0,
    interval_days INT DEFAULT 0,
    apps          TEXT[] NOT NULL DEFAULT '{}',
    is_active     BOOLEAN NOT NULL DEFAULT true,
    is_default    BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 插入默认 plans
INSERT INTO plans (id, name, price, interval_days, apps, is_default) VALUES
    ('free',     '免费',     0,     0,   ARRAY['yundian'],                    true),
    ('monthly',  '按月订阅', 29.9,  30,  ARRAY['yundian', 'yundash'],       false),
    ('quarterly','按季订阅', 79.9,  90,  ARRAY['yundian', 'yundash'],       false),
    ('yearly',   '按年订阅', 299,   365, ARRAY['yundian', 'yundash'],       false);

-- 2. 修改 subscriptions 表（先删外键和索引，因为它们依赖 apps_pkey）
-- 删除旧外键
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_app_id_fkey;
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_app_id_fkey;

-- 删除旧索引
DROP INDEX IF EXISTS idx_subscriptions_app_id;
DROP INDEX IF EXISTS idx_sessions_app_id;

-- 添加新列
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS plan_id TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ DEFAULT now();

-- 迁移数据：plan -> plan_id, 设置 started_at
UPDATE subscriptions SET plan_id = plan, started_at = created_at;

-- 设置非空约束
ALTER TABLE subscriptions ALTER COLUMN plan_id SET NOT NULL;
ALTER TABLE subscriptions ALTER COLUMN started_at SET NOT NULL;

-- 删除旧列
ALTER TABLE subscriptions DROP COLUMN IF EXISTS app_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS plan;

-- 添加新外键
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_id_fkey
    FOREIGN KEY (plan_id) REFERENCES plans(id);

-- 重建唯一约束（一个用户只能有一个活跃订阅）
DROP INDEX IF EXISTS idx_subscriptions_user_id;
DROP INDEX IF EXISTS idx_subscriptions_user_id_active;
CREATE UNIQUE INDEX idx_subscriptions_user_active
    ON subscriptions(user_id) WHERE status = 'active';

-- 3. 修改 apps 表（简化版）
-- 先添加新列
ALTER TABLE apps ADD COLUMN IF NOT EXISTS app_id TEXT;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS description TEXT DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS config JSONB DEFAULT '{}';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS is_active BOOLEAN DEFAULT true;

-- 迁移数据：把 apps.id (UUID) 转为 apps.app_id (TEXT)
UPDATE apps SET app_id = id::TEXT;

-- 设置 app_id 为主键（先加非空约束）
ALTER TABLE apps ALTER COLUMN app_id SET NOT NULL;

-- 添加主键（需要先删除旧主键）
ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_pkey;
ALTER TABLE apps ADD PRIMARY KEY (app_id);

-- 删除旧列
ALTER TABLE apps DROP COLUMN IF EXISTS id;
ALTER TABLE apps DROP COLUMN IF EXISTS secret;
ALTER TABLE apps DROP COLUMN IF EXISTS redirect_uris;
ALTER TABLE apps DROP COLUMN IF EXISTS providers;
ALTER TABLE apps DROP COLUMN IF EXISTS default_plan;

-- 4. 修改 sessions 表
-- 重命名旧列并添加新列（避免类型冲突）
ALTER TABLE sessions RENAME COLUMN app_id TO app_id_old;
ALTER TABLE sessions ADD COLUMN app_id TEXT;
UPDATE sessions SET app_id = app_id_old::TEXT;
ALTER TABLE sessions DROP COLUMN app_id_old;

-- 重建索引
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_app_id ON sessions(app_id);

COMMIT;
