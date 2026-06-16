package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

func isDuplicateKey(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23505"
	}
	if _, ok := err.(interface{ DuplicateKey() bool }); ok {
		return true
	}
	return false
}

type SubscriptionService struct {
	subRepo repo.SubscriptionRepo
}

func NewSubscriptionService(subRepo repo.SubscriptionRepo) *SubscriptionService {
	return &SubscriptionService{subRepo: subRepo}
}

func (s *SubscriptionService) Create(ctx context.Context, userID, appID, plan string, expiresAt *time.Time) (*model.Subscription, error) {
	sub := &model.Subscription{
		ID:        GenerateUUID(),
		UserID:    userID,
		AppID:     appID,
		Plan:      plan,
		Status:    "active",
		ExpiresAt: expiresAt,
	}
	if err := s.subRepo.Create(ctx, sub); err != nil {
		if isDuplicateKey(err) {
			return nil, fmt.Errorf("subscription already exists for this user and app")
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

func (s *SubscriptionService) CheckActive(ctx context.Context, userID, appID string) (bool, error) {
	sub, err := s.subRepo.FindByUserApp(ctx, userID, appID)
	if err != nil {
		return false, fmt.Errorf("check subscription: %v", err)
	}
	if sub == nil || sub.Status != "active" {
		return false, nil
	}
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now()) {
		if err := s.subRepo.UpdateStatus(ctx, sub.ID, "expired"); err != nil {
			return false, fmt.Errorf("mark subscription expired: %v", err)
		}
		return false, nil
	}
	return true, nil
}

func ensureActiveSubscription(ctx context.Context, subRepo repo.SubscriptionRepo, userID, appID string) error {
	sub, err := subRepo.FindByUserApp(ctx, userID, appID)
	if err != nil {
		return fmt.Errorf("check subscription: %v", err)
	}
	if sub == nil || sub.Status != "active" {
		return fmt.Errorf("subscription not active")
	}
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now()) {
		if err := subRepo.UpdateStatus(ctx, sub.ID, "expired"); err != nil {
			return fmt.Errorf("mark subscription expired: %v", err)
		}
		return fmt.Errorf("subscription expired")
	}
	return nil
}
