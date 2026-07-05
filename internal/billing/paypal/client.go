// Package paypal provides PayPal-specific billing primitives.
package paypal

// Mode selects the PayPal API base URL.
type Mode string

const (
	ModeLive    Mode = "live"
	ModeSandbox Mode = "sandbox"
)

// BaseURL returns the PayPal REST API base for the given mode.
func (m Mode) BaseURL() string {
	switch m {
	case ModeSandbox:
		return "https://api-m.sandbox.paypal.com"
	default:
		return "https://api-m.paypal.com"
	}
}

// Token is a low-level PayPal OAuth access token. The provider-token layer
// wraps this in model.ProviderToken for the API response shape.
type Token struct {
	AccessToken string
	ExpiresIn   int // seconds
}