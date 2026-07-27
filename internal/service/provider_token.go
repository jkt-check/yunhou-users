package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yunhou/users/internal/model"
)

// AppLookup is the subset of AppRepo the provider-token service needs.
type AppLookup interface {
	FindByID(ctx context.Context, id string) (*model.App, error)
}

// PaypalTokenFetcher abstracts the PayPal CachedClient (composes OAuth + cache).
// PayPal is the one channel that needs an outbound HTTP call (OAuth client_credentials
// grant); the rest just return data we already have in apps.config.
type PaypalTokenFetcher interface {
	FetchToken(ctx context.Context, clientID, clientSecret string) (*model.ProviderToken, error)
}

type ProviderTokenService struct {
	apps   AppLookup
	paypal PaypalTokenFetcher
}

func NewProviderTokenService(apps AppLookup, paypal PaypalTokenFetcher) *ProviderTokenService {
	return &ProviderTokenService{apps: apps, paypal: paypal}
}

// Sentinel errors mapped to HTTP codes by the handler. ErrAppNotFound and
// ErrAppInactive are declared in errors.go (shared with other services).
var (
	ErrUnsupportedChannel    = errors.New("unsupported channel")
	ErrProviderNotConfigured = errors.New("provider not configured for app")
)

// Get returns the token/credential for (appID, channel). Channel must be
// "paypal"; other values return ErrUnsupportedChannel before any DB work
// runs (saves a wasted app/config round-trip on attacker-supplied paths).
func (s *ProviderTokenService) Get(ctx context.Context, appID, channel string) (*model.ProviderToken, error) {
	if channel != "paypal" {
		return nil, ErrUnsupportedChannel
	}
	app, err := s.apps.FindByID(ctx, appID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
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
	if cfg.PaymentProviders == nil || cfg.PaymentProviders.Paypal == nil {
		return nil, ErrProviderNotConfigured
	}
	p := cfg.PaymentProviders.Paypal
	return s.paypal.FetchToken(ctx, p.ClientID, p.ClientSecret)
}
