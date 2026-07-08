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
	OAuthProviders   *OAuthProvidersConfig   `json:"oauth_providers,omitempty"`
}

// BrandConfig carries app-level display strings (e.g. brand name for the
// provider checkout). Operators set these once per app; service layer falls
// back to apps.name when absent so old configs without brand keep working.
type BrandConfig struct {
	Name string `json:"name,omitempty"`
}

type PaymentProvidersConfig struct {
	Paypal *PaypalConfig `json:"paypal,omitempty"`
}

type PaypalConfig struct {
	ClientID     string                      `json:"client_id"`
	ClientSecret string                      `json:"client_secret"`
	WebhookID    string                      `json:"webhook_id"`
	Mode         string                      `json:"mode"` // "live" | "sandbox"
	Plans        map[string]PaypalPlanConfig `json:"plans"`
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

// ProviderToken is the response shape for GET /apps/:id/provider-token/:channel.
// Only PayPal is supported; AccessToken + ExpiresIn are populated per response.
type ProviderToken struct {
	Channel     string `json:"channel"`
	AccessToken string `json:"access_token,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
}

// OAuthProvidersConfig groups all OAuth providers configured for an app.
// Today only GitHub is supported; the block is structured so future
// providers slot in alongside.
type OAuthProvidersConfig struct {
	GitHub *GitHubOAuthConfig `json:"github,omitempty"`
}

// GitHubOAuthConfig stores the GitHub OAuth App credentials Yunhou uses to
// run the /auth/github/redirect + /auth/github/callback flow on behalf of a
// consumer app.
//
// Boundary (design doc §"GitHub OAuth boundary"):
//
//   - ClientID is public — Yunhou may echo it back to the BFF (it appears in
//     the redirect URL anyway).
//   - ClientSecret is server-side only. It must NEVER appear in any response
//     body returned to the BFF or end-user; the handler maps ErrAppNotConfigured
//     or similar without surfacing the secret. Stored plaintext on disk
//     because Yunhou needs it to call GitHub's token endpoint — there is no
//     "hash this and never use it again" pattern available.
//   - CallbackURLs is the whitelist the callback handler matches the incoming
//     redirect_uri against. Multiple entries are allowed because a single
//     consumer app may have multiple client surfaces (web, iOS, Android)
//     sharing one GitHub OAuth App. Each callback must match exactly one
//     entry; no prefix or suffix matching.
type GitHubOAuthConfig struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	CallbackURLs []string `json:"callback_urls"`
}