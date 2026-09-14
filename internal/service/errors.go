package service

import (
	"errors"
	"fmt"

	"github.com/yunhou/users/internal/model"
)

// ErrSessionAlreadyRevoked is re-exported from the model package so service
// code can keep referring to it without an explicit model import at every
// call site. The canonical definition lives in model/session.go to avoid an
// import cycle (repo → service would otherwise happen).
var ErrSessionAlreadyRevoked = model.ErrSessionAlreadyRevoked

// Sentinel errors returned by services. Handlers and other callers should
// match these with errors.Is(), not by comparing err.Error() strings — that
// coupling is exactly the bug class fixed in the auth/subscription work.
//
// Errors here are the user-facing classification: each one corresponds to a
// distinct HTTP-shaped outcome. Anything not on this list is an internal
// error and should be surfaced as a generic 500.
var (
	ErrInvalidProviderToken = errors.New("invalid provider token")
	ErrUnsupportedProvider  = errors.New("unsupported provider")

	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrUserNotFound        = errors.New("user not found")
	ErrUserSuspended       = errors.New("user is suspended")
	ErrUserDeleted         = errors.New("user is deleted")

	ErrSubscriptionNotFound = errors.New("subscription not found")
	ErrAlreadyCancelled     = errors.New("already cancelled")
	ErrCannotRenewCancelled = errors.New("cannot renew a cancelled subscription")
	ErrInvalidExpiresAt     = errors.New("expires_at must be non-nil and in the future")
	ErrUserHasActiveSub     = errors.New("user already has an active subscription")
	ErrPlanDowngrade        = errors.New("downgrade to a shorter billing cycle is not allowed with an active subscription")
	ErrSubscriptionExists   = errors.New("subscription already exists for this user")
	ErrAppNotFound          = errors.New("app not found")
	ErrAppInactive          = errors.New("app is inactive")

	ErrPlanNotFound          = errors.New("plan not found")
	ErrPlanInactive          = errors.New("plan is inactive")
	ErrPaidPlanForbidden     = errors.New("paid plan: payment required, cannot self-subscribe")
	ErrPlanNotAcceptingNew   = errors.New("plan is not accepting new subscriptions")
	ErrPlanCurrencyMismatch  = errors.New("plan currency does not match order currency")
	ErrInvalidAppID          = errors.New("plan apps contains unknown or inactive app_id")
	ErrDeprecatedDefaultPlan = model.ErrDeprecatedDefaultPlan

	// Payment flow (design doc + webhook doc).
	ErrOrderNotFound           = errors.New("order not found")
	ErrOrderNotPending         = errors.New("order is not in pending status")
	ErrOrderChannelMismatch    = errors.New("order already has a paid payment on a different channel")
	ErrOrderAlreadyTerminal    = errors.New("order is in a non-recoverable terminal state")
	ErrPaymentNotFound         = errors.New("payment not found")
	ErrPaymentNotPaid          = errors.New("payment is not in paid status")
	ErrRefundNotFound          = errors.New("refund not found")
	ErrRefundAmountInvalid     = errors.New("refund amount must be > 0 and <= payment amount")
	ErrRefundSumExceedsPayment = errors.New("sum of refunds would exceed payment amount")
	ErrRefundChannelFailed     = errors.New("channel refund API call failed")
	ErrMissingIdempotencyKey   = errors.New("missing Idempotency-Key header")
	ErrInvalidChannel          = errors.New("invalid channel")

	// Chat proxy (POST /chat → LLM upstream, SSE).
	ErrChatNotEnabled       = errors.New("chat is not enabled")
	ErrChatNoAccess         = errors.New("active subscription with access to this app is required")
	ErrChatUnknownModel     = errors.New("unknown chat model")
	ErrChatModelNotAllowed  = errors.New("chat model is not allowed for the current plan")
	ErrChatRequestShape     = errors.New("chat request shape is not supported by the selected model")
	ErrChatRateLimited      = errors.New("chat upstream rate limit exceeded")
	ErrChatUpstreamError    = errors.New("chat upstream error")
	ErrChatUpstreamRejected = errors.New("chat request rejected by upstream")

	// Usage analytics (/user/usage/heartbeat + /admin/stats/*). Wrapped
	// with a detail message (fmt.Errorf %w); handlers map it to 400.
	ErrUsageInvalidParam = errors.New("invalid usage stats parameter")
)

// Normalized codes classifying an upstream 4xx rejection. Surfaced to
// clients (data.upstream_code) so they can tell a retryable-by-rewrite
// failure (context length) apart from billing and content-policy ones.
const (
	UpstreamCodeContextLengthExceeded = "context_length_exceeded"
	UpstreamCodeContentFilter         = "content_filter"
	UpstreamCodeInsufficientBalance   = "insufficient_balance"
	UpstreamCodeInvalidRequest        = "invalid_request"
)

// ChatUpstreamRejection carries the structured detail of an upstream 4xx
// (≠429): the real status, a normalized code, and the sanitized upstream
// message. It unwraps to ErrChatUpstreamRejected, so existing errors.Is
// mappings keep working; the handler additionally surfaces the fields.
type ChatUpstreamRejection struct {
	Status  int    // real upstream HTTP status
	Code    string // one of the UpstreamCode* constants
	Message string // sanitized upstream error message (capped)
}

func (e *ChatUpstreamRejection) Error() string {
	return fmt.Sprintf("%s (status %d, code %s): %s", ErrChatUpstreamRejected, e.Status, e.Code, e.Message)
}

func (e *ChatUpstreamRejection) Unwrap() error { return ErrChatUpstreamRejected }
