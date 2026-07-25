-- Migration: 015_plan_change_log_nullable_snapshots
-- 2026-07-25: relax plan_change_log.before / plan_change_log.after from
-- NOT NULL to nullable. Spec §6.1 (docs/superpowers/specs/2026-07-24-
-- plan-commercialization-design.md) explicitly requires this:
--   * CreatePlan writes the audit row with `before=NULL` (no prior state).
--   * DeletePlan writes the audit row with `after=NULL` (no successor state).
-- Migration 012 declared both columns NOT NULL, which silently blocks these
-- two write paths now that audit-insert errors surface (post-D3 atomic-
-- plan-mutation fix in commit 0a477ad). On CreatePlan / DeletePlan the
-- NOT NULL violation rolls the whole tx back, blocking the operation.
--
-- Pure DDL fix — no data backfill is needed; existing rows already satisfy
-- NOT NULL (only Create/Delete ever violate it, and they would have rolled
-- back before this migration landed). The ALTER ... DROP NOT NULL form
-- is idempotent in PostgreSQL, so re-running this file is a no-op.

ALTER TABLE plan_change_log ALTER COLUMN before DROP NOT NULL;
ALTER TABLE plan_change_log ALTER COLUMN after  DROP NOT NULL;