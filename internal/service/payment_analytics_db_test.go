package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/analytics"
)

// ============================================================================
// M3 analytics: purchase_completed / refund_completed (paddle only)
// ============================================================================

func paddlePurchaseEvent(eventID, txnID, orderID string, amount float64) WebhookEvent {
	return WebhookEvent{
		Channel: "paddle", EventID: eventID, EventType: "transaction.completed",
		TransactionID: txnID, OrderID: orderID, Origin: "web",
		Amount: amount, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}
}

func paddleRefundEvent(eventID, txnID, adjID string, amount float64) WebhookEvent {
	return WebhookEvent{
		Channel: "paddle", EventID: eventID, EventType: "adjustment.updated",
		TransactionID: txnID, ExternalRefundID: adjID,
		RefundAmount: amount, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}
}

// seedPaidPaddlePayment seeds a paid order + paid payment row pair on the
// paddle channel (the state onPaymentSucceeded leaves behind).
func seedPaidPaddlePayment(t *testing.T, db *sqlx.DB, userID, planID, txnID string, amount float64) (orderID, paymentID string) {
	t.Helper()
	orderID = mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, $3, $4, 'CNY', 'paid', now() + INTERVAL '30 minutes', NULL)
	`, orderID, userID, planID, amount); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	paymentID = mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, 'paddle', $3, $4, 'CNY', 'paid', now(), '{}')
	`, paymentID, orderID, txnID, amount); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return orderID, paymentID
}

func eventsNamed(rec *recordingAnalytics, name string) []analytics.Event {
	var out []analytics.Event
	for _, e := range rec.events() {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func TestOnWebhook_PaddlePurchase_EmitsPurchaseCompleted(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)

	t.Run("first purchase with attribution snapshot", func(t *testing.T) {
		orderID := seedPaidOrder(t, db, uid, "monthly", 19.9)
		// M1 snapshot on the order + the 029 interval descriptor so
		// billing_cycle is derivable.
		if _, err := db.ExecContext(context.Background(), `
			UPDATE orders SET attribution = $1, plan_interval_days = 30 WHERE id = $2
		`, `{"first_touch":{"utm_source":"google"},"last_touch":{"utm_medium":"cpc"}}`, orderID); err != nil {
			t.Fatalf("set attribution: %v", err)
		}

		txnID := "txn_pd_" + mustNewUUID()[:8]
		_, err := svc.OnWebhook(context.Background(),
			paddlePurchaseEvent("evt-pd-buy-"+mustNewUUID()[:8], txnID, orderID, 19.9))
		if err != nil {
			t.Fatalf("OnWebhook: %v", err)
		}

		evts := eventsNamed(rec, "purchase_completed")
		if len(evts) != 1 {
			t.Fatalf("purchase_completed count = %d, want 1 (all: %+v)", len(evts), rec.events())
		}
		evt := evts[0]
		if evt.DistinctID != uid {
			t.Errorf("distinct_id = %q, want %q", evt.DistinctID, uid)
		}
		if evt.UUID != analytics.EventUUID("purchase:"+txnID) {
			t.Errorf("uuid = %q, want EventUUID(purchase:%s)", evt.UUID, txnID)
		}
		for k, want := range map[string]any{
			"order_id":       orderID,
			"transaction_id": txnID,
			"plan_id":        "monthly",
			"currency":       "CNY",
			"amount":         19.9,
			"purchase_type":  "first_purchase",
			"channel":        "paddle",
			"region":         "intl",
			"environment":    "production",
			"billing_cycle":  "monthly",
		} {
			if evt.Properties[k] != want {
				t.Errorf("properties[%q] = %v, want %v", k, evt.Properties[k], want)
			}
		}
		attr, ok := evt.Properties["attribution"].(map[string]any)
		if !ok {
			t.Fatalf("attribution property missing or wrong type: %+v", evt.Properties)
		}
		ft, ok := attr["first_touch"].(map[string]any)
		if !ok || ft["utm_source"] != "google" {
			t.Errorf("attribution snapshot not verbatim: %v", attr)
		}
	})

	t.Run("order without attribution omits the key", func(t *testing.T) {
		orderID := seedPaidOrder(t, db, uid, "monthly", 19.9)
		txnID := "txn_pd_" + mustNewUUID()[:8]
		_, err := svc.OnWebhook(context.Background(),
			paddlePurchaseEvent("evt-pd-buy-"+mustNewUUID()[:8], txnID, orderID, 19.9))
		if err != nil {
			t.Fatalf("OnWebhook: %v", err)
		}
		var found *analytics.Event
		for _, e := range eventsNamed(rec, "purchase_completed") {
			if e.Properties["transaction_id"] == txnID {
				ev := e
				found = &ev
			}
		}
		if found == nil {
			t.Fatal("no purchase_completed for the second order")
		}
		if _, has := found.Properties["attribution"]; has {
			t.Errorf("attribution key must be omitted when the order has none: %+v", found.Properties)
		}
		if _, has := found.Properties["billing_cycle"]; has {
			t.Errorf("billing_cycle must be omitted without a snapshot interval: %+v", found.Properties)
		}
	})
}

func TestOnWebhook_PaddleRenewal_EmitsPurchaseCompletedRenewal(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "staging"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)
	extSubID := "sub_paddle_" + mustNewUUID()[:8]
	seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))
	next := time.Now().Add(45 * 24 * time.Hour).UTC().Truncate(time.Second)
	svc.SetPaddleClient(&stubPaddle{next: &next})

	txnID := "txn_pd_renew_" + mustNewUUID()[:8]
	_, err := svc.OnWebhook(context.Background(),
		paddleRenewalEvent("evt-pd-renew-"+mustNewUUID()[:8], txnID, extSubID))
	if err != nil {
		t.Fatalf("OnWebhook paddle renewal: %v", err)
	}

	evts := eventsNamed(rec, "purchase_completed")
	if len(evts) != 1 {
		t.Fatalf("purchase_completed count = %d, want 1 (all: %+v)", len(evts), rec.events())
	}
	evt := evts[0]
	if evt.DistinctID != uid {
		t.Errorf("distinct_id = %q, want %q", evt.DistinctID, uid)
	}
	if evt.UUID != analytics.EventUUID("purchase:"+txnID) {
		t.Errorf("uuid = %q, want EventUUID(purchase:%s)", evt.UUID, txnID)
	}
	if evt.Properties["purchase_type"] != "renewal" {
		t.Errorf("purchase_type = %v, want renewal", evt.Properties["purchase_type"])
	}
	if evt.Properties["plan_id"] != "monthly" || evt.Properties["billing_cycle"] != "monthly" {
		t.Errorf("plan/billing_cycle = %v/%v, want monthly/monthly",
			evt.Properties["plan_id"], evt.Properties["billing_cycle"])
	}
	if _, has := evt.Properties["attribution"]; has {
		t.Error("synthetic renewal orders carry no attribution — key must be omitted")
	}
}

