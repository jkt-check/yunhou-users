-- 041_zero_price_reservations.sql
-- Description: R7-N5 —— 0 价模型（sale_credit 价格版本全费率 = 0）的预占
--   放行零余额订阅用户：预占额 hold = 0 合法，预占行 amount_micros 允许 0。
--   对齐 031 对账本 charge 行的同款裁定（零额行由唯一键/状态机统一保护，
--   "已预占(0)"与"无预占"在数据面可区分，窗口求和不受零行影响）。
--   免费不等于无约束：权益门控、并发租约与强制输出上限（max_tokens）
--   照常生效；/chat 路径当前不执行按账户/Key 的 RPM/TPM 限额（该控制
--   挂在 /v1 的 API Key 鉴权中间件上；/chat 仍有按 IP 的入口限流）。
--   非 0 价路径行为不变。

-- 预占金额 CHECK：允许 0（0 价预占），负值仍拒绝。
ALTER TABLE inference_reservations
    DROP CONSTRAINT IF EXISTS inference_reservations_amount_micros_check;
ALTER TABLE inference_reservations
    ADD CONSTRAINT inference_reservations_amount_micros_check
    CHECK (amount_micros >= 0);
