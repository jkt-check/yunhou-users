-- 039_bulk_import_document_hash.sql
-- Description: inference_bulk_imports 增 document_hash 列 —— 批量导入
--   task_id 幂等重放校验文档一致性（安全评审 M-4，对齐 wallet adjustments /
--   admin idempotency 的同键异载荷 409 语义）。
--
-- 口径:
--   - 新提交的任务行必带 sha256(canonical JSON of BulkCatalog) 摘要
--     （management.BulkDocumentHash 计算，写路径与重放校验同一函数）。
--   - 重放时摘要不一致 → 409（调用方 task_id 复用错误，不得静默返回
--     旧任务的结果——旧结果对应的是另一份文档）。
--   - 列可空：034 时代的存量行无摘要，重放时按 NULL 跳过比对
--     （不做破坏性假设，存量 task_id 的既有重放语义不变）。
--   - IF NOT EXISTS，重跑为 no-op。

ALTER TABLE inference_bulk_imports
    ADD COLUMN IF NOT EXISTS document_hash TEXT;