func TestOnWebhook_PaddlePurchase_ZeroAmount_NoEvent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)

	// Zero-amount paddle settlement (e.g. a trial transaction): the order
	// still flips paid, but no purchase_completed fires.
	orderID := seedPaidOrder(t, db, uid, "free", 0)
	txnID := "txn_pd_zero_" + mustNewUUID()[:8]
	_, err := svc.OnWebhook(context.Background(),
		paddlePurchaseEvent("evt-pd-zero-"+mustNewUUID()[:8], txnID, orderID, 0))
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "paid" {
		t.Errorf("order status = %q, want paid (zero-amount settlement still counts)", status)
	}
	if n := len(eventsNamed(rec, "purchase_completed")); n != 0 {
		t.Errorf("zero-amount purchase emitted %d events, want 0", n)
	}
}

func TestOnWebhook_OtherChannels_NoPurchaseEvent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)

	for _, channel := range []string{"stripe", "wechat_pay", "paypal"} {
		orderID := seedPaidOrder(t, db, uid, "monthly", 19.9)
		txnID := "txn_" + channel + "_" + mustNewUUID()[:8]
		eventType := map[string]string{
			"stripe":     "payment_intent.succeeded",
			"wechat_pay": "TRANSACTION.SUCCESS",
			"paypal":     "PAYMENT.CAPTURE.COMPLETED",
		}[channel]
		_, err := svc.OnWebhook(context.Background(), WebhookEvent{
			Channel: channel, EventID: "evt-" + channel + "-" + mustNewUUID()[:8],
			EventType: eventType, TransactionID: txnID, OrderID: orderID,
			Amount: 19.9, Currency: "CNY",
			RawPayload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("OnWebhook %s: %v", channel, err)
		}
	}
	if n := len(rec.events()); n != 0 {
		t.Errorf("non-paddle payments emitted %d analytics events, want 0: %+v", n, rec.events())
	}
}

