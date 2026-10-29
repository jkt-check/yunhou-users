-- 049_usage_events_occurred_at_index.sql
-- /admin/stats/* 统一按请求时区对 occurred_at 归日，补范围查询索引。
-- 旧 local_date 索引保留：上报字段与既有客户端契约不变。

CREATE INDEX IF NOT EXISTS idx_usage_events_occurred_at
    ON usage_events (occurred_at);
