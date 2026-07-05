package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, map[string]string{"X-App-ID": "yundian"})
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
	if got.ProviderIDs["lemonsqueezy"] != "var-M" {
		t.Errorf("ls id = %q, want var-M", got.ProviderIDs["lemonsqueezy"])
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