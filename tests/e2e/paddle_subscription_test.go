package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// paddleSubscriptionBody builds a Paddle subscription.* webhook envelope
// (data.id is the Paddle subscription id — parsePaddle maps it onto
// ExternalSubscriptionID).
func paddleSubscriptionBody(eventID, eventType, subID string) []byte {
	return []byte(`{
	  "event_id": "` + eventID + `",
	  "event_type": "` + eventType + `",
	  "occurred_at": "` + time.Now().UTC().Format(time.RFC3339) + `",
	  "data": {"id": "` + subID + `", "status": "canceled"}
	}`)
}

// settlePaddleSub drives a paddle order to paid and returns the Paddle
// subscription id stamped on the local subscription.
func settlePaddleSub(t *testing.T, srv *E2EServer, token, orderID string) string {
	t.Helper()
	subID := "sub_e2e_" + uuid.NewString()
	body := paddleTransactionBody(
		"evt-e2e-paddle-"+uuid.NewString(),
		"transaction.completed",
		"txn-e2e-"+uuid.NewString(),
		subID, orderID, "2990", "USD", "web",
	)
	resp := postPaddleWebhook(t, srv, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settle webhook: %d %s", resp.StatusCode, string(resp.Body))
	}
	return subID
}

// ============================================================================
// Paddle — subscription self-service: cancel at period end + webhook flip
// ============================================================================

func TestE2E_Paddle_SubscriptionCancel_EndToEnd(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-cancel", "yundian").AccessToken
	orderID := createPaddleOrder(t, srv, token, "paddle-cancel")
	subID := settlePaddleSub(t, srv, token, orderID)

	// Self-serve cancel: channel-side cancel succeeds, local sub stays
	// active (access until the paid period ends).
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/subscription/cancel", `{}`, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", resp.StatusCode, string(resp.Body))
	}
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM subscriptions WHERE external_subscription_id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("sub status after cancel request: %s, want active (flip deferred to webhook)", status)
	}

	// Paddle applies the scheduled change at period end and fires
	// subscription.canceled → local flip.
	cancelBody := paddleSubscriptionBody("evt-e2e-pd-cancel-"+uuid.NewString(), "subscription.canceled", subID)
	resp = postPaddleWebhook(t, srv, cancelBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel webhook: %d %s", resp.StatusCode, string(resp.Body))
	}
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM subscriptions WHERE external_subscription_id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("sub status after cancel webhook: %s, want cancelled", status)
	}
}

func TestE2E_Paddle_SubscriptionCancel_NoActiveSub(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-cancel-none", "yundian").AccessToken
	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/subscription/cancel", `{}`, authHeader(token))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel with no sub: %d %s", resp.StatusCode, string(resp.Body))
	}
}

// ============================================================================
// Paddle — subscription upgrade: monthly → yearly with proration
// ============================================================================

func TestE2E_Paddle_SubscriptionUpgrade_EndToEnd(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-upgrade", "yundian").AccessToken
	orderID := createPaddleOrder(t, srv, token, "paddle-upgrade")
	subID := settlePaddleSub(t, srv, token, orderID)

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/subscription/upgrade",
		`{"plan_id":"yearly_usd"}`, authHeader(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upgrade: %d %s", resp.StatusCode, string(resp.Body))
	}

	var planID string
	var expAt time.Time
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT plan_id, expires_at FROM subscriptions WHERE external_subscription_id = $1`, subID,
	).Scan(&planID, &expAt); err != nil {
		t.Fatal(err)
	}
	if planID != "yearly_usd" {
		t.Fatalf("plan_id after upgrade: %s, want yearly_usd", planID)
	}
	// Mock client anchors next_billed_at ~1 year out.
	if expAt.Before(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("expires_at after upgrade: %v, want ~1 year out", expAt)
	}

	// Same-plan re-upgrade is a 409, not a second Paddle call.
	resp = doRequest(t, srv.Engine, http.MethodPost, "/payments/subscription/upgrade",
		`{"plan_id":"yearly_usd"}`, authHeader(token))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("same-plan upgrade: %d, want 409", resp.StatusCode)
	}
}
