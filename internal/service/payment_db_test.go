package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// ============================================================================
// Refund
// ============================================================================

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