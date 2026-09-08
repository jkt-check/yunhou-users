package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// ============================================================================
// Dual-product subscription isolation (migration 027 / design §4.1)
//
// Task 10 起 Coding Plan 开售需要支付配置（plan_benefit_configs，029）：这些
// 用例的 coding 套餐夹具随之补齐配置行，产品隔离断言原样保留。
// ============================================================================

// seedCodingPlans inserts one paid and one free coding-plan fixture, each
// with its payment/benefit configuration (mandatory for purchasability
// since migration 029).
func seedCodingPlans(t *testing.T, db *sqlx.DB) {
	t.Helper()
	ctx := context.Background()
	st := inferencepostgres.NewStore(db)
	pol := &inferencepostgres.PolicyVersion{
		Name: "dual-product-policy", Revision: 1, ModelIDs: []string{"glm-4.6"},
		MonthlyLimit: microcredits(100_000_000), Status: "published",
	}
	if err := st.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("seed policy version: %v", err)
	}
	for _, p := range []struct {
		id    string
		price float64
		days  int
	}{
		{"coding-monthly", 49.9, 30},
		{"coding-free", 0, 30},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
			VALUES ($1, $2, $3, $4, '{}', 'CNY', 'coding-plan')
			ON CONFLICT (id) DO NOTHING
		`, p.id, p.id, p.price, p.days); err != nil {
			t.Fatalf("seed coding plan %s: %v", p.id, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
			VALUES ($1, $2, '{glm-4.6}', 'subscription')
			ON CONFLICT (plan_id) DO NOTHING
		`, p.id, pol.ID); err != nil {
			t.Fatalf("seed coding benefit config %s: %v", p.id, err)
		}
	}
}

func newTestSubscriptionService(db *sqlx.DB) *SubscriptionService {
	planRepo := repo.NewPlanRepo(db)
	planSvc := NewPlanService(planRepo, repo.NewAppRepo(db), repo.NewPlanChangeLogRepo(db))
	return NewSubscriptionService(repo.NewSubscriptionRepo(db), planSvc)
}

// readSubExpiryForProduct returns (status, expires_at) of the user's
// subscription in one product, or ("", nil) when none exists.
func readSubForProduct(t *testing.T, db *sqlx.DB, uid, productCode string) (status string, expiresAt *time.Time) {
	t.Helper()
	row := db.QueryRowContext(context.Background(), `
		SELECT status, expires_at FROM subscriptions
		WHERE user_id = $1 AND product_code = $2
		ORDER BY created_at DESC LIMIT 1
	`, uid, productCode)
	var exp *time.Time
	if err := row.Scan(&status, &exp); err != nil {
		return "", nil
	}
	return status, exp
}

