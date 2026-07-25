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
-- Constraint name: hardcoded to `plan_change_log_plan_id_fkey` because
-- migration 012 declares the FK inline (`plan_id TEXT NOT NULL
-- REFERENCES plans(id) ON DELETE CASCADE`) without an explicit name,
-- so PostgreSQL falls back to its default `<table>_<column>_fkey`
-- convention. If 012 is ever edited to name the FK explicitly, this
-- name must be updated to match.
--
-- Idempotency: uses deterministic PG primitives (`DROP CONSTRAINT IF
-- EXISTS`, `ALTER COLUMN ... DROP NOT NULL` which is itself idempotent,
-- and a `pg_constraint` lookup guard for the re-add) instead of
-- `EXCEPTION WHEN OTHERS` error swallowing. Safe to re-run.

-- Drop the existing FK if it is present (default name from 012).
ALTER TABLE plan_change_log DROP CONSTRAINT IF EXISTS plan_change_log_plan_id_fkey;

-- plan_id becomes nullable. `ALTER COLUMN ... DROP NOT NULL` is itself
-- idempotent in PG, so no guard is needed.
ALTER TABLE plan_change_log ALTER COLUMN plan_id DROP NOT NULL;

-- Re-add the FK with ON DELETE SET NULL. PG has no `ADD CONSTRAINT IF
-- NOT EXISTS`, so we use the same `pg_constraint` lookup idiom as
-- 012_plan_commercial_fields.sql:37-53 to gate the ADD on absence.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'plan_change_log_plan_id_fkey' AND conrelid = 'plan_change_log'::regclass
    ) THEN
        ALTER TABLE plan_change_log
            ADD CONSTRAINT plan_change_log_plan_id_fkey
            FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE SET NULL;
    END IF;
END $$;