-- 031_inference_settlement_recovery.sql
-- Description: Task 9（幂等结算、未知用量与崩溃恢复）的支撑 DDL。
--   控制者裁决：无新迁移为原则，确需 DDL 用 031 起空号并说明 —— 本文件三处
--   变更均为 026 表组无法表达、且验收硬项要求的能力：
--
--   1) 零消费结算落 charge 行（Task 1 遗留决策点，Task 9 裁定）：amount=0 仅
--      对 entry_type='charge' 放行，reversal/adjustment 仍必须 > 0。理由见
--      任务报告：统一由 idx_inference_ledger_charge_once 唯一键保护全部结
--      算投递（含零额），"已结算(0)"与"未结算"在账本层可直接区分，窗口核对
--      求和不受零行影响。
--   2) 恢复估算依据持久化：准入时的输入安全上界/强制输出上限/额外计费项目
--      上界随请求落库。进程崩溃后恢复 worker 以"保守估算 = 预占上界"结算
--      （设计 §7.2 补充段：估算为主、可审计、可被后续真实证据冲正），禁止
--      TTL 到期视为零消费。
--   3) reconciliation_jobs：reason 增加 'settlement_overage'（实际费用超出
--      预占的异常进核对队列，不隐藏负差额）；request_id 允许 NULL，使窗口级
--      账本差异（ledger_mismatch 不归属单一请求）可以入队；按请求的 UNIQUE
--      语义由部分唯一索引保持。

-- 1) 账本金额 CHECK：charge 允许 0，其余类型仍必须为正。
ALTER TABLE inference_ledger_entries
    DROP CONSTRAINT IF EXISTS inference_ledger_entries_amount_micros_check;
ALTER TABLE inference_ledger_entries
    ADD CONSTRAINT inference_ledger_entries_amount_micros_check
    CHECK ((entry_type = 'charge' AND amount_micros >= 0)
        OR (entry_type IN ('reversal', 'adjustment') AND amount_micros > 0));

-- 2) 请求行持久化准入上界（恢复估算的审计依据）。
ALTER TABLE inference_requests
    ADD COLUMN IF NOT EXISTS input_bound_tokens BIGINT,
    ADD COLUMN IF NOT EXISTS output_cap_tokens BIGINT,
    ADD COLUMN IF NOT EXISTS extra_bounds JSONB NOT NULL DEFAULT '{"schema_version":1,"bounds":{}}';

DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_input_bound_check
        CHECK (input_bound_tokens IS NULL OR input_bound_tokens >= 0);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE inference_requests
        ADD CONSTRAINT inference_requests_output_cap_check
        CHECK (output_cap_tokens IS NULL OR output_cap_tokens > 0);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- 3) 核对任务：新 reason + 窗口级任务允许无请求归属。
ALTER TABLE inference_reconciliation_jobs
    DROP CONSTRAINT IF EXISTS inference_reconciliation_jobs_reason_check;
ALTER TABLE inference_reconciliation_jobs
    ADD CONSTRAINT inference_reconciliation_jobs_reason_check
    CHECK (reason IN ('unknown_usage', 'crash_recovery', 'ledger_mismatch',
                      'manual', 'settlement_overage'));

ALTER TABLE inference_reconciliation_jobs
    ALTER COLUMN request_id DROP NOT NULL;

-- 原 UNIQUE(request_id) 约束在请求级任务上继续生效（NULL 不冲突）；
-- 窗口级任务按 detail->>'window_id' 去重，避免每次核对扫描重复入队。
CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_reconciliation_window_job
    ON inference_reconciliation_jobs ((detail->>'window_id'))
    WHERE request_id IS NULL AND reason = 'ledger_mismatch'
      AND status IN ('pending', 'running');
