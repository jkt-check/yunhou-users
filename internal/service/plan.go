package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type PlanService struct {
	planRepo      repo.PlanRepo
	appRepo       AppLookup
	changeLogRepo repo.PlanChangeLogRepo
}

func NewPlanService(planRepo repo.PlanRepo, appRepo AppLookup, changeLogRepo repo.PlanChangeLogRepo) *PlanService {
	return &PlanService{planRepo: planRepo, appRepo: appRepo, changeLogRepo: changeLogRepo}
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
	if err := s.planRepo.Create(ctx, p); err != nil {
		return err
	}

	after := *p
	// The plan mutation has committed; audit logging is best-effort so an audit
	// outage does not turn a successful operation into a user-visible failure.
	_ = s.changeLogRepo.Insert(ctx, p.ID, "admin", "plan_create", nil, &after)
	return nil
}

func (s *PlanService) UpdatePlan(ctx context.Context, p *model.Plan) error {
	current, err := s.planRepo.FindByID(ctx, p.ID)
	if err != nil {
		return err
	}
	before := *current

	if err := s.planRepo.Update(ctx, p); err != nil {
		return err
	}
	after := *p
	// The plan mutation has committed; audit logging is best-effort so an audit
	// outage does not turn a successful operation into a user-visible failure.
	_ = s.changeLogRepo.Insert(ctx, p.ID, "admin", "plan_update", &before, &after)
	return nil
}

func (s *PlanService) DeletePlan(ctx context.Context, id string) error {
	current, err := s.planRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}
	snapshot := *current

	if err := s.planRepo.Delete(ctx, id); err != nil {
		return err
	}
	// The plan mutation has committed; audit logging is best-effort so an audit
	// outage does not turn a successful operation into a user-visible failure.
	_ = s.changeLogRepo.Insert(ctx, id, "admin", "plan_archive", &snapshot, nil)
	return nil
}

// ValidateApps checks that every app_id in the provided list exists in the
// apps table AND is currently active. Used by CreatePlan / UpdatePlan to
// reject unknown or deactivated apps before they hit plans.apps (spec §4.12).
// Returns an error wrapping ErrInvalidAppID on the first failure; the loop
// short-circuits so callers don't get a flood of partial diagnostics.
func (s *PlanService) ValidateApps(ctx context.Context, apps []string) error {
	for _, id := range apps {
		app, err := s.appRepo.FindByID(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrInvalidAppID, id)
			}
			return err
		}
		if !app.IsActive {
			return fmt.Errorf("%w: %s is inactive", ErrInvalidAppID, id)
		}
	}
	return nil
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
