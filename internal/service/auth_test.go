package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

func TestAuthService_Logout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("valid logout", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		pr := newMockPlanRepo()
		sr := newMockSubscriptionRepo()
		ssr := newMockSessionRepo()
		ar := newMockAppRepo()

		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		authSvc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		// Create a session first
		sess := &model.Session{
			ID:           "sess-1",
			UserID:       "user-1",
			AppID:        "yundian",
			SessionType:  "refresh",
			RefreshToken: hashToken("valid-refresh-token"),
			Revoked:      false,
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		ssr.sessions[sess.ID] = sess
		ssr.byToken[hashToken("valid-refresh-token")] = sess

		err := authSvc.Logout(ctx, "valid-refresh-token")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify session is revoked
		if !ssr.sessions["sess-1"].Revoked {
			t.Error("expected session to be revoked")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		pr := newMockPlanRepo()
		sr := newMockSubscriptionRepo()
		ssr := newMockSessionRepo()
		ar := newMockAppRepo()

		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		authSvc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		err := authSvc.Logout(ctx, "invalid-token")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestHashToken_Deterministic(t *testing.T) {
	t.Parallel()

	token := "test-token-123"
	h1 := hashToken(token)
	h2 := hashToken(token)
	if h1 != h2 {
		t.Errorf("hashToken is not deterministic: %q != %q", h1, h2)
	}

	other := "different-token"
	h3 := hashToken(other)
	if h1 == h3 {
		t.Error("different tokens should produce different hashes")
	}
}

func TestAuthService_RefreshToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	plans := map[string]*model.Plan{
		"free":    {ID: "free", Name: "免费", Apps: []string{"yundian"}},
		"monthly": {ID: "monthly", Name: "按月订阅", Apps: []string{"yundian", "yundash"}},
	}

	tests := []struct {
		name        string
		refreshToken string
		appID       string
		setup       func(*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo, *mockSubscriptionRepo, *mockSessionRepo, *mockAppRepo)
		wantErr     bool
		errContains string
		validate    func(t *testing.T, resp *LoginResponse)
	}{
		{
			name: "refresh with valid token",
			refreshToken: "valid-refresh-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]

				user := &model.User{ID: "user-1", Status: "active"}
				ur.users["user-1"] = user

				session := &model.Session{
					ID:           "sess-1",
					UserID:       "user-1",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("valid-refresh-token"),
					Scope:        []string{"yundian"},
					Revoked:      false,
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundian", "云店")
			},
			wantErr: false,
			validate: func(t *testing.T, resp *LoginResponse) {
				if resp.AccessToken == "" {
					t.Error("expected access token")
				}
				if resp.RefreshToken == "" {
					t.Error("expected refresh token")
				}
				if resp.Subscription.PlanID != "free" {
					t.Errorf("expected plan free, got %s", resp.Subscription.PlanID)
				}
			},
		},
		{
			name: "refresh with expired session token",
			refreshToken: "expired-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]

				user := &model.User{ID: "user-2", Status: "active"}
				ur.users["user-2"] = user

				session := &model.Session{
					ID:           "sess-2",
					UserID:       "user-2",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("expired-token"),
					Scope:        []string{"yundian"},
					Revoked:      false,
					ExpiresAt:    time.Now().Add(-time.Hour), // expired
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundian", "云店")
			},
			wantErr:     true,
			errContains: "invalid refresh token",
		},
		{
			name: "refresh with revoked token",
			refreshToken: "revoked-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]

				user := &model.User{ID: "user-3", Status: "active"}
				ur.users["user-3"] = user

				session := &model.Session{
					ID:           "sess-3",
					UserID:       "user-3",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("revoked-token"),
					Scope:        []string{"yundian"},
					Revoked:      true, // revoked
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundian", "云店")
			},
			wantErr:     true,
			errContains: "invalid refresh token",
		},
		{
			name: "refresh for paid app with subscription",
			refreshToken: "paid-user-token",
			appID:       "yundash",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]
				pr.plans["monthly"] = plans["monthly"]

				user := &model.User{ID: "user-paid", Status: "active"}
				ur.users["user-paid"] = user

				expiresAt := time.Now().Add(30 * 24 * time.Hour)
				sr.subs["sub-paid"] = &model.Subscription{
					ID:        "sub-paid",
					UserID:    "user-paid",
					PlanID:    "monthly",
					Status:    "active",
					ExpiresAt: &expiresAt,
				}
				sr.byUserID["user-paid"] = sr.subs["sub-paid"]

				session := &model.Session{
					ID:           "sess-paid",
					UserID:       "user-paid",
					AppID:        "yundash",
					SessionType:  "refresh",
					RefreshToken: hashToken("paid-user-token"),
					Scope:        []string{"yundian", "yundash"},
					Revoked:      false,
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundash", "云dash")
			},
			wantErr: false,
			validate: func(t *testing.T, resp *LoginResponse) {
				if resp.Subscription.PlanID != "monthly" {
					t.Errorf("expected plan monthly, got %s", resp.Subscription.PlanID)
				}
				if !resp.Subscription.HasAccess {
					t.Error("expected has_access=true for yundash on monthly plan")
				}
			},
		},
		{
			name: "refresh rejects suspended user",
			refreshToken: "suspended-user-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
				ur.users["user-susp"] = &model.User{ID: "user-susp", Status: "suspended"}
				session := &model.Session{
					ID:           "sess-susp",
					UserID:       "user-susp",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("suspended-user-token"),
					Revoked:      false,
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundian", "云店")
			},
			wantErr:     true,
			errContains: "suspended",
		},
		{
			name: "refresh rejects expired subscription",
			refreshToken: "expired-sub-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
				ur.users["user-x"] = &model.User{ID: "user-x", Status: "active"}
				pastExpiry := time.Now().Add(-time.Hour)
				sr.subs["sub-x"] = &model.Subscription{
					ID:        "sub-x",
					UserID:    "user-x",
					PlanID:    "free",
					Status:    "active",
					ExpiresAt: &pastExpiry,
				}
				sr.byUserID["user-x"] = sr.subs["sub-x"]
				session := &model.Session{
					ID:           "sess-x",
					UserID:       "user-x",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("expired-sub-token"),
					Revoked:      false,
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[session.ID] = session
				ssr.byToken[session.RefreshToken] = session
				ar.seedActive("yundian", "云店")
			},
			wantErr:     true,
			errContains: "expired",
		},
		{
			// Refresh-token reuse detection: rotating then replaying the
			// same refresh token must trigger the family-revoke response.
			// This is the security-critical test that would have caught
			// the earlier bug where RotateRefresh returned a plain error
			// that errors.Is could not match.
			name:        "refresh reuse revokes the family",
			refreshToken: "reuse-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo, ar *mockAppRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
				ur.users["user-reuse"] = &model.User{ID: "user-reuse", Status: "active"}
				// Two siblings in the (user, app) family so we can verify
				// both are revoked when reuse is detected.
				sibling := &model.Session{
					ID:           "sess-sibling",
					UserID:       "user-reuse",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("sibling-token"),
					Revoked:      false,
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				target := &model.Session{
					ID:           "sess-target",
					UserID:       "user-reuse",
					AppID:        "yundian",
					SessionType:  "refresh",
					RefreshToken: hashToken("reuse-token"),
					Revoked:      true, // already revoked by a prior rotation
					ExpiresAt:    time.Now().Add(time.Hour),
				}
				ssr.sessions[sibling.ID] = sibling
				ssr.sessions[target.ID] = target
				ssr.byToken[sibling.RefreshToken] = sibling
				ssr.byToken[target.RefreshToken] = target
				ar.seedActive("yundian", "云店")
			},
			wantErr:     true,
			errContains: "invalid refresh token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ur := newMockUserRepo()
			sir := newMockSocialIdentityRepo()
			pr := newMockPlanRepo()
			sr := newMockSubscriptionRepo()
			ssr := newMockSessionRepo()
			ar := newMockAppRepo()

			tc.setup(ur, sir, pr, sr, ssr, ar)

			tokenSvc := newTokenServiceWithMocks(ssr, sr)
			authSvc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

			resp, err := authSvc.RefreshToken(ctx, tc.refreshToken, tc.appID)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.validate != nil {
				tc.validate(t, resp)
			}
		})
	}
}

