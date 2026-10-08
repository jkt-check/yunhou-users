package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/repo"
)

// seedChannelSub inserts a subscription row with the M1 channel/auto_renew
// columns populated. extID "" → NULL external_subscription_id; channel "" →
// NULL channel. Returns the row id.
func seedChannelSub(t *testing.T, db *sqlx.DB, uid, planID, status, extID, channel string, autoRenew bool, expiry time.Time) string {
	t.Helper()
	var ext any
	if extID != "" {
		ext = extID
	}
	var ch any
	if channel != "" {
		ch = channel
	}
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id, channel, auto_renew)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, subID, uid, planID, status, expiry, ext, ch, autoRenew); err != nil {
		t.Fatalf("seed channel subscription: %v", err)
	}
	return subID
}

func readChannelState(t *testing.T, db *sqlx.DB, subID string) (planID, status string, autoRenew bool, expiresAt time.Time) {
	t.Helper()
	if err := db.QueryRowContext(context.Background(),
		`SELECT plan_id, status, auto_renew, expires_at FROM subscriptions WHERE id = $1`, subID).
		Scan(&planID, &status, &autoRenew, &expiresAt); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	return planID, status, autoRenew, expiresAt
}

// ============================================================================
// CancelSubscriptionByID — POST /user/subscriptions/:id/cancel
// ============================================================================

func TestCancelSubscriptionByID_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_cancelid_" + mustNewUUID()[:8]
	expiry := time.Now().Add(15 * 24 * time.Hour).UTC().Truncate(time.Second)
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, expiry)

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)

	sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "next_billing_period")
	if err != nil {
		t.Fatalf("CancelSubscriptionByID: %v", err)
	}
	if stub.cancelCalls != 1 || stub.cancelGotID != extSubID {
		t.Fatalf("paddle cancel calls=%d id=%q, want 1/%s", stub.cancelCalls, stub.cancelGotID, extSubID)
	}
	if sub.AutoRenew {
		t.Error("returned sub AutoRenew = true, want false")
	}
	planID, status, autoRenew, gotExpiry := readChannelState(t, db, subID)
	if autoRenew {
		t.Error("auto_renew still true after cancel")
	}
	// Contract: status/expires_at are NOT touched — the flip to cancelled
	// arrives via the subscription.canceled webhook at period end.
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if planID != "monthly" {
		t.Errorf("plan_id = %q, want monthly", planID)
	}
	if !gotExpiry.Equal(expiry) {
		t.Errorf("expires_at = %v, want untouched %v", gotExpiry, expiry)
	}
}

func TestCancelSubscriptionByID_IdempotentWhenAutoRenewFalse(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_idem_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)

	sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
	if err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if stub.cancelCalls != 0 {
		t.Fatalf("paddle must NOT be called again when auto_renew is already false (calls=%d)", stub.cancelCalls)
	}
	if sub == nil || sub.ID != subID || sub.AutoRenew {
		t.Errorf("returned current state = %+v", sub)
	}
}

func TestCancelSubscriptionByID_NotFoundOrNotOwned(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	otherUID := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_own_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})

	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, mustNewUUID(), ""); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("unknown id: expected ErrSubscriptionNotFound, got %v", err)
	}
	if _, err := svc.CancelSubscriptionByID(context.Background(), otherUID, subID, ""); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("other user's id: expected ErrSubscriptionNotFound (non-enumeration), got %v", err)
	}
}