// TestDualProduct_PurchaseCodingPlanLeavesKayaUntouched drives the full
// order → webhook → activation pipeline for a coding-plan order on a user
// who already holds an active kaya-membership subscription. The kaya
// subscription's expiry must be byte-identical afterwards, and both
// products end up with an active row (the (user, product) unique index
// permits exactly this coexistence).
func TestDualProduct_PurchaseCodingPlanLeavesKayaUntouched(t *testing.T) {
	db := setupPaymentDB(t)
	s := newBenefitStack(t, db).svc
	seedCodingPlans(t, db)
	uid := seedUser(t, db)

	kayaExpiry := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	seedActiveSub(t, db, uid, "monthly", kayaExpiry)

	// Pre-027 this CreateOrder would have hit the global active-sub guard
	// (ErrUserHasActiveSub / ErrPlanDowngrade comparisons against the kaya
	// row). Product-scoped, the coding-plan order must be allowed.
	order, err := s.CreateOrder(context.Background(), uid, "coding-monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder coding-monthly with active kaya sub: %v", err)
	}

	e := WebhookEvent{
		Channel:       "stripe",
		EventID:       "evt-dual-1",
		EventType:     "payment_intent.succeeded",
		TransactionID: "txn-dual-1",
		OrderID:       order.ID,
		Amount:        49.9,
		Currency:      "CNY",
	}
	if err := s.onPaymentSucceeded(context.Background(), e); err != nil {
		t.Fatalf("onPaymentSucceeded: %v", err)
	}

	// Coding-plan sub: active, fresh 30d window, correct product stamp.
	codingStatus, codingExp := readSubForProduct(t, db, uid, model.ProductCodingPlan)
	if codingStatus != "active" {
		t.Fatalf("coding sub status = %q, want active", codingStatus)
	}
	if codingExp == nil || time.Until(*codingExp) < 29*24*time.Hour {
		t.Errorf("coding sub expiry = %v, want ~30d from now", codingExp)
	}

	// Kaya sub: still active, expiry untouched (no rollover leakage across
	// products — resolveSubExpiry only sees same-product rows now).
	kayaStatus, kayaExp := readSubForProduct(t, db, uid, model.ProductKayaMembership)
	if kayaStatus != "active" {
		t.Fatalf("kaya sub status = %q, want active", kayaStatus)
	}
	if kayaExp == nil || !kayaExp.UTC().Equal(kayaExpiry) {
		t.Errorf("kaya sub expiry changed: got %v, want %v", kayaExp, kayaExpiry)
	}

	// Both rows are visible unfiltered; the legacy kaya view sees exactly one.
	var total int
	if err := db.Get(&total,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1 AND status = 'active'`, uid); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if total != 2 {
		t.Errorf("active subs = %d, want 2 (kaya + coding)", total)
	}
}

// TestDualProduct_DuplicateActivationRejected pins the per-product
// uniqueness rule at the service layer: a second ACTIVE subscription in the
// SAME product is refused (ErrUserHasActiveSub), while the same user may
// still activate the OTHER product.
func TestDualProduct_DuplicateActivationRejected(t *testing.T) {
	db := setupPaymentDB(t)
	seedCodingPlans(t, db)
	uid := seedUser(t, db)
	subSvc := newTestSubscriptionService(db)

	// Coding-plan active sub constructed directly (no sale path exists).
	codingExp := time.Now().Add(30 * 24 * time.Hour)
	if err := repo.NewSubscriptionRepo(db).Create(context.Background(), &model.Subscription{
		ID: mustNewUUID(), UserID: uid, PlanID: "coding-free",
		ProductCode: model.ProductCodingPlan,
		Status:      "active", StartedAt: time.Now(), ExpiresAt: &codingExp,
	}); err != nil {
		t.Fatalf("construct coding sub: %v", err)
	}

	// Self-serve creation of a coding-plan subscription is refused outright
	// (Task 10 product guard — stronger than the previous per-product
	// uniqueness refusal, which still holds one layer down at the DB index).
	if _, err := subSvc.Create(context.Background(), uid, "coding-free", nil); !errors.Is(err, ErrSelfServiceProductForbidden) {
		t.Fatalf("self-serve coding-plan Create err = %v, want ErrSelfServiceProductForbidden", err)
	}

	// Other product (kaya free plan) → allowed.
	sub, err := subSvc.Create(context.Background(), uid, "free", nil)
	if err != nil {
		t.Fatalf("kaya free Create with active coding sub: %v", err)
	}
	if sub.ProductCode != model.ProductKayaMembership {
		t.Errorf("created sub ProductCode = %q, want kaya-membership", sub.ProductCode)
	}

	// And now the kaya product is also saturated.
	if _, err := subSvc.Create(context.Background(), uid, "free", nil); !errors.Is(err, ErrUserHasActiveSub) {
		t.Fatalf("second kaya Create err = %v, want ErrUserHasActiveSub", err)
	}
}

// TestDualProduct_CancelAndRefundAreProductScoped proves the destructive
// paths only touch their own product's row: cancelling the coding sub and
// full-refunding the coding payment leave the kaya subscription active
// with its original expiry.
func TestDualProduct_CancelAndRefundAreProductScoped(t *testing.T) {
	db := setupPaymentDB(t)
	s := newBenefitStack(t, db).svc
	seedCodingPlans(t, db)
	uid := seedUser(t, db)

	kayaExpiry := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	seedActiveSub(t, db, uid, "monthly", kayaExpiry)

	// Buy coding plan (same pipeline as the purchase test).
	order, err := s.CreateOrder(context.Background(), uid, "coding-monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := s.onPaymentSucceeded(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-dual-r1", EventType: "payment_intent.succeeded",
		TransactionID: "txn-dual-r1", OrderID: order.ID, Amount: 49.9, Currency: "CNY",
	}); err != nil {
		t.Fatalf("onPaymentSucceeded: %v", err)
	}

	// Cancel via the user-facing API shape (by id + owner check).
	subSvc := newTestSubscriptionService(db)
	var codingSubID string
	if err := db.Get(&codingSubID,
		`SELECT id FROM subscriptions WHERE user_id = $1 AND product_code = 'coding-plan' AND status = 'active'`, uid); err != nil {
		t.Fatalf("find coding sub: %v", err)
	}
	if err := subSvc.Cancel(context.Background(), codingSubID, uid); err != nil {
		t.Fatalf("Cancel coding sub: %v", err)
	}
	kayaStatus, kayaExp := readSubForProduct(t, db, uid, model.ProductKayaMembership)
	if kayaStatus != "active" || kayaExp == nil || !kayaExp.UTC().Equal(kayaExpiry) {
		t.Errorf("after cancel: kaya = (%q, %v), want (active, %v)", kayaStatus, kayaExp, kayaExpiry)
	}

	// Re-activate coding via a second order, then full-refund it: the refund
	// cascade must cancel ONLY the coding sub.
	order2, err := s.CreateOrder(context.Background(), uid, "coding-monthly", "stripe")
	if err != nil {
		t.Fatalf("CreateOrder 2: %v", err)
	}
	if err := s.onPaymentSucceeded(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-dual-r2", EventType: "payment_intent.succeeded",
		TransactionID: "txn-dual-r2", OrderID: order2.ID, Amount: 49.9, Currency: "CNY",
	}); err != nil {
		t.Fatalf("onPaymentSucceeded 2: %v", err)
	}
	if err := s.onRefundSucceeded(context.Background(), WebhookEvent{
		Channel: "stripe", EventID: "evt-dual-r3", EventType: "charge.refunded",
		TransactionID: "txn-dual-r2", ExternalRefundID: "re-dual-r1", RefundAmount: 49.9,
	}); err != nil {
		t.Fatalf("onRefundSucceeded: %v", err)
	}

	codingStatus, _ := readSubForProduct(t, db, uid, model.ProductCodingPlan)
	if codingStatus != "cancelled" {
		t.Errorf("after full refund: coding sub status = %q, want cancelled", codingStatus)
	}
	kayaStatus, kayaExp = readSubForProduct(t, db, uid, model.ProductKayaMembership)
	if kayaStatus != "active" || kayaExp == nil || !kayaExp.UTC().Equal(kayaExpiry) {
		t.Errorf("after full refund: kaya = (%q, %v), want (active, %v)", kayaStatus, kayaExp, kayaExpiry)
	}
}
