package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/repo"
)

// dbURL returns the test database URL. Override with DATABASE_URL.
func dbURL2() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

// setupPaymentDB connects to the test DB, wipes payment tables, seeds plans
// + a super app + an active user. Returns the *sqlx.DB.
func setupPaymentDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", dbURL2())
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })

	tables := []string{
		"refunds", "payments", "webhook_events", "orders",
		"sessions", "subscriptions", "social_identities",
		"plans", "apps", "users", "audit_log",
	}
	for _, tbl := range tables {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatalf("wipe %s: %v", tbl, err)
		}
	}
	// Plans: free, monthly and yearly (yearly backs the repurchase-rule
	// tests — upgrade allowed, downgrade rejected).
	for _, p := range []struct {
		id, name string
		price    float64
		days     int
		apps     []string
	}{
		{"free", "Free", 0, 0, []string{"yundian"}},
		{"monthly", "Monthly", 19.9, 30, []string{"yundian", "yundash"}},
		{"yearly", "Yearly", 199.9, 365, []string{"yundian", "yundash"}},
		{"trial", "Free Trial", 0, 0, []string{"yundian", "yundash"}},
	} {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO plans (id, name, price, interval_days, apps)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO NOTHING
		`, p.id, p.name, p.price, p.days, p.apps)
		if err != nil {
			t.Fatalf("seed plan %s: %v", p.id, err)
		}
	}
	// trial mirrors migration 018: grantable by auth, never purchasable,
	// never listed. The rollover tests only need the row to exist with
	// interval_days=0; the 409 test needs accepting_new_subscriptions=false.
	if _, err := db.ExecContext(context.Background(), `
		UPDATE plans SET accepting_new_subscriptions = false, is_listed = false, trial_days = 7
		WHERE id = 'trial'
	`); err != nil {
		t.Fatalf("seed trial plan flags: %v", err)
	}
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active) VALUES ('yundian', 'Yundian', true)
		ON CONFLICT (app_id) DO NOTHING
	`)
	return db
}

func newTestPaymentService(t *testing.T, db *sqlx.DB) *PaymentService {
	t.Helper()
	return newTestPaymentServiceWith(t, db, nil)
}

func newTestPaymentServiceWith(t *testing.T, db *sqlx.DB, client wechatClient) *PaymentService {
	t.Helper()
	return NewPaymentService(
		db,
		repo.NewOrderRepo(db),
		repo.NewPaymentRepo(db),
		repo.NewRefundRepo(db),
		repo.NewSubscriptionRepo(db),
		repo.NewPlanRepo(db),
		repo.NewUserRepo(db),
		repo.NewWebhookEventRepo(db),
		repo.NewAuditLogRepo(db),
		&stubRefundAPI{}, client,
		30*time.Minute)
}

func mustNewUUID() string { return uuid.New().String() }

func seedUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	id := mustNewUUID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, status) VALUES ($1, 'active')`, id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedPaidOrder(t *testing.T, db *sqlx.DB, userID, planID string, amount float64) string {
	t.Helper()
	id := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, $3, $4, 'CNY', 'pending', now() + INTERVAL '30 minutes', NULL)
	`, id, userID, planID, amount); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return id
}

// ============================================================================
// CreateOrder
// ============================================================================

func TestPaymentService_CreateOrder_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.PlanID != "monthly" || order.Amount != 19.9 || order.Currency != "CNY" {
		t.Errorf("order = %+v", order)
	}
	if order.Status != "pending" {
		t.Errorf("Status = %q, want pending", order.Status)
	}
}

func TestPaymentService_CreateOrder_PlanNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	_, err := svc.CreateOrder(context.Background(), uid, "missing", "stripe")
	if !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("err = %v, want ErrPlanNotFound", err)
	}
}

func TestPaymentService_CreateOrder_PlanInactive(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	_, _ = db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active) VALUES ('inactive', 'X', 0, 0, '{}', false)`)
	_, err := svc.CreateOrder(context.Background(), uid, "inactive", "stripe")
	if !errors.Is(err, ErrPlanInactive) {
		t.Errorf("err = %v, want ErrPlanInactive", err)
	}
}

// TestPaymentService_CreateOrder_SamePlanRenewalAllowed covers the
// 2026-07-28 repurchase rule: an active, unexpired subscription no
// longer blanket-rejects new orders. A same-plan order is a renewal —
// activation rolls the remaining days over (covered by the
// Confirm_Rollover tests) — so it is allowed through.
func TestPaymentService_CreateOrder_SamePlanRenewalAllowed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	// First order + confirm activates subscription.
	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("first order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sub",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// Second CreateOrder for the SAME plan — renewal, allowed.
	if _, err = svc.CreateOrder(context.Background(), uid, "monthly", "stripe"); err != nil {
		t.Errorf("same-plan renewal must be allowed (2026-07-28 repurchase rule); got %v", err)
	}
}

// TestPaymentService_CreateOrder_UpgradeAllowed: monthly active →
// yearly order is the supported upgrade path.
func TestPaymentService_CreateOrder_UpgradeAllowed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("first order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sub",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	up, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("monthly→yearly upgrade must be allowed; got %v", err)
	}
	if up.PlanID != "yearly" || up.Status != "pending" {
		t.Errorf("order = %+v, want pending yearly", up)
	}
}

// TestPaymentService_CreateOrder_DowngradeRejected: yearly active →
// monthly order is a downgrade and stays rejected (409 mapping in the
// handler).
func TestPaymentService_CreateOrder_DowngradeRejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("first order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sub",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	_, err = svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if !errors.Is(err, ErrPlanDowngrade) {
		t.Errorf("err = %v, want ErrPlanDowngrade", err)
	}
}

// TestCreateOrder_TrialPlanNotPurchasable: the trial plan is granted by
// auth on first login and must never be orderable — even for a user
// with no subscription at all. eligibilityAndInsertOrderTx rejects it
// with ErrPlanNotAcceptingNew (409 mapping in the handler). Pins the
// accepting_new_subscriptions=false flag seeded above (migration 018).
func TestCreateOrder_TrialPlanNotPurchasable(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	_, err := svc.CreateOrder(context.Background(), uid, "trial", "stripe")
	if !errors.Is(err, ErrPlanNotAcceptingNew) {
		t.Fatalf("expected ErrPlanNotAcceptingNew, got %v", err)
	}
}

// ============================================================================
// Rollover + downgrade guard at activation (2026-07-28 repurchase rule)
// ============================================================================

// seedActiveSub inserts an active subscription expiring at `expiry` and
// returns nothing — the row is the fixture, read back via SQL in the
// assertions.
func seedActiveSub(t *testing.T, db *sqlx.DB, uid, planID string, expiry time.Time) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at)
		VALUES (gen_random_uuid(), $1, $2, 'active', now(), $3)
	`, uid, planID, expiry); err != nil {
		t.Fatalf("seed active sub: %v", err)
	}
}

func readSub(t *testing.T, db *sqlx.DB, uid string) (planID string, expiresAt time.Time) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT plan_id, expires_at FROM subscriptions WHERE user_id = $1 AND status = 'active'
	`, uid).Scan(&planID, &expiresAt)
	if err != nil {
		t.Fatalf("read sub: %v", err)
	}
	return planID, expiresAt
}

// withinSeconds fails unless got is within ±tol of want. Activation
// timestamps are computed from time.Now() deep in the service, so exact
// equality is impossible; a few seconds of execution slack is fine.
func withinSeconds(t *testing.T, got, want time.Time, tol time.Duration) {
	t.Helper()
	diff := got.Sub(want)
	if diff < -tol || diff > tol {
		t.Errorf("expires_at = %v, want %v ±%v (off by %v)", got, want, tol, diff)
	}
}

// TestConfirm_RolloverSamePlanRenewal: renewing monthly while monthly is
// still active extends from the OLD expiry (+30d from old), not from
// now() — the user never loses paid days by renewing early.
func TestConfirm_RolloverSamePlanRenewal(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	oldExpiry := time.Now().Add(10 * 24 * time.Hour)
	seedActiveSub(t, db, uid, "monthly", oldExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("renewal order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-renew-1",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	planID, expiresAt := readSub(t, db, uid)
	if planID != "monthly" {
		t.Errorf("plan = %q, want monthly", planID)
	}
	withinSeconds(t, expiresAt, oldExpiry.Add(30*24*time.Hour), 30*time.Second)
}

// TestConfirm_RolloverUpgrade: monthly with 10 days left upgrades to
// yearly — the remaining days carry over: expiry ≈ old + 365d.
func TestConfirm_RolloverUpgrade(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	oldExpiry := time.Now().Add(10 * 24 * time.Hour)
	seedActiveSub(t, db, uid, "monthly", oldExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-1",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	planID, expiresAt := readSub(t, db, uid)
	if planID != "yearly" {
		t.Errorf("plan = %q, want yearly", planID)
	}
	withinSeconds(t, expiresAt, oldExpiry.Add(365*24*time.Hour), 30*time.Second)
}

// TestConfirm_TrialRolloverOnFirstPurchase: a user on the granted 7-day
// trial (interval_days=0) buys their first paid plan while the trial is
// still active — the remaining trial days roll over: expiry ≈ trial
// expiry + 30d, not now() + 30d. Characterization test for the existing
// resolveSubExpiry rollover branch; the trial row behaves like any
// other unexpired active subscription here.
func TestConfirm_TrialRolloverOnFirstPurchase(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	trialExpiry := time.Now().Add(72 * time.Hour)
	seedActiveSub(t, db, uid, "trial", trialExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("first purchase order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-trial-first-1",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	planID, expiresAt := readSub(t, db, uid)
	if planID != "monthly" {
		t.Errorf("plan = %q, want monthly", planID)
	}
	withinSeconds(t, expiresAt, trialExpiry.Add(30*24*time.Hour), 2*time.Minute)
}

// The trial grant's concurrency safety net (auth.go grantTrialSubscription
// doc): a duplicate grant hits idx_subscriptions_user_active and is
// logged+swallowed. Pin the index itself at the DB layer.
func TestSubscriptions_UniqueActivePerUser(t *testing.T) {
	db := setupPaymentDB(t)
	uid := seedUser(t, db)
	seedActiveSub(t, db, uid, "trial", time.Now().Add(7*24*time.Hour))
	// second active row for the same user must be rejected
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at)
		VALUES (gen_random_uuid(), $1, 'monthly', 'active', now(), now() + interval '30 days')
	`, uid)
	if err == nil {
		t.Fatal("expected unique violation on second active subscription, got nil")
	}
	if !strings.Contains(err.Error(), "idx_subscriptions_user_active") {
		t.Fatalf("expected idx_subscriptions_user_active violation, got %v", err)
	}
}

