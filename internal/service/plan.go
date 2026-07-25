package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type PlanService struct {
	planRepo      repo.PlanRepo
	appRepo       AppLookup
	changeLogRepo repo.PlanChangeLogRepo

	// validateAppsForShareFn, if non-nil, replaces the production
	// SQL-driven FOR SHARE app lookup inside validateAppsForShareTx.
	// Tests use it to drive the "app-active-at-ValidateApps but
	// app-inactive-at-lock-step" race without standing up a tx-aware
	// app repo. Production leaves this nil and uses the default path.
	validateAppsForShareFn func(ctx context.Context, tx *sqlx.Tx, apps []string) error
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

// normalizeActorID pins the audit-log actor to the authenticated internal
// app identity ("admin:<appID>") when present, and falls back to "admin"
// otherwise. Backward-compatible: callers that don't yet thread the
// internal-app identity (existing internal jobs, hand-invoked REPLs)
// still record *something* in actor_id rather than panicking or silently
// logging empty strings. The handler MUST pass "admin:<appID>" — see
// internal/handler/app.go's CreatePlan/UpdatePlan/DeletePlan.
func normalizeActorID(actorID string) string {
	if actorID == "" {
		return "admin"
	}
	return actorID
}

func (s *PlanService) CreatePlan(ctx context.Context, p *model.Plan, actorID string) error {
	// Defense-in-depth (B1): reject unknown / inactive apps BEFORE the
	// transaction / repo write. The handler already calls ValidateApps,
	// but a future internal caller (batch job, second handler) could
	// otherwise slip past the FK-less TEXT[] apps column and insert a
	// row pointing at a non-existent app. The in-tx FOR SHARE check
	// below still runs for TOCTOU safety (D8).
	if err := s.ValidateApps(ctx, p.Apps); err != nil {
		return err
	}
	if p.Currency == "" {
		p.Currency = "CNY"
	}

	after := *p
	actor := normalizeActorID(actorID)

	// TOCTOU safety (D8 part 2): the ValidateApps eligibility loop (does
	// each app exist? is each app active?) and the plans INSERT must
	// commit atomically with respect to concurrent app deactivations.
	// Without FOR SHARE on each app row, an admin PATCH /apps/:id with
	// is_active=false could land between ValidateApps and the plan
	// INSERT, leaving a plan pointing at a now-inactive app.
	return s.planRepo.WithTx(ctx, func(tx *sqlx.Tx) error {
		if err := s.validateAppsForShareTx(ctx, tx, p.Apps); err != nil {
			return err
		}
		if err := s.planRepo.CreateTx(ctx, tx, p); err != nil {
			return err
		}
		return s.changeLogRepo.InsertTx(ctx, tx, p.ID, actor, "plan_create", nil, &after)
	})
}

func (s *PlanService) UpdatePlan(ctx context.Context, p *model.Plan, actorID string) error {
	current, err := s.planRepo.FindByID(ctx, p.ID)
	if err != nil {
		return err
	}
	before := *current
	after := *p
	actor := normalizeActorID(actorID)

	// Defense-in-depth (B1): see CreatePlan. Use the same apps list the
	// in-tx FOR SHARE check will lock — if the update doesn't touch
	// apps, fall back to the existing apps so we validate whatever will
	// end up in the row. An internal caller that bypasses the handler
	// (which already validates `*req.Apps`) would otherwise slip past
	// the FK-less TEXT[] apps column.
	apps := p.Apps
	if len(apps) == 0 {
		apps = before.Apps
	}
	if err := s.ValidateApps(ctx, apps); err != nil {
		return err
	}

	// TOCTOU safety (D8 part 2): see CreatePlan — wraps the
	// ValidateApps FOR SHARE loop + UPDATE + audit insert in one tx.
	return s.planRepo.WithTx(ctx, func(tx *sqlx.Tx) error {
		if err := s.validateAppsForShareTx(ctx, tx, apps); err != nil {
			return err
		}
		if err := s.planRepo.UpdateTx(ctx, tx, p); err != nil {
			return err
		}
		return s.changeLogRepo.InsertTx(ctx, tx, p.ID, actor, "plan_update", &before, &after)
	})
}

func (s *PlanService) DeletePlan(ctx context.Context, id string, actorID string) error {
	current, err := s.planRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}
	snapshot := *current
	actor := normalizeActorID(actorID)

	// Audit insert MUST happen before the hard delete. The
	// plan_change_log.plan_id FK was relaxed to ON DELETE SET NULL in
	// migration 013 (so an existing audit row survives a plan delete
	// with plan_id=NULL), but at the moment of the INSERT the row must
	// still exist for the FK to be satisfied. Both writes are wrapped
	// in a transaction (D8): if the hard delete fails with SQLSTATE
	// 23503 (FK violation: subscriptions/orders still reference the
	// plan), the tx rolls back taking the just-inserted audit row with
	// it, so the operator never sees a phantom "plan archived" row for
	// a plan that still exists.
	return s.planRepo.WithTx(ctx, func(tx *sqlx.Tx) error {
		if err := s.changeLogRepo.InsertTx(ctx, tx, id, actor, "plan_archive", &snapshot, nil); err != nil {
			return err
		}
		return s.planRepo.DeleteTx(ctx, tx, id)
	})
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

// validateAppsForShareTx is the D8 transactional sibling of ValidateApps.
// It uses SELECT ... FOR SHARE on each app row so a concurrent app
// deactivation (admin PATCH apps/:id with is_active=false) cannot race
// past the validation step before the plan INSERT/UPDATE commits.
// Returns the same error shape as ValidateApps.
//
// The plan service uses a separate path for the FOR SHARE lookup because
// AppLookup's FindByID doesn't accept a *sqlx.Tx — the production
// repo-backed implementation handles both via direct SQL inside the
// tx. Tests stub validateAppsForShareFn to fire the desired error
// (e.g. ErrAppInactive-from-lock) without standing up a tx.
func (s *PlanService) validateAppsForShareTx(ctx context.Context, tx *sqlx.Tx, apps []string) error {
	if s.validateAppsForShareFn != nil {
		return s.validateAppsForShareFn(ctx, tx, apps)
	}
	for _, id := range apps {
		var app model.App
		err := tx.GetContext(ctx, &app, `SELECT app_id, name, description, COALESCE(config, '{}'::jsonb) AS config, is_active, COALESCE(secret_hash, '') AS secret_hash, created_at, updated_at FROM apps WHERE app_id = $1 FOR SHARE`, id)
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
// A deactivated plan yields no access — even if its apps[] array still lists appID.
// This matters when an operator retires a SKU mid-cycle: existing subscribers
// should not keep access via the stale plan row. The DB doesn't filter deactivated
// plans here because the repo contract is "give me the row"; the service layer
// applies the is_active policy.
func (s *PlanService) CheckAppAccess(ctx context.Context, sub *model.Subscription, appID string) bool {
	if sub == nil {
		return false
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
