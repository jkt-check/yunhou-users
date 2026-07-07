package model

import (
	"encoding/json"
	"time"
)

type App struct {
	AppID       string          `db:"app_id" json:"app_id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	Config      json.RawMessage `db:"config" json:"config"`
	IsActive    bool            `db:"is_active" json:"is_active"`
	SecretHash  string          `db:"secret_hash" json:"-"` // bcrypt hash; never serialize (plaintext is returned once at create / rotate time)
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}

// AppConfig is the typed shape of apps.config JSONB. New optional provider
// blocks can be added without a DB migration — callers that omit a block get
// nil and Yunhou treats the provider as unconfigured.
type AppConfig struct {
	Brand            *BrandConfig            `json:"brand,omitempty"`
	PaymentProviders *PaymentProvidersConfig `json:"payment_providers,omitempty"`
}

// BrandConfig carries app-level display strings (e.g. brand name for the
// provider checkout). Operators set these once per app; service layer falls
// back to apps.name when absent so old configs without brand keep working.
type BrandConfig struct {
	Name string `json:"name,omitempty"`
}

type PaymentProvidersConfig struct {
	Paypal       *PaypalConfig       `json:"paypal,omitempty"`
	Lemonsqueezy *LemonsqueezyConfig `json:"lemonsqueezy,omitempty"`
}

type PaypalConfig struct {
	ClientID     string                       `json:"client_id"`
	ClientSecret string                       `json:"client_secret"`
	WebhookID    string                       `json:"webhook_id"`
	Mode         string                       `json:"mode"` // "live" | "sandbox"
	Plans        map[string]PaypalPlanConfig  `json:"plans"`
}

// PaypalPlanConfig is the per-plan record under paypal.plans. It carries the
// PayPal subscription plan ID plus the cycle info that drives sub_expires_at
// calculation — these must mirror the PayPal dashboard configuration or the
// computed expiry will diverge from what PayPal actually bills.
type PaypalPlanConfig struct {
	PlanID           string `json:"plan_id"`
	TrialDays        int    `json:"trial_days,omitempty"`
	BillingCycleDays int    `json:"billing_cycle_days,omitempty"`
}

type LemonsqueezyConfig struct {
	APIKey  string                  `json:"api_key"`
	StoreID string                  `json:"store_id"`
	Plans   map[string]LSPlanConfig `json:"plans"`
}

// LSPlanConfig mirrors PaypalPlanConfig for LemonSqueezy — same cycle fields,
// different ID field name (variant_id instead of plan_id).
type LSPlanConfig struct {
	VariantID        string `json:"variant_id"`
	TrialDays        int    `json:"trial_days,omitempty"`
	BillingCycleDays int    `json:"billing_cycle_days,omitempty"`
}

// ProviderToken is the response shape for GET /apps/:id/provider-token/:channel.
// Exactly one of AccessToken (PayPal) or APIKey (LS) is populated per channel.
type ProviderToken struct {
	Channel     string `json:"channel"`
	AccessToken string `json:"access_token,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
}