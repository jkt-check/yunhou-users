package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/model"
)

// ErrSubscriptionNotChannelManaged is returned by the channel subscription
// management endpoints when the user's active subscription has no
// Paddle-managed external id (local/WeChat/PayPal subs). Those subs keep
// their existing lifecycle (DELETE /subscriptions/:id, PayPal dashboard).
var ErrSubscriptionNotChannelManaged = errors.New("subscription is not managed by an auto-renewing channel")

// ErrPlanChangeNotUpgrade is returned by UpgradeChannelSubscription when the
// target plan is not a longer cycle than the current one. Only upgrades
// (e.g. monthly → yearly) go through Paddle subscription update +
// proration; downgrades stay manual (cancel + re-purchase) so the user
// keeps the longer cycle they already paid for.
var ErrPlanChangeNotUpgrade = errors.New("plan change is not an upgrade")

// ErrSamePlanChange is returned by UpgradeChannelSubscription when the
// target plan is the plan the subscription is already on.
var ErrSamePlanChange = errors.New("subscription is already on this plan")

// paddleManagedSubID returns the Paddle-side subscription id for a locally
// active subscription whose lifecycle is managed channel-side by Paddle.
// Paddle Billing ids carry the sub_ prefix (PayPal legacy ids are I-...,
// WeChat/local subs have no external id).
func paddleManagedSubID(sub *model.Subscription) (string, error) {
	if sub.ExternalSubscriptionID == nil || !strings.HasPrefix(*sub.ExternalSubscriptionID, "sub_") {
		return "", ErrSubscriptionNotChannelManaged
	}
	return *sub.ExternalSubscriptionID, nil
}

// CancelChannelSubscription cancels the caller's Paddle-managed active
// subscription channel-side with Paddle's DEFAULT effective_from
// (next_billing_period): the buyer keeps access until the paid period
// ends and is never charged again.
//
// The LOCAL status is deliberately left 'active' here: flipping it now
// would revoke access immediately while Paddle still honors the paid
// period. The flip happens when Paddle applies the scheduled change and
// fires subscription.canceled (onPaddleSubscriptionCancelled), so access
// and billing end at the same moment. Until then expires_at already
// bounds the access window, and no renewal webhook will arrive.
func (s *PaymentService) CancelChannelSubscription(ctx context.Context, userID string) (*model.Subscription, error) {
	if s.paddle == nil {
		return nil, ErrPaddleNotConfigured
	}
	sub, err := s.activePaddleSub(ctx, userID)
	if err != nil {
		return nil, err
	}
	extID, _ := paddleManagedSubID(sub) // guaranteed by activePaddleSub
	if err := s.paddle.CancelSubscription(ctx, extID); err != nil {
		return nil, fmt.Errorf("paddle cancel subscription: %w", err)
	}
	if err := s.writeAudit(ctx, "user:"+userID, "paddle_subscription_cancel_requested",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"paddle", "subscription", "cancel"},
		map[string]any{
			"subscription_id":          sub.ID,
			"external_subscription_id": extID,
			"plan_id":                  sub.PlanID,
			"expires_at":               sub.ExpiresAt,
		}); err != nil {
		// The channel-side cancel already succeeded; a failed audit write
		// must not surface as a failure (the user would retry and Paddle
		// would 4xx the double-cancel). Log and continue.
		log.Printf("paddle cancel: audit write failed for subscription %s: %v", sub.ID, err)
	}
	return sub, nil
}

// ChannelUpgradeResult describes a completed channel-side plan upgrade.
type ChannelUpgradeResult struct {
	SubscriptionID string
	FromPlanID     string
	ToPlanID       string
	// NextBilledAt is the channel-authoritative new billing anchor; the
	// local subscription's expires_at is set to the same value.
	NextBilledAt time.Time
}

