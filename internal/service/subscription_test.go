package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
				pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true, AcceptingNewSubscriptions: true}
			},
			expiresAt: timePtr(time.Now().Add(30 * 24 * time.Hour)),
			wantErr:   false,
		},
		{
			name:   "user already has active subscription",
			userID: "user-2",
			planID: "free",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true, AcceptingNewSubscriptions: true}
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
				pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "月付", Price: 9.99, IsActive: true, AcceptingNewSubscriptions: true}
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
			name:   "user without subscription has no plan",
			userID: "user-2",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				// No subscription in byUserID
			},
			wantPlanID: "",
			wantSubNil: true,
			wantErr:    false,
		},
		{
			name:   "subscription not found error",
			userID: "user-3",
			setup: func(sr *mockSubscriptionRepo, pr *mockPlanRepo) {
				sr.findErr = fmt.Errorf("db error")
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
			if tc.wantPlanID == "" {
				if plan != nil {
					t.Errorf("expected nil plan, got %v", plan)
				}
			} else if plan == nil || plan.ID != tc.wantPlanID {
				got := "<nil>"
				if plan != nil {
					got = plan.ID
				}
				t.Errorf("expected planID %s, got %s", tc.wantPlanID, got)
			}
		})
	}
}

func TestSubscriptionService_ListUserSubscriptions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("list user subscriptions", func(t *testing.T) {
		t.Parallel()
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

// TestSubscriptionService_GetUserSubscription_NilSub covers the
// "sub == nil" branch in GetUserSubscription. The mock's
// FindActiveByUserID returns (nil, nil) when byUserID has a nil
// entry for the user — this drives the "sub == nil" defensive
// path. In production this is rare (a real DB would return an
// error or a real row) but the defensive check exists.
func TestSubscriptionService_GetUserSubscription_NilSub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sr := newMockSubscriptionRepo()
	pr := newMockPlanRepo()
	// Pre-seed byUserID with a nil entry — simulates "DB returned
	// nil without an error" (defensive branch).
	var nilSub *model.Subscription
	sr.byUserID["u-nilsub"] = nilSub
	planSvc := &PlanService{planRepo: pr}
	subSvc := NewSubscriptionService(sr, planSvc)
	sub, plan, err := subSvc.GetUserSubscription(ctx, "u-nilsub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub != nil {
		t.Errorf("expected nil subscription, got %v", sub)
	}
	if plan != nil {
		t.Errorf("expected nil plan without subscription, got %v", plan)
	}
}

// TestSubscriptionService_GetUserSubscription_RarePaths fills in branches
// the table-driven test doesn't reach: subRepo returning a non-ErrNoRows
// error (e.g. connection failure), and the "get plan" error path when
// the user's active sub exists but the plan lookup fails.
func TestSubscriptionService_GetUserSubscription_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("subRepo FindActiveByUserID generic error", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.findErr = errors.New("db down")
		planSvc := &PlanService{planRepo: newMockPlanRepo()}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, _, err := subSvc.GetUserSubscription(ctx, "user-x")
		if err == nil {
			t.Fatal("expected error from subRepo, got nil")
		}
		if !strings.Contains(err.Error(), "get subscription") {
			t.Errorf("expected wrap 'get subscription', got %q", err.Error())
		}
	})

	t.Run("get plan by id errors out", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		expiresAt := time.Now().Add(30 * 24 * time.Hour)
		sr.subs["sub-x"] = &model.Subscription{
			ID: "sub-x", UserID: "user-x", PlanID: "missing-plan",
			Status: "active", ExpiresAt: &expiresAt,
		}
		sr.byUserID["user-x"] = sr.subs["sub-x"]
		// planRepo.FindByID returns the injected error
		pr := newMockPlanRepo()
		pr.err = errors.New("db down")
		planSvc := &PlanService{planRepo: pr}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, _, err := subSvc.GetUserSubscription(ctx, "user-x")
		if err == nil {
			t.Fatal("expected error from plan lookup, got nil")
		}
		if !strings.Contains(err.Error(), "get plan") {
			t.Errorf("expected wrap 'get plan', got %q", err.Error())
		}
	})
}

