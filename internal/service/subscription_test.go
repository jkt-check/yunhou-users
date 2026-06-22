package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

func TestSubscriptionService_Create(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name        string
		userID      string
		planID      string
		setup       func(*mockSubscriptionRepo, *mockPlanRepo)
		expiresAt   *time.Time
		wantErr     bool
		errContains string
	}{
		{
			name:   "create subscription successfully",
			userID: "user-1",
			planID: "free",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true}
			},
			expiresAt: timePtr(time.Now().Add(30 * 24 * time.Hour)),
			wantErr:   false,
		},
		{
			name:   "user already has active subscription",
			userID: "user-2",
			planID: "free",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true}
				expiresAt := time.Now().Add(30 * 24 * time.Hour)
				sr.subs["existing-sub"] = &model.Subscription{
					ID:        "existing-sub",
					UserID:    "user-2",
					PlanID:    "free",
					Status:    "active",
					ExpiresAt: &expiresAt,
				}
				sr.byUserID["user-2"] = sr.subs["existing-sub"]
			},
			wantErr:     true,
			errContains: "already has an active subscription",
		},
		{
			name:   "paid plan rejected — must use admin/payment",
			userID: "user-3",
			planID: "monthly",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "月付", Price: 9.99, IsActive: true}
			},
			wantErr:     true,
			errContains: "paid plan",
		},
		{
			name:   "inactive plan rejected",
			userID: "user-4",
			planID: "legacy",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.plans["legacy"] = &model.Plan{ID: "legacy", Name: "停用", IsActive: false}
			},
			wantErr:     true,
			errContains: "plan is inactive",
		},
		{
			name:        "unknown plan rejected",
			userID:      "user-5",
			planID:      "ghost",
			setup:       func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {},
			wantErr:     true,
			errContains: "plan not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			pr := newMockPlanRepo()

			if tc.setup != nil {
				tc.setup(sr, pr)
			}

			planSvc := &PlanService{planRepo: pr}
			subSvc := NewSubscriptionService(sr, planSvc)

			sub, err := subSvc.Create(ctx, tc.userID, tc.planID, tc.expiresAt)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if tc.errContains != "" && !contains(tc.errContains, err.Error()) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if sub.UserID != tc.userID {
				t.Errorf("expected userID %s, got %s", tc.userID, sub.UserID)
			}
			if sub.PlanID != tc.planID {
				t.Errorf("expected planID %s, got %s", tc.planID, sub.PlanID)
			}
			if sub.Status != "active" {
				t.Errorf("expected status active, got %s", sub.Status)
			}
		})
	}
}

func TestSubscriptionService_Renew(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name        string
		subID       string
		setup       func(*mockSubscriptionRepo)
		expiresAt   *time.Time
		wantErr     bool
		errContains string
	}{
		{
			name:  "renew active subscription",
			subID: "sub-1",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-1"] = &model.Subscription{
					ID:     "sub-1",
					UserID: "user-1",
					PlanID: "monthly",
					Status: "active",
				}
				sr.byUserID["user-1"] = sr.subs["sub-1"]
			},
			expiresAt: timePtr(time.Now().Add(60 * 24 * time.Hour)),
			wantErr:   false,
		},
		{
			name:        "subscription not found",
			subID:       "nonexistent",
			setup:       func(sr *mockSubscriptionRepo) {},
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:  "cannot renew cancelled subscription",
			subID: "sub-cancelled",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-cancelled"] = &model.Subscription{
					ID:     "sub-cancelled",
					UserID: "user-2",
					PlanID: "free",
					Status: "cancelled",
				}
			},
			wantErr:     true,
			errContains: "cannot renew a cancelled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			pr := newMockPlanRepo()
			planSvc := &PlanService{planRepo: pr}
			subSvc := NewSubscriptionService(sr, planSvc)

			if tc.setup != nil {
				tc.setup(sr)
			}

			sub, err := subSvc.Renew(ctx, tc.subID, tc.expiresAt)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if tc.errContains != "" && !contains(tc.errContains, err.Error()) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if sub.Status != "active" {
				t.Errorf("expected status active, got %s", sub.Status)
			}
		})
	}
}

