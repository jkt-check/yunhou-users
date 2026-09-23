package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type SubscriptionService struct {
	subRepo repo.SubscriptionRepo
	planSvc *PlanService

	// Task 10: when wired, Cancel flips the subscription AND enqueues the
	// entitlement-sync outbox message in ONE transaction (同事务 outbox),
	// so a cancelled coding-plan subscription revokes its model entitlement
	// with no crash window. Nil (unit tests with mock repos) keeps the
	// legacy non-tx path.
	db          *sqlx.DB
	benefitSync BenefitSyncOutbox
}

func NewSubscriptionService(subRepo repo.SubscriptionRepo, planSvc *PlanService) *SubscriptionService {
	return &SubscriptionService{subRepo: subRepo, planSvc: planSvc}
}

// SetBenefitSync wires the transactional entitlement-sync enqueue used by
// Cancel. Production calls this unconditionally.
func (s *SubscriptionService) SetBenefitSync(db *sqlx.DB, outbox BenefitSyncOutbox) {
	s.db = db
	s.benefitSync = outbox
}

func (s *SubscriptionService) Create(ctx context.Context, userID, planID string, expiresAt *time.Time) (*model.Subscription, error) {
	// Validate the requested plan and its price. Self-service subscription
	// creation is only allowed for free plans (Price == 0); paid plans must
	// be created by admin endpoints (or a future payment-webhook flow) so
	// users can't grant themselves paid access for free.
	// AcceptingNewSubscriptions: plans marked as not accepting new
	// subscriptions (e.g. legacy 'quarterly') reject self-subscribe. Renew is
	// unaffected.
	plan, err := s.planSvc.planRepo.FindByID(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find plan: %w", err)
	}
	// Product guard (Task 10, design §4.1/§4.3): self-serve Create is a
	// kaya-membership-only surface. Coding-plan subscriptions — even a
	// price-0 draft plan — must come through the paid order pipeline so the
	// entitlement grant always has payment evidence. The DB column is
	// NOT NULL (027), so "" only appears in in-memory fixtures (mock repos)
	// and is treated as the legacy kaya shape.
	if plan.ProductCode != "" && plan.ProductCode != model.ProductKayaMembership {
		return nil, ErrSelfServiceProductForbidden
	}
	// Order: IsActive (cheapest / most obvious) → AcceptingNewSubscriptions
	// (orthogonal to active; quarterly is active-but-not-accepting) → Price > 0
	// (defensive — admin could zero a paid plan) → active-sub check (depends on user context).
	if !plan.IsActive {
		return nil, ErrPlanInactive
	}
	if !plan.AcceptingNewSubscriptions {
		return nil, ErrPlanNotAcceptingNew
	}
	if plan.Price > 0 {
		return nil, ErrPaidPlanForbidden
	}

	// Ignore caller-provided expires_at; derive from plan.IntervalDays so the
	// user can't extend the lifetime of a free plan arbitrarily.
	var derivedExpiry *time.Time
	if plan.IntervalDays > 0 {
		t := time.Now().Add(time.Duration(plan.IntervalDays) * 24 * time.Hour)
		derivedExpiry = &t
	}

	// Check if user already has an active subscription IN THIS PLAN'S
	// PRODUCT (migration 027): a kaya-membership sub must not block a
	// coding-plan subscription and vice versa. plan.ProductCode comes from
	// the plan row, not from the user's existing rows.
	existing, err := s.subRepo.FindActiveByUserAndProduct(ctx, userID, plan.ProductCode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check existing: %w", err)
	}
	if existing != nil {
		return nil, ErrUserHasActiveSub
	}

	sub := &model.Subscription{
		ID:          GenerateUUID(),
		UserID:      userID,
		PlanID:      planID,
		ProductCode: plan.ProductCode,
		Status:      "active",
		StartedAt:   time.Now(),
		ExpiresAt:   derivedExpiry,
	}
	if err := s.subRepo.Create(ctx, sub); err != nil {
		if isDuplicateKey(err) {
			return nil, ErrSubscriptionExists
		}
		return nil, fmt.Errorf("create subscription: %w", err)
	}
	return sub, nil
}

func (s *SubscriptionService) Renew(ctx context.Context, id string, expiresAt *time.Time) (*model.Subscription, error) {
	sub, err := s.subRepo.FindByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find subscription: %w", err)
	}
	if sub.Status == "cancelled" {
		return nil, ErrCannotRenewCancelled
	}
	// Reject nil or past expiresAt before the UPDATE — writing
	// expires_at=NULL or expires_at=<past> would put the row into a
	// permanent active state with no future expiry, bypassing every
	// downstream expiry check (the auth path's "active-but-past"
	// detection depends on ExpiresAt being non-nil). Past values are
	// the cn-staging 2026-07-23 incident's root cause; reject loud
	// rather than silently masking it.
	if expiresAt == nil {
		return nil, ErrInvalidExpiresAt
	}
	if !expiresAt.After(time.Now()) {
		return nil, ErrInvalidExpiresAt
	}
	if err := s.subRepo.Renew(ctx, id, expiresAt); err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	return s.subRepo.FindByID(ctx, id)
}

