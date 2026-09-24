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

// entitlement_sync.go — 支付 → 权益的 outbox 消费 worker（Task 10，设计
// §7.2/§7.3）。
//
// 支付域（service/payment.go）在支付状态翻转的同一事务里写一条
// inference_outbox 消息（同事务 outbox，dedup_key 幂等）；本 worker 消费时
// 重新读取订阅当前状态与"为该订阅当前套餐最近一次已支付订单"的权益快照，
// 经 access.DecideSync/Converge 把权益收敛到目标状态，并把"权益变更 +
// delivered 标记"放进同一个事务提交。收敛语义保证：
//   - 重复/乱序 webhook、Confirm 与后台补单竞争：重复消息重复计算同一目标
//     状态 → noop；旧消息晚到读到的仍是当前状态 → 不会按旧事件发放；
//   - 崩溃于事务提交前：整体回滚，消息留在 pending 重试；
//   - 多实例竞争同一消息：乐观 revision 守卫 / 来源唯一键让输家回滚重试，
//     重读已收敛状态后 noop。
//
// 退款/取消：支付域翻转订阅状态的同一事务里入队，worker 把权益状态翻转为
// revoked（后续调用失去授权来源）；已消费账本与配额窗口一行不动
// （已消费账本不删除，设计 §7.3 追加+冲正口径）。

// EntitlementSyncStore is the persistence surface the worker needs;
// satisfied by *postgres.Store.
type EntitlementSyncStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error)
	FetchPendingOutboxByTopic(ctx context.Context, topic string, limit int) ([]postgres.OutboxMessage, error)
	MarkOutboxFailed(ctx context.Context, id int64, nextRetry time.Time) error
	MarkOutboxDeliveredTx(ctx context.Context, w domain.UnitOfWork, id int64) error
	GetSyncSubscription(ctx context.Context, userID, productCode string) (*access.SubscriptionState, error)
	GetLatestPaidBenefitOrder(ctx context.Context, userID, planID string) (*access.OrderBenefitSnapshot, error)
	GetLatestEntitlementBySource(ctx context.Context, sourceType domain.EntitlementSource, sourceID string) (*domain.Entitlement, error)
	InsertEntitlementTx(ctx context.Context, w domain.UnitOfWork, e *domain.Entitlement) error
	ReviseEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error)
	ReviveEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error)
	RetireEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, expectedRevision int, to domain.EntitlementStatus) error
}

// EntitlementSyncConfig tunes the worker; zero values take the defaults.
type EntitlementSyncConfig struct {
	// Interval between passes (default 2s — purchase UX wants the grant
	// visible within seconds of the payment committing).
	Interval time.Duration
	// BatchLimit caps messages per pass (default 100).
	BatchLimit int
	// MaxBackoff caps the failure retry backoff (default 10min). A
	// permanently failing message stays 'pending' and keeps retrying in
	// the slow lane — visible via idx_inference_outbox_pending — rather
	// than being silently dead-lettered.
	MaxBackoff time.Duration
}

func (c *EntitlementSyncConfig) withDefaults() EntitlementSyncConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = 2 * time.Second
	}
	if out.BatchLimit <= 0 {
		out.BatchLimit = 100
	}
	if out.MaxBackoff <= 0 {
		out.MaxBackoff = 10 * time.Minute
	}
	return out
}

// EntitlementSync is the outbox-consuming grant worker.
type EntitlementSync struct {
	store EntitlementSyncStore
	clock domain.Clock
	cfg   EntitlementSyncConfig
}

// NewEntitlementSync builds the worker; a nil clock uses the system clock
// (UTC).
func NewEntitlementSync(store EntitlementSyncStore, clock domain.Clock, cfg EntitlementSyncConfig) *EntitlementSync {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &EntitlementSync{store: store, clock: clock, cfg: cfg.withDefaults()}
}

// Start runs one pass immediately (a restart is exactly when a backlog
// drains) and then ticks until ctx is done. A failing pass is logged and
// retried next tick; runGuarded 兜底每轮 pass 的 panic（与 processMessage 的
// per-message recover 互补：Fetch 等 pass 级 panic 也不再终止进程）。
func (w *EntitlementSync) Start(ctx context.Context) {
	log.Printf("inference entitlement sync worker started (interval=%s batch=%d)", w.cfg.Interval, w.cfg.BatchLimit)
	pass := func() error {
		_, err := w.RunPass(ctx)
		return err
	}
	if err := runGuarded("inference entitlement sync", pass); err != nil {
		log.Printf("ERROR inference entitlement sync: initial pass failed: %v", err)
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("inference entitlement sync worker stopped")
			return
		case <-ticker.C:
			if err := runGuarded("inference entitlement sync", pass); err != nil {
				log.Printf("ERROR inference entitlement sync: pass failed: %v", err)
			}
		}
	}
}