// TestConfirm_RetryNoDoubleRollover: a Confirm retry of the SAME payment
// must not extend the sub a second time — preservedExpiry wins and the
// stored (already rolled-over) value is returned verbatim.
func TestConfirm_RetryNoDoubleRollover(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	oldExpiry := time.Now().Add(10 * 24 * time.Hour)
	seedActiveSub(t, db, uid, "monthly", oldExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	in := ConfirmInput{OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-retry"}
	if _, err := svc.Confirm(context.Background(), in); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	_, firstExpiry := readSub(t, db, uid)

	if _, err := svc.Confirm(context.Background(), in); err != nil {
		t.Fatalf("retry confirm: %v", err)
	}
	_, secondExpiry := readSub(t, db, uid)
	withinSeconds(t, secondExpiry, firstExpiry, time.Second)
}

// TestConfirm_DowngradeActivationBlocked: a stale monthly order created
// BEFORE the user upgraded is paid afterwards — the payment is honored
// (order goes paid) but the yearly subscription is left untouched and
// the block is audit-logged for a manual refund.
func TestConfirm_DowngradeActivationBlocked(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	// Stale monthly QR minted while the user had no subscription.
	staleOrder, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("stale order: %v", err)
	}
	// User upgrades to yearly.
	upOrder, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: upOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-first",
	}); err != nil {
		t.Fatalf("upgrade confirm: %v", err)
	}
	_, yearlyExpiry := readSub(t, db, uid)

	// ...then the stale monthly QR gets paid. Must NOT clobber yearly.
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: staleOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-stale-late",
	}); err != nil {
		t.Fatalf("stale confirm: %v", err)
	}

	planID, expiresAt := readSub(t, db, uid)
	if planID != "yearly" {
		t.Errorf("plan = %q, want yearly (downgrade must be blocked)", planID)
	}
	withinSeconds(t, expiresAt, yearlyExpiry, time.Second)

	var orderStatus string
	if err := db.QueryRowContext(context.Background(),
		`SELECT status FROM orders WHERE id = $1`, staleOrder.ID).Scan(&orderStatus); err != nil {
		t.Fatalf("read stale order: %v", err)
	}
	if orderStatus != "paid" {
		t.Errorf("stale order status = %q, want paid (payment honored, manual refund)", orderStatus)
	}

	var auditCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = 'downgrade_activation_blocked' AND target = $1`,
		"order:"+staleOrder.ID).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("downgrade_activation_blocked audit rows = %d, want 1", auditCount)
	}
}

// TestConfirm_DowngradeActivationBlocked_RetryStaysBlocked is the B1
// regression guard: a dedupe RETRY of a downgrade-blocked payment must
// not activate either. preservedExpiry short-circuits resolveSubExpiry
// before the downgrade comparison, so without the plan-mismatch check
// in the dedupe branch this second Confirm would overwrite the yearly
// sub's plan_id with monthly — silently undoing the first block.
func TestConfirm_DowngradeActivationBlocked_RetryStaysBlocked(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	staleOrder, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("stale order: %v", err)
	}
	upOrder, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: upOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-first",
	}); err != nil {
		t.Fatalf("upgrade confirm: %v", err)
	}

	staleIn := ConfirmInput{OrderID: staleOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-stale-late"}
	if _, err := svc.Confirm(context.Background(), staleIn); err != nil {
		t.Fatalf("first stale confirm: %v", err)
	}
	// The retry — same payment, dedupe hit. Must stay blocked.
	if _, err := svc.Confirm(context.Background(), staleIn); err != nil {
		t.Fatalf("retry stale confirm: %v", err)
	}

	planID, _ := readSub(t, db, uid)
	if planID != "yearly" {
		t.Errorf("plan = %q after blocked retry, want yearly (B1: retry undid the downgrade block)", planID)
	}

	var retryAudits int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = 'downgrade_activation_blocked' AND target = $1`,
		"order:"+staleOrder.ID).Scan(&retryAudits); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if retryAudits < 2 {
		t.Errorf("downgrade_activation_blocked audit rows = %d, want >= 2 (first delivery + retry)", retryAudits)
	}
}

// TestOnWebhook_DowngradeActivationBlocked mirrors the Confirm-path
// guard onto the webhook delivery channel: a stale monthly order paid
// after the upgrade must not clobber the yearly sub.
func TestOnWebhook_DowngradeActivationBlocked(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	staleOrder, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("stale order: %v", err)
	}
	upOrder, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: upOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-first",
	}); err != nil {
		t.Fatalf("upgrade confirm: %v", err)
	}
	_, yearlyExpiry := readSub(t, db, uid)

	// The stale order's payment arrives via WEBHOOK (not Confirm).
	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-stale-1", EventType: "payment_intent.succeeded",
		TransactionID: "pi-stale-wh", OrderID: staleOrder.ID,
		Amount: staleOrder.Amount, Currency: staleOrder.Currency,
		RawPayload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("webhook: %v", err)
	}

	planID, expiresAt := readSub(t, db, uid)
	if planID != "yearly" {
		t.Errorf("plan = %q, want yearly (webhook-path downgrade must be blocked)", planID)
	}
	withinSeconds(t, expiresAt, yearlyExpiry, time.Second)

	var auditCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = 'downgrade_activation_blocked' AND target = $1`,
		"order:"+staleOrder.ID).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("downgrade_activation_blocked audit rows = %d, want 1", auditCount)
	}
}

// TestConfirm_RolloverBeatsSmallerHint: when a hint is present but the
// rolled value (old expiry + interval) is later, the rolled value
// wins — the user never loses days to a stale quote.
func TestConfirm_RolloverBeatsSmallerHint(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	oldExpiry := time.Now().Add(10 * 24 * time.Hour)
	seedActiveSub(t, db, uid, "monthly", oldExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	// A hint SHORTER than old+365d (e.g. a quote computed before the
	// user dawdled) must not shrink the rolled expiry.
	hint := time.Now().Add(100 * 24 * time.Hour)
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-hint",
		ExpiresAt: &hint,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	_, expiresAt := readSub(t, db, uid)
	withinSeconds(t, expiresAt, oldExpiry.Add(365*24*time.Hour), 30*time.Second)
}

// TestConfirm_LifetimeOrderBlockedAfterUpgrade (M1): an interval_days=0
// ("lifetime") order minted before the user upgraded must not
// overwrite the finite-cycle subscription when it's paid afterwards.
func TestConfirm_LifetimeOrderBlockedAfterUpgrade(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps)
		VALUES ('lifetime', 'Lifetime', 999, 0, ARRAY['yundian'])
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed lifetime plan: %v", err)
	}

	staleOrder, err := svc.CreateOrder(context.Background(), uid, "lifetime", "stripe")
	if err != nil {
		t.Fatalf("stale lifetime order: %v", err)
	}
	upOrder, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: upOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-up-first",
	}); err != nil {
		t.Fatalf("upgrade confirm: %v", err)
	}

	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: staleOrder.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-lifetime-late",
	}); err != nil {
		t.Fatalf("stale confirm: %v", err)
	}

	planID, _ := readSub(t, db, uid)
	if planID != "yearly" {
		t.Errorf("plan = %q, want yearly (interval=0 stale order must be blocked)", planID)
	}
}

// TestConfirm_PlanMissing_ExistingSubUntouched: with the RESTRICT FKs on
// orders/subscriptions.plan_id, "plan deleted mid-payment" is only
// reachable via manual DB surgery (simulated below by bypassing FK
// enforcement). Confirm must not fail and must NOT touch the existing
// active subscription — no activation can reference the missing plan —
// and the audit trail must record the event.
func TestConfirm_PlanMissing_ExistingSubUntouched(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	seededExpiry := time.Now().Add(10 * 24 * time.Hour)
	seedActiveSub(t, db, uid, "monthly", seededExpiry)

	order, err := svc.CreateOrder(context.Background(), uid, "yearly", "stripe")
	if err != nil {
		t.Fatalf("upgrade order: %v", err)
	}
	// Delete the plan with FK enforcement bypassed — orders_plan_id_fkey
	// is ON DELETE RESTRICT and would refuse this in a consistent DB.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = 'replica'`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM plans WHERE id = 'yearly'`); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = 'origin'`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}

	hint := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-plan-missing",
		ExpiresAt: &hint,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// The existing active subscription must be untouched: still monthly,
	// still the seeded expiry — the missing-plan path skips activation.
	planID, expiresAt := readSub(t, db, uid)
	if planID != "monthly" {
		t.Errorf("active sub plan = %q, want monthly (untouched)", planID)
	}
	withinSeconds(t, expiresAt, seededExpiry, time.Second)

	// The audit trail records the plan-missing event.
	var auditCount int
	if err := db.Get(&auditCount,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'subscription_expiry_plan_missing' AND target = $1`,
		"plan:yearly"); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount == 0 {
		t.Error("expected a subscription_expiry_plan_missing audit row")
	}
}

// TestPaymentService_CreateOrder_StaleActiveSubAllowsRenewal exercises
// the cn-staging 2026-07-23 follow-up fix: a row with status='active'
// but expires_at in the past (e.g. from the pre-fix reconcile bug that
// aliased SuccessTime into SubExpiresAt) is NOT a "currently active"
// subscription for the purposes of the partial unique index guard.
// CreateOrder proceeds; the new checkout's ActivateOnTx UPDATEs the
// existing row in place, leaving exactly one status='active' row per
// user. Without this carve-out, users whose subscription quietly went
// past couldn't renew even after the login-decouple fix let them in.
func TestPaymentService_CreateOrder_StaleActiveSubAllowsRenewal(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	// Pre-seed a stale active sub: status='active' but expires_at past.
	past := time.Now().Add(-1 * time.Hour)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at)
		VALUES (gen_random_uuid(), $1, 'monthly', 'active', now(), $2)
	`, uid, past)
	if err != nil {
		t.Fatalf("seed stale sub: %v", err)
	}
	// CreateOrder must NOT return ErrUserHasActiveSub; the user is
	// permitted to renew. After payment, activateSubscriptionOnTx
	// transitions this same row to expires_at = NOW() + interval.
	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder must NOT reject a stale-active sub (cn-staging 2026-07-23 fix); got %v", err)
	}
	if order == nil || order.Status != "pending" {
		t.Errorf("order = %+v, want a fresh pending order", order)
	}
}

