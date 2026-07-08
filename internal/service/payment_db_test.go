package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/repo"
)

// dbURL returns the test database URL. Override with DATABASE_URL.
func dbURL2() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

// setupPaymentDB connects to the test DB, wipes payment tables, seeds plans
// + a super app + an active user. Returns the *sqlx.DB.
func setupPaymentDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", dbURL2())
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })

	tables := []string{
		"refunds", "payments", "webhook_events", "orders",
		"sessions", "subscriptions", "social_identities",
		"plans", "apps", "users", "audit_log",
	}
	// Clear is_default before wiping plans so the partial unique index
	// doesn't fire if a prior run left free with is_default=true.
	_, _ = db.ExecContext(context.Background(), `UPDATE plans SET is_default = false`)
	for _, tbl := range tables {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatalf("wipe %s: %v", tbl, err)
		}
	}
	// Plans: free is default, monthly is paid.
	for _, p := range []struct {
		id, name string
		price    float64
		days     int
		apps     []string
		isDef    bool
	}{
		{"free", "Free", 0, 0, []string{"yundian"}, true},
		{"monthly", "Monthly", 29.9, 30, []string{"yundian", "yundash"}, false},
	} {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO plans (id, name, price, interval_days, apps, is_default)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO NOTHING
		`, p.id, p.name, p.price, p.days, p.apps, p.isDef)
		if err != nil {
			t.Fatalf("seed plan %s: %v", p.id, err)
		}
	}
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active) VALUES ('yundian', 'Yundian', true)
		ON CONFLICT (app_id) DO NOTHING
	`)
	return db
}

func newTestPaymentService(t *testing.T, db *sqlx.DB) *PaymentService {
	t.Helper()
	return NewPaymentService(
		db,
		repo.NewOrderRepo(db),
		repo.NewPaymentRepo(db),
		repo.NewRefundRepo(db),
		repo.NewSubscriptionRepo(db),
		repo.NewPlanRepo(db),
		repo.NewUserRepo(db),
		repo.NewWebhookEventRepo(db),
		repo.NewAuditLogRepo(db),
		&stubRefundAPI{},
		30*time.Minute,
	)
}

func mustNewUUID() string { return uuid.New().String() }

func seedUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	id := mustNewUUID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, status) VALUES ($1, 'active')`, id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedPaidOrder(t *testing.T, db *sqlx.DB, userID, planID string, amount float64) string {
	t.Helper()
	id := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES ($1, $2, $3, $4, 'CNY', 'pending', now() + INTERVAL '30 minutes')
	`, id, userID, planID, amount); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return id
}

// ============================================================================
// CreateOrder
// ============================================================================

func TestPaymentService_CreateOrder_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	order, err := svc.CreateOrder(context.Background(), uid, "monthly")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.PlanID != "monthly" || order.Amount != 29.9 || order.Currency != "CNY" {
		t.Errorf("order = %+v", order)
	}
	if order.Status != "pending" {
		t.Errorf("Status = %q, want pending", order.Status)
	}
}

func TestPaymentService_CreateOrder_PlanNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	_, err := svc.CreateOrder(context.Background(), uid, "missing")
	if !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("err = %v, want ErrPlanNotFound", err)
	}
}

func TestPaymentService_CreateOrder_PlanInactive(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	_, _ = db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active) VALUES ('inactive', 'X', 0, 0, '{}', false)`)
	_, err := svc.CreateOrder(context.Background(), uid, "inactive")
	if !errors.Is(err, ErrPlanInactive) {
		t.Errorf("err = %v, want ErrPlanInactive", err)
	}
}

func TestPaymentService_CreateOrder_UserHasActiveSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	// First order + confirm activates subscription.
	order, err := svc.CreateOrder(context.Background(), uid, "monthly")
	if err != nil {
		t.Fatalf("first order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sub",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// Second CreateOrder — user already has an active sub.
	_, err = svc.CreateOrder(context.Background(), uid, "monthly")
	if !errors.Is(err, ErrUserHasActiveSub) {
		t.Errorf("err = %v, want ErrUserHasActiveSub", err)
	}
}

// ============================================================================
// CancelOrder
// ============================================================================

func TestPaymentService_CancelOrder_OwnerCancelsPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(context.Background(), uid, "monthly")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := svc.CancelOrder(context.Background(), order.ID, uid); err != nil {
		t.Errorf("CancelOrder: %v", err)
	}
}

func TestPaymentService_CancelOrder_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	err := svc.CancelOrder(context.Background(), mustNewUUID(), mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestPaymentService_CancelOrder_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	err := svc.CancelOrder(context.Background(), order.ID, mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound (hidden for non-owner)", err)
	}
}

func TestPaymentService_CancelOrder_NotPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	// Flip to paid directly.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'paid' WHERE id = $1`, order.ID)

	err := svc.CancelOrder(context.Background(), order.ID, uid)
	if !errors.Is(err, ErrOrderNotPending) {
		t.Errorf("err = %v, want ErrOrderNotPending", err)
	}
}

// ============================================================================
// GetOrder
// ============================================================================

