package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// mockResult satisfies sql.Result for the fake txs.
type mockResult struct{}

func (mockResult) LastInsertId() (int64, error) { return 0, nil }
func (mockResult) RowsAffected() (int64, error) { return 1, nil }

// fakeTx implements the dbTx interface. Tests configure the error
// fields to drive specific error paths in the webhook handlers.
// QueryRowID returns rowID directly — a plain string, no fake
// *sqlx.Row needed (the QueryRowID refactor on the production
// side wraps QueryRowxContext+Scan into a single typed method).
type fakeTx struct {
	getErr  error
	execErr error
	rowID   string
	rowErr  error
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
func (f *fakeTx) QueryRowID(_ context.Context, _ string, _ ...interface{}) (string, error) {
	return f.rowID, f.rowErr
}
func (f *fakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) Commit() error   { return nil }
func (f *fakeTx) Rollback() error { return nil }

// countingFakeTx counts ExecContext + NamedExecContext calls and
// also tracks GetContext calls so tests can make the channel-mismatch
// check return sql.ErrNoRows (allowing the function to fall through
// to the INSERT / activate-sub / update-order path).
type countingFakeTx struct {
	*fakeTx
	t              *testing.T
	execCallCount  int
	execErrsAtCall map[int]error
	getCallCount   int
	getErrsAtCall  map[int]error
}

func (c *countingFakeTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	c.execCallCount++
	if err, ok := c.execErrsAtCall[c.execCallCount]; ok {
		return mockResult{}, err
	}
	return mockResult{}, nil
}

func (c *countingFakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	c.execCallCount++
	if err, ok := c.execErrsAtCall[c.execCallCount]; ok {
		return mockResult{}, err
	}
	return mockResult{}, nil
}

func (c *countingFakeTx) GetContext(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
	c.getCallCount++
	if err, ok := c.getErrsAtCall[c.getCallCount]; ok {
		return err
	}
	return nil
}

// ptrTime returns a *time.Time for use in test fixtures.
func ptrTime(t time.Time) *time.Time { return &t }

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
// branch in onDisputeCreated. The UPDATE after find-payment fails.
func TestOnDisputeCreated_SetDisputedError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	txnID := "pi-sde-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &fakeTx{
			rowID:   txnID,
			execErr: errors.New("synthetic set disputed failure"),
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

// TestOnPaymentSucceeded_ActivateSubError covers the "activate sub"
// error branch in onPaymentSucceeded. Fresh txn_id (no pre-seeded
// payment). The first two GetContext calls return sql.ErrNoRows
// (find-payment + channel-mismatch), so the function falls through
// to the INSERT path. Call 1 (INSERT in findOrInsertPendingOnTx)
// succeeds. Call 2 (activateSubscriptionOnTx) fails.
func TestOnPaymentSucceeded_ActivateSubError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	txnID := "pi-ase-" + mustNewUUID()[:8]
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			fakeTx: &fakeTx{rowID: txnID},
			execErrsAtCall: map[int]error{
				2: errors.New("synthetic activate sub failure"),
			},
			getErrsAtCall: map[int]error{
				2: sql.ErrNoRows, // channel-mismatch check returns no rows
			},
		}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-ase-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from activate sub failure, got nil")
	}
	if !strings.Contains(err.Error(), "activate sub") {
		t.Errorf("expected wrap 'activate sub', got %q", err.Error())
	}
}

// TestOnPaymentSucceeded_UpdateOrderError covers the "update order"
// error branch. The fake's activate-sub UPDATE reports RowsAffected=1
// (no INSERT runs) because the mock unconditionally returns 1; so
// call 1 = activate-sub UPDATE, call 2 = UPDATE-order UPDATE.
// Set call 2 to fail and assert "update order" in the wrapped error.
func TestOnPaymentSucceeded_UpdateOrderError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	txnID := "pi-uoe-" + mustNewUUID()[:8]
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			fakeTx: &fakeTx{rowID: txnID},
			execErrsAtCall: map[int]error{
				2: errors.New("synthetic update order failure"),
			},
			getErrsAtCall: map[int]error{
				2: sql.ErrNoRows, // channel-mismatch check returns no rows
			},
		}, nil
	}
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-uoe-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from update order failure, got nil")
	}
	if !strings.Contains(err.Error(), "update order") {
		t.Errorf("expected wrap 'update order', got %q", err.Error())
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
// branch. Pre-seeded payment, call 1 (mark failed UPDATE) fails.
func TestOnPaymentFailed_MarkFailedError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	txnID := "pi-mfe-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			fakeTx: &fakeTx{rowID: txnID},
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
// branch. Call 2 (UPDATE orders) fails.
func TestOnPaymentFailed_FlipOrderError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	txnID := "pi-floe-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &countingFakeTx{
			fakeTx: &fakeTx{rowID: txnID},
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
		TransactionID:          "pi-prb-" + mustNewUUID()[:8],
		ExternalSubscriptionID: "I-prb",
		Amount:                 29.9, Currency: "USD", RawPayload: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected wrap 'begin tx', got %q", err.Error())
	}
}
