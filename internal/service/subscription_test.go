package service

import (
	"context"
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
		appID       string
		plan        string
		expiresAt   *time.Time
		setup       func(*mockSubscriptionRepo)
		wantErr     bool
		errContains string
		validate    func(t *testing.T, sub *model.Subscription, sr *mockSubscriptionRepo)
	}{
		{
			name:      "create new subscription",
			userID:    "user-1",
			appID:     "app-1",
			plan:      "free",
			expiresAt: nil,
			setup:     func(sr *mockSubscriptionRepo) {},
			wantErr:   false,
			validate: func(t *testing.T, sub *model.Subscription, sr *mockSubscriptionRepo) {
				if sub == nil {
					t.Fatal("expected non-nil subscription")
				}
				if sub.UserID != "user-1" {
					t.Errorf("expected UserID user-1, got %s", sub.UserID)
				}
				if sub.AppID != "app-1" {
					t.Errorf("expected AppID app-1, got %s", sub.AppID)
				}
				if sub.Plan != "free" {
					t.Errorf("expected Plan free, got %s", sub.Plan)
				}
				if sub.Status != "active" {
					t.Errorf("expected Status active, got %s", sub.Status)
				}
				if len(sr.subs) != 1 {
					t.Errorf("expected 1 sub in repo, got %d", len(sr.subs))
				}
			},
		},
		{
			name:    "create subscription with expiry",
			userID:  "user-2",
			appID:   "app-2",
			plan:    "pro",
			expiresAt: func() *time.Time {
				t := time.Now().Add(30 * 24 * time.Hour)
				return &t
			}(),
			setup:   func(sr *mockSubscriptionRepo) {},
			wantErr: false,
			validate: func(t *testing.T, sub *model.Subscription, sr *mockSubscriptionRepo) {
				if sub == nil {
					t.Fatal("expected non-nil subscription")
				}
				if sub.ExpiresAt == nil {
					t.Error("expected non-nil ExpiresAt")
				}
			},
		},
		{
			name:      "duplicate subscription rejected",
			userID:    "user-1",
			appID:     "app-1",
			plan:      "free",
			expiresAt: nil,
			setup: func(sr *mockSubscriptionRepo) {
				existing := &model.Subscription{
					ID:     "sub-exist",
					UserID: "user-1",
					AppID:  "app-1",
					Plan:   "free",
					Status: "active",
				}
				sr.subs["sub-exist"] = existing
				sr.byUserApp["user-1:app-1"] = existing
			},
			wantErr:     true,
			errContains: "subscription already exists",
		},
		{
			name:      "create failure on repo",
			userID:    "user-3",
			appID:     "app-3",
			plan:      "pro",
			expiresAt: nil,
			setup: func(sr *mockSubscriptionRepo) {
				sr.createErr = fmt.Errorf("db insert failed")
			},
			wantErr:     true,
			errContains: "db insert failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			tc.setup(sr)

			svc := NewSubscriptionService(sr)
			sub, err := svc.Create(ctx, tc.userID, tc.appID, tc.plan, tc.expiresAt)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.validate != nil {
				tc.validate(t, sub, sr)
			}
		})
	}
}

func TestSubscriptionService_Renew(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	futureExpiry := timeNow().Add(30 * 24 * time.Hour)

	tests := []struct {
		name        string
		subID       string
		expiresAt   *time.Time
		setup       func(*mockSubscriptionRepo)
		wantErr     bool
		errContains string
	}{
		{
			name:      "renew active subscription",
			subID:     "sub-active",
			expiresAt: &futureExpiry,
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-active"] = &model.Subscription{
					ID:        "sub-active",
					UserID:    "user-1",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "active",
					ExpiresAt: nil,
				}
			},
			wantErr: false,
		},
		{
			name:      "rewn expired subscription (not cancelled)",
			subID:     "sub-expired",
			expiresAt: &futureExpiry,
			setup: func(sr *mockSubscriptionRepo) {
				past := timeNow().Add(-24 * time.Hour)
				sr.subs["sub-expired"] = &model.Subscription{
					ID:        "sub-expired",
					UserID:    "user-2",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "expired",
					ExpiresAt: &past,
				}
			},
			wantErr: false,
		},
		{
			name:      "cannot renew cancelled subscription",
			subID:     "sub-cancelled",
			expiresAt: &futureExpiry,
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-cancelled"] = &model.Subscription{
					ID:        "sub-cancelled",
					UserID:    "user-3",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "cancelled",
					ExpiresAt: nil,
				}
			},
			wantErr:     true,
			errContains: "cannot renew a cancelled subscription",
		},
		{
			name:      "subscription not found",
			subID:     "nonexistent",
			expiresAt: &futureExpiry,
			setup:     func(sr *mockSubscriptionRepo) {},
			wantErr:   true,
			errContains: "subscription not found",
		},
		{
			name:      "renew repo failure",
			subID:     "sub-renew-fail",
			expiresAt: &futureExpiry,
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-renew-fail"] = &model.Subscription{
					ID:     "sub-renew-fail",
					Status: "active",
				}
				sr.renewErr = fmt.Errorf("renew db error")
			},
			wantErr:     true,
			errContains: "renew db error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			tc.setup(sr)

			svc := NewSubscriptionService(sr)
			sub, err := svc.Renew(ctx, tc.subID, tc.expiresAt)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sub == nil {
				t.Fatal("expected non-nil subscription after renew")
			}
			if sub.Status != "active" {
				t.Errorf("expected status active after renew, got %s", sub.Status)
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
		setup       func(*mockSubscriptionRepo)
		wantErr     bool
		errContains string
	}{
		{
			name:  "cancel active subscription",
			subID: "sub-active",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-active"] = &model.Subscription{
					ID:     "sub-active",
					Status: "active",
				}
			},
			wantErr: false,
		},
		{
			name:  "cancel expired subscription",
			subID: "sub-expired",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-expired"] = &model.Subscription{
					ID:     "sub-expired",
					Status: "expired",
				}
			},
			wantErr: false,
		},
		{
			name:        "already cancelled",
			subID:       "sub-cancelled",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-cancelled"] = &model.Subscription{
					ID:     "sub-cancelled",
					Status: "cancelled",
				}
			},
			wantErr:     true,
			errContains: "already cancelled",
		},
		{
			name:        "subscription not found",
			subID:       "nonexistent",
			setup:       func(sr *mockSubscriptionRepo) {},
			wantErr:     true,
			errContains: "subscription not found",
		},
		{
			name:  "update status failure",
			subID: "sub-update-fail",
			setup: func(sr *mockSubscriptionRepo) {
				sr.subs["sub-update-fail"] = &model.Subscription{
					ID:     "sub-update-fail",
					Status: "active",
				}
				sr.updateErr = fmt.Errorf("update db error")
			},
			wantErr:     true,
			errContains: "update db error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			tc.setup(sr)

			svc := NewSubscriptionService(sr)
			err := svc.Cancel(ctx, tc.subID)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify the subscription was marked as cancelled in the mock
			sub := sr.subs[tc.subID]
			if sub != nil && sub.Status != "cancelled" {
				t.Errorf("expected status cancelled, got %s", sub.Status)
			}
		})
	}
}

