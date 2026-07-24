package repo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// ============================================================================
// OrderRepo — pre-payment intent (design doc §"Order")
// ============================================================================

type OrderRepo interface {
	Create(ctx context.Context, o *model.Order) error
	FindByID(ctx context.Context, id string) (*model.Order, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Order, error)
	CancelPending(ctx context.Context, id, userID string) (bool, error)
	// SweepExpired flips pending orders past expires_at to 'expired'.
	// Returns number of rows flipped (for sweeper observability).
	// Sweeper interval must be < expiry window (design doc §"v1 decisions").
	SweepExpired(ctx context.Context, now time.Time) (int64, error)
	// UpdateProviderIntent writes a raw JSON payload into orders.provider_intent.
	// The caller is responsible for marshalling the struct; the repo writes the
	// bytes verbatim into the JSONB column ($1::jsonb cast). Used by
	// channel-specific pre-auth flows (wechat_pay writes appid / mchid /
	// code_url / out_trade_no; paypal reserves the slot for future use).
	UpdateProviderIntent(ctx context.Context, orderID string, payload []byte) error
	// FindByProviderOutTradeNo resolves a channel-side out_trade_no (e.g. the
	// 32-char hex WeChat sends in webhook bodies) to the canonical Yunhou
	// order. The lookup walks orders.provider_intent->>'out_trade_no' because
	// Yunhou's orders.id is a UUID and WeChat's protocol constrains
	// out_trade_no to 32 chars. The JSONB text-extraction operator returns
	// NULL for rows without a wechat_pay provider_intent, so the query
	// safely returns sql.ErrNoRows for non-wechat channels.
	//
	// No dedicated column is added: the value is already persisted inside
	// provider_intent by CreateOrder's wechat_pay pre-auth branch, and
	// adding a column would duplicate state. Webhook volume is low enough
	// that a seq scan is fine; if hot path, add a GIN index on
	// (provider_intent->>'out_trade_no') later.
	FindByProviderOutTradeNo(ctx context.Context, outTradeNo string) (*model.Order, error)
}

type orderRepo struct{ db *sqlx.DB }

func NewOrderRepo(db *sqlx.DB) *orderRepo { return &orderRepo{db: db} }

