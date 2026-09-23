package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
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

// ErrConfirmVerificationUnavailable is returned by Confirm for channels
// with no wired server-side order-query client (stripe / paypal / alipay
// today — their channel clients are stubs). Confirm must not mark an
// order paid on the caller's word alone; the channel webhook settles it.
var ErrConfirmVerificationUnavailable = errors.New("payment confirmation is unavailable for this channel")

// ErrConfirmNotVerified is returned by Confirm when the channel's
// server-side query does not show a settled payment matching the order
// (out_trade_no / transaction id / amount / currency). Terminal for the
// caller — retrying the same claim will fail again until the channel
// actually settles the order.
var ErrConfirmNotVerified = errors.New("payment not confirmed by the channel")

// ErrPlanMissingForExpiry is returned by resolveSubExpiry when the plan row
// was deleted between order creation and webhook/confirm arrival. Callers
// audit-log this and skip subscription activation: with the RESTRICT FKs on
// orders.plan_id / subscriptions.plan_id the state is only reachable via
// manual DB surgery, and no INSERT/UPDATE can reference a missing plan.
var ErrPlanMissingForExpiry = errors.New("plan missing for sub-expiry fallback")

// ErrDowngradeActivationBlocked is returned by resolveSubExpiry when a
// paid order would REPLACE an unexpired active subscription with a
// shorter-interval plan (e.g. a stale monthly QR paid after the user
// upgraded to yearly). The payment is still honored — the order goes
// paid and ops refunds manually — but the subscription is left
// untouched and the call sites audit-log "downgrade_activation_blocked".
var ErrDowngradeActivationBlocked = errors.New("activation blocked: would downgrade an active longer-cycle subscription")

// ErrPlanNotPurchasable is returned by CreateOrder (and the quote gate)
// when a coding-plan plan has no plan_benefit_configs row — the product's
// "payment configuration" (设计 §4.3: 没有配置的商品保持草稿或不可购买).
// Without the mapping the service could not grant anything on payment, so
// the order is refused up front instead of minting an orphan.
var ErrPlanNotPurchasable = errors.New("plan is not purchasable: no payment/benefit configuration")

// ErrPlanUpgradeNotConfigured is returned by CreateOrder when a coding-plan
// order would cross tiers without a plan_upgrade_rules row (设计 §4.2:
// 商品未配置升级报价规则时不开放即时跨档升级). The legacy
// "longer-interval is an upgrade" rule is NOT applied to coding-plan —
// only an explicit configured rule allows a cross-plan order there.
var ErrPlanUpgradeNotConfigured = errors.New("cross-tier upgrade is not configured for this plan pair")

// ErrOrderActivationConflict is returned by the coding-plan activation
// resolver when a PAID order conflicts with a different active plan at
// activation time (e.g. a stale order paid after the user changed tiers).
// Same handling shape as ErrDowngradeActivationBlocked: the payment is
// honored, the subscription/entitlement stay untouched, and the call
// sites audit-log "activation_conflict_blocked" for ops to refund.
var ErrOrderActivationConflict = errors.New("activation blocked: order conflicts with the current active plan")

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

	// confirmVerifier, when non-nil, replaces the upstream verification
	// Confirm runs before marking an order paid. Production leaves it nil
	// (verifyConfirmPayment's built-in dispatch: WeChat QueryOrder, other
	// channels rejected); tests install a double so the post-verification
	// pipeline (rollover, dedupe, retry) can be exercised without an
	// upstream.
	confirmVerifier func(ctx context.Context, order *model.Order, channel, externalTxnID string) error

	// orderExpiry drives the order's default expires_at. Service layer
	// sets this on INSERT (SQL DEFAULT is also 30 min; setting explicitly
	// makes it configurable without re-migrating).
	orderExpiry time.Duration

	// benefitRepo reads the plan→benefit mapping and configured upgrade
	// rules (migration 029). Nil in deployments/tests without the
	// inference module: coding-plan orders are then refused at the
	// benefit-config gate (fail-closed), kaya orders are unaffected.
	benefitRepo repo.PlanBenefitRepo

	// benefitSync enqueues entitlement-sync messages into
	// inference_outbox INSIDE the payment transaction (同事务 outbox,
	// Task 10): the message commits iff the payment state transition
	// commits, and the entitlement-sync worker converges idempotently.
	// Nil = feature off (unit tests); production wires it always.
	benefitSync BenefitSyncOutbox
}

// BenefitSyncOutbox is the narrow surface PaymentService needs from the
// inference module: enqueue one outbox row into the caller's *sqlx.Tx.
// Implemented by inference/postgres.Store.
type BenefitSyncOutbox interface {
	EnqueueOutboxSQLTx(ctx context.Context, tx *sqlx.Tx, topic string, payload json.RawMessage, dedupKey *string) (int64, error)
}

// SetBenefitRepo wires the plan benefit/upgrade-rule reads (migration 029).
// Production calls this unconditionally; without it coding-plan orders are
// refused (fail-closed) and kaya orders behave exactly as before.
func (s *PaymentService) SetBenefitRepo(r repo.PlanBenefitRepo) { s.benefitRepo = r }

// SetBenefitSync wires the transactional outbox enqueue used to drive
// entitlement grants after payment state transitions.
func (s *PaymentService) SetBenefitSync(o BenefitSyncOutbox) { s.benefitSync = o }

// enqueueBenefitSync appends one entitlement-sync message inside the
// caller's payment transaction. A nil benefitSync (unit tests) or a fake
// tx skips silently; a real enqueue failure aborts the caller's
// transaction so the payment transition and its outbox message commit or
// roll back together.
func (s *PaymentService) enqueueBenefitSync(ctx context.Context, tx dbTx, msg access.EntitlementSyncMessage, dedupKey *string) error {
	if s.benefitSync == nil {
		return nil
	}
	raw := rawSQLXTx(tx)
	if raw == nil {
		return nil
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal benefit sync message: %w", err)
	}
	if _, err := s.benefitSync.EnqueueOutboxSQLTx(ctx, raw, access.TopicEntitlementSync, payload, dedupKey); err != nil {
		return fmt.Errorf("enqueue benefit sync: %w", err)
	}
	return nil
}

// enqueueWalletSync appends one wallet-sync message inside the caller's
// payment transaction (Task 14; same 同事务 outbox contract as
// enqueueBenefitSync, on the wallet.sync topic).
func (s *PaymentService) enqueueWalletSync(ctx context.Context, tx dbTx, msg access.WalletSyncMessage, dedupKey *string) error {
	if s.benefitSync == nil {
		return nil
	}
	raw := rawSQLXTx(tx)
	if raw == nil {
		return nil
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal wallet sync message: %w", err)
	}
	if _, err := s.benefitSync.EnqueueOutboxSQLTx(ctx, raw, access.TopicWalletSync, payload, dedupKey); err != nil {
		return fmt.Errorf("enqueue wallet sync: %w", err)
	}
	return nil
}

// walletTopupMicros converts the order's frozen amount (decimal major
// units — the payment boundary contract stays as-is) into wallet micros,
// strictly, without float accumulation (设计 §7.1: 进入模型账本时严格转换).
func walletTopupMicros(amount float64, currency string) (int64, error) {
	return accounting.MicrosFromDecimalMajor(strconv.FormatFloat(amount, 'f', 6, 64), currency)
}

// enqueueWalletTopup enqueues the cash top-up for one settled wallet-topup
// order (dedup 钉 payment_id；消费侧按 wallet:topup:{payment_id} 业务键幂
// 等入账，重复回调/Confirm/webhook/补单竞争只入账一次).
func (s *PaymentService) enqueueWalletTopup(ctx context.Context, tx dbTx, order *model.Order, paymentID string) error {
	micros, err := walletTopupMicros(order.Amount, order.Currency)
	if err != nil {
		return fmt.Errorf("wallet topup amount: %w", err)
	}
	dedup := access.WalletTopupDedupKey(paymentID)
	return s.enqueueWalletSync(ctx, tx, access.WalletSyncMessage{
		Kind: access.WalletSyncTopup, UserID: order.UserID, OrderID: order.ID,
		PaymentID: paymentID, AmountMicros: micros, Currency: order.Currency,
	}, &dedup)
}

// enqueueWalletRefund enqueues the cash refund of one wallet-topup payment
// (dedup 钉 refund_id；消费侧 wallet:refund:{refund_id} 幂等). 退款只允许
// 现金来源原路退——赠送余额由钱包侧 CHECK/纯规则双兜底排除。
func (s *PaymentService) enqueueWalletRefund(ctx context.Context, tx dbTx, order *model.Order, paymentID, refundID string, amount float64) error {
	micros, err := walletTopupMicros(amount, order.Currency)
	if err != nil {
		return fmt.Errorf("wallet refund amount: %w", err)
	}
	dedup := access.WalletRefundDedupKey(refundID)
	return s.enqueueWalletSync(ctx, tx, access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: order.UserID, OrderID: order.ID,
		PaymentID: paymentID, RefundID: refundID, AmountMicros: micros, Currency: order.Currency,
	}, &dedup)
}

// enqueueWalletFailedDebit enqueues the cash reversal of one wallet-topup
// payment that FAILED after reaching paid（评审批次7 Important-3：paid 之后
// 来 payment_failed，支付行翻 failed 但已入账现金仍可花）。与
// enqueueWalletRefund 同一现金借记语义，但触发方没有退款行——dedup 键与
// 消费侧业务键都钉在 payment id 上（与 benefit:failed:{payment_id} 同一锚
// 点），渠道重投只入队/入账一次。amount 为应冲正差额（评审轮2 N3：订单全
// 额扣除该支付已 paid 退款后的余额，由调用方计算并保证 > 0）——部分退款
// 后按全额冲正会把已退回的钱再追一遍。
func (s *PaymentService) enqueueWalletFailedDebit(ctx context.Context, tx dbTx, order *model.Order, paymentID string, amount float64) error {
	micros, err := walletTopupMicros(amount, order.Currency)
	if err != nil {
		return fmt.Errorf("wallet failed-payment debit amount: %w", err)
	}
	dedup := "wallet:failed:" + paymentID
	return s.enqueueWalletSync(ctx, tx, access.WalletSyncMessage{
		Kind: access.WalletSyncRefund, UserID: order.UserID, OrderID: order.ID,
		PaymentID: paymentID, RefundID: "payment-failed:" + paymentID,
		AmountMicros: micros, Currency: order.Currency,
	}, &dedup)
}