func TestCancelSubscriptionByID_NonChannelSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)

	// Separate users per sub: the partial unique index allows only one
	// active kaya-membership sub per user.
	freeUID := seedUser(t, db)
	freeSub := seedChannelSub(t, db, freeUID, "free", "active", "", "", false, time.Now().Add(24*time.Hour))
	if _, err := svc.CancelSubscriptionByID(context.Background(), freeUID, freeSub, ""); !errors.Is(err, ErrSubscriptionNoAutoRenew) {
		t.Fatalf("free sub: expected ErrSubscriptionNoAutoRenew, got %v", err)
	}

	wechatUID := seedUser(t, db)
	activeWechat := seedChannelSub(t, db, wechatUID, "monthly", "active", "", "wechat_pay", false, time.Now().Add(24*time.Hour))
	if _, err := svc.CancelSubscriptionByID(context.Background(), wechatUID, activeWechat, ""); !errors.Is(err, ErrSubscriptionNoAutoRenew) {
		t.Fatalf("wechat sub: expected ErrSubscriptionNoAutoRenew, got %v", err)
	}
	if stub.cancelCalls != 0 {
		t.Fatal("paddle must not be called for non-channel subs")
	}
}

func TestCancelSubscriptionByID_AlreadyEnded(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	svc.SetPaddleClient(&stubPaddle{})

	expired := seedChannelSub(t, db, uid, "monthly", "expired", "sub_exp_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(-time.Hour))
	cancelled := seedChannelSub(t, db, uid, "monthly", "cancelled", "sub_can_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(time.Hour))

	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, expired, ""); !errors.Is(err, ErrSubscriptionAlreadyEnded) {
		t.Fatalf("expired: expected ErrSubscriptionAlreadyEnded, got %v", err)
	}
	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, cancelled, ""); !errors.Is(err, ErrSubscriptionAlreadyEnded) {
		t.Fatalf("cancelled: expected ErrSubscriptionAlreadyEnded, got %v", err)
	}
}

func TestCancelSubscriptionByID_PaypalNotFulfillable(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "I-PAYPAL-"+mustNewUUID()[:8], "paypal", true, time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, ""); !errors.Is(err, ErrSubscriptionNotChannelManaged) {
		t.Fatalf("paypal sub: expected ErrSubscriptionNotChannelManaged, got %v", err)
	}
	if stub.cancelCalls != 0 {
		t.Fatal("paddle must not be called for a PayPal-managed sub")
	}
}

func TestCancelSubscriptionByID_PaddleError_LocalUnchanged(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_err_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	svc.SetPaddleClient(&stubPaddle{cancelErr: errors.New("paddle 503")})
	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, ""); !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("expected ErrChannelUnavailable, got %v", err)
	}
	if _, _, autoRenew, _ := readChannelState(t, db, subID); !autoRenew {
		t.Error("auto_renew flipped despite paddle failure — local state must be unchanged")
	}
}

// Contract 2.1 step 5: the legacy /payments/subscription/cancel route keeps
// working AND now also flips auto_renew=false locally on success.
func TestCancelChannelSubscription_SetsAutoRenewFalse(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_legacy_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	svc.SetPaddleClient(&stubPaddle{})
	if _, err := svc.CancelChannelSubscription(context.Background(), uid); err != nil {
		t.Fatalf("CancelChannelSubscription: %v", err)
	}
	if _, _, autoRenew, _ := readChannelState(t, db, subID); autoRenew {
		t.Error("auto_renew still true after legacy-route cancel")
	}
}

// ============================================================================
// ChangePlanByID — POST /user/subscriptions/:id/change-plan
// ============================================================================

func TestChangePlanByID_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	extSubID := "sub_cp_" + mustNewUUID()[:8]
	subID := seedChannelSub(t, db, uid, "monthly", "active", extSubID, "paddle", true, time.Now().Add(15*24*time.Hour))

	next := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	stub := &stubPaddle{updateNext: &next}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_yearly_test"})

	sub, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly")
	if err != nil {
		t.Fatalf("ChangePlanByID: %v", err)
	}
	if stub.updateCalls != 1 || stub.updateGotID != extSubID || stub.updateGotPrice != "pri_yearly_test" {
		t.Fatalf("paddle update calls=%d id=%q price=%q", stub.updateCalls, stub.updateGotID, stub.updateGotPrice)
	}
	if sub.PlanID != "yearly" {
		t.Errorf("returned sub PlanID = %q, want yearly", sub.PlanID)
	}
	planID, _, autoRenew, expAt := readChannelState(t, db, subID)
	if planID != "yearly" {
		t.Errorf("plan_id = %q, want yearly", planID)
	}
	if !expAt.Equal(next) {
		t.Errorf("expires_at = %v, want channel period end %v", expAt, next)
	}
	if !autoRenew {
		t.Error("auto_renew must be preserved through a plan change")
	}
}

