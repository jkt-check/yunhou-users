-- Migration: 042_paddle_channel
-- Description: extend payments/refunds/webhook_events CHECK constraints to allow channel='paddle'.
-- 设计文档: docs/plans/2026-10-01-paddle-integration-research.md
--
-- 照 005/008 的 DO 块幂等模式(deploy.sh 会无差别重放;PG 不支持原位修改
-- CHECK 表达式,DROP + ADD)。约束列表以 008_drop_lemonsqueezy 为准 + 'paddle'。
DO $$
BEGIN
    ALTER TABLE payments DROP CONSTRAINT payments_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE payments ADD CONSTRAINT payments_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE refunds DROP CONSTRAINT refunds_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;