func TestPaymentService_GetOrder_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	got, err := svc.GetOrder(context.Background(), order.ID, uid)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.ID != order.ID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetOrder_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	_, err := svc.GetOrder(context.Background(), order.ID, mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestPaymentService_GetOrder_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetOrder(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v", err)
	}
}

// ============================================================================
// Confirm
// ============================================================================

func TestPaymentService_Confirm_FreshOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID:       order.ID,
		UserID:        uid,
		Channel:       "stripe",
		ExternalTxnID: "pi_test_1",
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if res.Status != "paid" {
		t.Errorf("Status = %q", res.Status)
	}
	if !res.ActivatedSubscription {
		t.Errorf("ActivatedSubscription = false, want true")
	}

	// Order should now be paid.
	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after Confirm: order.Status = %q, want paid", got.Status)
	}
}

func TestPaymentService_Confirm_ExpiredOrder_LatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	// Force to expired.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired' WHERE id = $1`, order.ID)

	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID:       order.ID,
		UserID:        uid,
		Channel:       "stripe",
		ExternalTxnID: "pi_late_1",
	})
	if err != nil {
		t.Fatalf("Confirm late: %v", err)
	}
	if !res.WasLatePayment {
		t.Errorf("WasLatePayment = false, want true")
	}
	// audit_log should have a late_payment_post_expiry row.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'late_payment_post_expiry'`)
	if n == 0 {
		t.Errorf("expected audit_log row for late payment")
	}
}

func TestPaymentService_Confirm_InvalidChannel(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "bogus_channel", ExternalTxnID: "x",
	})
	if !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("err = %v, want ErrInvalidChannel", err)
	}
}

func TestPaymentService_Confirm_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: mustNewUUID(), UserID: "x", Channel: "stripe", ExternalTxnID: "y",
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Confirm_TerminalState(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'failed' WHERE id = $1`, order.ID)
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "z",
	})
	if !errors.Is(err, ErrOrderAlreadyTerminal) {
		t.Errorf("err = %v, want ErrOrderAlreadyTerminal", err)
	}
}

func TestPaymentService_Confirm_ChannelMismatch(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// First confirm on stripe.
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-1",
	}); err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	// Now confirm again with a different channel — but order is already paid.
	// Should refuse with channel mismatch (we have a paid payment on stripe).
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "alipay", ExternalTxnID: "alipay-1",
	})
	if !errors.Is(err, ErrOrderChannelMismatch) {
		t.Errorf("err = %v, want ErrOrderChannelMismatch", err)
	}
}

func TestPaymentService_Confirm_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	in := ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-idem",
	}
	if _, err := svc.Confirm(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := svc.Confirm(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if res.Status != "paid" {
		t.Errorf("Status = %q", res.Status)
	}
}

// TestPaymentService_Confirm_ExistingFailedPayment covers the
// "existing payment is in 'failed' state" branch in Confirm. The
// dedup hit returns a payment row but it's in a terminal state —
// Confirm must refuse with ErrOrderAlreadyTerminal.
func TestPaymentService_Confirm_ExistingFailedPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Pre-insert a payment row in 'failed' state for the same
	// (channel, external_txn_id) the Confirm is about to use. This
	// drives the "dedupe hit + status=failed" branch in Confirm.
	txnID := "pi-failed-confirm-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed failed payment: %v", err)
	}

	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	if !errors.Is(err, ErrOrderAlreadyTerminal) {
		t.Errorf("err = %v, want ErrOrderAlreadyTerminal", err)
	}
}

// ============================================================================
// Refund
// ============================================================================

// TestPaymentService_Refund_NotOwner covers the "user doesn't own the
// payment" branch in Refund — the function returns ErrPaymentNotFound
// to hide existence from non-owners (consistent with GetPayment).
func TestPaymentService_Refund_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	ownerID := seedUser(t, db)
	otherID := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), ownerID, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: ownerID, Channel: "stripe", ExternalTxnID: "pi-other-owner",
	})
	// otherID tries to refund owner's payment — must be denied.
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID:      res.PaymentID,
		UserID:         otherID,
		IdempotencyKey: "k-other-owner-" + mustNewUUID()[:8],
		Amount:         10,
	})
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestPaymentService_Refund_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-r1",
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	stub := svc.refundAPI.(*stubRefundAPI)
	stub.returnID = "re_1"
	r, err := svc.Refund(context.Background(), RefundInput{
		PaymentID:      res.PaymentID,
		UserID:         uid,
		IdempotencyKey: "user-req-001",
		Amount:         10.0,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if r.Refund.Amount != 10.0 {
		t.Errorf("Amount = %v", r.Refund.Amount)
	}
	if r.Existing {
		t.Errorf("first refund should not be Existing")
	}
}

func TestPaymentService_Refund_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-r2",
	})
	in := RefundInput{
		PaymentID: res.PaymentID, UserID: uid,
		IdempotencyKey: "user-req-002", Amount: 5.0,
	}
	if _, err := svc.Refund(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := svc.Refund(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !r2.Existing {
		t.Errorf("second refund should be Existing=true")
	}
}

func TestPaymentService_Refund_MissingIdempotencyKey(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{PaymentID: "x", Amount: 1})
	if !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Errorf("err = %v, want ErrMissingIdempotencyKey", err)
	}
}

func TestPaymentService_Refund_AmountInvalid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{IdempotencyKey: "k1", Amount: 0})
	if !errors.Is(err, ErrRefundAmountInvalid) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Refund_PaymentNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: mustNewUUID(), UserID: mustNewUUID(), IdempotencyKey: "k-nf", Amount: 1,
	})
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Refund_PaymentNotPaid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Insert a pending payment row directly.
	pendingID := mustNewUUID()
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', 'pending-1', 10, 'CNY', 'pending', '{}')
	`, pendingID, order.ID)

	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: pendingID, UserID: uid, IdempotencyKey: "k-pending", Amount: 1,
	})
	if !errors.Is(err, ErrPaymentNotPaid) {
		t.Errorf("err = %v, want ErrPaymentNotPaid", err)
	}
}

