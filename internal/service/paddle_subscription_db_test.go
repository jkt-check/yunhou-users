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

// TestUpgradeChannelSubscription_RetiredPlanRejected pins the
// accepting_new_subscriptions gate on the upgrade path (2026-10 review):
// IsActive alone is not enough — a retired plan (active but no longer
// accepting new subscriptions) must reject upgrades with
// ErrPlanNotAcceptingNew, the same sentinel CreateOrder's eligibility tx
// uses, before any channel-side call.
func TestUpgradeChannelSubscription_RetiredPlanRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	seedPaddleSub(t, db, uid, "sub_retired_"+mustNewUUID()[:8], time.Now().Add(15*24*time.Hour))
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active, accepting_new_subscriptions)
		VALUES ('yearly-retired', 'Yearly Retired', 149.9, 365, '{}', true, false)
	`); err != nil {
		t.Fatalf("seed retired plan: %v", err)
	}

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly-retired": "pri_retired"})
	if _, err := svc.UpgradeChannelSubscription(context.Background(), uid, "yearly-retired"); !errors.Is(err, ErrPlanNotAcceptingNew) {
		t.Fatalf("expected ErrPlanNotAcceptingNew, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a retired target plan")
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

	// 第二个*不同* event id 的取消事件(自助取消 + 运营后台取消各发一次
	// 的场景):已 cancelled 的行是 no-op,不重复审计、不报错。注意同 event
	// id 的真重投在 OnWebhook 入口就被 webhook_events 去重,到不了这里。
	res2, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-"+mustNewUUID()[:8], extSubID))
	if err != nil {
		t.Fatalf("second distinct cancel event: %v", err)
	}
	if res2.DomainAction != "subscription_cancelled" {
		t.Fatalf("second cancel DomainAction = %q", res2.DomainAction)
	}
	if countAudit(t, db, "paddle_subscription_cancelled") != 1 {
		t.Error("second cancel event must not duplicate the cancel audit")
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

// TestUpgradeChannelSubscription_IntervalFallbackClamped pins the
// day→time.Duration overflow guard on the upgrade fallback path (2026-10
// review): when Paddle's update response carries no next_billed_at, the
// local expiry falls back to now()+plan.interval_days — and a huge
// operator-set interval would wrap the multiply into the past. The clamp
// (maxIntervalDays) keeps it far-future.
func TestUpgradeChannelSubscription_IntervalFallbackClamped(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	seedPaddleSub(t, db, uid, "sub_clamp_"+mustNewUUID()[:8], time.Now().Add(15*24*time.Hour))
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active, accepting_new_subscriptions)
		VALUES ('yearly-huge', 'Yearly Huge', 149.9, 2147483647, '{}', true, true)
	`); err != nil {
		t.Fatalf("seed huge-interval plan: %v", err)
	}

	// updateNext nil → the plan-interval fallback computes expires_at.
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly-huge": "pri_huge"})
	res, err := svc.UpgradeChannelSubscription(context.Background(), uid, "yearly-huge")
	if err != nil {
		t.Fatalf("UpgradeChannelSubscription: %v", err)
	}
	now := time.Now()
	if lo, hi := now.AddDate(200, 0, 0), now.AddDate(300, 0, 0); res.NextBilledAt.Before(lo) || res.NextBilledAt.After(hi) {
		t.Errorf("NextBilledAt = %v, want within [now+200y, now+300y] (clamped, not wrapped)", res.NextBilledAt)
	}
}

// ============================================================================
// Webhook: subscription.updated — scheduled cancel flip + plan re-sync (M3)
// ============================================================================

func paddleUpdatedEvent(eventID, extSubID string) WebhookEvent {
	return WebhookEvent{
		Channel: "paddle", EventID: eventID, EventType: "subscription.updated",
		ExternalSubscriptionID: extSubID,
		RawPayload:             json.RawMessage(`{}`),
	}
}

