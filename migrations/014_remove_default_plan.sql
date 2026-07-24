-- Migration: 014_remove_default_plan
-- 2026-07-25: retire the default-plan concept and the legacy free plan
-- after verifying no active subscriptions still reference plan_id='free'.
-- Drops the plans_one_default partial unique index and is_default column.
-- Idempotent where PostgreSQL supports IF EXISTS; the pre-check aborts when
-- active free-plan subscriptions would be orphaned by retiring the plan.

DO $$
DECLARE cnt INT;
BEGIN
    SELECT COUNT(*) INTO cnt
    FROM subscriptions
    WHERE plan_id = 'free' AND status = 'active';

    IF cnt > 0 THEN
        RAISE EXCEPTION 'migration aborted: % active subscriptions still reference plan_id=free; cancel them first', cnt;
    END IF;
END $$;

UPDATE plans
SET is_active = false,
    accepting_new_subscriptions = false
WHERE id = 'free';

DROP INDEX IF EXISTS plans_one_default;

ALTER TABLE plans
    DROP COLUMN IF EXISTS is_default;
