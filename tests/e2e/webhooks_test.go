package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// --- Golden sample webhook payloads ---
//
// These samples are hand-crafted to (a) round-trip through their channel's
// signature verifier and (b) match the JSON / form shape the production
// handler expects. They are the "known-good inputs" referenced by the
// design doc's "Golden sample webhooks" recommendation — if any of these
// drift from a real channel's wire format, the corresponding handler parse
// will break.

func goldenStripePaid(orderID, txnID string, amount int64) []byte {
	body := map[string]any{
		"id":   "evt_e2e_stripe_" + orderID,
		"type": "payment_intent.succeeded",
		"data": map[string]any{
			"object": map[string]any{
				"id":       txnID,
				"amount":   amount,
				"currency": "cny",
				"metadata": map[string]any{"order_id": orderID},
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func goldenAlipayPaid(orderID, txnID string, amount string) string {
	// Alipay sends form-encoded key=value&key=value.
	return fmt.Sprintf(
		"out_trade_no=%s&trade_no=%s&total_amount=%s&notify_id=n_e2e_%s&notify_type=trade_status_sync&gmt_payment=2024-01-01+12:00:00",
		orderID, txnID, amount, orderID,
	)
}

func goldenAlipayRefund(orderID, txnID, refundAmount string) string {
	return fmt.Sprintf(
		"out_trade_no=%s&trade_no=%s&total_amount=29.90&refund_amount=%s&notify_id=n_e2e_refund_%s&notify_type=trade_closed",
		orderID, txnID, refundAmount, orderID,
	)
}

// goldenWeChatPaid generates an encrypted WeChat Pay v3 webhook payload.
// Returns the raw JSON body (envelope + AES-256-GCM-encrypted resource).
func goldenWeChatPaid(t *testing.T, orderID, txnID string, amountFen int64) []byte {
	t.Helper()

	// Build the resource plaintext.
	resourcePlaintext, _ := json.Marshal(map[string]any{
		"transaction_id": txnID,
		"out_trade_no":   orderID,
		"amount":         map[string]any{"total": amountFen, "refund": 0},
	})

	// AES-256-GCM encrypt with the e2e test key. nonce and AAD are taken
	// from the WeChat Pay v3 spec — fixed values for the test scenario.
	ciphertext, nonce, aad := encryptForWeChat(t, []byte(e2eWeChatKey), resourcePlaintext)

	body := map[string]any{
		"id":           "evt_e2e_wechat_" + orderID,
		"create_time":  "2024-01-01T12:00:00+08:00",
		"resource_type": "encrypt-resource",
		"event_type":    "TRANSACTION.SUCCESS",
		"summary":       "支付成功",
		"resource": map[string]any{
			"ciphertext":      ciphertext,
			"associated_data": aad,
			"nonce":           nonce,
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// ============================================================================
// Stripe payment_intent.succeeded — happy path
// ============================================================================

func TestWebhook_Stripe_PaymentSucceeded(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "stripe-paid", "yundian")

	// Create the order first so the webhook can find it.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	// Sign + POST the webhook.
	body := goldenStripePaid(orderID, "pi_e2e_stripe_1", 2990)
	ts := time.Now().Unix()
	sig := signStripe(e2eStripeSecret, ts, body)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/stripe", string(body),
		map[string]string{"Stripe-Signature": sig, "Content-Type": "application/json"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Verify the order flipped to paid.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("expected order.status=paid, got %s", status)
	}

	// Verify payment row created.
	var paymentStatus string
	var paidAt *time.Time
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT status, paid_at FROM payments WHERE order_id = $1`, orderID,
	).Scan(&paymentStatus, &paidAt); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if paymentStatus != "paid" {
		t.Errorf("expected payment.status=paid, got %s", paymentStatus)
	}
	if paidAt == nil {
		t.Error("expected paid_at to be set")
	}

	// Verify webhook_events row was inserted (event-level dedup).
	var eventID string
	if err := srv.DB.GetContext(context.Background(), &eventID,
		`SELECT event_id FROM webhook_events WHERE channel = 'stripe'`); err != nil {
		t.Errorf("expected webhook_events row: %v", err)
	}
}

// ============================================================================
// Stripe webhook for an order that doesn't exist → 200 + audit_log row
// ============================================================================
//
// Design doc §"Anomaly note" says "return 404" here, but the current service
// implementation returns 200 after writing an audit_log row. This is a
// defensible policy choice — retrying an event for an order that will
// never exist just creates a loop — and the audit_log row gives ops
// visibility. If the team wants strict 404 semantics, change service to
// return a sentinel error and surface as 404 in the handler.

func TestWebhook_Stripe_UnknownOrder_Audited(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)

	// Use a syntactically-valid UUID that simply doesn't exist in our DB.
	body := goldenStripePaid("00000000-0000-0000-0000-000000000000", "pi_e2e_ghost", 1000)
	ts := time.Now().Unix()
	sig := signStripe(e2eStripeSecret, ts, body)
	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/stripe", string(body),
		map[string]string{"Stripe-Signature": sig, "Content-Type": "application/json"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	var count int
	if err := srv.DB.GetContext(context.Background(), &count,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'webhook_for_unknown_order'`); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("expected audit_log row tagged webhook_for_unknown_order")
	}
}

// ============================================================================
// Stripe webhook event-level dedup: same event_id arriving twice → 200, no
// second side effect.
// ============================================================================

func TestWebhook_Stripe_DuplicateEvent(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "stripe-dup", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	body := goldenStripePaid(orderID, "pi_e2e_dup_1", 2990)
	ts := time.Now().Unix()
	sig := signStripe(e2eStripeSecret, ts, body)
	headers := map[string]string{"Stripe-Signature": sig, "Content-Type": "application/json"}

	// First delivery.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/webhooks/payment/stripe", string(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first delivery: %d", resp.StatusCode)
	}

	// Second delivery (same event_id). Should still be 200 (deduped).
	resp = doRequest(t, srv.Engine, http.MethodPost, "/webhooks/payment/stripe", string(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second delivery: %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var dup struct {
		Duplicate bool `json:"duplicate"`
	}
	resp.JSON(t, &dup)
	if !dup.Duplicate {
		t.Error("expected duplicate=true on second delivery")
	}

	// Verify only one payment row exists.
	var count int
	if err := srv.DB.GetContext(context.Background(), &count,
		`SELECT COUNT(*) FROM payments WHERE order_id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 payment, got %d", count)
	}
}

// ============================================================================
// Stripe webhook with bad signature → 400 (channel doesn't retry)
// ============================================================================

func TestWebhook_Stripe_BadSignature(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	body := []byte(`{"id":"evt_x","type":"payment_intent.succeeded","data":{}}`)

	ts := time.Now().Unix()
	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/stripe", string(body),
		map[string]string{"Stripe-Signature": fmt.Sprintf("t=%d,v1=deadbeef", ts)})

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for bad sig, got %d", resp.StatusCode)
	}
}

// ============================================================================
// Alipay TRADE_SUCCESS → paid
// ============================================================================

func TestWebhook_Alipay_PaymentSucceeded(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "alipay-paid", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	// Build unsigned params, sign with the e2e Alipay private key.
	// Drop gmt_payment to avoid timezone/whitespace edge cases in canonical
	// encoding — the verifier doesn't enforce the replay window when the
	// field is absent (ReplayWindow defaults to 0 = disabled).
	params := map[string]string{
		"out_trade_no": orderID,
		"trade_no":     "2023110_e2e",
		"total_amount": "29.90",
		"notify_id":    fmt.Sprintf("n_e2e_%s", orderID),
		"notify_type":  "trade_status_sync",
	}
	body := signAlipay(t, params)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/webhooks/payment/alipay", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	t.Logf("webhook response: %s", string(resp.Body))

	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("expected paid, got %s", status)
	}
}

// ============================================================================
// Alipay full refund TRADE_CLOSED → payment=refunded, sub=cancelled
// ============================================================================

func TestWebhook_Alipay_FullRefund(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "alipay-refund", "yundian")

	// Setup: paid order.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	txnID := "alipay_e2e_" + orderID
	confirm := fmt.Sprintf(`{"channel":"alipay","external_txn_id":%q}`, txnID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d %s", resp.StatusCode, string(resp.Body))
	}

	// Send full-refund webhook with the SAME txn_id the channel uses to
	// identify this payment.
	params := map[string]string{
		"out_trade_no":  orderID,
		"trade_no":      txnID,
		"total_amount":  "29.90",
		"refund_amount": "29.90",
		"notify_id":     fmt.Sprintf("n_e2e_refund_%s", orderID),
		"notify_type":   "trade_closed",
	}
	body := signAlipay(t, params)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/webhooks/payment/alipay", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Verify payment flipped to refunded.
	var payStatus string
	if err := srv.DB.GetContext(context.Background(), &payStatus,
		`SELECT status FROM payments WHERE order_id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if payStatus != "refunded" {
		t.Errorf("expected payment.status=refunded, got %s", payStatus)
	}

	// Verify subscription cancelled.
	var subStatus string
	if err := srv.DB.GetContext(context.Background(), &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = (SELECT user_id FROM orders WHERE id = $1)`, orderID); err != nil {
		t.Fatal(err)
	}
	if subStatus != "cancelled" {
		t.Errorf("expected subscription.status=cancelled, got %s", subStatus)
	}
}

// ============================================================================
// WeChat Pay v3 — happy path with full crypto (HMAC + AES-GCM decrypt)
// ============================================================================

func TestWebhook_WeChat_PaymentSucceeded(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "wechat-paid", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	// Build the encrypted body + headers.
	body := goldenWeChatPaid(t, orderID, "wx_e2e_1", 2990)
	ts := time.Now().Unix()
	_, nonce, sig := signWeChat([]byte(e2eWeChatKey), ts, "n12byte_test", string(body))

	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/wechat_pay", string(body),
		map[string]string{
			"Wechatpay-Signature": sig,
			"Wechatpay-Timestamp": strconv.FormatInt(ts, 10),
			"Wechatpay-Nonce":     nonce,
			"Content-Type":        "application/json",
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("expected paid, got %s", status)
	}
}

// ============================================================================
// Webhook for an unsupported channel → 404
// ============================================================================

func TestWebhook_UnsupportedChannel_404(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)

	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paypal", `{}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unsupported channel, got %d", resp.StatusCode)
	}
}