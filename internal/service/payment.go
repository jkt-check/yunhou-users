package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// PaymentService implements the v1 payment data flow primitives:
// order creation, frontend confirm, refund creation, channel webhook reception.
// See docs/plans/2026-06-16-user-system-design.md and
// docs/plans/2026-06-23-payment-webhook-mechanism.md.
//
// Scope: yunhou-users is a primitive operations layer. Business policy
// (refund windows, eligibility, approval flows, who-can-refund) is composed
// by the caller — see the responsibility boundary memory.
type PaymentService struct {
	db          *sqlx.DB
	orderRepo   repo.OrderRepo
	paymentRepo repo.PaymentRepo
	refundRepo  repo.RefundRepo
	subRepo     repo.SubscriptionRepo
	planRepo    repo.PlanRepo
	userRepo    repo.UserRepo
	webhookRepo repo.WebhookEventRepo
	auditRepo   repo.AuditLogRepo

	// refundAPI is the channel-side refund caller. Production wires
	// real Stripe/WeChat/Alipay HTTP clients here; tests inject stubs.
	refundAPI RefundAPI

	// orderExpiry drives the order's default expires_at. Service layer
	// sets this on INSERT (SQL DEFAULT is also 30 min; setting explicitly
	// makes it configurable without re-migrating).
	orderExpiry time.Duration
}

// RefundAPI is the channel-side refund call. The service is the caller;
// the channel client is injected so production swaps in real HTTP and
// tests swap in a stub.
type RefundAPI interface {
	// Refund issues a refund on the channel and returns the channel's
	// refund ID (Stripe `re.id`, WeChat refund_id, Alipay trade_no for
	// the refund). idempotencyKey is forwarded to the channel (Stripe
	// supports this header; others ignore).
	Refund(ctx context.Context, channel, externalTxnID string, amount float64, idempotencyKey string) (externalRefundID string, err error)
}

func NewPaymentService(
	db *sqlx.DB,
	orderRepo repo.OrderRepo,
	paymentRepo repo.PaymentRepo,
	refundRepo repo.RefundRepo,
	subRepo repo.SubscriptionRepo,
	planRepo repo.PlanRepo,
	userRepo repo.UserRepo,
	webhookRepo repo.WebhookEventRepo,
	auditRepo repo.AuditLogRepo,
	refundAPI RefundAPI,
	orderExpiry time.Duration,
) *PaymentService {
	if orderExpiry == 0 {
		orderExpiry = 30 * time.Minute
	}
	return &PaymentService{
		db:          db,
		orderRepo:   orderRepo,
		paymentRepo: paymentRepo,
		refundRepo:  refundRepo,
		subRepo:     subRepo,
		planRepo:    planRepo,
		userRepo:    userRepo,
		webhookRepo: webhookRepo,
		auditRepo:   auditRepo,
		refundAPI:   refundAPI,
		orderExpiry: orderExpiry,
	}
}

// ============================================================================
// Order lifecycle
// ============================================================================

// CreateOrder mints an order row for a paid plan. The amount is a snapshot
// of plan.price at creation time — plan price changes don't retroactively
// affect in-flight orders.
func (s *PaymentService) CreateOrder(ctx context.Context, userID, planID string) (*model.Order, error) {
	plan, err := s.planRepo.FindByID(ctx, planID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPlanNotFound
		}
		return nil, fmt.Errorf("find plan: %w", err)
	}
	if !plan.IsActive {
		return nil, ErrPlanInactive
	}

	// Reject if the user already has an active subscription — buying again
	// should go through the upgrade flow (or cancel first). This keeps the
	// single-active-sub invariant clean at the API layer.
	if _, err := s.subRepo.FindActiveByUserID(ctx, userID); err == nil {
		return nil, ErrUserHasActiveSub
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check active sub: %w", err)
	}

	order := &model.Order{
		ID:        GenerateUUID(),
		UserID:    userID,
		PlanID:    planID,
		Amount:    plan.Price,
		Currency:  "CNY",
		Status:    "pending",
		ExpiresAt: time.Now().Add(s.orderExpiry),
	}
	if err := s.orderRepo.Create(ctx, order); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	return order, nil
}

// CancelOrder transitions a pending order to cancelled. Returns ErrOrderNotPending
// if the order is in any other state (already paid / failed / refunded /
// cancelled / expired — terminal or recoverable states don't accept cancel).
func (s *PaymentService) CancelOrder(ctx context.Context, orderID, userID string) error {
	ok, err := s.orderRepo.CancelPending(ctx, orderID, userID)
	if err != nil {
		return fmt.Errorf("cancel pending: %w", err)
	}
	if !ok {
		// Distinguish not-found from wrong-status so the handler can
		// return 404 vs 409. We re-read to disambiguate.
		o, ferr := s.orderRepo.FindByID(ctx, orderID)
		if ferr != nil {
			if errors.Is(ferr, sql.ErrNoRows) {
				return ErrOrderNotFound
			}
			return fmt.Errorf("re-read order: %w", ferr)
		}
		if o.UserID != userID {
			return ErrOrderNotFound // hide existence from non-owner
		}
		return ErrOrderNotPending
	}
	return nil
}

