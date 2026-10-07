package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
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
		PlanID      string          `json:"plan_id" binding:"required"`
		Channel     string          `json:"channel" binding:"required"`
		Attribution json.RawMessage `json:"attribution"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	order, err := h.svc.CreateOrder(c.Request.Context(), userID, req.PlanID, req.Channel, req.Attribution)
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

// ListOrders — GET /payments/orders
func (h *PaymentHandler) ListOrders(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	list, err := h.svc.ListUserOrders(c.Request.Context(), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
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

// CancelChannelSubscription — POST /payments/subscription/cancel
//
// Paddle-managed subscriptions only: cancels the channel-side subscription
// with Paddle's default effective_from (next_billing_period) — the buyer
// keeps access until the paid period ends, no more charges. The local
// status flips when Paddle's subscription.canceled webhook arrives.
func (h *PaymentHandler) CancelChannelSubscription(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	sub, err := h.svc.CancelChannelSubscription(c.Request.Context(), userID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"subscription_id": sub.ID,
		"plan_id":         sub.PlanID,
		"expires_at":      sub.ExpiresAt,
		"message":         "subscription will cancel at the end of the current billing period",
	}})
}

// UpgradeChannelSubscription — POST /payments/subscription/upgrade
//
// Paddle-managed subscriptions only: monthly → yearly via Paddle
// subscription update + proration (charged the difference immediately).
func (h *PaymentHandler) UpgradeChannelSubscription(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	var req struct {
		PlanID string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}
	res, err := h.svc.UpgradeChannelSubscription(c.Request.Context(), userID, req.PlanID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"subscription_id": res.SubscriptionID,
		"from_plan_id":    res.FromPlanID,
		"to_plan_id":      res.ToPlanID,
		"next_billed_at":  res.NextBilledAt,
	}})
}

// CancelSubscriptionByID — POST /user/subscriptions/:id/cancel
//
// M2 contract: cancels the subscription's channel auto-renew (Paddle only
// this milestone) with effective_from=next_billing_period — the buyer keeps
// access until the paid period ends. Local status/expires_at stay
// untouched; only auto_renew flips to false.
func (h *PaymentHandler) CancelSubscriptionByID(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	// The body is optional; omitted/empty effective_from defaults to
	// next_billing_period, and any other value is a 400.
	var req struct {
		EffectiveFrom string `json:"effective_from"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
			return
		}
	}
	switch req.EffectiveFrom {
	case "", "next_billing_period":
		req.EffectiveFrom = "next_billing_period"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	sub, err := h.svc.CancelSubscriptionByID(c.Request.Context(), userID, c.Param("id"), req.EffectiveFrom)
	if err != nil {
		writeSubSelfServiceError(c, err, "cancel")
		return
	}
	writeSubSelfServiceOK(c, sub)
}

// ChangeSubscriptionPlanByID — POST /user/subscriptions/:id/change-plan
//
// M2 contract: moves the subscription to a longer-cycle plan via Paddle
// subscription update with proration (charged the difference immediately).
// Upgrades only; downgrades stay manual (cancel + re-purchase).
func (h *PaymentHandler) ChangeSubscriptionPlanByID(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	var req struct {
		PlanID string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	sub, err := h.svc.ChangePlanByID(c.Request.Context(), userID, c.Param("id"), req.PlanID)
	if err != nil {
		writeSubSelfServiceError(c, err, "change")
		return
	}
	writeSubSelfServiceOK(c, sub)
}

// writeSubSelfServiceOK emits the contract's shared subscription object
// shape for both self-management endpoints.
func writeSubSelfServiceOK(c *gin.Context, sub *model.Subscription) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"id":         sub.ID,
		"plan_id":    sub.PlanID,
		"status":     sub.Status,
		"auto_renew": sub.AutoRenew,
		"expires_at": sub.ExpiresAt,
	}})
}