// ============================================================================
// CancelOrder
// ============================================================================

func TestPaymentService_CancelOrder_OwnerCancelsPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := svc.CancelOrder(context.Background(), order.ID, uid); err != nil {
		t.Errorf("CancelOrder: %v", err)
	}
}

func TestPaymentService_CancelOrder_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	err := svc.CancelOrder(context.Background(), mustNewUUID(), mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestPaymentService_CancelOrder_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	err := svc.CancelOrder(context.Background(), order.ID, mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound (hidden for non-owner)", err)
	}
}

func TestPaymentService_CancelOrder_NotPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	// Flip to paid directly.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'paid' WHERE id = $1`, order.ID)

	err := svc.CancelOrder(context.Background(), order.ID, uid)
	if !errors.Is(err, ErrOrderNotPending) {
		t.Errorf("err = %v, want ErrOrderNotPending", err)
	}
}

// ============================================================================
// GetOrder
// ============================================================================

func TestPaymentService_GetOrder_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	got, err := svc.GetOrder(context.Background(), order.ID, uid)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.ID != order.ID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetOrder_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	_, err := svc.GetOrder(context.Background(), order.ID, mustNewUUID())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestPaymentService_GetOrder_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetOrder(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v", err)
	}
}

// TestPaymentService_GetOrder_ReconcilesPaidWeChatOrder pins the active
// reconcile path: a pending wechat_pay order whose underlying WeChat
// state has flipped to SUCCESS is paid + subscription-activated just
// because the FE polled GetOrder (the webhook was lost / never arrived
// / failed verify before the platform-cert fix). Without reconcile,
// the user would see "still pending" forever.
func TestPaymentService_GetOrder_ReconcilesPaidWeChatOrder(t *testing.T) {
	db := setupPaymentDB(t)

	queryCalled := false
	stub := &stubWechat{
		mockMode: false, // non-mock so shouldReconcile engages
		mchID:    "1900000109",
		appID:    "wx_test_app",
		unifiedFn: func(_ context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
			return &wechat.UnifiedOrderResponse{
				OutTradeNo: req.OutTradeNo,
				CodeURL:    "weixin://wxpay/bizpayurl?pr=" + req.OutTradeNo,
			}, nil
		},
		queryFn: func(_ context.Context, outTradeNo string) (*wechat.OrderQueryResult, error) {
			queryCalled = true
			return &wechat.OrderQueryResult{
				OutTradeNo:    outTradeNo,
				TransactionID: "wx_txn_001",
				TradeState:    "SUCCESS",
				SuccessTime:   time.Now().UTC().Format(time.RFC3339),
				Amount: struct {
					Total    int64  `json:"total"`
					Currency string `json:"currency"`
				}{Total: 2900, Currency: "CNY"},
			}, nil
		},
	}
	svc := newTestPaymentServiceWith(t, db, stub)

	uid := seedUser(t, db)
	order, err := svc.CreateOrder(context.Background(), uid, "monthly", "wechat_pay")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// Force last_reconciled_at into the past so shouldReconcile engages
	// on the very next GetOrder (CreateOrder stamps it to now() so the
	// first poll after CreateOrder doesn't hit WeChat).
	if _, err := db.ExecContext(context.Background(),
		`UPDATE orders SET last_reconciled_at = now() - interval '1 minute' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("age last_reconciled_at: %v", err)
	}

	got, err := svc.GetOrder(context.Background(), order.ID, uid)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if !queryCalled {
		t.Fatalf("expected wechat.QueryOrder to be called during GetOrder")
	}
	if got.Status != "paid" {
		t.Errorf("after reconcile, order.Status = %q, want paid", got.Status)
	}
}

// TestPaymentService_GetOrder_ReconcileThrottled pins the rate-limit:
// a second GetOrder within reconcileMinInterval must NOT trigger a
// second upstream QueryOrder. The first one stamps last_reconciled_at
// = now; the second one sees < reconcileMinInterval since then and
// short-circuits.
func TestPaymentService_GetOrder_ReconcileThrottled(t *testing.T) {
	db := setupPaymentDB(t)
	var queryCalls int
	stub := &stubWechat{
		mockMode: false,
		appID:    "wx_test_app",
		unifiedFn: func(_ context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
			return &wechat.UnifiedOrderResponse{OutTradeNo: req.OutTradeNo, CodeURL: "weixin://x"}, nil
		},
		queryFn: func(_ context.Context, outTradeNo string) (*wechat.OrderQueryResult, error) {
			queryCalls++
			return &wechat.OrderQueryResult{OutTradeNo: outTradeNo, TradeState: "NOTPAY"}, nil
		},
	}
	svc := newTestPaymentServiceWith(t, db, stub)

	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "wechat_pay")
	if _, err := db.ExecContext(context.Background(),
		`UPDATE orders SET last_reconciled_at = now() - interval '1 minute' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("age last_reconciled_at: %v", err)
	}
	if _, err := svc.GetOrder(context.Background(), order.ID, uid); err != nil {
		t.Fatalf("first GetOrder: %v", err)
	}
	if _, err := svc.GetOrder(context.Background(), order.ID, uid); err != nil {
		t.Fatalf("second GetOrder: %v", err)
	}
	if queryCalls != 1 {
		t.Errorf("expected 1 QueryOrder (second poll throttled), got %d", queryCalls)
	}
}

// ============================================================================
// Confirm
// ============================================================================

func TestPaymentService_Confirm_FreshOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID:       order.ID,
		UserID:        uid,
		Channel:       "stripe",
		ExternalTxnID: "pi_test_1",
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if res.Status != "paid" {
		t.Errorf("Status = %q", res.Status)
	}
	if !res.ActivatedSubscription {
		t.Errorf("ActivatedSubscription = false, want true")
	}

	// Order should now be paid.
	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after Confirm: order.Status = %q, want paid", got.Status)
	}
}

func TestPaymentService_Confirm_ExpiredOrder_LatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	// Force to expired.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired' WHERE id = $1`, order.ID)

	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID:       order.ID,
		UserID:        uid,
		Channel:       "stripe",
		ExternalTxnID: "pi_late_1",
	})
	if err != nil {
		t.Fatalf("Confirm late: %v", err)
	}
	if !res.WasLatePayment {
		t.Errorf("WasLatePayment = false, want true")
	}
	// audit_log should have a late_payment_post_expiry row.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'late_payment_post_expiry'`)
	if n == 0 {
		t.Errorf("expected audit_log row for late payment")
	}
}

func TestPaymentService_Confirm_InvalidChannel(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "bogus_channel", ExternalTxnID: "x",
	})
	if !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("err = %v, want ErrInvalidChannel", err)
	}
}

func TestPaymentService_Confirm_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: mustNewUUID(), UserID: "x", Channel: "stripe", ExternalTxnID: "y",
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Confirm_TerminalState(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'failed' WHERE id = $1`, order.ID)
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "z",
	})
	if !errors.Is(err, ErrOrderAlreadyTerminal) {
		t.Errorf("err = %v, want ErrOrderAlreadyTerminal", err)
	}
}

func TestPaymentService_Confirm_ChannelMismatch(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// First confirm on stripe.
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-1",
	}); err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	// Now confirm again with a different channel — but order is already paid.
	// Should refuse with channel mismatch (we have a paid payment on stripe).
	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "alipay", ExternalTxnID: "alipay-1",
	})
	if !errors.Is(err, ErrOrderChannelMismatch) {
		t.Errorf("err = %v, want ErrOrderChannelMismatch", err)
	}
}

func TestPaymentService_Confirm_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	in := ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-idem",
	}
	if _, err := svc.Confirm(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := svc.Confirm(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if res.Status != "paid" {
		t.Errorf("Status = %q", res.Status)
	}
}

// TestPaymentService_Confirm_ExistingFailedPayment covers the
// "existing payment is in 'failed' state" branch in Confirm. The
// dedup hit returns a payment row but it's in a terminal state —
// Confirm must refuse with ErrOrderAlreadyTerminal.
func TestPaymentService_Confirm_ExistingFailedPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Pre-insert a payment row in 'failed' state for the same
	// (channel, external_txn_id) the Confirm is about to use. This
	// drives the "dedupe hit + status=failed" branch in Confirm.
	txnID := "pi-failed-confirm-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed failed payment: %v", err)
	}

	_, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: txnID,
	})
	if !errors.Is(err, ErrOrderAlreadyTerminal) {
		t.Errorf("err = %v, want ErrOrderAlreadyTerminal", err)
	}
}

// ============================================================================
// Refund
// ============================================================================

// TestPaymentService_Refund_NotOwner covers the "user doesn't own the
// payment" branch in Refund — the function returns ErrPaymentNotFound
// to hide existence from non-owners (consistent with GetPayment).
func TestPaymentService_Refund_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	ownerID := seedUser(t, db)
	otherID := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), ownerID, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: ownerID, Channel: "stripe", ExternalTxnID: "pi-other-owner",
	})
	// otherID tries to refund owner's payment — must be denied.
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID:      res.PaymentID,
		UserID:         otherID,
		IdempotencyKey: "k-other-owner-" + mustNewUUID()[:8],
		Amount:         10,
	})
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestPaymentService_Refund_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-r1",
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	stub := svc.refundAPI.(*stubRefundAPI)
	stub.returnID = "re_1"
	r, err := svc.Refund(context.Background(), RefundInput{
		PaymentID:      res.PaymentID,
		UserID:         uid,
		IdempotencyKey: "user-req-001",
		Amount:         10.0,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if r.Refund.Amount != 10.0 {
		t.Errorf("Amount = %v", r.Refund.Amount)
	}
	if r.Existing {
		t.Errorf("first refund should not be Existing")
	}
}

func TestPaymentService_Refund_Idempotent(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-r2",
	})
	in := RefundInput{
		PaymentID: res.PaymentID, UserID: uid,
		IdempotencyKey: "user-req-002", Amount: 5.0,
	}
	if _, err := svc.Refund(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := svc.Refund(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !r2.Existing {
		t.Errorf("second refund should be Existing=true")
	}
}

func TestPaymentService_Refund_MissingIdempotencyKey(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{PaymentID: "x", Amount: 1})
	if !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Errorf("err = %v, want ErrMissingIdempotencyKey", err)
	}
}

func TestPaymentService_Refund_AmountInvalid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{IdempotencyKey: "k1", Amount: 0})
	if !errors.Is(err, ErrRefundAmountInvalid) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Refund_PaymentNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: mustNewUUID(), UserID: mustNewUUID(), IdempotencyKey: "k-nf", Amount: 1,
	})
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_Refund_PaymentNotPaid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Insert a pending payment row directly.
	pendingID := mustNewUUID()
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', 'pending-1', 10, 'CNY', 'pending', '{}')
	`, pendingID, order.ID)

	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: pendingID, UserID: uid, IdempotencyKey: "k-pending", Amount: 1,
	})
	if !errors.Is(err, ErrPaymentNotPaid) {
		t.Errorf("err = %v, want ErrPaymentNotPaid", err)
	}
}