func TestGenerateRefreshToken_Unique(t *testing.T) {
	t.Parallel()

	tokens := make(map[string]bool)
	for i := 0; i < 100; i++ {
		tok, err := GenerateRefreshToken()
		if err != nil {
			t.Fatalf("GenerateRefreshToken: %v", err)
		}
		if tok == "" {
			t.Error("expected non-empty refresh token")
		}
		if tokens[tok] {
			t.Error("generated duplicate refresh token")
		}
		tokens[tok] = true
	}
}

// TestAuthService_RefreshToken_RarePaths fills in branches the table-driven
// TestAuthService_RefreshToken doesn't reach: deleted user, missing user
// row, appID fallback to session.AppID, ErrAppNotFound, ErrAppInactive,
// and a generic (non-ErrNoRows) session-lookup error.
func TestAuthService_RefreshToken_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plans := map[string]*model.Plan{
		"free": {ID: "free", Name: "免费", Apps: []string{"yundian"}},
	}

	// helper: build a session for a user with a given status.
	seed := func(t *testing.T, ur *mockUserRepo, ssr *mockSessionRepo, userID, status, token string) {
		t.Helper()
		ur.users[userID] = &model.User{ID: userID, Status: status}
		ssr.sessions["sess-"+userID] = &model.Session{
			ID:           "sess-" + userID,
			UserID:       userID,
			AppID:        "yundian",
			SessionType:  "refresh",
			RefreshToken: hashToken(token),
			Revoked:      false,
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		ssr.byToken[hashToken(token)] = ssr.sessions["sess-"+userID]
	}

	t.Run("deleted user is rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		seed(t, ur, ssr, "u-deleted", "deleted", "t-deleted")
		pr.plans["free"] = plans["free"]
		pr.defaultPlan = plans["free"]
		ar.seedActive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.RefreshToken(ctx, "t-deleted", "yundian")
		if !errors.Is(err, ErrUserDeleted) {
			t.Errorf("expected ErrUserDeleted, got %v", err)
		}
	})

	t.Run("missing user row → wrapped error", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		// Seed a session whose user_id is NOT in ur.users. The mock's
		// FindByID returns a generic "not found" (not sql.ErrNoRows), so
		// production code wraps it via the second branch
		// (fmt.Errorf("find user: %w", err)). We assert on the wrap.
		ssr.sessions["sess-orphan"] = &model.Session{
			ID: "sess-orphan", UserID: "u-orphan", AppID: "yundian",
			SessionType: "refresh", RefreshToken: hashToken("t-orphan"),
			Revoked: false, ExpiresAt: time.Now().Add(time.Hour),
		}
		ssr.byToken[hashToken("t-orphan")] = ssr.sessions["sess-orphan"]
		pr.plans["free"] = plans["free"]
		pr.defaultPlan = plans["free"]
		ar.seedActive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.RefreshToken(ctx, "t-orphan", "yundian")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "find user") {
			t.Errorf("expected wrap 'find user', got %q", err.Error())
		}
	})

	t.Run("appID empty falls back to session.AppID", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		seed(t, ur, ssr, "u-fb", "active", "t-fb")
		// Session is for yundian; the call passes appID="" so it must
		// fall back to yundian.
		pr.plans["free"] = plans["free"]
		pr.defaultPlan = plans["free"]
		ar.seedActive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		resp, err := svc.RefreshToken(ctx, "t-fb", "")
		if err != nil {
			t.Fatalf("expected success on appID fallback, got %v", err)
		}
		if resp.User.ID != "u-fb" {
			t.Errorf("user.ID = %q, want u-fb", resp.User.ID)
		}
	})

	t.Run("app not found (ErrAppNotFound)", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		seed(t, ur, ssr, "u-anf", "active", "t-anf")
		pr.plans["free"] = plans["free"]
		pr.defaultPlan = plans["free"]
		// No app seeded for "nonexistent" — FindByID returns sql.ErrNoRows.
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.RefreshToken(ctx, "t-anf", "nonexistent")
		if !errors.Is(err, ErrAppNotFound) {
			t.Errorf("expected ErrAppNotFound, got %v", err)
		}
	})

	t.Run("app inactive (ErrAppInactive)", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		seed(t, ur, ssr, "u-ai", "active", "t-ai")
		pr.plans["free"] = plans["free"]
		pr.defaultPlan = plans["free"]
		ar.seedInactive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.RefreshToken(ctx, "t-ai", "yundian")
		if !errors.Is(err, ErrAppInactive) {
			t.Errorf("expected ErrAppInactive, got %v", err)
		}
	})

	t.Run("session lookup generic error", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		// Inject a generic (non-ErrNoRows) error into the session lookup.
		ssr.findErr = errTest
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.RefreshToken(ctx, "t-anything", "yundian")
		if err == nil {
			t.Fatal("expected error from session lookup, got nil")
		}
		if !strings.Contains(err.Error(), "find session") {
			t.Errorf("expected wrap 'find session', got %q", err.Error())
		}
	})
}

