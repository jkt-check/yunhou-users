-- 2026-07-23: add orders.last_reconciled_at to support the active
-- wechat_pay QueryOrder reconcile path on GetOrder (see
-- service/payment.go shouldReconcile). Default now() so the column is
-- populated on existing rows (without this they'd all have NULL, the
-- `time.Since(...)` check would compare against zero, and every poll
-- would trigger a refresh). NOT surfaced via the JSON response
-- (model/order.go marks it json:"-").

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS last_reconciled_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS orders_last_reconciled_at_idx
    ON orders (last_reconciled_at);