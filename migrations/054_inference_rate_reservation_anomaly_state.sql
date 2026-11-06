-- 054_inference_rate_reservation_anomaly_state.sql
-- Description: 终审 Finding 1 —— 人工停放的账务异常的**速率预占处置**
--   （只追加，不改写任何历史迁移，也不回写历史账本行）。
--
--   问题：一张请求被人工停放（missing_reservation / reservation_drift /
--   insufficient_evidence）之后，它的 inference_rate_reservations 行永远停在
--   state='reserved'。而 TPM 的在途求和把**所有** state='reserved' 的行都算进
--   账户额度（与年龄无关），保留期清理又**永不删除**这类行 —— 一笔异常就会
--   永久吃掉账户的 TPM 额度：账户被永久 429（rate_limited）拦住，而且没有
--   任何自动或人工路径能把那笔额度取回来。
--
--   处置（新增状态 'anomaly'）：
--     - 速率预占行转入 state='anomaly'：**离开在途 TPM 求和**（不再占额度），
--       因此保留期清理可以像其它终态行一样删除它（滞留行不再无限增长）；
--     - 它**不是** 'released'（那等于宣称"确认零消费"），也**不是**
--       'settled'（那等于宣称"已知实际用量"）：消费仍然未知 —— 未知绝不
--       记成 0；
--     - RPM 事实保留：行还在、counted_at 不变，一次逻辑请求仍然计 1 次
--       （RPM 计数不看 state）；
--     - 异常证据留在行上（anomaly_reason / anomaly_at），核对任务
--       (inference_reconciliation_jobs) 里的原因与 request.last_error 保持
--       不变 —— 处置的是**额度**，不是**证据**；
--     - 金额侧完全不动：窗口/Key 的金额预占仍在，账本没有 charge，钱既没结
--       也没放。
--
--   迁移 052 的 state CHECK 必须扩词（新状态由 DB CHECK 强制），因此按
--   migrations/README.md 的幂等 DDL 口径 drop + re-add。

ALTER TABLE inference_rate_reservations
    ADD COLUMN IF NOT EXISTS anomaly_at TIMESTAMPTZ;
ALTER TABLE inference_rate_reservations
    ADD COLUMN IF NOT EXISTS anomaly_reason TEXT;

-- 处置痕迹只在 anomaly 行上产生；行后来被真的结算/释放（转成 settled /
-- released）时痕迹**保留**（审计不被抹掉），因此这个 CHECK 是单向的：anomaly
-- 行必须带原因，其它状态允许带（历史痕迹）。
ALTER TABLE inference_rate_reservations
    DROP CONSTRAINT IF EXISTS inference_rate_reservations_anomaly_trace_check;
ALTER TABLE inference_rate_reservations
    ADD CONSTRAINT inference_rate_reservations_anomaly_trace_check
    CHECK (state <> 'anomaly' OR anomaly_reason IS NOT NULL);

ALTER TABLE inference_rate_reservations
    DROP CONSTRAINT IF EXISTS inference_rate_reservations_state_check;
ALTER TABLE inference_rate_reservations
    ADD CONSTRAINT inference_rate_reservations_state_check
    CHECK (state IN ('reserved','settled','released','anomaly'));

-- 索引无需改动：idx_inference_rate_reservations_open 的谓词是 state='reserved'
-- （anomaly 行自动不在其中 —— 处置后不再占额度），而
-- idx_inference_rate_reservations_terminal 的谓词是 state <> 'reserved'
-- （anomaly 行自动在其中 —— 可被保留期清理，与其它终态行同一口径）。
