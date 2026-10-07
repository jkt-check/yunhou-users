package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/yunhou/users/internal/model"
)

// M2: user subscription self-management contract endpoints
// (POST /user/subscriptions/:id/cancel, POST /user/subscriptions/:id/change-plan).

// ErrSubscriptionNoAutoRenew: the subscription has no channel auto-renew to
// cancel/change — non-channel subs (wechat_pay, free self-serve, trial,
// admin grants). The handler maps it to the endpoint-specific 409 message
// ("...has no auto-renew to cancel" / "...to change").
var ErrSubscriptionNoAutoRenew = errors.New("subscription has no auto-renew")

// ErrSubscriptionAlreadyEnded: status is expired or cancelled — there is no
// live billing relationship left to cancel or change.
var ErrSubscriptionAlreadyEnded = errors.New("subscription already ended")

// ErrPlanDowngradeNotSupported: the change-plan contract only supports
// upgrades (longer billing cycle); downgrades stay manual (cancel +
// re-purchase) so the user keeps the cycle they paid for.
var ErrPlanDowngradeNotSupported = errors.New("plan downgrade not supported")

// ErrChannelUnavailable: the payment provider call failed (or its result
// could not be applied). Maps to 502; local state is left unchanged.
var ErrChannelUnavailable = errors.New("payment provider unavailable")

// ErrSubscriptionAutoRenewActive is returned by SubscriptionService.Cancel
// (DELETE /user/subscriptions/:id) when the subscription's channel
// auto-renew is still on — the local-only flip would leave the channel
// billing the user. The handler directs the caller to the channel-aware
// cancel endpoint.
var ErrSubscriptionAutoRenewActive = errors.New("use POST /user/subscriptions/:id/cancel for auto-renewing channel subscriptions")

// channelSelfServiceGate classifies the subscription for the self-service
// endpoints: fulfillable Paddle-managed rows return the external
// subscription id; PayPal-managed rows get ErrSubscriptionNotChannelManaged
// (409 with the existing not-channel-managed semantics — the PayPal
// symmetric is P1 and has no frontend entry); everything else (wechat_pay,
// free/trial/admin — no channel auto-renew) gets ErrSubscriptionNoAutoRenew.
func channelSelfServiceGate(sub *model.Subscription) (string, error) {
	if extID, err := paddleManagedSubID(sub); err == nil {
		return extID, nil
	}
	if sub.Channel != nil && *sub.Channel == "paypal" {
		return "", ErrSubscriptionNotChannelManaged
	}
	return "", ErrSubscriptionNoAutoRenew
}