// ============================================================================
// Reads (with ownership check)
// ============================================================================

// GetOrder returns an order by ID, or ErrOrderNotFound if missing or
// not owned by the caller. Internal-app callers (via SetOrderInternal)
// bypass the ownership check.
func (s *PaymentService) GetOrder(ctx context.Context, orderID, userID string) (*model.Order, error) {
	o, err := s.orderRepo.FindByID(ctx, orderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("find order: %w", err)
	}
	if o.UserID != userID {
		return nil, ErrOrderNotFound // hide existence from non-owner
	}
	return o, nil
}

// ListUserPayments returns payments for orders owned by userID.
func (s *PaymentService) ListUserPayments(ctx context.Context, userID string) ([]model.Payment, error) {
	return s.paymentRepo.ListByUserID(ctx, userID)
}

// GetPayment returns a payment by ID, or ErrPaymentNotFound if missing or
// not owned by the caller.
func (s *PaymentService) GetPayment(ctx context.Context, paymentID, userID string) (*model.Payment, error) {
	p, err := s.paymentRepo.FindByID(ctx, paymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPaymentNotFound
		}
		return nil, fmt.Errorf("find payment: %w", err)
	}
	o, oerr := s.orderRepo.FindByID(ctx, p.OrderID)
	if oerr != nil {
		return nil, fmt.Errorf("find order: %w", oerr)
	}
	if o.UserID != userID {
		return nil, ErrPaymentNotFound
	}
	return p, nil
}

// ListPaymentRefunds returns refunds for a payment, with ownership check.
func (s *PaymentService) ListPaymentRefunds(ctx context.Context, paymentID, userID string) ([]model.Refund, error) {
	p, err := s.GetPayment(ctx, paymentID, userID)
	if err != nil {
		return nil, err
	}
	return s.refundRepo.ListByPaymentID(ctx, p.ID)
}

// GetRefund returns a refund by ID, with ownership check via payment → order.
func (s *PaymentService) GetRefund(ctx context.Context, refundID, userID string) (*model.Refund, error) {
	r, err := s.refundRepo.FindByID(ctx, refundID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRefundNotFound
		}
		return nil, fmt.Errorf("find refund: %w", err)
	}
	p, perr := s.paymentRepo.FindByID(ctx, r.PaymentID)
	if perr != nil {
		return nil, fmt.Errorf("find payment: %w", perr)
	}
	o, oerr := s.orderRepo.FindByID(ctx, p.OrderID)
	if oerr != nil {
		return nil, fmt.Errorf("find order: %w", oerr)
	}
	if o.UserID != userID {
		return nil, ErrRefundNotFound
	}
	return r, nil
}

// ============================================================================
// Confirm (frontend SDK callback fast-track)
// ============================================================================

// ConfirmInput is the request body for POST /payments/orders/:order_id/confirm.
type ConfirmInput struct {
	OrderID       string
	UserID        string
	Channel       string
	ExternalTxnID string
	Amount        *float64   // optional; defaults to order.amount
	Currency      *string    // optional; defaults to order.currency
	ExpiresAt     *time.Time // optional; subscription expiry (nil = never expires per plan defaults)
}

// ConfirmResult is the response from Confirm.
type ConfirmResult struct {
	PaymentID             string
	OrderID               string
	Status                string
	ActivatedSubscription bool
	WasLatePayment        bool // true if order was expired and we honored
}

