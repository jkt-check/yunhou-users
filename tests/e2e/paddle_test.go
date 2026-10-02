package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// paddleTransactionBody builds a Paddle webhook envelope matching the REAL
// notification shape (verified against live sandbox payloads 2026-10-01):
// money lives under data.details.totals; top-level data.totals is null.
// total is in minor units ("999" = $9.99); empty orderID/total omit those
// blocks. origin mirrors data.origin: "web" for initial checkout
// transactions, "subscription_recurring" for channel-side auto-renewal
// charges (the service routes renewals on this field).
func paddleTransactionBody(eventID, eventType, txnID, subID, orderID, total, currency, origin string) []byte {
	custom := ""
	if orderID != "" {
		custom = `"custom_data":{"order_id":"` + orderID + `"},`
	}
	totals := `"totals":null,`
	if total != "" {
		totals = `"totals":null,"details":{"totals":{"total":"` + total + `","subtotal":"` + total + `","tax":"0","grand_total":"` + total + `"}},`
	}
	return []byte(`{
	  "event_id": "` + eventID + `",
	  "event_type": "` + eventType + `",
	  "occurred_at": "` + time.Now().UTC().Format(time.RFC3339) + `",
	  "data": {
	    "id": "` + txnID + `",
	    "status": "completed",
	    "subscription_id": "` + subID + `",
	    "currency_code": "` + currency + `",
	    ` + custom + totals + `
	    "origin": "` + origin + `"
	  }
	}`)
}

func postPaddleWebhook(t *testing.T, srv *E2EServer, body []byte) *httpResponse {
	t.Helper()
	return doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body),
		paddleSignatureHeaders(e2ePaddleSecret, body))
}

func createPaddleOrder(t *testing.T, srv *E2EServer, token, email string) string {
	t.Helper()
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order (%s): %d %s", email, resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	return r.Data.ID
}

// ============================================================================
// Paddle — order creation mints a checkout intent (mock client)
// ============================================================================