func TestChangePlanByID_SamePlan(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_same_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_m"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "monthly"); !errors.Is(err, ErrSamePlanChange) {
		t.Fatalf("expected ErrSamePlanChange, got %v", err)
	}
}

func TestChangePlanByID_PlanGuards(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_pg_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active)
		VALUES ('yearly-inactive', 'Yearly Inactive', 149.9, 365, '{}', false)
	`); err != nil {
		t.Fatalf("seed inactive plan: %v", err)
	}
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y", "yearly-inactive": "pri_yi"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "no-such-plan"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown plan: expected ErrPlanNotFound, got %v", err)
	}
	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly-inactive"); !errors.Is(err, ErrPlanInactive) {
		t.Fatalf("inactive plan: expected ErrPlanInactive, got %v", err)
	}
}

func TestChangePlanByID_DowngradeRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "yearly", "active", "sub_down_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(200*24*time.Hour))
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_m"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "monthly"); !errors.Is(err, ErrPlanDowngradeNotSupported) {
		t.Fatalf("expected ErrPlanDowngradeNotSupported, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a downgrade attempt")
	}
}

// M1: cross-product target plan must be rejected BEFORE any Paddle call —
// same gate as UpgradeChannelSubscription. Without it Paddle charges the
// proration and only then the subscriptions_enforce_plan_product trigger
// aborts the local write → handler 500 + money divergence.
func TestChangePlanByID_CrossProductRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_xp_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active, product_code)
		VALUES ('yearly-xproduct', 'Yearly XProduct', 149.9, 365, '{}', true, 'coding-plan')
	`); err != nil {
		t.Fatalf("seed cross-product plan: %v", err)
	}
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly-xproduct": "pri_yx"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly-xproduct"); !errors.Is(err, ErrPlanChangeNotUpgrade) {
		t.Fatalf("expected ErrPlanChangeNotUpgrade, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a cross-product plan change")
	}
}