// EntitlementSyncStats counts one pass's outcomes — the audit trail for
// every grant/revise/retire the worker applied.
type EntitlementSyncStats struct {
	Fetched   int
	Granted   int // insert: first grant for a source
	Revised   int // in-place spec/expiry patch (upgrade/renewal converge)
	Revived   int // retired → active (re-purchase after refund/cancel)
	Retired   int // active → revoked/expired (refund/cancel/lapse)
	Noops     int // already converged (idempotent replays)
	Racing    int // lost an optimistic race; rolled back, will re-read
	Failed    int
	Delivered int
}

// RunPass consumes one batch of pending entitlement.sync messages.
// Exported for tests and the startup pass.
func (w *EntitlementSync) RunPass(ctx context.Context) (EntitlementSyncStats, error) {
	stats := EntitlementSyncStats{}
	msgs, err := w.store.FetchPendingOutboxByTopic(ctx, access.TopicEntitlementSync, w.cfg.BatchLimit)
	if err != nil {
		return stats, err
	}
	stats.Fetched = len(msgs)
	for _, msg := range msgs {
		w.processMessage(ctx, msg, &stats)
	}
	return stats, nil
}

// processMessage applies one message: plan (pure reads + converge) → apply
// (one tx with the delivery mark). Failures reschedule with bounded
// exponential backoff; the message is never dropped.
//
// The recover is the crash-loop guard (审查修复): a panicking message must
// never take the server process down — with it, the message goes back into
// bounded backoff (observable via the pending index + ALARM log); without
// it a single poisoned message would crash every worker restart forever.
func (w *EntitlementSync) processMessage(ctx context.Context, msg postgres.OutboxMessage, stats *EntitlementSyncStats) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ALARM inference entitlement sync: outbox %d panicked: %v — rescheduled into backoff", msg.ID, r)
			w.reschedule(ctx, msg)
			stats.Failed++
		}
	}()
	var syncMsg access.EntitlementSyncMessage
	if err := json.Unmarshal(msg.Payload, &syncMsg); err != nil {
		log.Printf("ERROR inference entitlement sync: outbox %d undecodable payload: %v", msg.ID, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	plans, err := w.planSync(ctx, syncMsg.UserID, syncMsg.ProductCode, syncMsg.SubscriptionID)
	if err != nil {
		log.Printf("ERROR inference entitlement sync: outbox %d plan (%s/%s reason=%s): %v",
			msg.ID, syncMsg.UserID, syncMsg.ProductCode, syncMsg.Reason, err)
		w.reschedule(ctx, msg)
		stats.Failed++
		return
	}
	racing, err := w.applyPlans(ctx, syncMsg.UserID, plans, &msg.ID)
	switch {
	case err != nil:
		log.Printf("ERROR inference entitlement sync: outbox %d apply (%s/%s reason=%s): %v",
			msg.ID, syncMsg.UserID, syncMsg.ProductCode, syncMsg.Reason, err)
		w.reschedule(ctx, msg)
		stats.Failed++
	case racing:
		// A concurrent converger moved the state forward; the next pass
		// re-reads the converged state and noops. Not an error — 良性竞争
		// 立即重排，不吃失败指数退避（审查修复 Minor-5：backoff 会不必要
		// 地延迟 delivered 标记）。
		w.rescheduleSoon(ctx, msg)
		stats.Racing++
	default:
		stats.Delivered++
		for _, p := range plans {
			switch p.action.Op {
			case access.ConvergeInsert:
				stats.Granted++
			case access.ConvergeRevise:
				stats.Revised++
			case access.ConvergeRevive:
				stats.Revived++
			case access.ConvergeRetire:
				stats.Retired++
			default:
				stats.Noops++
			}
		}
		if len(plans) == 0 {
			stats.Noops++
		}
	}
}

// reschedule applies bounded exponential backoff: 5s × 2^attempts, capped
// at MaxBackoff.
func (w *EntitlementSync) reschedule(ctx context.Context, msg postgres.OutboxMessage) {
	backoff := 5 * time.Second << min(msg.Attempts, 10)
	if backoff > w.cfg.MaxBackoff || backoff < 0 {
		backoff = w.cfg.MaxBackoff
	}
	if err := w.store.MarkOutboxFailed(ctx, msg.ID, w.clock.Now().Add(backoff)); err != nil {
		log.Printf("ERROR inference entitlement sync: reschedule outbox %d: %v", msg.ID, err)
	}
}

// rescheduleSoon 立即重排（零延迟）：仅用于良性乐观竞争——状态已被并发收敛
// 者推前，下一轮重读即收敛 noop，无需失败退避。
func (w *EntitlementSync) rescheduleSoon(ctx context.Context, msg postgres.OutboxMessage) {
	if err := w.store.MarkOutboxFailed(ctx, msg.ID, w.clock.Now()); err != nil {
		log.Printf("ERROR inference entitlement sync: reschedule outbox %d: %v", msg.ID, err)
	}
}

// sourcePlan pairs one entitlement source with its converge action.
type sourcePlan struct {
	sourceType domain.EntitlementSource
	sourceID   string
	// entitlementID is the row UUID the revise/revive/retire guards target
	// (empty for insert — the row doesn't exist yet).
	entitlementID string
	action        access.ConvergeAction
}

// planSync computes the converge actions for one (user, product) pair from
// CURRENT state — the message payload is never trusted for state. Exported
// so tests can drive the planning directly.
func (w *EntitlementSync) planSync(ctx context.Context, userID, productCode, subscriptionIDHint string) ([]sourcePlan, error) {
	if userID == "" || productCode == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement sync: user and product required")
	}

	sub, err := w.store.GetSyncSubscription(ctx, userID, productCode)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeNotFound {
			return nil, err
		}
		sub = nil
	}

	var paidOrder *access.OrderBenefitSnapshot
	if sub != nil && sub.Status == "active" {
		po, perr := w.store.GetLatestPaidBenefitOrder(ctx, userID, sub.PlanID)
		if perr != nil {
			if domain.CodeOf(perr) != domain.CodeNotFound {
				return nil, perr
			}
		} else {
			paidOrder = po
		}
	}

	decision, err := access.DecideSync(sub, paidOrder, w.clock.Now())
	if err != nil {
		return nil, err
	}

	subID := subscriptionIDHint
	if sub != nil {
		subID = sub.ID
	}
	if subID == "" {
		// No subscription row and no hint. Entitlements are keyed on the
		// subscription id, so none can exist for this pair — nothing to do.
		return nil, nil
	}

	// Candidate sources this subscription could have granted under: the
	// explicit subscription source and the bundle-gift source. Converging
	// BOTH (the non-target one against a nil target) keeps mode transitions
	// and refund/cancel paths correct regardless of which mode originally
	// granted.
	candidates := []struct {
		typ domain.EntitlementSource
		id  string
	}{
		{domain.SourceSubscription, subID},
		{domain.SourceGrant, access.BundleGiftSourceID(subID)},
	}

	var plans []sourcePlan
	for _, src := range candidates {
		current, cerr := w.store.GetLatestEntitlementBySource(ctx, src.typ, src.id)
		if cerr != nil {
			if domain.CodeOf(cerr) != domain.CodeNotFound {
				return nil, cerr
			}
			current = nil
		}
		// The decision's target applies only to its own source; the other
		// candidate converges to "no entitlement" (mode-switch safety).
		target := decision.Target
		if target != nil && (target.SourceType != src.typ || target.SourceID != src.id) {
			target = nil
		}
		action, aerr := access.Converge(current, &access.SyncDecision{
			Reason: decision.Reason, Target: target, RetireAs: decision.RetireAs,
		}, w.clock.Now())
		if aerr != nil {
			return nil, aerr
		}
		if action.Op == access.ConvergeNoop {
			continue
		}
		var entID string
		if current != nil {
			entID = current.ID
		}
		plans = append(plans, sourcePlan{src.typ, src.id, entID, action})
	}
	return plans, nil
}

