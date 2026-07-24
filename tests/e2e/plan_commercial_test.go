package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_PlanCommercial_CreateWithNewFields is the Phase 1 spec §10.2
// "all seven new fields round-trip through POST/GET/PATCH" guard. The seven
// new fields (added in migration 012) are: is_listed,
// accepting_new_subscriptions, currency, trial_days, description,
// display_order, updated_at. We exercise POST + GET to confirm the initial
// round-trip, then PATCH a couple of the new commercial fields and verify
// via a follow-up GET that the update took effect.
func TestE2E_PlanCommercial_CreateWithNewFields(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	hdrs := appAuthHeaders(superAppID)

	suffix := randomSuffix()
	planID := "e2e-comm-" + suffix
	createBody := `{
		"id": "` + planID + `",
		"name": "Commercial Test",
		"price": 19.9,
		"interval_days": 30,
		"apps": ["yundian"],
		"currency": "USD",
		"is_listed": true,
		"accepting_new_subscriptions": true,
		"trial_days": 7,
		"description": "E2E plan with all 7 new fields",
		"display_order": 42
	}`

	// POST /admin/plans.
	resp := doRequest(t, engine, http.MethodPost, "/admin/plans", createBody, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create plan: status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}
	var created struct {
		Data struct {
			ID                        string   `json:"id"`
			Currency                  string   `json:"currency"`
			IsListed                  bool     `json:"is_listed"`
			AcceptingNewSubscriptions bool     `json:"accepting_new_subscriptions"`
			TrialDays                 int      `json:"trial_days"`
			Description               *string  `json:"description"`
			DisplayOrder              int      `json:"display_order"`
			UpdatedAt                 string   `json:"updated_at"`
			Apps                      []string `json:"apps"`
		} `json:"data"`
	}
	resp.JSON(t, &created)
	if created.Data.ID != planID {
		t.Errorf("create returned id = %q, want %q", created.Data.ID, planID)
	}
	if created.Data.Currency != "USD" {
		t.Errorf("create returned currency = %q, want USD", created.Data.Currency)
	}
	if !created.Data.IsListed {
		t.Error("create returned is_listed = false, want true")
	}
	if !created.Data.AcceptingNewSubscriptions {
		t.Error("create returned accepting_new_subscriptions = false, want true")
	}
	if created.Data.TrialDays != 7 {
		t.Errorf("create returned trial_days = %d, want 7", created.Data.TrialDays)
	}
	if created.Data.Description == nil || *created.Data.Description != "E2E plan with all 7 new fields" {
		t.Errorf("create returned description = %v, want %q", created.Data.Description, "E2E plan with all 7 new fields")
	}
	if created.Data.DisplayOrder != 42 {
		t.Errorf("create returned display_order = %d, want 42", created.Data.DisplayOrder)
	}
	if created.Data.UpdatedAt == "" {
		t.Error("create returned empty updated_at; trigger should set now() on insert")
	}

	// GET /admin/plans/:id and assert the same 7 fields round-trip.
	getResp := doRequest(t, engine, http.MethodGet, "/admin/plans/"+planID, "", hdrs)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get plan: status = %d, body = %s", getResp.StatusCode, string(getResp.Body))
	}
	var fetched struct {
		Data struct {
			ID                        string   `json:"id"`
			Currency                  string   `json:"currency"`
			IsListed                  bool     `json:"is_listed"`
			AcceptingNewSubscriptions bool     `json:"accepting_new_subscriptions"`
			TrialDays                 int      `json:"trial_days"`
			Description               *string  `json:"description"`
			DisplayOrder              int      `json:"display_order"`
			UpdatedAt                 string   `json:"updated_at"`
			Apps                      []string `json:"apps"`
		} `json:"data"`
	}
	getResp.JSON(t, &fetched)
	if fetched.Data.ID != planID {
		t.Errorf("get id = %q, want %q", fetched.Data.ID, planID)
	}
	if fetched.Data.Currency != "USD" {
		t.Errorf("get currency = %q, want USD", fetched.Data.Currency)
	}
	if !fetched.Data.IsListed || !fetched.Data.AcceptingNewSubscriptions {
		t.Errorf("get flags = (listed=%v, accepting=%v), want both true", fetched.Data.IsListed, fetched.Data.AcceptingNewSubscriptions)
	}
	if fetched.Data.TrialDays != 7 {
		t.Errorf("get trial_days = %d, want 7", fetched.Data.TrialDays)
	}
	if fetched.Data.Description == nil || *fetched.Data.Description != "E2E plan with all 7 new fields" {
		t.Errorf("get description = %v", fetched.Data.Description)
	}
	if fetched.Data.DisplayOrder != 42 {
		t.Errorf("get display_order = %d, want 42", fetched.Data.DisplayOrder)
	}
	if fetched.Data.UpdatedAt == "" {
		t.Error("get updated_at empty after GET; updated_at trigger should have run on POST")
	}
	if len(fetched.Data.Apps) != 1 || fetched.Data.Apps[0] != "yundian" {
		t.Errorf("get apps = %v, want [yundian]", fetched.Data.Apps)
	}

	// PATCH /admin/plans/:id — flip a few of the new commercial fields and
	// verify via a follow-up GET. Spec §10.2 requires PATCH coverage for the
	// round-trip; mirror the body shape used in
	// TestPlanHandler_Patch_AcceptsNewFields (internal/handler/handler_test.go).
	const patchedDescription = "PATCHED — E2E plan with all 7 new fields"
	patchBody := `{
		"trial_days": 14,
		"description": "` + patchedDescription + `",
		"display_order": 99
	}`
	patchResp := doRequest(t, engine, http.MethodPatch, "/admin/plans/"+planID, patchBody, hdrs)
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch plan: status = %d, body = %s", patchResp.StatusCode, string(patchResp.Body))
	}

	// Follow-up GET asserts the PATCH took effect for the new commercial
	// fields. Other fields (currency, is_listed, accepting_new_subscriptions)
	// already had full coverage on the initial GET above.
	getResp2 := doRequest(t, engine, http.MethodGet, "/admin/plans/"+planID, "", hdrs)
	if getResp2.StatusCode != http.StatusOK {
		t.Fatalf("get plan after patch: status = %d, body = %s", getResp2.StatusCode, string(getResp2.Body))
	}
	var fetched2 struct {
		Data struct {
			TrialDays    int     `json:"trial_days"`
			Description  *string `json:"description"`
			DisplayOrder int     `json:"display_order"`
			UpdatedAt    string  `json:"updated_at"`
		} `json:"data"`
	}
	getResp2.JSON(t, &fetched2)
	if fetched2.Data.TrialDays != 14 {
		t.Errorf("get-after-patch trial_days = %d, want 14", fetched2.Data.TrialDays)
	}
	if fetched2.Data.Description == nil || *fetched2.Data.Description != patchedDescription {
		t.Errorf("get-after-patch description = %v, want %q", fetched2.Data.Description, patchedDescription)
	}
	if fetched2.Data.DisplayOrder != 99 {
		t.Errorf("get-after-patch display_order = %d, want 99", fetched2.Data.DisplayOrder)
	}
	if fetched2.Data.UpdatedAt == "" || fetched2.Data.UpdatedAt == fetched.Data.UpdatedAt {
		t.Errorf("get-after-patch updated_at = %q, want new non-empty value (was %q)", fetched2.Data.UpdatedAt, fetched.Data.UpdatedAt)
	}
}

