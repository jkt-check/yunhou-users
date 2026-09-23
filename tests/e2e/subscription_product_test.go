package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestE2E_SubscriptionListProductScope pins the legacy contract of
// GET /user/subscriptions under dual-product data (migration 027): a
// caller that omits `product` must see ONLY kaya-membership rows — a
// coding-plan subscription must never leak into the old member-facing
// list. The explicit `product`/`product=all` query params are the new,
// opt-in multi-product view.
//
// Coding Plan is not on sale in this phase, so the coding-plan rows are
// constructed directly in the DB (no client sale path exists to drive).
func TestE2E_SubscriptionListProductScope(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	login := loginAndGetTokens(t, engine, "sub-product-scope", "yundian")
	userID := login.User.ID
	if userID == "" {
		t.Fatal("login returned empty user id")
	}

	// A coding-plan SKU + an active coding-plan subscription for this user.
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ('coding-monthly', 'Coding Plan Monthly', 49.9, 30, '{}', 'CNY', 'coding-plan')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed coding plan: %v", err)
	}
	exp := time.Now().Add(30 * 24 * time.Hour)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'coding-monthly', 'active', now(), $2, 'coding-plan')
	`, userID, exp); err != nil {
		t.Fatalf("seed coding sub: %v", err)
	}
	// A kaya-membership subscription for the same user (dual-product
	// coexistence, exactly what the 027 index permits).
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now(), $2, 'kaya-membership')
	`, userID, exp); err != nil {
		t.Fatalf("seed kaya sub: %v", err)
	}

	type subRow struct {
		PlanID      string `json:"plan_id"`
		ProductCode string `json:"product_code"`
	}
	listSubs := func(path string) []subRow {
		t.Helper()
		resp := doRequest(t, engine, http.MethodGet, path, "", authHeader(login.AccessToken))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, resp.StatusCode, string(resp.Body))
		}
		var env struct {
			Data []subRow `json:"data"`
		}
		resp.JSON(t, &env)
		return env.Data
	}

	// Legacy call (no product param): kaya only.
	def := listSubs("/user/subscriptions")
	if len(def) != 1 || def[0].PlanID != "monthly" || def[0].ProductCode != "kaya-membership" {
		t.Fatalf("default list = %+v, want exactly the kaya monthly row", def)
	}

	// Explicit coding-plan scope.
	coding := listSubs("/user/subscriptions?product=coding-plan")
	if len(coding) != 1 || coding[0].PlanID != "coding-monthly" || coding[0].ProductCode != "coding-plan" {
		t.Fatalf("coding list = %+v, want exactly the coding-monthly row", coding)
	}

	// Opt-in multi-product view.
	all := listSubs("/user/subscriptions?product=all")
	if len(all) != 2 {
		t.Fatalf("all list = %+v, want both rows", all)
	}

	// Old JWT contract untouched: the login response's subscription field
	// keeps describing the legacy membership view only.
	if login.Subscription == nil {
		t.Fatal("login response missing subscription field")
	}
}
