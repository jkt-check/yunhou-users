package model

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestUserJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal user", func(t *testing.T) {
		nickname := "testuser"
		email := "test@example.com"
		user := &User{
			ID:        "user-123",
			Nickname:  &nickname,
			Email:     &email,
			Status:    "active",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}

		data, err := json.Marshal(user)
		if err != nil {
			t.Fatalf("marshal user: %v", err)
		}

		var unmarshaled User
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal user: %v", err)
		}

		if unmarshaled.ID != user.ID {
			t.Errorf("expected ID %s, got %s", user.ID, unmarshaled.ID)
		}
		if *unmarshaled.Nickname != nickname {
			t.Errorf("expected nickname %s, got %s", nickname, *unmarshaled.Nickname)
		}
	})

	t.Run("unmarshal user from JSON", func(t *testing.T) {
		data := `{"id":"user-456","nickname":"john","email":"john@test.com","status":"active"}`

		var user User
		if err := json.Unmarshal([]byte(data), &user); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if user.ID != "user-456" {
			t.Errorf("expected ID user-456, got %s", user.ID)
		}
	})
}

func TestAppJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal app", func(t *testing.T) {
		app := &App{
			AppID:       "yundian",
			Name:        "Yundian App",
			Description: "A test app",
			IsActive:    true,
		}

		data, err := json.Marshal(app)
		if err != nil {
			t.Fatalf("marshal app: %v", err)
		}

		var unmarshaled App
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal app: %v", err)
		}

		if unmarshaled.AppID != app.AppID {
			t.Errorf("expected AppID %s, got %s", app.AppID, unmarshaled.AppID)
		}
	})

	t.Run("marshal app with config", func(t *testing.T) {
		app := &App{
			AppID:       "yundash",
			Name:        "Yundash App",
			Description: "Dashboard app",
			Config:      json.RawMessage(`{"theme":"dark"}`),
			IsActive:    true,
		}

		data, err := json.Marshal(app)
		if err != nil {
			t.Fatalf("marshal app: %v", err)
		}

		var unmarshaled App
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal app: %v", err)
		}

		if string(unmarshaled.Config) != `{"theme":"dark"}` {
			t.Errorf("expected config %s, got %s", `{"theme":"dark"}`, string(unmarshaled.Config))
		}
	})
}

func TestPlanJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal plan with apps", func(t *testing.T) {
		plan := &Plan{
			ID:           "monthly",
			Name:         "Monthly Plan",
			Price:        29.90,
			IntervalDays: 30,
			Apps:         []string{"yundian", "yundash"},
			IsActive:     true,
		}

		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}

		var unmarshaled Plan
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal plan: %v", err)
		}

		if len(unmarshaled.Apps) != 2 {
			t.Errorf("expected 2 apps, got %d", len(unmarshaled.Apps))
		}
	})

	t.Run("plan with empty apps", func(t *testing.T) {
		plan := &Plan{
			ID:       "free",
			Name:     "Free Plan",
			Apps:     []string{},
			IsActive: true,
		}

		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}

		var unmarshaled Plan
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal plan: %v", err)
		}

		if len(unmarshaled.Apps) != 0 {
			t.Errorf("expected 0 apps, got %d", len(unmarshaled.Apps))
		}
	})
}

