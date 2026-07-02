package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
)

// ============================================================================
// orderRepo
// ============================================================================

func TestOrderRepo_CreateFindCancelSweep(t *testing.T) {
	db := setupDB(t)
	u := NewUserRepo(db)
	alice := &model.User{ID: newUUID(), Status: "active"}
	_ = u.Create(context.Background(), alice)
	r := NewOrderRepo(db)

	order := &model.Order{
		ID: newUUID(), UserID: alice.ID, PlanID: "monthly",
		Amount: 29.9, Currency: "CNY", Status: "pending",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if err := r.Create(context.Background(), order); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := r.FindByID(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Amount != 29.9 {
		t.Errorf("Amount = %v", got.Amount)
	}

	t.Run("FindByID missing", func(t *testing.T) {
		_, err := r.FindByID(context.Background(), newUUID())
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v, want ErrNoRows", err)
		}
	})

	t.Run("CancelPending transitions pending → cancelled", func(t *testing.T) {
		ok, err := r.CancelPending(context.Background(), order.ID, alice.ID)
		if err != nil || !ok {
			t.Fatalf("CancelPending: ok=%v err=%v", ok, err)
		}
		got, _ := r.FindByID(context.Background(), order.ID)
		if got.Status != "cancelled" {
			t.Errorf("Status = %q, want cancelled", got.Status)
		}
	})

	t.Run("CancelPending refuses non-pending", func(t *testing.T) {
		ok, err := r.CancelPending(context.Background(), order.ID, alice.ID)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Errorf("expected ok=false on already-cancelled")
		}
	})

	t.Run("CancelPending refuses non-owner", func(t *testing.T) {
		order2 := &model.Order{
			ID: newUUID(), UserID: alice.ID, PlanID: "monthly",
			Amount: 29.9, Currency: "CNY", Status: "pending",
			ExpiresAt: time.Now().Add(30 * time.Minute),
		}
		_ = r.Create(context.Background(), order2)
		ok, err := r.CancelPending(context.Background(), order2.ID, newUUID())
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Errorf("expected ok=false for non-owner")
		}
	})

	t.Run("ListByUserID", func(t *testing.T) {
		list, err := r.ListByUserID(context.Background(), alice.ID)
		if err != nil {
			t.Fatalf("ListByUserID: %v", err)
		}
		if len(list) == 0 {
			t.Errorf("expected at least one order")
		}
	})
}