// applyPlans applies the planned converge actions in ONE transaction,
// optionally marking the outbox message delivered in the same commit
// (outboxID nil for direct SyncUserProduct drives). Returns racing=true
// when an optimistic guard lost to a concurrent converger — the caller
// reschedules; nothing was written.
func (w *EntitlementSync) applyPlans(ctx context.Context, userID string, plans []sourcePlan, outboxID *int64) (racing bool, err error) {
	if len(plans) == 0 {
		if outboxID != nil {
			uow, berr := w.store.Begin(ctx)
			if berr != nil {
				return false, berr
			}
			defer uow.Rollback(ctx) //nolint:errcheck
			if err := w.store.MarkOutboxDeliveredTx(ctx, uow, *outboxID); err != nil {
				return false, err
			}
			return false, uow.Commit(ctx)
		}
		return false, nil
	}

	needsAccount := false
	for _, p := range plans {
		if p.action.Op == access.ConvergeInsert {
			needsAccount = true
		}
	}
	var accountID string
	if needsAccount {
		// 首次发放时建立计费账户（每用户一个，所有权来自服务端身份，
		// 设计 §4.2）。幂等（ON CONFLICT 读回），崩溃后重试无副作用。
		acct, aerr := w.store.EnsureBillingAccount(ctx, userID)
		if aerr != nil {
			return false, aerr
		}
		accountID = acct.ID
	}

	uow, err := w.store.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer uow.Rollback(ctx) //nolint:errcheck

	for _, p := range plans {
		a := p.action
		switch a.Op {
		case access.ConvergeInsert:
			ent, gerr := access.GrantFromPlan(a.InsertPlan, a.Anchor, a.EffectiveFrom, a.EffectiveTo)
			if gerr != nil {
				return false, gerr
			}
			ent.BillingAccountID = accountID
			if err := w.store.InsertEntitlementTx(ctx, uow, ent); err != nil {
				// UNIQUE(source_type, source_id, revision=1) collision =
				// a concurrent granter won; the retry pass re-reads + noops.
				if domain.CodeOf(err) == domain.CodeConflict {
					return true, nil
				}
				return false, err
			}
		case access.ConvergeRevise, access.ConvergeRevive:
			var rerr error
			if a.Op == access.ConvergeRevise {
				_, rerr = w.store.ReviseEntitlementTx(ctx, uow, p.entitlementID, a.Patch)
			} else {
				_, rerr = w.store.ReviveEntitlementTx(ctx, uow, p.entitlementID, a.Patch)
			}
			if rerr != nil {
				if domain.CodeOf(rerr) == domain.CodeConflict {
					return true, nil
				}
				return false, rerr
			}
		case access.ConvergeRetire:
			if err := w.store.RetireEntitlementTx(ctx, uow, p.entitlementID, a.ExpectedRevision, a.RetireAs); err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					return true, nil
				}
				return false, err
			}
		}
	}
	if outboxID != nil {
		if err := w.store.MarkOutboxDeliveredTx(ctx, uow, *outboxID); err != nil {
			return false, err
		}
	}
	return false, uow.Commit(ctx)
}

