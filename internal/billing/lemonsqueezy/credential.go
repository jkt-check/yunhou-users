// Package lemonsqueezy provides LemonSqueezy-specific billing primitives.
// Yunhou currently does not call LS APIs outbound — LS is webhook-only —
// so this package holds credential lookup only.
package lemonsqueezy

import (
	"errors"

	"github.com/yunhou/users/internal/model"
)

// Credential exposes the LS api_key stored in apps.config.payment_providers
// .lemonsqueezy. No caching needed: the key is static.
type Credential struct {
	cfg *model.LemonsqueezyConfig
}

func NewCredential(cfg *model.LemonsqueezyConfig) *Credential {
	return &Credential{cfg: cfg}
}

// APIKey returns the configured LS api_key. Returns an error if the app has
// no lemonsqueezy config block; the service layer maps this to a 400.
func (c *Credential) APIKey() (string, error) {
	if c.cfg == nil || c.cfg.APIKey == "" {
		return "", errors.New("lemonsqueezy not configured for this app")
	}
	return c.cfg.APIKey, nil
}