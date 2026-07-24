package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// isValidIdempotencyKey enforces the Idempotency-Key character set. We
// allow only ASCII letters, digits, and a few separators commonly used
// by SDK-generated keys (UUID, Stripe-style). Anything else risks silent
// truncation or encoding bugs at the DB boundary.
func isValidIdempotencyKey(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

// PaymentHandler exposes the v1 payment data flow endpoints.
// All endpoints require JWT auth (set by middleware.JWTAuth) except where
// noted; ownership is enforced via the service layer (a caller can only
// read/write their own orders / payments / refunds).
type PaymentHandler struct {
	svc service.PaymentServiceInterface
}

func NewPaymentHandler(svc service.PaymentServiceInterface) *PaymentHandler {
	return &PaymentHandler{svc: svc}
}

// ============================================================================
// Order endpoints
// ============================================================================

// CreateOrder — POST /payments/orders
func (h *PaymentHandler) CreateOrder(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	var req struct {
		PlanID  string `json:"plan_id" binding:"required"`
		Channel string `json:"channel" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	order, err := h.svc.CreateOrder(c.Request.Context(), userID, req.PlanID, req.Channel)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": order})
}

// GetOrder — GET /payments/orders/:id
func (h *PaymentHandler) GetOrder(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	order, err := h.svc.GetOrder(c.Request.Context(), c.Param("id"), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": order})
}

// CancelOrder — DELETE /payments/orders/:id
func (h *PaymentHandler) CancelOrder(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	if err := h.svc.CancelOrder(c.Request.Context(), c.Param("id"), userID); err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "cancelled"})
}

// ConfirmOrder — POST /payments/orders/:order_id/confirm
func (h *PaymentHandler) ConfirmOrder(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}

	var req struct {
		Channel       string `json:"channel" binding:"required"`
		ExternalTxnID string `json:"external_txn_id" binding:"required"`
		// ExpiresAt is the subscription expiry the frontend computed from
		// plan.interval_days + business rules (rollover, grace, trial).
		// yunhou-users MUST NOT compute this server-side — see the
		// WebhookEvent.SubExpiresAt doc comment in service/payment.go.
		// nil = frontend declined to set one (free plan / explicit no-end).
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
		// Amount and Currency are NOT accepted from the caller — the order
		// row is the authoritative source. A caller-supplied amount lets a
		// user claim they paid $100 on a $1 order; the webhook will later
		// reconcile but the subscription would already be activated.
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	in := service.ConfirmInput{
		OrderID:       c.Param("order_id"),
		UserID:        userID,
		Channel:       req.Channel,
		ExternalTxnID: req.ExternalTxnID,
		ExpiresAt:     req.ExpiresAt,
	}
	res, err := h.svc.Confirm(c.Request.Context(), in)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res})
}

// ============================================================================
// Payment endpoints
// ============================================================================

// ListPayments — GET /payments
func (h *PaymentHandler) ListPayments(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	list, err := h.svc.ListUserPayments(c.Request.Context(), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

// GetPayment — GET /payments/:id
func (h *PaymentHandler) GetPayment(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	payment, err := h.svc.GetPayment(c.Request.Context(), c.Param("id"), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": payment})
}

// ============================================================================
// Refund endpoints
// ============================================================================

// CreateRefund — POST /refunds
//
// Requires Idempotency-Key header (caller retry → no double-refund).
func (h *PaymentHandler) CreateRefund(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}

	idemKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idemKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing Idempotency-Key header"})
		return
	}
	// Length + charset validation. Without these, a caller could supply
	// an 8KB key (bloat the unique index) or a non-ASCII key (silent
	// truncation/encoding issues across the boundary).
	if len(idemKey) < 8 || len(idemKey) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Idempotency-Key must be 8-128 characters"})
		return
	}
	if !isValidIdempotencyKey(idemKey) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Idempotency-Key must match [A-Za-z0-9_.:-]+"})
		return
	}

	var req struct {
		PaymentID string  `json:"payment_id" binding:"required"`
		Amount    float64 `json:"amount" binding:"required"`
		Reason    *string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	in := service.RefundInput{
		PaymentID:      req.PaymentID,
		UserID:         userID,
		IdempotencyKey: idemKey,
		Amount:         req.Amount,
		Reason:         req.Reason,
	}
	res, err := h.svc.Refund(c.Request.Context(), in)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res.Refund})
}

// GetRefund — GET /refunds/:id
func (h *PaymentHandler) GetRefund(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	refund, err := h.svc.GetRefund(c.Request.Context(), c.Param("id"), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": refund})
}

// ListPaymentRefunds — GET /payments/:id/refunds
func (h *PaymentHandler) ListPaymentRefunds(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	list, err := h.svc.ListPaymentRefunds(c.Request.Context(), c.Param("id"), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

// ============================================================================
// Error mapping
// ============================================================================

// writePaymentError translates service-layer sentinel errors to HTTP status
// codes. Internal errors (anything not on this list) are logged and surfaced
// as a generic 500 — see the responsibility boundary memory.
func writePaymentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrPlanNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan not found"})
	case errors.Is(err, service.ErrPlanInactive):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is inactive"})
	case errors.Is(err, service.ErrUserHasActiveSub):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "user already has an active subscription"})
	case errors.Is(err, service.ErrOrderNotFound), errors.Is(err, service.ErrPaymentNotFound), errors.Is(err, service.ErrRefundNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
	case errors.Is(err, service.ErrOrderNotPending):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "order is not in pending status"})
	case errors.Is(err, service.ErrOrderChannelMismatch):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "order already has a paid payment on a different channel"})
	case errors.Is(err, service.ErrOrderAlreadyTerminal):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "order is in a non-recoverable terminal state"})
	case errors.Is(err, service.ErrPaymentNotPaid):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "payment is not in paid status"})
	case errors.Is(err, service.ErrRefundAmountInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "refund amount must be > 0 and <= payment amount"})
	case errors.Is(err, service.ErrRefundSumExceedsPayment):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "sum of refunds would exceed payment amount"})
	case errors.Is(err, service.ErrRefundChannelFailed):
		// 502 — upstream channel rejected the refund request
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "channel refund API call failed"})
	case errors.Is(err, service.ErrMissingIdempotencyKey):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing Idempotency-Key header"})
	case errors.Is(err, service.ErrInvalidChannel):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid channel"})
	case errors.Is(err, service.ErrWechatPayNotConfigured):
		// 400 — the deployment chose not to wire a WeChat Pay client, so
		// the channel can't be served. Not a 404 (the route exists; the
		// channel on this deployment just isn't enabled) and not a 503
		// (we're not temporarily down — we never wire this channel).
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "wechat pay not configured on this deployment"})
	case errors.Is(err, wechat.ErrWechatMisconfigured):
		// 500 — the deployment is in real-mode (MockMode=false) but the
		// AppID/Signer/MchID is unset. Operator-fixable: re-set the env
		// vars and redeploy. Not 4xx (the request is well-formed) and not
		// 502 (this isn't an upstream failure).
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "wechat pay client misconfigured (check WECHAT_PAY_APP_ID / WECHAT_PAY_MCH_ID / signing key path)"})
	case errors.Is(err, wechat.ErrWeChatUnifiedOrderRejected):
		// 400 — WeChat returned 4xx (or empty code_url). Terminal: retrying
		// the same payload will fail again. Surface a 4xx so the caller
		// knows to fix the request, not retry.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "wechat pay rejected the order (4xx); check request and try a new order"})
	case errors.Is(err, wechat.ErrWeChatUpstream):
		// 502 — WeChat returned 5xx. Transient: caller may retry with the
		// same OutTradeNo after a backoff. This package does not retry
		// itself.
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "wechat pay upstream 5xx; retry after backoff"})
	case errors.Is(err, wechat.ErrWeChatNetwork):
		// 502 — outbound HTTP failure (timeout, DNS, ctx cancellation).
		// Transient: caller may retry.
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "wechat pay network error; retry after backoff"})
	default:
		log.Printf("payment handler error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal error"})
	}
}