// orderTouchesBenefits reports whether paying/refunding/failing this order
// can move an entitlement: either the order froze a benefit snapshot
// (post-029 benefit-bearing products) or it belongs to the coding-plan
// product (whose entitlement must be re-converged even when the individual
// order — e.g. a synthetic renewal row — carries no snapshot).
func orderTouchesBenefits(o *model.Order, productCode string) bool {
	return o.BenefitPolicyVersionID != nil || productCode == model.ProductCodingPlan
}

// deriveMerchantRefundNo 从客户端幂等键确定性派生商户退款单号（评审轮2
// N2）。微信/支付宝以商户退款单号为退款幂等键：同键重试必得同号，渠道侧
// 幂等兜底崩溃重试窗口的双退款。"mrn_" + sha256 hex 前 40 位 = 44 字符，
// 低于渠道 64 字符单号上限。
func deriveMerchantRefundNo(idempotencyKey string) string {
	sum := sha256.Sum256([]byte("yunhou-refund:" + idempotencyKey))
	return "mrn_" + hex.EncodeToString(sum[:])[:40]
}

// RefundAPI is the channel-side refund call. The service is the caller;
// the channel client is injected so production swaps in real HTTP and
// tests swap in a stub.
type RefundAPI interface {
	// Refund issues a refund on the channel. merchantRefundNo 是商户退款
	// 单号（微信 out_refund_no / 支付宝 out_biz_no / Stripe metadata），由
	// 调用方生成并持久化为 refunds.external_refund_id —— 渠道退款
	// webhook 以同一商户单号对账（评审批次7 Important-2：两侧键必须一
	// 致，否则 webhook 的 ON CONFLICT 重读永远 miss，API 行卡 pending）。
	// 返回渠道回显的商户退款单号（仅供调用方日志核对；评审轮2 M1 起调用
	// 方一律以已发送单号为对账键入库，回显不再覆盖）。渠道不回显时返回
	// 空串。idempotencyKey is forwarded to the channel (Stripe supports
	// this header; others ignore).
	Refund(ctx context.Context, channel, externalTxnID, merchantRefundNo string, amount float64, idempotencyKey string) (echoedMerchantRefundNo string, err error)
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

// txLookupActiveSubscription reads the user's current active sub for one
// product, sharing the surrounding tx's connection when one is in flight.
// Returns nil/nil when no active sub exists (sql.ErrNoRows is swallowed at
// the call site for retry-preservation lookups). productCode comes from the
// order's plan row (never from the user's other subscriptions); an empty
// productCode (plan row missing — activation will be skipped via
// ErrPlanMissingForExpiry) matches no rows, which is the safe outcome.
func (s *PaymentService) txLookupActiveSubscription(ctx context.Context, tx *sqlx.Tx, userID, productCode string) (*model.Subscription, error) {
	if tx != nil {
		return s.subRepo.FindActiveByUserAndProductTx(ctx, tx, userID, productCode)
	}
	return s.subRepo.FindActiveByUserAndProduct(ctx, userID, productCode)
}

// resolveOrderProduct prefers the order's frozen product snapshot
// (migration 029) and falls back to the live plan row for orders created
// before 029 (their snapshot columns are NULL by definition).
func (s *PaymentService) resolveOrderProduct(ctx context.Context, tx *sqlx.Tx, order *model.Order) (string, error) {
	if pc := order.SnapshotProductCode(); pc != "" {
		return pc, nil
	}
	return s.productCodeForPlan(ctx, tx, order.PlanID)
}

// resolveOrderActivation computes the expires_at a PAID order writes onto
// the subscription, choosing the resolver by what the order froze at
// creation time (migration 029):
//
//   - coding-plan orders (OrderKind set): access.ResolveCodingPlanActivation
//     over the ORDER SNAPSHOT — the product's own tier/cycle rules, no live
//     plan reads, conflicts return ErrOrderActivationConflict;
//   - legacy orders (no kind): resolveSubExpiry unchanged, except the new
//     plan's interval comes from the order snapshot when present (调价/
//     周期调整不影响已下单订单).
//
// Returned errors the call sites treat as "audit + honor payment + skip
// activation": ErrPlanMissingForExpiry, ErrDowngradeActivationBlocked,
// ErrOrderActivationConflict.
func (s *PaymentService) resolveOrderActivation(ctx context.Context, tx *sqlx.Tx, order *model.Order, orderProduct string, hint, preservedExpiry *time.Time) (*time.Time, error) {
	if order.SnapshotOrderKind() != "" {
		// Same retry short-circuit as the legacy path: a re-delivered
		// already-paid payment preserves the active sub's expiry instead
		// of rolling it forward a second time.
		if preservedExpiry != nil {
			return preservedExpiry, nil
		}
		existing, err := s.txLookupActiveSubscription(ctx, tx, order.UserID, orderProduct)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find active sub for coding-plan activation: %w", err)
		}
		var current *access.SubscriptionState
		if existing != nil {
			current = &access.SubscriptionState{
				ID: existing.ID, UserID: existing.UserID, PlanID: existing.PlanID,
				ProductCode: existing.ProductCode, Status: existing.Status, ExpiresAt: existing.ExpiresAt,
			}
		}
		act, err := access.ResolveCodingPlanActivation(order.SnapshotOrderKind(), order.SnapshotIntervalDays(),
			order.UpgradeFromPlanID, order.PlanID, current, hint, time.Now())
		if err != nil {
			return nil, err
		}
		if act.Blocked != "" {
			return nil, fmt.Errorf("%w: %s", ErrOrderActivationConflict, act.Blocked)
		}
		return act.ExpiresAt, nil
	}
	return s.resolveSubExpiry(ctx, tx, order.UserID, order.PlanID, orderProduct, hint, preservedExpiry, order.SnapshotIntervalDays())
}