// CancelSubscriptionByID implements POST /user/subscriptions/:id/cancel:
// validates per the contract (404 non-enumeration → ended → channel gate →
// idempotent), cancels channel-side with Paddle's next_billing_period
// effective_from, then flips ONLY auto_renew locally — status/expires_at
// stay untouched (access continues to period end; the subscription.canceled
// webhook flips status).
func (s *PaymentService) CancelSubscriptionByID(ctx context.Context, userID, subID, effectiveFrom string) (*model.Subscription, error) {
	sub, err := s.subRepo.FindByID(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find subscription: %w", err)
	}
	// Same response whether the subscription belongs to someone else or
	// doesn't exist, so attackers can't enumerate subscription IDs.
	if sub.UserID != userID {
		return nil, ErrSubscriptionNotFound
	}
	if sub.Status != "active" {
		return nil, ErrSubscriptionAlreadyEnded
	}
	extID, err := channelSelfServiceGate(sub)
	if err != nil {
		return nil, err
	}
	if !sub.AutoRenew {
		// Idempotent re-cancel: the channel cancel was already requested;
		// return current state without a second Paddle call.
		return sub, nil
	}
	if s.paddle == nil {
		return nil, ErrPaddleNotConfigured
	}
	if err := s.paddle.CancelSubscription(ctx, extID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChannelUnavailable, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE subscriptions SET auto_renew = false, updated_at = now() WHERE id = $1
	`, sub.ID); err != nil {
		// The channel-side cancel already succeeded; a failed local flip
		// must surface loudly (ops reconciles via log) while the user sees
		// a retryable error.
		log.Printf("cancel by id: auto_renew flip failed for subscription %s after paddle cancel: %v", sub.ID, err)
		return nil, fmt.Errorf("flip auto_renew: %w", err)
	}
	sub.AutoRenew = false
	if err := s.writeAudit(ctx, "user:"+userID, "paddle_subscription_cancel_requested",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"paddle", "subscription", "cancel"},
		map[string]any{
			"subscription_id":          sub.ID,
			"external_subscription_id": extID,
			"plan_id":                  sub.PlanID,
			"effective_from":           effectiveFrom,
		}); err != nil {
		log.Printf("cancel by id: audit write failed for subscription %s: %v", sub.ID, err)
	}
	return sub, nil
}

// ChangePlanByID implements POST /user/subscriptions/:id/change-plan:
// Paddle subscription update with proration (prorated_immediately — the
// difference is charged NOW), restricted to upgrades (longer billing
// cycle). On success the local row's plan_id and expires_at follow the
// channel; auto_renew is deliberately NOT touched. The prorated charge
// arrives later as transaction.completed with origin="subscription_update"
// and no-ops against the already-paid original order — see
// UpgradeChannelSubscription's doc comment.
func (s *PaymentService) ChangePlanByID(ctx context.Context, userID, subID, targetPlanID string) (*model.Subscription, error) {
	sub, err := s.subRepo.FindByID(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find subscription: %w", err)
	}
	if sub.UserID != userID {
		return nil, ErrSubscriptionNotFound
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
	fromPlan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find current plan: %w", err)
	}
	if toPlan.IntervalDays <= fromPlan.IntervalDays {
		return nil, ErrPlanDowngradeNotSupported
	}
	if sub.Status != "active" {
		return nil, ErrSubscriptionAlreadyEnded
	}
	extID, err := channelSelfServiceGate(sub)
	if err != nil {
		return nil, err
	}
	if s.paddle == nil {
		return nil, ErrPaddleNotConfigured
	}
	priceID := s.paddlePrices[targetPlanID]
	if priceID == "" {
		// Config gap (PADDLE_PRICES_JSON missing the plan), not a user
		// error — the handler logs and maps this to 502.
		return nil, ErrPaddlePriceNotConfigured
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin change-plan tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock the row and re-verify it's still active BEFORE moving money
	// channel-side — same race guard as UpgradeChannelSubscription (a
	// cancel webhook's flip landing between the unlocked read and the
	// final UPDATE would surface an error AFTER Paddle switched the
	// price).
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
		return nil, ErrSubscriptionAlreadyEnded
	}

	// From the Paddle call onward, money may move channel-side. Any local
	// failure after this point leaves a loud audit trail for ops
	// reconciliation — same failSync shape as UpgradeChannelSubscription.
	failSync := func(cause error) (*model.Subscription, error) {
		tx.Rollback() //nolint:errcheck
		if aerr := s.writeAudit(ctx, "user:"+userID, "paddle_change_plan_local_sync_failed",
			fmt.Sprintf("subscription:%s", sub.ID),
			[]string{"paddle", "subscription", "change_plan", "diverged"},
			map[string]any{
				"subscription_id":          sub.ID,
				"external_subscription_id": extID,
				"from_plan_id":             sub.PlanID,
				"to_plan_id":               targetPlanID,
				"error":                    cause.Error(),
			}); aerr != nil {
			log.Printf("change plan: divergence audit write failed for subscription %s: %v", sub.ID, aerr)
		}
		return nil, cause
	}

	nextBilled, err := s.paddle.UpdateSubscriptionPrice(ctx, extID, priceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChannelUnavailable, err)
	}
	// Channel-authoritative anchor preferred; fall back to the plan
	// interval when the response carries no next_billed_at. Clamp before
	// the day→Duration multiply (int64-ns wrap); see maxIntervalDays.
	expiresAt := time.Now().Add(time.Duration(min(toPlan.IntervalDays, maxIntervalDays)) * 24 * time.Hour)
	if nextBilled != nil {
		expiresAt = *nextBilled
	}

	// plan_id + expires_at follow the channel; auto_renew is NOT in the
	// column list on purpose — a plan change never alters the buyer's
	// renewal intent.
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET plan_id = $1, expires_at = $2, updated_at = now()
		WHERE id = $3 AND status = 'active'
	`, targetPlanID, expiresAt, sub.ID)
	if err != nil {
		return failSync(fmt.Errorf("switch subscription plan: %w", err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return failSync(ErrSubscriptionNotFound)
	}
	if err := writeAuditOnTx(ctx, &sqlxTx{tx}, "user:"+userID, "paddle_subscription_plan_changed",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"paddle", "subscription", "change_plan"},
		map[string]any{
			"subscription_id":          sub.ID,
			"external_subscription_id": extID,
			"from_plan_id":             sub.PlanID,
			"to_plan_id":               targetPlanID,
			"next_billed_at":           expiresAt,
		}); err != nil {
		return failSync(fmt.Errorf("write change-plan audit: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return failSync(fmt.Errorf("commit change-plan tx: %w", err))
	}
	sub.PlanID = targetPlanID
	sub.ExpiresAt = &expiresAt
	return sub, nil
}