// UpgradeChannelSubscription moves the caller's Paddle-managed subscription
// to a longer-cycle plan (monthly → yearly) via Paddle subscription update
// with proration_billing_mode=prorated_immediately — Paddle charges the
// prorated difference NOW, so the user never "先退再买". The local
// subscription's plan_id and expires_at are switched in the same call.
//
// The prorated charge arrives later as transaction.completed with
// origin="subscription_update" — NOT subscription_recurring — so it stays
// on the initial-settlement branch and no-ops against the already-paid
// original order (duplicate-paid-order audit only). Local state here is
// driven synchronously by the Paddle API response, not by that webhook.
//
// Note: Paddle silently DISCARDS a subscription's pending scheduled_change
// when items are updated, so cancel-then-upgrade lifts the pending
// cancellation channel-side (no subscription.canceled webhook will ever
// arrive for it). That's the desired outcome — the user re-committed to a
// longer cycle — and the local row simply stays active on the new plan.
func (s *PaymentService) UpgradeChannelSubscription(ctx context.Context, userID, targetPlanID string) (*ChannelUpgradeResult, error) {
	if s.paddle == nil {
		return nil, ErrPaddleNotConfigured
	}
	sub, err := s.activePaddleSub(ctx, userID)
	if err != nil {
		return nil, err
	}
	if sub.PlanID == targetPlanID {
		return nil, ErrSamePlanChange
	}
	toPlan, err := s.planRepo.FindByID(ctx, targetPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find target plan: %w", err)
	}
	if !toPlan.IsActive {
		return nil, ErrPlanInactive
	}
	if !toPlan.AcceptingNewSubscriptions {
		// Same retirement gate as CreateOrder's eligibility check
		// (eligibilityAndInsertOrderTx): an active-but-retired plan must
		// not acquire new billing relationships through the upgrade path
		// either — upgrading IS a new channel-side price attachment.
		return nil, ErrPlanNotAcceptingNew
	}
	if toPlan.ProductCode != sub.ProductCode {
		// Cross-product plan changes are out of scope (coding-plan has its
		// own plan_upgrade_rules flow).
		return nil, ErrPlanChangeNotUpgrade
	}
	fromPlan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find current plan: %w", err)
	}
	if toPlan.IntervalDays <= fromPlan.IntervalDays {
		return nil, ErrPlanChangeNotUpgrade
	}
	priceID := s.paddlePrices[targetPlanID]
	if priceID == "" {
		return nil, ErrPaddlePriceNotConfigured
	}
	extID, _ := paddleManagedSubID(sub) // guaranteed by activePaddleSub

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin upgrade tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock the row and re-verify it's still active BEFORE moving money
	// channel-side. onPaddleSubscriptionCancelled takes the same FOR UPDATE
	// lock; without it, a cancel webhook's flip could land between the
	// unlocked activePaddleSub read and the final UPDATE (0 rows → user
	// sees an error AFTER Paddle already switched the price), and two
	// concurrent upgrade requests could both reach Paddle → two proration
	// charges.
	var locked model.Subscription
	err = tx.GetContext(ctx, &locked,
		`SELECT * FROM subscriptions WHERE id = $1 FOR UPDATE`, sub.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock subscription: %w", err)
	}
	if locked.Status != "active" {
		// Lost the race with a cancel (webhook or local) while validating.
		return nil, ErrSubscriptionNotFound
	}

	// From the Paddle call onward, money may move channel-side
	// (prorated_immediately charges the difference). Any local failure
	// after this point must leave a loud audit trail for ops
	// reconciliation — a silent 5xx here would invite a retry that
	// double-charges.
	failSync := func(cause error) (*ChannelUpgradeResult, error) {
		// Roll the tx back BEFORE writeAudit: the divergence audit must
		// commit independently (the error return below discards the tx),
		// and writeAudit grabs a second pool connection — calling it while
		// this tx still holds one is the MaxOpenConns deadlock class.
		// Rollback after a failed Commit is a harmless sql.ErrTxDone.
		tx.Rollback() //nolint:errcheck
		if aerr := s.writeAudit(ctx, "user:"+userID, "paddle_upgrade_local_sync_failed",
			fmt.Sprintf("subscription:%s", sub.ID),
			[]string{"paddle", "subscription", "upgrade", "diverged"},
			map[string]any{
				"subscription_id":          sub.ID,
				"external_subscription_id": extID,
				"from_plan_id":             sub.PlanID,
				"to_plan_id":               targetPlanID,
				"error":                    cause.Error(),
			}); aerr != nil {
			log.Printf("paddle upgrade: divergence audit write failed for subscription %s: %v", sub.ID, aerr)
		}
		return nil, cause
	}

	nextBilled, err := s.paddle.UpdateSubscriptionPrice(ctx, extID, priceID)
	if err != nil {
		return nil, fmt.Errorf("paddle update subscription: %w", err)
	}
	// Channel-authoritative anchor preferred; fall back to the plan
	// interval when the response carries no next_billed_at. Clamp before
	// the day→Duration multiply (int64-ns wrap); see maxIntervalDays.
	expiresAt := time.Now().Add(time.Duration(min(toPlan.IntervalDays, maxIntervalDays)) * 24 * time.Hour)
	if nextBilled != nil {
		expiresAt = *nextBilled
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET plan_id = $1, expires_at = $2, updated_at = now()
		WHERE id = $3 AND status = 'active'
	`, targetPlanID, expiresAt, sub.ID)
	if err != nil {
		return failSync(fmt.Errorf("switch subscription plan: %w", err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The FOR UPDATE lock makes this near-impossible; keep the guard as
		// a divergence-audited failure rather than a silent error.
		return failSync(ErrSubscriptionNotFound)
	}
	if err := writeAuditOnTx(ctx, &sqlxTx{tx}, "user:"+userID, "paddle_subscription_upgraded",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"paddle", "subscription", "upgrade"},
		map[string]any{
			"subscription_id":          sub.ID,
			"external_subscription_id": extID,
			"from_plan_id":             sub.PlanID,
			"to_plan_id":               targetPlanID,
			"next_billed_at":           expiresAt,
		}); err != nil {
		return failSync(fmt.Errorf("write upgrade audit: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return failSync(fmt.Errorf("commit upgrade tx: %w", err))
	}
	return &ChannelUpgradeResult{
		SubscriptionID: sub.ID,
		FromPlanID:     sub.PlanID,
		ToPlanID:       targetPlanID,
		NextBilledAt:   expiresAt,
	}, nil
}

// activePaddleSub returns the caller's active kaya-membership subscription
// when (and only when) it is managed channel-side by Paddle. Paddle only
// sells kaya-membership today, so the legacy product-scoped lookup is the
// right surface.
func (s *PaymentService) activePaddleSub(ctx context.Context, userID string) (*model.Subscription, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find active subscription: %w", err)
	}
	if sub == nil {
		return nil, ErrSubscriptionNotFound
	}
	if _, err := paddleManagedSubID(sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// onPaddleSubscriptionCancelled handles Paddle's subscription.canceled
// webhook: the cancellation has TAKEN EFFECT channel-side (period end for
// a user self-cancel via CancelChannelSubscription, or immediate for an
// ops dashboard cancel). Flip the local subscription and revoke the
// entitlement in one transaction. Idempotent: an already-cancelled local
// row is a no-op (webhook redelivery).
func (s *PaymentService) onPaddleSubscriptionCancelled(ctx context.Context, e WebhookEvent) error {
	if e.ExternalSubscriptionID == "" {
		return s.writeAudit(ctx, "service", "paddle_cancel_missing_external_sub_id",
			fmt.Sprintf("event:%s", e.EventID),
			[]string{"webhook", "paddle", "cancel", "missing_field"},
			map[string]any{"event_id": e.EventID})
	}

	tx, err := s.dbBeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin cancel tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var sub model.Subscription
	err = tx.GetContext(ctx, &sub,
		`SELECT * FROM subscriptions WHERE external_subscription_id = $1 LIMIT 1 FOR UPDATE`,
		e.ExternalSubscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A Paddle subscription we never stamped (e.g. dashboard-created).
			// Audit-only ack — nothing local to flip.
			return auditAndCommit(ctx, tx, "service", "paddle_cancel_unknown_subscription",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "paddle", "cancel", "unknown_sub"},
				map[string]any{
					"event_id":                 e.EventID,
					"external_subscription_id": e.ExternalSubscriptionID,
				})
		}
		return fmt.Errorf("find subscription by external sub id: %w", err)
	}
	if sub.Status == "cancelled" {
		// Webhook redelivery after we already flipped — no-op.
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status <> 'cancelled'
	`, sub.ID); err != nil {
		return fmt.Errorf("cancel subscription: %w", err)
	}
	if err := writeAuditOnTx(ctx, tx, "service", "paddle_subscription_cancelled",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"webhook", "paddle", "subscription", "cancelled"},
		map[string]any{
			"event_id":                 e.EventID,
			"subscription_id":          sub.ID,
			"external_subscription_id": e.ExternalSubscriptionID,
			"user_id":                  sub.UserID,
		}); err != nil {
		return fmt.Errorf("write cancel audit: %w", err)
	}
	if s.benefitSync != nil {
		if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
			UserID:         sub.UserID,
			ProductCode:    sub.ProductCode,
			Reason:         access.SyncReasonSubCancelled,
			SubscriptionID: sub.ID,
		}, nil); err != nil {
			return fmt.Errorf("enqueue benefit sync: %w", err)
		}
	}
	return tx.Commit()
}
