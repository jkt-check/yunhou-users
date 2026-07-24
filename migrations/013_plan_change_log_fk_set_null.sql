-- Migration: 013_plan_change_log_fk_set_null
-- 2026-07-25: relax plan_change_log.plan_id FK so hard-deleted plans
-- preserve their audit history (see docs/superpowers/specs/2026-07-24-plan-commercialization-design.md).
--
-- Migration 012 created plan_change_log with
--     plan_id TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE
-- and the spec baked the assumption that plan deletion would be a
-- follow-up migration (`014_hard_delete_free.sql`) applied long after
-- any plan_delete audit row was wanted. The T6 code-quality review
-- surfaced a latent bug: PlanService.DeletePlan (commit d899a6b) does
-- the delete FIRST and then tries to INSERT the audit row, so the FK
-- is satisfied against a row that no longer exists and the INSERT
-- fails. The service uses a best-effort `_ = ...Insert(...)`, so the
-- audit row silently never lands — every plan_archive audit is lost.
--
-- Fix:
--   * plan_id becomes nullable.
--   * FK changes to ON DELETE SET NULL, so an existing audit row
--     survives a hard delete of its plan; the plan_id column becomes
--     NULL (history is preserved without dangling references).
--
-- PlanService.DeletePlan (internal/service/plan.go) is also reordered
-- to insert the audit row BEFORE the hard delete so the (now-relaxed)
-- FK is satisfied at INSERT time; the new FK only matters on the
-- subsequent DELETE.
--
-- Idempotency: uses the pg_constraint lookup pattern from
-- 012_plan_commercial_fields.sql:27-43 — safe to re-run.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'plan_change_log_plan_id_fkey' AND conrelid = 'plan_change_log'::regclass
    ) THEN
        ALTER TABLE plan_change_log DROP CONSTRAINT plan_change_log_plan_id_fkey;
    END IF;
END $$;

-- Drop NOT NULL only if it's currently enforced (idempotent against a
-- partial-apply order). ALTER TABLE ... ALTER COLUMN ... DROP NOT NULL
-- is itself idempotent in PG, but wrapping it keeps the file symmetric
-- with the constraint add below.
DO $$
BEGIN
    ALTER TABLE plan_change_log ALTER COLUMN plan_id DROP NOT NULL;
EXCEPTION
    WHEN OTHERS THEN NULL;
END $$;

-- Re-add the FK with ON DELETE SET NULL. Same EXCEPTION WHEN
-- duplicate_object pattern as 005_paypal_channel.sql so re-runs are
-- safe: if the constraint already exists with the new shape (e.g. a
-- hand-fix landed first), the DO block swallows the duplicate_object
-- error.
DO $$
BEGIN
    ALTER TABLE plan_change_log
        ADD CONSTRAINT plan_change_log_plan_id_fkey
        FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE SET NULL;
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;