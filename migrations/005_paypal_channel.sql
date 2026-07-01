-- Migration: 005_paypal_channel
-- Description: extend payments/refunds/webhook_events CHECK constraints to allow channel='paypal'.
-- 设计文档: docs/superpowers/specs/2026-07-01-paypal-channel-design.md
--
-- 约束名沿用 PG 默认命名（003_payments.sql 里的 inline CHECK 没
-- 显式命名，PG 自动给的）。DROP + ADD 而不是 ALTER — PG 不支持原位修改 CHECK 表达式。

BEGIN;

ALTER TABLE payments DROP CONSTRAINT payments_channel_check;
ALTER TABLE payments ADD CONSTRAINT payments_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

ALTER TABLE refunds DROP CONSTRAINT refunds_channel_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_channel_check;
ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy', 'paypal'));

COMMIT;
