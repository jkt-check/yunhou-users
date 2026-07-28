package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// wechatClient is the surface PaymentService needs from the wechat.Client.
// Defined here so tests can replace the concrete billing client with a
// hand-rolled stub. AppID() echoes into provider_intent.appid; MchID()
// echoes into provider_intent.mchid; both are required for the BFF to
// render the right QR + audit-log the upstream merchant.
type wechatClient interface {
	IsMockMode() bool
	MchID() string
	AppID() string
	UnifiedOrder(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error)
	QueryOrder(ctx context.Context, outTradeNo string) (*wechat.OrderQueryResult, error)
}

// ErrWechatPayNotConfigured is returned by CreateOrder when channel="wechat_pay"
// but no wechat client is wired (deployment chose not to accept WeChat Pay).
// Without this, the user would get a 201 pending order with no code_url and
// no way to pay — we surface a 4xx instead (mapped in handler/payment.go).
var ErrWechatPayNotConfigured = errors.New("wechat pay not configured on this deployment")

// ErrPlanMissingForExpiry is returned by resolveSubExpiry when the plan row
// was deleted between order creation and webhook/confirm arrival. Callers
// audit-log this and fall back to the existing "NULL = never expires" branch.
var ErrPlanMissingForExpiry = errors.New("plan missing for sub-expiry fallback")

// ErrDowngradeActivationBlocked is returned by resolveSubExpiry when a
// paid order would REPLACE an unexpired active subscription with a
// shorter-interval plan (e.g. a stale monthly QR paid after the user
// upgraded to yearly). The payment is still honored — the order goes
// paid and ops refunds manually — but the subscription is left
// untouched and the call sites audit-log "downgrade_activation_blocked".
var ErrDowngradeActivationBlocked = errors.New("activation blocked: would downgrade an active longer-cycle subscription")

// channelRequiredCurrency describes the settlement currency required by the
// channels that only support one currency in this service. Plans remain the
// source of truth for the order's persisted currency. Stripe and Alipay are
// intentionally omitted because they can settle the plan's configured
// currency.
var channelRequiredCurrency = map[string]string{
	"wechat_pay": "CNY",
	"paypal":     "USD",
}

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

	// dbBeginTx is the indirection used by the webhook handlers to
	// start a transaction. The default delegates to s.db.BeginTxx;
	// tests override it to inject a fake transaction that returns
	// configured errors, driving otherwise-unreachable
	// `return fmt.Errorf("...: %w", err)` branches in onPaymentSucceeded,
	// onPaymentFailed, onRefundSucceeded, and onDisputeCreated.
	dbBeginTx func(ctx context.Context) (dbTx, error)

	// refundAPI is the channel-side refund caller. Production wires
	// real Stripe/WeChat/Alipay HTTP clients here; tests inject stubs.
	refundAPI RefundAPI

	// wechat is optional; nil deployments can still create orders for
	// non-WeChat channels.
	wechat wechatClient

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
	wechat wechatClient,
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
		wechat:      wechat,
		orderExpiry: orderExpiry,
		dbBeginTx: func(ctx context.Context) (dbTx, error) {
			tx, err := db.BeginTxx(ctx, nil)
			if err != nil {
				return nil, err
			}
			return &sqlxTx{Tx: tx}, nil
		},
	}
}

// dbTx is a minimal interface for the transaction operations the
// webhook handlers use. Production code wraps *sqlx.Tx; tests wrap
// a custom in-memory struct that returns configured errors. The
// surface is the union of methods called by the four webhook
// handlers and the three helper functions (insertPaymentOnTx,
// activateSubscriptionOnTx, writeAuditOnTx).
type dbTx interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryRowxContext(ctx context.Context, query string, args ...interface{}) *sqlx.Row
	// QueryRowID runs a single-row query that returns a single
	// string column (e.g. INSERT … RETURNING id) and returns the
	// column value. The interface wraps the QueryRowxContext + Scan
	// dance so production code (which has a real *sqlx.Tx) and
	// tests (which have a fake tx returning a pre-configured string)
	// both satisfy the contract without leaking *sqlx.Row across
	// the boundary.
	QueryRowID(ctx context.Context, query string, args ...interface{}) (string, error)
	NamedExecContext(ctx context.Context, query string, arg interface{}) (sql.Result, error)
	Commit() error
	Rollback() error
}

// sqlxTx wraps *sqlx.Tx to implement the dbTx interface. The
// QueryRowID method does the QueryRowxContext + Scan and returns
// the first column as a string.
type sqlxTx struct{ *sqlx.Tx }

