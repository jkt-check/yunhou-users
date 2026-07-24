package service

import (
	"context"
	"slices"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type PlanService struct {
	planRepo repo.PlanRepo
}

func NewPlanService(planRepo repo.PlanRepo) *PlanService {
	return &PlanService{planRepo: planRepo}
}

func (s *PlanService) ListPlans(ctx context.Context) ([]model.Plan, error) {
	return s.planRepo.FindAll(ctx)
}

func (s *PlanService) GetPlan(ctx context.Context, id string) (*model.Plan, error) {
	return s.planRepo.FindByID(ctx, id)
}

// FindByApp returns active plans whose `apps` array contains appID. Used by
// the public GET /apps/:id/plans endpoint (M2) to enumerate plans available
// for a given app — including their provider_ids sourced from
// apps.config.payment_providers.
func (s *PlanService) FindByApp(ctx context.Context, appID string) ([]model.Plan, error) {
	return s.planRepo.FindByApp(ctx, appID)
}

func (s *PlanService) CreatePlan(ctx context.Context, p *model.Plan) error {
	if p.Currency == "" {
		p.Currency = "CNY"
	}
	// Phase 1 keeps the legacy column, but newly created plans must never be
	// designated as the default plan.
	p.IsDefault = false
	return s.planRepo.Create(ctx, p)
}

func (s *PlanService) UpdatePlan(ctx context.Context, p *model.Plan) error {
	return s.planRepo.Update(ctx, p)
}

func (s *PlanService) DeletePlan(ctx context.Context, id string) error {
	return s.planRepo.Delete(ctx, id)
}

// CheckAppAccess checks if a user with given subscription can access the specified app.
// A deactivated plan (or default plan) yields no access — even if its apps[]
// array still lists appID. This matters when an operator retires a SKU
// mid-cycle: existing subscribers should not keep access via the stale
// plan row. The DB doesn't filter deactivated plans here because the
// repo contract is "give me the row"; the service layer applies the
// is_active policy.
func (s *PlanService) CheckAppAccess(ctx context.Context, sub *model.Subscription, appID string) bool {
	if sub == nil {
		// No subscription, use default (free) plan
		defaultPlan, err := s.planRepo.FindDefault(ctx)
		if err != nil {
			return false
		}
		if !defaultPlan.IsActive {
			return false
		}
		return slices.Contains(defaultPlan.Apps, appID)
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return false
	}
	if !plan.IsActive {
		return false
	}
	return slices.Contains(plan.Apps, appID)
}