func (s *PaymentService) Confirm(ctx context.Context, in ConfirmInput) (*ConfirmResult, error) {
	if err := validateChannel(in.Channel); err != nil {
		return nil, err
	}

	order, err := s.orderRepo.FindByID(ctx, in.OrderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("find order: %w", err)
	}
	if order.UserID != in.UserID {
		return nil, ErrOrderNotFound // hide existence from non-owner
	}

	// Terminal-non-recoverable states: failed / refunded.
	// expired / cancelled are recoverable per §8 honor-payment policy.
	switch order.Status {
	case "failed", "refunded":
		return nil, ErrOrderAlreadyTerminal
	}

	amount := order.Amount
	if in.Amount != nil {
		amount = *in.Amount
	}
	currency := order.Currency
	if in.Currency != nil {
		currency = *in.Currency
	}

	// Channel mismatch pre-check (design doc confirm endpoint contract).
	// Without this, the INSERT in step 1 would surface as a 500 from the
	// partial unique index; we want a 409.
	if existing, perr := s.paymentRepo.FindPaidByOrderID(ctx, order.ID); perr == nil && existing != nil {
		if existing.Channel != in.Channel {
			return nil, ErrOrderChannelMismatch
		}
		// Same channel → fall through; the InsertPaidOnConflictDoNothing
		// will dedupe and we'll re-apply the state transition idempotently.
	}

	now := time.Now()
	rawPayload, _ := json.Marshal(map[string]any{
		"source": "frontend_confirm",
		"order_id": order.ID,
	})
	p := &model.Payment{
		ID:            GenerateUUID(),
		OrderID:       order.ID,
		Channel:       in.Channel,
		ExternalTxnID: in.ExternalTxnID,
		Amount:        amount,
		Currency:      currency,
		Status:        "paid",
		PaidAt:        &now,
		RawPayload:    rawPayload,
	}

	// Activate subscription + update order + handle late-payment honor
	// in one transaction so partial failures don't leave inconsistent state.
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	paymentID, inserted, err := insertPaymentOnTx(ctx, tx, p)
	if err != nil {
		return nil, fmt.Errorf("insert payment: %w", err)
	}
	if !inserted {
		// (channel, external_txn_id) dedupe hit — the row already exists.
		// Re-read it; if it's paid we're done (idempotent). If it's
		// failed, refuse (the channel says this attempt failed even
		// though the frontend thinks it succeeded).
		existing, ferr := s.paymentRepo.FindByChannelTxnID(ctx, in.Channel, in.ExternalTxnID)
		if ferr != nil {
			return nil, fmt.Errorf("re-read existing payment: %w", ferr)
		}
		if existing.Status == "failed" {
			return nil, ErrOrderAlreadyTerminal
		}
		// If the existing row is `paid`, this is a confirm retry — proceed
		// to ensure sub activation + order update are idempotent.
		paymentID = existing.ID
	}

	// Activate subscription (UPSERT single-row, webhook doc §5.3).
	activated, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, in.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("activate sub: %w", err)
	}

	// Update order to paid (covers pending/expired/cancelled per §5.3).
	wasLate := order.Status == "expired"
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'paid', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'expired', 'cancelled')
	`, order.ID)
	if err != nil {
		return nil, fmt.Errorf("update order: %w", err)
	}
	n, _ := res.RowsAffected()
	orderUpdated := n > 0

	if wasLate && orderUpdated {
		// Audit log for late-payment-honored event.
		if err := writeAuditOnTx(ctx, tx, "service", "late_payment_post_expiry",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"payment", "expiry", "honored"},
			map[string]any{"order_id": order.ID, "payment_id": paymentID},
		); err != nil {
			return nil, fmt.Errorf("write audit log: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit confirm tx: %w", err)
	}

	return &ConfirmResult{
		PaymentID:             paymentID,
		OrderID:               order.ID,
		Status:                "paid",
		ActivatedSubscription: activated,
		WasLatePayment:        wasLate && orderUpdated,
	}, nil
}

// ============================================================================
// Refund
// ============================================================================

// RefundInput is the request body for POST /refunds.
type RefundInput struct {
	PaymentID      string
	UserID         string  // empty for internal-app-auth
	InternalApp    bool    // skip ownership check
	IdempotencyKey string  // REQUIRED — header value
	Amount         float64 // > 0, <= payment.amount
	Reason         *string
}

// RefundResult is the response from Refund.
type RefundResult struct {
	Refund *model.Refund
	// Existing is true if this is a retry of a previously seen Idempotency-Key
	// (we did NOT call the channel API again — caller can poll status).
	Existing bool
}

func (s *PaymentService) Refund(ctx context.Context, in RefundInput) (*RefundResult, error) {
	if in.IdempotencyKey == "" {
		return nil, ErrMissingIdempotencyKey
	}
	if in.Amount <= 0 {
		return nil, ErrRefundAmountInvalid
	}

	// Caller-retry gate: same key → same row, no channel call.
	if existing, err := s.refundRepo.FindByIdempotencyKey(ctx, in.IdempotencyKey); err == nil && existing != nil {
		return &RefundResult{Refund: existing, Existing: true}, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check idempotency: %w", err)
	}

	payment, err := s.paymentRepo.FindByID(ctx, in.PaymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPaymentNotFound
		}
		return nil, fmt.Errorf("find payment: %w", err)
	}
	if !in.InternalApp {
		// Ownership check: the payment's user must match caller.
		o, oerr := s.orderRepo.FindByID(ctx, payment.OrderID)
		if oerr != nil {
			return nil, fmt.Errorf("load order for ownership: %w", oerr)
		}
		if o.UserID != in.UserID {
			return nil, ErrPaymentNotFound // hide existence from non-owner
		}
	}
	if payment.Status != "paid" {
		return nil, ErrPaymentNotPaid
	}
	if in.Amount > payment.Amount {
		return nil, ErrRefundAmountInvalid
	}

	// Serialize per-payment to enforce sum invariant. Single tx: lock + validate
	// + call channel + insert refund. The lock is held for the duration of
	// the channel API call — known trade-off, see webhook doc §5.4.
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock the payment row.
	var paymentAmount float64
	if err := tx.GetContext(ctx, &paymentAmount,
		`SELECT amount FROM payments WHERE id = $1 FOR UPDATE`, payment.ID); err != nil {
		return nil, fmt.Errorf("lock payment: %w", err)
	}

	// Sum invariant under lock.
	var currentSum float64
	if err := tx.GetContext(ctx, &currentSum,
		`SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1`, payment.ID); err != nil {
		return nil, fmt.Errorf("sum refunds: %w", err)
	}
	if currentSum+in.Amount > paymentAmount {
		return nil, ErrRefundSumExceedsPayment
	}

	// Call the channel refund API. Failure aborts before INSERT — we did
	// not create the refund row, so no orphan.
	externalRefundID, err := s.refundAPI.Refund(ctx, payment.Channel, payment.ExternalTxnID, in.Amount, in.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefundChannelFailed, err)
	}

	// INSERT pending refund. If another concurrent transaction beat us to
	// the same (channel, external_refund_id), the unique constraint fires;
	// since we just got externalRefundID from the channel API, that shouldn't
	// happen in practice (the channel wouldn't return the same id twice).
	extID := externalRefundID
	refund := &model.Refund{
		ID:               GenerateUUID(),
		PaymentID:        payment.ID,
		Channel:          payment.Channel,
		Amount:           in.Amount,
		Reason:           in.Reason,
		IdempotencyKey:   in.IdempotencyKey,
		ExternalRefundID: &extID,
		Status:           "pending",
	}
	if _, err := tx.NamedExecContext(ctx, `
		INSERT INTO refunds (id, payment_id, channel, amount, reason, idempotency_key, external_refund_id, status)
		VALUES (:id, :payment_id, :channel, :amount, :reason, :idempotency_key, :external_refund_id, :status)
	`, refund); err != nil {
		return nil, fmt.Errorf("insert refund: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refund tx: %w", err)
	}

	// If the refund is full, the channel webhook will flip the payment to
	// `refunded` and we'll cancel the subscription there (webhook doc §7
	// + §5.5). We don't preemptively cancel here — keeping the subscription
	// active until the channel confirms is a smaller blast radius if the
	// channel reverses the refund later.

	return &RefundResult{Refund: refund, Existing: false}, nil
}

// ============================================================================
// Webhook handler — channel → yunhou-users
// ============================================================================

// WebhookEvent is the channel-normalized event passed from the handler
// (which does signature verification and channel-specific parsing) to the
// service. Each channel's handler populates the fields it can extract;
// fields not relevant to the event type are left zero.
type WebhookEvent struct {
	Channel        string
	EventID        string // channel's event ID — Stripe `evt.id`, WeChat `notify_id`, Alipay `notify_id`
	EventType      string // channel's event type string
	RawPayload     json.RawMessage
	TransactionID  string     // channel's transaction ID — maps to payments.external_txn_id
	OrderID        string     // order UUID from channel metadata (Stripe) or out_trade_no (WeChat/Alipay)
	Amount         float64    // settled amount (major currency units, normalized by handler)
	Currency       string     // ISO 4217
	RefundAmount   float64    // for refund events
	ExternalRefundID string   // channel's refund ID
	SubExpiresAt   *time.Time // subscription expiry (from plan.interval_days; nil = never expires)
}

// OnWebhookResult reports what the handler did.
type OnWebhookResult struct {
	DuplicateEvent bool // true if event_id was already seen (handler should ack 200)
	DomainAction   string // "payment_paid" / "payment_failed" / "refund_paid" / "none"
}

// OnWebhook dispatches the (already signature-verified) event to the right
// side effect. See webhook doc §5.5 for refund path and §5.3 for payment path.
func (s *PaymentService) OnWebhook(ctx context.Context, e WebhookEvent) (*OnWebhookResult, error) {
	if err := validateChannel(e.Channel); err != nil {
		return nil, err
	}

	// 1. Event-level dedup (webhook doc §5.1). Insert into webhook_events
	//    first; if dedupe, ack 200 immediately.
	webhookRow := &model.WebhookEvent{
		Channel:    e.Channel,
		EventID:    e.EventID,
		EventType:  e.EventType,
		RawPayload: e.RawPayload,
	}
	eventRowID, inserted, err := s.webhookRepo.InsertOnConflictDoNothing(ctx, webhookRow)
	if err != nil {
		return nil, fmt.Errorf("insert webhook event: %w", err)
	}
	if !inserted {
		return &OnWebhookResult{DuplicateEvent: true}, nil
	}

	var domainAction string

	switch {
	case isPaymentSuccess(e.EventType):
		domainAction = "payment_paid"
		if err := s.onPaymentSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case isPaymentFailed(e.EventType):
		domainAction = "payment_failed"
		if err := s.onPaymentFailed(ctx, e); err != nil {
			return nil, err
		}
	case isRefundEvent(e.EventType):
		domainAction = "refund_paid"
		if err := s.onRefundSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case isDisputeCreated(e.EventType):
		domainAction = "payment_disputed"
		if err := s.onDisputeCreated(ctx, e); err != nil {
			return nil, err
		}
	case isDisputeClosed(e.EventType):
		// v1 only reacts when the merchant wins (clear disputed=true).
		// Loss path is handled via the chargeback's charge.refunded event
		// — see webhook doc §7.
		domainAction = "payment_dispute_closed"
		if err := s.onDisputeClosed(ctx, e); err != nil {
			return nil, err
		}
	default:
		// Unknown / uninteresting event types: log to webhook_events
		// (done above) and ack 200. No domain action.
		domainAction = "none"
	}

	// Mark processed regardless of whether a domain action ran. The
	// webhook_events.processed_at column tracks "we finished with this
	// event", not "we acted on it" (design doc WebhookEvent note).
	if err := s.webhookRepo.MarkProcessed(ctx, eventRowID); err != nil {
		return nil, fmt.Errorf("mark webhook processed: %w", err)
	}

	return &OnWebhookResult{DuplicateEvent: false, DomainAction: domainAction}, nil
}

// onPaymentSucceeded: payment_intent.succeeded (Stripe), TRANSACTION.SUCCESS (WeChat), TRADE_SUCCESS (Alipay).
// Mirrors Confirm but driven by the channel — the cross-table transaction
// must hold event-insert + payment-insert + sub-activate + order-update.
func (s *PaymentService) onPaymentSucceeded(ctx context.Context, e WebhookEvent) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Find the order (handler already extracted orderID from channel metadata).
	var order model.Order
	if err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, e.OrderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Webhook arrived for an order that doesn't exist in our DB.
			// Write audit log + 404 (channel retries per schedule).
			return s.writeAudit(ctx, "service", "webhook_for_unknown_order",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "unknown_order"},
				map[string]any{"channel": e.Channel, "order_id": e.OrderID, "event_id": e.EventID},
			)
		}
		return fmt.Errorf("find order: %w", err)
	}

	// Channel mismatch pre-check (same as Confirm).
	if existing, perr := s.paymentRepo.FindPaidByOrderID(ctx, order.ID); perr == nil && existing != nil {
		if existing.Channel != e.Channel {
			// Log and skip — webhook for a payment on a different channel that already paid.
			return s.writeAudit(ctx, "service", "webhook_channel_mismatch",
				fmt.Sprintf("order:%s", order.ID),
				[]string{"webhook", "channel_mismatch"},
				map[string]any{"order_id": order.ID, "webhook_channel": e.Channel, "existing_channel": existing.Channel},
			)
		}
	}

	now := time.Now()
	p := &model.Payment{
		ID:            GenerateUUID(),
		OrderID:       order.ID,
		Channel:       e.Channel,
		ExternalTxnID: e.TransactionID,
		Amount:        e.Amount,
		Currency:      e.Currency,
		Status:        "paid",
		PaidAt:        &now,
		RawPayload:    e.RawPayload,
	}

	paymentID, inserted, err := insertPaymentOnTx(ctx, tx, p)
	if err != nil {
		return fmt.Errorf("insert payment: %w", err)
	}
	if !inserted {
		// Dedupe hit — payment row already exists. Re-read to know whether
		// it's paid (no-op) or in a state we need to escalate.
		existing, ferr := s.paymentRepo.FindByChannelTxnID(ctx, e.Channel, e.TransactionID)
		if ferr != nil {
			return fmt.Errorf("re-read existing: %w", ferr)
		}
		if existing.Status != "paid" {
			// Defensive: SQL guard will make UPDATE a no-op + audit log
			// if the existing row is `failed`. See webhook doc §5.6.
			return s.writeAudit(ctx, "service", "unexpected_state_transition",
				fmt.Sprintf("payment:%s", existing.ID),
				[]string{"webhook", "defensive_transition"},
				map[string]any{"from": existing.Status, "to": "paid", "event_id": e.EventID},
			)
		}
		paymentID = existing.ID
	}

	if _, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiresAtFromWebhook(e)); err != nil {
		return fmt.Errorf("activate sub: %w", err)
	}

	wasLate := order.Status == "expired"
	res, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'paid', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'expired', 'cancelled')
	`, order.ID)
	if err != nil {
		return fmt.Errorf("update order: %w", err)
	}
	n, _ := res.RowsAffected()
	orderUpdated := n > 0

	if wasLate && orderUpdated {
		if err := writeAuditOnTx(ctx, tx, "service", "late_payment_post_expiry",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"payment", "expiry", "honored", "via_webhook"},
			map[string]any{"order_id": order.ID, "payment_id": paymentID, "channel": e.Channel},
		); err != nil {
			return fmt.Errorf("write audit: %w", err)
		}
	}

	return tx.Commit()
}