// TestIsTestIdentityProviderUID guards the prefix filter that protects
// real OAuth logins from being merged into the dev-only /test/login
// identities. The filter is the defence-in-depth backstop in case the
// PAYPAL_L3_E2E_MODE env gate is set in production by mistake — without
// it, an attacker could pre-claim a victim's email as a synthetic github
// identity and absorb the victim's real GitHub login into their account.
func TestIsTestIdentityProviderUID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		uid  string
		want bool
	}{
		{"l3-e2e-deadbeef", true},
		{"l3-e2e-", true},
		{"github_12345", false},
		{"wechat_openid_abc", false},
		{"", false},
		{"l3-e2e", false}, // prefix must include the trailing dash
		{"L3-E2E-X", false}, // case-sensitive: GitHub IDs are case-different
	}
	for _, c := range cases {
		if got := isTestIdentityProviderUID(c.uid); got != c.want {
			t.Errorf("isTestIdentityProviderUID(%q) = %v, want %v", c.uid, got, c.want)
		}
	}
}

func TestAuthService_LoginWithProfile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}, IsDefault: true}

	t.Run("nil profile rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{AppID: "yundian"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "nil profile") {
			t.Errorf("expected 'nil profile' error, got %q", err.Error())
		}
	})

	t.Run("app not found", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-1", Email: "a@b.com"},
			AppID:   "nonexistent",
		})
		if !errors.Is(err, ErrAppNotFound) {
			t.Errorf("expected ErrAppNotFound, got %v", err)
		}
	})

	t.Run("app inactive", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedInactive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-1", Email: "a@b.com"},
			AppID:   "yundian",
		})
		if !errors.Is(err, ErrAppInactive) {
			t.Errorf("expected ErrAppInactive, got %v", err)
		}
	})

	t.Run("suspended user rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Pre-existing identity pointing to a suspended user
		ur.users["user-susp"] = &model.User{ID: "user-susp", Status: "suspended"}
		sir.identities["github:gh-susp"] = &model.SocialIdentity{
			ID: "ident-susp", UserID: "user-susp",
			Provider: "github", ProviderUID: "gh-susp",
		}
		pr.defaultPlan = defaultPlan

		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-susp", Email: "x@y.com"},
			AppID:   "yundian",
		})
		if !errors.Is(err, ErrUserSuspended) {
			t.Errorf("expected ErrUserSuspended, got %v", err)
		}
	})

	t.Run("new user created and tokens issued", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-new", Email: "new@x.com"},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.AccessToken == "" {
			t.Error("expected access token")
		}
		if resp.RefreshToken == "" {
			t.Error("expected refresh token")
		}
		if resp.User.ID == "" {
			t.Error("expected user id in response")
		}
	})

	t.Run("existing user logged in via profile", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		// Pre-seed user + identity
		ur.users["user-1"] = &model.User{ID: "user-1", Status: "active"}
		email := "ex@x.com"
		sir.identities["github:gh-1"] = &model.SocialIdentity{
			ID: "ident-1", UserID: "user-1",
			Provider: "github", ProviderUID: "gh-1",
			Email: &email,
		}
		sir.byEmail["ex@x.com"] = []model.SocialIdentity{
			{ID: "ident-1", UserID: "user-1", Provider: "github", ProviderUID: "gh-1", Email: &email},
		}
		sir.byUserID["user-1"] = []model.SocialIdentity{
			{ID: "ident-1", UserID: "user-1", Provider: "github", ProviderUID: "gh-1", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-1", Email: "ex@x.com"},
			AppID:   "yundian",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.User.ID != "user-1" {
			t.Errorf("expected user-1, got %s", resp.User.ID)
		}
		if resp.User.Email == nil || *resp.User.Email != "ex@x.com" {
			t.Errorf("expected email ex@x.com in response, got %v", resp.User.Email)
		}
	})
}

