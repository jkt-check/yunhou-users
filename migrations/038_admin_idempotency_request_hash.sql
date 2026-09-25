-- 038_admin_idempotency_request_hash.sql
-- Description: admin_idempotency_keys 增 request_hash 列 —— VIP 幂等重放
--   校验载荷一致性（评审 I-8，对齐 wallet adjustments 面
--   admin_adjustments.go 的同键异载荷 409 语义）。
-- 规格: dashboard-admin-api-spec.md §5.3；参照 036_adjustments_source.sql 口径。
--
-- 口径:
--   - 新写入必带 sha256(user_id, days) 摘要（service 层计算）。
--   - 重放时摘要不一致 → 409（调用方键复用错误，不当良性重放）。
--   - 列可空：037 时代的存量行无摘要，重放时按 NULL 跳过比对
--     （不做破坏性假设，存量键的既有重放语义不变）。
--   - 只记录成功的行的口径不变（037）；拒绝不占幂等键。
--   - IF NOT EXISTS，重跑为 no-op。

ALTER TABLE admin_idempotency_keys
    ADD COLUMN IF NOT EXISTS request_hash TEXT;