func TestE2E_Paddle_CreateOrder_ProviderIntent(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-intent", "yundian").AccessToken

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID             string          `json:"id"`
			ProviderIntent json.RawMessage `json:"provider_intent"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	var intent map[string]string
	if err := json.Unmarshal(r.Data.ProviderIntent, &intent); err != nil {
		t.Fatalf("provider_intent: %v", err)
	}
	if !strings.HasPrefix(intent["transaction_id"], "txn_mock_") {
		t.Fatalf("transaction_id: %v", intent)
	}
	if !strings.Contains(intent["checkout_url"], "_ptxn="+intent["transaction_id"]) {
		t.Fatalf("checkout_url: %v", intent)
	}
	if intent["client_token"] != "" {
		// mock client has no token configured — must be empty, never invented
		t.Fatalf("client_token should be empty in mock mode: %v", intent)
	}
}

// ============================================================================
// Paddle — transaction.completed settles the order and stamps the sub id;
// transaction.billed is audit-only; renewal settles on completed with
// origin=subscription_recurring via the (mock) subscription lookup
// ============================================================================

func TestE2E_Paddle_TransactionCompleted_HappyPath(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-happy", "yundian").AccessToken
	orderID := createPaddleOrder(t, srv, token, "paddle-happy")

	subID := "sub_e2e_" + uuid.NewString()
	body := paddleTransactionBody(
		"evt-e2e-paddle-"+uuid.NewString(),
		"transaction.completed",
		"txn-e2e-"+uuid.NewString(),
		subID, orderID, "2990", "USD", "web",
	)
	resp := postPaddleWebhook(t, srv, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("order status: %s", status)
	}

	// Payment row with the paddle channel + minor-unit-normalized amount.
	var ch, payStatus string
	var amt float64
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT channel, status, amount FROM payments WHERE order_id = $1`, orderID,
	).Scan(&ch, &payStatus, &amt); err != nil {
		t.Fatal(err)
	}
	if ch != "paddle" || payStatus != "paid" || amt != 29.90 {
		t.Fatalf("payment row: %s %s %v", ch, payStatus, amt)
	}

	// Activation + external_subscription_id stamping for renewals.
	var subStatus, extSubID string
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT status, external_subscription_id FROM subscriptions
		 WHERE user_id = (SELECT user_id FROM orders WHERE id = $1)
		 ORDER BY created_at DESC LIMIT 1`, orderID,
	).Scan(&subStatus, &extSubID); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("sub status: %s", subStatus)
	}
	if extSubID != subID {
		t.Fatalf("external_subscription_id not stamped: %q", extSubID)
	}

	// transaction.billed fires at invoice ISSUANCE (pre-collection) and
	// must be audit-only: ack 200, no payment row, no expiry change.
	billedTxn := "txn-e2e-billed-" + uuid.NewString()
	billedBody := paddleTransactionBody(
		"evt-e2e-paddle-billed-"+uuid.NewString(),
		"transaction.billed",
		billedTxn, subID, "", "2990", "USD", "subscription_recurring",
	)
	resp = postPaddleWebhook(t, srv, billedBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("billed webhook: %d %s", resp.StatusCode, string(resp.Body))
	}
	var billedCount int
	if err := srv.DB.GetContext(context.Background(), &billedCount,
		`SELECT COUNT(*) FROM payments WHERE channel='paddle' AND external_txn_id = $1`, billedTxn); err != nil {
		t.Fatal(err)
	}
	if billedCount != 0 {
		t.Fatalf("transaction.billed must not book payments, got %d", billedCount)
	}

	// Renewal settlement: transaction.completed with
	// origin=subscription_recurring. Paddle propagates the subscription's
	// custom_data, so the ORIGINAL order_id rides along — routing must key
	// on origin, not custom_data. The mock client returns now+1mo as
	// next_billed_at (real mode would hit the Paddle API).
	renewBody := paddleTransactionBody(
		"evt-e2e-paddle-renew-"+uuid.NewString(),
		"transaction.completed",
		"txn-e2e-renew-"+uuid.NewString(),
		subID, orderID, "2990", "USD", "subscription_recurring",
	)
	resp = postPaddleWebhook(t, srv, renewBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renewal webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	var expires *time.Time
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT expires_at FROM subscriptions WHERE external_subscription_id = $1`, subID,
	).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires == nil || !expires.After(time.Now().Add(20*24*time.Hour)) {
		t.Fatalf("expires_at not extended by renewal: %v", expires)
	}

	var renewCount int
	if err := srv.DB.GetContext(context.Background(), &renewCount,
		`SELECT COUNT(*) FROM payments WHERE channel = 'paddle' AND external_txn_id LIKE 'txn-e2e-renew-%'`); err != nil {
		t.Fatal(err)
	}
	if renewCount != 1 {
		t.Fatalf("renewal payment rows: %d", renewCount)
	}

	// Lifecycle events are audit-only: subscription.canceled must ack 200
	// without touching the subscription.
	cancelBody := []byte(`{
	  "event_id": "evt-e2e-paddle-cancel-` + uuid.NewString() + `",
	  "event_type": "subscription.canceled",
	  "data": {"id": "` + subID + `", "status": "canceled"}
	}`)
	resp = postPaddleWebhook(t, srv, cancelBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lifecycle webhook: %d %s", resp.StatusCode, string(resp.Body))
	}
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT status FROM subscriptions WHERE external_subscription_id = $1`, subID,
	).Scan(&subStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("lifecycle event must not flip the subscription: %s", subStatus)
	}
}

// ============================================================================
// Paddle — a declined checkout attempt retries inside the SAME transaction:
// payment_failed must be audit-only so the later completed (paid retry)
// still activates the order
// ============================================================================

func TestE2E_Paddle_CheckoutDeclineThenRetry(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-retry", "yundian").AccessToken
	orderID := createPaddleOrder(t, srv, token, "paddle-retry")

	subID := "sub_e2e_" + uuid.NewString()
	txnID := "txn-e2e-retry-" + uuid.NewString()

	failBody := paddleTransactionBody(
		"evt-e2e-paddle-fail-"+uuid.NewString(),
		"transaction.payment_failed",
		txnID, subID, orderID, "", "", "web",
	)
	resp := postPaddleWebhook(t, srv, failBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("payment_failed webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	// The order must NOT be flipped failed — the customer is retrying in
	// the same Paddle checkout.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("order status after decline: %s, want pending", status)
	}

	// The successful retry completes the SAME transaction and activates.
	okBody := paddleTransactionBody(
		"evt-e2e-paddle-ok-"+uuid.NewString(),
		"transaction.completed",
		txnID, subID, orderID, "2990", "USD", "web",
	)
	resp = postPaddleWebhook(t, srv, okBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("completed webhook: %d %s", resp.StatusCode, string(resp.Body))
	}
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("order status after retry: %s, want paid", status)
	}
}

// ============================================================================
// Paddle — bad signature / missing header → 400
// ============================================================================

func TestE2E_Paddle_MissingSignature_400(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	body := paddleTransactionBody("evt-x", "transaction.completed", "txn-x", "sub-x", "order-x", "999", "USD", "web")
	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body), map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestE2E_Paddle_TamperedBody_400(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	body := paddleTransactionBody("evt-x", "transaction.completed", "txn-x", "sub-x", "order-x", "999", "USD", "web")
	headers := paddleSignatureHeaders(e2ePaddleSecret, body)
	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body)+` `, headers) // body no longer matches the hmac
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for tampered body, got %d", resp.StatusCode)
	}
}

// ============================================================================
// Paddle — active subscription blocks a new order (channel-side auto-renew)
// ============================================================================

func TestE2E_Paddle_ActiveSubBlocksNewOrder(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-guard", "yundian").AccessToken
	orderID := createPaddleOrder(t, srv, token, "paddle-guard")

	subID := "sub_e2e_" + uuid.NewString()
	body := paddleTransactionBody("evt-g-"+uuid.NewString(), "transaction.completed",
		"txn-g-"+uuid.NewString(), subID, orderID, "2990", "USD", "web")
	if resp := postPaddleWebhook(t, srv, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("settle: %d %s", resp.StatusCode, string(resp.Body))
	}

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", resp.StatusCode, string(resp.Body))
	}
}
