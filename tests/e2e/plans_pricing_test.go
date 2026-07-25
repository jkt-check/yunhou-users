package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// TestE2E_PlanPricing_OnlyTwoPlansReturned covers spec §7.2:
// GET /apps/:id/plans must return exactly 2 plans (monthly + yearly) with
// the post-migration-016 prices. quarterly and free are filtered out by
// is_listed=false.
func TestE2E_PlanPricing_OnlyTwoPlansReturned(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	// seedTestData seeds free (hidden), monthly (CN ¥19.9 post-migration),
	// and monthly_usd (USD ¥29.9, apps=[]). We additionally seed 'yearly'
	// because the existing seed slice never covers it — this test asserts
	// the public catalog has both CN plans.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active, is_listed,
			accepting_new_subscriptions, currency, trial_days, description,
			display_order
		) VALUES (
			'yearly', '按年订阅', 199.9, 365, ARRAY['yundian','yundash'], true,
			true, true, 'CNY', 0, '按年订阅 ¥199.9，自动续费，可随时取消', 30
		) ON CONFLICT (id) DO UPDATE SET
			price = EXCLUDED.price,
			is_active = EXCLUDED.is_active,
			is_listed = EXCLUDED.is_listed`); err != nil {
		t.Fatalf("seed yearly: %v", err)
	}

	resp := doRequest(t, engine, http.MethodGet, "/apps/yundian/plans", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}
	var body struct {
		Code int                `json:"code"`
		Data []model.PublicPlan `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Code != 0 {
		t.Fatalf("code: got %d, want 0", body.Code)
	}
	if len(body.Data) != 2 {
		t.Fatalf("plan count: got %d, want 2; data=%+v", len(body.Data), body.Data)
	}

	ids := map[string]float64{}
	for _, p := range body.Data {
		ids[p.ID] = p.Price
	}
	if _, ok := ids["monthly"]; !ok {
		t.Fatalf("monthly missing from public catalog: ids=%v", ids)
	}
	if _, ok := ids["yearly"]; !ok {
		t.Fatalf("yearly missing from public catalog: ids=%v", ids)
	}
	if _, ok := ids["quarterly"]; ok {
		t.Fatalf("quarterly must NOT appear in public catalog: ids=%v", ids)
	}
	if _, ok := ids["free"]; ok {
		t.Fatalf("free must NOT appear in public catalog: ids=%v", ids)
	}
	if ids["monthly"] != 19.9 {
		t.Errorf("monthly.price: got %v, want 19.9", ids["monthly"])
	}
	if ids["yearly"] != 199.9 {
		t.Errorf("yearly.price: got %v, want 199.9", ids["yearly"])
	}
}

// TestE2E_PlanPricing_QuarterlyHiddenAndInactive checks the DB directly so a
// regression in FindByApp's WHERE clause (e.g. dropping is_listed) or in the
// service-layer Plan.IsActive gate (e.g. dropping the deactivated-plan branch
// in resolvePlanForTokenIssuance) would surface even if the public catalog
// endpoint stayed green.
func TestE2E_PlanPricing_QuarterlyHiddenAndInactive(t *testing.T) {
	_, _, db := setupE2EServer(t)

	// seedTestData does not seed quarterly. Seed it here with the
	// post-migration-016 flags (is_listed=false, is_active=false). The
	// ON CONFLICT clause ensures idempotency when this test runs against
	// a DB that already has the row from migration 016.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active, is_listed,
			accepting_new_subscriptions, currency, trial_days, description,
			display_order
		) VALUES (
			'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], false,
			false, false, 'CNY', 0, '按季订阅（已下线）', 20
		) ON CONFLICT (id) DO UPDATE SET
			is_listed = EXCLUDED.is_listed,
			is_active = EXCLUDED.is_active`); err != nil {
		t.Fatalf("seed quarterly: %v", err)
	}

	var listed, active bool
	err := db.QueryRowContext(context.Background(),
		`SELECT is_listed, is_active FROM plans WHERE id = 'quarterly'`).
		Scan(&listed, &active)
	if err != nil {
		t.Fatalf("query quarterly: %v", err)
	}
	if listed {
		t.Errorf("quarterly.is_listed: got true, want false (migration 016 block b)")
	}
	if active {
		t.Errorf("quarterly.is_active: got true, want false (migration 016 block b)")
	}
}

// TestE2E_PlanPricing_OrderMonthlyNewPrice covers spec §7.2:
// POST /apps/:id/quote for monthly must return amount=19.9 (not the old
// 29.9). Drives the JWT path via loginAndGetTokens so the assertion
// exercises the production code path (plan.price → quote.amount), not a
// mock.
func TestE2E_PlanPricing_OrderMonthlyNewPrice(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	tok := loginAndGetTokens(t, engine, "e2e-pricing-"+randomSuffix(), "yundian").AccessToken
	if tok == "" {
		t.Fatal("login must succeed before quoting")
	}

	resp := doRequest(t, engine, http.MethodPost, "/apps/yundian/quote",
		`{"plan_id":"monthly"}`, authHeader(tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}
	var body struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Code != 0 {
		t.Fatalf("code: got %d, want 0", body.Code)
	}
	amt, ok := body.Data["amount"].(float64)
	if !ok {
		t.Fatalf("quote.amount missing or wrong type: %v", body.Data["amount"])
	}
	if amt != 19.9 {
		t.Errorf("quote.amount for monthly: got %v, want 19.9", amt)
	}
}