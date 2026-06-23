package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestPayments_OrderLifecycle: create order → confirm → verify state.
func TestPayments_OrderLifecycle(t *testing.T) {
	// Note: e2e tests share a single PG database. Each setupE2EServerWithVerifier
	// cleans the tables before seeding, so concurrent test runs would race on
	// those DELETEs. Run these tests serially.

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "payments-lifecycle", "yundian")

	// 1. Create order
	t.Run("create_order", func(t *testing.T) {
		body := `{"plan_id":"monthly"}`
		resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var r struct {
			Data struct {
				ID     string  `json:"id"`
				Status string  `json:"status"`
				Amount float64 `json:"amount"`
			} `json:"data"`
		}
		resp.JSON(t, &r)
		if r.Data.Status != "pending" {
			t.Errorf("expected status=pending, got %s", r.Data.Status)
		}
		if r.Data.Amount != 29.9 {
			t.Errorf("expected amount=29.9 (plan snapshot), got %v", r.Data.Amount)
		}
	})

	// 2. Re-create for the same user — should hit the "already has active sub"
	//    guard. Wait — that only fires when the user actually HAS an active sub.
	//    The first order is still `pending`, not `active`. So this should succeed.
	t.Run("second_pending_order_allowed", func(t *testing.T) {
		body := `{"plan_id":"monthly"}`
		resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 (no active sub yet), got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
	})

	// 3. Confirm the first order
	t.Run("confirm_order", func(t *testing.T) {
		// Create a fresh user for clean state.
		token2, userID := loginAndGetToken(t, srv.Engine, "payments-confirm", "yundian")
		body := `{"plan_id":"monthly"}`
		resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token2))
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

		confirm := fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_%s","amount":29.90,"currency":"CNY"}`, orderID)
		resp = doRequest(t, srv.Engine, http.MethodPost,
			"/payments/orders/"+orderID+"/confirm", confirm, authHeader(token2))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("confirm: expected 200, got %d — body: %s", resp.StatusCode, string(resp.Body))
		}
		var cr struct {
			Data struct {
				PaymentID             string `json:"PaymentID"`
				Status                string `json:"Status"`
				ActivatedSubscription bool   `json:"ActivatedSubscription"`
			} `json:"data"`
		}
		resp.JSON(t, &cr)
		if cr.Data.Status != "paid" {
			t.Errorf("expected status=paid, got %s", cr.Data.Status)
		}
		if !cr.Data.ActivatedSubscription {
			t.Error("expected activated_subscription=true")
		}

		// Verify order row in DB.
		var status string
		if err := srv.DB.GetContext(context.Background(), &status,
			`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
			t.Fatalf("read order: %v", err)
		}
		if status != "paid" {
			t.Errorf("expected DB status=paid, got %s", status)
		}

		// Verify subscription row exists.
		var subStatus string
		if err := srv.DB.GetContext(context.Background(), &subStatus,
			`SELECT status FROM subscriptions WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("read sub: %v", err)
		}
		if subStatus != "active" {
			t.Errorf("expected subscription=active, got %s", subStatus)
		}
	})
}

// TestPayments_ConfirmChannelMismatch: confirm with a different channel
// than the existing paid payment → 409.
func TestPayments_ConfirmChannelMismatch(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "channel-mismatch", "yundian")

	// First, create + confirm with Stripe.
	body := `{"plan_id":"monthly"}`
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	confirm1 := fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_first_%s"}`, orderID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm1, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first confirm: %d %s", resp.StatusCode, string(resp.Body))
	}

	// Now try to confirm with a different channel.
	confirm2 := fmt.Sprintf(`{"channel":"wechat_pay","external_txn_id":"wx_e2e_second_%s"}`, orderID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm2, authHeader(token))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 channel mismatch, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
}

// TestPayments_RefundIdempotency: same Idempotency-Key → same row, no second
// channel call. The stubRefundAPI counts calls externally via the DB: if
// idempotency works, exactly one refunds row exists.
func TestPayments_RefundIdempotency(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "refund-idem", "yundian")

	// Create + confirm to get a paid payment.
	body := `{"plan_id":"monthly"}`
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	confirm := fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_refund_%s"}`, orderID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d %s", resp.StatusCode, string(resp.Body))
	}
	var cr struct {
		Data struct{ PaymentID string `json:"PaymentID"` } `json:"data"`
	}
	resp.JSON(t, &cr)
	paymentID := cr.Data.PaymentID

	// First refund.
	refund1 := fmt.Sprintf(`{"payment_id":%q,"amount":5.0,"reason":"first call"}`, paymentID)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/refunds", refund1,
		map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": "idem-refund-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refund: %d %s", resp.StatusCode, string(resp.Body))
	}
	var rr1 struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &rr1)

	// Second refund with the SAME Idempotency-Key → must return the SAME id.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/refunds", refund1,
		map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": "idem-refund-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second refund: %d %s", resp.StatusCode, string(resp.Body))
	}
	var rr2 struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &rr2)

	if rr1.Data.ID != rr2.Data.ID {
		t.Errorf("idempotency violated: %s vs %s", rr1.Data.ID, rr2.Data.ID)
	}

	// And there should be exactly one refund row.
	var count int
	if err := srv.DB.GetContext(context.Background(), &count,
		`SELECT COUNT(*) FROM refunds WHERE payment_id = $1`, paymentID); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 refund row, got %d", count)
	}
}

