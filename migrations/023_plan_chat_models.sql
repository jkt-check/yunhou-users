-- 023_plan_chat_models.sql
-- Description: plans.chat_models — 套餐级内置模型白名单
-- 设计文档: docs/superpowers/plans/2026-09-07-multi-model-gateway.md
--
-- NULL(默认) = 不限制,目录内所有模型可用;
-- 非空数组 = 仅列出的逻辑模型 id 可用(如 '{deepseek-flash,kimi-k3}')。
-- 运维初期通过 SQL 维护,后台管理界面后续另起。

ALTER TABLE plans ADD COLUMN IF NOT EXISTS chat_models TEXT[] NULL;

COMMENT ON COLUMN plans.chat_models IS '套餐可用的内置聊天模型 id 白名单;NULL = 不限制';
