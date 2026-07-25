package model

import (
	"errors"
	"time"

	"github.com/lib/pq"
)

type Plan struct {
	ID                        string         `db:"id" json:"id"` // free/monthly/quarterly/yearly
	Name                      string         `db:"name" json:"name"`
	Price                     float64        `db:"price" json:"price"`
	IntervalDays              int            `db:"interval_days" json:"interval_days"`
	Apps                      pq.StringArray `db:"apps" json:"apps"`
	IsActive                  bool           `db:"is_active" json:"is_active"`
	IsListed                  bool           `db:"is_listed" json:"is_listed"`
	AcceptingNewSubscriptions bool           `db:"accepting_new_subscriptions" json:"accepting_new_subscriptions"`
	Currency                  string         `db:"currency" json:"currency"`
	TrialDays                 int            `db:"trial_days" json:"trial_days"`
	Description               *string        `db:"description" json:"description"`
	DisplayOrder              int            `db:"display_order" json:"display_order"`
	UpdatedAt                 time.Time      `db:"updated_at" json:"updated_at"`
	CreatedAt                 time.Time      `db:"created_at" json:"created_at"`
}

var ErrDeprecatedDefaultPlan = errors.New("default plan concept is deprecated; supply plan_id explicitly")

// CycleBaseFormula is the human-readable description of how SubExpiresAt is
// computed. Surfaced in Quote / PublicPlan responses so callers can audit the
// value without re-implementing the math.
const CycleBaseFormula = "now + trial + cycle"

// ResolveCycle returns the cycle configured for this plan under PayPal, or
// the plan.IntervalDays fallback when no PayPal per-plan entry exists.
//
// This is used by the PublicPlan builder. QuoteService does not call it; the
// quote endpoint inlines the equivalent plan-based cycle configuration.
func ResolveCycle(cfg AppConfig, planID string, planInterval int) CycleConfig {
	providers := cfg.PaymentProviders
	if providers == nil || providers.Paypal == nil {
		return CycleConfig{BillingCycleDays: planInterval, Base: CycleBaseFormula}
	}
	v, ok := providers.Paypal.Plans[planID]
	if !ok {
		return CycleConfig{BillingCycleDays: planInterval, Base: CycleBaseFormula}
	}
	billing := v.BillingCycleDays
	if billing <= 0 {
		billing = planInterval
	}
	return CycleConfig{
		TrialDays:        v.TrialDays,
		BillingCycleDays: billing,
		Base:             CycleBaseFormula,
	}
}

// PublicPlan is the DTO for GET /apps/:id/plans. It extends Plan with the
// per-channel provider IDs and resolved cycle config so the marketing page
// can render checkout buttons that point at the right provider subscription.
//
// IsListed mirrors the plans.is_listed column. Even though FindByApp only
// returns rows with is_listed=true, exposing the flag on the DTO lets the
// marketing page distinguish operator-hidden plans from operator-listed
// plans if a future caller reads from a wider table (e.g. an admin preview
// endpoint), and matches the spec §5.2 "expose is_listed to consumers" rule.
type PublicPlan struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Price        float64           `json:"price"`
	IntervalDays int               `json:"interval_days"`
	Currency     string            `json:"currency"`
	TrialDays    int               `json:"trial_days"`
	Description  *string           `json:"description"`
	Apps         []string          `json:"apps"`
	DisplayOrder int               `json:"display_order"`
	IsListed     bool              `json:"is_listed"`
	ProviderIDs  map[string]string `json:"provider_ids"`
	Cycle        *CycleSummary     `json:"cycle"`
}

// CycleSummary is the public view of trial + billing cycle. Used by the
// marketing page to render "X-day trial, then $Y per N days". Kept separate
// from model.CycleConfig (which is in quote.go for the quote endpoint) so the
// marketing-page shape stays minimal.
type CycleSummary struct {
	TrialDays        int `json:"trial_days"`
	BillingCycleDays int `json:"billing_cycle_days"`
}