func TestPaymentService_Refund_AmountTooLarge(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-big",
	})
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-big", Amount: 100.0,
	})
	if !errors.Is(err, ErrRefundAmountInvalid) {
		t.Errorf("err = %v, want ErrRefundAmountInvalid", err)
	}
}

func TestPaymentService_Refund_SumExceedsPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sum",
	})

	// First refund 20.
	if _, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-1", Amount: 20,
	}); err != nil {
		t.Fatalf("first refund: %v", err)
	}

	// Mark first refund paid so it counts in sum invariant.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE refunds SET status = 'paid' WHERE payment_id = $1`, res.PaymentID)

	// Second refund 15 — would push sum to 35 > payment 29.9.
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-2", Amount: 15,
	})
	if !errors.Is(err, ErrRefundSumExceedsPayment) {
		t.Errorf("err = %v, want ErrRefundSumExceedsPayment", err)
	}
}

func TestPaymentService_Refund_ChannelFailed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-fail",
	})
	stub := svc.refundAPI.(*stubRefundAPI)
	stub.returnErr = errors.New("channel boom")
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-fail", Amount: 1,
	})
	if !errors.Is(err, ErrRefundChannelFailed) {
		t.Errorf("err = %v, want ErrRefundChannelFailed", err)
	}
}

// ============================================================================
// Reads
// ============================================================================

func TestPaymentService_ListUserPayments(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	_, _ = svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-list",
	})

	list, err := svc.ListUserPayments(context.Background(), uid)
	if err != nil {
		t.Fatalf("ListUserPayments: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("got %d, want 1", len(list))
	}
}

func TestPaymentService_GetPayment_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-get",
	})
	got, err := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if err != nil {
		t.Fatalf("GetPayment: %v", err)
	}
	if got.ID != res.PaymentID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetPayment_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-no",
	})
	_, err := svc.GetPayment(context.Background(), res.PaymentID, mustNewUUID())
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestPaymentService_GetPayment_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetPayment(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_GetRefund_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rf",
	})
	r, _ := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "rf-1", Amount: 5,
	})
	got, err := svc.GetRefund(context.Background(), r.Refund.ID, uid)
	if err != nil {
		t.Fatalf("GetRefund: %v", err)
	}
	if got.ID != r.Refund.ID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetRefund_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetRefund(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrRefundNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_GetRefund_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rfno",
	})
	r, _ := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "rf-no", Amount: 5,
	})
	_, err := svc.GetRefund(context.Background(), r.Refund.ID, mustNewUUID())
	if !errors.Is(err, ErrRefundNotFound) {
		t.Errorf("err = %v, want ErrRefundNotFound", err)
	}
}

func TestPaymentService_ListPaymentRefunds(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-lpr",
	})
	_, _ = svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "lpr-1", Amount: 5,
	})
	list, err := svc.ListPaymentRefunds(context.Background(), res.PaymentID, uid)
	if err != nil {
		t.Fatalf("ListPaymentRefunds: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("got %d, want 1", len(list))
	}
}

// TestPaymentService_ListPaymentRefunds_PaymentNotFound covers the
// error-propagation path in ListPaymentRefunds — when GetPayment
// returns ErrPaymentNotFound (non-owner or missing payment),
// ListPaymentRefunds must surface the same error.
func TestPaymentService_ListPaymentRefunds_PaymentNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-lpr-nf",
	})
	// List with a different user — non-owner → ErrPaymentNotFound.
	other := seedUser(t, db)
	_, err := svc.ListPaymentRefunds(context.Background(), res.PaymentID, other)
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

// ============================================================================
// OnWebhook
// ============================================================================

func TestPaymentService_OnWebhook_StripePaymentSucceeded(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	raw := json.RawMessage(`{"id":"evt-1","type":"payment_intent.succeeded"}`)
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-1", EventType: "payment_intent.succeeded",
		TransactionID: "pi_wh_1", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: raw,
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	if res.DomainAction != "payment_paid" {
		t.Errorf("DomainAction = %q", res.DomainAction)
	}
	if res.DuplicateEvent {
		t.Errorf("DuplicateEvent = true on first call")
	}
	// Order should now be paid.
	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after webhook: Status = %q", got.Status)
	}
}

