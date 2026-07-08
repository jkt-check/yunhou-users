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
