-- 034_inference_operations.sql
-- Description: 运营面支撑 — 批量导入幂等任务表 + 运营统计/异常筛选的查询计划索引（Task 15）
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §5、§9.2
--
-- 口径:
--   - inference_bulk_imports 是批量导入的幂等任务注册表：commit 与目录写入在
--     同一事务（全部有效才落库，绝不半发布）；重复提交撞 task_id 唯一键后
--     重读已记录的结果，不重复创建。
--   - 统计/异常筛选全部直读权威表（inference_requests/attempts/usage_records/
--     ledger_entries），绝不使用 usage_events 心跳表；本文件只为真实查询
--     计划补索引，不引入外部分析库。额度闸门不受这些索引影响（只读派生路径）。

CREATE TABLE IF NOT EXISTS inference_bulk_imports (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- 幂等任务 ID（运营客户端提供；重复提交只生效一次，响应重放已记录结果）
    task_id    TEXT NOT NULL UNIQUE CHECK (task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    -- 人员及服务双重归因（"user:<uid>@app:<appid>"，设计 §9.2）
    actor      TEXT NOT NULL,
    -- 发布结果快照：逐项自然键 → 行 ID/状态 映射与计数（可追踪发布结果）
    summary    JSONB NOT NULL,
    item_count INT NOT NULL CHECK (item_count >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 运营统计的时间范围扫描（每模型/供应商/客户分组、异常预占筛选都先按
-- created_at 半开区间收敛；账户/模型/Key 维度的既有索引服务客户读面，
-- 运营面是跨账户扫描）。只读派生路径，不在写路径热点上加复合索引。
CREATE INDEX IF NOT EXISTS idx_inference_requests_created
    ON inference_requests (created_at);

-- 成本聚合按尝试落桶（cost_micros/cost_basis/currency 区分 reported/
-- estimated/allocated 与 unknown）；attempts 的 created_at 范围扫描 +
-- deployment 分组。
CREATE INDEX IF NOT EXISTS idx_inference_attempts_created
    ON inference_attempts (created_at);
