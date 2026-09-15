package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
)

func TestRelayCheckAccess(t *testing.T) {
	now := time.Now()
	activePlan := &model.Plan{ID: "p1", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	future := now.Add(24 * time.Hour)

	cases := []struct {
		name    string
		sub     *model.Subscription
		subErr  error
		plan    *model.Plan
		wantErr error
	}{
		{"有效订阅放行", &model.Subscription{ID: "s1", UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, activePlan, nil},
		{"无订阅拒绝", nil, sql.ErrNoRows, activePlan, ErrRelayNoAccess},
		{"订阅过期拒绝", &model.Subscription{ID: "s1", UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: ptrTime(now.Add(-time.Hour))}, nil, activePlan, ErrRelayNoAccess},
		{"plan 停用拒绝", &model.Subscription{ID: "s1", UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, &model.Plan{ID: "p1", IsActive: false}, ErrRelayNoAccess},
		{"plan 不含该 app 拒绝", &model.Subscription{ID: "s1", UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, &model.Plan{ID: "p1", IsActive: true, Apps: pq.StringArray{"other-app"}}, ErrRelayNoAccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subRepo := newMockSubscriptionRepo()
			if tc.sub != nil {
				subRepo.subs[tc.sub.ID] = tc.sub
				subRepo.byUserID[tc.sub.UserID] = tc.sub
			}
			subRepo.findErr = tc.subErr
			planRepo := newMockPlanRepo()
			if tc.plan != nil {
				planRepo.plans[tc.plan.ID] = tc.plan
			}
			svc := NewRelayService(subRepo, planRepo,
				NewRelayTicketService(testRelaySecret, "", 300*time.Second))
			err := svc.CheckAccess(context.Background(), "u1", "yunhou-website")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}
