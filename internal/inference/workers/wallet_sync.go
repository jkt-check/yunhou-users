// wallet_sync.go — 钱包同步 worker（Task 14）：消费 inference_outbox 的
// wallet.sync 消息，把已结算支付的充值（credit cash）与渠道确认的退款
// （debit cash）应用到模型钱包。
//
// 幂等（裁决 3/7）：入队由 dedup 键（wallet:paid:{payment_id} /
// wallet:refund:{refund_id}）兜底一次；应用层业务键
// （wallet:topup:{payment_id} / wallet:refund:{refund_id}）再兜底一次 —
// 重复回调、Confirm/webhook/补单竞争、worker 重投都只生效一次。失败消息
// 永不丢弃：有界指数退避重排，持续失败在 pending 索引中可见（慢车道）。
//
// 退款只允许现金来源原路退；赠送余额不参与（accounting.ValidateWalletEntry
// + migration 030 CHECK 双兜底）。退款落在已消费资金上时现金余额如实转负
// （不隐藏负差额），后续冻结按派生余额拒绝。

package workers

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// WalletSyncStore is the persistence surface the worker needs; satisfied
// by *postgres.Store.
type WalletSyncStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error)
	FetchPendingOutboxByTopic(ctx context.Context, topic string, limit int) ([]postgres.OutboxMessage, error)
	MarkOutboxFailed(ctx context.Context, id int64, nextRetry time.Time) error
	MarkOutboxDeliveredTx(ctx context.Context, w domain.UnitOfWork, id int64) error
	CreditTopupTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.WalletTopupCommand) error
	RefundWalletTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.WalletRefundCommand) error
}

// WalletSync consumes wallet.sync outbox messages.
type WalletSync struct {
	store WalletSyncStore
	clock domain.Clock
	cfg   EntitlementSyncConfig // same tuning knobs (interval/batch/backoff)
}

// NewWalletSync builds the worker; a nil clock uses the system clock (UTC).
func NewWalletSync(store WalletSyncStore, clock domain.Clock, cfg EntitlementSyncConfig) *WalletSync {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &WalletSync{store: store, clock: clock, cfg: cfg.withDefaults()}
}

// Start runs the periodic loop until ctx is canceled. The first pass runs
// immediately at startup (启动即跑一轮).
func (w *WalletSync) Start(ctx context.Context) {
	log.Printf("inference wallet sync worker started (interval=%s batch=%d)", w.cfg.Interval, w.cfg.BatchLimit)
	if _, err := w.RunPass(ctx); err != nil {
		log.Printf("ERROR inference wallet sync: initial pass failed: %v", err)
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("inference wallet sync worker stopped")
			return
		case <-ticker.C:
			if _, err := w.RunPass(ctx); err != nil {
				log.Printf("ERROR inference wallet sync: pass failed: %v", err)
			}
		}
	}
}

// WalletSyncStats counts one pass's outcomes.
type WalletSyncStats struct {
	Fetched   int
	Credited  int // topup entries written
	Refunded  int // refund entries written
	Replays   int // business key already present (duplicate delivery)
	Failed    int
	Delivered int
}

// RunPass consumes one batch of pending wallet.sync messages. Exported for
// tests and the startup pass.
func (w *WalletSync) RunPass(ctx context.Context) (WalletSyncStats, error) {
	stats := WalletSyncStats{}
	msgs, err := w.store.FetchPendingOutboxByTopic(ctx, access.TopicWalletSync, w.cfg.BatchLimit)
	if err != nil {
		return stats, err
	}
	stats.Fetched = len(msgs)
	for _, msg := range msgs {
		w.processMessage(ctx, msg, &stats)
	}
	return stats, nil
}