func TestPaymentService_Refund_AmountTooLarge(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-big",
	})
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-big", Amount: 100.0,
	})
	if !errors.Is(err, ErrRefundAmountInvalid) {
		t.Errorf("err = %v, want ErrRefundAmountInvalid", err)
	}
}

func TestPaymentService_Refund_SumExceedsPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-sum",
	})

	// First refund 10 (payment is 19.9 post-migration-016; was 29.9 before).
	if _, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-1", Amount: 10,
	}); err != nil {
		t.Fatalf("first refund: %v", err)
	}

	// Mark first refund paid so it counts in sum invariant.
	_, _ = db.ExecContext(context.Background(),
		`UPDATE refunds SET status = 'paid' WHERE payment_id = $1`, res.PaymentID)

	// Second refund 12 — would push sum to 22 > payment 19.9.
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-2", Amount: 12,
	})
	if !errors.Is(err, ErrRefundSumExceedsPayment) {
		t.Errorf("err = %v, want ErrRefundSumExceedsPayment", err)
	}
}

func TestPaymentService_Refund_ChannelFailed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-fail",
	})
	stub := svc.refundAPI.(*stubRefundAPI)
	stub.returnErr = errors.New("channel boom")
	_, err := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "k-fail", Amount: 1,
	})
	if !errors.Is(err, ErrRefundChannelFailed) {
		t.Errorf("err = %v, want ErrRefundChannelFailed", err)
	}
}

// ============================================================================
// Reads
// ============================================================================

func TestPaymentService_ListUserPayments(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	_, _ = svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-list",
	})

	list, err := svc.ListUserPayments(context.Background(), uid)
	if err != nil {
		t.Fatalf("ListUserPayments: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("got %d, want 1", len(list))
	}
}

func TestPaymentService_GetPayment_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-get",
	})
	got, err := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if err != nil {
		t.Fatalf("GetPayment: %v", err)
	}
	if got.ID != res.PaymentID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetPayment_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-no",
	})
	_, err := svc.GetPayment(context.Background(), res.PaymentID, mustNewUUID())
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestPaymentService_GetPayment_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetPayment(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_GetRefund_Owner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rf",
	})
	r, _ := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "rf-1", Amount: 5,
	})
	got, err := svc.GetRefund(context.Background(), r.Refund.ID, uid)
	if err != nil {
		t.Fatalf("GetRefund: %v", err)
	}
	if got.ID != r.Refund.ID {
		t.Errorf("ID = %q", got.ID)
	}
}

func TestPaymentService_GetRefund_NotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.GetRefund(context.Background(), mustNewUUID(), "any")
	if !errors.Is(err, ErrRefundNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_GetRefund_NotOwner(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rfno",
	})
	r, _ := svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "rf-no", Amount: 5,
	})
	_, err := svc.GetRefund(context.Background(), r.Refund.ID, mustNewUUID())
	if !errors.Is(err, ErrRefundNotFound) {
		t.Errorf("err = %v, want ErrRefundNotFound", err)
	}
}

func TestPaymentService_ListPaymentRefunds(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-lpr",
	})
	_, _ = svc.Refund(context.Background(), RefundInput{
		PaymentID: res.PaymentID, UserID: uid, IdempotencyKey: "lpr-1", Amount: 5,
	})
	list, err := svc.ListPaymentRefunds(context.Background(), res.PaymentID, uid)
	if err != nil {
		t.Fatalf("ListPaymentRefunds: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("got %d, want 1", len(list))
	}
}

// TestPaymentService_ListPaymentRefunds_PaymentNotFound covers the
// error-propagation path in ListPaymentRefunds — when GetPayment
// returns ErrPaymentNotFound (non-owner or missing payment),
// ListPaymentRefunds must surface the same error.
func TestPaymentService_ListPaymentRefunds_PaymentNotFound(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-lpr-nf",
	})
	// List with a different user — non-owner → ErrPaymentNotFound.
	other := seedUser(t, db)
	_, err := svc.ListPaymentRefunds(context.Background(), res.PaymentID, other)
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("err = %v, want ErrPaymentNotFound", err)
	}
}

// ============================================================================
// OnWebhook
// ============================================================================

func TestPaymentService_OnWebhook_StripePaymentSucceeded(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	raw := json.RawMessage(`{"id":"evt-1","type":"payment_intent.succeeded"}`)
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-1", EventType: "payment_intent.succeeded",
		TransactionID: "pi_wh_1", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: raw,
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	if res.DomainAction != "payment_paid" {
		t.Errorf("DomainAction = %q", res.DomainAction)
	}
	if res.DuplicateEvent {
		t.Errorf("DuplicateEvent = true on first call")
	}
	// Order should now be paid.
	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after webhook: Status = %q", got.Status)
	}
}

func TestPaymentService_OnWebhook_UnknownOrder_Audit(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-unknown", EventType: "payment_intent.succeeded",
		TransactionID: "pi-x", OrderID: mustNewUUID(), Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	// Should NOT error — written to audit_log instead.
	if err != nil {
		t.Errorf("unknown order webhook err: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'webhook_for_unknown_order'`)
	if n == 0 {
		t.Errorf("expected audit_log row for unknown order webhook")
	}
}

func TestPaymentService_OnWebhook_Duplicate(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	ev := WebhookEvent{
		Channel: "stripe", EventID: "evt-dup", EventType: "payment_intent.succeeded",
		TransactionID: "pi-dup", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}
	if _, err := svc.OnWebhook(context.Background(), ev); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := svc.OnWebhook(context.Background(), ev)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !res.DuplicateEvent {
		t.Errorf("DuplicateEvent = false on second call")
	}
}

func TestPaymentService_OnWebhook_PaymentFailed(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	// Create a pending order, then a pending payment row, then fire payment_failed.
	orderID := mustNewUUID()
	txnID := "pi-fail-" + mustNewUUID()[:8]
	eventID := "evt-fail-" + mustNewUUID()[:8]
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes', NULL)
	`, orderID, uid)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Pre-insert a pending payment row so payment_failed finds it directly.
	paymentID := mustNewUUID()
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'pending', '{}')
	`, paymentID, orderID, txnID)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err = svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook failed: %v", err)
	}
	// Both payment and order should be flipped to failed.
	var pStatus string
	_ = db.GetContext(context.Background(), &pStatus,
		`SELECT status FROM payments WHERE id = $1`, paymentID)
	if pStatus != "failed" {
		t.Errorf("payment.Status = %q, want failed", pStatus)
	}
	got, _ := svc.GetOrder(context.Background(), orderID, uid)
	if got.Status != "failed" {
		t.Errorf("order.Status = %q, want failed", got.Status)
	}
}

func TestPaymentService_OnWebhook_RefundFull(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rf",
	})

	// Mark a paid refund so the sum invariant holds for full refund.
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO refunds (id, payment_id, channel, user_id, amount, idempotency_key, external_refund_id, status)
		VALUES (gen_random_uuid(), $1, 'stripe', $2, $3, 'webhook:evt-rf', 're_wh_1', 'paid')
	`, res.PaymentID, uid, 29.9)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rf", EventType: "charge.refunded",
		TransactionID: "pi-rf", RefundAmount: 29.9, ExternalRefundID: "re_wh_1",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook refund: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "refunded" {
		t.Errorf("Payment.Status = %q, want refunded", got.Status)
	}
}

func TestPaymentService_OnWebhook_DisputeCreated(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-disp",
	})

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-disp", EventType: "charge.dispute.created",
		TransactionID: "pi-disp",
		RawPayload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dispute: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if !got.Disputed {
		t.Errorf("Disputed = false after dispute.created")
	}
}

func TestPaymentService_OnWebhook_InvalidChannel(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "bogus_channel", EventID: "evt-pp", EventType: "x",
		RawPayload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("err = %v", err)
	}
}

func TestPaymentService_OnWebhook_DisputeClosed_NoOp(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	// No matching payment — should silently no-op without error.
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-dc", EventType: "charge.dispute.closed",
		TransactionID: "pi-dc",
		RawPayload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.DomainAction != "payment_dispute_closed" {
		t.Errorf("DomainAction = %q", res.DomainAction)
	}
}

// ============================================================================
// onPaypalRenewalSucceeded (PAYMENT.SALE.COMPLETED) — M5 paypal channel
// ============================================================================

// TestPaymentService_OnWebhook_PaymentFailed_NoOrder covers the defensive
// "no payment row + no order row" path inside findOrInsertPendingOnTx.
// A payment_failed event for a non-existent order_id is a no-op (it
// should not error and should not insert anything); the handler returns
// nil so the webhook acks 200.
func TestPaymentService_OnWebhook_PaymentFailed_NoOrder(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	// No user, no order — just fire a payment_failed with a brand-new order_id.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-noorder-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: "pi-noorder-" + mustNewUUID()[:8], OrderID: mustNewUUID(),
		Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook no-order: %v", err)
	}
	// The webhook_events row was inserted (top-level dedup), but no
	// payments row was created.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments`)
	if n != 0 {
		t.Errorf("expected 0 payments rows for no-order path, got %d", n)
	}
}