// scheduled_change.action="cancel": flip auto_renew locally, leave
// status/expires_at untouched (the cancel takes effect at period end —
// the status flip belongs to subscription.canceled).
func TestOnWebhook_PaddleSubscriptionUpdated_ScheduledCancel(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_updc_" + mustNewUUID()[:8]
	expiry := time.Now().Add(15 * 24 * time.Hour).UTC().Truncate(time.Second)
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, expiry)

	e := paddleUpdatedEvent("evt-pd-updc-"+mustNewUUID()[:8], extSubID)
	e.ScheduledChangeAction = "cancel"
	res, err := svc.OnWebhook(context.Background(), e)
	if err != nil {
		t.Fatalf("OnWebhook subscription.updated: %v", err)
	}
	if res.DomainAction != "subscription_updated" {
		t.Errorf("DomainAction = %q, want subscription_updated", res.DomainAction)
	}
	planID, status, autoRenew, gotExpiry := readChannelState(t, db, subID)
	if autoRenew {
		t.Error("auto_renew still true after scheduled-cancel webhook")
	}
	if status != "active" {
		t.Errorf("status = %q, want active (cancel takes effect at period end)", status)
	}
	if planID != "monthly" {
		t.Errorf("plan_id = %q, want untouched monthly", planID)
	}
	if !gotExpiry.Equal(expiry) {
		t.Errorf("expires_at = %v, want untouched %v", gotExpiry, expiry)
	}

	// Idempotent replay (distinct event id, same content): no error, no
	// state change, no duplicate side effects.
	e2 := paddleUpdatedEvent("evt-pd-updc-"+mustNewUUID()[:8], extSubID)
	e2.ScheduledChangeAction = "cancel"
	if _, err := svc.OnWebhook(context.Background(), e2); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, status, autoRenew, _ := readChannelState(t, db, subID); autoRenew || status != "active" {
		t.Errorf("after replay: autoRenew=%v status=%q", autoRenew, status)
	}
}

// items change with a known price_id → plan_id + expires_at re-synced from
// the payload (reconciliation fallback for our change-plan and Paddle-side
// changes).
func TestOnWebhook_PaddleSubscriptionUpdated_PlanSync(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_upds_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_monthly_test", "yearly": "pri_yearly_test"})

	periodEnd := time.Now().Add(380 * 24 * time.Hour).UTC().Truncate(time.Second)
	e := paddleUpdatedEvent("evt-pd-upds-"+mustNewUUID()[:8], extSubID)
	e.PriceIDs = []string{"pri_yearly_test"}
	e.SubExpiresAt = &periodEnd
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("OnWebhook subscription.updated: %v", err)
	}
	planID, _, autoRenew, gotExpiry := readChannelState(t, db, subID)
	if planID != "yearly" {
		t.Errorf("plan_id = %q, want synced yearly", planID)
	}
	if !gotExpiry.Equal(periodEnd) {
		t.Errorf("expires_at = %v, want synced %v", gotExpiry, periodEnd)
	}
	if !autoRenew {
		t.Error("auto_renew must be preserved by a plan sync")
	}
}

// Unknown price_id: plan must NOT be touched, but the event still acks
// (scheduled-cancel flips in the same event would still apply).
func TestOnWebhook_PaddleSubscriptionUpdated_UnknownPrice(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_updu_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_monthly_test"})

	e := paddleUpdatedEvent("evt-pd-updu-"+mustNewUUID()[:8], extSubID)
	e.PriceIDs = []string{"pri_no_such_price"}
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("unknown price must not error: %v", err)
	}
	if planID, _, _, _ := readChannelState(t, db, subID); planID != "monthly" {
		t.Errorf("plan_id = %q, want untouched monthly", planID)
	}
}

// Ambiguous reverse mapping (two plans sharing one price) → skip the plan
// sync, no error.
func TestOnWebhook_PaddleSubscriptionUpdated_AmbiguousPrice(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_upda_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_shared", "yearly": "pri_shared"})

	e := paddleUpdatedEvent("evt-pd-upda-"+mustNewUUID()[:8], extSubID)
	e.PriceIDs = []string{"pri_shared"}
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("ambiguous price must not error: %v", err)
	}
	if planID, _, _, _ := readChannelState(t, db, subID); planID != "monthly" {
		t.Errorf("plan_id = %q, want untouched monthly", planID)
	}
}