// SyncUserProduct converges one (user, product) pair directly, without an
// outbox message. Exported for tests and future admin/reconciliation
// tooling; the worker itself only consumes the outbox.
func (w *EntitlementSync) SyncUserProduct(ctx context.Context, userID, productCode, subscriptionIDHint string) (int, error) {
	plans, err := w.planSync(ctx, userID, productCode, subscriptionIDHint)
	if err != nil {
		return 0, err
	}
	if _, err := w.applyPlans(ctx, userID, plans, nil); err != nil {
		return 0, err
	}
	return len(plans), nil
}

// IssueMigrationGift grants a migration gift entitlement with its own
// independent idempotency source key (设计 §4.3: 历史 Kaya 用户切换额度体系
// 的迁移权益策略). Re-issuing the same (ruleID, userID) is a no-op — the
// UNIQUE(source_type, source_id, revision) key absorbs it. Non-stackable by
// default: suppressed while the account holds any explicit entitlement
// (SelectEntitlement, Task 6).
//
// This is the phase-1 primitive; the ops surface that invokes it (migration
// batches) belongs to later admin tasks.
func (w *EntitlementSync) IssueMigrationGift(ctx context.Context, userID, ruleID string, modelIDs []string, policyVersionID string, effectiveTo *time.Time) (bool, error) {
	if userID == "" || ruleID == "" {
		return false, domain.NewError(domain.CodeInvalidInput, "migration gift: user id and rule id required")
	}
	acct, err := w.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		return false, err
	}
	now := w.clock.Now()
	ent, err := access.MigrationGift(ruleID, userID, modelIDs, policyVersionID, now, now, effectiveTo)
	if err != nil {
		return false, err
	}
	ent.BillingAccountID = acct.ID

	uow, err := w.store.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer uow.Rollback(ctx) //nolint:errcheck
	if err := w.store.InsertEntitlementTx(ctx, uow, ent); err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			return false, nil // 独立幂等来源键：同一规则对同一用户只发一次
		}
		return false, err
	}
	if err := uow.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
