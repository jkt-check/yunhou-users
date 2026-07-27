package model

import "time"

// Quote is the response shape for POST /apps/:id/quote. BFF hands the
// provider_data block to PayPal/LS to create checkout; sub_expires_at is what
// Yunhou will record on the subscription once PayPal/LS confirm the cycle.
// cycle_config echoes back the resolved cycle so BFF can audit / log how
// sub_expires_at was computed.
type Quote struct {
	PlanID       string         `json:"plan_id"`
	Amount       float64        `json:"amount"`
	Currency     string         `json:"currency"`
	SubExpiresAt time.Time      `json:"sub_expires_at"`
	CycleConfig  CycleConfig    `json:"cycle_config"`
	ProviderData map[string]any `json:"provider_data"`
}

// CycleConfig explains how sub_expires_at was computed. Operators should be
// able to compare this with the PayPal dashboard cycle definition to spot
// drift before a customer notices a billing mismatch.
type CycleConfig struct {
	TrialDays        int    `json:"trial_days"`
	BillingCycleDays int    `json:"billing_cycle_days"`
	Base             string `json:"base"` // human-readable: "now + trial + cycle"
}
