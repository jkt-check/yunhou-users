-- 046_analytics_outbox: durable retry queue for analytics delivery.
--
-- The PostHog emitter is fail-silent on the request path: a transport
-- error or non-2xx from /batch/ used to mean the event was gone for good.
-- Emitter.Capture now persists such failures here (INSERT ... ON CONFLICT
-- (uuid) DO NOTHING — the event's stable UUID is the idempotency key, so a
-- redelivery or a repeated failure of the same event never produces a
-- second row), and the RetryWorker in internal/analytics redelivers due
-- rows with exponential backoff. PostHog dedupes on uuid, so a late
-- redelivery is harmless even when the first attempt actually landed.
--
-- payload is the full internal/analytics/Event JSON, so a redelivery
-- carries the same event (JSONB normalizes key order/format, which is
-- harmless). Rows dead-lettered by the worker — attempts >= the
-- retryMaxAttempts constant in internal/analytics/outbox.go, or an
-- undecodable payload — are parked with next_attempt_at = 'infinity'
-- (kept for inspection, never picked up again).

CREATE TABLE IF NOT EXISTS analytics_outbox (
    uuid           TEXT PRIMARY KEY,
    event_name     TEXT NOT NULL,
    distinct_id    TEXT NOT NULL,
    payload        JSONB NOT NULL,
    attempts       INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS analytics_outbox_due_idx
    ON analytics_outbox (next_attempt_at)
    WHERE sent_at IS NULL;