func TestAuthService_TestLogin(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}, IsDefault: true}

	t.Run("app not found", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "x@y.com", AppID: "nonexistent"})
		if !errors.Is(err, ErrAppNotFound) {
			t.Errorf("expected ErrAppNotFound, got %v", err)
		}
	})

	t.Run("app inactive", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedInactive("yundian", "云店")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "x@y.com", AppID: "yundian"})
		if !errors.Is(err, ErrAppInactive) {
			t.Errorf("expected ErrAppInactive, got %v", err)
		}
	})

	t.Run("new user created with synthetic identity", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.TestLogin(ctx, TestLoginRequest{Email: "fresh@x.com", AppID: "yundian"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.AccessToken == "" {
			t.Error("expected access token")
		}
		if resp.User.ID == "" {
			t.Error("expected user id")
		}
		// Email should be populated in the response
		if resp.User.Email == nil || *resp.User.Email != "fresh@x.com" {
			t.Errorf("expected email fresh@x.com, got %v", resp.User.Email)
		}
		// A synthetic identity with the l3-e2e- prefix should exist
		found := false
		for _, si := range sir.identities {
			if si.ProviderUID == "l3-e2e-"+resp.User.ID {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected synthetic l3-e2e- identity to be created")
		}
	})

	t.Run("existing user by email is reused", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		// Pre-seed user + identity with same email
		ur.users["user-existing"] = &model.User{ID: "user-existing", Status: "active"}
		email := "ex@x.com"
		sir.identities["github:gh-existing"] = &model.SocialIdentity{
			ID: "ident-existing", UserID: "user-existing",
			Provider: "github", ProviderUID: "gh-existing",
			Email: &email,
		}
		sir.byEmail["ex@x.com"] = []model.SocialIdentity{
			{ID: "ident-existing", UserID: "user-existing", Provider: "github", ProviderUID: "gh-existing", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.TestLogin(ctx, TestLoginRequest{Email: "ex@x.com", AppID: "yundian"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.User.ID != "user-existing" {
			t.Errorf("expected user-existing, got %s", resp.User.ID)
		}
	})

	t.Run("suspended user rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		ur.users["user-susp"] = &model.User{ID: "user-susp", Status: "suspended"}
		email := "susp@x.com"
		sir.identities["github:gh-susp"] = &model.SocialIdentity{
			ID: "ident-susp", UserID: "user-susp",
			Provider: "github", ProviderUID: "gh-susp",
			Email: &email,
		}
		sir.byEmail["susp@x.com"] = []model.SocialIdentity{
			{ID: "ident-susp", UserID: "user-susp", Provider: "github", ProviderUID: "gh-susp", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "susp@x.com", AppID: "yundian"})
		if !errors.Is(err, ErrUserSuspended) {
			t.Errorf("expected ErrUserSuspended, got %v", err)
		}
	})
}

// newAuthMocks is a small helper that bundles the 6 mock repos a test
// needs. Returns (*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo,
// *mockSubscriptionRepo, *mockSessionRepo, *mockAppRepo).
func newAuthMocks() (*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo, *mockSubscriptionRepo, *mockSessionRepo, *mockAppRepo) {
	ur := newMockUserRepo()
	sir := newMockSocialIdentityRepo()
	pr := newMockPlanRepo()
	sr := newMockSubscriptionRepo()
	ssr := newMockSessionRepo()
	ar := newMockAppRepo()
	return ur, sir, pr, sr, ssr, ar
}

// TestAuthService_issueTokensForUser_ErrorPaths covers the internal error
// paths in the shared token-issuance tail. The function is private but
// reachable through LoginWithProfile / TestLogin / RefreshToken — these
// tests focus on branches those callers do not directly hit.
func TestAuthService_issueTokensForUser_ErrorPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}, IsDefault: true}

	t.Run("plan not found error", func(t *testing.T) {
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// User has no subscription → falls into FindDefault. Make that fail.
		// Actually no: with no subscription, FindActiveByUserID returns
		// ErrNoRows, so the else branch runs FindDefault. We force the
		// first branch (active sub present) by seeding one, then have
		// FindByID fail.
		exp := time.Now().Add(time.Hour)
		sr.subs["sub-1"] = &model.Subscription{ID: "sub-1", UserID: "u-1", PlanID: "missing", Status: "active", ExpiresAt: &exp}
		sr.byUserID["u-1"] = sr.subs["sub-1"]
		pr.err = errTest // FindByID will return this
		// pre-seed a user with matching email for the lookup
		ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.issueTokensForUser(ctx, ur.users["u-1"], "yundian", nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "get plan") {
			t.Errorf("expected 'get plan' in error, got %q", err.Error())
		}
	})

	t.Run("find default plan error", func(t *testing.T) {
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// No sub, so FindActiveByUserID returns ErrNoRows, and FindDefault
		// is called. We need FindDefault to return an error other than
		// ErrNoRows so the service bubbles it.
		pr.err = errTest
		ur.users["u-2"] = &model.User{ID: "u-2", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		_, err := svc.issueTokensForUser(ctx, ur.users["u-2"], "yundian", nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "get default plan") {
			t.Errorf("expected 'get default plan' in error, got %q", err.Error())
		}
	})

	t.Run("overrideEmail applied to response", func(t *testing.T) {
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		ur.users["u-3"] = &model.User{ID: "u-3", Status: "active", Email: nil}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		override := "testlogin@x.com"
		resp, err := svc.issueTokensForUser(ctx, ur.users["u-3"], "yundian", &override)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.User.Email == nil || *resp.User.Email != "testlogin@x.com" {
			t.Errorf("User.Email = %v, want testlogin@x.com (override)", resp.User.Email)
		}
	})

	t.Run("active sub's expires_at carried to response", func(t *testing.T) {
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		expAt := time.Now().Add(7 * 24 * time.Hour)
		sr.subs["sub-4"] = &model.Subscription{ID: "sub-4", UserID: "u-4", PlanID: "free", Status: "active", ExpiresAt: &expAt}
		sr.byUserID["u-4"] = sr.subs["sub-4"]
		pr.plans["free"] = defaultPlan
		ur.users["u-4"] = &model.User{ID: "u-4", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)
		resp, err := svc.issueTokensForUser(ctx, ur.users["u-4"], "yundian", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription.ExpiresAt == nil {
			t.Error("Subscription.ExpiresAt is nil; expected sub's ExpiresAt to be carried")
		} else if !resp.Subscription.ExpiresAt.Equal(expAt) {
			t.Errorf("ExpiresAt: got %v, want %v", resp.Subscription.ExpiresAt, expAt)
		}
	})
}

// TestAuthService_getOrCreateUser covers the internal helper that
// LoginWithProfile / TestLogin share for the "find existing identity or
// create new user + bind new identity" dance. The function has three
// exit paths:
//   1. existing identity → return that user
//   2. brand-new user + identity
//   3. race: a concurrent caller bound the same (provider, uid) first
//      (mock this by seeding the identity BEFORE the call, but in the
//      wrong order to force the duplicate-key path on Create)
func TestAuthService_getOrCreateUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("existing identity → return that user", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		ur.users["u-existing"] = &model.User{ID: "u-existing", Status: "active"}
		email := "ex@x.com"
		sir.identities["github:gh-1"] = &model.SocialIdentity{
			ID: "ident-1", UserID: "u-existing", Provider: "github",
			ProviderUID: "gh-1", Email: &email,
		}
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		u, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-1", Email: "ex@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.ID != "u-existing" {
			t.Errorf("user.ID = %q, want u-existing", u.ID)
		}
	})

	t.Run("new user + new identity", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		u, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-fresh", Email: "fresh@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.ID == "" {
			t.Error("expected non-empty user.ID for new user")
		}
		// The mock's Create stores by provider+uid key, so the new
		// identity should be retrievable.
		if _, ok := sir.identities["github:gh-fresh"]; !ok {
			t.Error("expected identity to be created")
		}
	})

	t.Run("race: duplicate-key on identity.Create falls back to winner", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		// Seed a "winner" identity that the duplicate-key path will
		// resolve to. The mock's Create rejects with a duplicateKeyError
		// when the identity already exists.
		ur.users["u-winner"] = &model.User{ID: "u-winner", Status: "active"}
		email := "race@x.com"
		sir.identities["github:gh-race"] = &model.SocialIdentity{
			ID: "ident-winner", UserID: "u-winner", Provider: "github",
			ProviderUID: "gh-race", Email: &email,
		}
		sir.createErr = &duplicateKeyError{}
		defer func() { sir.createErr = nil }()
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		u, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-race", Email: "race@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.ID != "u-winner" {
			t.Errorf("user.ID = %q, want u-winner (winner of the race)", u.ID)
		}
	})

	t.Run("identity.Create generic error → wrap", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		// Inject a non-duplicate-key error on Create.
		sir.createErr = errors.New("db down")
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		_, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-wrap", Email: "wrap@x.com",
		})
		if err == nil {
			t.Fatal("expected wrapped error, got nil")
		}
		if !strings.Contains(err.Error(), "create identity") {
			t.Errorf("expected wrap 'create identity', got %q", err.Error())
		}
	})

	t.Run("resolveOrCreateUser error → wrap", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		// Inject an error into userRepo.Create. resolveOrCreateUser calls
		// it on the new-user path; that error propagates up and is wrapped
		// as "resolve user: ..." inside getOrCreateUser.
		ur.err = errors.New("db down on create user")
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		_, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-resolve", Email: "resolve@x.com",
		})
		if err == nil {
			t.Fatal("expected wrapped error, got nil")
		}
		if !strings.Contains(err.Error(), "resolve user") {
			t.Errorf("expected wrap 'resolve user', got %q", err.Error())
		}
	})
}

