-- Migration: 006_paypal_sub_mapping
-- Description: add subscriptions.external_subscription_id (PayPal subscription ID) + partial UNIQUE index.
-- 设计文档: docs/superpowers/specs/2026-07-01-paypal-channel-design.md

BEGIN;

ALTER TABLE subscriptions ADD COLUMN external_subscription_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_external_sub_id
    ON subscriptions (external_subscription_id)
    WHERE external_subscription_id IS NOT NULL;

COMMIT;
