-- Migration: 027_subscription_product_scope
-- Description: 套餐与订阅按商业产品隔离（product_code）；活跃订阅唯一约束升级为 (user_id, product_code)
-- 设计文档: docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md §4.1
--
-- 口径:
--   - product_code 是商业产品归属，与表示登录入口的 app_id 分离。
--     'kaya-membership' 为现有会员的兼容产品；'coding-plan' 为独立模型 API 套餐
--     （本阶段不在售：代码支持双产品落库，售卖入口由后续任务引入）。
--   - plans.product_code 为 NOT NULL DEFAULT 'kaya-membership'（套餐无触发器，
--     DEFAULT 是其兼容路径）；subscriptions.product_code 为 NOT NULL，存量行由
--     ADD COLUMN ... DEFAULT 一次性回填后 DROP DEFAULT，新写入由触发器
--     以套餐行为准回填/校验（省略 → 回填；显式不匹配 → 报错）。
--   - 套餐↔订阅的产品一致性由触发器 subscriptions_enforce_plan_product 在
--     数据库层保证（plans 主键是单列 id，无法使用组合外键；触发器即
--     "数据库约束或等效保证" 的落地形态）。INSERT/UPDATE 订阅时：
--     product_code 缺省（NULL/''）→ 以套餐行为准回填；显式提供但不匹配 → 报错。
--   - 002 的 idx_subscriptions_user_active（每用户全局一个活跃订阅）替换为
--     idx_subscriptions_user_product_active（每用户每产品一个活跃订阅）。

ALTER TABLE plans ADD COLUMN IF NOT EXISTS product_code TEXT NOT NULL DEFAULT 'kaya-membership';
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS product_code TEXT NOT NULL DEFAULT 'kaya-membership';

-- 防御性对齐：订阅行跟随其套餐行的产品归属（DEFAULT 已覆盖全部存量行；
-- 此 UPDATE 兜底任何在迁移间隙发生的计划侧变更）。按 subscriptions_plan_id_fkey
-- (RESTRICT)，每个订阅的 plan_id 必有对应 plans 行，不会出现 join 丢行。
UPDATE subscriptions s SET product_code = p.product_code
  FROM plans p
 WHERE s.plan_id = p.id
   AND s.product_code IS DISTINCT FROM p.product_code;

-- 回填完成后撤掉 subscriptions.product_code 的列默认值：否则新增订阅省略
-- product_code 时会先被 DEFAULT 写成 'kaya-membership'，触发器看到的就不再是
-- NULL/''，coding-plan 套餐的订阅会被静默归错产品或被误判为不匹配。新写入一律
-- 由 trg_subscriptions_plan_product 触发器以套餐行为准回填/校验。
-- （plans.product_code 保留默认值——套餐没有触发器，DEFAULT 是它的兼容路径。）
ALTER TABLE subscriptions ALTER COLUMN product_code DROP DEFAULT;

-- 迁移前异常数据诊断：新部分唯一索引要求每 (user_id, product_code) 至多一个
-- status='active' 的订阅。旧全局索引在位时该状态不可能自然产生，但人工修数
-- 可能绕过。此处输出可读的违规清单并中止迁移（整个文件在 cmd/migrate 的
-- 单事务内执行，RAISE EXCEPTION 会回滚且不落 ledger），而不是让
-- CREATE UNIQUE INDEX 抛一条没有上下文的 unique_violation。
DO $$
DECLARE
    v_bad    RECORD;
    v_detail TEXT := '';
BEGIN
    FOR v_bad IN
        SELECT user_id, product_code, COUNT(*) AS n,
               string_agg(id::text, ', ' ORDER BY created_at) AS sub_ids
          FROM subscriptions
         WHERE status = 'active'
         GROUP BY user_id, product_code
        HAVING COUNT(*) > 1
    LOOP
        v_detail := v_detail || format(
            'user_id=%s product_code=%s active_count=%s subscription_ids=[%s]',
            v_bad.user_id, v_bad.product_code, v_bad.n, v_bad.sub_ids) || E'\n';
    END LOOP;
    IF v_detail <> '' THEN
        RAISE EXCEPTION '027_subscription_product_scope: duplicate active subscriptions per (user_id, product_code); resolve manually (keep at most one active per pair) and re-run. Offending rows:%',
            E'\n' || v_detail;
    END IF;
END $$;

-- 替换唯一约束：全局 "每用户一个活跃订阅" → "每用户每产品一个活跃订阅"。
DROP INDEX IF EXISTS idx_subscriptions_user_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_product_active
    ON subscriptions (user_id, product_code) WHERE status = 'active';

-- 套餐↔订阅产品一致性触发器（幂等：OR REPLACE + DROP IF EXISTS 后重建）。
CREATE OR REPLACE FUNCTION subscriptions_enforce_plan_product() RETURNS trigger AS $fn$
DECLARE
    v_product TEXT;
BEGIN
    SELECT product_code INTO v_product FROM plans WHERE id = NEW.plan_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'subscriptions.plan_id % references a missing plans row', NEW.plan_id;
    END IF;
    IF NEW.product_code IS NULL OR NEW.product_code = '' THEN
        NEW.product_code := v_product;
    ELSIF NEW.product_code <> v_product THEN
        RAISE EXCEPTION 'subscriptions.product_code % does not match plan % (product_code %)',
            NEW.product_code, NEW.plan_id, v_product;
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_subscriptions_plan_product ON subscriptions;
CREATE TRIGGER trg_subscriptions_plan_product
    BEFORE INSERT OR UPDATE OF plan_id, product_code ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION subscriptions_enforce_plan_product();