// TestPaymentService_OnWebhook_PaymentFailed_InsertPending covers the
// "no payment row, order exists, INSERT pending" path inside
// findOrInsertPendingOnTx. The handler must mint a fresh pending
// payment row and link it to the order.
func TestPaymentService_OnWebhook_PaymentFailed_InsertPending(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-insert-pending-" + mustNewUUID()[:8]
	eventID := "evt-insert-pending-" + mustNewUUID()[:8]
	// Seed an order with NO payment row.
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes', NULL)
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook insert-pending: %v", err)
	}
	// A pending payment row should have been inserted.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments WHERE order_id = $1 AND status = 'failed'`, orderID)
	if n != 1 {
		t.Errorf("expected 1 payment row in failed state, got %d", n)
	}
}

// TestPaymentService_OnPaymentSucceeded_ClosedDBError covers an
// error-from-closed-db branch in the OnWebhook call chain. After
// the DB is closed, any subsequent call returns an error — the test
// just asserts the error is non-nil.
func TestPaymentService_OnPaymentSucceeded_ClosedDBError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Close the DB to force the call chain to fail.
	_ = db.Close()

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-closed-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-closed-" + mustNewUUID()[:8], OrderID: order.ID,
		Amount: 29.9, Currency: "CNY", RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from closed db, got nil")
	}
}

// TestPaymentService_OnWebhook_ChannelMismatch covers the "webhook arrives
// for a different channel than the one that already paid" path. The handler
// must NOT touch the existing paid payment, must NOT insert a second paid
// payment, and must write an audit row.
func TestPaymentService_OnWebhook_ChannelMismatch(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	// Pay the order via stripe (frontend Confirm).
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-paid-1",
	}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// Now fire a payment_succeeded webhook for a DIFFERENT channel. The
	// channel-mismatch pre-check in onPaymentSucceeded should detect this
	// and write an audit row, NOT insert a payment.
	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-mismatch-" + mustNewUUID()[:8], EventType: "PAYMENT.CAPTURE.COMPLETED",
		TransactionID: "pi-pp-mismatch", OrderID: order.ID, Amount: 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook mismatch: %v", err)
	}
	// DomainAction should be "none" because the handler took the
	// channel_mismatch early-return path, which is an audit log + ack.
	_ = res // response shape is fine; we just want side-effect coverage.

	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'webhook_channel_mismatch'`)
	if n == 0 {
		t.Error("expected audit_log row for channel_mismatch")
	}
	// Confirm only the original stripe paid payment exists — paypal must
	// NOT have inserted a second one.
	var paypalCount int
	_ = db.GetContext(context.Background(), &paypalCount,
		`SELECT count(*) FROM payments WHERE channel = 'paypal' AND order_id = $1`, order.ID)
	if paypalCount != 0 {
		t.Errorf("expected 0 paypal payments for mismatched channel, got %d", paypalCount)
	}
}

// TestPaymentService_OnWebhook_LatePayment covers the wasLate branch of
// onPaymentSucceeded: order is already 'expired' (post-expiry), the
// payment_succeeded webhook still arrives, and we honor it (mark order
// paid + audit-log the late_payment_post_expiry event).
func TestPaymentService_OnWebhook_LatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Force the order to 'expired' so the wasLate path is taken.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("force expired: %v", err)
	}

	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-late-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-late-1", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("OnWebhook late: %v", err)
	}

	got, _ := svc.GetOrder(context.Background(), order.ID, uid)
	if got.Status != "paid" {
		t.Errorf("after late webhook: order.Status = %q, want paid", got.Status)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'late_payment_post_expiry'`)
	if n == 0 {
		t.Error("expected audit_log row for late_payment_post_expiry")
	}
}

// TestPaymentService_OnWebhook_PaypalStampsExternalSubID covers the
// "e.ExternalSubscriptionID != ”" branch of onPaymentSucceeded — the
// active subscription's external_subscription_id column gets stamped so
// later renewal webhooks can find it.
func TestPaymentService_OnWebhook_PaypalStampsExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	if _, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-stamp-" + mustNewUUID()[:8], EventType: "PAYMENT.CAPTURE.COMPLETED",
		TransactionID: "pi-pp-stamp", OrderID: order.ID, Amount: 29.9, Currency: "USD",
		ExternalSubscriptionID: "I-PAYPAL-SUB-STAMP",
		RawPayload:             json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("OnWebhook stamp: %v", err)
	}

	var gotID string
	if err := db.GetContext(context.Background(), &gotID,
		`SELECT external_subscription_id FROM subscriptions
		 WHERE user_id = $1 AND plan_id = 'monthly' AND status = 'active' LIMIT 1`, uid); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if gotID != "I-PAYPAL-SUB-STAMP" {
		t.Errorf("external_subscription_id = %q, want I-PAYPAL-SUB-STAMP", gotID)
	}
}

// TestPaymentService_OnWebhook_UnexpectedStateTransition covers the
// "existing.Status != paid" branch in onPaymentSucceeded: a (channel,
// external_txn_id) dedupe hit whose existing row is in a non-paid
// state (e.g. 'failed'). The function should write an
// unexpected_state_transition audit row and return nil (no escalation).
func TestPaymentService_OnWebhook_UnexpectedStateTransition(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Pre-insert a payment row that's already 'failed' for a txn_id
	// we're about to redeliver as payment_succeeded.
	txnID := "pi-failed-then-succeeded-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-ust-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook unexpected-state: %v", err)
	}
	_ = res
	// Audit row for unexpected_state_transition.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'unexpected_state_transition'`)
	if n == 0 {
		t.Error("expected audit_log row for unexpected_state_transition")
	}
}

// TestPaymentService_OnWebhook_DisputeCreated_NoPayment covers the
// "no matching payment row" early-return in onDisputeCreated. The handler
// should not error; it just no-ops because there's nothing to flag.
func TestPaymentService_OnWebhook_DisputeCreated_NoPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-disp-nopay-" + mustNewUUID()[:8], EventType: "charge.dispute.created",
		TransactionID: "pi-nopay-" + mustNewUUID()[:8], Amount: 1, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dispute-no-payment: %v", err)
	}
	// No payments row was created by the dispute handler.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM payments WHERE external_txn_id LIKE 'pi-nopay-%'`)
	if n != 0 {
		t.Errorf("expected 0 payments rows for no-payment dispute, got %d", n)
	}
}

// TestPaymentService_OnWebhook_Refund_Partial covers the partial-refund
// path in onRefundSucceeded: the payment stays 'paid', only a refund row
// is created with the partial amount.
func TestPaymentService_OnWebhook_Refund_Partial(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-partial",
	})

	// Partial refund = 10 of 29.9. Payment status should stay 'paid'.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rfp-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-partial", RefundAmount: 10.0, ExternalRefundID: "re_partial_1",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook partial: %v", err)
	}
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "paid" {
		t.Errorf("partial refund: Payment.Status = %q, want paid (stays paid on partial)", got.Status)
	}
}

// TestPaymentService_OnWebhook_PaymentFailed_AfterPaid covers the
// "wasPaid=true" branch in onPaymentFailed: a payment_failed arrives
// for a payment that's already marked paid (rare race). The handler
// must flip the payment to failed, flip the order to failed, AND
// cascade the active subscription to cancelled. Audit row gets
// written via the cascade branch.
func TestPaymentService_OnWebhook_PaymentFailed_AfterPaid(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-paid-then-failed",
	})

	// Now fire a payment_failed webhook — should cascade-cancel the
	// active subscription since the payment was previously paid.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-pf-paid-" + mustNewUUID()[:8], EventType: "payment_intent.payment_failed",
		TransactionID: "pi-paid-then-failed", OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook paid-then-failed: %v", err)
	}
	// Subscription should be cancelled (cascade).
	var subStatus string
	_ = db.GetContext(context.Background(), &subStatus,
		`SELECT status FROM subscriptions WHERE user_id = $1 AND plan_id = 'monthly'`, uid)
	if subStatus != "cancelled" {
		t.Errorf("sub.Status after paid-then-failed = %q, want cancelled (cascade)", subStatus)
	}
	// Audit row should mention the cascade.
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'subscription_deactivated_failed_payment'`)
	if n == 0 {
		t.Error("expected audit row for subscription_deactivated_failed_payment")
	}
	_ = res // silence unused
}

// TestPaymentService_OnWebhook_Refund_MissingPayment covers the
// "no payment row" branch in onRefundSucceeded (the handler should
// no-op rather than error).
func TestPaymentService_OnWebhook_Refund_MissingPayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rf-nopay-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rf-nopay-" + mustNewUUID()[:8], RefundAmount: 1, ExternalRefundID: "re_nopay",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Errorf("missing-payment refund should not error, got: %v", err)
	}
}

// TestPaymentService_OnWebhook_Refund_ZeroAmount covers the
// "insert refund" error path in onRefundSucceeded. The refunds table
// has CHECK (amount > 0); a RefundAmount of 0 violates this and
// the INSERT fails with a check_violation error. The handler must
// surface the wrapped error.
func TestPaymentService_OnWebhook_Refund_ZeroAmount(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rf-zero",
	})

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-rf-zero-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rf-zero", RefundAmount: 0, ExternalRefundID: "re_zero",
		RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from zero-amount refund insert, got nil")
	}
	if !strings.Contains(err.Error(), "insert refund") {
		t.Errorf("expected wrap 'insert refund', got %q", err.Error())
	}
	_ = res // silence unused
}

// TestPaymentService_OnWebhook_PaymentFailed_TerminalState covers the
// "n == 0" branch in onPaymentFailed: a payment_failed arrives for a
// payment that's already in a terminal state (e.g. 'failed'). The SQL
// guard refuses to transition, the function commits the empty tx and
// returns nil — no-op.
func TestPaymentService_OnWebhook_PaymentFailed_TerminalState(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-failed-term-" + mustNewUUID()[:8]
	eventID := "evt-failed-term-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'failed', now() + INTERVAL '30 minutes', NULL)
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Pre-insert a payment row that's already 'failed' (terminal).
	paymentID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'failed', '{}')
	`, paymentID, orderID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook terminal-state: %v", err)
	}
	// Payment should still be 'failed' (no transition, no change).
	var pStatus string
	_ = db.GetContext(context.Background(), &pStatus,
		`SELECT status FROM payments WHERE id = $1`, paymentID)
	if pStatus != "failed" {
		t.Errorf("payment.Status = %q, want failed (unchanged)", pStatus)
	}
}

