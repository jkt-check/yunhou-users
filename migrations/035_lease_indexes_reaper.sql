-- 035_lease_indexes_reaper.sql
-- Description: 租约热路径索引 + 终态租约收割支撑索引 + 钱包月度支出索引（评审修复批次8）
--
-- 背景:
--   - AcquireLeaseTx 此前对 (scope, scope_id) 全历史做聚合：fencing 的
--     MAX(fencing_token) 无法用 026 的部分索引（WHERE state='held'）服务，
--     迫使扫描该 scope 有史以来全部租约行；且无任何 job 删除终态租约，
--     繁忙账户 scope 每请求一行永久累积，最热准入路径成本随账户生命周期
--     线性增长。
--   - monthSpendLocked（钱包准入路径）按 wallet_id + created_at 范围过滤，
--     唯一索引是 (wallet_id, id DESC)，月度支出派生缺支撑索引。
--
-- 口径:
--   - fencing 单调性只需对当前 held 行成立：续租/释放/使用点检查都按行级
--     (id, owner_token, fencing_token) 精确匹配，终态行被收割后旧持有者的
--     一切所有权断言天然失败，不存在跨收割复用授权的窗口。收割逻辑在
--     internal/inference/postgres/lease_repo.go（DeleteTerminalLeases），
--     挂 upstream_health 轮次（与绑定/会话链 TTL 清扫同处）。
--   - 全部 IF NOT EXISTS，重跑为 no-op。

-- MAX(fencing_token) 走索引首行（DESC 序）即可，不再扫描 scope 全历史。
CREATE INDEX IF NOT EXISTS idx_inference_concurrency_leases_fencing
    ON inference_concurrency_leases (scope, scope_id, fencing_token DESC);

-- 终态租约收割（state IN ('released','expired') 且终态时间早于保留窗口）：
-- released 行以 released_at 为终态时刻，expired 行（超时回收，released_at
-- 为 NULL）以 expires_at 为终态时刻。
CREATE INDEX IF NOT EXISTS idx_inference_concurrency_leases_terminal
    ON inference_concurrency_leases (COALESCE(released_at, expires_at))
    WHERE state IN ('released', 'expired');

-- 钱包月度支出派生（monthSpendLocked）：按 wallet_id + created_at 范围过滤。
-- 不用部分索引：查询的 entry_type 过滤在 CASE 表达式里（wallet_repo.go 只读
-- 不改），WHERE 子句不蕴含 entry_type 谓词，部分索引永远不会被选中。
CREATE INDEX IF NOT EXISTS idx_inference_wallet_entries_wallet_created
    ON inference_wallet_entries (wallet_id, created_at);