func TestOnWebhook_PaddleAdjustmentRefund_Full(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)
	txnID := "txn_pd_" + mustNewUUID()[:8]
	orderID, _ := seedPaidPaddlePayment(t, db, uid, "monthly", txnID, 19.9)
	// Active sub to be cancelled by the full-refund cascade.
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '15 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	adjID := "adj_pd_" + mustNewUUID()[:8]
	_, err := svc.OnWebhook(context.Background(),
		paddleRefundEvent("evt-pd-adj-"+mustNewUUID()[:8], txnID, adjID, 19.9))
	if err != nil {
		t.Fatalf("OnWebhook adjustment: %v", err)
	}

	// Refund row persisted, keyed (paddle, adjustment id), already paid.
	var refundID, refundStatus string
	var refundAmount float64
	if err := db.QueryRowContext(context.Background(), `
		SELECT id, status, amount FROM refunds WHERE channel = 'paddle' AND external_refund_id = $1
	`, adjID).Scan(&refundID, &refundStatus, &refundAmount); err != nil {
		t.Fatalf("refund row missing: %v", err)
	}
	if refundStatus != "paid" || refundAmount != 19.9 {
		t.Errorf("refund row = (%s, %v), want (paid, 19.9)", refundStatus, refundAmount)
	}

	// Full-refund cascade: payment + order refunded, subscription cancelled.
	for table, want := range map[string]string{"payments": "refunded", "orders": "refunded"} {
		var st string
		q := `SELECT status FROM ` + table + ` WHERE id = $1`
		id := orderID
		if table == "payments" {
			var pid string
			_ = db.GetContext(context.Background(), &pid,
				`SELECT id FROM payments WHERE external_txn_id = $1`, txnID)
			id = pid
		}
		if err := db.GetContext(context.Background(), &st, q, id); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if st != want {
			t.Errorf("%s status = %q, want %q", table, st, want)
		}
	}
	var subStatus string
	if err := db.GetContext(context.Background(), &subStatus,
		`SELECT status FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if subStatus != "cancelled" {
		t.Errorf("subscription status = %q, want cancelled on full refund", subStatus)
	}

	evts := eventsNamed(rec, "refund_completed")
	if len(evts) != 1 {
		t.Fatalf("refund_completed count = %d, want 1 (all: %+v)", len(evts), rec.events())
	}
	evt := evts[0]
	if evt.DistinctID != uid {
		t.Errorf("distinct_id = %q, want %q", evt.DistinctID, uid)
	}
	if evt.UUID != analytics.EventUUID("refund:"+refundID) {
		t.Errorf("uuid = %q, want EventUUID(refund:%s)", evt.UUID, refundID)
	}
	for k, want := range map[string]any{
		"refund_id":       refundID,
		"transaction_id":  txnID,
		"order_id":        orderID,
		"amount_refunded": 19.9,
		"currency":        "CNY",
		"channel":         "paddle",
		"region":          "intl",
		"environment":     "production",
	} {
		if evt.Properties[k] != want {
			t.Errorf("properties[%q] = %v, want %v", k, evt.Properties[k], want)
		}
	}
}

func TestOnWebhook_PaddleAdjustmentRefund_Partial(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)
	txnID := "txn_pd_" + mustNewUUID()[:8]
	_, paymentID := seedPaidPaddlePayment(t, db, uid, "monthly", txnID, 19.9)
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '15 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	adjID := "adj_pd_" + mustNewUUID()[:8]
	_, err := svc.OnWebhook(context.Background(),
		paddleRefundEvent("evt-pd-adj-"+mustNewUUID()[:8], txnID, adjID, 5.0))
	if err != nil {
		t.Fatalf("OnWebhook partial adjustment: %v", err)
	}

	var refundID string
	if err := db.GetContext(context.Background(), &refundID,
		`SELECT id FROM refunds WHERE channel = 'paddle' AND external_refund_id = $1`, adjID); err != nil {
		t.Fatalf("partial refund row missing: %v", err)
	}
	// Partial: payment stays paid, subscription untouched.
	var payStatus, subStatus string
	if err := db.GetContext(context.Background(), &payStatus,
		`SELECT status FROM payments WHERE id = $1`, paymentID); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if err := db.GetContext(context.Background(), &subStatus,
		`SELECT status FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if payStatus != "paid" {
		t.Errorf("payment status = %q, want paid (partial refund)", payStatus)
	}
	if subStatus != "active" {
		t.Errorf("subscription status = %q, want active (partial refund)", subStatus)
	}

	evts := eventsNamed(rec, "refund_completed")
	if len(evts) != 1 {
		t.Fatalf("refund_completed count = %d, want 1", len(evts))
	}
	if evts[0].UUID != analytics.EventUUID("refund:"+refundID) {
		t.Errorf("uuid = %q, want EventUUID(refund:%s)", evts[0].UUID, refundID)
	}
	if evts[0].Properties["amount_refunded"] != 5.0 {
		t.Errorf("amount_refunded = %v, want 5.0", evts[0].Properties["amount_refunded"])
	}
}