// TestPaymentService_OnWebhook_UnknownEventType covers the default
// branch in OnWebhook's switch: event types that match no domain
// action (e.g. "ping" or any other uninteresting type). The handler
// should ack 200 with DomainAction="none".
func TestPaymentService_OnWebhook_UnknownEventType(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-ping-" + mustNewUUID()[:8], EventType: "ping.unknown_type",
		TransactionID: "tx-ping-" + mustNewUUID()[:8], Amount: 0, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("unknown event type: %v", err)
	}
	if res.DomainAction != "none" {
		t.Errorf("DomainAction = %q, want none", res.DomainAction)
	}
	if res.DuplicateEvent {
		t.Error("DuplicateEvent should be false on first delivery")
	}
}

// TestPaymentService_OnWebhook_Refund_ReRun covers the "refund row
// already inserted by a prior delivery" branch in onRefundSucceeded.
// The INSERT ... ON CONFLICT DO NOTHING returns no row, the handler
// re-reads the existing refund ID, and the rest of the flow proceeds
// idempotently.
func TestPaymentService_OnWebhook_Refund_ReRun(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	res, _ := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: order.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi-rerun",
	})

	ev := WebhookEvent{
		Channel: "stripe", EventID: "evt-rerun-" + mustNewUUID()[:8], EventType: "charge.refunded",
		TransactionID: "pi-rerun", RefundAmount: 29.9, ExternalRefundID: "re_rerun_1",
		RawPayload: json.RawMessage(`{}`),
	}
	// First delivery — INSERTs the refund row.
	if _, err := svc.OnWebhook(context.Background(), ev); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	// Second delivery with a different event_id (so the webhook_events
	// dedup at the top doesn't short-circuit) but the same external_refund_id.
	// This drives the "INSERT returned no row, re-read by ext id" branch.
	ev2 := ev
	ev2.EventID = "evt-rerun2-" + mustNewUUID()[:8]
	if _, err := svc.OnWebhook(context.Background(), ev2); err != nil {
		t.Fatalf("re-run refund: %v", err)
	}
	// Payment should still be refunded (idempotent).
	got, _ := svc.GetPayment(context.Background(), res.PaymentID, uid)
	if got.Status != "refunded" {
		t.Errorf("after re-run: Payment.Status = %q, want refunded", got.Status)
	}
}

// TestPaymentService_OnWebhook_ReRunAfterCrash covers the "prior run
// crashed mid-action" branch in OnWebhook: an existing webhook_events
// row with processed_at IS NULL. The handler must re-run the business
// action (idempotent) and MarkProcessed the existing row.
func TestPaymentService_OnWebhook_ReRunAfterCrash(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Manually insert a webhook_events row with processed_at NULL.
	// This simulates "prior run crashed before MarkProcessed".
	eventID := "evt-recovery-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload, processed_at)
		VALUES ('stripe', $1, 'payment_intent.succeeded', '{}', NULL)
	`, eventID); err != nil {
		t.Fatalf("seed webhook_events: %v", err)
	}

	res, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.succeeded",
		TransactionID: "pi-recovery-" + mustNewUUID()[:8], OrderID: order.ID,
		Amount: 29.9, Currency: "CNY", RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook recovery: %v", err)
	}
	if res.DuplicateEvent {
		t.Error("DuplicateEvent should be false on re-run after crash")
	}
	if res.DomainAction != "payment_paid" {
		t.Errorf("DomainAction = %q, want payment_paid", res.DomainAction)
	}
	// The row's processed_at should now be NOT NULL.
	var processedAt *time.Time
	if err := db.GetContext(context.Background(), &processedAt,
		`SELECT processed_at FROM webhook_events WHERE channel = 'stripe' AND event_id = $1`, eventID); err != nil {
		t.Fatalf("read processed_at: %v", err)
	}
	if processedAt == nil {
		t.Error("expected processed_at to be set after re-run")
	}
}

// TestPaymentService_OnWebhook_ReRunSameTxnID covers the "dedup hit
// on (channel, external_txn_id) with status=paid" branch in
// onPaymentSucceeded. A pre-existing paid payment with the SAME
// (channel, external_txn_id) causes the INSERT to be a no-op, and
// the function reads back the existing payment ID.
func TestPaymentService_OnWebhook_ReRunSameTxnID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	// Pre-insert a paid payment + a webhook_events row that
	// references the same (channel, external_txn_id). When the new
	// webhook fires, the InsertOnConflictDoNothing on webhook_events
	// returns inserted=false (since the event_id is the same), and
	// the onPaymentSucceeded handler's INSERT into payments also
	// returns inserted=false (ON CONFLICT on channel, external_txn_id).
	txnID := "pi-dedup-" + mustNewUUID()[:8]
	eventID := "evt-dedup-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO webhook_events (channel, event_id, event_type, raw_payload, processed_at)
		VALUES ('stripe', $1, 'payment_intent.succeeded', '{}', NULL)
	`, eventID); err != nil {
		t.Fatalf("seed webhook_events: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, paid_at, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'paid', now(), '{}')
	`, mustNewUUID(), order.ID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.succeeded",
		TransactionID: txnID, OrderID: order.ID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook dedup: %v", err)
	}

	// The order's status should still be 'paid' (it was paid before).
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("order.Status = %q, want paid (unchanged after dedup)", status)
	}
}

// TestPaymentService_OnWebhook_BadCurrency covers the "insert
// payment" error path in onPaymentSucceeded. The payments table
// has CHECK (length(currency) = 3); a currency of wrong length
// violates the constraint and the INSERT fails with check_violation.
func TestPaymentService_OnWebhook_BadCurrency(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	order, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-bad-cur-" + mustNewUUID()[:8], EventType: "payment_intent.succeeded",
		TransactionID: "pi-bad-cur-" + mustNewUUID()[:8], OrderID: order.ID,
		Amount: 29.9, Currency: "DOLLAR", // 6 chars — violates CHECK (length=3)
		RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from bad-currency insert, got nil")
	}
	if !strings.Contains(err.Error(), "insert payment") {
		t.Errorf("expected wrap 'insert payment', got %q", err.Error())
	}
}

// TestPaymentService_CreateOrder_GenericError covers the wrap paths
// in CreateOrder that aren't covered by the "plan not found" / "plan
// inactive" / "user has active sub" tests. After D8 the subRepo
// active-sub check runs before the eligibility tx, so a closed DB now
// surfaces as "check active sub" (subRepo is the first DB-backed
// call). The planRepo wrap path is exercised in payment_db_test.go's
// real-DB CreateOrder tests where the plan lookup runs inside a tx.
func TestPaymentService_CreateOrder_GenericErrors(t *testing.T) {
	t.Run("planRepo generic error", func(t *testing.T) {
		db := setupPaymentDB(t)
		planRepo := repo.NewPlanRepo(db)
		// Drop the seeded plans so FindByID returns a non-ErrNoRows error.
		// We need a true generic error — easiest is to close the DB and
		// call on a closed handle.
		_ = db.Close()
		uid := mustNewUUID()
		svc := &PaymentService{
			planRepo: planRepo,
			subRepo:  repo.NewSubscriptionRepo(db),
		}
		_, err := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
		if err == nil {
			t.Fatal("expected error from closed db, got nil")
		}
		// SubRepo active-sub check runs first now (see CreateOrder doc),
		// so a closed DB surfaces as "check active sub". Plan eligibility
		// (FOR SHARE on plans) is exercised separately in the real-DB
		// TestPaymentService_CreateOrder_PlanDeactivatedDuringTx.
		if !strings.Contains(err.Error(), "check active sub") {
			t.Errorf("expected wrap 'check active sub', got %q", err.Error())
		}
	})
}

// TestPaymentService_OnPaymentFailed_BeginTxError covers an
// error-from-closed-db branch in the OnWebhook call chain. After
// the DB is closed, any subsequent call returns an error — the test
// just asserts the error is non-nil and surfaced. The exact wrap
// depends on which tx-bound call the runtime hits first.
func TestPaymentService_OnPaymentFailed_BeginTxError(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	orderID := mustNewUUID()
	txnID := "pi-begin-tx-fail-" + mustNewUUID()[:8]
	eventID := "evt-begin-tx-fail-" + mustNewUUID()[:8]
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO orders (id, user_id, plan_id, amount, currency, status, expires_at, provider_intent)
		VALUES ($1, $2, 'monthly', 29.9, 'CNY', 'pending', now() + INTERVAL '30 minutes', NULL)
	`, orderID, uid); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	paymentID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO payments (id, order_id, channel, external_txn_id, amount, currency, status, raw_payload)
		VALUES ($1, $2, 'stripe', $3, 29.9, 'CNY', 'pending', '{}')
	`, paymentID, orderID, txnID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	// Close the DB to force the call chain to fail.
	_ = db.Close()

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: eventID, EventType: "payment_intent.payment_failed",
		TransactionID: txnID, OrderID: orderID, Amount: 29.9, Currency: "CNY",
		RawPayload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error from closed db, got nil")
	}
	// We don't assert on the exact wrap — closed-db errors vary by
	// driver / Go version. The point is that the failure surfaces.
}

func TestPaymentService_OnWebhook_PaypalRenewal_Success(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	// Seed a user with an active subscription that has a PayPal
	// external_subscription_id, so the renewal webhook can find it.
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, $4)
	`, subID, uid, expAt, "I-PAYPAL-SUB-1"); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	txnID := "PAYMENT-SALE-" + mustNewUUID()[:8]
	eventID := "evt-renewal-" + mustNewUUID()[:8]
	newExpAt := time.Now().Add(45 * 24 * time.Hour)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: eventID, EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: txnID, ExternalSubscriptionID: "I-PAYPAL-SUB-1",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &newExpAt,
		RawPayload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook renewal: %v", err)
	}

	// 1. Subscription expires_at should be extended to newExpAt.
	var newExp time.Time
	if err := db.GetContext(context.Background(), &newExp,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if newExp.Unix() != newExpAt.Unix() {
		t.Errorf("expires_at: got %v, want %v", newExp, newExpAt)
	}
}

func TestPaymentService_OnWebhook_PaypalRenewal_MissingExternalSubID(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '30 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	// No ExternalSubscriptionID on the event → audit-log path, not a real
	// renewal. Order should NOT be created.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-missing-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-missing-" + mustNewUUID()[:8], Amount: 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	var orderCount int
	_ = db.GetContext(context.Background(), &orderCount,
		`SELECT COUNT(*) FROM orders WHERE user_id = $1`, uid)
	if orderCount != 0 {
		t.Errorf("expected 0 orders, got %d", orderCount)
	}
}