func TestOrderRepo_SweepExpired(t *testing.T) {
	db := setupDB(t)
	u := NewUserRepo(db)
	alice := &model.User{ID: newUUID(), Status: "active"}
	_ = u.Create(context.Background(), alice)
	r := NewOrderRepo(db)

	// One already expired, one fresh.
	old := &model.Order{
		ID: newUUID(), UserID: alice.ID, PlanID: "monthly",
		Amount: 29.9, Currency: "CNY", Status: "pending",
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}
	_ = r.Create(context.Background(), old)
	fresh := &model.Order{
		ID: newUUID(), UserID: alice.ID, PlanID: "monthly",
		Amount: 29.9, Currency: "CNY", Status: "pending",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	_ = r.Create(context.Background(), fresh)

	n, err := r.SweepExpired(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n < 1 {
		t.Errorf("swept %d, want >= 1", n)
	}

	got, _ := r.FindByID(context.Background(), old.ID)
	if got.Status != "expired" {
		t.Errorf("old.Status = %q, want expired", got.Status)
	}
	got, _ = r.FindByID(context.Background(), fresh.ID)
	if got.Status != "pending" {
		t.Errorf("fresh.Status = %q, want pending (untouched)", got.Status)
	}
}

// ============================================================================
// paymentRepo
// ============================================================================

func seedOrderAndUser(t *testing.T, db *sqlx.DB) (string, string) {
	t.Helper()
	u := NewUserRepo(db)
	alice := &model.User{ID: newUUID(), Status: "active"}
	if err := u.Create(context.Background(), alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	or := NewOrderRepo(db)
	order := &model.Order{
		ID: newUUID(), UserID: alice.ID, PlanID: "monthly",
		Amount: 29.9, Currency: "CNY", Status: "pending",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if err := or.Create(context.Background(), order); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return alice.ID, order.ID
}

func TestPaymentRepo_InsertPaidOnConflictDoNothing(t *testing.T) {
	db := setupDB(t)
	_, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)

	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi_1",
		Amount: 29.9, Currency: "CNY", Status: "paid",
		RawPayload: json.RawMessage(`{"src":"test"}`),
	}
	id, inserted, err := r.InsertPaidOnConflictDoNothing(context.Background(), p)
	if err != nil {
		t.Fatalf("InsertPaidOnConflictDoNothing: %v", err)
	}
	if !inserted || id == "" {
		t.Errorf("expected inserted=true and id, got id=%q inserted=%v", id, inserted)
	}

	// Duplicate insert should NOT insert.
	_, inserted2, err := r.InsertPaidOnConflictDoNothing(context.Background(), p)
	if err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	if inserted2 {
		t.Errorf("expected inserted=false on duplicate (channel, external_txn_id)")
	}
}

func TestPaymentRepo_FindByID_FindByChannelTxnID(t *testing.T) {
	db := setupDB(t)
	_, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)

	p := &model.Payment{
		OrderID: orderID, Channel: "wechat_pay", ExternalTxnID: "txn-1",
		Amount: 10.0, Currency: "CNY", Status: "paid",
	}
	id, _, err := r.InsertPaidOnConflictDoNothing(context.Background(), p)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Run("FindByID", func(t *testing.T) {
		got, err := r.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.ExternalTxnID != "txn-1" {
			t.Errorf("ExternalTxnID = %q", got.ExternalTxnID)
		}
	})
	t.Run("FindByID missing", func(t *testing.T) {
		_, err := r.FindByID(context.Background(), newUUID())
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("FindByChannelTxnID", func(t *testing.T) {
		got, err := r.FindByChannelTxnID(context.Background(), "wechat_pay", "txn-1")
		if err != nil {
			t.Fatalf("FindByChannelTxnID: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %q", got.ID)
		}
	})
	t.Run("FindByChannelTxnID missing", func(t *testing.T) {
		_, err := r.FindByChannelTxnID(context.Background(), "wechat_pay", "missing")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPaymentRepo_FindPaidByOrderID(t *testing.T) {
	db := setupDB(t)
	_, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)

	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-1",
		Amount: 29.9, Currency: "CNY", Status: "paid",
	}
	_, _, _ = r.InsertPaidOnConflictDoNothing(context.Background(), p)

	got, err := r.FindPaidByOrderID(context.Background(), orderID)
	if err != nil {
		t.Fatalf("FindPaidByOrderID: %v", err)
	}
	if got.ExternalTxnID != "pi-1" {
		t.Errorf("ExternalTxnID = %q", got.ExternalTxnID)
	}
}

func TestPaymentRepo_FindPaidByOrderID_NoRow(t *testing.T) {
	db := setupDB(t)
	r := NewPaymentRepo(db)
	_, err := r.FindPaidByOrderID(context.Background(), newUUID())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentRepo_ListByOrderID_ListByUserID(t *testing.T) {
	db := setupDB(t)
	userID, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)

	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-list",
		Amount: 29.9, Currency: "CNY", Status: "paid",
	}
	_, _, _ = r.InsertPaidOnConflictDoNothing(context.Background(), p)

	t.Run("ListByOrderID", func(t *testing.T) {
		list, err := r.ListByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("ListByOrderID: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("got %d, want 1", len(list))
		}
	})
	t.Run("ListByUserID", func(t *testing.T) {
		list, err := r.ListByUserID(context.Background(), userID)
		if err != nil {
			t.Fatalf("ListByUserID: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("got %d, want 1", len(list))
		}
	})
}

func TestPaymentRepo_MarkPaid_MarkFailed_MarkRefunded(t *testing.T) {
	db := setupDB(t)
	_, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)

	// Insert as pending first via InsertPaidOnConflictDoNothing with status=paid
	// is the only path; for MarkPaid we need a pending row. Use direct INSERT.
	pID := newUUID()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', 'pi-mark', 10.0, 'CNY', 'pending', '{}')
	`, pID, orderID)
	if err != nil {
		t.Fatalf("seed pending payment: %v", err)
	}

	now := time.Now()
	if err := r.MarkPaid(context.Background(), pID, now); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	got, _ := r.FindByID(context.Background(), pID)
	if got.Status != "paid" {
		t.Errorf("after MarkPaid: Status = %q", got.Status)
	}
	if got.PaidAt == nil {
		t.Errorf("after MarkPaid: PaidAt is nil")
	}

	if err := r.MarkRefunded(context.Background(), pID); err != nil {
		t.Fatalf("MarkRefunded: %v", err)
	}
	got, _ = r.FindByID(context.Background(), pID)
	if got.Status != "refunded" {
		t.Errorf("after MarkRefunded: Status = %q", got.Status)
	}

	// MarkFailed from refunded should no-op (status not in pending/paid).
	if err := r.MarkFailed(context.Background(), pID, "too late"); err != nil {
		t.Fatalf("MarkFailed (no-op): %v", err)
	}
	got, _ = r.FindByID(context.Background(), pID)
	if got.Status != "refunded" {
		t.Errorf("after MarkFailed on refunded: Status = %q", got.Status)
	}

	// MarkFailed from pending works.
	pID2 := newUUID()
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', 'pi-fail', 5.0, 'CNY', 'pending', '{}')
	`, pID2, orderID)
	if err := r.MarkFailed(context.Background(), pID2, "test reason"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _ = r.FindByID(context.Background(), pID2)
	if got.Status != "failed" {
		t.Errorf("after MarkFailed: Status = %q", got.Status)
	}
	if got.FailedReason == nil || *got.FailedReason != "test reason" {
		t.Errorf("FailedReason = %v", got.FailedReason)
	}
}

func TestPaymentRepo_Disputed(t *testing.T) {
	db := setupDB(t)
	_, orderID := seedOrderAndUser(t, db)
	r := NewPaymentRepo(db)
	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-dis",
		Amount: 10.0, Currency: "CNY", Status: "paid",
	}
	id, _, _ := r.InsertPaidOnConflictDoNothing(context.Background(), p)

	now := time.Now()
	if err := r.SetDisputed(context.Background(), id, now); err != nil {
		t.Fatalf("SetDisputed: %v", err)
	}
	got, _ := r.FindByID(context.Background(), id)
	if !got.Disputed {
		t.Errorf("Disputed = false")
	}
	if got.DisputedAt == nil {
		t.Errorf("DisputedAt is nil")
	}

	if err := r.ClearDisputed(context.Background(), id); err != nil {
		t.Fatalf("ClearDisputed: %v", err)
	}
	got, _ = r.FindByID(context.Background(), id)
	if got.Disputed {
		t.Errorf("after ClearDisputed: Disputed = true")
	}
	if got.DisputedAt != nil {
		t.Errorf("after ClearDisputed: DisputedAt = %v", got.DisputedAt)
	}
}

// ============================================================================
// refundRepo
// ============================================================================

func TestRefundRepo_FindByIdempotencyKey_InsertPending_FindByID(t *testing.T) {
	db := setupDB(t)
	userID, orderID := seedOrderAndUser(t, db)
	pr := NewPaymentRepo(db)
	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-r",
		Amount: 100.0, Currency: "CNY", Status: "paid",
	}
	pid, _, _ := pr.InsertPaidOnConflictDoNothing(context.Background(), p)

	r := NewRefundRepo(db)
	ref := &model.Refund{
		ID: newUUID(), PaymentID: pid, Channel: "stripe", UserID: userID,
		Amount: 10.0, IdempotencyKey: "user-req-001",
		Status: "pending",
	}
	if err := r.InsertPending(context.Background(), ref); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	t.Run("FindByIdempotencyKey", func(t *testing.T) {
		got, err := r.FindByIdempotencyKey(context.Background(), userID, "user-req-001")
		if err != nil {
			t.Fatalf("FindByIdempotencyKey: %v", err)
		}
		if got.Amount != 10.0 {
			t.Errorf("Amount = %v", got.Amount)
		}
	})
	t.Run("FindByIdempotencyKey scoped to user", func(t *testing.T) {
		_, err := r.FindByIdempotencyKey(context.Background(), newUUID(), "user-req-001")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v, want ErrNoRows (scoped to user)", err)
		}
	})
	t.Run("FindByID", func(t *testing.T) {
		got, err := r.FindByID(context.Background(), ref.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.ID != ref.ID {
			t.Errorf("got %q", got.ID)
		}
	})
	t.Run("FindByID missing", func(t *testing.T) {
		_, err := r.FindByID(context.Background(), newUUID())
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("FindByIdempotencyKey missing", func(t *testing.T) {
		_, err := r.FindByIdempotencyKey(context.Background(), userID, "nope")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRefundRepo_FindByChannelRefundID(t *testing.T) {
	db := setupDB(t)
	userID, orderID := seedOrderAndUser(t, db)
	pr := NewPaymentRepo(db)
	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-cr",
		Amount: 50.0, Currency: "CNY", Status: "paid",
	}
	pid, _, _ := pr.InsertPaidOnConflictDoNothing(context.Background(), p)

	r := NewRefundRepo(db)
	extID := "re_x"
	ref := &model.Refund{
		ID: newUUID(), PaymentID: pid, Channel: "stripe", UserID: userID,
		Amount: 5.0, IdempotencyKey: "k-cr", ExternalRefundID: &extID,
		Status: "pending",
	}
	if err := r.InsertPending(context.Background(), ref); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	got, err := r.FindByChannelRefundID(context.Background(), "stripe", "re_x")
	if err != nil {
		t.Fatalf("FindByChannelRefundID: %v", err)
	}
	if got.ID != ref.ID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestRefundRepo_ListByPaymentID_Sum_MarkPaid(t *testing.T) {
	db := setupDB(t)
	userID, orderID := seedOrderAndUser(t, db)
	pr := NewPaymentRepo(db)
	p := &model.Payment{
		OrderID: orderID, Channel: "stripe", ExternalTxnID: "pi-sum",
		Amount: 100.0, Currency: "CNY", Status: "paid",
	}
	pid, _, _ := pr.InsertPaidOnConflictDoNothing(context.Background(), p)

	r := NewRefundRepo(db)
	ref := &model.Refund{
		ID: newUUID(), PaymentID: pid, Channel: "stripe", UserID: userID,
		Amount: 30.0, IdempotencyKey: "k-sum", Status: "pending",
	}
	_ = r.InsertPending(context.Background(), ref)

	t.Run("ListByPaymentID", func(t *testing.T) {
		list, err := r.ListByPaymentID(context.Background(), pid)
		if err != nil {
			t.Fatalf("ListByPaymentID: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("got %d, want 1", len(list))
		}
	})
	t.Run("SumByPaymentID — pending not yet counted", func(t *testing.T) {
		sum, err := r.SumByPaymentID(context.Background(), pid)
		if err != nil {
			t.Fatalf("SumByPaymentID: %v", err)
		}
		if sum != 0 {
			t.Errorf("pending refunds should not be summed, got %v", sum)
		}
	})
	t.Run("MarkPaid and re-sum", func(t *testing.T) {
		if err := r.MarkPaid(context.Background(), ref.ID); err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		sum, _ := r.SumByPaymentID(context.Background(), pid)
		if sum != 30.0 {
			t.Errorf("after MarkPaid: sum = %v, want 30", sum)
		}
	})
	t.Run("MarkPaid idempotent", func(t *testing.T) {
		if err := r.MarkPaid(context.Background(), ref.ID); err != nil {
			t.Fatalf("MarkPaid (idempotent): %v", err)
		}
	})
}

// ============================================================================
// webhookEventRepo
// ============================================================================

func TestWebhookEventRepo_Insert_Find_MarkProcessed(t *testing.T) {
	db := setupDB(t)
	r := NewWebhookEventRepo(db)
	ev := &model.WebhookEvent{
		Channel: "stripe", EventID: "evt-1", EventType: "payment_intent.succeeded",
		RawPayload: json.RawMessage(`{"id":"evt-1"}`),
	}
	id, inserted, err := r.InsertOnConflictDoNothing(context.Background(), ev)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted || id == "" {
		t.Errorf("expected inserted, got id=%q inserted=%v", id, inserted)
	}

	t.Run("dedupe — second insert", func(t *testing.T) {
		_, inserted2, err := r.InsertOnConflictDoNothing(context.Background(), ev)
		if err != nil {
			t.Fatalf("dup insert: %v", err)
		}
		if inserted2 {
			t.Errorf("expected inserted=false on duplicate (channel, event_id)")
		}
	})

	t.Run("FindByChannelEventID", func(t *testing.T) {
		got, err := r.FindByChannelEventID(context.Background(), "stripe", "evt-1")
		if err != nil {
			t.Fatalf("FindByChannelEventID: %v", err)
		}
		if got.ID != id {
			t.Errorf("got %v, want %v", got.ID, id)
		}
		if got.ProcessedAt != nil {
			t.Errorf("ProcessedAt = %v, want nil before MarkProcessed", got.ProcessedAt)
		}
	})
	t.Run("FindByChannelEventID missing", func(t *testing.T) {
		_, err := r.FindByChannelEventID(context.Background(), "stripe", "nope")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("MarkProcessed sets processed_at", func(t *testing.T) {
		if err := r.MarkProcessed(context.Background(), id); err != nil {
			t.Fatalf("MarkProcessed: %v", err)
		}
		got, _ := r.FindByChannelEventID(context.Background(), "stripe", "evt-1")
		if got.ProcessedAt == nil {
			t.Errorf("after MarkProcessed: ProcessedAt is nil")
		}
	})

	t.Run("MarkProcessedOnTx works inside tx", func(t *testing.T) {
		ev2 := &model.WebhookEvent{
			Channel: "wechat_pay", EventID: "evt-2", EventType: "TRANSACTION.SUCCESS",
			RawPayload: json.RawMessage(`{}`),
		}
		id2, _, _ := r.InsertOnConflictDoNothing(context.Background(), ev2)
		tx, err := db.BeginTxx(context.Background(), nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if err := r.MarkProcessedOnTx(context.Background(), tx, id2); err != nil {
			t.Fatalf("MarkProcessedOnTx: %v", err)
		}
		_ = tx.Commit()
		got, _ := r.FindByChannelEventID(context.Background(), "wechat_pay", "evt-2")
		if got.ProcessedAt == nil {
			t.Errorf("after MarkProcessedOnTx: ProcessedAt is nil")
		}
	})

	t.Run("InsertOnConflictDoNothing with nil RawPayload", func(t *testing.T) {
		ev3 := &model.WebhookEvent{
			Channel: "alipay", EventID: "evt-3", EventType: "TRADE_SUCCESS",
			RawPayload: nil,
		}
		_, inserted3, err := r.InsertOnConflictDoNothing(context.Background(), ev3)
		if err != nil {
			t.Fatalf("insert nil payload: %v", err)
		}
		if !inserted3 {
			t.Errorf("expected inserted=true")
		}
	})
}

// ============================================================================
// auditLogRepo
// ============================================================================

func TestAuditLogRepo_Insert(t *testing.T) {
	db := setupDB(t)
	r := NewAuditLogRepo(db)

	target := "order:abc-123"
	tags := []string{"payment", "expiry"}
	log := &model.AuditLog{
		Actor: "service", Action: "late_payment_post_expiry",
		Target: &target, Tags: pq.StringArray(tags),
		Context: json.RawMessage(`{"order_id":"abc-123"}`),
	}
	if err := r.Insert(context.Background(), log); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var got model.AuditLog
	err := db.GetContext(context.Background(), &got, `SELECT * FROM audit_log WHERE actor = 'service' AND action = 'late_payment_post_expiry'`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Actor != "service" {
		t.Errorf("Actor = %q", got.Actor)
	}
	if got.Target == nil || *got.Target != target {
		t.Errorf("Target = %v", got.Target)
	}
}