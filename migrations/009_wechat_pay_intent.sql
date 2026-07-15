-- Migration: 009_wechat_pay_intent
-- Description: add orders.provider_intent JSONB for channel-specific pre-auth metadata.
--   wechat_pay NATIVE → {code_url, out_trade_no, mch_id}
--   paypal            → (reserved for future use)
-- 设计文档: docs/superpowers/specs/2026-07-15-wechat-pay-real-client-design.md

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS provider_intent JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN orders.provider_intent IS
    'Per-channel provider metadata written after channel-specific pre-auth: '
    'wechat_pay → {code_url, out_trade_no, mch_id}; paypal → ...';