func TestSubscriptionService_Cancel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name        string
		subID       string
		userID      string
		setup       func(*mockSubscriptionRepo)
		wantErr     bool
		errContains string
	}{
		{
			name:   "cancel active subscription",
			subID:  "sub-1",
			userID: "user-1",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-1"] = &model.Subscription{
					ID:     "sub-1",
					UserID: "user-1",
					PlanID: "free",
					Status: "active",
				}
				sr.byUserID["user-1"] = sr.subs["sub-1"]
			},
			wantErr: false,
		},
		{
			name:        "subscription not found",
			subID:       "nonexistent",
			userID:      "user-1",
			setup:       func(sr *mockSubscriptionRepo) {},
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:   "wrong user cannot cancel — surfaces as not found",
			subID:  "sub-other",
			userID: "attacker",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-other"] = &model.Subscription{
					ID:     "sub-other",
					UserID: "victim",
					PlanID: "free",
					Status: "active",
				}
			},
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:   "already cancelled",
			subID:  "sub-cancelled",
			userID: "user-2",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-cancelled"] = &model.Subscription{
					ID:     "sub-cancelled",
					UserID: "user-2",
					PlanID: "free",
					Status: "cancelled",
				}
			},
			wantErr:     true,
			errContains: "already cancelled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			pr := newMockPlanRepo()
			planSvc := &PlanService{planRepo: pr}
			subSvc := NewSubscriptionService(sr, planSvc)

			if tc.setup != nil {
				tc.setup(sr)
			}

			err := subSvc.Cancel(ctx, tc.subID, tc.userID)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if tc.errContains != "" && !contains(tc.errContains, err.Error()) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify status is cancelled
			if sr.subs[tc.subID].Status != "cancelled" {
				t.Errorf("expected status cancelled, got %s", sr.subs[tc.subID].Status)
			}
		})
	}
}

func TestSubscriptionService_GetUserSubscription(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name       string
		userID     string
		setup      func(*mockSubscriptionRepo, *mockPlanRepo)
		wantPlanID string
		wantSubNil bool
		wantErr    bool
	}{
		{
			name:   "user with active subscription",
			userID: "user-1",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				expiresAt := time.Now().Add(30 * 24 * time.Hour)
				sr.subs["sub-1"] = &model.Subscription{
					ID:        "sub-1",
					UserID:    "user-1",
					PlanID:    "monthly",
					Status:    "active",
					ExpiresAt: &expiresAt,
				}
				sr.byUserID["user-1"] = sr.subs["sub-1"]
				pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅"}
			},
			wantPlanID: "monthly",
			wantSubNil: false,
			wantErr:    false,
		},
		{
			name:   "user without subscription gets default plan",
			userID: "user-2",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				// No subscription in byUserID
				pr.defaultPlan = &model.Plan{ID: "free", Name: "免费"}
			},
			wantPlanID: "free",
			wantSubNil: true,
			wantErr:    false,
		},
		{
			name:   "subscription not found error",
			userID: "user-3",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.err = fmt.Errorf("db error")
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			pr := newMockPlanRepo()

			if tc.setup != nil {
				tc.setup(sr, pr)
			}

			planSvc := &PlanService{planRepo: pr}
			subSvc := NewSubscriptionService(sr, planSvc)

			sub, plan, err := subSvc.GetUserSubscription(ctx, tc.userID)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantSubNil && sub != nil {
				t.Errorf("expected nil subscription, got %v", sub)
			}
			if !tc.wantSubNil && sub == nil {
				t.Errorf("expected non-nil subscription")
			}
			if plan.ID != tc.wantPlanID {
				t.Errorf("expected planID %s, got %s", tc.wantPlanID, plan.ID)
			}
		})
	}
}

func TestSubscriptionService_ListUserSubscriptions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("list user subscriptions", func(t *testing.T) {
		sr := newMockSubscriptionRepo()
		pr := newMockPlanRepo()
		planSvc := &PlanService{planRepo: pr}

		sr.subs["sub-1"] = &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "free", Status: "active"}
		sr.subs["sub-2"] = &model.Subscription{ID: "sub-2", UserID: "user-1", PlanID: "monthly", Status: "expired"}
		sr.subs["sub-3"] = &model.Subscription{ID: "sub-3", UserID: "user-2", PlanID: "free", Status: "active"}

		subSvc := NewSubscriptionService(sr, planSvc)

		subs, err := subSvc.ListUserSubscriptions(ctx, "user-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(subs) != 2 {
			t.Errorf("expected 2 subscriptions, got %d", len(subs))
		}
	})
}

// Helper functions

func contains(substr, s string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(substr, s))
}

func containsHelper(substr, s string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func timePtr(t time.Time) *time.Time {
	return &t
}
