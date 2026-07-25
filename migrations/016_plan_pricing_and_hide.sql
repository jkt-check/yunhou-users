-- Migration: 016_plan_pricing_and_hide
-- 2026-07-25: align plans.price with the yunhou-website frontend promo
-- (see docs/superpowers/specs/2026-07-25-plan-pricing-realignment-design.md).
--   - monthly ¥29.9/月 → ¥19.9/月
--   - yearly  ¥299/年  → ¥199.9/年
--   - quarterly: is_listed=false, is_active=false (full retirement;
--     historical plan_id preserved in LoginResponse.subscription.plan_id)
--   - free: is_listed=false (already is_active=false since migration 014)
--
-- Idempotency: each UPDATE is keyed on a specific id literal; non-matching
-- rows leave the column untouched. Re-running after first apply writes the
-- same value back (semantic no-op), but bumps updated_at via the trigger
-- from migration 012 — operators who care about a clean updated_at
-- timeline should run once.

-- (a) Re-price the two surviving public plans
UPDATE plans SET price = CASE id
    WHEN 'monthly' THEN 19.9
    WHEN 'yearly'  THEN 199.9
    ELSE price
END
WHERE id IN ('monthly','yearly');

-- (b) Retire quarterly fully (is_listed=false + is_active=false)
UPDATE plans SET is_listed = false WHERE id = 'quarterly';
UPDATE plans SET is_active = false WHERE id = 'quarterly';

-- (c) Hide free from the public catalog
UPDATE plans SET is_listed = false WHERE id = 'free';