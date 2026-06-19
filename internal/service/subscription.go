package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	// Check if user already has an active subscription
	existing, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("user already has an active subscription")
	}

	sub := &model.Subscription{
		ID:        GenerateUUID(),
		UserID:    userID,
		PlanID:    planID,
		Status:    "active",
		StartedAt: time.Now(),
		ExpiresAt: expiresAt,
	}
	if err := s.subRepo.Create(ctx, sub); err != nil {
		if isDuplicateKey(err) {
			return nil, fmt.Errorf("subscription already exists for this user")
		}
		return nil, err
	}
	return sub, nil
}

func (s *SubscriptionService) Renew(ctx context.Context, id string, expiresAt *time.Time) (*model.Subscription, error) {
	sub, err := s.subRepo.FindByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("subscription not found")
	}
	if sub.Status == "cancelled" {
		return nil, fmt.Errorf("cannot renew a cancelled subscription")
	}
	if err := s.subRepo.Renew(ctx, id, expiresAt); err != nil {
		return nil, err
	}
	return s.subRepo.FindByID(ctx, id)
}

func (s *SubscriptionService) Cancel(ctx context.Context, id string) error {
	sub, err := s.subRepo.FindByID(ctx, id)
	if err != nil {
		return fmt.Errorf("subscription not found")
	}
	if sub.Status == "cancelled" {
		return fmt.Errorf("already cancelled")
	}
	return s.subRepo.UpdateStatus(ctx, id, "cancelled")
}

// GetUserSubscription returns the user's active subscription with plan info.
func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		// If "not found", user has no subscription - use default plan
		if err.Error() == "not found" {
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

func isDuplicateKey(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23505"
	}
	if _, ok := err.(interface{ DuplicateKey() bool }); ok {
		return true
	}
	return false
}