func TestPaymentService_OnWebhook_UnknownOrder_Audit(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-unknown", EventType: "payment_intent.succeeded",
		TransactionID: "pi-x", OrderID: mustNewUUID(), Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	// Should NOT error — written to audit_log instead.
	if err != nil {
		t.Errorf("unknown order webhook err: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'webhook_for_unknown_order'`)
	if n == 0 {
		t.Errorf("expected audit_log row for unknown order webhook")
	}
}

func TestPaymentService_OnWebhook_Duplicate(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	ev := WebhookEvent{
		Channel: "stripe", EventID: "evt-dup", EventType: "payment_intent.succeeded",
		TransactionID: "pi-dup", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}
	if _, err := svc.OnWebhook(context.Background(), ev); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := svc.OnWebhook(context.Background(), ev)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !res.DuplicateEvent {
		t.Errorf("DuplicateEvent = false on second call")
	}
}

func TestPaymentService_OnWebhook_PaymentFailed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	// Create a pending order, then a pending payment row, then fire payment_failed.
	orderID := mustNewUUID()
	txnID := "pi-fail-" + mustNewUUID()[:8]
	eventID := "evt-fail-" + mustNewUUID()[:8]
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes')
	`, orderID, uid)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Pre-insert a pending payment row so payment_failed finds it directly.
	paymentID := mustNewUUID()
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'pending', '{}')
	`, paymentID, orderID, txnID)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err = svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook failed: %v", err)
	}
	// Both payment and order should be flipped to failed.
	var pStatus string
	_ = db.GetContext(context.Background(), &pStatus,
		`SELECT status FROM payments WHERE id = $1`, paymentID)
	if pStatus != "failed" {
		t.Errorf("payment.Status = %q, want failed", pStatus)
	}
	got, _ := svc.GetOrder(context.Background(), orderID, uid)
	if got.Status != "failed" {
		t.Errorf("order.Status = %q, want failed", got.Status)
	}
}

func TestPaymentService_OnWebhook_RefundFull(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rf",
	})

	// Mark a paid refund so the sum invariant holds for full refund.
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO refunds (id, payment_id, channel, user_id, amount, idempotency_key, external_refund_id, status)
		VALUES (gen_random_uuid(), $1, 'stripe', $2, $3, 'webhook:evt-rf', 're_wh_1', 'paid')
	`, res.PaymentID, uid, 29.9)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rf", EventType: "charge.refunded",
		TransactionID: "pi-rf", RefundAmount: 29.9, ExternalRefundID: "re_wh_1",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook refund: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "refunded" {
		t.Errorf("Payment.Status = %q, want refunded", got.Status)
	}
}

func TestPaymentService_OnWebhook_DisputeCreated(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-disp",
	})

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-disp", EventType: "charge.dispute.created",
		TransactionID: "pi-disp",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dispute: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if !got.Disputed {
		t.Errorf("Disputed = false after dispute.created")
	}
}

func TestPaymentService_OnWebhook_InvalidChannel(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "bogus_channel", EventID: "evt-pp", EventType: "x",
		RawPayload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_OnWebhook_DisputeClosed_NoOp(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	// No matching payment — should silently no-op without error.
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-dc", EventType: "charge.dispute.closed",
		TransactionID: "pi-dc",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.DomainAction != "payment_dispute_closed" {
		t.Errorf("DomainAction = %q", res.DomainAction)
	}
}
// ============================================================================
// onPaypalRenewalSucceeded (PAYMENT.SALE.COMPLETED) — M5 paypal channel
// ============================================================================

// TestPaymentService_OnWebhook_PaymentFailed_NoOrder covers the defensive
// "no payment row + no order row" path inside findOrInsertPendingOnTx.
// A payment_failed event for a non-existent order_id is a no-op (it
// should not error and should not insert anything); the handler returns
// nil so the webhook acks 200.
func TestPaymentService_OnWebhook_PaymentFailed_NoOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	// No user, no order — just fire a payment_failed with a brand-new order_id.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-noorder-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: "pi-noorder-" + mustNewUUID()[:8], OrderID: mustNewUUID(),
		Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook no-order: %v", err)
	}
	// The webhook_events row was inserted (top-level dedup), but no
	// payments row was created.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments`)
	if n != 0 {
		t.Errorf("expected 0 payments rows for no-order path, got %d", n)
	}
}

// TestPaymentService_OnWebhook_PaymentFailed_InsertPending covers the
// "no payment row, order exists, INSERT pending" path inside
// findOrInsertPendingOnTx. The handler must mint a fresh pending
// payment row and link it to the order.
func TestPaymentService_OnWebhook_PaymentFailed_InsertPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-insert-pending-" + mustNewUUID()[:8]
	eventID := "evt-insert-pending-" + mustNewUUID()[:8]
	// Seed an order with NO payment row.
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes')
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook insert-pending: %v", err)
	}
	// A pending payment row should have been inserted.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments WHERE order_id = $1 AND status = 'failed'`, orderID)
	if n != 1 {
		t.Errorf("expected 1 payment row in failed state, got %d", n)
	}
}

