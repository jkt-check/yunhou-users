package service

import (
	"context"
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
type PaypalTokenFetcher interface {
	FetchToken(ctx context.Context, clientID, clientSecret string) (*model.ProviderToken, error)
}

// LSCredential abstracts the LemonSqueezy credential lookup.
type LSCredential interface {
	APIKey() (string, error)
}

type ProviderTokenService struct {
	apps   AppLookup
	paypal PaypalTokenFetcher
	ls     LSCredential
}

func NewProviderTokenService(apps AppLookup, paypal PaypalTokenFetcher, ls LSCredential) *ProviderTokenService {
	return &ProviderTokenService{apps: apps, paypal: paypal, ls: ls}
}

// Sentinel errors mapped to HTTP codes by the handler. ErrAppNotFound and
// ErrAppInactive are declared in errors.go (shared with other services).
var (
	ErrUnsupportedChannel    = errors.New("unsupported channel")
	ErrProviderNotConfigured = errors.New("provider not configured for app")
)

// Get returns the token/credential for (appID, channel). Channel must be
// "paypal" or "lemonsqueezy"; other values return ErrUnsupportedChannel.
func (s *ProviderTokenService) Get(ctx context.Context, appID, channel string) (*model.ProviderToken, error) {
	app, err := s.apps.FindByID(ctx, appID)
	if err != nil {
		return nil, ErrAppNotFound
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
	switch channel {
	case "paypal":
		if cfg.PaymentProviders == nil || cfg.PaymentProviders.Paypal == nil {
			return nil, ErrProviderNotConfigured
		}
		p := cfg.PaymentProviders.Paypal
		return s.paypal.FetchToken(ctx, p.ClientID, p.ClientSecret)
	case "lemonsqueezy":
		if cfg.PaymentProviders == nil || cfg.PaymentProviders.Lemonsqueezy == nil {
			return nil, ErrProviderNotConfigured
		}
		key, err := s.ls.APIKey()
		if err != nil {
			return nil, ErrProviderNotConfigured
		}
		return &model.ProviderToken{Channel: "lemonsqueezy", APIKey: key}, nil
	default:
		return nil, ErrUnsupportedChannel
	}
}