// onPaymentFailed: payment_intent.payment_failed / .canceled (Stripe) and
// equivalent failures on WeChat/Alipay. Sets payment → failed; if the order
// was previously paid (rare race with Confirm), also cascades to deactivate
// subscription (webhook doc §7).
func (s *PaymentService) onPaymentFailed(ctx context.Context, e WebhookEvent) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	payment, err := s.findOrInsertPendingOnTx(ctx, tx, e)
	if err != nil {
		return err
	}
	if payment == nil {
		return nil // defensive: no payment row to update
	}

	wasPaid := payment.Status == "paid"
	res, err := tx.ExecContext(ctx, `
		UPDATE payments SET status = 'failed', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'paid')
	`, payment.ID)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// SQL guard: terminal states (failed/refunded) don't transition. No-op.
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}

	// Find the order to flip its status.
	var order model.Order
	if err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, payment.OrderID); err != nil {
		return fmt.Errorf("find order: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE orders SET status = 'failed', updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'paid', 'expired', 'cancelled')
	`, order.ID); err != nil {
		return fmt.Errorf("flip order: %w", err)
	}

	if wasPaid {
		// Deactivate subscription (rare race; web doc §7 cascade).
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET status = 'cancelled', updated_at = now()
			WHERE user_id = $1 AND status = 'active'
		`, order.UserID); err != nil {
			return fmt.Errorf("deactivate sub: %w", err)
		}
		if err := writeAuditOnTx(ctx, tx, "service", "subscription_deactivated_failed_payment",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"payment", "failed", "cascade"},
			map[string]any{"order_id": order.ID, "payment_id": payment.ID, "channel": e.Channel},
		); err != nil {
			return fmt.Errorf("write audit: %w", err)
		}
	}

	return tx.Commit()
}