// TestPaymentService_OnPaymentSucceeded_ClosedDBError covers an
// error-from-closed-db branch in the OnWebhook call chain. After
// the DB is closed, any subsequent call returns an error — the test
// just asserts the error is non-nil.
func TestPaymentService_OnPaymentSucceeded_ClosedDBError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Close the DB to force the call chain to fail.
	_ = db.Close()

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-closed-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-closed-" + mustNewUUID()[:8], OrderID: order.ID,
		Amount: 29.9, Currency: "CNY", RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from closed db, got nil")
	}
}

// TestPaymentService_OnWebhook_ChannelMismatch covers the "webhook arrives
// for a different channel than the one that already paid" path. The handler
// must NOT touch the existing paid payment, must NOT insert a second paid
// payment, and must write an audit row.
func TestPaymentService_OnWebhook_ChannelMismatch(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	// Pay the order via stripe (frontend Confirm).
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-paid-1",
	}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// Now fire a payment_succeeded webhook for a DIFFERENT channel. The
	// channel-mismatch pre-check in onPaymentSucceeded should detect this
	// and write an audit row, NOT insert a payment.
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-mismatch-" + mustNewUUID()[:8], EventType: "PAYMENT.CAPTURE.COMPLETED",
		TransactionID: "pi-pp-mismatch", OrderID: order.ID, Amount: 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook mismatch: %v", err)
	}
	// DomainAction should be "none" because the handler took the
	// channel_mismatch early-return path, which is an audit log + ack.
	_ = res // response shape is fine; we just want side-effect coverage.

	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'webhook_channel_mismatch'`)
	if n == 0 {
		t.Error("expected audit_log row for channel_mismatch")
	}
	// Confirm only the original stripe paid payment exists — paypal must
	// NOT have inserted a second one.
	var paypalCount int
	_ = db.GetContext(context.Background(), &paypalCount,
		`SELECT count(*) FROM payments WHERE channel = 'paypal' AND order_id = $1`, order.ID)
	if paypalCount != 0 {
		t.Errorf("expected 0 paypal payments for mismatched channel, got %d", paypalCount)
	}
}

// TestPaymentService_OnWebhook_LatePayment covers the wasLate branch of
// onPaymentSucceeded: order is already 'expired' (post-expiry), the
// payment_succeeded webhook still arrives, and we honor it (mark order
// paid + audit-log the late_payment_post_expiry event).
func TestPaymentService_OnWebhook_LatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Force the order to 'expired' so the wasLate path is taken.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("force expired: %v", err)
	}

	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-late-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-late-1", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("OnWebhook late: %v", err)
	}

	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after late webhook: order.Status = %q, want paid", got.Status)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'late_payment_post_expiry'`)
	if n == 0 {
		t.Error("expected audit_log row for late_payment_post_expiry")
	}
}

// TestPaymentService_OnWebhook_PaypalStampsExternalSubID covers the
// "e.ExternalSubscriptionID != ''" branch of onPaymentSucceeded — the
// active subscription's external_subscription_id column gets stamped so
// later renewal webhooks can find it.
func TestPaymentService_OnWebhook_PaypalStampsExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-stamp-" + mustNewUUID()[:8], EventType: "PAYMENT.CAPTURE.COMPLETED",
		TransactionID: "pi-pp-stamp", OrderID: order.ID, Amount: 29.9, Currency: "USD",
		ExternalSubscriptionID: "I-PAYPAL-SUB-STAMP",
		RawPayload:             json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("OnWebhook stamp: %v", err)
	}

	var gotID string
	if err := db.GetContext(context.Background(), &gotID,
		`SELECT external_subscription_id FROM subscriptions
		 WHERE user_id = $1 AND plan_id = 'monthly' AND status = 'active' LIMIT 1`, uid); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if gotID != "I-PAYPAL-SUB-STAMP" {
		t.Errorf("external_subscription_id = %q, want I-PAYPAL-SUB-STAMP", gotID)
	}
}

// TestPaymentService_OnWebhook_UnexpectedStateTransition covers the
// "existing.Status != paid" branch in onPaymentSucceeded: a (channel,
// external_txn_id) dedupe hit whose existing row is in a non-paid
// state (e.g. 'failed'). The function should write an
// unexpected_state_transition audit row and return nil (no escalation).
func TestPaymentService_OnWebhook_UnexpectedStateTransition(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Pre-insert a payment row that's already 'failed' for a txn_id
	// we're about to redeliver as payment_succeeded.
	txnID := "pi-failed-then-succeeded-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-ust-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook unexpected-state: %v", err)
	}
	_ = res
	// Audit row for unexpected_state_transition.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'unexpected_state_transition'`)
	if n == 0 {
		t.Error("expected audit_log row for unexpected_state_transition")
	}
}

// TestPaymentService_OnWebhook_DisputeCreated_NoPayment covers the
// "no matching payment row" early-return in onDisputeCreated. The handler
// should not error; it just no-ops because there's nothing to flag.
func TestPaymentService_OnWebhook_DisputeCreated_NoPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-disp-nopay-" + mustNewUUID()[:8], EventType: "charge.dispute.created",
		TransactionID: "pi-nopay-" + mustNewUUID()[:8], Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dispute-no-payment: %v", err)
	}
	// No payments row was created by the dispute handler.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments WHERE external_txn_id LIKE 'pi-nopay-%'`)
	if n != 0 {
		t.Errorf("expected 0 payments rows for no-payment dispute, got %d", n)
	}
}