// processMessage applies one message in ONE transaction (movement +
// delivery mark commit or roll back together). The recover is the
// crash-loop guard (同 entitlement_sync：一条毒消息不得拖垮进程).
func (w *WalletSync) processMessage(ctx context.Context, msg postgres.OutboxMessage, stats *WalletSyncStats) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ALARM inference wallet sync: outbox %d panicked: %v — rescheduled into backoff", msg.ID, r)
			w.reschedule(ctx, msg)
			stats.Failed++
		}
	}()
	var wm access.WalletSyncMessage
	if err := json.Unmarshal(msg.Payload, &wm); err != nil {
		log.Printf("ERROR inference wallet sync: outbox %d undecodable payload: %v", msg.ID, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	if wm.AmountMicros <= 0 || wm.Currency == "" || wm.UserID == "" || wm.PaymentID == "" {
		log.Printf("ERROR inference wallet sync: outbox %d malformed payload (%+v)", msg.ID, wm)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}

	// 账户幂等建立（ON CONFLICT 读回赢家），随后单事务应用。
	acct, err := w.store.EnsureBillingAccount(ctx, wm.UserID)
	if err != nil {
		log.Printf("ERROR inference wallet sync: outbox %d account (%s): %v", msg.ID, wm.UserID, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}

	uow, err := w.store.Begin(ctx)
	if err != nil {
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	var applied bool
	switch wm.Kind {
	case access.WalletSyncTopup:
		err = w.store.CreditTopupTx(ctx, uow, postgres.WalletTopupCommand{
			AccountID: acct.ID, Currency: wm.Currency,
			AmountMicros: wm.AmountMicros, PaymentID: wm.PaymentID, OrderID: wm.OrderID,
		})
		applied = err == nil
	case access.WalletSyncRefund:
		if wm.RefundID == "" {
			err = domain.NewError(domain.CodeInvalidInput, "wallet sync: refund message without refund_id")
			break
		}
		err = w.store.RefundWalletTx(ctx, uow, postgres.WalletRefundCommand{
			AccountID: acct.ID, Currency: wm.Currency,
			AmountMicros: wm.AmountMicros, RefundID: wm.RefundID, PaymentID: wm.PaymentID,
		})
		applied = err == nil
	default:
		err = domain.NewError(domain.CodeInvalidInput, "wallet sync: unknown kind "+wm.Kind)
	}
	if err != nil {
		_ = uow.Rollback(ctx)
		if domain.CodeOf(err) == domain.CodeConflict {
			// 幂等业务键已生效（重复回调/重投）：移动早已入账，单独事务
			// 标记交付即可——只生效一次，不叠加。
			w.markDeliveredAfterReplay(ctx, msg)
			stats.Replays++
			stats.Delivered++
			return
		}
		log.Printf("ERROR inference wallet sync: outbox %d apply (%s payment=%s refund=%s): %v",
			msg.ID, wm.Kind, wm.PaymentID, wm.RefundID, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	if applied {
		switch wm.Kind {
		case access.WalletSyncTopup:
			stats.Credited++
		case access.WalletSyncRefund:
			stats.Refunded++
		}
	}
	if err := w.store.MarkOutboxDeliveredTx(ctx, uow, msg.ID); err != nil {
		_ = uow.Rollback(ctx)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	if err := uow.Commit(ctx); err != nil {
		log.Printf("ERROR inference wallet sync: outbox %d commit: %v", msg.ID, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	stats.Delivered++
}

// markDeliveredAfterReplay marks a message whose movement was already
// applied (business-key conflict) delivered in its own transaction.
func (w *WalletSync) markDeliveredAfterReplay(ctx context.Context, msg postgres.OutboxMessage) {
	uow, err := w.store.Begin(ctx)
	if err != nil {
		w.reschedule(ctx, msg)
		return
	}
	if err := w.store.MarkOutboxDeliveredTx(ctx, uow, msg.ID); err != nil {
		_ = uow.Rollback(ctx)
		w.reschedule(ctx, msg)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		log.Printf("ERROR inference wallet sync: outbox %d replay-mark commit: %v", msg.ID, err)
		w.reschedule(ctx, msg)
	}
}

// reschedule applies bounded exponential backoff (与 entitlement_sync 同
// 口径：5s × 2^attempts，封顶 MaxBackoff；持续失败在 pending 索引可见).
func (w *WalletSync) reschedule(ctx context.Context, msg postgres.OutboxMessage) {
	backoff := 5 * time.Second << min(msg.Attempts, 10)
	if backoff > w.cfg.MaxBackoff || backoff < 0 {
		backoff = w.cfg.MaxBackoff
	}
	if err := w.store.MarkOutboxFailed(ctx, msg.ID, w.clock.Now().Add(backoff)); err != nil {
		log.Printf("ERROR inference wallet sync: reschedule outbox %d: %v", msg.ID, err)
	}
}
