package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

func TestE2E_GetAppPlans_PublicCatalog(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// /apps/:id/plans is public — no X-App-ID, no JWT. seedTestData already
	// created the "yundian" super app and two plans (free, monthly).
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
	if len(body.Data) != 2 {
		t.Fatalf("got %d plans, want 2 (free+monthly); data = %+v", len(body.Data), body.Data)
	}
	// free plan: defaults (no provider config in seed data).
	if body.Data[0].ID != "free" || len(body.Data[0].ProviderIDs) != 0 {
		t.Errorf("free plan = %+v", body.Data[0])
	}
	// monthly: no provider config in seed data either, so provider_ids empty.
	if body.Data[1].ID != "monthly" || len(body.Data[1].ProviderIDs) != 0 {
		t.Errorf("monthly plan = %+v", body.Data[1])
	}
}

func TestE2E_GetAppPlans_WithProviderConfig(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	// Create an app with nested paypal + LS plan configs, then call the
	// public endpoint and verify both provider IDs surface.
	appID := "e2e-plans-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"Plans E2E","config":{"brand":{"name":"E2E Brand"},"payment_providers":{"paypal":{"client_id":"cid","client_secret":"cs","webhook_id":"W","mode":"sandbox","plans":{"mp":{"plan_id":"P-M","trial_days":7,"billing_cycle_days":30}}},"lemonsqueezy":{"api_key":"lsq_k","store_id":"12345","plans":{"mp":{"variant_id":"var-M","trial_days":0,"billing_cycle_days":30}}}}}}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, appAuthHeaders(superAppID))
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}
	// Add a plan that maps to this app so the listing has something to return.
	_, _ = db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active)
		 VALUES ('mp', 'Monthly Pro', 49.9, 30, ARRAY[$1], true)`, appID)

	resp := doRequest(t, engine, http.MethodGet, "/apps/"+appID+"/plans", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}
	var body struct {
		Code int                `json:"code"`
		Data []model.PublicPlan `json:"data"`
	}
	json.Unmarshal(resp.Body, &body)
	if len(body.Data) != 1 || body.Data[0].ID != "mp" {
		t.Fatalf("data = %+v, want [mp]", body.Data)
	}
	got := body.Data[0]
	if got.ProviderIDs["paypal"] != "P-M" {
		t.Errorf("paypal id = %q, want P-M", got.ProviderIDs["paypal"])
	}
	if got.Cycle == nil || got.Cycle.TrialDays != 7 || got.Cycle.BillingCycleDays != 30 {
		t.Errorf("cycle = %+v, want {7,30}", got.Cycle)
	}
}

func TestE2E_GetAppPlans_AppNotFound(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	resp := doRequest(t, engine, http.MethodGet, "/apps/no-such-app/plans", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_PostQuote_HappyPath(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	appID := "e2e-quote-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"Quote E2E","config":{"brand":{"name":"E2E Brand"},"payment_providers":{"paypal":{"client_id":"cid","client_secret":"cs","webhook_id":"W","mode":"sandbox","plans":{"qp":{"plan_id":"P-Q","trial_days":7,"billing_cycle_days":30}}},"lemonsqueezy":{"api_key":"lsq_k","store_id":"12345","plans":{"qp":{"variant_id":"var-Q","trial_days":0,"billing_cycle_days":30}}}}}}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, appAuthHeaders(superAppID))
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	// Seed a plan whose apps include our new app_id. setupE2EServer would
	// wipe the app we just created if called again, so we reuse the db handle.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active, is_listed,
			accepting_new_subscriptions, currency, trial_days, description,
			display_order
		) VALUES (
			'qp', 'Quote Plan', 29.9, 30, ARRAY[$1], true, true,
			true, 'USD', 7, 'Quote test fixture', 0
		)`, appID); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	token := loginAndGetTokens(t, engine, "e2e-quote-user-"+randomSuffix(), appID).AccessToken

	body := `{"plan_id":"qp"}`
	req, _ := http.NewRequest(http.MethodPost, "/apps/"+appID+"/quote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int          `json:"code"`
		Data *model.Quote `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Data == nil {
		t.Fatalf("nil data; body = %s", w.Body.String())
	}
	if resp.Data.PlanID != "qp" {
		t.Errorf("plan_id = %q", resp.Data.PlanID)
	}
	if resp.Data.Amount != 29.9 {
		t.Errorf("amount = %v, want 29.9", resp.Data.Amount)
	}
	if resp.Data.Currency != "USD" {
		t.Errorf("currency = %q", resp.Data.Currency)
	}
	if resp.Data.CycleConfig.TrialDays != 7 || resp.Data.CycleConfig.BillingCycleDays != 30 {
		t.Errorf("cycle_config = %+v, want {7,30}", resp.Data.CycleConfig)
	}
	delta := time.Until(resp.Data.SubExpiresAt)
	wantDelta := 37 * 24 * time.Hour
	if delta < wantDelta-time.Hour || delta > wantDelta+time.Hour {
		t.Errorf("sub_expires_at delta = %v, want ~%v", delta, wantDelta)
	}
	if _, ok := resp.Data.ProviderData["paypal"]; !ok {
		t.Error("provider_data missing paypal")
	}
}

func TestE2E_PostQuote_NoAuthReturns401(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	req, _ := http.NewRequest(http.MethodPost, "/apps/yundian/quote", strings.NewReader(`{"plan_id":"monthly"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
}

func TestE2E_PostQuote_PlanNotFound(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	token := loginAndGetTokens(t, engine, "e2e-quote-user-"+randomSuffix(), "yundian").AccessToken
	req, _ := http.NewRequest(http.MethodPost, "/apps/yundian/quote", strings.NewReader(`{"plan_id":"no-such-plan"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

func TestE2E_PostQuote_PlanAppMismatch(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	// monthly plan apps={yundian, yundash}; free plan apps={yundian}.
	// Requesting "free" plan for app yundash — free doesn't include yundash.
	token := loginAndGetTokens(t, engine, "e2e-mismatch-"+randomSuffix(), "yundash").AccessToken
	req, _ := http.NewRequest(http.MethodPost, "/apps/yundash/quote", strings.NewReader(`{"plan_id":"free"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}