// productCodeForPlan resolves the commercial product a plan belongs to.
// A missing plan row returns ("", nil): the plan-missing case is reported
// downstream by resolveSubExpiry (ErrPlanMissingForExpiry), and an empty
// product code makes the product-scoped subscription lookups return no
// rows — the same shape as "user has no active sub in this product".
func (s *PaymentService) productCodeForPlan(ctx context.Context, tx *sqlx.Tx, planID string) (string, error) {
	plan, err := s.txLookupPlan(ctx, tx, planID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return plan.ProductCode, nil
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

	// Enforce the partial unique index `UNIQUE(user_id, product_code) WHERE
	// status='active'` (idx_subscriptions_user_product_active, migration 027)
	// at the order layer. Without this pre-check, a concurrent order + activate
	// would hit the constraint at INSERT time and surface as a 500; the user
	// gets a clean 409 instead. The DB invariant IS the primitive — this is
	// just a friendly surface for it.
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
	// Product scope (migration 027): the pre-check only considers an active
	// subscription in the REQUESTED PLAN's product — an active
	// kaya-membership sub must not block a coding-plan order and vice
	// versa. The product is resolved from the plan row, never inferred
	// from the user's existing subscriptions. A missing requested plan
	// skips the pre-check entirely; eligibilityAndInsertOrderTx then
	// returns ErrPlanNotFound, matching the pre-027 outcome for unknown
	// plans (repurchaseAllowed treated them as allowed and let the tx
	// produce the real error).
	//
	// Coding Plan divergence (migration 029, design §4.2): coding-plan
	// orders do NOT use the interval-comparison repurchase rule below.
	// Their order kind is frozen here: same plan = renewal, different
	// plan requires an explicit plan_upgrade_rules row (otherwise
	// ErrPlanUpgradeNotConfigured), no active sub = new. The kind +
	// upgrade-from plan ride the order row so activation revalidates
	// against the live subscription at payment time.
	//
	// "active" here means status='active' AND the sub has not lapsed
	// (expires_at NULL or future). A stale row (status='active' with
	// expires_at < now()) is treated as expired and permitted through;
	// the upcoming activateSubscriptionOnTx will UPDATE that row in
	// place rather than create a new one, preserving the partial
	// unique index. Without this carve-out, users whose subscription
	// quietly went past could not renew even after the cn-staging
	// 2026-07-23 login-decouple fix let them log in.
	requestedPlan, perr := s.planRepo.FindByID(ctx, planID)
	if perr != nil && !errors.Is(perr, sql.ErrNoRows) {
		return nil, fmt.Errorf("find requested plan: %w", perr)
	}
	// orderKind/upgradeFromPlanID are the frozen upgrade-rule outcome for
	// coding-plan orders (migration 029). Kaya-membership orders keep both
	// empty — the legacy interval-comparison rules drive them verbatim.
	var orderKind string
	var upgradeFromPlanID *string
	if requestedPlan != nil {
		if requestedPlan.ProductCode == model.ProductCodingPlan && s.benefitRepo == nil {
			// Fail-closed: without the benefit-config read surface the
			// service cannot prove the plan is purchasable (设计 §4.3).
			return nil, ErrPlanNotPurchasable
		}
		if existing, err := s.subRepo.FindActiveByUserAndProduct(ctx, userID, requestedPlan.ProductCode); err == nil {
			if existing.ExpiresAt == nil || existing.ExpiresAt.After(time.Now()) {
				// PayPal 订阅制与 WeChat 的根本差异：渠道侧自动续费
				// （PAYMENT.SALE.COMPLETED webhook 延期），用户无需也不应手动
				// "续费"。这里每放行一单，BFF 就在 PayPal 创建一个全新的
				// subscription 对象（重新吃 plan 内嵌的 trial），而旧订阅仍在
				// 自动扣费 → 双重扣费（2026-08-17 intl-staging 验收实测同一
				// 用户 3 个 ACTIVE PayPal 订阅并存、到期叠到两个月后）。
				// 改签（月↔年）需要专门的"取消旧订阅+建新订阅"流程，落地前
				// PayPal 渠道对任何未过期 active 订阅一律拒绝新单（409）。
				// WeChat 无自动续费，手动续费 rollover 是正确行为，不受影响。
				// trial 订阅是 OAuth 首登赠予的（migration 018），**不是**
				// PayPal 订阅：渠道侧没有对应的自动扣费 subscription，豁免
				// 它不会造成双重扣费，反而正是 trial→付费 的核心转化漏斗
				// （review users-1, 2026-08-17）。
				if channel == "paypal" && existing.PlanID != "trial" {
					return nil, ErrUserHasActiveSub
				}
				if requestedPlan.ProductCode == model.ProductCodingPlan {
					// Coding Plan 自身档位规则（设计 §4.2）：同套餐 = 续费；
					// 跨套餐必须有 plan_upgrade_rules 显式规则行，否则 409。
					// 不用"周期更长即可升级"的会员旧逻辑推导新商品。
					if existing.PlanID == planID {
						orderKind = model.OrderKindRenewal
					} else {
						ok, rerr := s.benefitRepo.HasUpgradeRule(ctx, existing.PlanID, planID)
						if rerr != nil {
							return nil, fmt.Errorf("check upgrade rule: %w", rerr)
						}
						if !ok {
							return nil, ErrPlanUpgradeNotConfigured
						}
						orderKind = model.OrderKindUpgrade
						from := existing.PlanID
						upgradeFromPlanID = &from
					}
				} else {
					allowed, aerr := s.repurchaseAllowed(ctx, existing.PlanID, planID)
					if aerr != nil {
						return nil, aerr
					}
					if !allowed {
						return nil, ErrPlanDowngrade
					}
				}
			}
			// stale: status='active' but expires_at < now(). Allow order
			// creation — activateSubscriptionOnTx will update this row.
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("check active sub: %w", err)
		}
		if requestedPlan.ProductCode == model.ProductCodingPlan && orderKind == "" {
			orderKind = model.OrderKindNew
		}
	}

	var order *model.Order
	err := s.eligibilityAndInsertOrderTx(ctx, userID, planID, channel, orderKind, upgradeFromPlanID, &order)
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
// Confirm (caller-initiated payment confirmation, upstream-verified)
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
	//
	// ExpiresAt is accepted for API compatibility but IGNORED: a
	// caller-supplied subscription expiry is untrusted (a caller could
	// grant themselves a longer subscription than the plan grants).
	// Activation derives expires_at from the plan via resolveSubExpiry,
	// the same trust model as SubscriptionService.Create.
	ExpiresAt *time.Time
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

	// Upstream verification (2026-08 trust-model fix): the caller's claim
	// of payment is NOT trusted. The channel must independently confirm
	// the transaction server-side (WeChat QueryOrder: SUCCESS + matching
	// out_trade_no / transaction id / amount / currency) before anything
	// is marked paid. Channels without a wired query client are rejected
	// with ErrConfirmVerificationUnavailable — their orders settle via
	// the signature-verified webhook instead.
	if err := s.verifyConfirmPayment(ctx, order, in.Channel, in.ExternalTxnID); err != nil {
		return nil, err
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

	// Product scope (migration 027): every subscription read/write below is
	// scoped to the product of the ORDER'S plan. Migration 029: prefer the
	// order's frozen product snapshot; the live plan row is the pre-029
	// fallback. An empty product (plan row missing) is safe: product-scoped
	// lookups return no rows and the resolver surfaces
	// ErrPlanMissingForExpiry, which the audit-and-skip branch handles.
	orderProduct, pcErr := s.resolveOrderProduct(ctx, txSQLX, order)
	if pcErr != nil {
		return nil, fmt.Errorf("resolve order product: %w", pcErr)
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

		activeSub, sErr := s.txLookupActiveSubscription(ctx, txSQLX, order.UserID, orderProduct)
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

	// 余额充值商品（Task 14）：不激活订阅、不发模型权益——同事务入队钱包
	// 充值消息（dedup 钉 payment_id；消费侧按 wallet:topup:{payment_id}
	// 业务键幂等入账，重复回调不重复入账）。充值金额不作为订阅有效期。
	activated := false
	if orderProduct == model.ProductWalletTopup {
		if err := s.enqueueWalletTopup(ctx, tx, order, paymentID); err != nil {
			return nil, err
		}
	} else {

		// Activate subscription (UPSERT single-row, webhook doc §5.3).
		// expires_at resolution mirrors the webhook path, but the hint is
		// ALWAYS nil here: the caller-supplied ExpiresAt is untrusted and
		// ignored (a caller could otherwise extend their own subscription
		// past what the plan grants). The resolver falls back to the order's
		// snapshot interval so channels whose upstream payload doesn't ship
		// sub_expires_at (real WeChat v3 NATIVE today) still produce a finite
		// subscription. A first activation replacing an unexpired sub rolls
		// the remaining days over (legacy rollover / coding-plan kind rules).
		subExpiry, rerr := s.resolveOrderActivation(ctx, txSQLX, order, orderProduct, nil, preservedExpiry)
		planMissing := errors.Is(rerr, ErrPlanMissingForExpiry)
		// ErrOrderActivationConflict is the coding-plan counterpart of the
		// legacy downgrade block: the paid order conflicts with a different
		// active plan at activation time. Same handling — honor the payment,
		// leave the subscription/entitlement untouched, audit for ops.
		activationConflict := errors.Is(rerr, ErrOrderActivationConflict)
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
		case activationConflict:
			_ = writeAuditOnTx(ctx, tx, "service", "activation_conflict_blocked",
				fmt.Sprintf("order:%s", order.ID),
				[]string{"confirm", "coding_plan", "activation_blocked"},
				map[string]any{
					"order_id": order.ID,
					"channel":  in.Channel,
					"plan_id":  order.PlanID,
					"kind":     order.SnapshotOrderKind(),
				})
		default:
			return nil, fmt.Errorf("resolve sub expiry: %w", rerr)
		}
		// planMissing: a subscription cannot reference the missing plan (the
		// FK would reject the INSERT/UPDATE), so skip activation; the order
		// still goes paid below and ops follows up from the audit log.
		if !downgradeBlocked && !activationConflict && !planMissing {
			activated, err = activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, orderProduct, subExpiry)
			if err != nil {
				return nil, fmt.Errorf("activate sub: %w", err)
			}
			// 同事务 outbox（Task 10）：权益同步消息与支付状态翻转同一事务
			// 提交；dedup 键钉在 payment 上，Confirm/webhook/补单三路重复
			// 投递只会入队一次。blocked/planMissing 时订阅未动，无需同步。
			if orderTouchesBenefits(order, orderProduct) {
				dedup := access.PaidSyncDedupKey(paymentID)
				if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
					UserID:      order.UserID,
					ProductCode: orderProduct,
					Reason:      access.SyncReasonPaymentPaid,
					OrderID:     order.ID,
					PaymentID:   paymentID,
				}, &dedup); err != nil {
					return nil, err
				}
			}
		}
	} // end non-topup activation branch

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