func TestPaymentService_OnWebhook_PaypalRenewal_UnknownSubscription(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at)
		VALUES ($1, $2, 'monthly', 'active', now() + INTERVAL '30 days')
	`, subID, uid); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	// ExternalSubscriptionID does not match any row → audit-log "unknown
	// subscription" path.
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-unk-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID:          "txn-unk-" + mustNewUUID()[:8],
		ExternalSubscriptionID: "I-DOES-NOT-EXIST",
		Amount:                 29.9, Currency: "USD",
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook: %v", err)
	}
	var orderCount int
	_ = db.GetContext(context.Background(), &orderCount,
		`SELECT COUNT(*) FROM orders WHERE user_id = $1`, uid)
	if orderCount != 0 {
		t.Errorf("expected 0 orders, got %d", orderCount)
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_DuplicatePayment covers
// the "dedupe-check existing renewal payment" branch in
// onPaypalRenewalSucceeded: a payment row already exists for the same
// (channel, external_txn_id). The handler must short-circuit with an
// audit log, NOT create a new order or payment.
func TestPaymentService_OnWebhook_PaypalRenewal_DuplicatePayment(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, 'I-PP-DUP')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	txnID := "txn-pp-dup-" + mustNewUUID()[:8]
	// First, fire the renewal normally to create a payment row.
	first := WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-dup-first-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: txnID, ExternalSubscriptionID: "I-PP-DUP",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &expAt,
		RawPayload:   json.RawMessage(`{}`),
	}
	if _, err := svc.OnWebhook(context.Background(), first); err != nil {
		t.Fatalf("first OnWebhook: %v", err)
	}
	// Now fire a second webhook with a NEW event_id (so the top-level
	// dedup at webhook_events doesn't catch it) but the same (channel,
	// external_txn_id). This drives the "existing payment" branch in
	// onPaypalRenewalSucceeded.
	second := first
	second.EventID = "evt-pp-dup-second-" + mustNewUUID()[:8]
	if _, err := svc.OnWebhook(context.Background(), second); err != nil {
		t.Fatalf("second OnWebhook: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_payment_already_exists'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_payment_already_exists")
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_NoExpiryHint covers the
// "SubExpiresAt is nil" branch in onPaypalRenewalSucceeded: PayPal
// charged but didn't ship a next_billing_time hint. The handler must
// still record the payment (PayPal did charge) and write a
// paypal_renewal_no_expiry_hint audit row.
func TestPaymentService_OnWebhook_PaypalRenewal_NoExpiryHint(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'active', $3, 'I-PP-NOEXP')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-noexp-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-pp-noexp-" + mustNewUUID()[:8], ExternalSubscriptionID: "I-PP-NOEXP",
		Amount: 29.9, Currency: "USD",
		// No SubExpiresAt — drives the "no expiry hint" branch.
		RawPayload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook no-expiry: %v", err)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_no_expiry_hint'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_no_expiry_hint")
	}
}

// TestPaymentService_OnWebhook_PaypalRenewal_SubNotActive covers the
// "sub.Status != active but UPDATE didn't fire" branch in
// onPaypalRenewalSucceeded. Pre-cancel the sub, then fire a renewal —
// the handler records the payment (PayPal did charge) but does NOT
// extend expires_at, and writes a paypal_renewal_sub_not_active audit.
func TestPaymentService_OnWebhook_PaypalRenewal_SubNotActive(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := mustNewUUID()
	expAt := time.Now().Add(15 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, plan_id, status, expires_at, external_subscription_id)
		VALUES ($1, $2, 'monthly', 'cancelled', $3, 'I-PP-CANCEL')
	`, subID, uid, expAt); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	newExpAt := time.Now().Add(45 * 24 * time.Hour)
	_, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel: "paypal", EventID: "evt-pp-cancel-" + mustNewUUID()[:8], EventType: "PAYMENT.SALE.COMPLETED",
		TransactionID: "txn-pp-cancel-" + mustNewUUID()[:8], ExternalSubscriptionID: "I-PP-CANCEL",
		Amount: 29.9, Currency: "USD",
		SubExpiresAt: &newExpAt,
		RawPayload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("OnWebhook sub-cancelled: %v", err)
	}
	// Sub's expires_at must NOT have been extended (still the original).
	var gotExp time.Time
	_ = db.GetContext(context.Background(), &gotExp,
		`SELECT expires_at FROM subscriptions WHERE id = $1`, subID)
	if gotExp.Unix() != expAt.Unix() {
		t.Errorf("sub.expires_at was extended despite cancelled status: got %v, want %v", gotExp, expAt)
	}
	var n int
	_ = db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM audit_log WHERE action = 'paypal_renewal_sub_not_active'`)
	if n == 0 {
		t.Error("expected audit_log row for paypal_renewal_sub_not_active")
	}
}

// TestResolveSubExpiry_HintForwarded covers the hint branch of
// resolveSubExpiry: when the caller supplies a hint (BFF on Confirm;
// webhook payload on channels that ship sub_expires_at, e.g. Stripe
// metadata / PayPal renewal) AND there is no active subscription to
// roll over, the helper must forward the hint verbatim and never touch
// the plan row. The retry/rollover/fallback branches are exercised
// end-to-end by the OnWebhook + Confirm tests.
func TestResolveSubExpiry_HintForwarded(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)
	uid := seedUser(t, db)

	hint := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	got, err := s.resolveSubExpiry(context.Background(), tx, uid, "monthly", &hint, nil)
	if err != nil {
		t.Fatalf("resolveSubExpiry: %v", err)
	}
	if got == nil || !got.Equal(hint) {
		t.Errorf("hint not forwarded: got %v, want %v", got, hint)
	}
}

// TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval exercises the
// WeChat v3 NATIVE fallback path inside onPaymentSucceeded: the webhook
// payload carries no SubExpiresAt, so resolveSubExpiry must compute
// expires_at = NOW() + plan.interval_days (monthly → 30 days) and the
// activated subscription row must have a non-NULL expires_at. Pre-fix
// the row stayed NULL because subExpiresAtFromWebhook returned e.SubExpiresAt
// verbatim — which for real WeChat webhooks is always nil.
func TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)

	userID := seedUser(t, db)
	planID := "monthly"
	orderID := seedPaidOrder(t, db, userID, planID, 19.9)

	// WebhookEvent with no SubExpiresAt — simulates real WeChat v3.
	e := WebhookEvent{
		Channel:       "wechat_pay",
		EventID:       "evt-wc-1",
		EventType:     "TRANSACTION.SUCCESS",
		TransactionID: "txn-wc-1",
		OrderID:       orderID,
		Amount:        19.9,
		Currency:      "CNY",
	}

	if err := s.onPaymentSucceeded(context.Background(), e); err != nil {
		t.Fatalf("onPaymentSucceeded: %v", err)
	}

	var exp sql.NullTime
	if err := db.Get(&exp,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if !exp.Valid {
		t.Fatal("expires_at is NULL — fallback did not fire")
	}
	if exp.Time.Before(time.Now()) {
		t.Error("expires_at is in the past")
	}
	// Tight range around monthly.interval_days = 30. The 23h lower-bound
	// slack absorbs up to one day of test-fixture build / CI scheduling
	// latency; the +1min upper-bound slack absorbs the clock drift between
	// resolveSubExpiry's time.Now() and this SELECT. A regression to a
	// 29d / 31d / 1d fallback would land outside this window.
	delta := time.Until(exp.Time)
	if delta < 29*24*time.Hour || delta > 30*24*time.Hour+time.Minute {
		t.Errorf("expires_at outside monthly window: delta=%v, want ≈30d", delta)
	}
}

// TestConfirm_NoHint_UsesPlanInterval exercises the BFF-confirmed path's
// fallback to plan.interval_days when the caller (typically the frontend
// after a WeChat v3 NATIVE charge) does NOT forward an ExpiresAt. This
// mirrors the webhook fallback (TestOnPaymentSucceeded_WeChatNoHint_UsesPlanInterval)
// but goes through Confirm rather than onPaymentSucceeded. Pre-fix, the
// subscription row's expires_at stayed NULL because Confirm forwarded
// in.ExpiresAt verbatim — which for WeChat v3 is always nil. After the
// fix, Confirm calls resolveSubExpiry(planID, in.ExpiresAt), and the
// helper falls through to plan.interval_days (monthly = 30 days).
func TestConfirm_NoHint_UsesPlanInterval(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)

	userID := seedUser(t, db)
	planID := "monthly"
	orderID := seedPaidOrder(t, db, userID, planID, 19.9)

	res, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       orderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-confirm-1",
		ExpiresAt:     nil, // BFF didn't pass one
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !res.ActivatedSubscription {
		t.Error("subscription not activated")
	}

	var exp sql.NullTime
	if err := db.Get(&exp,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if !exp.Valid {
		t.Fatal("expires_at is NULL — Confirm fallback did not fire")
	}
	if exp.Time.Before(time.Now()) {
		t.Error("expires_at is in the past")
	}
	// Tight range around monthly.interval_days = 30. The 23h lower-bound
	// slack absorbs up to one day of test-fixture build / CI scheduling
	// latency; the +1min upper-bound slack absorbs the clock drift between
	// resolveSubExpiry's time.Now() and this SELECT. A regression to a
	// 29d / 31d / 1d fallback would land outside this window.
	delta := time.Until(exp.Time)
	if delta < 29*24*time.Hour || delta > 30*24*time.Hour+time.Minute {
		t.Errorf("expires_at outside monthly window: delta=%v, want ≈30d", delta)
	}
}

// TestConfirm_RetryDoesNotExtend is the regression test for the
// idempotency bug introduced by Task 3: resolveSubExpiry was called
// on every Confirm/webhook event, including dedupe retries on the
// same (channel, external_txn_id). Each retry re-computed
// time.Now()+interval_days*24h, and activateSubscriptionOnTx then
// unconditionally overwrote expires_at. So a t0+1h retry shifted
// expires_at from t0+30d to t0+1h+30d — every retry granted extra time.
//
// This test pins the fix: the first Confirm establishes expires_at at
// t0+30d, then we sleep long enough for a buggy implementation to
// compute a noticeably different "now()+30d". The second Confirm with
// the same ExternalTxnID is a dedupe hit on the payments row, but
// resolveSubExpiry must return the existing expires_at verbatim
// rather than recomputing. Without the fix, the second expires_at
// would be strictly later than the first.
func TestConfirm_RetryDoesNotExtend(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)

	userID := seedUser(t, db)
	planID := "monthly"
	orderID := seedPaidOrder(t, db, userID, planID, 19.9)

	// First confirm — establishes the expiry.
	if _, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       orderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-retry-1",
		ExpiresAt:     nil,
	}); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	var firstExp sql.NullTime
	if err := db.Get(&firstExp,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("read sub after first: %v", err)
	}
	if !firstExp.Valid {
		t.Fatal("first expires_at is NULL")
	}

	// Sleep a small amount so a buggy implementation that re-runs the
	// fallback would compute a noticeably different `now()` + 30d.
	time.Sleep(50 * time.Millisecond)

	// Second confirm — same ExternalTxnID, dedupe hit. The activation
	// SQL still runs; resolveSubExpiry must preserve the existing expiry
	// rather than recompute.
	if _, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       orderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-retry-1",
		ExpiresAt:     nil,
	}); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	var secondExp sql.NullTime
	if err := db.Get(&secondExp,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("read sub after second: %v", err)
	}
	if !secondExp.Valid {
		t.Fatal("second expires_at is NULL")
	}
	if !secondExp.Time.Equal(firstExp.Time) {
		t.Errorf("retry extended expires_at: first=%v second=%v", firstExp.Time, secondExp.Time)
	}
}

// TestConfirm_DifferentOrder_SameUser_FallbackApplies covers the
// critical case where a user has an active sub from a previous order
// and a NEW order for a different plan arrives. The helper must
// apply the new plan's interval_days, NOT preserve the previous sub's
// expiry. Pre-fix the helper checked "any active sub for user" and
// would return the previous sub's expiry — paying for a 365-day
// yearly would only get 30 days because the existing monthly's
// expires_at was preserved.
func TestConfirm_DifferentOrder_SameUser_UpgradeRollover(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)

	// Seed the yearly plan alongside the seeded monthly/free. The test
	// setup only seeds monthly + free; this test needs a different
	// plan with a different interval_days to detect the cross-order
	// preservation bug.
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps)
		VALUES ('yearly', 'Yearly', 199.9, 365, ARRAY['yundian', 'yundash'])
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed yearly: %v", err)
	}

	userID := seedUser(t, db)
	monthlyPlanID := "monthly"
	yearlyPlanID := "yearly"

	// First order: monthly. Confirm.
	monthlyOrderID := seedPaidOrder(t, db, userID, monthlyPlanID, 19.9)
	if _, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       monthlyOrderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-monthly-1",
		ExpiresAt:     nil,
	}); err != nil {
		t.Fatalf("first confirm (monthly): %v", err)
	}

	// Second order: yearly. Different order, different ExternalTxnID,
	// no dedupe hit. The helper must compute a fresh expiry.
	yearlyOrderID := seedPaidOrder(t, db, userID, yearlyPlanID, 199.9)
	if _, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       yearlyOrderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-yearly-1",
		ExpiresAt:     nil,
	}); err != nil {
		t.Fatalf("second confirm (yearly): %v", err)
	}

	// The active sub now reflects yearly. Under the 2026-07-28 rollover
	// rule the remaining monthly days (~30d from the first confirm's
	// interval fallback) carry over: expiry ≈ now + 30d + 365d, NOT a
	// plain now+365d fallback. This still pins the original cross-order
	// regression too — a wrongly-preserved monthly expiry would sit at
	// ~now+30d, far outside the window.
	var exp sql.NullTime
	if err := db.Get(&exp,
		`SELECT expires_at FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("read sub: %v", err)
	}
	if !exp.Valid {
		t.Fatal("expires_at is NULL")
	}
	expectedMin := time.Now().Add(394 * 24 * time.Hour)
	expectedMax := time.Now().Add(396 * 24 * time.Hour)
	if exp.Time.Before(expectedMin) || exp.Time.After(expectedMax) {
		t.Errorf("expires_at should be ~now+395d (monthly remainder + 365d rollover); got %v", exp.Time)
	}
}