// writeSubSelfServiceError maps the M2 contract's error rows. action is
// "cancel" or "change" — the no-auto-renew 409 message is endpoint-specific.
func writeSubSelfServiceError(c *gin.Context, err error, action string) {
	switch {
	case errors.Is(err, service.ErrSubscriptionNotFound):
		// Same response whether the subscription belongs to someone else or
		// doesn't exist, so callers can't enumerate subscription IDs.
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "subscription not found"})
	case errors.Is(err, service.ErrSubscriptionAlreadyEnded):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "subscription already ended"})
	case errors.Is(err, service.ErrSubscriptionNoAutoRenew):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "subscription has no auto-renew to " + action})
	case errors.Is(err, service.ErrSubscriptionNotChannelManaged):
		// PayPal-managed sub: the symmetric endpoint is P1 (no frontend
		// entry); same 409 semantics as the legacy channel routes.
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "active subscription is not managed by an auto-renewing channel"})
	case errors.Is(err, service.ErrSamePlanChange):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "already on plan"})
	case errors.Is(err, service.ErrPlanDowngradeNotSupported):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan downgrade not supported"})
	case errors.Is(err, service.ErrPlanNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan not found"})
	case errors.Is(err, service.ErrPlanInactive):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is inactive"})
	case errors.Is(err, service.ErrPaddlePriceNotConfigured):
		// Config gap (PADDLE_PRICES_JSON missing the plan), not a user
		// error — log server-side, surface the provider-unavailable 502.
		log.Printf("change-plan blocked: paddle price not configured: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "payment provider unavailable"})
	case errors.Is(err, service.ErrChannelUnavailable):
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "payment provider unavailable"})
	case errors.Is(err, service.ErrPaddleNotConfigured):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "paddle not configured on this deployment"})
	default:
		log.Printf("subscription self-service error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal error"})
	}
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
		// ExpiresAt is accepted for API compatibility but IGNORED — a
		// caller-supplied subscription expiry is untrusted (a caller could
		// extend their own subscription past what the plan grants). The
		// service derives expires_at from the plan at activation.
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
//
// 鉴权(安全审计 2026-10):本端点是资金出账操作——service 层只做金额上限
// 校验、部分退款不回收权益,任意用户 JWT 直调在 v2 接入真实渠道退款客户
// 端后即为套现漏洞。因此 handler 层硬要求 InternalAppAuth 认证的 app 上
// 下文(ContextApp),无 app 上下文一律 403,不设 JWT 回退——防御纵深:
// 即使未来路由被误挂回 JWT 组,用户 JWT 也无法通过此 handler。退款请求
// 走 RefundInput.InternalApp 路径(跳过 ownership 校验,userID 由
// service 层按订单归属回填)。
func (h *PaymentHandler) CreateRefund(c *gin.Context) {
	if app := callerApp(c); app == nil {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "internal apps only"})
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
	// payment_id 必须是合法 uuid——非法值（含空串）直接 400，不能放到
	// service 层让 pq 22P02 以 500 形态漏出（pr-ci 回归反馈）。
	if _, err := uuid.Parse(req.PaymentID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid payment_id"})
		return
	}

	in := service.RefundInput{
		PaymentID:      req.PaymentID,
		InternalApp:    true,
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
	case errors.Is(err, service.ErrPlanNotAcceptingNew):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan is not accepting new subscriptions"})
	case errors.Is(err, service.ErrPlanCurrencyMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan currency does not match order currency"})
	case errors.Is(err, service.ErrInvalidAttribution):
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid attribution"})
	case errors.Is(err, service.ErrUserHasActiveSub):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "user already has an active subscription"})
	case errors.Is(err, service.ErrUserHasPendingOrder):
		// 未完成的 checkout（用户尚未付款）——与 active 订阅的 409 区分
		// 文案，避免 "already has an active subscription" 让用户误以为
		// 无法订阅（2026-10-03 intl-prod 真实用户被误导）。
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "you have an unfinished checkout in progress — it expires within 30 minutes, then you can retry"})
	case errors.Is(err, service.ErrPlanDowngrade):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "downgrade to a shorter billing cycle is not allowed with an active subscription"})
	case errors.Is(err, service.ErrPlanNotPurchasable):
		// 400 — the plan has no payment/benefit configuration (migration 029);
		// it is a draft, not purchasable (设计 §4.3).
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is not purchasable: no payment configuration"})
	case errors.Is(err, service.ErrPlanUpgradeNotConfigured):
		// 409 — cross-tier change for a coding-plan subscription requires an
		// explicitly configured upgrade rule (设计 §4.2).
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "cross-tier upgrade is not configured for this plan"})
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
	case errors.Is(err, service.ErrConfirmVerificationUnavailable):
		// 400 — the channel has no wired server-side query client, so a
		// caller-initiated confirm can never be trusted. Terminal: the
		// signature-verified channel webhook settles the order instead.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "payment confirmation is unavailable for this channel; the order will be settled by the channel webhook"})
	case errors.Is(err, service.ErrConfirmNotVerified):
		// 400 — the channel's server-side query does not show a settled
		// payment matching this order. Terminal for this claim: retrying
		// fails again until the channel actually settles the order.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "payment not confirmed by the channel"})
	case errors.Is(err, service.ErrWechatPayNotConfigured):
		// 400 — the deployment chose not to wire a WeChat Pay client, so
		// the channel can't be served. Not a 404 (the route exists; the
		// channel on this deployment just isn't enabled) and not a 503
		// (we're not temporarily down — we never wire this channel).
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "wechat pay not configured on this deployment"})
	case errors.Is(err, service.ErrPaddleNotConfigured):
		// 400 — same contract as ErrWechatPayNotConfigured: the route
		// exists, this deployment just doesn't wire the paddle channel.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "paddle not configured on this deployment"})
	case errors.Is(err, service.ErrPaddlePriceNotConfigured):
		// 400 — operator error: the plan has no entry in
		// PADDLE_PRICES_JSON. Terminal until the config is fixed.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "paddle price not configured for this plan"})
	case errors.Is(err, service.ErrSubscriptionNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "no active subscription"})
	case errors.Is(err, service.ErrSubscriptionNotChannelManaged):
		// 409 — the active subscription is local/WeChat/PayPal-managed;
		// its lifecycle lives elsewhere (DELETE /subscriptions/:id, the
		// PayPal dashboard).
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "active subscription is not managed by an auto-renewing channel"})
	case errors.Is(err, service.ErrSamePlanChange):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "subscription is already on this plan"})
	case errors.Is(err, service.ErrPlanChangeNotUpgrade):
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan change is not an upgrade; downgrade stays manual (cancel + re-purchase)"})
	case errors.Is(err, wechat.ErrWechatMisconfigured):
		// 500 — the deployment is in real-mode (MockMode=false) but the
		// AppID/Signer/MchID is unset. Operator-fixable and logged with
		// the full detail server-side; the client only gets a generic
		// message — env var names and key paths are config internals and
		// must not leak to callers.
		log.Printf("wechat pay client misconfigured: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "payment channel unavailable"})
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
