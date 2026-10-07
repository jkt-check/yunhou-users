-- 045: subscriptions.channel + subscriptions.auto_renew (Paddle subscription
-- self-management, M1).
--
-- channel: which payment channel manages this subscription's lifecycle
-- ('paddle' / 'paypal' / 'wechat_pay'; NULL = free self-serve / trial /
-- admin-grant — no channel-side billing relationship).
-- auto_renew: true while the channel will bill the buyer automatically;
-- false after a cancel (or subscription.canceled webhook) and always for
-- non-channel subs.

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS channel TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS auto_renew BOOLEAN NOT NULL DEFAULT false;

-- Backfill (idempotent — safe to re-run).
-- Channel was previously inferred from the external_subscription_id prefix
-- (internal/service/paddle_subscription.go): 'sub_' = Paddle Billing,
-- 'I-' = PayPal. These two MUST be exact — the self-management feature keys
-- on channel.
UPDATE subscriptions
SET channel = 'paddle'
WHERE channel IS NULL
  AND substring(external_subscription_id from 1 for 4) = 'sub_';

UPDATE subscriptions
SET channel = 'paypal'
WHERE channel IS NULL
  AND substring(external_subscription_id from 1 for 2) = 'I-';

-- Auto-renewing channels with a live subscription renew automatically.
UPDATE subscriptions
SET auto_renew = true
WHERE auto_renew = false
  AND channel IN ('paddle', 'paypal')
  AND status = 'active';

-- WeChat backfill is deliberately SKIPPED: matching legacy wechat_pay orders
-- to subscription rows would need a correlated join on (user_id, plan_id,
-- started_at) that is unreliable for users with repeat purchases, and a
-- wrong guess is worse than no guess — NULL already reads as "not managed
-- by an auto-renewing channel", which is the only thing the feature gates
-- on. Going-forward writes stamp channel='wechat_pay' on activation, so new
-- WeChat subs are covered.
