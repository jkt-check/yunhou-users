package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Code is the stable internal error code surface (design §9.1). Protocol
// adapters map these to each API family's native error shape; the codes
// themselves never leak stack traces or secrets.
type Code string

const (
	CodeInvalidInput         Code = "invalid_input"
	CodeInvalidKey           Code = "invalid_key"
	CodeModelNotAllowed      Code = "model_not_allowed"
	CodeQuotaExceeded        Code = "quota_exceeded"
	CodeRateLimited          Code = "rate_limited"
	CodeUpstreamUnavailable  Code = "upstream_unavailable"
	CodeNotFound             Code = "not_found"
	CodeConflict             Code = "conflict"
	CodeInsufficientCapacity Code = "insufficient_capacity"
	// CodeUnpricedCapability rejects a chargeable capability that has no
	// effective price version (设计 §7.1/Task 6: 拒绝未定价且需扣费的能力).
	CodeUnpricedCapability Code = "unpriced_capability"
	CodeInternal           Code = "internal"
)

// Error is the domain error: a stable Code plus a human-readable message.
// Cause chains are preserved for errors.Is/As but never serialized to
// clients verbatim.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("inference/%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("inference/%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError builds an *Error without a cause.
func NewError(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// WrapError builds an *Error preserving the cause chain.
func WrapError(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Cause: cause}
}

// CodeOf extracts the domain Code from err, falling back to CodeInternal
// for foreign errors. A nil error yields an empty code.
func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	var qe *QuotaExceededError
	if errors.As(err, &qe) {
		return CodeQuotaExceeded
	}
	if err == nil {
		return ""
	}
	return CodeInternal
}

// WindowBlock describes one quota window that rejected admission
// (design §9.1: 多窗口共同阻断时返回全部约束). ResetsAt is nil when the
// recovery time is unknown — the API layer must not invent a countdown.
type WindowBlock struct {
	Kind           WindowKind
	LimitMicros    Microcredit
	UsedMicros     Microcredit
	ReservedMicros Microcredit
	ResetsAt       *time.Time
}

// QuotaExceededError carries every constraint that blocked admission so
// the API layer can answer 429 with full window detail (design §9.1).
type QuotaExceededError struct {
	BlockedBy []WindowBlock
	// KeyBudgetExhausted marks the per-Key budget (not a time window) as a
	// blocking constraint.
	KeyBudgetExhausted bool
	// DeficitMicros is the smallest additional credit that would have
	// admitted the request, when known.
	DeficitMicros *Microcredit
}

func (e *QuotaExceededError) Error() string {
	parts := make([]string, 0, len(e.BlockedBy)+1)
	for _, b := range e.BlockedBy {
		parts = append(parts, string(b.Kind))
	}
	if e.KeyBudgetExhausted {
		parts = append(parts, "key_budget")
	}
	return "inference/quota_exceeded: blocked by " + strings.Join(parts, ",")
}

// NewQuotaExceeded builds a QuotaExceededError from blocking windows.
func NewQuotaExceeded(blockedBy []WindowBlock, keyBudget bool) *QuotaExceededError {
	return &QuotaExceededError{BlockedBy: blockedBy, KeyBudgetExhausted: keyBudget}
}

// Sentinel errors for the money/quota arithmetic paths. They are wrapped
// (never shadowed) so callers can errors.Is them through *Error.
var (
	ErrOverflow         = errors.New("inference: arithmetic overflow")
	ErrNegativeValue    = errors.New("inference: negative value not allowed")
	ErrCurrencyMismatch = errors.New("inference: currency mismatch")
	ErrInvalidCurrency  = errors.New("inference: invalid currency (want ISO-4217 uppercase)")
	ErrInvalidRate      = errors.New("inference: invalid rate (denominator must be > 0, numerator >= 0)")
	ErrInvalidDecimal   = errors.New("inference: invalid decimal literal")
	ErrRoundingMode     = errors.New("inference: unknown rounding mode")
)
