-- Correction receipts are metering/audit facts, not financial entries.
-- All new settlement corrections share this key, including zero-money
-- corrections; financial adjustments retain their strictly positive amount.
-- Legacy corrections keep their keys in inference_adjustments and are still
-- recognized by the repository. Apply before using the new correction code.
CREATE TABLE IF NOT EXISTS inference_usage_corrections (
    idempotency_key TEXT PRIMARY KEY CHECK (idempotency_key <> ''),
    request_id UUID NOT NULL REFERENCES inference_requests (id),
    usage_record_id UUID NOT NULL UNIQUE REFERENCES inference_usage_records (id),
    corrected_micros BIGINT NOT NULL CHECK (corrected_micros >= 0),
    corrected_tokens BIGINT CHECK (corrected_tokens >= 0),
    reason TEXT NOT NULL CHECK (reason <> ''),
    operator_subject TEXT NOT NULL CHECK (operator_subject <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inference_usage_corrections_request
    ON inference_usage_corrections (request_id, created_at);