// TestConfirm_PlanMissing_AuditLogNoActivation covers the audit-log
// branch in resolveSubExpiry when the plan row was deleted between
// order creation and Confirm arrival. Expected behavior: audit-log is
// written; subscription activation is skipped (a subscription cannot
// reference the missing plan — the FK would reject the insert); the
// order's payment still succeeds for manual ops follow-up.
func TestConfirm_PlanMissing_AuditLogNoActivation(t *testing.T) {
	db := setupPaymentDB(t)
	s := newTestPaymentService(t, db)

	userID := seedUser(t, db)
	planID := "doomed"
	// Insert a plan, seed the order, then delete the plan. The
	// orders.plan_id FK is ON DELETE RESTRICT, so we have to bypass
	// the FK enforcement via session_replication_role='replica' to
	// simulate the rare "plan row missing" state (production rarely
	// hits it because the FK would block plan deletion in the first
	// place, but the audit-log path must still behave correctly when
	// reached).
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active) VALUES ($1, 'Doomed', 0, 30, '{yundian}', true)`,
		planID); err != nil {
		t.Fatalf("seed doomed plan: %v", err)
	}
	orderID := seedPaidOrder(t, db, userID, planID, 0.0)

	// PostgreSQL SET is per-session, and database/sql returns connections
	// to the pool after each ExecContext call. The three statements below
	// (SET replica, DELETE, SET origin) must therefore run on the SAME
	// pinned connection — otherwise the SET may land on a connection that
	// never sees the DELETE, leaving the FK enforcement enabled and
	// making this test flaky. Use db.Conn to pin the connection; the
	// Confirm call below goes through the normal pool (the pin is
	// released by the deferred Close when the test returns).
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = 'replica'`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM plans WHERE id = $1`, planID); err != nil {
		t.Fatalf("delete doomed plan: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET session_replication_role = 'origin'`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}

	if _, err := s.Confirm(context.Background(), ConfirmInput{
		OrderID:       orderID,
		UserID:        userID,
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-plan-missing-1",
		ExpiresAt:     nil,
	}); err != nil {
		t.Fatalf("Confirm should not fail on plan missing (audit + no activation): %v", err)
	}

	// No subscription may be created: a row referencing the missing plan
	// would violate subscriptions_plan_id_fkey. The payment still goes
	// through for manual ops to follow up from the audit log.
	var subCount int
	if err := db.Get(&subCount,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("count subs: %v", err)
	}
	if subCount != 0 {
		t.Errorf("expected no subscription row when plan is missing, got %d", subCount)
	}

	// Audit log entry was written.
	var auditCount int
	if err := db.Get(&auditCount,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'subscription_expiry_plan_missing' AND target = $1`,
		fmt.Sprintf("plan:%s", planID)); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount == 0 {
		t.Error("expected subscription_expiry_plan_missing audit log, got none")
	}
}

// TestConfirm_ConcurrentFallback_NoDeadlock exercises the connection-pool fix:
// with MaxOpenConns=25, before the fix 25 concurrent fallback Confirm calls
// would deadlock (each holding a tx connection while trying to grab a second
// for planRepo.FindByID). With the fix, all 25 share the tx connection for
// the plan lookup, so they complete.
func TestConfirm_ConcurrentFallback_NoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	db := setupPaymentDB(t)
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	s := newTestPaymentService(t, db)

	const n = 25
	userIDs := make([]string, n)
	orderIDs := make([]string, n)
	for i := 0; i < n; i++ {
		userIDs[i] = seedUser(t, db)
		orderIDs[i] = seedPaidOrder(t, db, userIDs[i], "monthly", 19.9)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := s.Confirm(ctx, ConfirmInput{
				OrderID:       orderIDs[i],
				UserID:        userIDs[i],
				Channel:       "wechat_pay",
				ExternalTxnID: fmt.Sprintf("txn-conc-%d", i),
				ExpiresAt:     nil,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Confirm failed: %v", err)
		}
	}
}

// TestConfirm_ConcurrentPaymentDedupe_NoDeadlock exercises the connection
// pool fix on the payment dedupe path: pre-fix s.paymentRepo.FindByChannelTxnID
// would grab a second connection while inside the tx. With 25 concurrent
// retry Confirms (each hitting dedupe on its own (channel, txn_id)), 25
// tx connections + 25 second-connection grabs could deadlock. With the
// fix, dedupe reuses the tx connection via FindByChannelTxnIDTx.
//
// Each goroutine runs two Confirms back-to-back against the same
// (channel, external_txn_id): the first inserts the payment row, the
// second hits the !inserted dedupe branch — which is the read we care
// about. Without the tx-bound variant this would deadlock under
// MaxOpenConns=25 just like the resolveSubExpiry fallback path.
func TestConfirm_ConcurrentPaymentDedupe_NoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	db := setupPaymentDB(t)
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	s := newTestPaymentService(t, db)

	const n = 25
	userIDs := make([]string, n)
	orderIDs := make([]string, n)
	for i := 0; i < n; i++ {
		userIDs[i] = seedUser(t, db)
		orderIDs[i] = seedPaidOrder(t, db, userIDs[i], "monthly", 19.9)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// First Confirm (fresh insert, exercises the !inserted=false path).
			if _, err := s.Confirm(ctx, ConfirmInput{
				OrderID:       orderIDs[i],
				UserID:        userIDs[i],
				Channel:       "wechat_pay",
				ExternalTxnID: fmt.Sprintf("txn-dedupe-%d", i),
				ExpiresAt:     nil,
			}); err != nil {
				errs <- fmt.Errorf("first: %w", err)
				return
			}
			// Second Confirm (dedupe hit — re-read inside the tx).
			// This is the call site where FindByChannelTxnIDTx replaces
			// FindByChannelTxnID. Under MaxOpenConns=25, the pre-fix
			// version would deadlock here; the post-fix version
			// completes deterministically.
			if _, err := s.Confirm(ctx, ConfirmInput{
				OrderID:       orderIDs[i],
				UserID:        userIDs[i],
				Channel:       "wechat_pay",
				ExternalTxnID: fmt.Sprintf("txn-dedupe-%d", i),
				ExpiresAt:     nil,
			}); err != nil {
				errs <- fmt.Errorf("second: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent dedupe Confirm failed: %v", err)
		}
	}
}