// TestSubscriptionService_Create_RejectsNotAcceptingNew covers the new
// `AcceptingNewSubscriptions` guard (spec §6.2 / Task 7). Quarterly is
// `accepting_new_subscriptions=false` in production — existing subscribers
// can renew, but a fresh self-subscribe via `POST /user/subscriptions`
// must be rejected with `ErrPlanNotAcceptingNew`.
func TestSubscriptionService_Create_RejectsNotAcceptingNew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sr := newMockSubscriptionRepo()
	pr := newMockPlanRepo()
	pr.plans["quarterly"] = &model.Plan{
		ID:                        "quarterly",
		Name:                      "季付",
		IsActive:                  true,
		AcceptingNewSubscriptions: false,
		Price:                     0,
	}
	planSvc := &PlanService{planRepo: pr}
	subSvc := NewSubscriptionService(sr, planSvc)

	_, err := subSvc.Create(ctx, "user-1", "quarterly", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrPlanNotAcceptingNew) {
		t.Errorf("expected error wrapping ErrPlanNotAcceptingNew, got %v", err)
	}
}

// TestSubscriptionService_Create_RarePaths covers error paths the
// table-driven Create test doesn't reach: generic planRepo / subRepo
// errors, and subRepo.Create's generic (non-duplicate) error path.
func TestSubscriptionService_Create_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("planRepo FindByID generic error", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		pr := newMockPlanRepo()
		pr.err = errors.New("db down on plan lookup")
		planSvc := &PlanService{planRepo: pr}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, err := subSvc.Create(ctx, "user-x", "free", nil)
		if err == nil {
			t.Fatal("expected error from plan lookup, got nil")
		}
		if !strings.Contains(err.Error(), "find plan") {
			t.Errorf("expected wrap 'find plan', got %q", err.Error())
		}
	})

	t.Run("subRepo FindActiveByUserID generic error", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.findErr = errors.New("db down on active sub lookup")
		pr := newMockPlanRepo()
		pr.plans["free"] = &model.Plan{ID: "free", IsActive: true, AcceptingNewSubscriptions: true}
		planSvc := &PlanService{planRepo: pr}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, err := subSvc.Create(ctx, "user-x", "free", nil)
		if err == nil {
			t.Fatal("expected error from subRepo lookup, got nil")
		}
		if !strings.Contains(err.Error(), "check existing") {
			t.Errorf("expected wrap 'check existing', got %q", err.Error())
		}
	})

	t.Run("subRepo.Create generic error", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.createErr = errors.New("db down on create")
		pr := newMockPlanRepo()
		pr.plans["free"] = &model.Plan{ID: "free", IsActive: true, AcceptingNewSubscriptions: true}
		planSvc := &PlanService{planRepo: pr}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, err := subSvc.Create(ctx, "user-x", "free", nil)
		if err == nil {
			t.Fatal("expected error from subRepo.Create, got nil")
		}
		if !strings.Contains(err.Error(), "create subscription") {
			t.Errorf("expected wrap 'create subscription', got %q", err.Error())
		}
	})

	t.Run("subRepo.Create duplicate-key → ErrSubscriptionExists", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.createErr = &duplicateKeyError{}
		pr := newMockPlanRepo()
		pr.plans["free"] = &model.Plan{ID: "free", IsActive: true, AcceptingNewSubscriptions: true}
		planSvc := &PlanService{planRepo: pr}
		subSvc := NewSubscriptionService(sr, planSvc)
		_, err := subSvc.Create(ctx, "user-x", "free", nil)
		if !errors.Is(err, ErrSubscriptionExists) {
			t.Errorf("expected ErrSubscriptionExists, got %v", err)
		}
	})
}

// TestIsDuplicateKey covers the postgres 23505 / driver DuplicateKey()
// detection used to map unique-constraint errors to friendly service
// sentinels (ErrUserHasActiveSub, ErrSubscriptionExists).
func TestIsDuplicateKey(t *testing.T) {
	t.Parallel()
	t.Run("plain non-DB error → false", func(t *testing.T) {
		t.Parallel()
		if isDuplicateKey(errors.New("plain error")) {
			t.Error("plain error should not classify as duplicate-key")
		}
	})
	t.Run("DuplicateKey() interface impl → true", func(t *testing.T) {
		t.Parallel()
		if !isDuplicateKey(fakeDupKeyErr{}) {
			t.Error("Duck-typed DuplicateKey impl should be detected")
		}
	})
}

type fakeDupKeyErr struct{}

func (fakeDupKeyErr) Error() string      { return "fake dup" }
func (fakeDupKeyErr) DuplicateKey() bool { return true }