func (r *orderRepo) Create(ctx context.Context, o *model.Order) error {
	// provider_intent is bound explicitly (not via the schema default) so
	// callers can write the wechat_pay pre-auth payload in the same INSERT.
	// A nil ProviderIntent must round-trip as SQL NULL — required after
	// migration 010_provider_intent_nullable so the JSON response's
	// omitempty fires for orders without a pre-auth payload. We use the
	// nullableJSONB driver.Valuer wrapper so empty intent binds NULL on
	// the wire; passing a raw []byte would have Postgres reject '' with
	// SQLSTATE 22P02.
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES (:id, :user_id, :plan_id, :amount, :currency, :status, :expires_at, :provider_intent)
	`, flattenOrderForInsert(o))
	return err
}

// orderInsertRow flattens an Order into the shape sqlx's NamedExecContext
// can bind. The provider_intent field is a *nullableJSONB (pointer to a
// driver.Valuer) so a nil pointer + nil/empty message becomes SQL NULL —
// required by migration 010_provider_intent_nullable so the JSON
// response's omitempty fires for orders without a pre-auth payload.
// Without this wrapper, sqlx would bind []byte as bytea, and Postgres
// JSONB rejects '' with SQLSTATE 22P02.
type orderInsertRow struct {
	ID             string         `db:"id"`
	UserID         string         `db:"user_id"`
	PlanID         string         `db:"plan_id"`
	Amount         float64        `db:"amount"`
	Currency       string         `db:"currency"`
	Status         string         `db:"status"`
	ExpiresAt      time.Time      `db:"expires_at"`
	ProviderIntent *nullableJSONB `db:"provider_intent"`
}

func flattenOrderForInsert(o *model.Order) *orderInsertRow {
	return &orderInsertRow{
		ID:             o.ID,
		UserID:         o.UserID,
		PlanID:         o.PlanID,
		Amount:         o.Amount,
		Currency:       o.Currency,
		Status:         o.Status,
		ExpiresAt:      o.ExpiresAt,
		ProviderIntent: wrapNullableJSONB(o.ProviderIntent),
	}
}

// wrapNullableJSONB returns nil when the message is nil or empty (so
// sqlx binds SQL NULL via the nullableJSONB.Value receiver). Returns a
// pointer to a populated nullableJSONB otherwise so the JSON bytes
// round-trip verbatim.
func wrapNullableJSONB(p *json.RawMessage) *nullableJSONB {
	if p == nil || len(*p) == 0 {
		return nil
	}
	return &nullableJSONB{msg: *p}
}

// nullableJSONB adapts a json.RawMessage for binding to a JSONB column.
// Empty input (nil pointer or zero-length message) round-trips as SQL
// NULL; non-empty input is passed through as raw bytes (pq driver
// accepts []byte for JSONB columns via implicit bytea→jsonb cast).
type nullableJSONB struct {
	msg json.RawMessage
}

// Value implements driver.Valuer. Returns nil for empty input so the
// Postgres driver binds SQL NULL; otherwise returns the raw bytes.
func (n *nullableJSONB) Value() (driver.Value, error) {
	if n == nil || len(n.msg) == 0 {
		return nil, nil
	}
	return []byte(n.msg), nil
}

func (r *orderRepo) FindByID(ctx context.Context, id string) (*model.Order, error) {
	var o model.Order
	err := r.db.GetContext(ctx, &o, `SELECT * FROM orders WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *orderRepo) ListByUserID(ctx context.Context, userID string) ([]model.Order, error) {
	var list []model.Order
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC
	`, userID)
	return list, err
}

// CancelPending atomically transitions pending → cancelled. Returns false if
// the order doesn't exist, isn't owned by userID, or isn't pending (already
// paid / failed / refunded / expired). The WHERE clause is the guard.
func (r *orderRepo) CancelPending(ctx context.Context, id, userID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE orders SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'pending'
	`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *orderRepo) SweepExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE orders SET status = 'expired', updated_at = now()
		WHERE status = 'pending' AND expires_at < $1
	`, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UpdateProviderIntent writes payload verbatim into orders.provider_intent.
// The `$1::jsonb` cast lets sqlx bind []byte (Postgres `bytea`) and converts
// to JSONB server-side; if payload is not valid JSON, Postgres rejects with
// SQLSTATE 22P02 and we surface the wrap below.
//
// Gated to status='pending': a slow pre-auth write landing after the
// payment has already settled would otherwise overwrite the code_url
// of a paid order, and the BFF would re-render a stale QR for a
// payment that already succeeded. Once the order is paid/failed/etc.
// the provider_intent is a forensic record; mutations belong to the
// status-transition path, not this one.
func (r *orderRepo) UpdateProviderIntent(ctx context.Context, orderID string, payload []byte) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE orders SET provider_intent = $1::jsonb, updated_at = now()
		 WHERE id = $2 AND status = 'pending'`,
		payload, orderID,
	)
	if err != nil {
		return fmt.Errorf("update provider_intent: %w", err)
	}
	return nil
}