// onRefundSucceeded: charge.refunded / TRANSACTION.REFUND / TRADE_CLOSED.
// Full refund → payment → refunded, sub → cancelled. Partial refund →
// payment stays paid, no sub change.
func (s *PaymentService) onRefundSucceeded(ctx context.Context, e WebhookEvent) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Find the payment by (channel, transaction_id).
	var payment model.Payment
	if err := tx.GetContext(ctx, &payment, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2 FOR UPDATE
	`, e.Channel, e.TransactionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.writeAudit(ctx, "service", "webhook_refund_unknown_payment",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "unknown_payment"},
				map[string]any{"channel": e.Channel, "transaction_id": e.TransactionID, "event_id": e.EventID},
			)
		}
		return fmt.Errorf("find payment: %w", err)
	}

	// Find or insert the refund row keyed on (channel, external_refund_id).
	// ON CONFLICT DO NOTHING absorbs webhook retries; re-read for the
	// (channel, external_refund_id) → id mapping.
	extID := e.ExternalRefundID
	var refundID string
	err = tx.QueryRowxContext(ctx, `
		INSERT INTO refunds (payment_id, channel, amount, idempotency_key, external_refund_id, status)
		VALUES ($1, $2, $3, $4, $5, 'paid')
		ON CONFLICT (channel, external_refund_id) DO NOTHING
		RETURNING id
	`, payment.ID, e.Channel, e.RefundAmount, "webhook:"+e.EventID, extID).Scan(&refundID)
	switch {
	case err == nil:
		// inserted
	case err.Error() == "sql: no rows in result set":
		// already inserted by a prior webhook delivery — look it up
		if lerr := tx.GetContext(ctx, &refundID, `
			SELECT id FROM refunds WHERE channel = $1 AND external_refund_id = $2
		`, e.Channel, e.ExternalRefundID); lerr != nil {
			return fmt.Errorf("re-read refund: %w", lerr)
		}
	default:
		return fmt.Errorf("insert refund: %w", err)
	}

	// Mark the refund paid (idempotent — if already paid, no-op).
	if _, err := tx.ExecContext(ctx, `
		UPDATE refunds SET status = 'paid', updated_at = now()
		WHERE id = $1 AND status = 'pending'
	`, refundID); err != nil {
		return fmt.Errorf("mark refund paid: %w", err)
	}

	// Full vs partial refund — only the channel's amount tells us.
	if e.RefundAmount+0.0001 >= payment.Amount {
		// Full refund: payment → refunded, sub → cancelled.
		if _, err := tx.ExecContext(ctx, `
			UPDATE payments SET status = 'refunded', updated_at = now()
			WHERE id = $1 AND status = 'paid'
		`, payment.ID); err != nil {
			return fmt.Errorf("mark payment refunded: %w", err)
		}
		var order model.Order
		if err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, payment.OrderID); err != nil {
			return fmt.Errorf("find order: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE orders SET status = 'refunded', updated_at = now()
			WHERE id = $1 AND status = 'paid'
		`, order.ID); err != nil {
			return fmt.Errorf("flip order refunded: %w", err)
		}
		// Deactivate subscription. Only cancel an active sub — already
		// expired/cancelled subs are terminal (don't reopen them).
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET status = 'cancelled', updated_at = now()
			WHERE user_id = $1 AND status = 'active'
		`, order.UserID); err != nil {
			return fmt.Errorf("cancel sub on full refund: %w", err)
		}
		if err := writeAuditOnTx(ctx, tx, "service", "subscription_cancelled_full_refund",
			fmt.Sprintf("payment:%s", payment.ID),
			[]string{"refund", "full", "sub_cancelled"},
			map[string]any{"payment_id": payment.ID, "refund_id": refundID, "channel": e.Channel},
		); err != nil {
			return fmt.Errorf("write audit: %w", err)
		}
	}
	// Partial refund: no domain action beyond marking the refund paid.

	return tx.Commit()
}

func (s *PaymentService) onDisputeCreated(ctx context.Context, e WebhookEvent) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var payment model.Payment
	if err := tx.GetContext(ctx, &payment, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2 FOR UPDATE
	`, e.Channel, e.TransactionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // ignore — no matching payment
		}
		return fmt.Errorf("find payment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE payments SET disputed = true, disputed_at = now(), updated_at = now()
		WHERE id = $1
	`, payment.ID); err != nil {
		return fmt.Errorf("set disputed: %w", err)
	}
	return tx.Commit()
}

// onDisputeClosed: v1 only reacts when the merchant wins. Loss path goes
// through the chargeback's charge.refunded event — webhook doc §7.
func (s *PaymentService) onDisputeClosed(ctx context.Context, e WebhookEvent) error {
	if e.Amount > 0 {
		// Loss path — let the charge.refunded handler take it.
		return nil
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var payment model.Payment
	if err := tx.GetContext(ctx, &payment, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2 FOR UPDATE
	`, e.Channel, e.TransactionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("find payment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE payments SET disputed = false, disputed_at = NULL, updated_at = now()
		WHERE id = $1
	`, payment.ID); err != nil {
		return fmt.Errorf("clear disputed: %w", err)
	}
	return tx.Commit()
}

// ============================================================================
// Internal helpers — transaction-scoped SQL for cross-repo atomicity.
// ============================================================================

// insertPaymentOnTx does the business-level idempotency INSERT inside an
// existing transaction. Returns (paymentID, true) if inserted, (_, false)
// if a row already exists for (channel, external_txn_id).
func insertPaymentOnTx(ctx context.Context, tx *sqlx.Tx, p *model.Payment) (string, bool, error) {
	rawPayload := p.RawPayload
	if rawPayload == nil {
		rawPayload = json.RawMessage(`{}`)
	}
	var paidAt *time.Time = p.PaidAt
	var id string
	err := tx.QueryRowxContext(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (channel, external_txn_id) DO NOTHING
		RETURNING id
	`, p.OrderID, p.Channel, p.ExternalTxnID, p.Amount, p.Currency, p.Status, paidAt, rawPayload).Scan(&id)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

// activateSubscriptionOnTx: the single-row UPSERT from webhook doc §5.3.
// Returns whether activation actually happened (true if the user just got
// a new active sub this call; false if they already had one or we reactivated
// an existing row).
func activateSubscriptionOnTx(ctx context.Context, tx *sqlx.Tx, userID, planID string, expiresAt *time.Time) (bool, error) {
	// Step 1: UPDATE the target row (active first, else most recent).
	_, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET
			plan_id = $1,
			started_at = now(),
			expires_at = $2,
			status = 'active'
		WHERE id = (
			SELECT id FROM subscriptions
			WHERE user_id = $3
			ORDER BY CASE WHEN status = 'active' THEN 0 ELSE 1 END, created_at DESC
			LIMIT 1
		)
	`, planID, expiresAt, userID)
	if err != nil {
		return false, fmt.Errorf("update subscription: %w", err)
	}

	// Check whether the UPDATE hit a row (vs no rows existed at all).
	var count int
	if err := tx.GetContext(ctx, &count,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		return false, fmt.Errorf("count subs: %w", err)
	}

	if count == 0 {
		// Step 2: INSERT a new active row.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at)
			VALUES ($1, $2, $3, 'active', now(), $4)
		`, GenerateUUID(), userID, planID, expiresAt)
		if err != nil {
			return false, fmt.Errorf("insert subscription: %w", err)
		}
		return true, nil
	}
	return false, nil
}

