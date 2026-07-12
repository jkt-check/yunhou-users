-- Migration: 006_paypal_sub_mapping
-- Description: add subscriptions.external_subscription_id (PayPal subscription ID) + partial UNIQUE index.
-- 设计文档: docs/superpowers/specs/2026-07-01-paypal-channel-design.md
--
-- The partial UNIQUE index (NOT NULL rows only) prevents two subscriptions
-- from sharing the same PayPal subscription ID. PayPal guarantees unique
-- billing_agreement_id values per subscription, so the index is a
-- defence-in-depth backstop — if a bug elsewhere inserts a duplicate
-- external_subscription_id, the constraint rejects the second INSERT.
--
-- Renewal handlers SELECT by external_subscription_id (FOR UPDATE) rather
-- than INSERT, so duplicate-key errors do not surface in the renewal
-- path. The only INSERT that touches this column is in
-- onPaymentSucceeded (payment.go:769-779), which uses a subquery filtered
-- by user_id + plan_id + status='active', so the partial index cannot
-- collide there either.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS external_subscription_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_external_sub_id
    ON subscriptions (external_subscription_id)
    WHERE external_subscription_id IS NOT NULL;