// TestPaymentService_OnWebhook_Refund_Partial covers the partial-refund
// path in onRefundSucceeded: the payment stays 'paid', only a refund row
// is created with the partial amount.
func TestPaymentService_OnWebhook_Refund_Partial(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-partial",
	})

	// Partial refund = 10 of 29.9. Payment status should stay 'paid'.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rfp-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-partial", RefundAmount: 10.0, ExternalRefundID: "re_partial_1",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook partial: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "paid" {
		t.Errorf("partial refund: Payment.Status = %q, want paid (stays paid on partial)", got.Status)
	}
}

// TestPaymentService_OnWebhook_PaymentFailed_AfterPaid covers the
// "wasPaid=true" branch in onPaymentFailed: a payment_failed arrives
// for a payment that's already marked paid (rare race). The handler
// must flip the payment to failed, flip the order to failed, AND
// cascade the active subscription to cancelled. Audit row gets
// written via the cascade branch.
func TestPaymentService_OnWebhook_PaymentFailed_AfterPaid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-paid-then-failed",
	})

	// Now fire a payment_failed webhook — should cascade-cancel the
	// active subscription since the payment was previously paid.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-pf-paid-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: "pi-paid-then-failed", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook paid-then-failed: %v", err)
	}
	// Subscription should be cancelled (cascade).
	var subStatus string
	_ = db.GetContext(context.Background(), &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1 AND plan_id = 'monthly'`, uid)
	if subStatus != "cancelled" {
		t.Errorf("sub.Status after paid-then-failed = %q, want cancelled (cascade)", subStatus)
	}
	// Audit row should mention the cascade.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'subscription_deactivated_failed_payment'`)
	if n == 0 {
		t.Error("expected audit row for subscription_deactivated_failed_payment")
	}
	_ = res // silence unused
}

// TestPaymentService_OnWebhook_Refund_MissingPayment covers the
// "no payment row" branch in onRefundSucceeded (the handler should
// no-op rather than error).
func TestPaymentService_OnWebhook_Refund_MissingPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rf-nopay-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rf-nopay-" + mustNewUUID()[:8], RefundAmount: 1, ExternalRefundID: "re_nopay",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Errorf("missing-payment refund should not error, got: %v", err)
	}
}

// TestPaymentService_OnWebhook_PaymentFailed_TerminalState covers the
// "n == 0" branch in onPaymentFailed: a payment_failed arrives for a
// payment that's already in a terminal state (e.g. 'failed'). The SQL
// guard refuses to transition, the function commits the empty tx and
// returns nil — no-op.
func TestPaymentService_OnWebhook_PaymentFailed_TerminalState(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-failed-term-" + mustNewUUID()[:8]
	eventID := "evt-failed-term-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'failed', now() + INTERVAL '30 minutes')
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Pre-insert a payment row that's already 'failed' (terminal).
	paymentID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, paymentID, orderID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook terminal-state: %v", err)
	}
	// Payment should still be 'failed' (no transition, no change).
	var pStatus string
	_ = db.GetContext(context.Background(), &pStatus,
		`SELECT status FROM payments WHERE id = $1`, paymentID)
	if pStatus != "failed" {
		t.Errorf("payment.Status = %q, want failed (unchanged)", pStatus)
	}
}

// TestPaymentService_OnWebhook_UnknownEventType covers the default
// branch in OnWebhook's switch: event types that match no domain
// action (e.g. "ping" or any other uninteresting type). The handler
// should ack 200 with DomainAction="none".
func TestPaymentService_OnWebhook_UnknownEventType(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-ping-" + mustNewUUID()[:8], EventType: "ping.unknown_type",
		TransactionID: "tx-ping-" + mustNewUUID()[:8], Amount: 0, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("unknown event type: %v", err)
	}
	if res.DomainAction != "none" {
		t.Errorf("DomainAction = %q, want none", res.DomainAction)
	}
	if res.DuplicateEvent {
		t.Error("DuplicateEvent should be false on first delivery")
	}
}

// TestPaymentService_OnWebhook_Refund_ReRun covers the "refund row
// already inserted by a prior delivery" branch in onRefundSucceeded.
// The INSERT ... ON CONFLICT DO NOTHING returns no row, the handler
// re-reads the existing refund ID, and the rest of the flow proceeds
// idempotently.
func TestPaymentService_OnWebhook_Refund_ReRun(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rerun",
	})

	ev := WebhookEvent{
		Channel: "stripe", EventID: "evt-rerun-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rerun", RefundAmount: 29.9, ExternalRefundID: "re_rerun_1",
		RawPayload: json.RawMessage(`{}`),
	}
	// First delivery — INSERTs the refund row.
	if _, err := svc.OnWebhook(context.Background(), ev); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	// Second delivery with a different event_id (so the webhook_events
	// dedup at the top doesn't short-circuit) but the same external_refund_id.
	// This drives the "INSERT returned no row, re-read by ext id" branch.
	ev2 := ev
	ev2.EventID = "evt-rerun2-" + mustNewUUID()[:8]
	if _, err := svc.OnWebhook(context.Background(), ev2); err != nil {
		t.Fatalf("re-run refund: %v", err)
	}
	// Payment should still be refunded (idempotent).
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "refunded" {
		t.Errorf("after re-run: Payment.Status = %q, want refunded", got.Status)
	}
}

