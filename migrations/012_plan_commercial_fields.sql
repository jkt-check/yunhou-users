-- Migration: 012_plan_commercial_fields
-- 2026-07-24: turn the plans table into a commercial product surface
-- (see docs/superpowers/specs/2026-07-24-plan-commercialization-design.md §5.1).
-- Adds is_listed, accepting_new_subscriptions, currency, trial_days,
-- description, display_order, updated_at on plans; CHECK constraints on
-- currency + trial_days; a BEFORE UPDATE trigger to maintain updated_at;
-- backfills the existing seed rows; and a new plan_change_log audit
-- table. Purely additive and idempotent — does NOT drop is_default or
-- touch 'free' semantics; that lives in 013.

-- (a) Add new columns
ALTER TABLE plans
    ADD COLUMN IF NOT EXISTS is_listed                   BOOLEAN     NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS accepting_new_subscriptions BOOLEAN     NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS currency                    TEXT        NOT NULL DEFAULT 'CNY',
    ADD COLUMN IF NOT EXISTS trial_days                  INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS description                 TEXT,
    ADD COLUMN IF NOT EXISTS display_order               INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now();

-- (b) Constraints
-- PostgreSQL does NOT support `ADD CONSTRAINT IF NOT EXISTS` (only
-- ADD COLUMN IF NOT EXISTS). Spec §5.1 wrote it that way — the
-- intent was idempotency; the standard PG idiom is a DO block
-- against pg_constraint, which is what we use here. Re-running this
-- migration on a partially-applied DB is therefore safe.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'plans_currency_supported' AND conrelid = 'plans'::regclass
    ) THEN
        ALTER TABLE plans ADD CONSTRAINT plans_currency_supported
            CHECK (currency IN ('CNY','USD','EUR'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'plans_trial_nonneg' AND conrelid = 'plans'::regclass
    ) THEN
        ALTER TABLE plans ADD CONSTRAINT plans_trial_nonneg
            CHECK (trial_days >= 0);
    END IF;
END $$;

-- (c) updated_at trigger
CREATE OR REPLACE FUNCTION plans_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS plans_touch_updated_at ON plans;
CREATE TRIGGER plans_touch_updated_at
    BEFORE UPDATE ON plans
    FOR EACH ROW EXECUTE FUNCTION plans_touch_updated_at();

-- (d) Backfill the existing 4 rows
UPDATE plans SET
    currency = 'CNY',
    trial_days = 0,
    description = CASE id
        WHEN 'monthly'   THEN '按月订阅 ¥29.9，自动续费，可随时取消'
        WHEN 'quarterly' THEN '按季订阅 ¥79.9，暂不开放新订阅，已有订阅保留'
        WHEN 'yearly'    THEN '按年订阅 ¥299（约 83 折，比月付年省 ¥59.8）'
        WHEN 'free'      THEN '免费版（已下线）'
    END,
    is_listed = true,
    accepting_new_subscriptions = CASE id WHEN 'free' THEN false WHEN 'quarterly' THEN false ELSE true END,
    display_order = CASE id
        WHEN 'monthly' THEN 10
        WHEN 'quarterly' THEN 20
        WHEN 'yearly' THEN 30
        ELSE 0
    END
WHERE id IN ('monthly','quarterly','yearly','free');

-- (e) plan_change_log (new audit table)
CREATE TABLE IF NOT EXISTS plan_change_log (
    id          BIGSERIAL PRIMARY KEY,
    plan_id     TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    actor_id    TEXT NOT NULL,                              -- 'admin:<appID>' or 'system:<job>'
    change_type TEXT NOT NULL CHECK (change_type IN ('apps_update','plan_create','plan_update','plan_deactivate','plan_archive')),
    before      JSONB NOT NULL,
    after       JSONB NOT NULL,
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_plan_change_log_plan_changed_at
    ON plan_change_log (plan_id, changed_at DESC);
