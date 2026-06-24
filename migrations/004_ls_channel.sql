-- Migration: 004_ls_channel
-- Description: extend payments/refunds/webhook_events CHECK constraints to allow channel='lemonsqueezy'.
-- 设计文档: docs/plans/2026-06-23-payment-webhook-mechanism.md
--
-- 约束名沿用 PG 默认命名 <table>_<col>_check（003_payments.sql 里的 inline CHECK 没
-- 显式命名，PG 自动给的）。DROP + ADD 而不是 ALTER — PG 不支持原位修改 CHECK 表达式。

BEGIN;

ALTER TABLE payments DROP CONSTRAINT payments_channel_check;
ALTER TABLE payments ADD CONSTRAINT payments_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy'));

ALTER TABLE refunds DROP CONSTRAINT refunds_channel_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy'));

ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_channel_check;
ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
    CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'lemonsqueezy'));

COMMIT;