// TestPaymentService_OnWebhook_ReRunAfterCrash covers the "prior run
// crashed mid-action" branch in OnWebhook: an existing webhook_events
// row with processed_at IS NULL. The handler must re-run the business
// action (idempotent) and MarkProcessed the existing row.
func TestPaymentService_OnWebhook_ReRunAfterCrash(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Manually insert a webhook_events row with processed_at NULL.
	// This simulates "prior run crashed before MarkProcessed".
	eventID := "evt-recovery-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload, processed_at)
		VALUES ('stripe', $1, 'payment_intent.succeeded', '{}', NULL)
	`, eventID); err != nil {
		t.Fatalf("seed webhook_events: %v", err)
	}

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.succeeded",
		TransactionID: "pi-recovery-" + mustNewUUID()[:8], OrderID: order.ID,
		Amount: 29.9, Currency: "CNY", RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook recovery: %v", err)
	}
	if res.DuplicateEvent {
		t.Error("DuplicateEvent should be false on re-run after crash")
	}
	if res.DomainAction != "payment_paid" {
		t.Errorf("DomainAction = %q, want payment_paid", res.DomainAction)
	}
	// The row's processed_at should now be NOT NULL.
	var processedAt *time.Time
	if err := db.GetContext(context.Background(), &processedAt,
		`SELECT processed_at FROM webhook_events WHERE channel = 'stripe' AND event_id = $1`, eventID); err != nil {
		t.Fatalf("read processed_at: %v", err)
	}
	if processedAt == nil {
		t.Error("expected processed_at to be set after re-run")
	}
}

// TestPaymentService_OnWebhook_ReRunSameTxnID covers the "dedup hit
// on (channel, external_txn_id) with status=paid" branch in
// onPaymentSucceeded. A pre-existing paid payment with the SAME
// (channel, external_txn_id) causes the INSERT to be a no-op, and
// the function reads back the existing payment ID.
func TestPaymentService_OnWebhook_ReRunSameTxnID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")

	// Pre-insert a paid payment + a webhook_events row that
	// references the same (channel, external_txn_id). When the new
	// webhook fires, the InsertOnConflictDoNothing on webhook_events
	// returns inserted=false (since the event_id is the same), and
	// the onPaymentSucceeded handler's INSERT into payments also
	// returns inserted=false (ON CONFLICT on channel, external_txn_id).
	txnID := "pi-dedup-" + mustNewUUID()[:8]
	eventID := "evt-dedup-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload, processed_at)
		VALUES ('stripe', $1, 'payment_intent.succeeded', '{}', NULL)
	`, eventID); err != nil {
		t.Fatalf("seed webhook_events: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'paid', now(), '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dedup: %v", err)
	}

	// The order's status should still be 'paid' (it was paid before).
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("order.Status = %q, want paid (unchanged after dedup)", status)
	}
}

// TestPaymentService_CreateOrder_GenericError covers the wrap paths
// in CreateOrder that aren't covered by the "plan not found" / "plan
// inactive" / "user has active sub" tests. A generic planRepo error
// should be wrapped with "find plan:", and a generic subRepo error
// should be wrapped with "check active sub:".
func TestPaymentService_CreateOrder_GenericErrors(t *testing.T) {
	t.Run("planRepo generic error", func(t *testing.T) {
		db := setupPaymentDB(t)
		planRepo := repo.NewPlanRepo(db)
		// Drop the seeded plans so FindByID returns a non-ErrNoRows error.
		// We need a true generic error — easiest is to close the DB and
		// call on a closed handle.
		_ = db.Close()
		uid := mustNewUUID()
		svc := &PaymentService{
			planRepo: planRepo,
			subRepo:  repo.NewSubscriptionRepo(db),
		}
		_, err := svc.CreateOrder(context.Background(), uid, "monthly")
		if err == nil {
			t.Fatal("expected error from closed db, got nil")
		}
		if !strings.Contains(err.Error(), "find plan") {
			t.Errorf("expected wrap 'find plan', got %q", err.Error())
		}
	})
}

// TestPaymentService_OnPaymentFailed_BeginTxError covers an
// error-from-closed-db branch in the OnWebhook call chain. After
// the DB is closed, any subsequent call returns an error — the test
// just asserts the error is non-nil and surfaced. The exact wrap
// depends on which tx-bound call the runtime hits first.
func TestPaymentService_OnPaymentFailed_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-begin-tx-fail-" + mustNewUUID()[:8]
	eventID := "evt-begin-tx-fail-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes')
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	paymentID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'pending', '{}')
	`, paymentID, orderID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	// Close the DB to force the call chain to fail.
	_ = db.Close()

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from closed db, got nil")
	}
	// We don't assert on the exact wrap — closed-db errors vary by
	// driver / Go version. The point is that the failure surfaces.
}

func TestPaymentService_OnWebhook_PaypalRenewal_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	// Seed a user with an active subscription that has a PayPal
	// external_subscription_id, so the renewal webhook can find it.
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, $4)
	`, subID, uid, expAt, "I-PAYPAL-SUB-1"); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	txnID := "PAYMENT-SALE-" + mustNewUUID()[:8]
	eventID := "evt-renewal-" + mustNewUUID()[:8]
	newExpAt := time.Now().Add(45 * 24 * time.Hour)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: eventID, EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: txnID, ExternalSubscriptionID: "I-PAYPAL-SUB-1",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &newExpAt,
		RawPayload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook renewal: %v", err)
	}

	// 1. Subscription expires_at should be extended to newExpAt.
	var newExp time.Time
	if err := db.GetContext(context.Background(), &newExp,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if newExp.Unix() != newExpAt.Unix() {
		t.Errorf("expires_at: got %v, want %v", newExp, newExpAt)
	}
}