func TestPlanCommercialModelShape(t *testing.T) {
	t.Parallel()

	planType := reflect.TypeOf(Plan{})
	wantPlanFields := []struct {
		name string
		db   string
		json string
	}{
		{"ID", "id", "id"},
		{"Name", "name", "name"},
		{"Price", "price", "price"},
		{"IntervalDays", "interval_days", "interval_days"},
		{"Apps", "apps", "apps"},
		{"IsActive", "is_active", "is_active"},
		{"IsListed", "is_listed", "is_listed"},
		{"AcceptingNewSubscriptions", "accepting_new_subscriptions", "accepting_new_subscriptions"},
		{"Currency", "currency", "currency"},
		{"TrialDays", "trial_days", "trial_days"},
		{"Description", "description", "description"},
		{"DisplayOrder", "display_order", "display_order"},
		{"UpdatedAt", "updated_at", "updated_at"},
		{"CreatedAt", "created_at", "created_at"},
	}
	if planType.NumField() != len(wantPlanFields) {
		t.Fatalf("Plan field count = %d, want %d", planType.NumField(), len(wantPlanFields))
	}
	for i, want := range wantPlanFields {
		got := planType.Field(i)
		if got.Name != want.name || got.Tag.Get("db") != want.db || got.Tag.Get("json") != want.json {
			t.Errorf("Plan field %d = %s db:%q json:%q, want %s db:%q json:%q",
				i, got.Name, got.Tag.Get("db"), got.Tag.Get("json"), want.name, want.db, want.json)
		}
	}

	publicType := reflect.TypeOf(PublicPlan{})
	wantPublicFields := []struct {
		name string
		json string
	}{
		{"ID", "id"},
		{"Name", "name"},
		{"Price", "price"},
		{"IntervalDays", "interval_days"},
		{"Currency", "currency"},
		{"TrialDays", "trial_days"},
		{"Description", "description"},
		{"Apps", "apps"},
		{"DisplayOrder", "display_order"},
		{"IsListed", "is_listed"},
		{"ProviderIDs", "provider_ids"},
		{"Cycle", "cycle"},
	}
	if publicType.NumField() != len(wantPublicFields) {
		t.Fatalf("PublicPlan field count = %d, want %d", publicType.NumField(), len(wantPublicFields))
	}
	for i, want := range wantPublicFields {
		got := publicType.Field(i)
		if got.Name != want.name || got.Tag.Get("json") != want.json {
			t.Errorf("PublicPlan field %d = %s json:%q, want %s json:%q",
				i, got.Name, got.Tag.Get("json"), want.name, want.json)
		}
	}
}

func TestSubscriptionJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal subscription", func(t *testing.T) {
		expiresAt := time.Now().Add(30 * 24 * time.Hour)
		sub := &Subscription{
			ID:        "sub-123",
			UserID:    "user-456",
			PlanID:    "monthly",
			Status:    "active",
			StartedAt: time.Now(),
			ExpiresAt: &expiresAt,
		}

		data, err := json.Marshal(sub)
		if err != nil {
			t.Fatalf("marshal subscription: %v", err)
		}

		var unmarshaled Subscription
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal subscription: %v", err)
		}

		if unmarshaled.ID != sub.ID {
			t.Errorf("expected ID %s, got %s", sub.ID, unmarshaled.ID)
		}
		if unmarshaled.PlanID != sub.PlanID {
			t.Errorf("expected PlanID %s, got %s", sub.PlanID, unmarshaled.PlanID)
		}
	})

	t.Run("subscription without expires_at", func(t *testing.T) {
		sub := &Subscription{
			ID:        "sub-789",
			UserID:    "user-123",
			PlanID:    "free",
			Status:    "active",
			StartedAt: time.Now(),
			ExpiresAt: nil, // never expires
		}

		data, err := json.Marshal(sub)
		if err != nil {
			t.Fatalf("marshal subscription: %v", err)
		}

		var unmarshaled Subscription
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal subscription: %v", err)
		}

		if unmarshaled.ExpiresAt != nil {
			t.Error("expected nil expires_at")
		}
	})
}

func TestSocialIdentityJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal social identity", func(t *testing.T) {
		email := "test@example.com"
		ident := &SocialIdentity{
			ID:          "ident-123",
			UserID:      "user-456",
			Provider:    "github",
			ProviderUID: "gh_12345",
			Email:       &email,
		}

		data, err := json.Marshal(ident)
		if err != nil {
			t.Fatalf("marshal identity: %v", err)
		}

		var unmarshaled SocialIdentity
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal identity: %v", err)
		}

		if unmarshaled.Provider != "github" {
			t.Errorf("expected provider github, got %s", unmarshaled.Provider)
		}
	})
}

func TestSessionJSON(t *testing.T) {
	t.Parallel()

	t.Run("marshal session", func(t *testing.T) {
		session := &Session{
			ID:          "sess-123",
			UserID:      "user-456",
			AppID:       "yundian",
			SessionType: "refresh", // not exported (json:"-")
			Scope:       []string{"yundian", "yundash"},
			Revoked:     false,
			ExpiresAt:   time.Now().Add(7 * 24 * time.Hour),
		}

		data, err := json.Marshal(session)
		if err != nil {
			t.Fatalf("marshal session: %v", err)
		}

		var unmarshaled Session
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal session: %v", err)
		}

		// SessionType is not exported via JSON (json:"-")
		if unmarshaled.Revoked != false {
			t.Errorf("expected revoked false, got %v", unmarshaled.Revoked)
		}
		if len(unmarshaled.Scope) != 2 {
			t.Errorf("expected 2 scope items, got %d", len(unmarshaled.Scope))
		}
	})
}