// Cancel marks a subscription as cancelled. Requires the caller's userID and
// will refuse if the subscription belongs to someone else.
//
// Task 10: when the benefit-sync outbox is wired (production), the status
// flip and the entitlement-sync message commit in ONE transaction — a
// cancelled coding-plan subscription's model entitlement is revoked by the
// sync worker with no crash window, and a cancel → re-purchase → cancel
// sequence enqueues twice (no dedup key on purpose; convergence makes the
// duplicate a no-op). When unwired (unit tests over mock repos) the legacy
// non-transactional path runs unchanged.
func (s *SubscriptionService) Cancel(ctx context.Context, id, userID string) error {
	if s.db != nil && s.benefitSync != nil {
		return s.cancelWithBenefitSync(ctx, id, userID)
	}
	return s.cancelLegacy(ctx, id, userID)
}

// cancelWithBenefitSync is the transactional Cancel: lock → guard → flip →
// enqueue → commit.
func (s *SubscriptionService) cancelWithBenefitSync(ctx context.Context, id, userID string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cancel tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var sub model.Subscription
	if err := tx.GetContext(ctx, &sub, `SELECT * FROM subscriptions WHERE id = $1 FOR UPDATE`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return fmt.Errorf("lock subscription: %w", err)
	}
	if sub.UserID != userID {
		// Same response whether the subscription belongs to someone else or
		// doesn't exist, so attackers can't enumerate subscription IDs.
		return ErrSubscriptionNotFound
	}
	if sub.Status == "cancelled" {
		return ErrAlreadyCancelled
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status <> 'cancelled'
	`, id)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost a concurrent cancel race after the FOR UPDATE read.
		return ErrAlreadyCancelled
	}

	payload, err := json.Marshal(access.EntitlementSyncMessage{
		UserID:         userID,
		ProductCode:    sub.ProductCode,
		Reason:         access.SyncReasonSubCancelled,
		SubscriptionID: sub.ID,
	})
	if err != nil {
		return fmt.Errorf("marshal benefit sync message: %w", err)
	}
	if _, err := s.benefitSync.EnqueueOutboxSQLTx(ctx, tx, access.TopicEntitlementSync, payload, nil); err != nil {
		return fmt.Errorf("enqueue benefit sync: %w", err)
	}
	return tx.Commit()
}

// cancelLegacy is the pre-Task-10 Cancel: plain repo calls, no outbox.
func (s *SubscriptionService) cancelLegacy(ctx context.Context, id, userID string) error {
	sub, err := s.subRepo.FindByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSubscriptionNotFound
	}
	if err != nil {
		return fmt.Errorf("find subscription: %w", err)
	}
	if sub.UserID != userID {
		// Same response whether the subscription belongs to someone else or
		// doesn't exist, so attackers can't enumerate subscription IDs.
		return ErrSubscriptionNotFound
	}
	if sub.Status == "cancelled" {
		return ErrAlreadyCancelled
	}
	// The repo's UpdateStatus is a plain UPDATE; without a
	// `WHERE status <> 'cancelled'` guard, a concurrent Cancel racing a
	// Renew could each pass the in-memory check and the last UPDATE
	// wins. Reject here so the in-DB guard returns 0 rows affected on a
	// concurrent double-cancel and the caller sees ErrAlreadyCancelled
	// (the existing behaviour for a no-op double-cancel).
	if err := s.subRepo.UpdateStatus(ctx, id, "cancelled"); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	// Re-read to confirm the row actually transitioned; if a concurrent
	// caller already cancelled, our UPDATE was a no-op.
	refreshed, ferr := s.subRepo.FindByID(ctx, id)
	if ferr != nil {
		return fmt.Errorf("re-read after cancel: %w", ferr)
	}
	if refreshed.Status != "cancelled" {
		return ErrAlreadyCancelled
	}
	return nil
}

// GetUserSubscription returns the user's active KAYA-MEMBERSHIP
// subscription with plan info — the legacy view (design §4.1: old API
// paths that don't name a product operate on kaya-membership only; a
// coding-plan subscription is never surfaced here). If no active
// kaya-membership subscription exists, returns (nil, nil, nil).
func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("get subscription: %w", err)
	}
	if sub == nil {
		return nil, nil, nil
	}
	plan, err := s.planSvc.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return nil, nil, fmt.Errorf("get plan: %w", err)
	}
	return sub, plan, nil
}

// ListUserSubscriptions returns the user's KAYA-MEMBERSHIP subscriptions —
// the legacy contract of GET /user/subscriptions when no product is named.
// Use ListUserSubscriptionsByProduct for an explicit product scope.
func (s *SubscriptionService) ListUserSubscriptions(ctx context.Context, userID string) ([]model.Subscription, error) {
	return s.ListUserSubscriptionsByProduct(ctx, userID, model.ProductKayaMembership)
}

// ListUserSubscriptionsByProduct lists a user's subscriptions (all
// statuses) for one product. The sentinel productCode "all" returns every
// product's rows — reserved for future multi-product consoles; the legacy
// list endpoint never passes it by default.
func (s *SubscriptionService) ListUserSubscriptionsByProduct(ctx context.Context, userID, productCode string) ([]model.Subscription, error) {
	if productCode == "all" {
		return s.subRepo.ListByUserID(ctx, userID)
	}
	if productCode == "" {
		productCode = model.ProductKayaMembership
	}
	return s.subRepo.ListByUserAndProduct(ctx, userID, productCode)
}

// isDuplicateKey reports whether err is a Postgres unique-constraint violation
// (SQLSTATE 23505). The project uses lib/pq, which returns *pq.Error — earlier
// versions checked for pgx's *pgconn.PgError, which never matched, so the
// branch silently never fired.
func isDuplicateKey(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	if _, ok := err.(interface{ DuplicateKey() bool }); ok {
		return true
	}
	return false
}
