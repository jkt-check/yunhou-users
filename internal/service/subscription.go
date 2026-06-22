package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type SubscriptionService struct {
	subRepo  repo.SubscriptionRepo
	planSvc  *PlanService
}

func NewSubscriptionService(subRepo repo.SubscriptionRepo, planSvc *PlanService) *SubscriptionService {
	return &SubscriptionService{subRepo: subRepo, planSvc: planSvc}
}

func (s *SubscriptionService) Create(ctx context.Context, userID, planID string, expiresAt *time.Time) (*model.Subscription, error) {
	// Validate the requested plan and its price. Self-service subscription
	// creation is only allowed for free plans (Price == 0); paid plans must
	// be created by admin endpoints (or a future payment-webhook flow) so
	// users can't grant themselves paid access for free.
	plan, err := s.planSvc.planRepo.FindByID(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find plan: %w", err)
	}
	if !plan.IsActive {
		return nil, ErrPlanInactive
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

	// Check if user already has an active subscription
	existing, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check existing: %w", err)
	}
	if existing != nil {
		return nil, ErrUserHasActiveSub
	}

	sub := &model.Subscription{
		ID:        GenerateUUID(),
		UserID:    userID,
		PlanID:    planID,
		Status:    "active",
		StartedAt: time.Now(),
		ExpiresAt: derivedExpiry,
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
	if err := s.subRepo.Renew(ctx, id, expiresAt); err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	return s.subRepo.FindByID(ctx, id)
}

// Cancel marks a subscription as cancelled. Requires the caller's userID and
// will refuse if the subscription belongs to someone else.
func (s *SubscriptionService) Cancel(ctx context.Context, id, userID string) error {
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
	return s.subRepo.UpdateStatus(ctx, id, "cancelled")
}

// GetUserSubscription returns the user's active subscription with plan info.
func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		// No row → user has no subscription, use default plan
		if errors.Is(err, sql.ErrNoRows) {
			plan, planErr := s.planSvc.planRepo.FindDefault(ctx)
			if planErr != nil {
				return nil, nil, fmt.Errorf("get default plan: %w", planErr)
			}
			return nil, plan, nil
		}
		return nil, nil, fmt.Errorf("get subscription: %w", err)
	}
	if sub == nil {
		// Return default plan
		plan, err := s.planSvc.planRepo.FindDefault(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("get default plan: %w", err)
		}
		return nil, plan, nil
	}
	plan, err := s.planSvc.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return nil, nil, fmt.Errorf("get plan: %w", err)
	}
	return sub, plan, nil
}

// ListUserSubscriptions returns all subscriptions for a user.
func (s *SubscriptionService) ListUserSubscriptions(ctx context.Context, userID string) ([]model.Subscription, error) {
	return s.subRepo.ListByUserID(ctx, userID)
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
