package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
)

// stubQuoteAppRepo is the AppRepo subset QuoteService needs.
type stubQuoteAppRepo struct {
	app *model.App
	err error
}

func (s *stubQuoteAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.app != nil && s.app.AppID == id {
		return s.app, nil
	}
	return nil, errors.New("not found")
}

func mustJSONRawQuote(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid test JSON: %s", s)
	}
	return json.RawMessage(s)
}

func TestQuote_Get_HappyPath_PayPalConfigured(t *testing.T) {
	plan := &model.Plan{
		ID: "monthly", Name: "Monthly", Price: 29.9, IntervalDays: 30,
		Apps: pq.StringArray{"yundian"}, IsActive: true,
	}
	app := &model.App{
		AppID: "yundian", Name: "Yundian", IsActive: true,
		Config: mustJSONRawQuote(t, `{
			"brand": {"name": "Yundian Brand"},
			"payment_providers": {
				"paypal": {"plans": {"monthly": {"plan_id": "P-1", "trial_days": 7, "billing_cycle_days": 30}}}
			}
		}`),
	}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: app})

	quote, err := svc.Get(context.Background(), "yundian", "monthly", "user-123")
	if err != nil {
		t.Fatal(err)
	}
	if quote.PlanID != "monthly" || quote.Amount != 29.9 || quote.Currency != "USD" {
		t.Errorf("basic fields = %+v", quote)
	}
	if quote.CycleConfig.TrialDays != 7 || quote.CycleConfig.BillingCycleDays != 30 {
		t.Errorf("cycle = %+v", quote.CycleConfig)
	}
	if quote.SubExpiresAt.IsZero() {
		t.Error("sub_expires_at not set")
	}
	// 7 + 30 = 37 days from now
	wantExpiry := time.Now().Add(37 * 24 * time.Hour)
	if delta := quote.SubExpiresAt.Sub(wantExpiry); delta > time.Minute || delta < -time.Minute {
		t.Errorf("sub_expires_at delta from now+37d = %v", delta)
	}
	pd, ok := quote.ProviderData["paypal"].(map[string]any)
	if !ok {
		t.Fatalf("paypal provider_data missing or wrong type: %+v", quote.ProviderData)
	}
	if pd["plan_id"] != "P-1" {
		t.Errorf("paypal.plan_id = %v", pd["plan_id"])
	}
}

func TestQuote_Get_PlanNotFound(t *testing.T) {
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{}}, &stubQuoteAppRepo{})
	_, err := svc.Get(context.Background(), "yundian", "missing", "u")
	if !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("err = %v, want ErrPlanNotFound", err)
	}
}

func TestQuote_Get_PlanInactive(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: false}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: &model.App{AppID: "yundian", IsActive: true}})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if !errors.Is(err, ErrPlanInactive) {
		t.Errorf("err = %v, want ErrPlanInactive", err)
	}
}

func TestQuote_Get_AppNotFound(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{err: sql.ErrNoRows})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

func TestQuote_Get_PlanAppMismatch(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"other-app"}}
	app := &model.App{AppID: "yundian", IsActive: true}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: app})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if !errors.Is(err, ErrPlanAppMismatch) {
		t.Errorf("err = %v, want ErrPlanAppMismatch", err)
	}
}

func TestQuote_Get_NoProviderConfigured_ReturnsEmptyProviderData(t *testing.T) {
	plan := &model.Plan{ID: "monthly", Price: 0, IntervalDays: 30, IsActive: true, Apps: pq.StringArray{"yundian"}}
	app := &model.App{AppID: "yundian", IsActive: true}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: app})

	quote, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(quote.ProviderData) != 0 {
		t.Errorf("expected empty provider_data, got %+v", quote.ProviderData)
	}
	// cycle_config still resolved from plan.interval_days as fallback
	if quote.CycleConfig.BillingCycleDays != 30 || quote.CycleConfig.TrialDays != 0 {
		t.Errorf("cycle = %+v, want {0,30}", quote.CycleConfig)
	}
}

// TestQuote_Get_AppInactive covers the "app found but disabled" branch
// (matches the handler's 403 / 404 mapping in the route).
func TestQuote_Get_AppInactive(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}
	app := &model.App{AppID: "yundian", IsActive: false}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: app})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if !errors.Is(err, ErrAppInactive) {
		t.Errorf("err = %v, want ErrAppInactive", err)
	}
}

// TestQuote_Get_AppFindGenericError covers the non-sql.ErrNoRows error
// path on the app lookup — surface as "find app: ...".
func TestQuote_Get_AppFindGenericError(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{err: errors.New("db down")})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "find app") {
		t.Errorf("expected wrap 'find app', got %q", err.Error())
	}
}

// TestQuote_Get_BadConfig covers the "app.config is unparseable JSON"
// branch. The function wraps the json error as "decode app config: ...".
func TestQuote_Get_BadConfig(t *testing.T) {
	plan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}
	// Bypass the mustJSONRawQuote validator — we WANT bad JSON here.
	app := &model.App{
		AppID: "yundian", IsActive: true,
		Config: json.RawMessage(`not-json`),
	}
	svc := NewQuoteService(&mockPlanRepo{plans: map[string]*model.Plan{"monthly": plan}}, &stubQuoteAppRepo{app: app})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if err == nil {
		t.Fatal("expected error from bad config, got nil")
	}
	if !strings.Contains(err.Error(), "decode app config") {
		t.Errorf("expected wrap 'decode app config', got %q", err.Error())
	}
}

// TestQuote_Get_GenericPlanFindError covers the wrap path on planRepo
// (a non-ErrNoRows error from the plan lookup).
func TestQuote_Get_GenericPlanFindError(t *testing.T) {
	pr := newMockPlanRepo()
	pr.err = errors.New("db down on plan lookup")
	svc := NewQuoteService(pr, &stubQuoteAppRepo{app: &model.App{AppID: "yundian"}})
	_, err := svc.Get(context.Background(), "yundian", "monthly", "u")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "find plan") {
		t.Errorf("expected wrap 'find plan', got %q", err.Error())
	}
}