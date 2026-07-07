package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// PayPal webhook (one-time capture) — happy path
// ============================================================================

func TestE2E_Paypal_CaptureCompleted_HappyPath(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "paypal-cap", "yundian")

	// Create the order first so the webhook can find it.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	// Build + send the PayPal capture-completed webhook.
	body := paypalCaptureCompletedBody(
		"WH-E2E-PAYPAL-"+uuid.NewString(),
		"CAPTURE-E2E-"+uuid.NewString(),
		orderID,
		"29.90",
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paypal", string(body),
		paypalHeaders("tid-capture-"+uuid.NewString(), "sig-stub"))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Order should be paid.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("expected order.status=paid, got %s", status)
	}

	// Payment row should exist with channel='paypal' and the expected amount.
	var ch, payStatus string
	var amt float64
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT channel, status, amount FROM payments WHERE order_id = $1`, orderID,
	).Scan(&ch, &payStatus, &amt); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if ch != "paypal" {
		t.Errorf("expected channel=paypal, got %s", ch)
	}
	if payStatus != "paid" {
		t.Errorf("expected payment.status=paid, got %s", payStatus)
	}
	if amt != 29.90 {
		t.Errorf("expected amount=29.90, got %v", amt)
	}

	// Event-level dedup row inserted.
	var eventID string
	if err := srv.DB.GetContext(context.Background(), &eventID,
		`SELECT event_id FROM webhook_events WHERE channel = 'paypal' LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if eventID == "" {
		t.Error("expected webhook_events row for PayPal channel")
	}
}

// ============================================================================
// PayPal webhook — missing headers → 400
// ============================================================================

func TestE2E_Paypal_MissingHeaders_400(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)

	// Need an order so verify doesn't side-effect; the missing-header path
	// rejects the request before any DB write.
	token, _ := loginAndGetToken(t, srv.Engine, "paypal-bad", "yundian")
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	body := paypalCaptureCompletedBody(
		"WH-E2E-BAD-"+uuid.NewString(),
		"CAP-X", orderID, "1.00",
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paypal", string(body), map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 on missing headers, got %d (body: %s)", resp.StatusCode, string(resp.Body))
	}
}

// ============================================================================
// PayPal webhook — channel disabled would return 404, but our harness
// always wires all five channels. Covered by middleware unit tests; the
// e2e is left as a stub.
// ============================================================================

func TestE2E_Paypal_ChannelDisabled_404(t *testing.T) {
	t.Skip("E2E server always configures all five channels; covered by middleware unit tests")
}

// ============================================================================
// PayPal webhook — renewal (PAYMENT.SALE.COMPLETED) extends expires_at
// ============================================================================

func TestE2E_Paypal_SaleCompleted_ExtendsExpiresAt(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "paypal-renew", "yundian")

	// Initial capture creates the order + payment + activates the subscription.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r2 struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r2)
	orderID := r2.Data.ID

	billingAgreementID := "I-E2E-" + uuid.NewString()
	body := paypalCaptureCompletedBody(
		"WH-E2E-INIT-"+uuid.NewString(),
		"CAP-INIT-"+uuid.NewString(),
		orderID, "29.90",
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paypal", string(body),
		paypalHeaders("tid-init-"+uuid.NewString(), "sig-stub"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial capture webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	// Locate the active subscription.
	var userID string
	if err := srv.DB.GetContext(context.Background(), &userID,
		`SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	var subID string
	var expiresBefore *time.Time
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT id, expires_at FROM subscriptions WHERE user_id = $1 AND status = 'active' LIMIT 1`,
		userID,
	).Scan(&subID, &expiresBefore); err != nil {
		t.Fatal(err)
	}

	// Shortcut the BILLING.SUBSCRIPTION.CREATED path by stamping external_sub_id.
	if _, err := srv.DB.ExecContext(context.Background(),
		`UPDATE subscriptions SET external_subscription_id = $1 WHERE id = $2`,
		billingAgreementID, subID); err != nil {
		t.Fatal(err)
	}

	// Renewal webhook.
	nextBilling := time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body = paypalSaleCompletedBody(
		"WH-E2E-RENEW-"+uuid.NewString(),
		"SALE-RENEW-"+uuid.NewString(),
		billingAgreementID,
		orderID,
		nextBilling,
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paypal", string(body),
		paypalHeaders("tid-renew-"+uuid.NewString(), "sig-stub"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renewal webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	// After renewal: same subscription, expires_at advanced.
	var expiresAfter *time.Time
	if err := srv.DB.GetContext(context.Background(), &expiresAfter,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if expiresAfter == nil {
		t.Fatalf("expected expires_at to be set after renewal, got nil")
	}
	if expiresBefore != nil && !expiresAfter.After(*expiresBefore) {
		t.Errorf("expected expires_at to advance: before=%v after=%v", expiresBefore, expiresAfter)
	}

	// A renewal payment row exists.
	var renewalCount int
	if err := srv.DB.GetContext(context.Background(), &renewalCount,
		`SELECT COUNT(*) FROM payments WHERE channel = 'paypal' AND external_txn_id LIKE 'SALE-RENEW-%'`); err != nil {
		t.Fatal(err)
	}
	if renewalCount == 0 {
		t.Errorf("expected a renewal payment row")
	}
}