// FindByProviderOutTradeNo resolves a channel-side out_trade_no to the
// canonical Yunhou order. WeChat sends 32-char hex out_trade_no; Alipay
// sends its own shape (the handler would do similar JSONB extraction
// when Alipay mock ships). Non-wechat orders naturally don't match
// because provider_intent->>'out_trade_no' returns NULL without that key.
// See OrderRepo interface doc for the rationale on JSONB vs a column.
func (r *orderRepo) FindByProviderOutTradeNo(ctx context.Context, outTradeNo string) (*model.Order, error) {
	var o model.Order
	err := r.db.GetContext(ctx, &o, `
		SELECT * FROM orders
		WHERE provider_intent IS NOT NULL
		  AND provider_intent->>'out_trade_no' = $1
		LIMIT 1
	`, outTradeNo)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ============================================================================
// PaymentRepo — channel-side transaction (design doc §"Payment", webhook doc §5.2)
// ============================================================================

type PaymentRepo interface {
	// InsertPaidOnConflictDoNothing is the business-level idempotency key
	// for webhook + confirm flows. UNIQUE (channel, external_txn_id) absorbs
	// duplicates. Returns (id, true) if inserted, (uuid.Zero, false) if
	// a row already exists for the same (channel, external_txn_id).
	InsertPaidOnConflictDoNothing(ctx context.Context, p *model.Payment) (string, bool, error)
	FindByID(ctx context.Context, id string) (*model.Payment, error)
	FindByChannelTxnID(ctx context.Context, channel, externalTxnID string) (*model.Payment, error)
	FindPaidByOrderID(ctx context.Context, orderID string) (*model.Payment, error) // channel mismatch pre-check (design doc confirm endpoint)
	ListByOrderID(ctx context.Context, orderID string) ([]model.Payment, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Payment, error) // GET /payments — joins via orders
	MarkPaid(ctx context.Context, id string, paidAt time.Time) error
	MarkFailed(ctx context.Context, id, reason string) error
	MarkRefunded(ctx context.Context, id string) error
	SetDisputed(ctx context.Context, id string, disputedAt time.Time) error
	ClearDisputed(ctx context.Context, id string) error
}

type paymentRepo struct{ db *sqlx.DB }

func NewPaymentRepo(db *sqlx.DB) *paymentRepo { return &paymentRepo{db: db} }

func (r *paymentRepo) InsertPaidOnConflictDoNothing(ctx context.Context, p *model.Payment) (string, bool, error) {
	var id string
	err := r.db.QueryRowxContext(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, $3, $4, $5, 'paid', now(), $6)
		ON CONFLICT (channel, external_txn_id) DO NOTHING
		RETURNING id
	`, p.OrderID, p.Channel, p.ExternalTxnID, p.Amount, p.Currency, NonNilRawPayload(p.RawPayload)).Scan(&id)
	if err != nil {
		// sql.ErrNoRows means the ON CONFLICT absorbed a duplicate. The
		// payment row already exists; caller should re-read it.
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

func (r *paymentRepo) FindByID(ctx context.Context, id string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `SELECT * FROM payments WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) FindByChannelTxnID(ctx context.Context, channel, externalTxnID string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `
		SELECT * FROM payments WHERE channel = $1 AND external_txn_id = $2
	`, channel, externalTxnID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) FindPaidByOrderID(ctx context.Context, orderID string) (*model.Payment, error) {
	var p model.Payment
	err := r.db.GetContext(ctx, &p, `
		SELECT * FROM payments WHERE order_id = $1 AND status = 'paid' LIMIT 1
	`, orderID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *paymentRepo) ListByOrderID(ctx context.Context, orderID string) ([]model.Payment, error) {
	var list []model.Payment
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM payments WHERE order_id = $1 ORDER BY created_at
	`, orderID)
	return list, err
}

// ListByUserID joins via orders.user_id for ownership-scoped listing (GET /payments).
func (r *paymentRepo) ListByUserID(ctx context.Context, userID string) ([]model.Payment, error) {
	var list []model.Payment
	err := r.db.SelectContext(ctx, &list, `
		SELECT p.* FROM payments p
		JOIN orders o ON o.id = p.order_id
		WHERE o.user_id = $1
		ORDER BY p.created_at DESC
	`, userID)
	return list, err
}

func (r *paymentRepo) MarkPaid(ctx context.Context, id string, paidAt time.Time) error {
	// Guard: only allow pending → paid at the row level (terminal states
	// won't match the WHERE; webhook doc §5.6 handles the no-op).
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'paid', paid_at = $1, updated_at = now()
		WHERE id = $2 AND status = 'pending'
	`, paidAt, id)
	return err
}

func (r *paymentRepo) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'failed', failed_reason = $1, updated_at = now()
		WHERE id = $2 AND status IN ('pending', 'paid')
	`, reason, id)
	return err
}

func (r *paymentRepo) MarkRefunded(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET status = 'refunded', updated_at = now()
		WHERE id = $1 AND status = 'paid'
	`, id)
	return err
}

