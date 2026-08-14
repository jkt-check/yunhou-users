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
// reads the plan row as the source of truth for trial, currency, and billing
// cycle, while reading the app's config for provider IDs. It computes
// sub_expires_at from the plan cycle. It does NOT make any HTTP calls —
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

// Get returns the quote for (appID, planID). Plan.TrialDays, Plan.IntervalDays,
// and Plan.Currency are authoritative for the quote. userID is currently unused
// but kept in the signature for future audit logging — quote calls are user-scoped
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

	cycle := model.CycleConfig{
		TrialDays:        plan.TrialDays,
		BillingCycleDays: plan.IntervalDays,
		Base:             model.CycleBaseFormula,
	}
	subExpires := time.Now().Add(time.Duration(cycle.TrialDays+cycle.BillingCycleDays) * 24 * time.Hour)

	return &model.Quote{
		PlanID: plan.ID,
		// ApplyPlanAmountOverride lets dev/staging drive payment flows
		// at "fake" amounts (e.g. ¥0.01 / ¥0.10) without a per-stage
		// migration. Defaults to plan.Price verbatim when
		// PLAN_AMOUNT_OVERRIDE_JSON is unset. See
		// internal/service/price_override.go for the override contract
		// (parse-at-boot, malformed-env = log-and-noop).
		Amount:       ApplyPlanAmountOverride(plan.ID, plan.Price),
		Currency:     plan.Currency,
		SubExpiresAt: subExpires,
		CycleConfig:  cycle,
		ProviderData: buildProviderData(cfg, app.Name, planID),
	}, nil
}

// buildProviderData assembles the per-channel payload BFF hands to PayPal
// to create a checkout session. brand_name comes from apps.config.brand.name
// and falls back to apps.name — PayPal rejects an empty brand_name with
// 400 INVALID_PARAMETER_VALUE, so an app without a brand block must still
// get a non-empty value. PayPal computes its own billing cycle from
// plan_id; subExpires is not surfaced here (it lives at the top-level
// Quote.sub_expires_at instead).
func buildProviderData(cfg model.AppConfig, appName, planID string) map[string]any {
	out := map[string]any{}
	brandName := appName
	if cfg.Brand != nil && cfg.Brand.Name != "" {
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