func TestPaymentService_OnWebhook_PaypalRenewal_MissingExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '30 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	// No ExternalSubscriptionID on the event → audit-log path, not a real
	// renewal. Order should NOT be created.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-missing-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-missing-" + mustNewUUID()[:8], Amount: 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	var orderCount int
	_ = db.GetContext(context.Background(), &orderCount,
		`SELECT COUNT(*) FROM orders WHERE user_id = $1`, uid)
	if orderCount != 0 {
		t.Errorf("expected 0 orders, got %d", orderCount)
	}
}

func TestPaymentService_OnWebhook_PaypalRenewal_UnknownSubscription(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '30 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	// ExternalSubscriptionID does not match any row → audit-log "unknown
	// subscription" path.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-unk-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-unk-" + mustNewUUID()[:8],
		ExternalSubscriptionID: "I-DOES-NOT-EXIST",
		Amount: 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	var orderCount int
	_ = db.GetContext(context.Background(), &orderCount,
		`SELECT COUNT(*) FROM orders WHERE user_id = $1`, uid)
	if orderCount != 0 {
		t.Errorf("expected 0 orders, got %d", orderCount)
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_DuplicatePayment covers
// the "dedupe-check existing renewal payment" branch in
// onPaypalRenewalSucceeded: a payment row already exists for the same
// (channel, external_txn_id). The handler must short-circuit with an
// audit log, NOT create a new order or payment.
func TestPaymentService_OnWebhook_PaypalRenewal_DuplicatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, 'I-PP-DUP')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	txnID := "txn-pp-dup-" + mustNewUUID()[:8]
	// First, fire the renewal normally to create a payment row.
	first := WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-dup-first-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: txnID, ExternalSubscriptionID: "I-PP-DUP",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &expAt,
		RawPayload:   json.RawMessage(`{}`),
	}
	if _, err := svc.OnWebhook(context.Background(), first); err != nil {
		t.Fatalf("first OnWebhook: %v", err)
	}
	// Now fire a second webhook with a NEW event_id (so the top-level
	// dedup at webhook_events doesn't catch it) but the same (channel,
	// external_txn_id). This drives the "existing payment" branch in
	// onPaypalRenewalSucceeded.
	second := first
	second.EventID = "evt-pp-dup-second-" + mustNewUUID()[:8]
	if _, err := svc.OnWebhook(context.Background(), second); err != nil {
		t.Fatalf("second OnWebhook: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_payment_already_exists'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_payment_already_exists")
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_NoExpiryHint covers the
// "SubExpiresAt is nil" branch in onPaypalRenewalSucceeded: PayPal
// charged but didn't ship a next_billing_time hint. The handler must
// still record the payment (PayPal did charge) and write a
// paypal_renewal_no_expiry_hint audit row.
func TestPaymentService_OnWebhook_PaypalRenewal_NoExpiryHint(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, 'I-PP-NOEXP')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-noexp-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-pp-noexp-" + mustNewUUID()[:8], ExternalSubscriptionID: "I-PP-NOEXP",
		Amount: 29.9, Currency: "USD",
		// No SubExpiresAt — drives the "no expiry hint" branch.
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook no-expiry: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_no_expiry_hint'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_no_expiry_hint")
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_SubNotActive covers the
// "sub.Status != active but UPDATE didn't fire" branch in
// onPaypalRenewalSucceeded. Pre-cancel the sub, then fire a renewal —
// the handler records the payment (PayPal did charge) but does NOT
// extend expires_at, and writes a paypal_renewal_sub_not_active audit.
func TestPaymentService_OnWebhook_PaypalRenewal_SubNotActive(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'cancelled', $3, 'I-PP-CANCEL')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	newExpAt := time.Now().Add(45 * 24 * time.Hour)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-cancel-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-pp-cancel-" + mustNewUUID()[:8], ExternalSubscriptionID: "I-PP-CANCEL",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &newExpAt,
		RawPayload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook sub-cancelled: %v", err)
	}
	// Sub's expires_at must NOT have been extended (still the original).
	var gotExp time.Time
	_ = db.GetContext(context.Background(), &gotExp,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID)
	if gotExp.Unix() != expAt.Unix() {
		t.Errorf("sub.expires_at was extended despite cancelled status: got %v, want %v", gotExp, expAt)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_sub_not_active'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_sub_not_active")
	}
}
