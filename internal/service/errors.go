package service

import (
	"errors"

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

	ErrSubscriptionNotFound  = errors.New("subscription not found")
	ErrAlreadyCancelled      = errors.New("already cancelled")
	ErrCannotRenewCancelled  = errors.New("cannot renew a cancelled subscription")
	ErrUserHasActiveSub      = errors.New("user already has an active subscription")
	ErrSubscriptionExists    = errors.New("subscription already exists for this user")
	ErrAppNotFound           = errors.New("app not found")
	ErrAppInactive           = errors.New("app is inactive")

	ErrPlanNotFound    = errors.New("plan not found")
	ErrPlanInactive    = errors.New("plan is inactive")
	ErrPaidPlanForbidden = errors.New("paid plan: payment required, cannot self-subscribe")

	// Payment flow (design doc + webhook doc).
	ErrOrderNotFound          = errors.New("order not found")
	ErrOrderNotPending        = errors.New("order is not in pending status")
	ErrOrderChannelMismatch   = errors.New("order already has a paid payment on a different channel")
	ErrOrderAlreadyTerminal   = errors.New("order is in a non-recoverable terminal state")
	ErrPaymentNotFound        = errors.New("payment not found")
	ErrPaymentNotPaid         = errors.New("payment is not in paid status")
	ErrRefundNotFound         = errors.New("refund not found")
	ErrRefundAmountInvalid    = errors.New("refund amount must be > 0 and <= payment amount")
	ErrRefundSumExceedsPayment = errors.New("sum of refunds would exceed payment amount")
	ErrRefundChannelFailed    = errors.New("channel refund API call failed")
	ErrMissingIdempotencyKey  = errors.New("missing Idempotency-Key header")
	ErrInvalidChannel         = errors.New("invalid channel")
)
