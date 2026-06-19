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

-- 2. 修改 apps 表（简化版）
-- 先添加新列
ALTER TABLE apps ADD COLUMN IF NOT EXISTS app_id TEXT;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS description TEXT DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS config JSONB DEFAULT '{}';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS is_active BOOLEAN DEFAULT true;

-- 迁移数据：把 apps.id (UUID) 转为 apps.app_id (TEXT)
UPDATE apps SET app_id = id::TEXT;

-- 设置 app_id 为主键（临时，先加非空约束）
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

-- 3. 修改 subscriptions 表
-- 添加新列
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS plan_id TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ DEFAULT now();

-- 迁移数据：plan -> plan_id, 设置 started_at
UPDATE subscriptions SET plan_id = plan, started_at = created_at;

-- 设置非空约束
ALTER TABLE subscriptions ALTER COLUMN plan_id SET NOT NULL;
ALTER TABLE subscriptions ALTER COLUMN started_at SET NOT NULL;

-- 删除旧列（先删外键约束和索引）
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_app_id_fkey;
DROP INDEX IF EXISTS idx_subscriptions_app_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS app_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS plan;

-- 添加外键
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_id_fkey
    FOREIGN KEY (plan_id) REFERENCES plans(id);

-- 重建唯一约束（一个用户只能有一个活跃订阅）
DROP INDEX IF EXISTS idx_subscriptions_user_id;
DROP INDEX IF EXISTS idx_subscriptions_user_id_active;
CREATE UNIQUE INDEX idx_subscriptions_user_active
    ON subscriptions(user_id) WHERE status = 'active';

-- 4. 修改 sessions 表
-- 添加 app_id (TEXT) 列
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS app_id TEXT;

-- 迁移数据
UPDATE sessions SET app_id = app_id::TEXT;

-- 删除旧外键
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_app_id_fkey;

-- 删除旧 app_id 列（UUID 类型）
ALTER TABLE sessions DROP COLUMN IF EXISTS app_id;

-- 重命名新列
-- 实际上上面已经添加了 app_id，我们只是需要确保它是正确的类型
ALTER TABLE sessions ALTER COLUMN app_id TYPE TEXT USING app_id::TEXT;

-- 重建索引
DROP INDEX IF EXISTS idx_sessions_app_id;
DROP INDEX IF EXISTS idx_sessions_user_id;
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_app_id ON sessions(app_id);

COMMIT;
