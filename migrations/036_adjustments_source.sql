-- 036_adjustments_source.sql
-- Description: inference_adjustments 增加 source 列（cash|bonus）并回填（评审轮2 C-I1）
--
-- 背景:
--   - 运营钱包调整（POST /admin/wallet/adjustments）的 source 决定资金路由
--     （bonus 赠送不得走现金退款路径），但 026 的 inference_adjustments
--     表没有 source 列：幂等键重放只能比对 account/amount/direction/
--     currency，同键异 source 的重发会被当成良性重放（applied=false），
--     调用方误以为另一种来源的调整已生效。
--
-- 回填口径:
--   - 钱包调整行（unit='micromoney'）由 ApplyWalletAdjustmentTx 与配对的
--     inference_wallet_entries 分录（entry_type='adjustment'，经
--     adjustment_id 关联）同事务写入，分录上的 source 就是该调整的真实
--     来源——按此联表回填，与既有数据真实来源一致（非默认猜测）。
--   - 冲正分录不带 adjustment_id，每条调整至多一条 adjustment 分录，
--     联表回填无歧义。
--   - 额度调整行（unit='microcredit'，AppendAdjustment 骨架路径）无
--     cash/bonus 概念、也无配对钱包分录，source 保持 NULL（CHECK 允许）。
--
-- 全部 IF NOT EXISTS / duplicate_object 兜底，重跑为 no-op。

ALTER TABLE inference_adjustments
    ADD COLUMN IF NOT EXISTS source TEXT;

UPDATE inference_adjustments a
   SET source = e.source
  FROM inference_wallet_entries e
 WHERE e.adjustment_id = a.id
   AND e.entry_type = 'adjustment'
   AND a.source IS NULL;

DO $$
BEGIN
    ALTER TABLE inference_adjustments
        ADD CONSTRAINT inference_adjustments_source_check
        CHECK (source IS NULL OR source IN ('cash', 'bonus'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