// Unknown subscription → audit-only ack 200 (a Paddle sub we never stamped).
func TestOnWebhook_PaddleSubscriptionUpdated_UnknownSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	e := paddleUpdatedEvent("evt-pd-updunk-"+mustNewUUID()[:8], "sub_unknown_"+mustNewUUID()[:8])
	e.ScheduledChangeAction = "cancel"
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("unknown sub must ack, got %v", err)
	}
	if countAudit(t, db, "paddle_updated_unknown_subscription") != 1 {
		t.Error("expected audit row paddle_updated_unknown_subscription")
	}
}

// subscription.past_due stays audit-only: ack 200, no state change.
func TestOnWebhook_PaddleSubscriptionPastDue_AuditOnly(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_pd_due_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paddle", EventID: "evt-pd-due-" + mustNewUUID()[:8], EventType: "subscription.past_due",
		ExternalSubscriptionID: extSubID,
		RawPayload:             json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("past_due must ack: %v", err)
	}
	if res.DomainAction != "none" {
		t.Errorf("DomainAction = %q, want none", res.DomainAction)
	}
	if _, _, autoRenew, _ := readChannelState(t, db, subID); !autoRenew {
		t.Error("past_due must not change subscription state")
	}
}

// subscription.canceled also flips auto_renew=false (M3 contract addition).
func TestOnWebhook_PaddleSubscriptionCancelled_FlipsAutoRenew(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_wcar_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))

	if _, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-ar-"+mustNewUUID()[:8], extSubID)); err != nil {
		t.Fatalf("OnWebhook subscription.canceled: %v", err)
	}
	_, status, autoRenew, _ := readChannelState(t, db, subID)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if autoRenew {
		t.Error("auto_renew still true after subscription.canceled")
	}
}

// Already-cancelled replay with a stale auto_renew=true (pre-M2 row): the
// flag is healed idempotently, no duplicate side effects.
func TestOnWebhook_PaddleSubscriptionCancelled_AlreadyCancelledHealsAutoRenew(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_wcheal_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "cancelled", extSubID, "paddle", true, time.Now().Add(-time.Hour))

	if _, err := svc.OnWebhook(context.Background(),
		paddleCancelEvent("evt-pd-cancel-heal-"+mustNewUUID()[:8], extSubID)); err != nil {
		t.Fatalf("replay cancel: %v", err)
	}
	_, status, autoRenew, _ := readChannelState(t, db, subID)
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if autoRenew {
		t.Error("auto_renew not healed on already-cancelled replay")
	}
}

// A subscription.updated without data.id never reaches the domain handler
// (parse hard-errors), but the service stays defensive: audit-only ack.
func TestOnWebhook_PaddleSubscriptionUpdated_MissingExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paddle", EventID: "evt-pd-updmiss-" + mustNewUUID()[:8], EventType: "subscription.updated",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("missing external sub id must ack: %v", err)
	}
	if res.DomainAction != "subscription_updated" {
		t.Errorf("DomainAction = %q, want subscription_updated", res.DomainAction)
	}
	if countAudit(t, db, "paddle_updated_missing_external_sub_id") != 1 {
		t.Error("expected audit row paddle_updated_missing_external_sub_id")
	}
}

// subscription.canceled without an external sub id: audit-only ack
// (defensive branch — parse hard-errors these upstream).
func TestOnWebhook_PaddleSubscriptionCancelled_MissingExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paddle", EventID: "evt-pd-canmiss-" + mustNewUUID()[:8], EventType: "subscription.canceled",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("missing external sub id must ack: %v", err)
	}
	if countAudit(t, db, "paddle_cancel_missing_external_sub_id") != 1 {
		t.Error("expected audit row paddle_cancel_missing_external_sub_id")
	}
}