// M1: an active-but-retired plan must not acquire a new billing
// relationship through change-plan either — same retirement gate as
// UpgradeChannelSubscription and CreateOrder.
func TestChangePlanByID_RetiredPlanRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_ret_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active, accepting_new_subscriptions)
		VALUES ('yearly-retired', 'Yearly Retired', 149.9, 365, '{}', true, false)
	`); err != nil {
		t.Fatalf("seed retired plan: %v", err)
	}
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly-retired": "pri_yr"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly-retired"); !errors.Is(err, ErrPlanNotAcceptingNew) {
		t.Fatalf("expected ErrPlanNotAcceptingNew, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a retired target plan")
	}
}

// Minor 4 (review): a cancelled subscription must report "already ended"
// even when the target plan equals the current one — the ended check runs
// before the same-plan check.
func TestChangePlanByID_CancelledSubSamePlan_ReturnsAlreadyEnded(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "cancelled", "sub_cesp_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(time.Hour))
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"monthly": "pri_m"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "monthly"); !errors.Is(err, ErrSubscriptionAlreadyEnded) {
		t.Fatalf("expected ErrSubscriptionAlreadyEnded, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for an ended subscription")
	}
}

func TestChangePlanByID_ExpiredSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "expired", "sub_cpexp_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(-time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrSubscriptionAlreadyEnded) {
		t.Fatalf("expected ErrSubscriptionAlreadyEnded, got %v", err)
	}
}

func TestChangePlanByID_NonChannelSub(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "", "wechat_pay", false, time.Now().Add(15*24*time.Hour))
	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrSubscriptionNoAutoRenew) {
		t.Fatalf("expected ErrSubscriptionNoAutoRenew, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a non-channel sub")
	}
}

func TestChangePlanByID_PaypalNotFulfillable(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "I-PAYPAL-"+mustNewUUID()[:8], "paypal", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrSubscriptionNotChannelManaged) {
		t.Fatalf("expected ErrSubscriptionNotChannelManaged, got %v", err)
	}
}

func TestChangePlanByID_MissingPriceMapping(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_nopri_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	// no SetPaddlePrices — operator forgot the yearly entry
	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrPaddlePriceNotConfigured) {
		t.Fatalf("expected ErrPaddlePriceNotConfigured, got %v", err)
	}
}

func TestChangePlanByID_PaddleError_LocalUnchanged(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_cperr_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	svc.SetPaddleClient(&stubPaddle{updateErr: errors.New("paddle 503")})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("expected ErrChannelUnavailable, got %v", err)
	}
	if planID, _, _, _ := readChannelState(t, db, subID); planID != "monthly" {
		t.Errorf("plan_id = %q after paddle failure, want monthly (unchanged)", planID)
	}
}

func TestChangePlanByID_NotFoundOrNotOwned(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	otherUID := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_cpo_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddleClient(&stubPaddle{})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, mustNewUUID(), "yearly"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("unknown id: expected ErrSubscriptionNotFound, got %v", err)
	}
	if _, err := svc.ChangePlanByID(context.Background(), otherUID, subID, "yearly"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("other user's id: expected ErrSubscriptionNotFound, got %v", err)
	}
}

// ============================================================================
// Coverage: unwired-paddle branches, nil next_billed fallback, tx-path guard
// ============================================================================

func TestCancelSubscriptionByID_NoPaddleClient(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_nopc_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	if _, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, ""); !errors.Is(err, ErrPaddleNotConfigured) {
		t.Fatalf("expected ErrPaddleNotConfigured, got %v", err)
	}
}

func TestChangePlanByID_NoPaddleClient(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_nocp_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})

	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrPaddleNotConfigured) {
		t.Fatalf("expected ErrPaddleNotConfigured, got %v", err)
	}
}

// When Paddle's update response carries no next_billed_at, expires_at falls
// back to now()+plan.interval_days (yearly ≈ 365d).
func TestChangePlanByID_NilNextBilled_FallsBackToPlanInterval(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_fb_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	svc.SetPaddleClient(&stubPaddle{}) // updateNext nil
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); err != nil {
		t.Fatalf("ChangePlanByID: %v", err)
	}
	_, _, _, expAt := readChannelState(t, db, subID)
	delta := time.Until(expAt)
	if delta < 364*24*time.Hour || delta > 365*24*time.Hour+time.Minute {
		t.Errorf("expires_at delta = %v, want ≈365d (plan-interval fallback)", delta)
	}
}

// The transactional Cancel path (benefit-sync wired) applies the same
// auto_renew guard as the legacy path.
func TestSubscriptionService_Cancel_AutoRenewGuard_TxPath(t *testing.T) {
	db := setupPaymentDB(t)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_txg_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	subSvc := NewSubscriptionService(repo.NewSubscriptionRepo(db), NewPlanService(repo.NewPlanRepo(db), repo.NewAppRepo(db), repo.NewPlanChangeLogRepo(db)))
	subSvc.SetBenefitSync(db, inferencepostgres.NewStore(db))
	err := subSvc.Cancel(context.Background(), subID, uid)
	if !errors.Is(err, ErrSubscriptionAutoRenewActive) {
		t.Fatalf("expected ErrSubscriptionAutoRenewActive, got %v", err)
	}
	if _, status, _, _ := readChannelState(t, db, subID); status != "active" {
		t.Errorf("status = %q despite the guard, want active", status)
	}
}