// verifyConfirmPayment is Confirm's upstream gate: it returns nil only
// when the channel has independently confirmed, server-side, that
// externalTxnID is a settled payment for this order. WeChat is verified
// via QueryOrder (the same client call the active-reconcile path in
// GetOrder uses); stripe / paypal / alipay have no wired query client
// today, so their confirms are refused with
// ErrConfirmVerificationUnavailable and left to the channel webhook.
func (s *PaymentService) verifyConfirmPayment(ctx context.Context, order *model.Order, channel, externalTxnID string) error {
	if s.confirmVerifier != nil {
		return s.confirmVerifier(ctx, order, channel, externalTxnID)
	}
	if channel != "wechat_pay" {
		return ErrConfirmVerificationUnavailable
	}
	if s.wechat == nil {
		return ErrWechatPayNotConfigured
	}
	// The order's provider_intent carries the out_trade_no minted at
	// CreateOrder — the only handle WeChat accepts for an order query.
	// Orders created for a different channel (or a wechat order that
	// failed pre-auth) have none; either way the claim cannot be
	// verified upstream, so it is refused as unverified (4xx), not a
	// server error.
	if order.ProviderIntent == nil {
		return ErrConfirmNotVerified
	}
	var intent struct {
		OutTradeNo string `json:"out_trade_no"`
	}
	if err := json.Unmarshal(*order.ProviderIntent, &intent); err != nil || intent.OutTradeNo == "" {
		return ErrConfirmNotVerified
	}
	res, err := s.wechat.QueryOrder(ctx, intent.OutTradeNo)
	if err != nil {
		// Wrapped with the wechat sentinels (ErrWeChatUpstream /
		// ErrWeChatNetwork / ...) the handler already maps — a transient
		// upstream failure must surface as retryable, not as "not paid".
		return err
	}
	// Every field the caller could lie about is checked against the
	// upstream answer: trade state, the order handle, the transaction id
	// they claimed, and the settled amount/currency against the order's
	// snapshot (amount.total is fen, i.e. cents).
	if res == nil || res.TradeState != "SUCCESS" ||
		res.OutTradeNo != intent.OutTradeNo ||
		res.TransactionID != externalTxnID ||
		res.Amount.Total < toCents(order.Amount) ||
		!strings.EqualFold(res.Amount.Currency, order.Currency) {
		return ErrConfirmNotVerified
	}
	return nil
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
	// Compare in integer cents (DECIMAL(10,2) → int64) — a float64 sum
	// like 0.1+0.2 > 0.3 would wrongly reject an exact full refund.
	if toCents(currentSum)+toCents(in.Amount) > toCents(paymentAmount) {
		return nil, ErrRefundSumExceedsPayment
	}

	// Call the channel refund API. Failure aborts before INSERT — we did
	// not create the refund row, so no orphan.
	// 商户退款单号由我方生成并传给渠道（评审批次7 Important-2）：渠道退款
	// webhook 以同一商户单号派生 external_refund_id（微信 out_refund_no /
	// 支付宝 out_biz_no），两侧键一致，webhook 的 ON CONFLICT 重读才能命
	// 中本行（翻 paid），而不是为同一笔钱插入第二条退款行。单号从
	// Idempotency-Key 确定性派生（评审轮2 N2）：渠道退款成功但 refunds 行
	// INSERT 前崩溃/回滚时，客户端持同一幂等键重试必得同一单号，渠道以商
	// 户单号为幂等键拒绝第二笔退款（随机单号则会被渠道当成新退款执行）。
	merchantRefundNo := deriveMerchantRefundNo(in.IdempotencyKey)
	echoed, err := s.refundAPI.Refund(ctx, payment.Channel, payment.ExternalTxnID, merchantRefundNo, in.Amount, in.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefundChannelFailed, err)
	}
	// 评审轮2 M1：对账键一律用已发送的商户单号入库，不采纳渠道回显覆盖
	// ——真实客户端返回非空不同值会让 refunds 行与渠道侧静默错键，webhook
	// 的 ON CONFLICT 重读永远 miss。回显不一致仅记 WARN 供排查。
	if echoed != "" && echoed != merchantRefundNo {
		log.Printf("WARN refund: channel echoed merchant refund no %q differs from sent %q (payment=%s) — keeping the sent value as the reconciliation key",
			echoed, merchantRefundNo, payment.ID)
	}

	// INSERT pending refund. If another concurrent transaction beat us to
	// the same (channel, external_refund_id), the unique constraint fires;
	// since we just got the merchant refund no from the channel API, that
	// shouldn't happen in practice (the channel wouldn't accept the same
	// merchant refund no twice).
	extID := merchantRefundNo
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
	TransactionID          string  // channel's transaction ID — maps to payments.external_txn_id
	OrderID                string  // order UUID from channel metadata (Stripe) or out_trade_no (WeChat/Alipay)
	Amount                 float64 // settled amount (major currency units, normalized by handler)
	Currency               string  // ISO 4217
	RefundAmount           float64 // for refund events
	ExternalRefundID       string  // channel's refund ID
	ExternalSubscriptionID string  // PayPal: subscription ID (`I-...`) — used by renewal branch to find the active sub
	// SkipAmountCheck exempts PayPal lifecycle events
	// (BILLING.SUBSCRIPTION.ACTIVATED etc.) from the amount/currency
	// validation in onPaymentSucceeded: PayPal omits resource.amount from
	// lifecycle payloads entirely, so Amount=0/Currency="" there is by
	// construction, not evidence of underpayment. The trust anchor for
	// activation is the verified signature + custom_id order link +
	// subscription status, not the (absent) amount. Payment events
	// (CAPTURE/SALE) always carry amounts and keep the strict check.
	SkipAmountCheck bool
	SubExpiresAt    *time.Time // subscription expiry at activation. MUST be supplied by the
	// caller (e.g. an explicit channel metadata field) — yunhou-users
	// MUST NOT compute it from plan.interval_days; that calculation is
	// a frontend product decision (rollover rules, grace periods, trials).
	// nil = never expires.
}

