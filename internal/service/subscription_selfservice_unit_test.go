package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// Validation-branch unit tests for the M2 self-service methods: error
// injection on the repo layer drives the wrapped-error branches that DB
// tests can't reach (no tx is ever begun on these paths, so a nil *sqlx.DB
// service is safe).

type selfServiceSubRepo struct {
	stubSubRepo
	byIDSub *model.Subscription
	byIDErr error
}

func (s *selfServiceSubRepo) FindByID(_ context.Context, _ string) (*model.Subscription, error) {
	if s.byIDErr != nil {
		return nil, s.byIDErr
	}
	if s.byIDSub == nil {
		return nil, sql.ErrNoRows
	}
	return s.byIDSub, nil
}

type selfServicePlanRepo struct {
	stubPlanRepo
	byID   map[string]*model.Plan
	errFor map[string]error
}

func (s *selfServicePlanRepo) FindByID(_ context.Context, id string) (*model.Plan, error) {
	if err, ok := s.errFor[id]; ok {
		return nil, err
	}
	if p, ok := s.byID[id]; ok {
		return p, nil
	}
	return nil, sql.ErrNoRows
}

func newSelfServiceUnitSvc(subRepo *selfServiceSubRepo, planRepo *selfServicePlanRepo) *PaymentService {
	return NewPaymentService(
		nil,
		&stubOrderRepoLookup{},
		nil,
		nil,
		subRepo,
		planRepo,
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
}

func TestCancelSubscriptionByID_FindErrorWrapped(t *testing.T) {
	svc := newSelfServiceUnitSvc(
		&selfServiceSubRepo{byIDErr: errors.New("db down")},
		&selfServicePlanRepo{},
	)
	_, err := svc.CancelSubscriptionByID(context.Background(), "u1", "s1", "")
	if err == nil || !strings.Contains(err.Error(), "find subscription") {
		t.Fatalf("expected wrapped find error, got %v", err)
	}
}

func TestChangePlanByID_ValidationErrorBranches(t *testing.T) {
	paddle := "paddle"
	activeSub := &model.Subscription{
		ID: "s1", UserID: "u1", PlanID: "monthly", Status: "active",
		Channel: &paddle, AutoRenew: true,
		ExternalSubscriptionID: &paddleExt,
	}
	monthly := &model.Plan{ID: "monthly", IntervalDays: 30, IsActive: true}
	yearly := &model.Plan{ID: "yearly", IntervalDays: 365, IsActive: true}

	t.Run("find subscription error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDErr: errors.New("db down")},
			&selfServicePlanRepo{},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find subscription") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("find target plan error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{errFor: map[string]error{"yearly": errors.New("db down")}},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find target plan") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("current plan missing → ErrPlanNotFound", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			// yearly exists, monthly (the sub's plan) does not.
			&selfServicePlanRepo{byID: map[string]*model.Plan{"yearly": yearly}},
		)
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("find current plan error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{
				byID:   map[string]*model.Plan{"yearly": yearly},
				errFor: map[string]error{"monthly": errors.New("db down")},
			},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find current plan") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no paddle client → ErrPaddleNotConfigured (unit)", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{byID: map[string]*model.Plan{"monthly": monthly, "yearly": yearly}},
		)
		svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPaddleNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})
}

var paddleExt = "sub_unit_test"
