-- 033_inference_response_chains.sql
-- Description: OpenAI Responses 会话链持久化（Task 13）
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §8/§9.1
--
-- 口径:
--   - previous_response_id 接续在语义上 = 服务端回放既有上下文 + 新输入。
--     本表持久化每一轮的规范 Responses items（截至该轮的完整会话内容），
--     使接续语义成立——不是"仅记录绑定、让上游猜上下文"。
--   - 账户归属与上游账号归属都落库：billing_account_id 用于跨客户隔离
--     （查询按账户限定，跨客户引用与不存在无差别）；upstream_account_id
--     记录本轮实际服务账号（审计可读）；活跃钉住关系仍以
--     inference_session_bindings（032）为唯一权威。
--   - store:false 的请求不落链（OpenAI 语义：不被后续 previous_response_id
--     引用）；过期行按 TTL 清扫，链接续到过期行与不存在无差别。
--   - transcript 有大小上限（见 providers 层常量），超上限的请求正常服务
--     但不落链——引用它的后续请求得到明确的 404，不静默丢上下文。

CREATE TABLE IF NOT EXISTS inference_response_chains (
    -- 公开响应 id（resp_<hex>），客户持有并可能引用；不暴露内部 request id
    id                  TEXT PRIMARY KEY,
    -- 链首响应 id：整条链共享，作为 session_bindings 的 session_key
    chain_id            TEXT NOT NULL,
    billing_account_id  UUID NOT NULL REFERENCES inference_billing_accounts (id),
    request_id          UUID NOT NULL REFERENCES inference_requests (id),
    model_id            TEXT NOT NULL REFERENCES inference_models (id),
    -- 本轮实际服务的上游账号（审计/归属口径；活跃绑定以 session_bindings 为准）
    upstream_account_id UUID NOT NULL REFERENCES inference_upstream_accounts (id),
    -- 规范 Responses items JSON 数组：截至本轮的全部输入 + 本轮输出
    transcript          JSONB NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL
);

-- 跨客户隔离查询与运营排查
CREATE INDEX IF NOT EXISTS idx_inference_response_chains_account
    ON inference_response_chains (billing_account_id, created_at DESC);

-- TTL 清扫（upstream_health 轮次顺带执行）
CREATE INDEX IF NOT EXISTS idx_inference_response_chains_expiry
    ON inference_response_chains (expires_at);
