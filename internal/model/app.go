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
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}

// AppConfig is the typed shape of apps.config JSONB. New optional provider
// blocks can be added without a DB migration — callers that omit a block get
// nil and Yunhou treats the provider as unconfigured.
type AppConfig struct {
	PaymentProviders *PaymentProvidersConfig `json:"payment_providers,omitempty"`
}

type PaymentProvidersConfig struct {
	Paypal       *PaypalConfig       `json:"paypal,omitempty"`
	Lemonsqueezy *LemonsqueezyConfig `json:"lemonsqueezy,omitempty"`
}

type PaypalConfig struct {
	ClientID     string            `json:"client_id"`
	ClientSecret string            `json:"client_secret"`
	WebhookID    string            `json:"webhook_id"`
	Mode         string            `json:"mode"` // "live" | "sandbox"
	PlanIDs      map[string]string `json:"plan_ids"`
}

type LemonsqueezyConfig struct {
	APIKey     string            `json:"api_key"`
	StoreID    string            `json:"store_id"`
	VariantIDs map[string]string `json:"variant_ids"`
}

// ProviderToken is the response shape for GET /apps/:id/provider-token/:channel.
// Exactly one of AccessToken (PayPal) or APIKey (LS) is populated per channel.
type ProviderToken struct {
	Channel     string `json:"channel"`
	AccessToken string `json:"access_token,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
}