// writeAuditOnTx inserts an audit_log row within an existing transaction.
func writeAuditOnTx(ctx context.Context, tx *sqlx.Tx, actor, action, target string, tags []string, ctxData map[string]any) error {
	data, _ := json.Marshal(ctxData)
	_, err := tx.NamedExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target, tags, context)
		VALUES (:actor, :action, :target, :tags, :context)
	`, &model.AuditLog{
		Actor:   actor,
		Action:  action,
		Target:  &target,
		Tags:    tags,
		Context: data,
	})
	return err
}

// writeAudit is the non-transactional variant. Used for events that need to
// be recorded but don't fit inside a larger tx (e.g. unknown-order webhooks
// where the tx has already rolled back).
func (s *PaymentService) writeAudit(ctx context.Context, actor, action, target string, tags []string, ctxData map[string]any) error {
	return s.auditRepo.Insert(ctx, &model.AuditLog{
		Actor:   actor,
		Action:  action,
		Target:  &target,
		Tags:    tags,
		Context: mustJSON(ctxData),
	})
}

// findOrInsertPendingOnTx: helper for onPaymentFailed — the payment row
// may not exist yet if `.payment_failed` arrives before any INSERT (rare but
// possible). We insert a `pending` row first, then mark it failed.
func (s *PaymentService) findOrInsertPendingOnTx(ctx context.Context, tx *sqlx.Tx, e WebhookEvent) (*model.Payment, error) {
	var p model.Payment
	err := tx.GetContext(ctx, &p, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2 FOR UPDATE
	`, e.Channel, e.TransactionID)
	if err == nil {
		return &p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find payment: %w", err)
	}
	// No row yet — find order and insert a pending payment.
	var order model.Order
	if err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, e.OrderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // no order either; nothing to do
		}
		return nil, fmt.Errorf("find order: %w", err)
	}
	now := time.Now()
	p = model.Payment{
		ID:            GenerateUUID(),
		OrderID:       order.ID,
		Channel:       e.Channel,
		ExternalTxnID: e.TransactionID,
		Amount:        e.Amount,
		Currency:      e.Currency,
		Status:        "pending",
		RawPayload:    e.RawPayload,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if _, err := tx.NamedExecContext(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES (:order_id, :channel, :external_txn_id, :amount, :currency, :status, :raw_payload)
	`, &p); err != nil {
		return nil, fmt.Errorf("insert pending payment: %w", err)
	}
	return &p, nil
}

// ============================================================================
// Pure helpers
// ============================================================================

func validateChannel(channel string) error {
	switch channel {
	case "stripe", "wechat_pay", "alipay":
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInvalidChannel, channel)
	}
}

func isPaymentSuccess(eventType string) bool {
	switch eventType {
	case "payment_intent.succeeded", "TRANSACTION.SUCCESS",
		"TRADE_SUCCESS", "trade_status_sync":
		return true
	}
	return false
}

func isPaymentFailed(eventType string) bool {
	switch eventType {
	case "payment_intent.payment_failed", "payment_intent.canceled",
		"TRANSACTION.PAY_FAILED", "TRANSACTION.REVOKED":
		return true
	}
	return false
}

func isRefundEvent(eventType string) bool {
	switch eventType {
	case "charge.refunded", "TRANSACTION.REFUND",
		"TRADE_CLOSED", "trade_closed":
		return true
	}
	return false
}

func isDisputeCreated(eventType string) bool {
	return eventType == "charge.dispute.created"
}

func isDisputeClosed(eventType string) bool {
	return eventType == "charge.dispute.closed"
}

// subExpiresAtFromWebhook forwards the webhook payload's expires_at to the
// subscription activation. nil = never expires (free plan / explicit no-end).
func subExpiresAtFromWebhook(e WebhookEvent) *time.Time {
	return e.SubExpiresAt
}

func mustJSON(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}