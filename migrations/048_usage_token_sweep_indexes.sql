-- 048_usage_token_sweep_indexes.sql — kaya token 用量统计保留期清理索引
-- (PR #61 review M2)
--
-- 背景:契约 v1.2 §4.9 的保留期 sweeper 是全表 DELETE
-- (DELETE FROM usage_token_buckets WHERE bucket_start < $1),不带
-- user_id 谓词,047 的 (user_id, bucket_start) 联合索引最左前缀用不上;
-- 墓碑清理按 to_bucket 过滤同样无索引。数据量上来后每轮全表扫。
--
-- 本文件只加索引;DELETE 改分批循环在 internal/repo/usage_token_repo.go
-- (SweepRetention),避免单语句长事务持锁。

CREATE INDEX IF NOT EXISTS idx_usage_token_buckets_sweep
    ON usage_token_buckets (bucket_start);

CREATE INDEX IF NOT EXISTS idx_usage_token_tombstones_sweep
    ON usage_token_tombstones (to_bucket);