func (s *sqlxTx) QueryRowID(ctx context.Context, query string, args ...interface{}) (string, error) {
	var id string
	if err := s.QueryRowxContext(ctx, query, args...).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// rawSQLXTx returns the underlying transaction for production dbTx values.
// Test-only fake transactions intentionally return nil; those tests exercise
// error paths before a tx-bound fallback lookup is needed.
func rawSQLXTx(tx dbTx) *sqlx.Tx {
	if wrapped, ok := tx.(*sqlxTx); ok {
		return wrapped.Tx
	}
	return nil
}

// txLookupPlan reads a plan by id, sharing the surrounding tx's connection
// when one is in flight. The nil-tx branch is exercised only by unit tests
// that stub dbTx; production always carries a real *sqlx.Tx.
func (s *PaymentService) txLookupPlan(ctx context.Context, tx *sqlx.Tx, planID string) (*model.Plan, error) {
	if tx != nil {
		return s.planRepo.FindByIDForShareTx(ctx, tx, planID)
	}
	return s.planRepo.FindByID(ctx, planID)
}

// txLookupActiveSubscription reads the user's current active sub, sharing
// the surrounding tx's connection when one is in flight. Returns nil/nil
// when no active sub exists (sql.ErrNoRows is swallowed at the call site
// for retry-preservation lookups).
func (s *PaymentService) txLookupActiveSubscription(ctx context.Context, tx *sqlx.Tx, userID string) (*model.Subscription, error) {
	if tx != nil {
		return s.subRepo.FindActiveByUserIDTx(ctx, tx, userID)
	}
	return s.subRepo.FindActiveByUserID(ctx, userID)
}

// txLookupPaymentByChannelTxnID reads a payment by (channel, externalTxnID),
// sharing the surrounding tx's connection when one is in flight.
func (s *PaymentService) txLookupPaymentByChannelTxnID(ctx context.Context, tx *sqlx.Tx, channel, externalTxnID string) (*model.Payment, error) {
	if tx != nil {
		return s.paymentRepo.FindByChannelTxnIDTx(ctx, tx, channel, externalTxnID)
	}
	return s.paymentRepo.FindByChannelTxnID(ctx, channel, externalTxnID)
}

// ============================================================================
// Order lifecycle
// ============================================================================

// CreateOrder mints an order row for a paid plan. The amount is a snapshot
// of plan.price at creation time — plan price changes don't retroactively
// affect in-flight orders.
//
// TOCTOU safety (D8): the plan eligibility check (active, accepting-new,
// currency match) and the order INSERT must commit atomically with
// respect to concurrent plan mutations. Without a lock, an operator
// deactivating the plan between FindByID and the order INSERT could
// leave an order pointing at a now-inactive plan. We wrap the check
// + INSERT in a single transaction and lock the plan row with
// FOR SHARE — concurrent FOR UPDATE / UPDATE / DELETE on the plan is
// blocked until commit, but other FOR SHARE reads coexist (no deadlock
// from concurrent payment attempts on the same plan).
//
// Order: subRepo active-sub check → eligibility tx. We check for an
// already-active sub BEFORE acquiring any tx-scoped lock or persisting
// an order row; otherwise a user with an active sub would receive an
// orphan pending order that the sweeper eventually expires. The plan
// eligibility lookup (FOR SHARE) only happens once we know the user is
// allowed to create an order.
func (s *PaymentService) CreateOrder(ctx context.Context, userID, planID, channel string) (*model.Order, error) {
	if err := validateChannel(channel); err != nil {
		return nil, err
	}

	// Channel-specific pre-auth gate (D1): some channels refuse to mint
	// the pre-auth artifact (e.g. WeChat `code_url`) without a configured
	// client. The check MUST run BEFORE orderRepo.Create — otherwise the
	// order row is persisted as `pending`, then CreateOrder returns an
	// error, and the caller is left staring at an orphan pending order
	// until the sweeper expires it (ORDER_EXPIRY_DURATION, default 30m).
	// Retries multiply the rows. New channels should extend
	// providerPreAuth rather than inserting ad-hoc guards here.
	if err := s.providerPreAuth(channel); err != nil {
		return nil, err
	}

	// Enforce the partial unique index `UNIQUE(user_id) WHERE status='active'`
	// at the order layer. Without this pre-check, a concurrent order + activate
	// would hit the constraint at INSERT time and surface as a 500; the user
	// gets a clean 409 instead. The DB invariant IS the primitive — this is
	// just a friendly surface for it. If the product later allows multiple
	// active rows, both this check and the partial unique index need to change
	// together.
	//
	// Repurchase rule (2026-07-28): an active, unexpired subscription no
	// longer blanket-rejects new orders. With rollover at activation
	// (resolveSubExpiry), a same-plan renewal extends from the current
	// expiry and an upgrade to a longer-interval plan carries the
	// remaining days over — both are fair to the user, so both are
	// allowed. Only a DOWNGRADE to a shorter-interval plan is rejected
	// (ErrPlanDowngrade → 409). resolveSubExpiry blocks the mirror-image
	// race at activation time (a stale shorter-cycle order paid after an
	// upgrade) with ErrDowngradeActivationBlocked.
	//
	// "active" here means status='active' AND the sub has not lapsed
	// (expires_at NULL or future). A stale row (status='active' with
	// expires_at < now()) is treated as expired and permitted through;
	// the upcoming activateSubscriptionOnTx will UPDATE that row in
	// place rather than create a new one, preserving the partial
	// unique index. Without this carve-out, users whose subscription
	// quietly went past could not renew even after the cn-staging
	// 2026-07-23 login-decouple fix let them log in.
	if existing, err := s.subRepo.FindActiveByUserID(ctx, userID); err == nil {
		if existing.ExpiresAt == nil || existing.ExpiresAt.After(time.Now()) {
			allowed, aerr := s.repurchaseAllowed(ctx, existing.PlanID, planID)
			if aerr != nil {
				return nil, aerr
			}
			if !allowed {
				return nil, ErrPlanDowngrade
			}
		}
		// stale: status='active' but expires_at < now(). Allow order
		// creation — activateSubscriptionOnTx will update this row.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check active sub: %w", err)
	}

	var order *model.Order
	err := s.eligibilityAndInsertOrderTx(ctx, userID, planID, channel, &order)
	if err != nil {
		return nil, err
	}

	// WeChat Pay NATIVE: mint code_url so the BFF can render a QR. The
	// providerPreAuth above already ensured s.wechat != nil when channel
	// is wechat_pay, so this block is reached only on a WeChat-enabled
	// deployment. Mock mode and non-WeChat channels do not need an
	// upstream pre-auth call.
	if channel == "wechat_pay" {
		// Mock-mode UnifiedOrder returns a deterministic code_url
		// synchronously (weixin://wxpay/bizpayurl?pr=mock_<OutTradeNo>);
		// no HTTP. The previous `&& !s.wechat.IsMockMode()` guard
		// short-circuited the mock path, leaving mock-mode orders with
		// provider_intent unset — BFF WeChatPayModal then 500s on
		// "WeChat order missing provider_intent.code_url" (see
		// yunhou-website server/src/providers/wechat.ts:40-42).
		// The CN-staging demo path needs the mock to mint a code_url
		// end-to-end; real mode is unaffected because the real client
		// hits api.mch.weixin.qq.com exactly as before.
		// Convert CNY decimal to fen without float multiplication: format to
		// two decimal places, strip the decimal point, then parse the integer.
		amountStr := fmt.Sprintf("%.2f", order.Amount)
		normalized := strings.ReplaceAll(amountStr, ".", "")
		amountFen, err := strconv.ParseInt(normalized, 10, 64)
		if err != nil {
			return order, fmt.Errorf("amount to fen: %w", err)
		}

		// WeChat's out_trade_no max length is 32 chars; our UUIDs are 36
		// chars. Strip hyphens + truncate to 32 — still globally unique
		// (UUIDs are hex digits and the prefix keeps the lexicographic
		// ordering for human-readable logs / database inspection).
		outTradeNo := strings.ReplaceAll(order.ID, "-", "")[:32]
		resp, err := s.wechat.UnifiedOrder(ctx, wechat.UnifiedOrderRequest{
			OutTradeNo:  outTradeNo,
			Description: fmt.Sprintf("plan-%s", planID),
			Amount:      wechat.Amount{Total: amountFen, Currency: order.Currency},
			TradeType:   wechat.TradeTypeNative,
		})
		if err != nil {
			// The pending order already exists. The caller may cancel and retry,
			// or the sweeper will eventually expire it.
			return order, fmt.Errorf("wechat unified order: %w", err)
		}

		// v3 NATIVE body fields are `appid` + `mchid` (no underscores) —
		// the BFF uses mchid to audit-log which merchant handled each
		// payment, and appid to cross-reference WeChat Open Platform info.
		intentBytes, _ := json.Marshal(map[string]string{
			"appid":        s.wechat.AppID(),
			"mchid":        s.wechat.MchID(),
			"code_url":     resp.CodeURL,
			"out_trade_no": outTradeNo,
		})
		intent := json.RawMessage(intentBytes)
		if err := s.orderRepo.UpdateProviderIntent(ctx, order.ID, intentBytes); err != nil {
			return order, fmt.Errorf("persist provider intent: %w", err)
		}
		// Stamp LastReconciledAt = now so the first FE poll after
		// CreateOrder doesn't immediately hit WeChat (orders typically
		// sit pending for minutes while the user scans the QR; we don't
		// need an outbound call in that window). Set on the in-memory
		// order so tests that don't persist (or that use a nil
		// sqlx.DB) still see the new gate; the default NOT NULL now() on
		// the column covers orders that go through the normal DB path.
		order.LastReconciledAt = time.Now()
		// ProviderIntent is *json.RawMessage so a SQL NULL column scans
		// into a nil pointer, which then trips omitempty on the JSON
		// response. The marshalled intent is addressable here (we just
		// allocated it), so the pointer is safe to share with the row.
		order.ProviderIntent = &intent
	}

	return order, nil
}

// repurchaseAllowed reports whether a user whose active, unexpired
// subscription is on currentPlanID may create an order for
// requestedPlanID. Same-or-longer billing cycle → allowed: a same-plan
// renewal rolls over at activation, and a longer-cycle upgrade carries
// the remaining days over (both in resolveSubExpiry). Shorter cycle →
// downgrade, rejected with ErrPlanDowngrade by the caller.
//
// Two non-comparable cases defer rather than block: an unknown
// requested plan is left to the eligibility tx's own validation (so the
// caller gets the proper plan error, not a misleading downgrade one),
// and a retired/legacy current plan (no plans row) allows the purchase.
func (s *PaymentService) repurchaseAllowed(ctx context.Context, currentPlanID, requestedPlanID string) (bool, error) {
	requested, err := s.planRepo.FindByID(ctx, requestedPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("find requested plan: %w", err)
	}
	current, err := s.planRepo.FindByID(ctx, currentPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("find current plan: %w", err)
	}
	return requested.IntervalDays >= current.IntervalDays, nil
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

// reconcileMinInterval bounds the active-query rate when a pending order
// has no payments yet: each caller (FE polling at 500ms) would otherwise
// hit WeChat on every poll, which WeChat rate-limits aggressively.
// Real production deployments should also throttle by user; we keep
// the per-order throttle simple and rely on the FE poll cadence for
// user-side throttle.
const reconcileMinInterval = 10 * time.Second

// GetOrder returns an order by ID, or ErrOrderNotFound if missing or
// not owned by the caller. Internal-app callers (via SetOrderInternal)
// bypass the ownership check.
//
// Active reconciliation (2026-07-23): for a still-pending wechat_pay
// order with no payments yet, GetOrder also calls
// wechat.QueryOrder(out_trade_no) at most once per reconcileMinInterval
// and, on TradeState=SUCCESS, drives the order through the same
// payment-paid → subscription-activated pipeline that the webhook
// uses. The reconcile is the safety net for the 2026-07-22 bug
// where every webhook failed signature verification (HMAC with the
// wrong key) and paid orders never reconciled; with the new
// platform-cert RSA verifier webhooks usually land, but a transient
// WeChat outage or a WeChat cert rotation can still drop a delivery,
// and the FE will keep polling until it sees paid.
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

	if s.shouldReconcile(ctx, o) {
		if rerr := s.reconcileFromChannel(ctx, o); rerr != nil {
			// Reconcile failure must not block the read; the FE poll
			// retries. Log so an operator can spot a sustained upstream
			// outage.
			log.Printf("payment reconcile failed for order=%s: %v", o.ID, rerr)
		} else {
			// Re-read so the response reflects the post-reconcile state.
			if refreshed, ferr := s.orderRepo.FindByID(ctx, orderID); ferr == nil {
				o = refreshed
			}
		}
	}
	return o, nil
}

// shouldReconcile gates the active QueryOrder path. Conditions:
//   - status still pending/expired (terminal states have nothing to gain)
//   - the order carries a wechat provider_intent block (every wechat_pay
//     order does; PayPal/Alipay orders have their own reconciliation)
//   - last_reconciled_at > reconcileMinInterval ago, so 500ms FE polls
//     collapse to ~1 outbound call per 10s per order
//   - wechat client is wired (non-nil) AND not in mock mode (mock mode
//     already short-circuits via the mock webhook)
func (s *PaymentService) shouldReconcile(ctx context.Context, o *model.Order) bool {
	if o.Status != "pending" && o.Status != "expired" {
		return false
	}
	if s.wechat == nil || s.wechat.IsMockMode() {
		return false
	}
	if time.Since(o.LastReconciledAt) < reconcileMinInterval {
		return false
	}
	// WeChat orders always write provider_intent.appid; PayPal/Alipay
	// orders carry different keys. A nil provider_intent is rare (would
	// mean a wechat_pay order that failed pre-auth, which the handler
	// already 4xx'd) — skip silently.
	if o.ProviderIntent == nil {
		return false
	}
	var intent struct {
		AppID string `json:"appid"`
	}
	if err := json.Unmarshal(*o.ProviderIntent, &intent); err != nil || intent.AppID == "" {
		return false
	}
	return true
}

// reconcileFromChannel queries WeChat for the order's current state and,
// if SUCCESS, dispatches the same payment-paid handler that the webhook
// would. Errors are returned to the caller (GetOrder logs them); the
// reconcile is idempotent — re-running it on an already-paid order is a
// no-op (no payments row, then payments-insert dedupe, then
// activateSubscriptionOnTx UPSERT, all safe).
func (s *PaymentService) reconcileFromChannel(ctx context.Context, o *model.Order) error {
	if o.ProviderIntent == nil {
		// No pre-auth payload: nothing to query WeChat with. Skip
		// silently (this can happen for non-wechat channels, but the
		// caller already gates on channel == wechat_pay).
		return nil
	}
	var intent struct {
		OutTradeNo string `json:"out_trade_no"`
	}
	if err := json.Unmarshal(*o.ProviderIntent, &intent); err != nil || intent.OutTradeNo == "" {
		// Malformed provider_intent (or no out_trade_no). Skip rather
		// than 500; the order would have failed earlier if out_trade_no
		// was genuinely missing.
		return nil
	}
	res, err := s.wechat.QueryOrder(ctx, intent.OutTradeNo)
	if err != nil {
		return err
	}
	if res == nil || res.TradeState != "SUCCESS" {
		// Persistent NOTPAY (or upstream returned a body we can't decode
		// past). Stamp last_reconciled_at so the next FE poll within
		// reconcileMinInterval doesn't trigger another outbound call.
		// Successful reconcile stamps AFTER OnWebhook commits (see below)
		// — see the matching comment there.
		if _, err := s.db.ExecContext(ctx, `UPDATE orders SET last_reconciled_at = now() WHERE id = $1`, o.ID); err != nil {
			return fmt.Errorf("stamp last_reconciled_at: %w", err)
		}
		return nil
	}
	// Treat as a TRANSACTION.SUCCESS webhook. The real WeChat webhook
	// and the reconcile-synthesized event use DIFFERENT EventID shapes
	// (real: WeChat's `evt.ID` UUID; reconcile: "reconcile:" + out_trade_no
	// + ":" + transaction_id) so they CAN both land in webhook_events.
	// Dedupe therefore happens at the payment-row layer, not the
	// event-row layer: onPaymentSucceeded's INSERT-payment-on-conflict-
	// do-nothing path re-reads the existing payment, and if it's already
	// paid the subscription activation runs again with the same plan +
	// same nil expiry — a no-op UPSERT. To skip the second activation
	// altogether (and avoid touching webhook_events for the synthetic
	// event when a real one already paid), pre-check the payment row
	// here and short-circuit before OnWebhook. Race-safe: if a real
	// webhook lands between this read and the OnWebhook transaction,
	// onPaymentSucceeded's existing dedupe catches it.
	existing, err := s.paymentRepo.FindByChannelTxnID(ctx, "wechat_pay", res.TransactionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reconcile pre-check: %w", err)
	}
	if skip, skipErr := reconcilePreCheck(existing, err); skip || skipErr != nil {
		if skip {
			// Real webhook (or earlier reconcile) already paid this
			// transaction. OnWebhook would just upsert with the same
			// values and waste a row in webhook_events. Tag with all
			// three correlation keys (order / payment / txn) so on-call
			// can grep by any of them when investigating a stuck
			// reconciliation.
			log.Printf("payment reconcile: order=%s txn=%s already paid (payment=%s); skipping OnWebhook",
				o.ID, res.TransactionID, existing.ID)
			if _, err := s.db.ExecContext(ctx, `UPDATE orders SET last_reconciled_at = now() WHERE id = $1`, o.ID); err != nil {
				return fmt.Errorf("stamp last_reconciled_at: %w", err)
			}
		}
		if skipErr != nil {
			return fmt.Errorf("reconcile pre-check: %w", skipErr)
		}
		return nil
	}

	event, err := buildReconcileWebhookEvent(res)
	if err != nil {
		return fmt.Errorf("build reconcile event: %w", err)
	}
	if _, err := s.OnWebhook(ctx, event); err != nil {
		// Don't stamp last_reconciled_at on failure — we want the next
		// FE poll within reconcileMinInterval to retry the reconcile
		// instead of silently leaving a paid order as 'pending' until
		// the throttle expires. OnWebhook is idempotent (eventID dedupe),
		// so retrying is safe.
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE orders SET last_reconciled_at = now() WHERE id = $1`, o.ID); err != nil {
		// Non-fatal: the order is already paid; the throttle will
		// re-engage on the first OnWebhook-side retry of a later poll.
		log.Printf("payment reconcile: stamp last_reconciled_at after success failed for order=%s: %v", o.ID, err)
	}
	return nil
}

// ListUserPayments returns payments for orders owned by userID. Like
// ListUserOrders, a nil repo result is normalised to an empty slice so
// the JSON response is `"data": []`, not `"data": null` — the two list
// endpoints keep the same contract for FE consumers.
func (s *PaymentService) ListUserPayments(ctx context.Context, userID string) ([]model.Payment, error) {
	list, err := s.paymentRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	if list == nil {
		list = []model.Payment{}
	}
	return list, nil
}

// ListUserOrders returns the caller's orders newest-first for the console
// order-history view. Read-only: unlike GetOrder it does NOT drive the
// channel reconcile path — a history listing must not fan out one upstream
// QueryOrder per pending row. Stale pending rows still converge via the
// sweeper and the single-order poll the FE runs while a QR is open.
//
// A nil repo result (no rows) is normalised to an empty slice so the JSON
// response is `"data": []`, not `"data": null` — the FE renders the empty
// state straight from the array without a null branch.
func (s *PaymentService) ListUserOrders(ctx context.Context, userID string) ([]model.Order, error) {
	list, err := s.orderRepo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	if list == nil {
		list = []model.Order{}
	}
	return list, nil
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
	// Amount and Currency are NOT caller-supplied; the order row is the
	// authoritative source. Adding them here would let a caller claim they
	// paid a different amount than the order — the webhook reconciles
	// against the channel's actual amount, but the subscription would
	// already be activated for the wrong amount by then.
	ExpiresAt *time.Time // optional; subscription expiry (nil = never expires per plan defaults)
}

// ConfirmResult is the response from Confirm.
type ConfirmResult struct {
	PaymentID             string `json:"payment_id"`
	OrderID               string `json:"order_id"`
	Status                string `json:"status"`
	ActivatedSubscription bool   `json:"activated_subscription"`
	WasLatePayment        bool   `json:"was_late_payment"` // true if order was expired and we honored
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

	// Amount and currency come from the order — the order is the
	// authoritative source. Caller-supplied amounts are not accepted
	// (see ConfirmInput doc comment).

	// Activate subscription + update order + handle late-payment honor
	// in one transaction so partial failures don't leave inconsistent state.
	tx, err := s.dbBeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	txSQLX := rawSQLXTx(tx)

	// Channel mismatch check happens INSIDE the tx with FOR UPDATE on the
	// existing paid payment row, so a concurrent webhook can't slip a
	// different-channel payment in between this check and the INSERT below.
	// Without this, the partial unique index would surface a unique_violation
	// as a 500 instead of the desired 409 ErrOrderChannelMismatch.
	var existingChannel string
	err = tx.QueryRowxContext(ctx, `
		SELECT channel FROM payments WHERE order_id = $1 AND status = 'paid' FOR UPDATE
	`, order.ID).Scan(&existingChannel)
	switch {
	case err == nil:
		if existingChannel != in.Channel {
			return nil, ErrOrderChannelMismatch
		}
	case errors.Is(err, sql.ErrNoRows):
		// no paid row yet — proceed to INSERT
	default:
		return nil, fmt.Errorf("check existing payment: %w", err)
	}

	now := time.Now()
	rawPayload, _ := json.Marshal(map[string]any{
		"source":   "frontend_confirm",
		"order_id": order.ID,
	})
	p := &model.Payment{
		ID:            GenerateUUID(),
		OrderID:       order.ID,
		Channel:       in.Channel,
		ExternalTxnID: in.ExternalTxnID,
		Amount:        order.Amount,
		Currency:      order.Currency,
		Status:        "paid",
		PaidAt:        &now,
		RawPayload:    rawPayload,
	}

	paymentID, inserted, err := insertPaymentOnTx(ctx, tx, p)
	if err != nil {
		return nil, fmt.Errorf("insert payment: %w", err)
	}

	// Retry path: pre-fetch the existing active sub's expiry so
	// resolveSubExpiry can preserve it. Only triggered when the payment
	// row already exists (dedupe hit on channel+external_txn_id); a
	// fresh order does not enter this branch — the cross-order scenario
	// the original user-id check broke.
	var preservedExpiry *time.Time
	// downgradeRetry mirrors the first-delivery downgrade guard onto
	// dedupe retries: preservedExpiry short-circuits resolveSubExpiry
	// BEFORE the downgrade comparison, so without this check a retry of
	// a downgrade-blocked payment (Confirm + webhook double delivery is
	// the norm) would sail straight into activateSubscriptionOnTx and
	// overwrite the longer-cycle sub's plan_id — silently undoing the
	// block the first delivery applied.
	downgradeRetry := false
	if !inserted {
		// (channel, external_txn_id) dedupe hit — the row already exists.
		// Re-read it; if it's paid we're done (idempotent). If it's
		// failed, refuse (the channel says this attempt failed even
		// though the frontend thinks it succeeded).
		//
		// Use the tx-bound variant when tx is a real *sqlx.Tx so the read
		// shares the surrounding tx's connection. Without this, with
		// MaxOpenConns=25, 25 concurrent dedupe Confirms would each hold
		// a tx connection and then fight for a second one here — same
		// deadlock class as resolveSubExpiry's planRepo lookup.
		existing, ferr := s.txLookupPaymentByChannelTxnID(ctx, txSQLX, in.Channel, in.ExternalTxnID)
		if ferr != nil {
			return nil, fmt.Errorf("re-read existing payment: %w", ferr)
		}
		if existing.Status == "failed" {
			return nil, ErrOrderAlreadyTerminal
		}
		// If the existing row is `paid`, this is a confirm retry — proceed
		// to ensure sub activation + order update are idempotent.
		paymentID = existing.ID

		activeSub, sErr := s.txLookupActiveSubscription(ctx, txSQLX, order.UserID)
		if sErr != nil && !errors.Is(sErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("find active sub for retry preservation: %w", sErr)
		}
		if activeSub != nil && activeSub.ExpiresAt != nil && activeSub.ExpiresAt.After(time.Now()) {
			preservedExpiry = activeSub.ExpiresAt
			// Plan mismatch on a retry means the first delivery did NOT
			// activate this order's plan (a successful activation would
			// have stamped order.PlanID onto the sub) — i.e. it was
			// downgrade-blocked. Block the retry too.
			if activeSub.PlanID != order.PlanID {
				downgradeRetry = true
			}
		}
	}

	// Activate subscription (UPSERT single-row, webhook doc §5.3).
	// expires_at resolution mirrors the webhook path: a BFF-supplied
	// ExpiresAt wins; otherwise fall back to plan.interval_days so that
	// channels whose upstream payload doesn't ship sub_expires_at (real
	// WeChat v3 NATIVE today) still produce a finite subscription. A
	// first activation replacing an unexpired sub rolls the remaining
	// days over (resolveSubExpiry branch 3).
	subExpiry, rerr := s.resolveSubExpiry(ctx, txSQLX, order.UserID, order.PlanID, in.ExpiresAt, preservedExpiry)
	downgradeBlocked := errors.Is(rerr, ErrDowngradeActivationBlocked) || downgradeRetry
	if downgradeRetry {
		// The first delivery wrote its own audit row when it blocked;
		// log the retry too so the repeat delivery is visible rather
		// than silently no-op'd.
		_ = writeAuditOnTx(ctx, tx, "service", "downgrade_activation_blocked",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"confirm", "downgrade", "activation_blocked", "retry"},
			map[string]any{
				"order_id": order.ID,
				"channel":  in.Channel,
				"plan_id":  order.PlanID,
			})
	}
	switch {
	case rerr == nil:
	case errors.Is(rerr, ErrPlanMissingForExpiry):
		_ = writeAuditOnTx(ctx, tx, "service", "subscription_expiry_plan_missing",
			fmt.Sprintf("plan:%s", order.PlanID),
			[]string{"confirm", "expiry_fallback", "plan_missing"},
			map[string]any{
				"order_id": order.ID,
				"channel":  in.Channel,
			})
	case downgradeBlocked:
		// A stale shorter-cycle order (e.g. an old monthly QR) was paid
		// after the user upgraded. Honor the payment — the order goes
		// paid below and ops refunds manually — but leave the
		// longer-cycle subscription untouched.
		_ = writeAuditOnTx(ctx, tx, "service", "downgrade_activation_blocked",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"confirm", "downgrade", "activation_blocked"},
			map[string]any{
				"order_id": order.ID,
				"channel":  in.Channel,
				"plan_id":  order.PlanID,
			})
	default:
		return nil, fmt.Errorf("resolve sub expiry: %w", rerr)
	}
	activated := false
	if !downgradeBlocked {
		activated, err = activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiry)
		if err != nil {
			return nil, fmt.Errorf("activate sub: %w", err)
		}
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

	// Caller-retry gate: same (user, key) → same row, no channel call.
	// Scoped to in.UserID — a global key lookup would let user B see user
	// A's refund response by reusing the same key (IDOR).
	if existing, err := s.refundRepo.FindByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey); err == nil && existing != nil {
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
	// Load the order for ownership + user_id propagation. For internal-app
	// callers, in.UserID may be empty — we adopt the order's user_id as
	// the canonical owner for the refund row.
	o, oerr := s.orderRepo.FindByID(ctx, payment.OrderID)
	if oerr != nil {
		return nil, fmt.Errorf("load order: %w", oerr)
	}
	if !in.InternalApp {
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
	tx, err := s.dbBeginTx(ctx)
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

	// Sum invariant under lock. Count both `paid` and `pending` so two
	// concurrent refunds serialized by the FOR UPDATE lock can't each pass
	// the check (the prior version excluded `pending` rows and let 4×$10
	// slip past on a $30 payment when all 4 were still pending).
	// `failed` rows are excluded — those are terminal denials, not
	// reservations, and don't block a retry of the same logical amount.
	var currentSum float64
	if err := tx.GetContext(ctx, &currentSum,
		`SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1 AND status IN ('paid', 'pending')`, payment.ID); err != nil {
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
		UserID:           o.UserID,
		Amount:           in.Amount,
		Reason:           in.Reason,
		IdempotencyKey:   in.IdempotencyKey,
		ExternalRefundID: &extID,
		Status:           "pending",
	}
	if _, err := tx.NamedExecContext(ctx, `
		INSERT INTO refunds (id, payment_id, channel, user_id, amount, reason, idempotency_key, external_refund_id, status)
		VALUES (:id, :payment_id, :channel, :user_id, :amount, :reason, :idempotency_key, :external_refund_id, :status)
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
	Channel                string
	EventID                string // channel's event ID — Stripe `evt.id`, WeChat `notify_id`, Alipay `notify_id`
	EventType              string // channel's event type string
	RawPayload             json.RawMessage
	TransactionID          string     // channel's transaction ID — maps to payments.external_txn_id
	OrderID                string     // order UUID from channel metadata (Stripe) or out_trade_no (WeChat/Alipay)
	Amount                 float64    // settled amount (major currency units, normalized by handler)
	Currency               string     // ISO 4217
	RefundAmount           float64    // for refund events
	ExternalRefundID       string     // channel's refund ID
	ExternalSubscriptionID string     // PayPal: subscription ID (`I-...`) — used by renewal branch to find the active sub
	SubExpiresAt           *time.Time // subscription expiry at activation. MUST be supplied by the
	// caller (e.g. an explicit channel metadata field) — yunhou-users
	// MUST NOT compute it from plan.interval_days; that calculation is
	// a frontend product decision (rollover rules, grace periods, trials).
	// nil = never expires.
}

// OnWebhookResult reports what the handler did.
type OnWebhookResult struct {
	DuplicateEvent bool   // true if event_id was already seen (handler should ack 200)
	DomainAction   string // set only when an action ran. Values:
	//   "payment_paid" / "payment_failed" / "refund_paid"
	//   / "payment_disputed" / "payment_dispute_closed" / "none"
	// Empty string ("") means no action ran — either a dedupe hit
	// (DuplicateEvent=true) or an uninteresting event type. Consumers
	// should branch on `duplicate` for the dedupe case, not on
	// `domain_action == "none"`.
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
		// Dedupe hit. Two cases:
		//  (a) processed_at IS NOT NULL — prior run finished cleanly.
		//      Ack 200, no re-run.
		//  (b) processed_at IS NULL — prior run crashed mid-action.
		//      Re-run the business action. Each handler is idempotent
		//      (UPSERT in payments, UPDATE on terminal states, etc.) so
		//      a re-run is safe. This converts at-most-once delivery
		//      into at-least-once with idempotent side effects.
		prior, ferr := s.webhookRepo.FindByChannelEventID(ctx, e.Channel, e.EventID)
		if ferr != nil {
			return nil, fmt.Errorf("lookup prior webhook: %w", ferr)
		}
		if prior.ProcessedAt != nil {
			return &OnWebhookResult{DuplicateEvent: true}, nil
		}
		// Re-run; reuse the same webhook_events.id so the MarkProcessed
		// below updates the existing row instead of inserting a duplicate.
		eventRowID = prior.ID
	}

	var domainAction string

	switch {
	case isPaymentSuccess(e.EventType):
		domainAction = "payment_paid"
		if err := s.onPaymentSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case isPaypalRenewal(e.EventType):
		domainAction = "payment_paid"
		if err := s.onPaypalRenewalSucceeded(ctx, e); err != nil {
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
	tx, err := s.dbBeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	txSQLX := rawSQLXTx(tx)

	// Find the order. WeChat and Alipay send a channel-side identifier
	// (out_trade_no, 32-char hex) rather than our UUID, and the
	// WebhookEvent.OrderID contract documents this for the handler. The
	// PRIMARY lookup is by id (covers Stripe + e2e tests that pass the
	// UUID); the FALLBACK is a JSONB walk for wechat_pay/alipay's
	// out_trade_no (covers real-world webhooks that send the 32-char
	// form). Both queries run inside the same tx so MaxOpenConns
	// constraints don't deadlock (the same constraint the comment below
	// the SELECT-by-id block already documents).
	//
	// MAJOR fix (review 2): without the fallback, real WeChat webhooks
	// 404'd because no order has the 32-char hex as its primary id; the
	// order would stay "pending" forever and the user never got the
	// subscription.
	var order model.Order
	err = tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, e.OrderID)
	if errors.Is(err, sql.ErrNoRows) && (e.Channel == "wechat_pay" || e.Channel == "alipay") {
		// JSONB text-extraction ->> returns NULL for rows without the
		// key; equality with the channel's out_trade_no resolves it to
		// the canonical order. LIMIT 1 in case the JSONB value collides
		// (operator error — duplicate out_trade_no is a YDN alert).
		err = tx.GetContext(ctx, &order, `
			SELECT * FROM orders
			WHERE provider_intent IS NOT NULL
			  AND provider_intent->>'out_trade_no' = $1
			LIMIT 1
		`, e.OrderID)
	}
	if err != nil {
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

	// Channel mismatch pre-check (same as Confirm). Inlined as a tx
	// query (instead of calling paymentRepo.FindPaidByOrderID) so the
	// lookup reuses the same connection as the surrounding transaction.
	// Calling the repo here would acquire a fresh connection from the
	// pool, and under load (MaxOpenConns capped in tests) 5 concurrent
	// webhooks each holding 1 tx connection would deadlock waiting for
	// the second connection — the original bug that surfaced in
	// TestPayments_ConcurrentWebhookSameOrder.
	var existing model.Payment
	if err := tx.GetContext(ctx, &existing,
		`SELECT * FROM payments WHERE order_id = $1 AND status = 'paid' LIMIT 1`,
		order.ID); err == nil {
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

	// Retry path: pre-fetch the existing active sub's expiry so
	// resolveSubExpiry can preserve it instead of computing a new
	// `now() + interval_days` and shifting the subscription forward.
	// Only set when the current call is a retry of an already-paid
	// payment on the same (channel, external_txn_id). A fresh order,
	// even for the same user, must NOT trigger this branch — that's
	// the cross-order scenario the original user-id check broke.
	var preservedExpiry *time.Time
	// downgradeRetry mirrors the first-delivery downgrade guard onto
	// dedupe retries: preservedExpiry short-circuits resolveSubExpiry
	// BEFORE the downgrade comparison, so without this check a retry of
	// a downgrade-blocked payment (Confirm + webhook double delivery is
	// the norm) would sail straight into activateSubscriptionOnTx and
	// overwrite the longer-cycle sub's plan_id — silently undoing the
	// block the first delivery applied.
	downgradeRetry := false
	if !inserted {
		// Dedupe hit — payment row already exists. Re-read to know whether
		// it's paid (no-op) or in a state we need to escalate.
		//
		// Use the tx-bound variant when tx is a real *sqlx.Tx so the read
		// shares the surrounding tx's connection. Without this, with
		// MaxOpenConns=25, 25 concurrent webhook retries for the same
		// (channel, external_txn_id) would each hold a tx connection and
		// then fight for a second one here — same deadlock class as
		// resolveSubExpiry's planRepo lookup.
		existing, ferr := s.txLookupPaymentByChannelTxnID(ctx, txSQLX, e.Channel, e.TransactionID)
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

		activeSub, sErr := s.txLookupActiveSubscription(ctx, txSQLX, order.UserID)
		if sErr != nil && !errors.Is(sErr, sql.ErrNoRows) {
			return fmt.Errorf("find active sub for retry preservation: %w", sErr)
		}
		if activeSub != nil && activeSub.ExpiresAt != nil && activeSub.ExpiresAt.After(time.Now()) {
			preservedExpiry = activeSub.ExpiresAt
			// Plan mismatch on a retry means the first delivery did NOT
			// activate this order's plan (a successful activation would
			// have stamped order.PlanID onto the sub) — i.e. it was
			// downgrade-blocked. Block the retry too.
			if activeSub.PlanID != order.PlanID {
				downgradeRetry = true
			}
		}
	}

	// Subscription activation is gated by payment success, not by order
	// status — see doc §"Subscription activation". The order UPDATE below
	// is idempotent (already-paid / late-paid / cancelled-but-honored all
	// succeed). Re-running the activation UPSERT on a retried event is
	// safe — the UPDATE branch of activateSubscriptionOnTx hits the same
	// row.
	subExpiry, rerr := s.resolveSubExpiry(ctx, txSQLX, order.UserID, order.PlanID, e.SubExpiresAt, preservedExpiry)
	downgradeBlocked := errors.Is(rerr, ErrDowngradeActivationBlocked) || downgradeRetry
	if downgradeRetry {
		// The first delivery wrote its own audit row when it blocked;
		// log the retry too so the repeat delivery is visible rather
		// than silently no-op'd.
		_ = writeAuditOnTx(ctx, tx, "service", "downgrade_activation_blocked",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"webhook", "downgrade", "activation_blocked", "retry"},
			map[string]any{
				"order_id": order.ID,
				"channel":  e.Channel,
				"event_id": e.EventID,
				"plan_id":  order.PlanID,
			})
	}
	switch {
	case rerr == nil:
	case errors.Is(rerr, ErrPlanMissingForExpiry):
		// Intentional: plan_missing is informational — the payment already
		// succeeded, so we silently audit and let NULL fall through instead
		// of failing the activation.
		_ = writeAuditOnTx(ctx, tx, "service", "subscription_expiry_plan_missing",
			fmt.Sprintf("plan:%s", order.PlanID),
			[]string{"webhook", "expiry_fallback", "plan_missing"},
			map[string]any{
				"order_id": order.ID,
				"channel":  e.Channel,
				"event_id": e.EventID,
			})
	case downgradeBlocked:
		// A stale shorter-cycle order (e.g. an old monthly QR) was paid
		// after the user upgraded. Honor the payment — the order goes
		// paid below and ops refunds manually — but leave the
		// longer-cycle subscription untouched.
		_ = writeAuditOnTx(ctx, tx, "service", "downgrade_activation_blocked",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"webhook", "downgrade", "activation_blocked"},
			map[string]any{
				"order_id": order.ID,
				"channel":  e.Channel,
				"event_id": e.EventID,
				"plan_id":  order.PlanID,
			})
	default:
		return fmt.Errorf("resolve sub expiry: %w", rerr)
	}
	if !downgradeBlocked {
		if _, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, subExpiry); err != nil {
			return fmt.Errorf("activate sub: %w", err)
		}
	}

	// PayPal: stamp the PayPal subscription ID on the active row so renewal
	// webhooks (PAYMENT.SALE.COMPLETED) can find the user's subscription
	// via external_subscription_id. The partial UNIQUE index
	// subs_external_sub_id makes re-runs a no-op, so retries are safe.
	// Skipped silently when e.ExternalSubscriptionID is empty (one-time
	// capture without a subscription context).
	//
	// Without the "external_subscription_id IS NULL" guard, re-activation
	// after a previous PayPal sub was cancelled would leave the stale
	// "I-OLD" ID on the row — subsequent renewals for the NEW subscription
	// would fail to find the row, hitting paypal_renewal_unknown_subscription
	// and silently dropping paid charges.
	if e.ExternalSubscriptionID != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			SET external_subscription_id = $1
			WHERE id = (
				SELECT id FROM subscriptions
				WHERE user_id = $2
				  AND plan_id = $3
				  AND status = 'active'
				ORDER BY created_at DESC
				LIMIT 1
			)
		`, e.ExternalSubscriptionID, order.UserID, order.PlanID); err != nil {
			return fmt.Errorf("set external_subscription_id: %w", err)
		}
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
	tx, err := s.dbBeginTx(ctx)
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
		// Scope to the order's plan_id: an unrelated active subscription on
		// a different plan must NOT be torn down by this order's failure
		// (the user may have multiple plans in play).
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET status = 'cancelled', updated_at = now()
			WHERE user_id = $1 AND plan_id = $2 AND status = 'active'
		`, order.UserID, order.PlanID); err != nil {
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
	tx, err := s.dbBeginTx(ctx)
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
	// Load the order to get user_id for the refunds row. The webhook
	// path needs to populate refunds.user_id since the user didn't
	// pass an Idempotency-Key here (the channel drives the event).
	var order model.Order
	if err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, payment.OrderID); err != nil {
		return fmt.Errorf("load order: %w", err)
	}

	// Find or insert the refund row keyed on (channel, external_refund_id).
	// Insert as `pending` first so the sum-invariant (which counts pending
	// rows in Refund) holds even when this path creates a refund row that
	// wasn't initiated via POST /refunds. The follow-up UPDATE flips
	// pending → paid atomically; re-runs of the same webhook are no-ops
	// because the second pass sees `paid` and skips.
	// ON CONFLICT DO NOTHING absorbs webhook retries; re-read for the
	// (channel, external_refund_id) → id mapping.
	extID := e.ExternalRefundID
	var refundID string
	err = tx.QueryRowxContext(ctx, `
		INSERT INTO refunds (payment_id, channel, user_id, amount, idempotency_key, external_refund_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		ON CONFLICT (channel, external_refund_id) DO NOTHING
		RETURNING id
	`, payment.ID, e.Channel, order.UserID, e.RefundAmount, "webhook:"+e.EventID, extID).Scan(&refundID)
	switch {
	case err == nil:
		// inserted (new pending row — will be flipped below)
	case errors.Is(err, sql.ErrNoRows):
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

	// Full vs partial refund — only the channel's amount tells us. We
	// compare in integer cents (DECIMAL(10,2) → int64) to avoid float
	// round-trip drift; the +0.0001 epsilon was masking this and mis-
	// classifying fee-inclusive refunds as full refunds.
	if toCents(e.RefundAmount) >= toCents(payment.Amount) {
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
		// Deactivate subscription. Only cancel the sub matching this
		// order's plan_id — an unrelated active subscription on a
		// different plan must NOT be cancelled. Already
		// expired/cancelled subs are terminal (don't reopen them).
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET status = 'cancelled', updated_at = now()
			WHERE user_id = $1 AND plan_id = $2 AND status = 'active'
		`, order.UserID, order.PlanID); err != nil {
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
	tx, err := s.dbBeginTx(ctx)
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
	// The previous heuristic `if e.Amount > 0` was wrong — Stripe encodes
	// win/loss in the event's `status` field, not amount. A won dispute
	// can have any amount. Until we plumb `data.object.status` through
	// WebhookEvent, we conservatively no-op on every dispute.closed and
	// rely on the explicit `charge.refunded` event for the loss cascade.
	// This is intentional: mis-classifying a loss as a win would clear
	// `disputed=true` and skip the refund cascade; the prior code did
	// exactly the inverse.
	_ = e
	return nil
}

// Note: onPaypalRenewalSucceeded does NOT use resolveSubExpiry. The
// webhook path (onPaymentSucceeded → resolveSubExpiry) and the Confirm
// path (Confirm → resolveSubExpiry) both fall back to plan.interval_days
// when no sub_expires_at hint is supplied. PayPal renewal is intentionally
// different:
//
// - WeChat onboarding: sub_expires_at is structurally absent (v3 NATIVE
//   protocol doesn't carry it); the fallback is the only way to write a
//   non-NULL expires_at.
// - PayPal renewal: sub_expires_at is structurally PRESENT (resource.
//   billing_info.next_billing_time); falling back to plan.interval_days
//   when it's missing would silently mask a contract drift between
//   PayPal's product definition and our Plan. The
//   paypal_renewal_no_expiry_hint audit log lets ops reconcile manually.

// onPaypalRenewalSucceeded handles PAYMENT.SALE.COMPLETED — the renewal
// charge that PayPal fires automatically when a PayPal subscription
// auto-renews. We don't have an `orders` row for renewals (the original
// order was months ago); instead we mint a synthetic orders row keyed to
// the renewal payment, INSERT the payments row, and extend
// subscriptions.expires_at from resource.billing_info.next_billing_time.
//
// Refund of a renewal payment uses the same charge.refunded path on
// Stripe / WeChat / Alipay; PayPal uses PAYMENT.SALE.REFUNDED. We
// currently don't see renewal-refund events in scope — if/when PayPal
// adds one, routing it to isRefundEvent + onRefundSucceeded will Just
// Work because the channel=paypal + external_txn_id are populated the
// same way.
func (s *PaymentService) onPaypalRenewalSucceeded(ctx context.Context, e WebhookEvent) error {
	if e.ExternalSubscriptionID == "" {
		return s.writeAudit(ctx, "service", "paypal_renewal_missing_external_sub_id",
			fmt.Sprintf("event:%s", e.EventID),
			[]string{"webhook", "paypal", "renewal", "missing_field"},
			map[string]any{"event_id": e.EventID})
	}

	tx, err := s.dbBeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin renewal tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Advisory xact lock on (channel, external_txn_id) so two concurrent
	// deliveries of the same PAYMENT.SALE.COMPLETED can't both pass the
	// dedup SELECT and each mint a fresh synthetic order row. The lock
	// auto-releases on COMMIT/ROLLBACK — no manual unlock needed.
	// hashtext converts the 2-tuple into a single int8 key.
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))
	`, e.Channel, e.TransactionID); err != nil {
		return fmt.Errorf("acquire renewal lock: %w", err)
	}

	var sub model.Subscription
	err = tx.GetContext(ctx, &sub,
		`SELECT * FROM subscriptions WHERE external_subscription_id = $1 LIMIT 1 FOR UPDATE`,
		e.ExternalSubscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.writeAudit(ctx, "service", "paypal_renewal_unknown_subscription",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "paypal", "renewal", "unknown_sub"},
				map[string]any{
					"event_id":                 e.EventID,
					"external_subscription_id": e.ExternalSubscriptionID,
				})
		}
		return fmt.Errorf("find subscription by external sub id: %w", err)
	}

	// Dedupe BEFORE minting the synthetic order: if a payment row already
	// exists for (channel, external_txn_id), this is a webhook retry. The
	// event-level webhook_events.UNIQUE(channel, event_id) catch in OnWebhook
	// handles the happy path; this catches the rare case where the event
	// signature changed (e.g. PayPal rotates IDs) but the payment ID
	// matched a prior delivery.
	var existingPaymentID string
	err = tx.QueryRowxContext(ctx, `
		SELECT id FROM payments WHERE channel = $1 AND external_txn_id = $2 LIMIT 1
	`, e.Channel, e.TransactionID).Scan(&existingPaymentID)
	switch {
	case err == nil:
		// Already processed — skip the renew side-effects, audit-log only,
		// ack 200. The webhook_events table earlier should have made this
		// impossible, but if we got here it's a defensive guard.
		return s.writeAudit(ctx, "service", "paypal_renewal_payment_already_exists",
			fmt.Sprintf("event:%s", e.EventID),
			[]string{"webhook", "paypal", "renewal", "duplicate"},
			map[string]any{
				"event_id":                 e.EventID,
				"existing_payment_id":      existingPaymentID,
				"external_subscription_id": e.ExternalSubscriptionID,
			})
	case errors.Is(err, sql.ErrNoRows):
		// expected: this is a new renewal
	default:
		return fmt.Errorf("dedupe-check existing renewal payment: %w", err)
	}

	var orderID string
	// expires_at is NOT NULL on the orders schema (003_payments.sql).
	// A synthetic renewal order is paid immediately and never transitions
	// through the expiry sweeper, so its expires_at is purely cosmetic —
	// reconciliation queries that filter "expires_at < now() AND
	// status='paid'" would mis-classify a renewal as expired if we used
	// the schema default (now() + 30m). Use a far-future sentinel so the
	// row is unambiguously "not expired". When the webhook carries a
	// sub_expires_at hint, mirror it on the order so any operator query
	// joining orders→subscriptions sees a consistent timeline.
	orderExpiresAt := time.Now().AddDate(100, 0, 0) // +100y sentinel
	if e.SubExpiresAt != nil {
		orderExpiresAt = *e.SubExpiresAt
	}
	err = tx.QueryRowxContext(ctx, `
		INSERT INTO orders (user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, $3, $4, 'paid', $5, NULL)
		RETURNING id
	`, sub.UserID, sub.PlanID, e.Amount, e.Currency, orderExpiresAt).Scan(&orderID)
	if err != nil {
		return fmt.Errorf("insert synthetic renewal order: %w", err)
	}

	now := time.Now()
	p := &model.Payment{
		OrderID:       orderID,
		Channel:       e.Channel,
		ExternalTxnID: e.TransactionID,
		Amount:        e.Amount,
		Currency:      e.Currency,
		Status:        "paid",
		PaidAt:        &now,
		RawPayload:    e.RawPayload,
	}
	paymentID, _, err := insertPaymentOnTx(ctx, tx, p)
	if err != nil {
		return fmt.Errorf("insert renewal payment: %w", err)
	}

	if e.SubExpiresAt != nil {
		// The sub may be cancelled/expired since activation; in that case
		// we still INSERT the payment row (PayPal did charge) but we must
		// NOT extend expires_at. We audit-log when the UPDATE didn't fire
		// so operators see the "PayPal charging a sub our DB says is dead"
		// mismatch.
		res, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			SET expires_at = $1, updated_at = now()
			WHERE id = $2 AND status = 'active'
		`, *e.SubExpiresAt, sub.ID)
		if err != nil {
			return fmt.Errorf("extend expires_at on renewal: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 && sub.Status != "active" {
			_ = writeAuditOnTx(ctx, tx, "service", "paypal_renewal_sub_not_active",
				fmt.Sprintf("subscription:%s", sub.ID),
				[]string{"webhook", "paypal", "renewal", "sub_not_active"},
				map[string]any{
					"payment_id":               paymentID,
					"order_id":                 orderID,
					"external_subscription_id": e.ExternalSubscriptionID,
					"sub_status":               sub.Status,
				})
		}
	} else {
		// PayPal charged the customer but didn't ship a next_billing_time
		// hint. Recording the payment without extending the subscription
		// would leave a paying customer without access — silently fail and
		// let the operator investigate. Audit-log loudly so the
		// reconciliation job (or ops) can match the payment to a manual
		// subscription fix-up.
		_ = writeAuditOnTx(ctx, tx, "service", "paypal_renewal_no_expiry_hint",
			fmt.Sprintf("subscription:%s", sub.ID),
			[]string{"webhook", "paypal", "renewal", "no_expiry_hint"},
			map[string]any{
				"payment_id":               paymentID,
				"order_id":                 orderID,
				"external_subscription_id": e.ExternalSubscriptionID,
				"amount":                   e.Amount,
				"currency":                 e.Currency,
			})
	}

	if err := writeAuditOnTx(ctx, tx, "service", "paypal_subscription_renewed",
		fmt.Sprintf("subscription:%s", sub.ID),
		[]string{"payment", "paypal", "renewal"},
		map[string]any{
			"payment_id":               paymentID,
			"order_id":                 orderID,
			"external_subscription_id": e.ExternalSubscriptionID,
			"amount":                   e.Amount,
			"currency":                 e.Currency,
		}); err != nil {
		return fmt.Errorf("audit renewal: %w", err)
	}

	return tx.Commit()
}

// ============================================================================
// Internal helpers — transaction-scoped SQL for cross-repo atomicity.
// ============================================================================

// insertPaymentOnTx does the business-level idempotency INSERT inside an
// existing transaction. Returns (paymentID, true) if inserted, (_, false)
// if a row already exists for (channel, external_txn_id).
func insertPaymentOnTx(ctx context.Context, tx dbTx, p *model.Payment) (string, bool, error) {
	rawPayload := repo.NonNilRawPayload(p.RawPayload)
	var paidAt *time.Time = p.PaidAt
	id, err := tx.QueryRowID(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (channel, external_txn_id) DO NOTHING
		RETURNING id
	`, p.OrderID, p.Channel, p.ExternalTxnID, p.Amount, p.Currency, p.Status, paidAt, rawPayload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
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
func activateSubscriptionOnTx(ctx context.Context, tx dbTx, userID, planID string, expiresAt *time.Time) (bool, error) {
	// Step 1: UPDATE the target row (active first, else most recent).
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET
			plan_id = $1,
			started_at = COALESCE(started_at, now()),
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

	// RowsAffected tells us whether the UPDATE hit a row (vs no rows
	// existed at all) — no need for the follow-up COUNT(*) round-trip.
	n, _ := res.RowsAffected()

	if n == 0 {
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
func writeAuditOnTx(ctx context.Context, tx dbTx, actor, action, target string, tags []string, ctxData map[string]any) error {
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
func (s *PaymentService) findOrInsertPendingOnTx(ctx context.Context, tx dbTx, e WebhookEvent) (*model.Payment, error) {
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
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES (:id, :order_id, :channel, :external_txn_id, :amount, :currency, :status, :raw_payload)
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
	case "stripe", "wechat_pay", "alipay", "paypal":
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInvalidChannel, channel)
	}
}

// providerPreAuth is the channel-specific configuration gate. A return
// value other than nil means the deployment refuses to mint the pre-auth
// artifact for the requested channel; CreateOrder must reject the
// request BEFORE persisting the order row. Today the only check is the
// WeChat Pay NATIVE pre-auth (wechat == nil ⇒ deployment has no client
// to mint a code_url against). Adding a new channel means adding a new
// branch here so the same "no orphan pending order" invariant holds.
//
// The function deliberately does not touch the database — the gate is
// pure deployment configuration, so it can run before any plan/sub
// lookups. Order matters only relative to validateChannel: an unknown
// channel is already rejected there.
func (s *PaymentService) providerPreAuth(channel string) error {
	if channel == "wechat_pay" {
		if s.wechat == nil {
			return ErrWechatPayNotConfigured
		}
	}
	// Other channels either don't need an upstream pre-auth (stripe uses
	// Elements + intent, alipay uses the page-redirect pattern) or wire
	// their client unconditionally. Add a new branch here when that
	// changes.
	return nil
}

// eligibilityAndInsertOrderTx runs the plan eligibility check
// (existence, is_active, accepting_new_subscriptions, currency match)
// AND the order INSERT inside a single transaction. The plan row is
// locked with FOR SHARE for the duration of the tx so a concurrent
// plan deactivation can't race past the check and leave an order
// pointing at an inactive plan. Order is left at "pending" and the
// caller (CreateOrder) drives the post-commit pre-auth (WeChat
// UnifiedOrder) outside the tx; pre-auth is an external HTTP call and
// must NOT run inside an open tx.
//
// On any returned error no order row exists (the tx rolled back).
//
// The transactional boundary is owned by PlanRepo.WithTx — the same
// helper PlanService.CreatePlan/UpdatePlan/DeletePlan use for their
// D8 mutations. Driving the tx through PlanRepo.WithTx makes the
// "eligibility reads + order INSERT commit together" guarantee part
// of the repo contract: callers can't opt out by skipping the wrapper
// (the previous s.db.BeginTxx path was easy to bypass and the
// pre-D8 no-tx fallback made it easy to ship a regression). The
// repo implementation owns begin/commit/rollback; the closure here
// only threads the tx through FindByIDForShareTx and CreateInTx.
func (s *PaymentService) eligibilityAndInsertOrderTx(ctx context.Context, userID, planID, channel string, out **model.Order) error {
	var order *model.Order
	err := s.planRepo.WithTx(ctx, func(tx *sqlx.Tx) error {
		plan, err := s.planRepo.FindByIDForShareTx(ctx, tx, planID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPlanNotFound
			}
			return fmt.Errorf("lock plan: %w", err)
		}
		if !plan.IsActive {
			return ErrPlanInactive
		}
		if !plan.AcceptingNewSubscriptions {
			return ErrPlanNotAcceptingNew
		}
		if required, ok := channelRequiredCurrency[channel]; ok && plan.Currency != required {
			return ErrPlanCurrencyMismatch
		}

		order = &model.Order{
			ID:     GenerateUUID(),
			UserID: userID,
			PlanID: planID,
			// ApplyPlanAmountOverride mirrors the QuoteService.Get
			// override (price_override.go). The order row is the
			// authoritative source for the WeChat UnifiedOrder amount
			// fan-out — overriding only in QuoteService would leave
			// the actual charge still at the DB price, defeating the
			// test-mode intent. The override is per-plan-id, currency
			// preserved from plans.currency.
			Amount:    ApplyPlanAmountOverride(planID, plan.Price),
			Currency:  plan.Currency,
			Status:    "pending",
			ExpiresAt: time.Now().Add(s.orderExpiry),
		}
		if err := s.orderRepo.CreateInTx(ctx, tx, order); err != nil {
			return fmt.Errorf("create order: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	*out = order
	return nil
}

// TODO: refactor to per-channel predicate maps at 5+ channels — the flat
// switch lists are getting hard to scan as channels multiply.
func isPaymentSuccess(eventType string) bool {
	switch eventType {
	case "payment_intent.succeeded", "TRANSACTION.SUCCESS",
		"TRADE_SUCCESS", "trade_status_sync",
		"order_created", "subscription_created",
		"PAYMENT.CAPTURE.COMPLETED", "BILLING.SUBSCRIPTION.CREATED":
		return true
	}
	return false
}

func isPaymentFailed(eventType string) bool {
	switch eventType {
	case "payment_intent.payment_failed", "payment_intent.canceled",
		"TRANSACTION.PAY_FAILED", "TRANSACTION.REVOKED",
		"PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.FAILED":
		return true
	}
	return false
}

func isRefundEvent(eventType string) bool {
	switch eventType {
	case "charge.refunded", "TRANSACTION.REFUND",
		"TRADE_CLOSED", "trade_closed",
		"order_refunded", "subscription_payment_refunded",
		"PAYMENT.CAPTURE.REFUNDED", "PAYMENT.SALE.REFUNDED":
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

// isPaypalRenewal — handler-implementation detail lifted here because the
// OnWebhook dispatch table is in service. PAYMENT.SALE.COMPLETED is the
// auto-renewal charge PayPal fires when a subscription's billing period
// completes.
func isPaypalRenewal(eventType string) bool {
	return eventType == "PAYMENT.SALE.COMPLETED"
}

// resolveSubExpiry returns the expires_at to write on a subscription
// activation. Priority:
//
//  1. preserved expiry (retry path). When the caller detects that the
//     current call is a retry of an already-paid payment
//     (paymentRepo.FindByChannelTxnID returned a row with status='paid'),
//     the caller pre-fetches the active sub's expires_at (via
//     subRepo.FindActiveByUserIDTx, sharing this tx's connection) and
//     passes it here. This keeps activation idempotent: a webhook or
//     Confirm retry doesn't shift the expiry forward — and doesn't
//     double-apply the rollover below. Preserved wins over the hint:
//     the first activation already folded the hint (and any rollover)
//     into the stored value, so re-forwarding a fresh hint would
//     shorten or shift an already-extended sub.
//
//  2. caller-supplied hint (BFF on Confirm; webhook payload on channels
//     that ship sub_expires_at, e.g. Stripe metadata / PayPal renewal).
//     nil = no hint, fall through.
//
//  3. rollover (2026-07-28 upgrade/renewal rule). When this activation
//     REPLACES an unexpired active subscription, the remaining days
//     carry over: the new expiry extends from the OLD expires_at, not
//     from now(). Applies to same-plan renewal and longer-cycle
//     upgrades — CreateOrder's repurchase rule already limits order
//     creation to those two. A shorter-cycle replacement is a
//     downgrade and fails with ErrDowngradeActivationBlocked: the
//     payment is still honored (order goes paid, ops refunds), but the
//     subscription is left untouched. A missing current-plan row
//     (retired plan) can't be compared and is treated as non-downgrade.
//
//  4. plan.interval_days fallback. Real WeChat NATIVE v3 doesn't ship
//     sub_expires_at (verified 2026-07-27), so this fires for every
//     fresh WeChat charge unless the BFF forwards one via
//     /payments/orders/:order_id/confirm. Looked up via
//     planRepo.FindByIDForShareTx so the read shares the calling tx's
//     connection (otherwise with MaxOpenConns=25, 25 concurrent
//     fallback requests can deadlock waiting for a second connection).
//
//  5. nil (plan missing OR interval_days == 0). Caller decides: webhook
//     paths audit-log + write NULL; Confirm path mirrors the same shape.
func (s *PaymentService) resolveSubExpiry(
	ctx context.Context,
	tx *sqlx.Tx,
	userID, planID string,
	hint, preservedExpiry *time.Time,
) (*time.Time, error) {
	if preservedExpiry != nil {
		return preservedExpiry, nil
	}

	// Cap at ~290 years to keep the time.Duration multiply well below
	// int64 nanosecond overflow. Plan.IntervalDays is operator-controlled;
	// a defensive check prevents a typo from turning into a wildly past
	// or future expires_at.
	const maxIntervalDays = 365 * 290

	// The new plan row is loaded lazily and at most once: the fallback
	// branch needs its interval, and the rollover branch needs it for
	// the downgrade comparison. The hint-only path with no active sub
	// keeps its historical plan-free semantics (TestResolveSubExpiry_
	// HintForwarded).
	var plan *model.Plan
	var planErr error
	planLoaded := false
	loadPlan := func() (*model.Plan, error) {
		if planLoaded {
			return plan, planErr
		}
		planLoaded = true
		p, err := s.txLookupPlan(ctx, tx, planID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				planErr = ErrPlanMissingForExpiry
			} else {
				planErr = fmt.Errorf("find plan for expiry fallback: %w", err)
			}
			return nil, planErr
		}
		plan = p
		return plan, nil
	}

	var candidate *time.Time
	if hint != nil {
		c := *hint
		candidate = &c
	}

	existing, sErr := s.txLookupActiveSubscription(ctx, tx, userID)
	if sErr != nil && !errors.Is(sErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("find active sub for rollover: %w", sErr)
	}
	// The downgrade guard covers every replacement of an UNEXPIRED sub —
	// including the two edge shapes that carry no rollable date:
	// interval_days=0 plans (lifetime/free; 0 is the SMALLEST interval
	// here, so a stale lifetime order paid after an upgrade is blocked
	// by the same comparison) and expires_at=NULL rows (never-expire).
	// Rollover itself needs a concrete old expires_at AND a positive
	// new interval; anything else just skips the extension.
	if existing != nil && (existing.ExpiresAt == nil || existing.ExpiresAt.After(time.Now())) {
		p, err := loadPlan()
		if err != nil {
			return nil, err
		}
		oldPlan, oErr := s.txLookupPlan(ctx, tx, existing.PlanID)
		if oErr != nil && !errors.Is(oErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("find current plan for rollover: %w", oErr)
		}
		if oldPlan != nil && oldPlan.IntervalDays > p.IntervalDays {
			return nil, ErrDowngradeActivationBlocked
		}
		if existing.ExpiresAt != nil && p.IntervalDays > 0 {
			if p.IntervalDays > maxIntervalDays {
				return nil, fmt.Errorf("plan %s interval_days=%d exceeds %d-day cap", planID, p.IntervalDays, maxIntervalDays)
			}
			// max(): a hint that already sits beyond the rolled value
			// still wins. Note this intentionally prefers the rolled
			// value over a same-plan renewal hint (e.g. a BFF quote for
			// a renewal Confirm) — "extend the current expiry" is the
			// product rule; the channel-side billing anchor (PayPal
			// renewals run their own onPaypalRenewalSucceeded path and
			// never reach here) is unaffected.
			rolled := existing.ExpiresAt.Add(time.Duration(p.IntervalDays) * 24 * time.Hour)
			if candidate == nil || rolled.After(*candidate) {
				candidate = &rolled
			}
		}
	}

	if candidate != nil {
		return candidate, nil
	}

	p, err := loadPlan()
	if err != nil {
		return nil, err
	}
	if p.IntervalDays <= 0 {
		return nil, nil
	}
	if p.IntervalDays > maxIntervalDays {
		return nil, fmt.Errorf("plan %s interval_days=%d exceeds %d-day cap", planID, p.IntervalDays, maxIntervalDays)
	}
	t := time.Now().Add(time.Duration(p.IntervalDays) * 24 * time.Hour)
	return &t, nil
}

// buildReconcileWebhookEvent builds the WebhookEvent that the active
// reconciliation path (`reconcileFromChannel` in GetOrder) feeds into
// OnWebhook when WeChat's QueryOrder reports the order as SUCCESS.
//
// SubExpiresAt is intentionally LEFT NIL. WeChat's QueryOrder response
// carries `success_time` (when the payment settled — a moment in the
// past), not a subscription expiry. Reusing it as SubExpiresAt would
// write subscriptions.expires_at = <past>, and the auth path's
// `findUsableSubscription` would refuse the next login with
// ErrSubscriptionExpired even though the user just paid (real-world
// observed in cn-staging 2026-07-23).
//
// Sub-expiry is computed downstream by onPaymentSucceeded's
// resolveSubExpiry helper, which falls back to plan.interval_days when
// no hint is provided. Pre-fix behavior was "never expires"; post-fix
// behavior is "now() + plan.interval_days*24h" — same shape as the
// BFF-confirmed Confirm path.
//
// `res` is checked for nil to keep the helper safe to call from tests
// that exercise the failure-shape (QueryOrder returning nil) without
// going through reconcileFromChannel's earlier `res == nil` early-out.
func buildReconcileWebhookEvent(res *wechat.OrderQueryResult) (WebhookEvent, error) {
	if res == nil {
		return WebhookEvent{}, errors.New("nil query result")
	}
	if res.TradeState != "SUCCESS" {
		// Reconcile only invokes the webhook path on SUCCESS; for other
		// states callers should use the throttle + last_reconciled_at
		// update and return early. Guard the helper too so a future
		// caller can't accidentally promote a NOTPAY into a paid event.
		return WebhookEvent{}, fmt.Errorf("build reconcile: trade_state=%q, want SUCCESS", res.TradeState)
	}
	return WebhookEvent{
		Channel:       "wechat_pay",
		EventID:       "reconcile:" + res.OutTradeNo + ":" + res.TransactionID,
		EventType:     "TRANSACTION.SUCCESS",
		TransactionID: res.TransactionID,
		OrderID:       res.OutTradeNo,
		Amount:        float64(res.Amount.Total) / 100,
		Currency:      res.Amount.Currency,
		// SubExpiresAt is nil on purpose — see the comment above. Do not
		// populate from res.SuccessTime (it's a past timestamp).
	}, nil
}

// reconcilePreCheck decides whether reconcileFromChannel should
// short-circuit before calling OnWebhook. The real WeChat webhook and
// the reconcile-synthesized event use different EventID shapes (real:
// WeChat's evt.ID UUID; reconcile: "reconcile:" + out_trade_no + ":" +
// transaction_id), so the (channel, event_id) unique constraint in
// webhook_events does NOT catch a real webhook that arrives later. The
// (channel, external_txn_id) unique constraint in payments catches the
// payment-row side, but the subscribe path in onPaymentSucceeded
// falls through on duplicate-payment to a re-activation UPSERT —
// functionally a no-op today, but architecturally wrong. This helper
// lets the reconcile path exit early when a paid payment row for the
// same WeChat transaction already exists, so OnWebhook never even
// fires for a transaction that a real webhook has already settled.
//
// Inputs:
//   - existing: the row returned by paymentRepo.FindByChannelTxnID
//     (nil on sql.ErrNoRows)
//   - findErr: the error from FindByChannelTxnID
//
// Outputs:
//   - skip=true: caller stamps last_reconciled_at and returns.
//   - skipErr != nil: caller returns wrapped as the function's error
//     so the FE poll retries.
//   - both zero: continue to OnWebhook.
//
// A query error OTHER than sql.ErrNoRows (e.g. DB unreachable) is
// treated as an error and bubbles up — the FE poll will retry. We
// deliberately don't translate every error into "skip" because doing
// so would mask real outages behind a silent success.
func reconcilePreCheck(existing *model.Payment, findErr error) (skip bool, skipErr error) {
	if findErr != nil {
		// sql.ErrNoRows is the only "no row" signal; everything else
		// is a DB error worth surfacing.
		if errors.Is(findErr, sql.ErrNoRows) {
			return false, nil
		}
		return false, findErr
	}
	if existing == nil {
		return false, nil
	}
	if existing.Status == "paid" {
		return true, nil
	}
	// Payment row exists but isn't 'paid' (e.g. 'failed' / 'pending').
	// Fall through to OnWebhook so the reconcile can drive the state
	// transition.
	return false, nil
}

func mustJSON(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// toCents converts a major-units float64 (DECIMAL(10,2) round-trip) to
// integer cents. Used for exact monetary comparisons that must not
// suffer float round-trip drift (refund full-vs-partial detection).
// Non-finite or out-of-range values return math.MinInt64/0 to fail
// comparisons safely.
func toCents(v float64) int64 {
	if v != v { // NaN
		return 0
	}
	c := int64(v * 100)
	if v > 0 && c < 0 {
		return 1<<62 - 1 // overflow clamp
	}
	return c
}