// OnWebhookResult reports what the handler did.
type OnWebhookResult struct {
	DuplicateEvent bool   // true if event_id was already seen (handler should ack 200)
	DomainAction   string // set only when an action ran. Values:
	//   "payment_paid" / "payment_failed" / "refund_paid" / "refund_failed"
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
	case isRefundFailedEvent(e.EventType):
		domainAction = "refund_failed"
		if err := s.onRefundFailed(ctx, e); err != nil {
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

// lookupOrderByWebhookID finds the order behind a webhook's order
// reference. The PRIMARY lookup is by orders.id (Stripe + e2e tests that
// pass the UUID); the FALLBACK is a JSONB walk for wechat_pay/alipay's
// out_trade_no — the channel-side identifier those channels send (32-char
// hyphenless form minted at CreateOrder as
// strings.ReplaceAll(order.ID,"-","")[:32]). Shared by onPaymentSucceeded
// and the refund path (评审轮3 D-2: N-2 的关单反查必须走同一段两段式查
// 找——只用主键在真实渠道键形态下必然 miss，未支付关单 500 风暴原样保留).
// Callers run it inside their own tx (same-connection constraint).
func lookupOrderByWebhookID(ctx context.Context, tx dbTx, channel, orderRef string) (*model.Order, error) {
	var order model.Order
	err := tx.GetContext(ctx, &order, `SELECT * FROM orders WHERE id = $1`, orderRef)
	if errors.Is(err, sql.ErrNoRows) && (channel == "wechat_pay" || channel == "alipay") {
		// JSONB text-extraction ->> returns NULL for rows without the
		// key; equality with the channel's out_trade_no resolves it to
		// the canonical order. LIMIT 1 in case the JSONB value collides
		// (operator error — duplicate out_trade_no is a YDN alert).
		err = tx.GetContext(ctx, &order, `
			SELECT * FROM orders
			WHERE provider_intent IS NOT NULL
			  AND provider_intent->>'out_trade_no' = $1
			LIMIT 1
		`, orderRef)
	}
	if err != nil {
		return nil, err
	}
	return &order, nil
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

	// Find the order (两段式：主键 + provider_intent out_trade_no 回退 —
	// 与退款路径共用 lookupOrderByWebhookID，评审轮3 D-2). WeChat and
	// Alipay send the 32-char hyphenless out_trade_no rather than our UUID.
	//
	// MAJOR fix (review 2): without the fallback, real WeChat webhooks
	// 404'd because no order has the 32-char hex as its primary id; the
	// order would stay "pending" forever and the user never got the
	// subscription.
	order, err := lookupOrderByWebhookID(ctx, tx, e.Channel, e.OrderID)
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
		if existing.ExternalTxnID != e.TransactionID {
			// Same channel, already-paid order, but a DIFFERENT transaction:
			// letting the INSERT below run would collide with the partial
			// unique index (UNIQUE(order_id) WHERE status='paid') and
			// surface as a 500 — the channel would retry forever. Treat as
			// dedupe: ack 200 + audit. A matching txn id is a legitimate
			// redelivery and falls through to the
			// (channel, external_txn_id) dedupe below.
			return s.writeAudit(ctx, "service", "webhook_duplicate_paid_order",
				fmt.Sprintf("order:%s", order.ID),
				[]string{"webhook", "duplicate_paid_order"},
				map[string]any{
					"order_id": order.ID, "channel": e.Channel,
					"existing_txn_id": existing.ExternalTxnID, "event_txn_id": e.TransactionID,
					"event_id": e.EventID,
				},
			)
		}
	}

	// Amount/currency validation (2026-08 trust-model fix): the channel
	// signature proves the event came from the channel, not that the
	// settled amount matches the order. An under-paid or wrong-currency
	// event must NOT activate the subscription. Compare in integer cents
	// (DECIMAL(10,2) round-trip drifts in float64); currency match is
	// case-insensitive. Ack 200 (non-retryable) + audit so channels
	// don't retry forever and ops can reconcile manually.
	if !e.SkipAmountCheck && (toCents(e.Amount) < toCents(order.Amount) || !strings.EqualFold(e.Currency, order.Currency)) {
		return s.writeAudit(ctx, "service", "webhook_amount_mismatch",
			fmt.Sprintf("order:%s", order.ID),
			[]string{"webhook", "amount_mismatch"},
			map[string]any{
				"order_id": order.ID, "channel": e.Channel, "event_id": e.EventID,
				"event_amount": e.Amount, "event_currency": e.Currency,
				"order_amount": order.Amount, "order_currency": order.Currency,
			},
		)
	}

	now := time.Now()
	// PayPal lifecycle events (ACTIVATED) carry no amount/currency — the
	// payment row records a $0 first settlement in the ORDER's currency
	// (payments_currency_check is length=3; "" would violate it). For a
	// trialed subscription $0 is genuinely what was charged at approval;
	// the first real charge arrives later as PAYMENT.SALE.COMPLETED on the
	// renewal path with its own amount.
	paymentAmount, paymentCurrency := e.Amount, e.Currency
	if e.SkipAmountCheck && paymentCurrency == "" {
		paymentCurrency = order.Currency
	}
	p := &model.Payment{
		ID:            GenerateUUID(),
		OrderID:       order.ID,
		Channel:       e.Channel,
		ExternalTxnID: e.TransactionID,
		Amount:        paymentAmount,
		Currency:      paymentCurrency,
		Status:        "paid",
		PaidAt:        &now,
		RawPayload:    e.RawPayload,
	}

	paymentID, inserted, err := insertPaymentOnTx(ctx, tx, p)
	if err != nil {
		return fmt.Errorf("insert payment: %w", err)
	}

	// Product scope (migration 027): every subscription read/write below is
	// scoped to the product of the ORDER'S plan. Migration 029: prefer the
	// order's frozen product snapshot; the live plan row is the pre-029
	// fallback. An empty product (plan row missing) matches no subscription
	// rows and lets the resolver surface ErrPlanMissingForExpiry instead.
	orderProduct, pcErr := s.resolveOrderProduct(ctx, txSQLX, order)
	if pcErr != nil {
		return fmt.Errorf("resolve order product: %w", pcErr)
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
		if existing.OrderID != order.ID {
			// The (channel, external_txn_id) row belongs to a DIFFERENT
			// order than this event resolved. Activating here would grant
			// this event's order a subscription another order paid for.
			// Audit + skip activation; ack 200 (non-retryable).
			return s.writeAudit(ctx, "service", "webhook_payment_order_mismatch",
				fmt.Sprintf("payment:%s", existing.ID),
				[]string{"webhook", "payment_order_mismatch"},
				map[string]any{
					"channel": e.Channel, "event_id": e.EventID,
					"event_order_id": order.ID, "payment_order_id": existing.OrderID,
				},
			)
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

		activeSub, sErr := s.txLookupActiveSubscription(ctx, txSQLX, order.UserID, orderProduct)
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

	// 余额充值商品（Task 14）：不激活订阅、不发模型权益——同事务入队钱包
	// 充值消息（dedup 钉 payment_id；重复 webhook 投递不重复入账）。
	if orderProduct == model.ProductWalletTopup {
		if err := s.enqueueWalletTopup(ctx, tx, order, paymentID); err != nil {
			return err
		}
	} else {

		// Subscription activation is gated by payment success, not by order
		// status — see doc §"Subscription activation". The order UPDATE below
		// is idempotent (already-paid / late-paid / cancelled-but-honored all
		// succeed). Re-running the activation UPSERT on a retried event is
		// safe — the UPDATE branch of activateSubscriptionOnTx hits the same
		// row.
		subExpiry, rerr := s.resolveOrderActivation(ctx, txSQLX, order, orderProduct, e.SubExpiresAt, preservedExpiry)
		planMissing := errors.Is(rerr, ErrPlanMissingForExpiry)
		activationConflict := errors.Is(rerr, ErrOrderActivationConflict)
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
			// succeeded, so we silently audit and skip activation (a
			// subscription cannot reference the missing plan).
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
		case activationConflict:
			// coding-plan counterpart of the downgrade block (Task 10): the
			// paid order conflicts with a DIFFERENT active plan at activation
			// time (e.g. a stale QR paid after a tier change). Honor the
			// payment, leave subscription + entitlement untouched, audit.
			_ = writeAuditOnTx(ctx, tx, "service", "activation_conflict_blocked",
				fmt.Sprintf("order:%s", order.ID),
				[]string{"webhook", "coding_plan", "activation_blocked"},
				map[string]any{
					"order_id": order.ID,
					"channel":  e.Channel,
					"event_id": e.EventID,
					"plan_id":  order.PlanID,
					"kind":     order.SnapshotOrderKind(),
				})
		default:
			return fmt.Errorf("resolve sub expiry: %w", rerr)
		}
		if !downgradeBlocked && !activationConflict && !planMissing {
			if _, err := activateSubscriptionOnTx(ctx, tx, order.UserID, order.PlanID, orderProduct, subExpiry); err != nil {
				return fmt.Errorf("activate sub: %w", err)
			}
			// 同事务 outbox（Task 10）：权益同步消息随支付状态翻转同一事务
			// 提交；dedup 键钉在 payment 上，webhook/Confirm/主动补单三路
			// 重复投递只入队一次。blocked/planMissing 时订阅未动，无需同步。
			if orderTouchesBenefits(order, orderProduct) {
				dedup := access.PaidSyncDedupKey(paymentID)
				if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
					UserID:      order.UserID,
					ProductCode: orderProduct,
					Reason:      access.SyncReasonPaymentPaid,
					OrderID:     order.ID,
					PaymentID:   paymentID,
				}, &dedup); err != nil {
					return err
				}
			}
		}
	} // end non-topup activation branch

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
	//
	// The subquery is scoped by (user_id, plan_id, product_code): plan_id
	// already pins the product via the 027 trigger, but carrying
	// product_code explicitly keeps the row selection unambiguous if a
	// plan ever changes product between order and webhook.
	if e.ExternalSubscriptionID != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			SET external_subscription_id = $1
			WHERE id = (
				SELECT id FROM subscriptions
				WHERE user_id = $2
				  AND plan_id = $3
				  AND product_code = $4
				  AND status = 'active'
				ORDER BY created_at DESC
				LIMIT 1
			)
		`, e.ExternalSubscriptionID, order.UserID, order.PlanID, orderProduct); err != nil {
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
		// 同事务 outbox（Task 10）：支付失败级联取消订阅后，权益同步
		// 翻转为 revoked。dedup 键钉在 payment 上。
		failProduct, fpErr := s.resolveOrderProduct(ctx, rawSQLXTx(tx), &order)
		if fpErr != nil {
			return fmt.Errorf("resolve failed order product: %w", fpErr)
		}
		if orderTouchesBenefits(&order, failProduct) {
			dedup := access.FailedSyncDedupKey(payment.ID)
			if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
				UserID:      order.UserID,
				ProductCode: failProduct,
				Reason:      access.SyncReasonPaymentFailed,
				OrderID:     order.ID,
				PaymentID:   payment.ID,
			}, &dedup); err != nil {
				return err
			}
		}
		// 评审批次7 Important-3：wallet-topup 支付在 paid 之后收到
		// payment_failed —— 支付行翻 failed 但已入账现金仍可花。同事务入
		// 队钱包冲正（现金借记原路收回，已消费则如实转负），与权益吊销同
		// 一级联。
		if failProduct == model.ProductWalletTopup {
			// 评审轮2 N3：冲正额 = 订单全额 − 该支付已 paid 退款合计。充值
			// 50 → 部分退款 20（钱包已借记 20）→ payment_failed 乱序到达，
			// 再按 50 冲正等于向用户追 70。只计 paid 行：pending 退款尚未触
			// 发钱包借记，其 webhook 到达时会被 payment_not_paid 守卫拦下。
			// 已全额退款（差额 ≤ 0）则跳过冲正——全额退款的支付通常已是
			// refunded 终态走不到这里，此分支作纵深防御；dedup 守卫仍钉
			// payment id，语义不变。
			var refundedSum float64
			if err := tx.GetContext(ctx, &refundedSum,
				`SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1 AND status = 'paid'`, payment.ID); err != nil {
				return fmt.Errorf("sum paid refunds for failed-payment debit: %w", err)
			}
			if debit := order.Amount - refundedSum; toCents(debit) > 0 {
				if err := s.enqueueWalletFailedDebit(ctx, tx, &order, payment.ID, debit); err != nil {
					return err
				}
			}
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
			// 评审轮3 D-1+D-2：TRADE_CLOSED/trade_closed 是双义事件——既表
			// "未支付超时/取消关单"（高频正常事件，永远无支付行、不可能有
			// 退款），也表已支付交易的关闭。仅当 (a) 事件不携退款额且
			// (b) 订单从未支付 时才 audit+200：
			//   (a) 依赖渠道语义——Alipay 未支付关单的 trade_closed 不携带
			//       refund fee（webhook parser 的 refund_fee 字段，未支
			//       付关单解析为 0）。携带退款额（RefundAmount > 0）说明渠
			//       道侧确已支付并退款（可能只是 TRADE_SUCCESS 仍在约 24h
			//       的重投窗口内）：一律落到下面的返错重投分支，否则
			//       audit+200 → MarkProcessed → 退款永久丢失、随后
			//       TRADE_SUCCESS 重投照常充值 = 双花（轮 2 N-2 重开的乱
			//       序窗口）。
			//   (b) 订单反查走与 onPaymentSucceeded 相同的两段式查找
			//       （主键 + provider_intent->>'out_trade_no'）——真实渠道
			//       键是 32 位无横线 out_trade_no，只查主键必然 miss。
			// charge.refunded/TRANSACTION.REFUND 等无歧义退款事件不走此分
			// 支——查无支付行一律按乱序处理（保轮 1 C2 语义）。
			if e.OrderID != "" && (e.EventType == "TRADE_CLOSED" || e.EventType == "trade_closed") && e.RefundAmount <= 0 {
				order, oerr := lookupOrderByWebhookID(ctx, tx, e.Channel, e.OrderID)
				switch {
				case oerr == nil:
					if order.Status == "pending" || order.Status == "expired" ||
						order.Status == "cancelled" || order.Status == "failed" {
						return s.writeAudit(ctx, "service", "webhook_refund_unpaid_order",
							fmt.Sprintf("event:%s", e.EventID),
							[]string{"webhook", "unpaid_order"},
							map[string]any{"channel": e.Channel, "transaction_id": e.TransactionID,
								"order_id": e.OrderID, "order_status": order.Status, "event_id": e.EventID},
						)
					}
				case !errors.Is(oerr, sql.ErrNoRows):
					return fmt.Errorf("lookup order for close event: %w", oerr)
				}
			}
			// 评审轮1 C2：退款事件可能先于支付成功事件到达（渠道乱序投递）。
			// 审计照留（writeAudit 独立连接提交，不随本事务回滚），但必须返回
			// 错误让 handler 映射为非 2xx —— 渠道按其重投计划再次投递；ack
			// 200 会让 OnWebhook 标记 processed、渠道不再重投，之后支付成功
			// 照常给钱包充值而退款永久丢失（双花）。订单不存在或订单已支付
			// 但支付行缺失都落此分支。
			if aerr := s.writeAudit(ctx, "service", "webhook_refund_unknown_payment",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "unknown_payment"},
				map[string]any{"channel": e.Channel, "transaction_id": e.TransactionID, "event_id": e.EventID},
			); aerr != nil {
				return fmt.Errorf("write audit: %w", aerr)
			}
			return fmt.Errorf("refund for unknown payment (channel=%s txn=%s): payment success event not processed yet — returning an error so the channel retries", e.Channel, e.TransactionID)
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

	// 评审轮4 B：Alipay 的 refund_fee 是**累计**退款总额——本次事件金额
	// = 累计值 − 该支付已记录退款总额。重复投递同一累计值（或同一通知重
	// 投）→ 增量为 0 → 幂等收敛不双退；部分退款序列（refund_fee 递增）
	// 逐笔只认增量。全额判定仍按累计值（e.RefundAmount ≥ payment.Amount）。
	// 其他渠道按单笔事件金额（既有语义）。
	// 评审批次7 Critical-1：增量 ≤ 0 只抑制「退款行 INSERT」这一项记账
	// 效应——全额/部分级联（payment/order 翻转、订阅取消、权益吊销入
	// 队，全部幂等）必须永远执行。此前增量 ≤ 0 直接 tx.Commit() 早退：
	// POST /refunds 的 API 行已计入 prior（金额被它承载），webhook 算出
	// delta=0 便跳过整个级联——用户拿了全额现金退款还保留订阅权益。
	// failed 行不计入 prior：那是渠道的终态否认，钱没有动（与 Refund()
	// 合计不变量同一口径）。
	eventRefundAmount := e.RefundAmount
	deltaSkip := false
	if e.Channel == "alipay" {
		var prior float64
		if err := tx.GetContext(ctx, &prior,
			`SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1 AND status IN ('paid', 'pending')`, payment.ID); err != nil {
			return fmt.Errorf("sum prior refunds: %w", err)
		}
		eventRefundAmount = e.RefundAmount - prior
		deltaSkip = eventRefundAmount <= 0
	}

	// Find or insert the refund row keyed on (channel, external_refund_id).
	// Insert as `pending` first so the sum-invariant (which counts pending
	// rows in Refund) holds even when this path creates a refund row that
	// wasn't initiated via POST /refunds. The follow-up UPDATE flips
	// pending → paid atomically; re-runs of the same webhook are no-ops
	// because the second pass sees `paid` and skips.
	// ON CONFLICT DO NOTHING absorbs webhook retries and——键统一为商户退
	// 款单号后（评审批次7 Important-2）——命中 POST /refunds 的 API 行；
	// re-read for the (channel, external_refund_id) → id mapping.
	extID := normalizeExternalRefundID(e.Channel, e.ExternalRefundID)
	var refundID string
	rowAmount := eventRefundAmount
	if deltaSkip {
		// 增量 ≤ 0：不落新行，金额效应由既有退款行承载。找该事件对应的
		// 既有行（POST /refunds 的 API 行，或本累计值首次投递时落的行）
		// 作为级联/钱包退款的锚点；找不到（换 notify_id 的重投）则级联
		// 仍以支付行幂等收敛，钱包退款不再重复入队。
		var row struct {
			ID     string  `db:"id"`
			Amount float64 `db:"amount"`
		}
		if err := tx.GetContext(ctx, &row, `
			SELECT id, amount FROM refunds WHERE channel = $1 AND external_refund_id = $2
		`, e.Channel, extID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lookup recorded refund: %w", err)
			}
		} else {
			refundID = row.ID
			rowAmount = row.Amount
		}
	} else {
		err = tx.QueryRowxContext(ctx, `
			INSERT INTO refunds (payment_id, channel, user_id, amount, idempotency_key, external_refund_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')
			ON CONFLICT (channel, external_refund_id) DO NOTHING
			RETURNING id
		`, payment.ID, e.Channel, order.UserID, eventRefundAmount, "webhook:"+e.EventID, extID).Scan(&refundID)
		switch {
		case err == nil:
			// inserted (new pending row — will be flipped below)
		case errors.Is(err, sql.ErrNoRows):
			// already inserted (POST /refunds 的 API 行或 webhook 重投) — look it up
			if lerr := tx.GetContext(ctx, &refundID, `
				SELECT id FROM refunds WHERE channel = $1 AND external_refund_id = $2
			`, e.Channel, extID); lerr != nil {
				return fmt.Errorf("re-read refund: %w", lerr)
			}
		default:
			return fmt.Errorf("insert refund: %w", err)
		}
	}

	// Mark the refund paid (idempotent — if already paid, no-op). webhook
	// 即渠道确认：POST /refunds 落的 pending API 行也在此翻 paid，不再
	// 永远卡住合计不变量。空 refundID（无归属行的重投）没有行可翻，跳过。
	if refundID != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE refunds SET status = 'paid', updated_at = now()
			WHERE id = $1 AND status = 'pending'
		`, refundID); err != nil {
			return fmt.Errorf("mark refund paid: %w", err)
		}
	}

	// 订单产品归属（订单快照优先，029 口径）；钱包充值订单的退款走钱包
	// 现金原路退，订阅/权益路径与充值订单无关（Task 14）。
	refundProduct, rpErr := s.resolveOrderProduct(ctx, rawSQLXTx(tx), &order)
	if rpErr != nil {
		return fmt.Errorf("resolve refunded order product: %w", rpErr)
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
		if refundProduct != model.ProductWalletTopup {
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
			// 同事务 outbox（Task 10）：全额退款把权益翻转为 revoked —— 阻止
			// 后续不再具备权益的调用；已消费账本与配额窗口一行不动（账本
			// 追加+冲正，设计 §7.3）。dedup 键钉在 refund 行上：渠道重投与
			// 重复退款事件只入队一次，worker 收敛本身也是幂等的。无归属行
			// 的重投（refundID 为空）用 nil dedup——多笔支付不得共享一个
			// 空键互相吃掉吊销（同 CancelSync 裁决：收敛幂等兜底）。
			if orderTouchesBenefits(&order, refundProduct) {
				var dedup *string
				if refundID != "" {
					d := access.RefundSyncDedupKey(refundID)
					dedup = &d
				}
				if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
					UserID:      order.UserID,
					ProductCode: refundProduct,
					Reason:      access.SyncReasonRefundFull,
					OrderID:     order.ID,
					PaymentID:   payment.ID,
					RefundID:    refundID,
				}, dedup); err != nil {
					return err
				}
			}
		}
	}
	// 余额充值订单的退款（全额或部分，Task 14）：同事务入队钱包退款消息
	// ——钱包现金原路退（dedup 钉 refund 行；消费侧 wallet:refund:{id} 业
	// 务键幂等）。赠送余额不参与退款（钱包侧纯规则 + CHECK 双兜底）。
	// 纵深防御（Task 14 deferred minor，Task 15 收尾）：只有支付行在本事务
	// 入口仍处 paid 才允许入队。dedup 键钉的是同一笔 refund 的重投；对已
	// refunded 的支付再到达的*另一笔*退款事件（不同 external_refund_id）
	// 若不入守卫会再次扣减钱包现金。退款行本身照常记录（支付域事实），
	// 但钱包侧不再跟随。
	// 金额口径（评审批次7 Critical-1）：增量事件用增量；deltaSkip（金额
	// 由 POST /refunds 的 API 行承载）用行金额——否则 API 发起的全额退
	// 款算出 delta=0，钱包退款永远不入队。无归属行的重投跳过（首次投递
	// 已按行入队）。
	if refundProduct == model.ProductWalletTopup {
		switch {
		case payment.Status != "paid":
			if err := writeAuditOnTx(ctx, tx, "service", "wallet_refund_skipped_payment_not_paid",
				fmt.Sprintf("payment:%s", payment.ID),
				[]string{"refund", "wallet_topup", "guard"},
				map[string]any{"payment_id": payment.ID, "payment_status": payment.Status,
					"refund_id": refundID, "channel": e.Channel}); err != nil {
				return fmt.Errorf("write audit: %w", err)
			}
		case refundID == "":
			// 无归属退款行的重投：金额效应已随首次投递入队，不重复扣减。
		default:
			if err := s.enqueueWalletRefund(ctx, tx, &order, payment.ID, refundID, rowAmount); err != nil {
				return err
			}
		}
	}
	// Partial refund: no domain action beyond marking the refund paid.
	// 部分退款不动订阅与权益（既有规则保留）：订阅继续到原到期点；
	// 金额侧由 refunds 行与渠道对账承载。Alipay 累计语义下 eventRefundAmount
	// 是本笔增量（评审轮4 B），全额判定用的累计值在 e.RefundAmount。

	return tx.Commit()
}

