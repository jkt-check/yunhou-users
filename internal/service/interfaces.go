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
}

// SubscriptionServiceInterface defines the interface for subscription operations
type SubscriptionServiceInterface interface {
	Create(ctx context.Context, userID, planID string, expiresAt *time.Time) (*model.Subscription, error)
	Renew(ctx context.Context, id string, expiresAt *time.Time) (*model.Subscription, error)
	Cancel(ctx context.Context, id, userID string) error
	GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error)
	ListUserSubscriptions(ctx context.Context, userID string) ([]model.Subscription, error)
}

// PlanServiceInterface defines the interface for plan operations
type PlanServiceInterface interface {
	ListPlans(ctx context.Context) ([]model.Plan, error)
	GetPlan(ctx context.Context, id string) (*model.Plan, error)
	FindByApp(ctx context.Context, appID string) ([]model.Plan, error)
	CreatePlan(ctx context.Context, p *model.Plan) error
	UpdatePlan(ctx context.Context, p *model.Plan) error
	DeletePlan(ctx context.Context, id string) error
}

// PaymentServiceInterface is the v1 payment data flow surface that handlers
// depend on. See docs/plans/2026-06-16-user-system-design.md and
// docs/plans/2026-06-23-payment-webhook-mechanism.md for full semantics.
//
// RefundRequest is the request shape for POST /refunds; PaymentService.Refund
// returns the (idempotent) refund row and whether it was newly inserted or
// resolved from a previous Idempotency-Key match.
type PaymentServiceInterface interface {
	CreateOrder(ctx context.Context, userID, planID string) (*model.Order, error)
	CancelOrder(ctx context.Context, orderID, userID string) error
	Confirm(ctx context.Context, in ConfirmInput) (*ConfirmResult, error)
	Refund(ctx context.Context, in RefundInput) (*RefundResult, error)
	OnWebhook(ctx context.Context, e WebhookEvent) (*OnWebhookResult, error)
	GetOrder(ctx context.Context, orderID, userID string) (*model.Order, error)
	ListUserPayments(ctx context.Context, userID string) ([]model.Payment, error)
	GetPayment(ctx context.Context, paymentID, userID string) (*model.Payment, error)
	ListPaymentRefunds(ctx context.Context, paymentID, userID string) ([]model.Refund, error)
	GetRefund(ctx context.Context, refundID, userID string) (*model.Refund, error)
}