// TestAuthService_resolveOrCreateUser covers the inner resolve helper that
// the public Login/TestLogin paths transitively use.
func TestAuthService_resolveOrCreateUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no email → create new user", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		id, isNew, err := svc.resolveOrCreateUser(ctx, &ProviderUserInfo{Provider: "github"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !isNew {
			t.Error("isNew: got false, want true (no email → must create)")
		}
		if id == "" {
			t.Error("userID is empty")
		}
	})

	t.Run("email with no match → create new user", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		// sir.FindByEmail returns no rows by default
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		id, isNew, err := svc.resolveOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-1", Email: "new@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !isNew {
			t.Error("isNew: got false, want true (no email match)")
		}
		if id == "" {
			t.Error("userID is empty")
		}
	})

	t.Run("email with existing identity → reuse user", func(t *testing.T) {
		ur := newMockUserRepo()
		ur.users["existing-user"] = &model.User{ID: "existing-user", Status: "active"}
		sir := newMockSocialIdentityRepo()
		// Pre-existing identity bound to existing-user
		email := "found@x.com"
		sir.byEmail["found@x.com"] = []model.SocialIdentity{
			{ID: "ident-1", UserID: "existing-user", Provider: "github", ProviderUID: "gh-existing", Email: &email},
		}
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		id, isNew, err := svc.resolveOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-new", Email: "found@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if isNew {
			t.Error("isNew: got true, want false (email matched existing identity)")
		}
		if id != "existing-user" {
			t.Errorf("userID: got %q, want existing-user", id)
		}
	})

	t.Run("email match is filtered for l3-e2e prefix", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		// The only identity bound to this email is an l3-e2e-* test
		// identity — must NOT be used as merge target.
		email := "victim@x.com"
		sir.byEmail["victim@x.com"] = []model.SocialIdentity{
			{ID: "ident-e2e", UserID: "e2e-user", Provider: "github", ProviderUID: "l3-e2e-fake", Email: &email},
		}
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		_, isNew, err := svc.resolveOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-real", Email: "victim@x.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !isNew {
			t.Error("isNew: got false, want true (l3-e2e identity must be filtered)")
		}
	})

	t.Run("user repo create error propagated", func(t *testing.T) {
		ur := newMockUserRepo()
		ur.err = errTest
		sir := newMockSocialIdentityRepo()
		svc := &AuthService{userRepo: ur, identityRepo: sir}
		_, _, err := svc.resolveOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", Email: "x@y.com",
		})
		if err == nil {
			t.Fatal("expected error from user create, got nil")
		}
	})
}
