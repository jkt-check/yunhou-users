-- 044_order_attribution: optional marketing-attribution snapshot on orders.
-- POST /payments/orders accepts an optional `attribution` object
-- (first_touch / last_touch UTM + referrer + landing + captured_at); the
-- sanitized payload is persisted verbatim here. NULL for orders created
-- without attribution — i.e. all pre-044 rows and every order where the
-- website tracker had nothing to report.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS attribution JSONB;
