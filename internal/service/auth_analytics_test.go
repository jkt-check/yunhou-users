package service

import (
	"context"
	"sync"
	"testing"

	"github.com/yunhou/users/internal/analytics"
	"github.com/yunhou/users/internal/model"
)

// recordingAnalytics is the test double for the AnalyticsEmitter
// consumption-point interface: it records every event instead of doing HTTP.
type recordingAnalytics struct {
	mu   sync.Mutex
	env  string
	evts []analytics.Event
}

func (r *recordingAnalytics) Capture(evt analytics.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = append(r.evts, evt)
}

func (r *recordingAnalytics) Environment() string { return r.env }

func (r *recordingAnalytics) events() []analytics.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]analytics.Event(nil), r.evts...)
}

func seedTrialPlanForAnalytics(pr *mockPlanRepo) {
	pr.plans["trial"] = &model.Plan{
		ID: "trial", Name: "Free Trial", Apps: []string{"yundian", "yundash"},
		IsActive: true, AcceptingNewSubscriptions: false, TrialDays: 7,
	}
}

func TestAuthService_Analytics_SignupCompleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("new github user emits exactly one signup_completed", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlanForAnalytics(pr)
		rec := &recordingAnalytics{env: "production"}
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		svc.SetAnalytics(rec)

		resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-signup-1", Email: "s@x.com"},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("LoginWithProfile: %v", err)
		}

		var signups []analytics.Event
		for _, e := range rec.events() {
			if e.Name == "signup_completed" {
				signups = append(signups, e)
			}
		}
		if len(signups) != 1 {
			t.Fatalf("signup_completed count = %d, want 1 (all events: %+v)", len(signups), rec.events())
		}
		evt := signups[0]
		if evt.DistinctID != resp.User.ID {
			t.Errorf("distinct_id = %q, want new user id %q", evt.DistinctID, resp.User.ID)
		}
		if evt.UUID != analytics.EventUUID("signup:"+resp.User.ID) {
			t.Errorf("uuid = %q, want EventUUID(signup:<userID>)", evt.UUID)
		}
		for k, want := range map[string]any{
			"region":        "intl",
			"auth_provider": "github",
			"app_id":        "yundian",
			"environment":   "production",
		} {
			if evt.Properties[k] != want {
				t.Errorf("properties[%q] = %v, want %v", k, evt.Properties[k], want)
			}
		}
		if evt.Timestamp.IsZero() {
			t.Error("timestamp must be set to now")
		}
	})

	t.Run("existing user login emits no signup event", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		ur.users["user-old"] = &model.User{ID: "user-old", Status: "active"}
		sir.identities["github:gh-old"] = &model.SocialIdentity{
			ID: "ident-old", UserID: "user-old", Provider: "github", ProviderUID: "gh-old",
		}
		rec := &recordingAnalytics{env: "production"}
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		svc.SetAnalytics(rec)

		if _, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-old"},
			AppID:   "yundian",
		}); err != nil {
			t.Fatalf("LoginWithProfile: %v", err)
		}
		for _, e := range rec.events() {
			if e.Name == "signup_completed" {
				t.Fatalf("existing user emitted signup_completed: %+v", e)
			}
		}
	})

	t.Run("wechat new user emits zero events (cn region)", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlanForAnalytics(pr)
		rec := &recordingAnalytics{env: "production"}
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		svc.SetAnalytics(rec)

		if _, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "wechat", ProviderUID: "wx-new-1"},
			AppID:   "yundian",
		}); err != nil {
			t.Fatalf("LoginWithProfile: %v", err)
		}
		if n := len(rec.events()); n != 0 {
			t.Fatalf("wechat login emitted %d events, want 0: %+v", n, rec.events())
		}
	})

	t.Run("nil emitter leaves the login flow unaffected", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlanForAnalytics(pr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		// No SetAnalytics call — the nil receiver must be safe.

		resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-noemit", Email: "n@x.com"},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("LoginWithProfile with nil emitter: %v", err)
		}
		if resp.AccessToken == "" {
			t.Error("expected access token")
		}
		if sr.byUserID[resp.User.ID] == nil {
			t.Error("trial grant must still happen with nil emitter")
		}
	})
}

func TestAuthService_Analytics_TrialStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("new github user emits trial_started matching the granted sub", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlanForAnalytics(pr)
		rec := &recordingAnalytics{env: "staging"}
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		svc.SetAnalytics(rec)

		resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-trial-evt", Email: "t@x.com"},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("LoginWithProfile: %v", err)
		}
		sub := sr.byUserID[resp.User.ID]
		if sub == nil {
			t.Fatal("expected a trial subscription row")
		}

		var trials []analytics.Event
		for _, e := range rec.events() {
			if e.Name == "trial_started" {
				trials = append(trials, e)
			}
		}
		if len(trials) != 1 {
			t.Fatalf("trial_started count = %d, want 1 (all events: %+v)", len(trials), rec.events())
		}
		evt := trials[0]
		if evt.DistinctID != resp.User.ID {
			t.Errorf("distinct_id = %q, want %q", evt.DistinctID, resp.User.ID)
		}
		// Idempotency key derives from the created subscription's ID.
		if evt.UUID != analytics.EventUUID("trial:"+sub.ID) {
			t.Errorf("uuid = %q, want EventUUID(trial:%s)", evt.UUID, sub.ID)
		}
		for k, want := range map[string]any{
			"region":           "intl",
			"plan_id":          sub.PlanID,
			"entitlement_type": "trial",
			"trial_days":       7,
			"environment":      "staging",
		} {
			if evt.Properties[k] != want {
				t.Errorf("properties[%q] = %v, want %v", k, evt.Properties[k], want)
			}
		}
		if _, hasSource := evt.Properties["source"]; hasSource {
			t.Error("source must be omitted (web vs desktop is unknowable)")
		}
	})

	t.Run("existing user gets no trial event", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		seedTrialPlanForAnalytics(pr)
		ur.users["user-old"] = &model.User{ID: "user-old", Status: "active"}
		sir.identities["github:gh-old"] = &model.SocialIdentity{
			ID: "ident-old", UserID: "user-old", Provider: "github", ProviderUID: "gh-old",
		}
		rec := &recordingAnalytics{env: "staging"}
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))
		svc.SetAnalytics(rec)

		if _, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-old"},
			AppID:   "yundian",
		}); err != nil {
			t.Fatalf("LoginWithProfile: %v", err)
		}
		for _, e := range rec.events() {
			if e.Name == "trial_started" {
				t.Fatalf("existing user emitted trial_started: %+v", e)
			}
		}
	})
}
