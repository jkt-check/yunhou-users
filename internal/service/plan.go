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

func (s *PlanService) CreatePlan(ctx context.Context, p *model.Plan) error {
	return s.planRepo.Create(ctx, p)
}

func (s *PlanService) UpdatePlan(ctx context.Context, p *model.Plan) error {
	return s.planRepo.Update(ctx, p)
}

func (s *PlanService) DeletePlan(ctx context.Context, id string) error {
	return s.planRepo.Delete(ctx, id)
}

// CheckAppAccess checks if a user with given subscription can access the specified app.
func (s *PlanService) CheckAppAccess(ctx context.Context, sub *model.Subscription, appID string) bool {
	if sub == nil {
		// No subscription, use default (free) plan
		defaultPlan, err := s.planRepo.FindDefault(ctx)
		if err != nil {
			return false
		}
		return slices.Contains(defaultPlan.Apps, appID)
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return false
	}
	return slices.Contains(plan.Apps, appID)
}
