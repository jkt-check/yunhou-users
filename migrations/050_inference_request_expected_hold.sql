-- 050_inference_request_expected_hold.sql
-- Description: T2（无窗口结算与冲突分类）的支撑 DDL —— 只追加，不改历史
--   迁移、不回写历史账本行。
--
--   1) inference_requests.expected_hold_micros：准入定价的单次消费金额上界
--      （可恢复估算上界）。口径与 reserved_micros 一致：charge_source='plan'
--      时是 microcredit，charge_source='wallet' 时是钱包币种微金额。
--
--      为什么必须有它：有金额窗口的请求，金额由各 hold 行承载，reserved_micros
--      已经等于这个上界；但**无金额窗口**的策略（只限 RPM/TPM/并发的赠送）
--      根本没有任何 hold 行可承载它 —— 请求行是它唯一的落点。没有这条持久
--      事实，崩溃恢复只能读到 reserved_micros=0，把一个已经消费过的请求
--      当成免费请求结掉（静默免费 = 账务事故）。
--
--      NULL = 迁移之前的老行（证据不足）：恢复对这类行只保留待核对，既不按 0
--      记零放行，也不猜测补扣、更不自动重算历史（追加迁移必须区分新旧请求）。
--
--   2) inference_reconciliation_jobs.reason 扩容：新增四类**专门**核对原因 ——
--      缺预占 / released 后晚到用量 / 结算不一致 / 估算证据不足。已终态请求
--      （released）不能再改写终态，但账务异常必须进入核对队列并可被审计。

ALTER TABLE inference_requests
    ADD COLUMN IF NOT EXISTS expected_hold_micros BIGINT;

DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_expected_hold_check
        CHECK (expected_hold_micros IS NULL OR expected_hold_micros >= 0);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

ALTER TABLE inference_reconciliation_jobs
    DROP CONSTRAINT IF EXISTS inference_reconciliation_jobs_reason_check;
ALTER TABLE inference_reconciliation_jobs
    ADD CONSTRAINT inference_reconciliation_jobs_reason_check
    CHECK (reason IN ('unknown_usage', 'crash_recovery', 'ledger_mismatch',
                      'manual', 'settlement_overage',
                      'missing_reservation', 'late_usage_after_release',
                      'settlement_mismatch', 'insufficient_evidence'));