func (r *paymentRepo) SetDisputed(ctx context.Context, id string, disputedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET disputed = true, disputed_at = $1, updated_at = now()
		WHERE id = $2
	`, disputedAt, id)
	return err
}

func (r *paymentRepo) ClearDisputed(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payments SET disputed = false, disputed_at = NULL, updated_at = now()
		WHERE id = $1
	`, id)
	return err
}

// NonNilRawPayload returns p unchanged when it holds valid JSON, otherwise
// an empty JSON object. Two cases are coerced to `'{}'`:
//   - nil — sqlx's positional binding cannot represent a nil json.RawMessage.
//   - empty (len==0, non-nil) — a zero-length json.RawMessage binds to '' which
//     Postgres rejects as invalid JSON (SQLSTATE 22023/22P02).
//
// All Postgres JSONB raw_payload columns in this package have a DEFAULT '{}'
// on the schema side, but the schema default is bypassed by an explicit bind.
//
// Exported (capital N) so callers in other packages (e.g. service/payment.go's
// insertPaymentOnTx) can share the same coercion at every INSERT that binds
// a raw payload.
func NonNilRawPayload(p json.RawMessage) json.RawMessage {
	if len(p) == 0 {
		return json.RawMessage(`{}`)
	}
	return p
}

// ============================================================================
// RefundRepo (design doc §"Refund", POST /refunds contract)
// ============================================================================

type RefundRepo interface {
	// FindByIdempotencyKey is the caller-retry gate. POST /refunds MUST
	// call this (scoped to userID) before invoking the channel refund API;
	// if a row exists, return it without calling the channel.
	FindByIdempotencyKey(ctx context.Context, userID, key string) (*model.Refund, error)
	InsertPending(ctx context.Context, r *model.Refund) error
	FindByID(ctx context.Context, id string) (*model.Refund, error)
	// FindByChannelRefundID matches a webhook refund event to its row.
	FindByChannelRefundID(ctx context.Context, channel, externalRefundID string) (*model.Refund, error)
	ListByPaymentID(ctx context.Context, paymentID string) ([]model.Refund, error)
	// SumByPaymentID returns the current refund total for an amount invariant check.
	SumByPaymentID(ctx context.Context, paymentID string) (float64, error)
	MarkPaid(ctx context.Context, id string) error
}

type refundRepo struct{ db *sqlx.DB }

func NewRefundRepo(db *sqlx.DB) *refundRepo { return &refundRepo{db: db} }

func (r *refundRepo) FindByIdempotencyKey(ctx context.Context, userID, key string) (*model.Refund, error) {
	// Scoped to user_id — global UNIQUE(key) would let one user see
	// another's refund response (IDOR) by guessing/reusing a key.
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `SELECT * FROM refunds WHERE user_id = $1 AND idempotency_key = $2`, userID, key)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) InsertPending(ctx context.Context, ref *model.Refund) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO refunds (id, payment_id, channel, user_id, amount, reason, idempotency_key, external_refund_id, status)
		VALUES (:id, :payment_id, :channel, :user_id, :amount, :reason, :idempotency_key, :external_refund_id, :status)
	`, ref)
	return err
}

func (r *refundRepo) FindByID(ctx context.Context, id string) (*model.Refund, error) {
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `SELECT * FROM refunds WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) FindByChannelRefundID(ctx context.Context, channel, externalRefundID string) (*model.Refund, error) {
	var ref model.Refund
	err := r.db.GetContext(ctx, &ref, `
		SELECT * FROM refunds WHERE channel = $1 AND external_refund_id = $2
	`, channel, externalRefundID)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *refundRepo) ListByPaymentID(ctx context.Context, paymentID string) ([]model.Refund, error) {
	var list []model.Refund
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM refunds WHERE payment_id = $1 ORDER BY created_at
	`, paymentID)
	return list, err
}

func (r *refundRepo) SumByPaymentID(ctx context.Context, paymentID string) (float64, error) {
	// Only count settled refunds — failed ones must not block retries.
	var sum float64
	err := r.db.GetContext(ctx, &sum, `
		SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_id = $1 AND status = 'paid'
	`, paymentID)
	return sum, err
}

func (r *refundRepo) MarkPaid(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE refunds SET status = 'paid', updated_at = now()
		WHERE id = $1 AND status = 'pending'
	`, id)
	return err
}

