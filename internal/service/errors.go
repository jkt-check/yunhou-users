package service

import "errors"

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

	ErrSubscriptionNotActive = errors.New("subscription not active")
	ErrSubscriptionExpired   = errors.New("subscription expired")
	ErrSubscriptionNotFound  = errors.New("subscription not found")
	ErrAlreadyCancelled      = errors.New("already cancelled")
	ErrCannotRenewCancelled  = errors.New("cannot renew a cancelled subscription")
	ErrUserHasActiveSub      = errors.New("user already has an active subscription")
	ErrSubscriptionExists    = errors.New("subscription already exists for this user")

	ErrPlanNotFound    = errors.New("plan not found")
	ErrPlanInactive    = errors.New("plan is inactive")
	ErrPaidPlanForbidden = errors.New("paid plan: payment required, cannot self-subscribe")
)