func TestSubscriptionService_CheckActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name        string
		userID      string
		appID       string
		setup       func(*mockSubscriptionRepo)
		wantActive  bool
		wantErr     bool
	}{
		{
			name:   "active subscription",
			userID: "user-1",
			appID:  "app-1",
			setup: func(sr *mockSubscriptionRepo) {
				sr.byUserApp["user-1:app-1"] = &model.Subscription{
					ID:        "sub-1",
					UserID:    "user-1",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "active",
					ExpiresAt: nil,
				}
				sr.subs["sub-1"] = sr.byUserApp["user-1:app-1"]
			},
			wantActive: true,
		},
		{
			name:   "expired subscription — time passed",
			userID: "user-2",
			appID:  "app-1",
			setup: func(sr *mockSubscriptionRepo) {
				past := timeNow().Add(-24 * time.Hour)
				sr.byUserApp["user-2:app-1"] = &model.Subscription{
					ID:        "sub-2",
					UserID:    "user-2",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "active",
					ExpiresAt: &past,
				}
				sr.subs["sub-2"] = sr.byUserApp["user-2:app-1"]
			},
			wantActive: false,
		},
		{
			name:   "cancelled subscription",
			userID: "user-3",
			appID:  "app-1",
			setup: func(sr *mockSubscriptionRepo) {
				sr.byUserApp["user-3:app-1"] = &model.Subscription{
					ID:     "sub-3",
					UserID: "user-3",
					AppID:  "app-1",
					Plan:   "free",
					Status: "cancelled",
				}
				sr.subs["sub-3"] = sr.byUserApp["user-3:app-1"]
			},
			wantActive: false,
		},
		{
			name:       "no subscription found",
			userID:     "user-4",
			appID:      "app-1",
			setup:      func(sr *mockSubscriptionRepo) {},
			wantActive: false,
			wantErr:    true,
		},
		{
			name:   "active subscription with future expiry",
			userID: "user-5",
			appID:  "app-1",
			setup: func(sr *mockSubscriptionRepo) {
				future := timeNow().Add(30 * 24 * time.Hour)
				sr.byUserApp["user-5:app-1"] = &model.Subscription{
					ID:        "sub-5",
					UserID:    "user-5",
					AppID:     "app-1",
					Plan:      "pro",
					Status:    "active",
					ExpiresAt: &future,
				}
				sr.subs["sub-5"] = sr.byUserApp["user-5:app-1"]
			},
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := newMockSubscriptionRepo()
			tc.setup(sr)

			svc := NewSubscriptionService(sr)
			active, err := svc.CheckActive(ctx, tc.userID, tc.appID)

			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if active != tc.wantActive {
				t.Errorf("expected active=%v, got active=%v", tc.wantActive, active)
			}
		})
	}
}

func TestCheckActive_ExpiredTransition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	sr := newMockSubscriptionRepo()
	past := timeNow().Add(-1 * time.Hour)
	sr.byUserApp["user-exp:app-exp"] = &model.Subscription{
		ID:        "sub-exp",
		UserID:    "user-exp",
		AppID:     "app-exp",
		Plan:      "pro",
		Status:    "active", // still marked active but past expiry
		ExpiresAt: &past,
	}
	sr.subs["sub-exp"] = sr.byUserApp["user-exp:app-exp"]

	svc := NewSubscriptionService(sr)
	active, err := svc.CheckActive(ctx, "user-exp", "app-exp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active != false {
		t.Error("expected active=false for expired subscription")
	}

	// Verify that UpdateStatus was called to transition to "expired"
	sub := sr.subs["sub-exp"]
	if sub.Status != "expired" {
		t.Errorf("expected subscription status updated to 'expired', got %q", sub.Status)
	}
}
