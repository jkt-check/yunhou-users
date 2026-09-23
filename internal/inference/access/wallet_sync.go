// wallet_sync.go — 支付事件 → 钱包余额的消息契约（Task 14，设计 §3 模块
// 边界 + §7.3 inference_outbox = 支付发放的持久任务）。
//
// 本文件不碰数据库：它定义支付域（service/payment.go）写入
// inference_outbox 的消息形状与 dedup 键；消费侧是
// workers/wallet_sync.go。与 benefit_grant.go 同一模式：状态翻转与消息
// 同事务提交，消费侧按幂等业务键收敛（wallet:topup:{payment_id} /
// wallet:refund:{refund_id}），重复/乱序回调只生效一次。
//
// 与权益同步不同的是金额口径：金额在入队时按订单快照（支付事务内）严格
// 换算成微金额写入载荷——充值金额不作为订阅有效期（裁决：单独定义余额
// 充值商品）。

package access

// TopicWalletSync is the inference_outbox topic the payment domain enqueues
// wallet movements on and the wallet-sync worker consumes.
const TopicWalletSync = "wallet.sync"

// Wallet sync kinds.
const (
	// WalletSyncTopup credits cash from a settled payment.
	WalletSyncTopup = "topup"
	// WalletSyncRefund debits cash for a channel-confirmed refund
	// (原路退，只允许现金来源).
	WalletSyncRefund = "refund"
)

// WalletSyncMessage is the outbox payload. AmountMicros/Currency are
// computed from the order row INSIDE the payment transaction (订单快照口
// 径：回调不按可能被改写的当前价目换算).
type WalletSyncMessage struct {
	Kind         string `json:"kind"` // topup | refund
	UserID       string `json:"user_id"`
	OrderID      string `json:"order_id"`
	PaymentID    string `json:"payment_id"`
	RefundID     string `json:"refund_id,omitempty"`
	AmountMicros int64  `json:"amount_micros"`
	Currency     string `json:"currency"`
}

// WalletTopupDedupKey dedups the outbox enqueue of one settled top-up
// payment across the trigger paths (channel webhook / caller Confirm /
// active reconcile) — all know the same payments.id.
func WalletTopupDedupKey(paymentID string) string { return "wallet:paid:" + paymentID }

// WalletRefundDedupKey dedups the refund enqueue per refund row (渠道重投
// 与重复退款事件只入队一次).
func WalletRefundDedupKey(refundID string) string { return "wallet:refund:" + refundID }
