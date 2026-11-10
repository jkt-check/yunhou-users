-- 051_inference_reconciliation_reasons.sql
-- Description: T2 修复轮（终态投递分类 + 核对原因持久化）的 DDL —— 只追加，
--   不改写任何历史迁移，也不回写历史账本行。
--
--   背景：T2 把"已终态的结算投递"分成了可识别的几类，其中两类是**真正的
--   账务漂移**，必须与"金额不一致的重复结算"分开持久化，恢复 worker 才能
--   区分"良性竞态"与"必须人工处理"：
--     - ledger_drift：请求已 settled，但账本上没有 charge 行；
--     - reservation_drift：held 预占数与准入创建的数目不符。
--
--   两者此前共用一个 mismatch 原因，恢复 worker 因此把真正的账务漂移当成
--   良性竞态吞掉（无告警、无核对任务）。核对任务的原因由 DB CHECK 约束，
--   新增两类必须追加迁移。

ALTER TABLE inference_reconciliation_jobs
    DROP CONSTRAINT IF EXISTS inference_reconciliation_jobs_reason_check;
ALTER TABLE inference_reconciliation_jobs
    ADD CONSTRAINT inference_reconciliation_jobs_reason_check
    CHECK (reason IN ('unknown_usage', 'crash_recovery', 'ledger_mismatch',
                      'manual', 'settlement_overage',
                      'missing_reservation', 'late_usage_after_release',
                      'settlement_mismatch', 'insufficient_evidence',
                      'ledger_drift', 'reservation_drift'));