// ============================================================================
// M3 review: stale subscription.updated deliveries must not regress
// plan_id / expires_at; expires_at is monotonic (GREATEST, same rationale
// as the renewal path's out-of-order guard).
// ============================================================================

// A delayed PRE-change event (old occurred_at, old items) arriving after a
// change-plan must not flip plan_id back to the old plan and must not
// shrink expires_at. The scheduled-cancel flip still applies (idempotent).
func TestOnWebhook_PaddleSubscriptionUpdated_StaleEvent_NoRegression(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_stale_" + mustNewUUID()[:8]
	// The row is ALREADY on yearly (change-plan happened), expiry far out.
	farExpiry := time.Now().Add(380 * 24 * time.Hour).UTC().Truncate(time.Second)
	subID := seedChannelSub(t, db, uid, "yearly", "active", extSubID, "paddle", true, farExpiry)
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_monthly_test", "yearly": "pri_yearly_test"})

	staleTime := time.Now().Add(-time.Hour).UTC() // predates the row's updated_at
	oldExpiry := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	e := paddleUpdatedEvent("evt-pd-stale-"+mustNewUUID()[:8], extSubID)
	e.OccurredAt = &staleTime
	e.PriceIDs = []string{"pri_monthly_test"} // the OLD plan's price
	e.SubExpiresAt = &oldExpiry               // smaller than the row's expiry
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("stale event must ack: %v", err)
	}
	planID, _, _, gotExpiry := readChannelState(t, db, subID)
	if planID != "yearly" {
		t.Errorf("plan_id regressed to %q by a stale event", planID)
	}
	if !gotExpiry.Equal(farExpiry) {
		t.Errorf("expires_at = %v, shrank from %v on a stale event", gotExpiry, farExpiry)
	}
}

// A fresh event (occurred_at after the row's updated_at) syncs as before.
func TestOnWebhook_PaddleSubscriptionUpdated_FreshEvent_Syncs(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_fresh_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_monthly_test", "yearly": "pri_yearly_test"})

	freshTime := time.Now().Add(time.Hour).UTC()
	periodEnd := time.Now().Add(380 * 24 * time.Hour).UTC().Truncate(time.Second)
	e := paddleUpdatedEvent("evt-pd-fresh-"+mustNewUUID()[:8], extSubID)
	e.OccurredAt = &freshTime
	e.PriceIDs = []string{"pri_yearly_test"}
	e.SubExpiresAt = &periodEnd
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("fresh event: %v", err)
	}
	planID, _, _, gotExpiry := readChannelState(t, db, subID)
	if planID != "yearly" {
		t.Errorf("plan_id = %q, want synced yearly", planID)
	}
	if !gotExpiry.Equal(periodEnd) {
		t.Errorf("expires_at = %v, want synced %v", gotExpiry, periodEnd)
	}
}

// Even on a FRESH plan-change sync, expires_at never shrinks: GREATEST
// keeps the larger value (mirror of the renewal out-of-order guard).
func TestOnWebhook_PaddleSubscriptionUpdated_ExpiryNeverShrinks(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_greatest_" + mustNewUUID()[:8]
	farExpiry := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, farExpiry)
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_monthly_test", "yearly": "pri_yearly_test"})

	freshTime := time.Now().Add(time.Hour).UTC()
	smallExpiry := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	e := paddleUpdatedEvent("evt-pd-grt-"+mustNewUUID()[:8], extSubID)
	e.OccurredAt = &freshTime
	e.PriceIDs = []string{"pri_yearly_test"}
	e.SubExpiresAt = &smallExpiry
	if _, err := svc.OnWebhook(context.Background(), e); err != nil {
		t.Fatalf("fresh event: %v", err)
	}
	planID, _, _, gotExpiry := readChannelState(t, db, subID)
	if planID != "yearly" {
		t.Errorf("plan_id = %q, want synced yearly", planID)
	}
	if !gotExpiry.Equal(farExpiry) {
		t.Errorf("expires_at = %v, want GREATEST (unchanged %v)", gotExpiry, farExpiry)
	}
}