// ============================================================================
// WebhookEventRepo (webhook doc §5.1 — event-level dedup)
// ============================================================================

type WebhookEventRepo interface {
	// InsertOnConflictDoNothing is the event-level gate. If a row already
	// exists for (channel, event_id), returns (uuid.Zero, false) — the
	// handler should ack 200 and stop.
	InsertOnConflictDoNothing(ctx context.Context, e *model.WebhookEvent) (string, bool, error)
	// FindByChannelEventID is used on a dedupe hit to check whether the
	// prior run finished (processed_at IS NOT NULL) or crashed mid-action
	// (processed_at IS NULL — caller must re-run business action).
	FindByChannelEventID(ctx context.Context, channel, eventID string) (*model.WebhookEvent, error)
	MarkProcessed(ctx context.Context, id string) error
	// MarkProcessedOnTx folds the processed_at write into the same tx as
	// the business action, so a crash between action-commit and
	// MarkProcessed no longer leaves processed_at=NULL with side effects done.
	MarkProcessedOnTx(ctx context.Context, tx *sqlx.Tx, id string) error
}

type webhookEventRepo struct{ db *sqlx.DB }

func NewWebhookEventRepo(db *sqlx.DB) *webhookEventRepo { return &webhookEventRepo{db: db} }

func (r *webhookEventRepo) InsertOnConflictDoNothing(ctx context.Context, e *model.WebhookEvent) (string, bool, error) {
	var id string
	err := r.db.QueryRowxContext(ctx, `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (channel, event_id) DO NOTHING
		RETURNING id
	`, e.Channel, e.EventID, e.EventType, NonNilRawPayload(e.RawPayload)).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

func (r *webhookEventRepo) MarkProcessed(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE webhook_events SET processed_at = now() WHERE id = $1
	`, id)
	return err
}

func (r *webhookEventRepo) MarkProcessedOnTx(ctx context.Context, tx *sqlx.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE webhook_events SET processed_at = now() WHERE id = $1
	`, id)
	return err
}

func (r *webhookEventRepo) FindByChannelEventID(ctx context.Context, channel, eventID string) (*model.WebhookEvent, error) {
	var ev model.WebhookEvent
	err := r.db.GetContext(ctx, &ev, `
		SELECT * FROM webhook_events WHERE channel = $1 AND event_id = $2
	`, channel, eventID)
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// ============================================================================
// AuditLogRepo (design doc §AuditLog)
// ============================================================================

type AuditLogRepo interface {
	Insert(ctx context.Context, a *model.AuditLog) error
}

type auditLogRepo struct{ db *sqlx.DB }

func NewAuditLogRepo(db *sqlx.DB) *auditLogRepo { return &auditLogRepo{db: db} }

func (r *auditLogRepo) Insert(ctx context.Context, a *model.AuditLog) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target, tags, context)
		VALUES (:actor, :action, :target, :tags, :context)
	`, a)
	return err
}

// Compile-time interface conformance assertions.
var (
	_ OrderRepo      = (*orderRepo)(nil)
	_ PaymentRepo    = (*paymentRepo)(nil)
	_ RefundRepo     = (*refundRepo)(nil)
	_ WebhookEventRepo = (*webhookEventRepo)(nil)
	_ AuditLogRepo   = (*auditLogRepo)(nil)
)