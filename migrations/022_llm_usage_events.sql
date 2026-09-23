-- 022_llm_usage_events.sql
-- Description: LLM 聊天计量流水表 — /chat 每完成一次上游调用落一行
-- 设计文档: docs/superpowers/plans/2026-09-07-multi-model-gateway.md
--
-- 口径:
--   - 一次 /chat 请求 = 一行;status 复用 handler 的 relay 结果
--     (ok / disconnected / upstream_error) —— 上游已 200 即计费,
--     断连/断流的已消耗 token 照常记录
--   - input_tokens/output_tokens 取自上游流末 usage 块(OpenAI
--     stream_options.include_usage;Anthropic 由翻译层合成);上游未上报
--     时记 0,行仍然落库(结构性不漏记)
--   - cost_micros = input_tokens*input_price_per_mtok
--     + output_tokens*output_price_per_mtok (微元,1e-6 CNY)
--
-- 隐私边界:不含消息内容(内容审计在 CHAT_LOG_PATH 的访问日志里,
-- 该日志可独立关闭);身份仅 user_id。
--
-- 幂等:不需要客户端幂等键 —— 每次真实上游调用都应计量,客户端重试
-- 产生的是第二次真实调用,两行都正确。

CREATE TABLE IF NOT EXISTS llm_usage_events (
    id             BIGSERIAL PRIMARY KEY,
    -- 删用户即删其计量数据,与 usage_events 一致
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id         TEXT NOT NULL REFERENCES apps(app_id),
    -- 逻辑模型 id(kaya 选的,如 deepseek-flash)+ 实际路由信息
    model          TEXT NOT NULL CHECK (length(model) <= 64),
    provider       TEXT NOT NULL CHECK (length(provider) <= 64),
    upstream_model TEXT NOT NULL CHECK (length(upstream_model) <= 128),
    status         TEXT NOT NULL CHECK (status IN ('ok', 'disconnected', 'upstream_error')),
    input_tokens   INT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens  INT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_micros    BIGINT NOT NULL DEFAULT 0 CHECK (cost_micros >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 单用户用量明细/额度核对
CREATE INDEX IF NOT EXISTS idx_llm_usage_events_user_created
    ON llm_usage_events (user_id, created_at);

-- 按时间+模型聚合(运营统计、成本核算)
CREATE INDEX IF NOT EXISTS idx_llm_usage_events_model_created
    ON llm_usage_events (model, created_at);
