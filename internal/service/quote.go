package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// QuoteService assembles the BFF-facing quote for a (app, plan) pair. It
// reads the plan row, the app's config (provider IDs + cycle), and computes
// sub_expires_at from the configured cycle. It does NOT make any HTTP calls —
// PayPal/LS API calls happen in the BFF using the returned provider_data.
type QuoteService struct {
	plans repo.PlanRepo
	apps  AppLookup
}

func NewQuoteService(plans repo.PlanRepo, apps AppLookup) *QuoteService {
	return &QuoteService{plans: plans, apps: apps}
}

// ErrPlanAppMismatch is returned when the requested plan does not include
// the requested app in its `apps` array — i.e. this plan does not grant
// access to that app.
var ErrPlanAppMismatch = errors.New("plan does not include this app")

// Get returns the quote for (appID, planID). userID is currently unused but
// kept in the signature for future audit logging — quote calls are user-scoped
// (the route requires JWT) and operators may want to know who requested what.
func (s *QuoteService) Get(ctx context.Context, appID, planID, userID string) (*model.Quote, error) {
	plan, err := s.plans.FindByID(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find plan: %w", err)
	}
	if !plan.IsActive {
		return nil, ErrPlanInactive
	}
	if !slices.Contains(plan.Apps, appID) {
		return nil, ErrPlanAppMismatch
	}

	app, err := s.apps.FindByID(ctx, appID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		// Some FindByID impls (e.g. the handler mock) return generic errors
		// rather than sql.ErrNoRows; surface those as ErrAppNotFound so the
		// handler can return 404 consistently.
		if errors.Is(err, ErrAppNotFound) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("find app: %w", err)
	}
	if !app.IsActive {
		return nil, ErrAppInactive
	}

	var cfg model.AppConfig
	if len(app.Config) > 0 {
		if err := json.Unmarshal(app.Config, &cfg); err != nil {
			return nil, fmt.Errorf("decode app config: %w", err)
		}
	}

	cycle := resolveCycle(cfg, planID, plan.IntervalDays)
	subExpires := time.Now().Add(time.Duration(cycle.TrialDays+cycle.BillingCycleDays) * 24 * time.Hour)

	return &model.Quote{
		PlanID:       plan.ID,
		Amount:       plan.Price,
		Currency:     "USD",
		SubExpiresAt: subExpires,
		CycleConfig:  cycle,
		ProviderData: buildProviderData(cfg, planID, subExpires),
	}, nil
}

// resolveCycle returns the cycle configured for this plan under PayPal, or
// the plan.interval_days fallback when no PayPal per-plan entry exists.
// Mirrors the logic in handler.buildPublicPlan — keeping them in sync
// prevents the marketing page and the quote from showing different cycle
// values.
func resolveCycle(cfg model.AppConfig, planID string, planInterval int) model.CycleConfig {
	providers := cfg.PaymentProviders
	if providers == nil || providers.Paypal == nil {
		return model.CycleConfig{BillingCycleDays: planInterval, Base: cycleBaseFormula}
	}
	v, ok := providers.Paypal.Plans[planID]
	if !ok {
		return model.CycleConfig{BillingCycleDays: planInterval, Base: cycleBaseFormula}
	}
	billing := v.BillingCycleDays
	if billing <= 0 {
		billing = planInterval
	}
	return model.CycleConfig{
		TrialDays:        v.TrialDays,
		BillingCycleDays: billing,
		Base:             cycleBaseFormula,
	}
}

// cycleBaseFormula is the human-readable description of how SubExpiresAt is
// computed. Surfaced in the Quote / PublicPlan response so callers can audit
// the value without re-implementing the math.
const cycleBaseFormula = "now + trial + cycle"

// buildProviderData assembles the per-channel payload BFF hands to PayPal
// to create a checkout session. brand_name comes from apps.config.brand.name
// (fallback to apps.name). PayPal computes its own billing cycle from
// plan_id; subExpires is not surfaced here (it lives at the top-level
// Quote.sub_expires_at instead).
func buildProviderData(cfg model.AppConfig, planID string, _ time.Time) map[string]any {
	out := map[string]any{}
	brandName := ""
	if cfg.Brand != nil {
		brandName = cfg.Brand.Name
	}
	if providers := cfg.PaymentProviders; providers != nil {
		if pp := providers.Paypal; pp != nil {
			if v, ok := pp.Plans[planID]; ok && v.PlanID != "" {
				out["paypal"] = map[string]any{
					"plan_id": v.PlanID,
					"application_context": map[string]any{
						"brand_name":          brandName,
						"shipping_preference": "NO_SHIPPING",
						"user_action":         "SUBSCRIBE_NOW",
					},
				}
			}
		}
	}
	return out
}