// onRefundFailed: 微信 REFUND.ABNORMAL / REFUND.CLOSED（渠道终态退款失
// 败，评审批次7 Important-2）。匹配的 pending 退款行翻 failed —— failed
// 是终态否认：不计入 Refund() 合计不变量的预留，也不计入 Alipay 累计口
// 径的 prior，用户可重试同一逻辑退款。支付/订单/订阅一行不动：钱没退出
// 去，paid 状态与权益保持。
func (s *PaymentService) onRefundFailed(ctx context.Context, e WebhookEvent) error {
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
			// 乱序：支付成功事件尚未处理。与退款成功同一哲学（评审轮1
			// C2）——审计照留但返错让渠道重投，ack 200 会把失败事实永久
			// 丢掉（退款行卡 pending，堵住合计不变量）。
			if aerr := s.writeAudit(ctx, "service", "webhook_refund_failed_unknown_payment",
				fmt.Sprintf("event:%s", e.EventID),
				[]string{"webhook", "unknown_payment", "refund_failed"},
				map[string]any{"channel": e.Channel, "transaction_id": e.TransactionID, "event_id": e.EventID},
			); aerr != nil {
				return fmt.Errorf("write audit: %w", aerr)
			}
			return fmt.Errorf("refund-failure for unknown payment (channel=%s txn=%s): payment success event not processed yet — returning an error so the channel retries", e.Channel, e.TransactionID)
		}
		return fmt.Errorf("find payment: %w", err)
	}

	extID := normalizeExternalRefundID(e.Channel, e.ExternalRefundID)
	if extID == "" {
		// 事件未携商户退款单号（解析层未覆盖该事件类型）：无法键控匹配，
		// 审计后 ack —— 返错重投也永远匹配不上，只会空转重投窗口。
		return s.writeAudit(ctx, "service", "webhook_refund_failed_missing_refund_no",
			fmt.Sprintf("event:%s", e.EventID),
			[]string{"webhook", "refund_failed", "missing_key"},
			map[string]any{"channel": e.Channel, "transaction_id": e.TransactionID, "event_id": e.EventID},
		)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE refunds SET status = 'failed', updated_at = now()
		WHERE channel = $1 AND external_refund_id = $2 AND payment_id = $3 AND status = 'pending'
	`, e.Channel, extID, payment.ID)
	if err != nil {
		return fmt.Errorf("mark refund failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 无匹配 pending 行。区分两种情形：行存在但已是终态（paid——成
		// 功/失败事件乱序，以先到的终态为准）→ 幂等收敛；行不存在（事
		// 件先于 API 行的 commit 到达）→ 返错让渠道重投。
		var status string
		lerr := tx.GetContext(ctx, &status, `
			SELECT status FROM refunds WHERE channel = $1 AND external_refund_id = $2 AND payment_id = $3
		`, e.Channel, extID, payment.ID)
		switch {
		case lerr == nil:
			// 已是终态：幂等 no-op。
		case errors.Is(lerr, sql.ErrNoRows):
			return fmt.Errorf("refund-failure matched no refund row (channel=%s txn=%s refund=%s): API row not committed yet — returning an error so the channel retries", e.Channel, e.TransactionID, extID)
		default:
			return fmt.Errorf("lookup refund for failure: %w", lerr)
		}
	} else if err := writeAuditOnTx(ctx, tx, "service", "refund_marked_failed",
		fmt.Sprintf("payment:%s", payment.ID),
		[]string{"refund", "failed", "webhook"},
		map[string]any{"payment_id": payment.ID, "external_refund_id": extID, "channel": e.Channel},
	); err != nil {
		return fmt.Errorf("write audit: %w", err)
	}

	return tx.Commit()
}

// normalizeExternalRefundID 把 webhook 解析层为兜底唯一性加的渠道前缀
// （"wechat-"/"alipay-"/"paypal-"，见 handler/webhook.go）剥掉，统一以裸
// 商户退款单号作为 refunds.(channel, external_refund_id) 的键（评审批次
// 7 Important-2）：POST /refunds 的 API 行存的就是裸商户单号，两侧键一
// 致，webhook 的 ON CONFLICT 重读才能命中 API 行，不为同一笔钱插入第二
// 条退款行。解析层若已改为不加前缀，TrimPrefix 是 no-op。
func normalizeExternalRefundID(channel, extID string) string {
	prefix := map[string]string{
		"wechat_pay": "wechat-",
		"alipay":     "alipay-",
		"paypal":     "paypal-",
	}[channel]
	if prefix != "" {
		return strings.TrimPrefix(extID, prefix)
	}
	return extID
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
	// Amount/currency sanity check (review users-1, 2026-08-17): the
	// signature proves the event came from PayPal, not that the settled
	// amount matches the plan. A wrong-currency or below-plan renewal must
	// NOT silently extend the subscription. Unlike the order-activation
	// path (webhook_amount_mismatch → reject), the renewal path audit-logs
	// mismatches but still processes: intl-staging runs plan-amount-override
	// ($0.01/$0.10 sandbox charges) and the L3 suite fires $4.99 renewals,
	// so hard-rejecting on amount would break legitimate test/staging flows.
	// The audit row is the ops signal to investigate a genuine undercharge.
	//
	// planRow is also the source of the synthetic order's snapshot
	// descriptors (product_code / plan_interval_days, migration 029). The
	// benefit_* columns stay NULL by design: a renewal is not a new
	// purchase negotiation — the entitlement worker falls back to the
	// ORIGINAL paid order's frozen snapshot, so a renewal never picks up
	// a silently-rewritten benefit config.
	var planRow *model.Plan
	if plan, perr := s.txLookupPlan(ctx, rawSQLXTx(tx), sub.PlanID); perr != nil {
		// Missing plan is not this event's fault — proceed with the renewal
		// but flag it so ops can reconcile (mirrors onPaymentSucceeded's
		// lenient plan lookup).
		_ = writeAuditOnTx(ctx, tx, "service", "paypal_renewal_plan_lookup_failed",
			fmt.Sprintf("subscription:%s", sub.ID),
			[]string{"webhook", "paypal", "renewal", "plan_lookup"},
			map[string]any{
				"plan_id":  sub.PlanID,
				"event_id": e.EventID,
			})
	} else {
		planRow = plan
		if !strings.EqualFold(e.Currency, plan.Currency) {
			_ = writeAuditOnTx(ctx, tx, "service", "paypal_renewal_currency_mismatch",
				fmt.Sprintf("subscription:%s", sub.ID),
				[]string{"webhook", "paypal", "renewal", "currency_mismatch"},
				map[string]any{
					"event_id":       e.EventID,
					"event_currency": e.Currency,
					"plan_currency":  plan.Currency,
					"amount":         e.Amount,
					"payment_id":     "",
					"order_id":       "",
				})
		} else if toCents(e.Amount) < toCents(plan.Price) {
			_ = writeAuditOnTx(ctx, tx, "service", "paypal_renewal_amount_below_plan",
				fmt.Sprintf("subscription:%s", sub.ID),
				[]string{"webhook", "paypal", "renewal", "amount_mismatch"},
				map[string]any{
					"event_id":     e.EventID,
					"event_amount": e.Amount,
					"plan_amount":  plan.Price,
					"currency":     e.Currency,
					"payment_id":   "",
					"order_id":     "",
					"note":         "audit-only; renewal still processed (plan-amount-override / test flows)",
				})
		}
	}
	// Snapshot descriptors for the synthetic order: product comes from the
	// subscription row (trigger-maintained, always right), interval from
	// the plan when found. Both are descriptive only — the coding-plan
	// activation path never runs for synthetic renewal orders.
	var synInterval *int
	if planRow != nil {
		synInterval = &planRow.IntervalDays
	}
	err = tx.QueryRowxContext(ctx, `
		INSERT INTO orders (user_id, plan_id, amount, currency, status, expires_at, provider_intent, product_code, plan_interval_days)
		VALUES ($1, $2, $3, $4, 'paid', $5, NULL, $6, $7)
		RETURNING id
	`, sub.UserID, sub.PlanID, e.Amount, e.Currency, orderExpiresAt, sub.ProductCode, synInterval).Scan(&orderID)
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

	// 同事务 outbox（Task 10）：渠道侧续费延期后同步权益（续费延长有效期，
	// 不提前重置窗口）。dedup 键钉在续费 payment 上，重投只入队一次。
	// 门控：coding-plan 订阅总是相关；kaya 订阅仅当其套餐挂了捆绑赠送
	// 配置（plan_benefit_configs）时才可能带动权益。worker 消费时按
	// "最近一次已支付订单快照"收敛，续费权益规格沿用原购买快照。
	if s.benefitSync != nil &&
		(sub.ProductCode == model.ProductCodingPlan || s.planHasBenefitConfig(ctx, rawSQLXTx(tx), sub.PlanID)) {
		dedup := access.PaidSyncDedupKey(paymentID)
		if err := s.enqueueBenefitSync(ctx, tx, access.EntitlementSyncMessage{
			UserID:         sub.UserID,
			ProductCode:    sub.ProductCode,
			Reason:         access.SyncReasonRenewalPaid,
			OrderID:        orderID,
			PaymentID:      paymentID,
			SubscriptionID: sub.ID,
		}, &dedup); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// planHasBenefitConfig is the renewal-path gate for kaya bundle plans:
// true when the plan carries a gift mapping (plan_benefit_configs). A live
// read is fine here — the message is only a trigger; the worker converges
// from the frozen order snapshots. Errors fail open to "enqueue anyway":
// a spurious message converges to a no-op, a missed one delays a grant.
// The read shares the surrounding tx's connection when one is in flight —
// a second pool grab while holding a tx conn is the MaxOpenConns deadlock
// class documented on resolveSubExpiry.
func (s *PaymentService) planHasBenefitConfig(ctx context.Context, tx *sqlx.Tx, planID string) bool {
	if s.benefitRepo == nil {
		return false
	}
	var err error
	if tx != nil {
		_, err = s.benefitRepo.FindByPlanIDTx(ctx, tx, planID)
	} else {
		_, err = s.benefitRepo.FindByPlanID(ctx, planID)
	}
	return err == nil
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

// activateSubscriptionOnTx: the single-row UPSERT from webhook doc §5.3,
// scoped to one (user, product) pair since migration 027. Returns whether
// activation actually happened (true if the user just got a new active sub
// this call; false if they already had one or we reactivated an existing
// row). The product scope means a payment for product A never mutates the
// user's product-B subscription row.
func activateSubscriptionOnTx(ctx context.Context, tx dbTx, userID, planID, productCode string, expiresAt *time.Time) (bool, error) {
	// Step 1: UPDATE the target row within this product (active first, else
	// most recent). The 027 trigger validates plan↔product consistency on
	// the plan_id write; a mismatch aborts the whole activation tx.
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET
			plan_id = $1,
			started_at = COALESCE(started_at, now()),
			expires_at = $2,
			status = 'active'
		WHERE id = (
			SELECT id FROM subscriptions
			WHERE user_id = $3 AND product_code = $4
			ORDER BY CASE WHEN status = 'active' THEN 0 ELSE 1 END, created_at DESC
			LIMIT 1
		)
	`, planID, expiresAt, userID, productCode)
	if err != nil {
		return false, fmt.Errorf("update subscription: %w", err)
	}

	// RowsAffected tells us whether the UPDATE hit a row (vs no rows
	// existed at all) — no need for the follow-up COUNT(*) round-trip.
	n, _ := res.RowsAffected()

	if n == 0 {
		// Step 2: INSERT a new active row for this product. The partial
		// unique index idx_subscriptions_user_product_active rejects a
		// concurrent duplicate activation for the same (user, product).
		_, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at, product_code)
			VALUES ($1, $2, $3, 'active', now(), $4, $5)
		`, GenerateUUID(), userID, planID, expiresAt, productCode)
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
func (s *PaymentService) eligibilityAndInsertOrderTx(ctx context.Context, userID, planID, channel, orderKind string, upgradeFromPlanID *string, out **model.Order) error {
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

		// Benefit snapshot (migration 029): freeze the product, billing
		// cycle, entitlement spec and upgrade-rule outcome ON the order.
		// The payment callback honors only this snapshot — later operator
		// edits (price change, deactivation, config rewrite) never change
		// what an already-created order grants.
		//
		// coding-plan plans REQUIRE a plan_benefit_configs row in
		// 'subscription' mode — it is the product's payment configuration
		// (设计 §4.3: 没有配置的商品不可购买); without it paying would
		// grant nothing, so the order is refused here. kaya-membership
		// plans may OPTIONALLY carry a 'gift' config (bundle mapping,
		// 设计 §4.1): paying then issues a non-stackable gift entitlement.
		// A wrong-mode config row is an operator error and fails closed.
		var benefitPolicyVersionID *string
		var benefitModelIDs pq.StringArray
		var benefitGrantMode string
		// 余额充值商品（Task 14）无权益映射：支付成功后按订单快照金额
		// 充值钱包，benefit 快照列保持 NULL（不把充值金额当订阅有效期）。
		if s.benefitRepo != nil && plan.ProductCode != model.ProductWalletTopup {
			cfg, cerr := s.benefitRepo.FindByPlanIDTx(ctx, tx, planID)
			switch {
			case errors.Is(cerr, sql.ErrNoRows):
				if plan.ProductCode == model.ProductCodingPlan {
					return ErrPlanNotPurchasable
				}
			case cerr != nil:
				return fmt.Errorf("read benefit config: %w", cerr)
			default:
				wantMode := model.BenefitGrantModeGift
				if plan.ProductCode == model.ProductCodingPlan {
					wantMode = model.BenefitGrantModeSubscription
				}
				if cfg.GrantMode != wantMode {
					return ErrPlanNotPurchasable
				}
				benefitPolicyVersionID = &cfg.PolicyVersionID
				benefitModelIDs = cfg.ModelIDs
				benefitGrantMode = cfg.GrantMode
			}
		} else if plan.ProductCode == model.ProductCodingPlan {
			// No benefit-config read surface at all (unit tests / partial
			// deployments): coding-plan orders fail closed.
			return ErrPlanNotPurchasable
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

			ProductCode:            ptrIfNotEmpty(plan.ProductCode),
			PlanIntervalDays:       ptrInt(plan.IntervalDays),
			BenefitPolicyVersionID: benefitPolicyVersionID,
			BenefitModelIDs:        benefitModelIDs,
			BenefitGrantMode:       ptrIfNotEmpty(benefitGrantMode),
			OrderKind:              ptrIfNotEmpty(orderKind),
			UpgradeFromPlanID:      upgradeFromPlanID,
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
		"PAYMENT.CAPTURE.COMPLETED", "BILLING.SUBSCRIPTION.ACTIVATED":
		// ACTIVATED is the subscription activation trigger: its resource
		// carries custom_id (the order UUID the BFF set at creation),
		// status=ACTIVE (the buyer actually approved) and
		// billing_info.next_billing_time (expiry hint — 7-day trial end).
		// CREATED fires pre-approval with status=APPROVAL_PENDING and must
		// NOT activate: the buyer may abandon at the PayPal login.
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
		// 评审轮4：trade_refund = Alipay TRADE_SUCCESS/TRADE_FINISHED 携带
		// refund_fee 的部分退款通知；REFUND.SUCCESS = 真实 WeChat v3 退款
		// 事件类型（TRANSACTION.REFUND 为既有 mock 契约，两者并容）。
		"trade_refund", "REFUND.SUCCESS",
		"PAYMENT.CAPTURE.REFUNDED", "PAYMENT.SALE.REFUNDED":
		return true
	}
	return false
}

// isRefundFailedEvent — 退款终态失败事件（评审批次7 Important-2）。微信
// v3 REFUND.ABNORMAL（退款异常）/ REFUND.CLOSED（退款关闭）：此前不在任
// 何分发分支里，落进 audit-only 默认分支，失败退款永远卡 pending。路由
// 到 onRefundFailed 翻 failed。
func isRefundFailedEvent(eventType string) bool {
	switch eventType {
	case "REFUND.ABNORMAL", "REFUND.CLOSED":
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
//  2. channel-authoritative hint (webhook payload on channels that ship
//     sub_expires_at, e.g. Stripe metadata / PayPal renewal), clamped
//     to now() + plan.interval_days so even a verified hint cannot
//     extend the subscription beyond what the plan grants. The Confirm
//     path passes nil — caller-supplied expiry is untrusted and ignored
//     there. nil = no hint, fall through.
//
//  3. rollover (2026-07-28 upgrade/renewal rule). When this activation
//     REPLACES an unexpired active subscription IN THE SAME PRODUCT
//     (productCode scope, migration 027 — an active sub in another
//     product is invisible here and must not trigger rollover or the
//     downgrade block), the remaining days
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
//     Migration 029: when the order carries a snapshot interval
//     (snapshotIntervalDays > 0), it REPLACES the live plan interval
//     everywhere below — 已支付订单按下单快照兑现，运营改周期不影响它。
//
//  5. nil (plan missing OR interval_days == 0). Caller decides: webhook
//     paths audit-log + write NULL; Confirm path mirrors the same shape.
func (s *PaymentService) resolveSubExpiry(
	ctx context.Context,
	tx *sqlx.Tx,
	userID, planID, productCode string,
	hint, preservedExpiry *time.Time,
	snapshotIntervalDays int,
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
	// branch needs its interval, the rollover branch needs it for the
	// downgrade comparison, and the hint branch needs it for the clamp.
	// The interval the order actually honors is the frozen snapshot when
	// present; the live plan row still drives the downgrade comparison
	// against the CURRENT sub's plan (legacy rule, unchanged).
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
	// intervalOf applies the snapshot substitution for THIS order's plan.
	intervalOf := func(p *model.Plan) int {
		if snapshotIntervalDays > 0 {
			return snapshotIntervalDays
		}
		return p.IntervalDays
	}

	var candidate *time.Time
	if hint != nil {
		// Hints reach here only from signature-verified channel payloads
		// (Stripe metadata / PayPal next_billing_time) — Confirm no longer
		// forwards the caller-supplied ExpiresAt. Clamp anyway: even a
		// channel-authoritative hint must not extend the subscription
		// beyond what the plan grants from now (now + interval_days).
		// Legitimate longer values derived from an existing unexpired
		// sub are produced by the rollover branch below, which runs its
		// own max() against this candidate.
		p, err := loadPlan()
		if err != nil {
			return nil, err
		}
		c := *hint
		if iv := intervalOf(p); iv > 0 {
			if iv > maxIntervalDays {
				return nil, fmt.Errorf("plan %s interval_days=%d exceeds %d-day cap", planID, iv, maxIntervalDays)
			}
			if maxHint := time.Now().Add(time.Duration(iv) * 24 * time.Hour); c.After(maxHint) {
				c = maxHint
			}
		}
		candidate = &c
	}

	existing, sErr := s.txLookupActiveSubscription(ctx, tx, userID, productCode)
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
			// Plan deleted mid-payment: with the RESTRICT FKs on
			// orders/subscriptions.plan_id this state is only reachable
			// via manual DB surgery, and no activation can reference the
			// missing plan (any INSERT/UPDATE would violate the FK). The
			// caller's hint has no destination to attach to — always
			// surface ErrPlanMissingForExpiry so call sites audit and
			// skip activation.
			return nil, err
		}
		oldPlan, oErr := s.txLookupPlan(ctx, tx, existing.PlanID)
		if oErr != nil && !errors.Is(oErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("find current plan for rollover: %w", oErr)
		}
		if oldPlan != nil && oldPlan.IntervalDays > intervalOf(p) {
			return nil, ErrDowngradeActivationBlocked
		}
		if existing.ExpiresAt != nil && intervalOf(p) > 0 {
			if intervalOf(p) > maxIntervalDays {
				return nil, fmt.Errorf("plan %s interval_days=%d exceeds %d-day cap", planID, intervalOf(p), maxIntervalDays)
			}
			// max(): a hint that already sits beyond the rolled value
			// still wins. Note this intentionally prefers the rolled
			// value over a same-plan renewal hint (e.g. a BFF quote for
			// a renewal Confirm) — "extend the current expiry" is the
			// product rule; the channel-side billing anchor (PayPal
			// renewals run their own onPaypalRenewalSucceeded path and
			// never reach here) is unaffected.
			rolled := existing.ExpiresAt.Add(time.Duration(intervalOf(p)) * 24 * time.Hour)
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
	if intervalOf(p) <= 0 {
		return nil, nil
	}
	if intervalOf(p) > maxIntervalDays {
		return nil, fmt.Errorf("plan %s interval_days=%d exceeds %d-day cap", planID, intervalOf(p), maxIntervalDays)
	}
	t := time.Now().Add(time.Duration(intervalOf(p)) * 24 * time.Hour)
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

// ptrIfNotEmpty returns nil for an empty string so the 029 snapshot columns
// bind SQL NULL (their CHECK constraints only accept enum literals or NULL).
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ptrInt returns a pointer to n (snapshot interval; 0 is a meaningful
// "lifetime plan" value and must round-trip as 0, not NULL).
func ptrInt(n int) *int { return &n }

// toCents converts a major-units float64 (DECIMAL(10,2) round-trip) to
// integer cents. Used for exact monetary comparisons that must not
// suffer float round-trip drift (refund full-vs-partial detection).
// Non-finite or out-of-range values return math.MinInt64/0 to fail
// comparisons safely.
func toCents(v float64) int64 {
	if v != v { // NaN
		return 0
	}
	// 先比 float 界再做窄化转换：float64→int64 的溢出转换结果是实现定
	// 义（amd64 得 MinInt64、arm64 得 MaxInt64），单靠转换后的 c<0 守
	// 不住 arm64。阈值 = math.MaxInt64/100 的 float64 近似。
	if v >= 9.223372036854776e16 {
		return 1<<62 - 1 // overflow clamp
	}
	c := int64(v * 100)
	if v > 0 && c < 0 {
		return 1<<62 - 1 // overflow clamp
	}
	return c
}
