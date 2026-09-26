-- 040_policy_superseded.sql — 配额策略管理面 Q3 拍板(需求
-- yunhou-users-quota-policies-api-requirements.md §4.5 选项 a;设计
-- docs/superpowers/specs/2026-09-26-admin-quota-policies-design.md):
-- publish 同事务将同名旧 published 转 superseded、读侧只认 published,
-- 与 catalog revision 语义对齐。
--
-- 1) status CHECK 扩 'superseded'(现状只有 draft/published/retired)。
-- 2) 每 name 至多一条 published 的部分唯一索引 —— 写侧同事务转换是常态
--    路径,此索引是并发/缺陷的 DB 层兜底(第二个 INSERT/UPDATE 到
--    published 会直接 23505 而非静默双发布)。

ALTER TABLE inference_policy_versions
    DROP CONSTRAINT IF EXISTS inference_policy_versions_status_check;
ALTER TABLE inference_policy_versions
    ADD CONSTRAINT inference_policy_versions_status_check
    CHECK (status IN ('draft', 'published', 'superseded', 'retired'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_inference_policy_versions_one_published
    ON inference_policy_versions (name) WHERE status = 'published';
