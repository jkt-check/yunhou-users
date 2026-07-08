-- Migration 008: drop 'lemonsqueezy' from the channel CHECK constraints.
--
-- The LemonSqueezy payment channel was removed from the application code in
-- commit d8f333d. Migrations 004_ls_channel and 005_paypal_channel had extended
-- the CHECK constraints to allow channel='lemonsqueezy'. We now drop and
-- re-add the constraints without that value, so:
--
--   1. Operators following CLAUDE.md cannot configure a channel the app does
--      not process (the webhook handler returns 404 for any unknown channel).
--   2. The schema, the docs, and the handler agree on the same channel set.
--
-- This migration is idempotent against fresh installs (those rows never had
-- 'lemonsqueezy' in the constraint) and against existing installs that
-- already had it (replaces the constraint definition).
--
-- Apply AFTER 007_app_secret.

BEGIN;

-- payments.channel
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_channel_check;
ALTER TABLE payments ADD CONSTRAINT payments_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal'));

-- refunds.channel
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS refunds_channel_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal'));

-- webhook_events.channel
ALTER TABLE webhook_events DROP CONSTRAINT IF EXISTS webhook_events_channel_check;
ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal'));

COMMIT;