func TestOnWebhook_PaddleAdjustment_NonRefund_AuditOnly(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)
	txnID := "txn_pd_" + mustNewUUID()[:8]
	seedPaidPaddlePayment(t, db, uid, "monthly", txnID, 19.9)

	// adjustment.updated that is NOT a completed refund (no
	// ExternalRefundID — the parser leaves it bare) stays audit-only.
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paddle", EventID: "evt-pd-adj-credit-" + mustNewUUID()[:8],
		EventType: "adjustment.updated", TransactionID: txnID,
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	if res.DomainAction != "" && res.DomainAction != "none" {
		t.Errorf("DomainAction = %q, want none/empty", res.DomainAction)
	}
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM refunds WHERE channel = 'paddle'`); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if n != 0 {
		t.Errorf("refund rows = %d, want 0 for a non-refund adjustment", n)
	}
	if len(rec.events()) != 0 {
		t.Errorf("emitted %d events, want 0", len(rec.events()))
	}
}

func TestOnWebhook_PaddleAdjustmentRefund_ReplayDedup(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	rec := &recordingAnalytics{env: "production"}
	svc.SetAnalytics(rec)
	uid := seedUser(t, db)
	txnID := "txn_pd_" + mustNewUUID()[:8]
	seedPaidPaddlePayment(t, db, uid, "monthly", txnID, 19.9)

	adjID := "adj_pd_" + mustNewUUID()[:8]
	// Same adjustment redelivered under a DIFFERENT event id (the
	// event-level webhook_events dedupe only catches identical event_ids).
	for _, suffix := range []string{"a", "b"} {
		_, err := svc.OnWebhook(context.Background(),
			paddleRefundEvent("evt-pd-adj-"+suffix+"-"+mustNewUUID()[:6], txnID, adjID, 19.9))
		if err != nil {
			t.Fatalf("OnWebhook replay %s: %v", suffix, err)
		}
	}

	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM refunds WHERE channel = 'paddle' AND external_refund_id = $1`, adjID); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if n != 1 {
		t.Fatalf("refund rows = %d, want 1 (find-or-insert dedupe)", n)
	}
	// Both passes may emit, but the uuid derives from the stable internal
	// refund row id — PostHog dedupes on it.
	evts := eventsNamed(rec, "refund_completed")
	if len(evts) == 0 {
		t.Fatal("no refund_completed emitted")
	}
	wantUUID := evts[0].UUID
	for _, e := range evts[1:] {
		if e.UUID != wantUUID {
			t.Errorf("replay uuid = %q, want stable %q", e.UUID, wantUUID)
		}
	}
	var refundID string
	_ = db.GetContext(context.Background(), &refundID,
		`SELECT id FROM refunds WHERE channel = 'paddle' AND external_refund_id = $1`, adjID)
	if wantUUID != analytics.EventUUID("refund:"+refundID) {
		t.Errorf("uuid = %q, want EventUUID(refund:%s)", wantUUID, refundID)
	}
}