// TestE2E_PlanCommercial_AppsValidation covers spec §10.2
// "POST /admin/plans with apps=['nope'] → 400" — exercises the
// PlanService.ValidateApps guard that wraps ErrInvalidAppID.
func TestE2E_PlanCommercial_AppsValidation(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	hdrs := appAuthHeaders(superAppID)

	planID := "e2e-badapps-" + randomSuffix()
	body := `{
		"id": "` + planID + `",
		"name": "Bad Apps",
		"price": 1.0,
		"interval_days": 30,
		"apps": ["nonexistent"]
	}`

	resp := doRequest(t, engine, http.MethodPost, "/admin/plans", body, hdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown app_id, got %d; body = %s", resp.StatusCode, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "unknown or inactive app_id") {
		t.Errorf("expected message to mention ErrInvalidAppID, got body: %s", string(resp.Body))
	}
}

// TestE2E_PlanCommercial_QuarterlyNotAcceptingNew covers spec §10.2
// "POST /user/subscriptions with quarterly → 409". The fixture seeds
// the `quarterly` plan with accepting_new_subscriptions=false (matching
// the production behavior the cn-staging rollout established). The
// SubscriptionService validation order is: IsActive →
// AcceptingNewSubscriptions → Price > 0 → active-sub check, so the
// 409 is reached regardless of the user's existing-sub state.
func TestE2E_PlanCommercial_QuarterlyNotAcceptingNew(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	// Seed the quarterly plan with accepting_new_subscriptions=false. We
	// seed inline (rather than reusing e2eMustSeedExpiredSubUser) because
	// that helper also creates a user+sub, which we don't need here — the
	// subscription creation path hits the validation guard before the
	// active-sub check.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active, is_default,
			is_listed, accepting_new_subscriptions, currency, trial_days,
			description, display_order
		) VALUES (
			'quarterly', '按季订阅', 79.9, 90, ARRAY['yundian','yundash'], true, false,
			true, false, 'CNY', 0, 'Quarterly test fixture (not accepting new)', 20
		) ON CONFLICT (id) DO UPDATE SET
			accepting_new_subscriptions = EXCLUDED.accepting_new_subscriptions,
			is_active = EXCLUDED.is_active`); err != nil {
		t.Fatalf("seed quarterly plan: %v", err)
	}

	// /test/login?plan_id=monthly mints a JWT for a fresh user; the
	// monthly plan exists, is active, and is accepting new subs, so this
	// call succeeds. The user is unbound (no subscription row written).
	tok := loginAndGetTokens(t, engine, "e2e-quarterly-"+randomSuffix(), "yundian").AccessToken

	resp := doRequest(t, engine, http.MethodPost, "/user/subscriptions",
		`{"plan_id":"quarterly"}`, authHeader(tok))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for not-accepting-new plan, got %d; body = %s", resp.StatusCode, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "plan is not accepting new subscriptions") {
		t.Errorf("expected message 'plan is not accepting new subscriptions', got body: %s", string(resp.Body))
	}
}

// TestE2E_PlanCommercial_OrderCurrencyMismatch covers spec §10.2
// "Plan CNY + channel USD → 400". The paypal channel requires USD
// (channelRequiredCurrency[paypal] = "USD" in internal/service/payment.go);
// the seeded `monthly` plan is CNY. CreateOrder must reject the request
// with ErrPlanCurrencyMismatch → 400, before any DB write.
func TestE2E_PlanCommercial_OrderCurrencyMismatch(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// /test/login?plan_id=monthly mints a JWT for a fresh user. monthly
	// is the plan-under-test (CNY). The user has no active subscription
	// — CreateOrder's active-sub check is downstream of the currency
	// check, so we never reach it on the 400 path.
	tok := loginAndGetTokens(t, engine, "e2e-curmis-"+randomSuffix(), "yundian").AccessToken

	resp := doRequest(t, engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly","channel":"paypal"}`, authHeader(tok))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for currency mismatch, got %d; body = %s", resp.StatusCode, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "currency") {
		t.Errorf("expected message to mention 'currency', got body: %s", string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "does not match") {
		t.Errorf("expected message to mention 'does not match', got body: %s", string(resp.Body))
	}
}
