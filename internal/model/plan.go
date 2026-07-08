package model

import (
	"time"

	"github.com/lib/pq"
)

type Plan struct {
	ID           string         `db:"id" json:"id"` // free/monthly/quarterly/yearly
	Name         string         `db:"name" json:"name"`
	Price        float64        `db:"price" json:"price"`
	IntervalDays int            `db:"interval_days" json:"interval_days"`
	Apps         pq.StringArray `db:"apps" json:"apps"`
	IsActive     bool           `db:"is_active" json:"is_active"`
	IsDefault    bool           `db:"is_default" json:"is_default"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}

// CycleBaseFormula is the human-readable description of how SubExpiresAt is
// computed. Surfaced in Quote / PublicPlan responses so callers can audit the
// value without re-implementing the math.
const CycleBaseFormula = "now + trial + cycle"

// ResolveCycle returns the cycle configured for this plan under PayPal, or
// the plan.IntervalDays fallback when no PayPal per-plan entry exists.
//
// Single source of truth — both the marketing-page PublicPlan builder and
// the BFF quote endpoint must show identical cycle values, so the resolution
// logic lives here in model (not in handler or service). Adding a new
// payment channel means extending AppConfig and this function together.
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
type PublicPlan struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Price        float64           `json:"price"`
	IntervalDays int               `json:"interval_days"`
	IsDefault    bool              `json:"is_default"`
	// ProviderIDs maps channel → provider plan/variant ID. Empty when the
	// app has no payment provider configured.
	ProviderIDs  map[string]string `json:"provider_ids"`
	// Cycle is the resolved cycle for this plan when PayPal is configured
	// for it; nil when no per-plan cycle is available. The marketing page
	// uses this to display "first X days free, then $Y" — for the quote
	// endpoint, the same logic is recomputed server-side from the
	// authoritative config.
	Cycle        *CycleSummary     `json:"cycle,omitempty"`
}

// CycleSummary is the public view of trial + billing cycle. Used by the
// marketing page to render "X-day trial, then $Y per N days". Kept separate
// from model.CycleConfig (which is in quote.go for the quote endpoint) so the
// marketing-page shape stays minimal.
type CycleSummary struct {
	TrialDays        int `json:"trial_days"`
	BillingCycleDays int `json:"billing_cycle_days"`
}
