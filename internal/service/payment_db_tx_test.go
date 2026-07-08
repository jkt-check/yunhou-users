package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/jmoiron/sqlx"
)

// mockResult satisfies sql.Result for the fake txs.
type mockResult struct{}

func (mockResult) LastInsertId() (int64, error) { return 0, nil }
func (mockResult) RowsAffected() (int64, error) { return 1, nil }

// fakeTx implements the dbTx interface. Tests configure the error
// fields to drive specific error paths in the webhook handlers.
//
// QueryRowxContext returns a *sqlx.Row backed by a *sql.Rows that
// returns one string column (the value of rowID). rowErr, if non-nil,
// is what Row.Scan returns. We build the *sql.Rows via a custom
// in-process driver and inject it into sqlx.Row via reflection on
// the unexported `rows` field — the only way to construct a
// *sqlx.Row from outside the sqlx package.
type fakeTx struct {
	getErr error
	execErr error
	rowID  string
	rowErr error
}

func (f *fakeTx) GetContext(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
	return f.getErr
}
func (f *fakeTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) QueryRowxContext(_ context.Context, _ string, _ ...interface{}) *sqlx.Row {
	return makeFakeRow(f.rowID, f.rowErr)
}
func (f *fakeTx) NamedExecContext(_ context.Context, _ string, _ interface{}) (sql.Result, error) {
	return mockResult{}, f.execErr
}
func (f *fakeTx) Commit() error   { return nil }
func (f *fakeTx) Rollback() error { return nil }

// valueDriver is a minimal driver.Driver that returns a *sql.Rows
// carrying a single string column populated from a pre-set value.
// The driver is registered globally on first use and used to build
// the fake *sql.Rows injected into sqlx.Row.
type valueDriver struct {
	mu       sync.Mutex
	rowValue string
	rowErr   error
	counter  int
}

var valueDriverInstance = &valueDriver{}

func init() {
	sql.Register("yunhouUsersFakeTx", valueDriverInstance)
}

// Open returns a fresh conn per Open call. The conn's QueryContext
// returns a valueRows that yields one row of one string column.
func (d *valueDriver) Open(_ string) (driver.Conn, error) {
	d.mu.Lock()
	d.counter++
	name := fmt.Sprintf("yunhou-fake-tx-%d", d.counter)
	d.mu.Unlock()
	return &valueConn{d: d, name: name}, nil
}

// valueConn is a per-Open driver.Conn.
type valueConn struct {
	d    *valueDriver
	name string
}

func (c *valueConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, errors.New("Prepare not supported")
}
func (c *valueConn) Close() error { return nil }
func (c *valueConn) Begin() (driver.Tx, error) {
	return nil, errors.New("Begin not supported")
}
func (c *valueConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.mu.Lock()
	rv := c.d.rowValue
	re := c.d.rowErr
	c.d.mu.Unlock()
	return &valueRows{value: rv, err: re}, nil
}

// valueRows is a single-row driver.Rows.
type valueRows struct {
	value string
	err   error
	done  bool
}

func (r *valueRows) Columns() []string { return []string{"id"} }
func (r *valueRows) Close() error      { return nil }
func (r *valueRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	if r.err != nil {
		return r.err
	}
	dest[0] = r.value
	return nil
}

// makeFakeRow builds a *sqlx.Row whose Scan fills *dest[0] with id,
// or returns scanErr if non-nil. The construction uses a tiny
// in-process driver; the *sql.Rows is injected into sqlx.Row via
// reflection (the only way to do this from outside the sqlx package).
func makeFakeRow(id string, scanErr error) *sqlx.Row {
	// Set the current rowValue and rowErr on the global driver.
	valueDriverInstance.mu.Lock()
	valueDriverInstance.rowValue = id
	valueDriverInstance.rowErr = scanErr
	valueDriverInstance.mu.Unlock()

	conn, err := sql.Open("yunhouUsersFakeTx", "")
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(context.Background(), "SELECT id")
	if err != nil {
		// Driver-level error (e.g. rowErr injected before the query
		// ran). Build a row whose Scan returns that error.
		return buildErrRow(scanErr)
	}
	// Use reflection to set sqlx.Row's unexported `rows` field.
	row := &sqlx.Row{}
	v := reflect.ValueOf(row).Elem().FieldByName("rows")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(rows))
	return row
}

// buildErrRow returns a *sqlx.Row whose Scan returns err. We use this
// when the driver's Query itself errored (rare in our tests).
func buildErrRow(err error) *sqlx.Row {
	row := &sqlx.Row{}
	// Inject the error into the unexported `err` field.
	v := reflect.ValueOf(row).Elem().FieldByName("err")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(err))
	return row
}

var _ = io.EOF

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
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly")
	txnID := "pi-sde-" + mustNewUUID()[:8]
	svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	// The fakeTx's first Exec (UPDATE in set disputed) fails. The
	// QueryRowxContext returns a row carrying the pre-seeded
	// payment id so the function reaches the set-disputed step.
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &fakeTx{
			rowID:  txnID,
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
