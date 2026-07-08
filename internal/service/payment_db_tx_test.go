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
}

func (f *fakeTx) GetContext(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
	return f.getErr
}
func (f *fakeTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) QueryRowxContext(_ context.Context, _ string, _ ...interface{}) *sqlx.Row {
	// Return nil — insertPaymentOnTx handles sql.ErrNoRows from
	// QueryRow.Scan and returns ("", false, nil) for the dedupe
	// case. The webhook handlers take the `!inserted` branch which
	// exits early without reaching the deeper exec paths.
	return nil
}
func (f *fakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) Commit() error   { return nil }
func (f *fakeTx) Rollback() error { return nil }

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
