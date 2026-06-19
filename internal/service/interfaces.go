package service

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/model"
)

// AuthServiceInterface defines the interface for authentication operations
type AuthServiceInterface interface {
	Login(ctx context.Context, req LoginRequest) (*LoginResponse, error)
	Logout(ctx context.Context, refreshToken string) error
	RefreshToken(ctx context.Context, refreshToken, appID string) (*LoginResponse, error)
}

// TokenServiceInterface defines the interface for token operations
type TokenServiceInterface interface {
	JWKS() map[string]interface{}
	SignAccessToken(userID, appID string, scope []string) (string, error)
	VerifyAccessToken(token string) (*TokenClaims, error)
	Refresh(ctx context.Context, refreshToken, appID string) (string, string, error)
}

// SubscriptionServiceInterface defines the interface for subscription operations
type SubscriptionServiceInterface interface {
	Create(ctx context.Context, userID, planID string, expiresAt *time.Time) (*model.Subscription, error)
	Renew(ctx context.Context, id string, expiresAt *time.Time) (*model.Subscription, error)
	Cancel(ctx context.Context, id string) error
	GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error)
	ListUserSubscriptions(ctx context.Context, userID string) ([]model.Subscription, error)
}

// PlanServiceInterface defines the interface for plan operations
type PlanServiceInterface interface {
	ListPlans(ctx context.Context) ([]model.Plan, error)
	GetPlan(ctx context.Context, id string) (*model.Plan, error)
	CreatePlan(ctx context.Context, p *model.Plan) error
	UpdatePlan(ctx context.Context, p *model.Plan) error
	DeletePlan(ctx context.Context, id string) error
}
