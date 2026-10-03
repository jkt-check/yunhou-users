-- 043_zero_price_wallet_holds.sql
-- Description: 0 价模型（sale_money 价格版本全费率 = 0 是合法配置，
--   price_versions 只拒负费率）的钱包冻结放行：hold = 0 合法，
--   inference_wallet_holds.amount_micros 允许 0。对齐 041 对
--   inference_reservations 的同款裁定（零额行由 request_id 唯一键与
--   held→settled/released 状态机统一保护，"已冻结(0)"与"无冻结"在数据
--   面可区分；现金/赠送拆分 0+0=0 仍满足行之和 CHECK；结算侧零额分录
--   不落行本就成立）。负值仍拒绝。非 0 价路径行为不变。

-- 冻结金额 CHECK：允许 0（0 价冻结），负值仍拒绝。
ALTER TABLE inference_wallet_holds
    DROP CONSTRAINT IF EXISTS inference_wallet_holds_amount_micros_check;
ALTER TABLE inference_wallet_holds
    ADD CONSTRAINT inference_wallet_holds_amount_micros_check
    CHECK (amount_micros >= 0);
