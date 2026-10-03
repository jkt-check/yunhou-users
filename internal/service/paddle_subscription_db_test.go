package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// ============================================================================
// CancelChannelSubscription — POST /payments/subscription/cancel (Paddle)
// ============================================================================

func countAudit(t *testing.T, db *sqlx.DB, action string) int {
	t.Helper()
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = $1`, action); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return n
}

func TestCancelChannelSubscription_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_cancel_" + mustNewUUID()[:8]
	subID := seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)

	sub, err := svc.CancelChannelSubscription(context.Background(), uid)
	if err != nil {
		t.Fatalf("CancelChannelSubscription: %v", err)
	}
	if stub.cancelCalls != 1 || stub.cancelGotID != extSubID {
		t.Fatalf("paddle cancel calls=%d id=%q, want 1/%s", stub.cancelCalls, stub.cancelGotID, extSubID)
	}
	if sub.ID != subID {
		t.Fatalf("sub id = %q, want %q", sub.ID, subID)
	}
	// Local status stays active — the flip hangs off the
	// subscription.canceled webhook (period end), so the buyer keeps the
	// access they paid for.
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if status != "active" {
		t.Fatalf("status = %q, want active (flip deferred to webhook)", status)
	}
	if countAudit(t, db, "paddle_subscription_cancel_requested") != 1 {
		t.Error("expected audit row paddle_subscription_cancel_requested")
	}
}

func TestCancelChannelSubscription_NoActiveSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	svc.SetPaddleClient(&stubPaddle{})

	if _, err := svc.CancelChannelSubscription(context.Background(), uid); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("expected ErrSubscriptionNotFound, got %v", err)
	}
}

func TestCancelChannelSubscription_NotChannelManaged(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	// Local/WeChat-style sub: no external_subscription_id.
	seedActiveSub(t, db, uid, "monthly", time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	if _, err := svc.CancelChannelSubscription(context.Background(), uid); !errors.Is(err, ErrSubscriptionNotChannelManaged) {
		t.Fatalf("expected ErrSubscriptionNotChannelManaged, got %v", err)
	}
	if stub.cancelCalls != 0 {
		t.Fatal("paddle must not be called for a non-channel-managed sub")
	}
}

func TestCancelChannelSubscription_UpstreamError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_cancel_err_" + mustNewUUID()[:8]
	seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))

	svc.SetPaddleClient(&stubPaddle{cancelErr: errors.New("paddle 503")})
	if _, err := svc.CancelChannelSubscription(context.Background(), uid); err == nil || !strings.Contains(err.Error(), "paddle cancel subscription") {
		t.Fatalf("expected wrapped paddle error, got %v", err)
	}
}

func TestCancelChannelSubscription_NoPaddleClient(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	if _, err := svc.CancelChannelSubscription(context.Background(), uid); !errors.Is(err, ErrPaddleNotConfigured) {
		t.Fatalf("expected ErrPaddleNotConfigured, got %v", err)
	}
}

// ============================================================================
// UpgradeChannelSubscription — POST /payments/subscription/upgrade (Paddle)
// ============================================================================

func TestUpgradeChannelSubscription_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_up_" + mustNewUUID()[:8]
	subID := seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))

	next := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	stub := &stubPaddle{updateNext: &next}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_yearly_test"})

	res, err := svc.UpgradeChannelSubscription(context.Background(), uid, "yearly")
	if err != nil {
		t.Fatalf("UpgradeChannelSubscription: %v", err)
	}
	if stub.updateCalls != 1 || stub.updateGotID != extSubID || stub.updateGotPrice != "pri_yearly_test" {
		t.Fatalf("paddle update calls=%d id=%q price=%q", stub.updateCalls, stub.updateGotID, stub.updateGotPrice)
	}
	if res.FromPlanID != "monthly" || res.ToPlanID != "yearly" {
		t.Fatalf("result = %+v", res)
	}
	var planID string
	var expAt time.Time
	if err := db.GetContext(context.Background(), &planID,
		`SELECT plan_id FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if err := db.GetContext(context.Background(), &expAt,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub expiry: %v", err)
	}
	if planID != "yearly" {
		t.Fatalf("plan_id = %q, want yearly", planID)
	}
	if !expAt.Equal(next) {
		t.Fatalf("expires_at = %v, want channel next_billed_at %v", expAt, next)
	}
	if countAudit(t, db, "paddle_subscription_upgraded") != 1 {
		t.Error("expected audit row paddle_subscription_upgraded")
	}
}

