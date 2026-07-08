package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// mockResult satisfies sql.Result for the fake txs.
type mockResult struct{}

func (mockResult) LastInsertId() (int64, error) { return 0, nil }
func (mockResult) RowsAffected() (int64, error) { return 1, nil }

// fakeTx implements the dbTx interface. Tests configure the error
// fields to drive specific error paths in the webhook handlers.
type fakeTx struct {
	getErr   error
	execErr  error
	commitErr error
}

func (f *fakeTx) GetContext(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
	return f.getErr
}
func (f *fakeTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) QueryRowxContext(_ context.Context, _ string, _ ...interface{}) *sqlx.Row {
	return &sqlx.Row{}
}
func (f *fakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) Commit() error   { return f.commitErr }
func (f *fakeTx) Rollback() error { return nil }

// countingFakeTx is like fakeTx but can fail specific call numbers.
// This lets us target a particular operation in a chain of ExecContext
// calls without breaking the earlier ones — essential for testing
// later-stage branches like "deactivate sub" and "write audit" that
// are only reached after multiple successful operations.
//
// The getErrsAtCall map controls per-call GetContext results — e.g.,
// the 2nd GetContext call (channel-mismatch check) can return
// sql.ErrNoRows so the function falls through to the INSERT path.
type countingFakeTx struct {
	execCallCount  int
	execErrsAtCall map[int]error
	getCallCount  int
	getErrsAtCall map[int]error
}

func (c *countingFakeTx) GetContext(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
	c.getCallCount++
	if err, ok := c.getErrsAtCall[c.getCallCount]; ok {
		return err
	}
	return nil
}
func (c *countingFakeTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	c.execCallCount++
	if err, ok := c.execErrsAtCall[c.execCallCount]; ok {
		return mockResult{}, err
	}
	return mockResult{}, nil
}
func (c *countingFakeTx) QueryRowxContext(_ context.Context, _ string, _ ...interface{}) *sqlx.Row {
	// Return sql.ErrNoRows to make insertPaymentOnTx return
	// ("", false, nil) — i.e. "no row inserted" (ON CONFLICT DO
	// NOTHING semantics). The function code then takes the
	// `if !inserted` branch which exits early with the existing
	// payment ID. This matches what happens when the INSERT hits
	// the UNIQUE(channel, external_txn_id) conflict in production.
	return &sqlx.Row{}
}
func (c *countingFakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	c.execCallCount++
	if err, ok := c.execErrsAtCall[c.execCallCount]; ok {
		return mockResult{}, err
	}
	return mockResult{}, nil
}
func (c *countingFakeTx) Commit() error   { return nil }
func (c *countingFakeTx) Rollback() error { return nil }

// TestOnDisputeCreated_BeginTxError covers the BeginTxx error branch
// in onDisputeCreated.
func TestOnDisputeCreated_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return nil, errors.New("synthetic begin tx failure")
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-btx-" + mustNewUUID()[:8], EventType: "charge.dispute.created",
		TransactionID: "pi-btx-" + mustNewUUID()[:8], Amount: 1, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}

// TestOnDisputeCreated_FindPaymentError covers the "find payment" error
// branch in onDisputeCreated.
func TestOnDisputeCreated_FindPaymentError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &fakeTx{getErr: errors.New("synthetic find payment failure")}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-fpe-" + mustNewUUID()[:8], EventType: "charge.dispute.created",
		TransactionID: "pi-fpe-" + mustNewUUID()[:8], Amount: 1, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from find payment failure, got nil")
	}
	if !strings.Contains(err.Error(), "find payment") {
		t.Errorf("expected wrap 'find payment', got %q", err.Error())
	}
}

// TestOnDisputeCreated_SetDisputedError covers the "set disputed" error
// branch in onDisputeCreated.
func TestOnDisputeCreated_SetDisputedError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	txnID := "pi-sde-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	// Use a counting fake that fails only the first exec call (the
	// "set disputed" UPDATE). The function's findOrInsertPendingOnTx
	// succeeds against the real DB; the UPDATE in onDisputeCreated
	// itself fails.
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			execErrsAtCall: map[int]error{
				1: errors.New("synthetic set disputed failure"),
			},
		}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-sde-" + mustNewUUID()[:8], EventType: "charge.dispute.created",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from set disputed failure, got nil")
	}
	if !strings.Contains(err.Error(), "set disputed") {
		t.Errorf("expected wrap 'set disputed', got %q", err.Error())
	}
}

// TestOnPaymentSucceeded_BeginTxError covers the BeginTxx error branch
// in onPaymentSucceeded.
func TestOnPaymentSucceeded_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return nil, errors.New("synthetic begin tx failure")
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-psb-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-psb-" + mustNewUUID()[:8], OrderID: mustNewUUID(),
		Amount: 29.9, Currency: "CNY", RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}

// TestOnPaymentFailed_BeginTxError covers the BeginTxx error branch
// in onPaymentFailed.
func TestOnPaymentFailed_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return nil, errors.New("synthetic begin tx failure")
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-pfb-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: "pi-pfb-" + mustNewUUID()[:8], OrderID: mustNewUUID(),
		Amount: 29.9, Currency: "CNY", RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}

// TestOnPaymentFailed_MarkFailedError covers the "mark failed" error
// branch in onPaymentFailed. The 1st exec call (UPDATE payments
// SET status='failed') fails.
func TestOnPaymentFailed_MarkFailedError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	txnID := "pi-mfe-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			execErrsAtCall: map[int]error{
				1: errors.New("synthetic mark failed failure"),
			},
		}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-mfe-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from mark failed failure, got nil")
	}
	if !strings.Contains(err.Error(), "mark failed") {
		t.Errorf("expected wrap 'mark failed', got %q", err.Error())
	}
}

// TestOnPaymentFailed_FlipOrderError covers the "flip order" error
// branch in onPaymentFailed. The 2nd exec call (UPDATE orders
// SET status='failed') fails.
func TestOnPaymentFailed_FlipOrderError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	txnID := "pi-floe-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			execErrsAtCall: map[int]error{
				2: errors.New("synthetic flip order failure"),
			},
		}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-floe-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from flip order failure, got nil")
	}
	if !strings.Contains(err.Error(), "flip order") {
		t.Errorf("expected wrap 'flip order', got %q", err.Error())
	}
}

// TestOnPaymentFailed_FindOrderError covers the "find order" error
// branch in onPaymentFailed.
func TestOnPaymentFailed_FindOrderError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	txnID := "pi-foe-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &fakeTx{getErr: errors.New("synthetic find order failure")}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-foe-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from find order failure, got nil")
	}
	if !strings.Contains(err.Error(), "find order") {
		t.Errorf("expected wrap 'find order', got %q", err.Error())
	}
}

// TestOnRefundSucceeded_BeginTxError covers the BeginTxx error branch
// in onRefundSucceeded.
func TestOnRefundSucceeded_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return nil, errors.New("synthetic begin tx failure")
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rsb-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rsb-" + mustNewUUID()[:8], OrderID: mustNewUUID(),
		Amount: 29.9, Currency: "CNY", ExternalRefundID: "re-rsb",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}

// TestOnPaypalRenewalSucceeded_BeginTxError covers the BeginTxx error
// branch in onPaypalRenewalSucceeded.
func TestOnPaypalRenewalSucceeded_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return nil, errors.New("synthetic begin tx failure")
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-prb-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "pi-prb-" + mustNewUUID()[:8],
		ExternalSubscriptionID: "I-prb",
		Amount: 29.9, Currency: "USD", RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}