// TestPayments_RefundMissingIdempotencyKey: 400 (handler validation).
func TestPayments_RefundMissingIdempotencyKey(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "refund-noidem", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodPost, "/refunds",
		`{"payment_id":"00000000-0000-0000-0000-000000000000","amount":5.0}`,
		authHeader(token))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestPayments_RefundSumInvariant: two refunds with different keys that
// exceed the payment amount → 400.
func TestPayments_RefundSumInvariant(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "refund-sum", "yundian")

	// Setup: paid payment of 29.90
	body := `{"plan_id":"monthly"}`
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders", body, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	confirm := fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_sum_%s"}`, orderID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm, authHeader(token))
	var cr struct {
		Data struct{ PaymentID string `json:"PaymentID"` } `json:"data"`
	}
	resp.JSON(t, &cr)
	paymentID := cr.Data.PaymentID

	// First refund of 25 — OK.
	refund1 := fmt.Sprintf(`{"payment_id":%q,"amount":25.0}`, paymentID)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/refunds", refund1,
		map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": "sum-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first refund: %d %s", resp.StatusCode, string(resp.Body))
	}

	// Second refund of 10 — would push total to 35, exceeding 29.90. Must fail.
	refund2 := fmt.Sprintf(`{"payment_id":%q,"amount":10.0}`, paymentID)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/refunds", refund2,
		map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": "sum-2"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 (sum invariant), got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
}

// TestPayments_OwnershipIsolation: another user cannot see this user's
// payment.
func TestPayments_OwnershipIsolation(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	tokenA, _ := loginAndGetToken(t, srv.Engine, "owner-a", "yundian")
	tokenB, _ := loginAndGetToken(t, srv.Engine, "owner-b", "yundian")

	// A creates + confirms an order.
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(tokenA))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm",
		fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_owner_%s"}`, orderID),
		authHeader(tokenA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d %s", resp.StatusCode, string(resp.Body))
	}
	var cr struct {
		Data struct{ PaymentID string `json:"PaymentID"` } `json:"data"`
	}
	resp.JSON(t, &cr)
	paymentID := cr.Data.PaymentID

	// B tries to GET A's payment → 404 (hide existence from non-owner).
	resp = doRequest(t, srv.Engine, http.MethodGet, "/payments/"+paymentID, "", authHeader(tokenB))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-owner read, got %d", resp.StatusCode)
	}

	// B tries to refund A's payment → 404.
	refundBody := fmt.Sprintf(`{"payment_id":%q,"amount":1.0}`, paymentID)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/refunds", refundBody,
		map[string]string{"Authorization": "Bearer " + tokenB, "Idempotency-Key": "cross-owner"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-owner refund, got %d", resp.StatusCode)
	}
}

// TestPayments_UnauthenticatedRejected: no JWT → 401.
func TestPayments_UnauthenticatedRejected(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	resp := doRequest(t, srv.Engine, http.MethodGet, "/payments", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// TestPayments_CancelOrder: pending → cancelled.
func TestPayments_CancelOrder(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "cancel-pending", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	resp = doRequest(t, srv.Engine, http.MethodDelete,
		"/payments/orders/"+orderID, "", authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", resp.StatusCode, string(resp.Body))
	}

	// Verify status in DB.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Errorf("expected status=cancelled, got %s", status)
	}
}

// TestPayments_LatePaymentHonored: an order that the sweeper marked
// `expired` can still be confirmed — the channel webhook arrives late and
// we honor the payment. We simulate the race by manually flipping the
// order to expired and then calling confirm.
func TestPayments_LatePaymentHonored(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "late-pay", "yundian")

	// Create + manually expire (simulating sweeper).
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly"}`, authHeader(token))
	var r struct {
		Data struct{ ID string `json:"id"` } `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	if _, err := srv.DB.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired', expires_at = now() - INTERVAL '1 minute' WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}

	// Confirm — should honor.
	confirm := fmt.Sprintf(`{"channel":"stripe","external_txn_id":"pi_e2e_late_%s"}`, orderID)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/payments/orders/"+orderID+"/confirm", confirm, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (honor), got %d — body: %s", resp.StatusCode, string(resp.Body))
	}

	// Verify order flipped back to paid.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("expected status=paid, got %s", status)
	}

	// Verify audit_log row was written.
	var action string
	var tags []byte
	err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT action, tags FROM audit_log WHERE target = $1 AND action = 'late_payment_post_expiry'`,
		fmt.Sprintf("order:%s", orderID)).Scan(&action, &tags)
	if err != nil {
		if err == sql.ErrNoRows {
			t.Error("expected audit_log row tagged late_payment_post_expiry")
		} else {
			t.Fatal(err)
		}
	}
}

// TestPayments_ListPayments: returns the user's payments.
func TestPayments_ListPayments(t *testing.T) {

	srv := setupE2EServerWithVerifier(t)
	token, _ := loginAndGetToken(t, srv.Engine, "list-payments", "yundian")

	resp := doRequest(t, srv.Engine, http.MethodGet, "/payments", "", authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Empty list for fresh user.
	if len(r.Data) != 0 {
		t.Errorf("expected 0 payments, got %d", len(r.Data))
	}
}