func TestUpgradeChannelSubscription_Guards(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_guard_" + mustNewUUID()[:8]
	seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_yearly_test", "monthly": "pri_monthly_test"})

	ctx := context.Background()
	if _, err := svc.UpgradeChannelSubscription(ctx, uid, "monthly"); !errors.Is(err, ErrSamePlanChange) {
		t.Fatalf("same plan: expected ErrSamePlanChange, got %v", err)
	}
	if _, err := svc.UpgradeChannelSubscription(ctx, uid, "no-such-plan"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown plan: expected ErrPlanNotFound, got %v", err)
	}
	if _, err := svc.UpgradeChannelSubscription(ctx, uid, "free"); !errors.Is(err, ErrPlanChangeNotUpgrade) {
		t.Fatalf("shorter cycle: expected ErrPlanChangeNotUpgrade, got %v", err)
	}
}

func TestUpgradeChannelSubscription_NoPriceConfigured(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	seedPaddleSub(t, db, uid, "sub_nopri_"+mustNewUUID()[:8], time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	// no SetPaddlePrices — operator forgot the yearly entry
	if _, err := svc.UpgradeChannelSubscription(context.Background(), uid, "yearly"); !errors.Is(err, ErrPaddlePriceNotConfigured) {
		t.Fatalf("expected ErrPaddlePriceNotConfigured, got %v", err)
	}
}

func TestUpgradeChannelSubscription_DowngradeRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	// Yearly sub trying to "upgrade" to monthly = downgrade.
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'yearly', 'active', $3, $4)
	`, subID, uid, time.Now().Add(200*24*time.Hour), "sub_down_"+mustNewUUID()[:8]); err != nil {
		t.Fatalf("seed yearly sub: %v", err)
	}
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_m", "yearly": "pri_y"})
	if _, err := svc.UpgradeChannelSubscription(context.Background(), uid, "monthly"); !errors.Is(err, ErrPlanChangeNotUpgrade) {
		t.Fatalf("expected ErrPlanChangeNotUpgrade, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a downgrade attempt")
	}
}

// ============================================================================
// Webhook: subscription.canceled → local flip + audit
// ============================================================================

func paddleCancelEvent(eventID, extSubID string) WebhookEvent {
	return WebhookEvent{
		Channel: "paddle", EventID: eventID, EventType: "subscription.canceled",
		ExternalSubscriptionID: extSubID,
		RawPayload:             json.RawMessage(`{}`),
	}
}

func TestOnWebhook_PaddleSubscriptionCancelled_Flips(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_wcancel_" + mustNewUUID()[:8]
	subID := seedPaddleSub(t, db, uid, extSubID, time.Now().Add(15*24*time.Hour))

	res, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-"+mustNewUUID()[:8], extSubID))
	if err != nil {
		t.Fatalf("OnWebhook subscription.canceled: %v", err)
	}
	if res.DomainAction != "subscription_cancelled" {
		t.Fatalf("DomainAction = %q", res.DomainAction)
	}
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", status)
	}
	if countAudit(t, db, "paddle_subscription_cancelled") != 1 {
		t.Error("expected audit row paddle_subscription_cancelled")
	}

	// Redelivery (same event id) → duplicate ack, still cancelled, no error.
	res2, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-"+mustNewUUID()[:8], extSubID))
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	_ = res2
	if countAudit(t, db, "paddle_subscription_cancelled") != 1 {
		t.Error("redelivery must not duplicate the cancel audit")
	}
}

func TestOnWebhook_PaddleSubscriptionCancelled_UnknownSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-unk-"+mustNewUUID()[:8], "sub_unknown_"+mustNewUUID()[:8]))
	if err != nil {
		t.Fatalf("unknown sub must ack, got %v", err)
	}
	if countAudit(t, db, "paddle_cancel_unknown_subscription") != 1 {
		t.Error("expected audit row paddle_cancel_unknown_subscription")
	}
}
