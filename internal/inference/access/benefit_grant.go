package access

import (
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// benefit_grant.go — 支付事件 → 模型权益的纯决策规则（Task 10，设计
// §4.1–§4.3、§7.2）。
//
// 本文件不碰数据库：它定义
//  1. 支付域（service/payment.go）写入 inference_outbox 的消息契约；
//  2. 消费侧（workers/entitlement_sync.go）的"状态收敛"决策：权益 = f(订阅
//     当前状态, 最近一次已支付订单的权益快照)，与到达顺序/重复投递无关；
//  3. coding-plan 订单的激活期规则（按订单快照，而非 live plan 行）；
//  4. 捆绑赠送与迁移赠送的来源幂等键。
//
// 幂等总原则：所有效果都是"向目标状态收敛"，重复/乱序 webhook、主动
// Confirm 与后台补单竞争只会重复计算同一个目标状态，不叠加效果。

// TopicEntitlementSync is the inference_outbox topic the payment domain
// enqueues and the entitlement-sync worker consumes (设计 §7.3:
// inference_outbox = 支付发放及异常恢复的持久任务).
const TopicEntitlementSync = "entitlement.sync"

// Sync reasons (diagnostic; the decision never branches on them).
const (
	SyncReasonPaymentPaid   = "payment_paid"
	SyncReasonRenewalPaid   = "renewal_paid"
	SyncReasonRefundFull    = "refund_full"
	SyncReasonPaymentFailed = "payment_failed_after_paid"
	SyncReasonSubCancelled  = "subscription_cancelled"
	SyncReasonDriftRecheck  = "drift_recheck"
)

// EntitlementSyncMessage is the outbox payload. It carries correlation
// references only — the consumer RE-READS the current subscription and the
// paying order's frozen snapshot, so a stale/duplicated/reordered message
// can never grant against stale state.
type EntitlementSyncMessage struct {
	UserID         string `json:"user_id"`
	ProductCode    string `json:"product_code"`
	Reason         string `json:"reason"`
	OrderID        string `json:"order_id,omitempty"`
	PaymentID      string `json:"payment_id,omitempty"`
	RefundID       string `json:"refund_id,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
}

// PaidSyncDedupKey dedups the outbox enqueue for one settled payment across
// the three trigger paths (channel webhook / caller Confirm / active
// reconcile) — all three know the same payments.id.
func PaidSyncDedupKey(paymentID string) string { return "benefit:paid:" + paymentID }

// RefundSyncDedupKey dedups the full-refund revoke enqueue per refund row.
func RefundSyncDedupKey(refundID string) string { return "benefit:refund:" + refundID }

// FailedSyncDedupKey dedups the rare payment-failed-after-paid cascade.
func FailedSyncDedupKey(paymentID string) string { return "benefit:failed:" + paymentID }

// CancelSyncDedupKey is deliberately NOT used: a cancel → re-purchase →
// cancel sequence must enqueue twice, so subscription-cancel messages go in
// with a NULL dedup key. Convergence idempotency absorbs the duplicates.

// BundleGiftSourceID is the entitlement source id for a bundle gift
// (设计 §4.1: 捆绑商品通过明确的 benefit/grant 映射发放模型权益). The id
// pins the SUBSCRIPTION row, so a renewal of the bundle extends the same
// gift entitlement instead of issuing a second one.
func BundleGiftSourceID(subscriptionID string) string { return "bundle:" + subscriptionID }

// MigrationGiftSourceID is the independent idempotency source key for a
// migration gift (设计 §4.3: 历史用户切换额度体系的迁移权益). One rule
// grants one user exactly one gift entitlement — re-issuance hits
// UNIQUE(source_type, source_id, revision).
func MigrationGiftSourceID(ruleID, userID string) string { return "migration:" + ruleID + ":" + userID }

// ---------------------------------------------------------------------------
// 同步决策：订阅当前状态 + 已支付订单快照 → 目标权益
// ---------------------------------------------------------------------------

// SubscriptionState is the current truth of one subscriptions row.
type SubscriptionState struct {
	ID          string
	UserID      string
	PlanID      string
	ProductCode string
	Status      string
	ExpiresAt   *time.Time
}

// OrderBenefitSnapshot is the benefit spec frozen on the paying order
// (migration 029 columns). The consumer never reads the live plan/config
// rows — 调价/撤售后已支付订单仍按快照兑现.
type OrderBenefitSnapshot struct {
	OrderID         string
	PolicyVersionID string
	ModelIDs        []string
	GrantMode       string // model.BenefitGrantMode*
}

// BenefitTarget is the entitlement the current state demands.
type BenefitTarget struct {
	SourceType      domain.EntitlementSource
	SourceID        string
	PolicyVersionID string
	ModelIDs        []string
	// EffectiveTo tracks the subscription expiry (续费延长有效期，不提前
	// 重置窗口); nil = open-ended (lifetime / no-expiry subscription).
	EffectiveTo *time.Time
	// Stackable is always false in phase 1 (设计 §4.2: 不发布叠加合并规则).
	Stackable bool
}

// SyncDecision is the outcome of evaluating the current state.
type SyncDecision struct {
	Reason string
	// Target is the entitlement that must be active; nil = no active
	// entitlement may exist for this source.
	Target *BenefitTarget
	// RetireAs is the terminal status applied to an existing active
	// entitlement when Target is nil: EntitlementExpired for a natural
	// lapse, EntitlementRevoked for cancel/refund/missing payment evidence.
	RetireAs domain.EntitlementStatus
}

// DecideSync maps (subscription, paying order snapshot, now) to the target.
// paidOrder is the LATEST paid order carrying a benefit snapshot whose plan
// matches the subscription's current plan (nil when none — the backstop
// that a self-created or evidence-less subscription grants nothing).
func DecideSync(sub *SubscriptionState, paidOrder *OrderBenefitSnapshot, now time.Time) (*SyncDecision, error) {
	if sub == nil {
		return &SyncDecision{Reason: "no_subscription", RetireAs: domain.EntitlementRevoked}, nil
	}
	if sub.Status != "active" {
		return &SyncDecision{Reason: "subscription_" + sub.Status, RetireAs: domain.EntitlementRevoked}, nil
	}
	if sub.ExpiresAt != nil && !sub.ExpiresAt.After(now.UTC()) {
		// 自然到期：授权本就被 effective_to 截断，这里把状态如实翻为
		// expired（区别于取消/退款的 revoked）。
		return &SyncDecision{Reason: "subscription_lapsed", RetireAs: domain.EntitlementExpired}, nil
	}
	if paidOrder == nil {
		// 兜底：订阅没有已支付订单作证据就不发权益（自助免费订阅接口
		// 不能变出付费 Coding Plan 额度）。
		return &SyncDecision{Reason: "no_paid_order_evidence", RetireAs: domain.EntitlementRevoked}, nil
	}
	if paidOrder.PolicyVersionID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "benefit sync: paying order "+paidOrder.OrderID+" carries no policy version")
	}

	t := &BenefitTarget{
		PolicyVersionID: paidOrder.PolicyVersionID,
		ModelIDs:        dedupeModels(paidOrder.ModelIDs),
		EffectiveTo:     sub.ExpiresAt,
		Stackable:       false,
	}
	switch paidOrder.GrantMode {
	case model.BenefitGrantModeSubscription:
		t.SourceType, t.SourceID = domain.SourceSubscription, sub.ID
	case model.BenefitGrantModeGift:
		t.SourceType, t.SourceID = domain.SourceGrant, BundleGiftSourceID(sub.ID)
	default:
		return nil, domain.NewError(domain.CodeInvalidInput,
			"benefit sync: order "+paidOrder.OrderID+" has unknown grant mode "+paidOrder.GrantMode)
	}
	return &SyncDecision{Reason: "active_" + paidOrder.GrantMode, Target: t}, nil
}

// ---------------------------------------------------------------------------
// 收敛：当前权益 → 目标权益（全部效果幂等）
// ---------------------------------------------------------------------------

// Converge op codes.
const (
	ConvergeNoop   = "noop"
	ConvergeInsert = "insert"
	ConvergeRevise = "revise" // active entitlement: spec/effective_to patch
	ConvergeRevive = "revive" // retired entitlement returning to active
	ConvergeRetire = "retire" // active → expired/revoked
)

// ConvergeAction is one idempotent step the worker applies inside its
// per-message transaction. Re-deriving it from the same (current, target)
// pair always yields the same action; a converged state yields noop.
type ConvergeAction struct {
	Op string
	// Insert (Op=insert): GrantFromPlan parameters.
	InsertPlan    PlanRevision
	Anchor        time.Time
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	// Revise/Revive (Op=revise/revive): the optimistic in-place patch.
	// Entitlement ID and anchor NEVER change (升级沿用同一消费主体与已有
	// 窗口，设计 §4.2/§6).
	Patch domain.EntitlementPatch
	// Retire (Op=retire): expected revision + terminal status.
	ExpectedRevision int
	RetireAs         domain.EntitlementStatus
}

// Converge computes the next action moving `current` toward the decision's
// target. `current` is the latest entitlement row for the decision's source
// (any status; nil when never granted).
func Converge(current *domain.Entitlement, decision *SyncDecision, now time.Time) (ConvergeAction, error) {
	if decision == nil {
		return ConvergeAction{}, domain.NewError(domain.CodeInvalidInput, "benefit converge: nil decision")
	}
	want := decision.Target

	if want == nil {
		if current == nil || current.Status != domain.EntitlementActive {
			return ConvergeAction{Op: ConvergeNoop}, nil
		}
		retireAs := decision.RetireAs
		if retireAs == "" || retireAs == domain.EntitlementActive {
			retireAs = domain.EntitlementRevoked
		}
		return ConvergeAction{
			Op:               ConvergeRetire,
			ExpectedRevision: current.Revision,
			RetireAs:         retireAs,
		}, nil
	}

	if want.PolicyVersionID == "" {
		return ConvergeAction{}, domain.NewError(domain.CodeInvalidInput, "benefit converge: target policy version required")
	}
	if current == nil {
		// 首次发放：锚点 = 生效时刻（weekly/monthly 窗口由此推进，续费/
		// 升级不再移动，设计 §6）。
		at := now.UTC()
		return ConvergeAction{
			Op: ConvergeInsert,
			InsertPlan: PlanRevision{
				SourceType:      want.SourceType,
				SourceID:        want.SourceID,
				ModelIDs:        want.ModelIDs,
				PolicyVersionID: want.PolicyVersionID,
			},
			Anchor:        at,
			EffectiveFrom: at,
			EffectiveTo:   want.EffectiveTo,
		}, nil
	}

	patch := domain.EntitlementPatch{ExpectedRevision: current.Revision}
	if want.PolicyVersionID != current.PolicyVersionID {
		v := want.PolicyVersionID
		patch.PolicyVersionID = &v
	}
	if !equalModelSets(want.ModelIDs, current.ModelIDs) {
		patch.ModelIDs = dedupeModels(want.ModelIDs)
	}
	if !sameEffectiveTo(want.EffectiveTo, current.EffectiveTo) {
		if want.EffectiveTo == nil {
			// 目标为开放型（订阅 expires_at 为 NULL）：显式置 NULL。
			// 绝不对 nil *time.Time 调 .UTC()——那是空指针 panic
			// （审查修复 Important：worker 无 recover 时单条消息即可
			// 形成崩溃循环）。
			patch.ClearEffectiveTo = true
		} else {
			if !want.EffectiveTo.After(current.EffectiveFrom) {
				return ConvergeAction{}, domain.NewError(domain.CodeInvalidInput,
					"benefit converge: effective_to must stay after effective_from")
			}
			to := want.EffectiveTo.UTC()
			patch.EffectiveTo = &to
		}
	}

	if current.Status != domain.EntitlementActive {
		// 退款/取消后重新购买（订阅行原地复活）：同一权益行复活并带上
		// 新快照规格；锚点与消费主体保持不变（既有窗口不跨期重叠，
		// EXCLUDE 约束保证，新周期自然开新窗口）。
		if patch.PolicyVersionID == nil && patch.ModelIDs == nil && patch.EffectiveTo == nil && !patch.ClearEffectiveTo {
			// 规格与有效期未变也仍须复活（状态翻转本身是一次修订）。
		}
		return ConvergeAction{Op: ConvergeRevive, Patch: patch}, nil
	}
	if patch.PolicyVersionID == nil && patch.ModelIDs == nil && patch.EffectiveTo == nil && !patch.ClearEffectiveTo {
		return ConvergeAction{Op: ConvergeNoop}, nil
	}
	return ConvergeAction{Op: ConvergeRevise, Patch: patch}, nil
}

func sameEffectiveTo(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UTC().Equal(b.UTC())
}

// ---------------------------------------------------------------------------
// coding-plan 激活期规则（按订单快照；设计 §4.2 — 商品自身档位/周期规则，
// 不复用"周期更长即可升级"的会员旧逻辑）
// ---------------------------------------------------------------------------

// CodingPlanActivation is the result of applying a PAID coding-plan order
// to the subscription's expiry.
type CodingPlanActivation struct {
	// ExpiresAt to write on the subscription (nil = open-ended). Computed
	// exclusively from the order snapshot + current subscription state.
	ExpiresAt *time.Time
	// Blocked is set when the order conflicts with a DIFFERENT active
	// plan (e.g. a stale order paid after the user changed tiers). The
	// payment is still honored (order goes paid, ops refunds manually);
	// the subscription and entitlement stay untouched — mirrors the
	// legacy downgrade_activation_blocked shape.
	Blocked string // "" or a machine-readable conflict reason
}

// ResolveCodingPlanActivation applies a paid coding-plan order:
//
//   - kind "new":     fresh activation; an unexpired sub on ANOTHER plan
//     blocks (double purchase); on the SAME plan it
//     degenerates to a renewal (concurrent double payment).
//   - kind "renewal": same-plan unexpired sub extends from its current
//     expiry (rollover); lapsed/missing sub starts fresh
//     from now; a different plan blocks.
//   - kind "upgrade": only configured at order time (plan_upgrade_rules);
//     replacing the pinned from-plan starts a FRESH period
//     from now (升级立即生效新配额，周期重新起算); landing on
//     the order's own plan extends (double upgrade order);
//     anything else blocks.
//
// hint is the channel-authoritative expiry hint (rare for coding plan;
// WeChat NATIVE ships none), clamped to [now, now+intervalDays] — even a
// verified hint cannot extend past what the order's plan grants, and a past
// hint must never shorten an entitlement the customer just paid for. A nil
// base (lifetime order / open-ended rollover) ignores the hint entirely: an
// open-ended grant has no expiry to extend, and letting any hint replace it
// would turn a past hint into a past ExpiresAt that the next DecideSync
// lapse-check revokes. intervalDays <= 0 means open-ended (lifetime),
// matching the legacy interval semantics.
func ResolveCodingPlanActivation(
	kind string,
	intervalDays int,
	upgradeFromPlanID *string,
	orderPlanID string,
	current *SubscriptionState,
	hint *time.Time,
	now time.Time,
) (CodingPlanActivation, error) {
	now = now.UTC()
	const maxIntervalDays = 365 * 290 // same overflow guard as the legacy resolver
	if intervalDays > maxIntervalDays {
		return CodingPlanActivation{}, domain.NewError(domain.CodeInvalidInput,
			"benefit activation: interval_days exceeds safety cap")
	}

	currentLive := current != nil && current.Status == "active" &&
		(current.ExpiresAt == nil || current.ExpiresAt.After(now))

	var base *time.Time
	freshFromNow := func() *time.Time {
		if intervalDays <= 0 {
			return nil
		}
		t := now.Add(time.Duration(intervalDays) * 24 * time.Hour)
		return &t
	}

	switch kind {
	case model.OrderKindNew, "":
		if currentLive {
			if current.PlanID != orderPlanID {
				return CodingPlanActivation{Blocked: "conflict_existing_other_plan"}, nil
			}
			base = rollFrom(current.ExpiresAt, intervalDays)
		} else {
			base = freshFromNow()
		}
	case model.OrderKindRenewal:
		if currentLive {
			if current.PlanID != orderPlanID {
				return CodingPlanActivation{Blocked: "conflict_existing_other_plan"}, nil
			}
			base = rollFrom(current.ExpiresAt, intervalDays)
		} else {
			base = freshFromNow()
		}
	case model.OrderKindUpgrade:
		if currentLive {
			switch {
			case upgradeFromPlanID != nil && current.PlanID == *upgradeFromPlanID:
				base = freshFromNow() // 跨档升级：新周期自支付时刻起算
			case current.PlanID == orderPlanID:
				base = rollFrom(current.ExpiresAt, intervalDays) // 重复升级单 = 续费语义
			default:
				return CodingPlanActivation{Blocked: "conflict_existing_other_plan"}, nil
			}
		} else {
			base = freshFromNow()
		}
	default:
		return CodingPlanActivation{}, domain.NewError(domain.CodeInvalidInput,
			"benefit activation: unknown order kind "+kind)
	}

	if hint != nil && base != nil {
		// 只上钳不下钳会让过去的 hint 盖过开放期 base（base == nil 已在
		// 上面排除）；这里再把 hint 下钳到 now，双重保证 hint 绝不缩短
		// 既有授权（base 恒 > now，钳制是纯防御）。
		c := hint.UTC()
		if c.Before(now) {
			c = now
		}
		if intervalDays > 0 {
			if max := now.Add(time.Duration(intervalDays) * 24 * time.Hour); c.After(max) {
				c = max
			}
		}
		if c.After(*base) {
			base = &c
		}
	}
	return CodingPlanActivation{ExpiresAt: base}, nil
}

// rollFrom extends from the current expiry (续费 rollover — 不提前重置、
// 从原到期点顺延). A nil current expiry (open-ended) or a non-positive
// interval has nothing to roll.
func rollFrom(current *time.Time, intervalDays int) *time.Time {
	if current == nil || intervalDays <= 0 {
		return nil
	}
	t := current.UTC().Add(time.Duration(intervalDays) * 24 * time.Hour)
	return &t
}

// ---------------------------------------------------------------------------
// 迁移赠送（设计 §4.3）：独立幂等来源键的赠送发放原语
// ---------------------------------------------------------------------------

// MigrationGift builds the gift entitlement for one (rule, user) pair. The
// source id (MigrationGiftSourceID) is the idempotency key: re-issuing the
// same rule to the same user collides on UNIQUE(source_type, source_id,
// revision) instead of double-granting. Gift entitlements are non-stackable
// and suppressed while any explicit plan is active (SelectEntitlement).
func MigrationGift(ruleID, userID string, modelIDs []string, policyVersionID string, anchor, effectiveFrom time.Time, effectiveTo *time.Time) (*domain.Entitlement, error) {
	if ruleID == "" || userID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "migration gift: rule id and user id required")
	}
	return GrantFromPlan(PlanRevision{
		SourceType:      domain.SourceGrant,
		SourceID:        MigrationGiftSourceID(ruleID, userID),
		ModelIDs:        modelIDs,
		PolicyVersionID: policyVersionID,
	}, anchor, effectiveFrom, effectiveTo)
}
