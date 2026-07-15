-- Migration: 010_provider_intent_nullable
-- Description: change orders.provider_intent to nullable so non-wechat
--   orders don't return `provider_intent: {}` in JSON responses
--   (omitempty on Go's json.RawMessage only fires when the field is nil;
--   the previous DEFAULT '{}'::jsonb NOT NULL meant the column was always
--   non-nil on read, defeating omitempty). Migration 009 added the column
--   NOT NULL with a '{}' default for the original wechat_pay case; this
--   migration relaxes both so callers can leave the column unset on
--   INSERT and read it back as SQL NULL → nil RawMessage → no JSON field.

ALTER TABLE orders ALTER COLUMN provider_intent DROP NOT NULL;
ALTER TABLE orders ALTER COLUMN provider_intent SET DEFAULT NULL;