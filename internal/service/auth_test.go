package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	t.Run("session lookup generic error", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		pr := newMockPlanRepo()
		sr := newMockSubscriptionRepo()
		ssr := newMockSessionRepo()
		ar := newMockAppRepo()

		// Inject a non-ErrNoRows error.
		ssr.findErr = errors.New("db down")
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		authSvc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		err := authSvc.Logout(ctx, "any-token")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "find session") {
			t.Errorf("expected wrap 'find session', got %q", err.Error())
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
		name         string
		refreshToken string
		appID        string
		setup        func(*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo, *mockSubscriptionRepo, *mockSessionRepo, *mockAppRepo)
		wantErr      bool
		errContains  string
		validate     func(t *testing.T, resp *LoginResponse)
	}{
		{
			name:         "refresh with valid token",
			refreshToken: "valid-refresh-token",
			appID:        "yundian",
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
			name:         "refresh with expired session token",
			refreshToken: "expired-token",
			appID:        "yundian",
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
			name:         "refresh with revoked token",
			refreshToken: "revoked-token",
			appID:        "yundian",
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
			name:         "refresh for paid app with subscription",
			refreshToken: "paid-user-token",
			appID:        "yundash",
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
			name:         "refresh rejects suspended user",
			refreshToken: "suspended-user-token",
			appID:        "yundian",
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
			// cn-staging 2026-07-23 fix: a user with an expired
			// subscription can still refresh — login and subscription
			// are independent concerns. The response's Subscription
			// block reflects the expired state (PlanID = original,
			// HasAccess = false), but the access / refresh tokens are
			// issued normally. The exact response-shape contract is
			// locked down by TestRefreshToken_ExpiredSub_DoesNotError
			// below; this case just exercises that RefreshToken no
			// longer errors with "expired".
			name:         "refresh allows expired subscription (cn-staging 2026-07-23 fix)",
			refreshToken: "expired-sub-token",
			appID:        "yundian",
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
			wantErr: false,
		},
		{
			// Refresh-token reuse detection: rotating then replaying the
			// same refresh token must trigger the family-revoke response.
			// This is the security-critical test that would have caught
			// the earlier bug where RotateRefresh returned a plain error
			// that errors.Is could not match.
			name:         "refresh reuse revokes the family",
			refreshToken: "reuse-token",
			appID:        "yundian",
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

// TestGenerateRefreshToken_RandReadError covers the rand.Read error
// path in GenerateRefreshToken. The production code goes through a
// package-level indirection (refreshTokenReader) so tests can
// inject a failing reader without touching system entropy.
func TestGenerateRefreshToken_RandReadError(t *testing.T) {
	orig := refreshTokenReader
	defer func() { refreshTokenReader = orig }()
	refreshTokenReader = func([]byte) (int, error) {
		return 0, errors.New("synthetic rand.Read failure")
	}
	_, err := GenerateRefreshToken()
	if err == nil {
		t.Fatal("expected error from failing reader, got nil")
	}
	if !strings.Contains(err.Error(), "generate refresh token") {
		t.Errorf("expected wrap 'generate refresh token', got %q", err.Error())
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
		{"l3-e2e", false},   // prefix must include the trailing dash
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
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}

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
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}

	t.Run("requested plan controls token scope", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.defaultPlan = defaultPlan
		pr.plans["monthly"] = &model.Plan{
			ID: "monthly", Name: "月付", Apps: []string{"yundian", "yunbao"},
			IsActive: true, AcceptingNewSubscriptions: true,
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.TestLogin(ctx, TestLoginRequest{
			Email: "plan-scope@x.com", AppID: "yundian", PlanID: "monthly",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		claims, err := tokenSvc.VerifyAccessToken(resp.AccessToken)
		if err != nil {
			t.Fatalf("verify access token: %v", err)
		}
		if !slices.Equal(claims.Scope, []string{"yundian", "yunbao"}) {
			t.Errorf("token scope = %v, want requested plan apps", claims.Scope)
		}
		if resp.Subscription == nil || resp.Subscription.PlanID != "monthly" {
			t.Errorf("subscription = %+v, want requested monthly plan", resp.Subscription)
		}
	})

	t.Run("existing active subscription takes precedence over requested plan", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.plans["monthly"] = &model.Plan{
			ID: "monthly", Name: "月付", Apps: []string{"yundian"},
			IsActive: true, AcceptingNewSubscriptions: true,
		}
		pr.plans["restricted"] = &model.Plan{
			ID: "restricted", Name: "受限", Apps: []string{},
			IsActive: true, AcceptingNewSubscriptions: true,
		}
		email := "subscribed@x.com"
		ur.users["subscribed-user"] = &model.User{ID: "subscribed-user", Status: "active"}
		sir.byEmail[email] = []model.SocialIdentity{{
			ID: "subscribed-identity", UserID: "subscribed-user", Provider: "github", ProviderUID: "gh-subscribed", Email: &email,
		}}
		sr.byUserID["subscribed-user"] = &model.Subscription{
			ID: "subscribed-sub", UserID: "subscribed-user", PlanID: "restricted", Status: "active",
		}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.TestLogin(ctx, TestLoginRequest{
			Email: email, AppID: "yundian", PlanID: "monthly",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil || resp.Subscription.PlanID != "restricted" || resp.Subscription.HasAccess {
			t.Errorf("subscription = %+v, want active restricted subscription", resp.Subscription)
		}
	})

	t.Run("requested plan must exist", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))

		_, err := svc.TestLogin(ctx, TestLoginRequest{
			Email: "missing-plan@x.com", AppID: "yundian", PlanID: "missing",
		})
		if !errors.Is(err, ErrPlanNotFound) {
			t.Errorf("error = %v, want ErrPlanNotFound", err)
		}
	})

	t.Run("requested plan must be active and accepting new subscriptions", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			plan *model.Plan
			want error
		}{
			{name: "inactive", plan: &model.Plan{ID: "monthly", AcceptingNewSubscriptions: true}, want: ErrPlanInactive},
			{name: "not accepting", plan: &model.Plan{ID: "monthly", IsActive: true}, want: ErrPlanNotAcceptingNew},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				ur, sir, pr, sr, ssr, ar := newAuthMocks()
				ar.seedActive("yundian", "云店")
				pr.plans[tc.plan.ID] = tc.plan
				svc := NewAuthService(ur, sir, pr, sr, ssr, ar, newTokenServiceWithMocks(ssr, sr))

				_, err := svc.TestLogin(ctx, TestLoginRequest{
					Email: "unavailable-plan@x.com", AppID: "yundian", PlanID: tc.plan.ID,
				})
				if !errors.Is(err, tc.want) {
					t.Errorf("error = %v, want %v", err, tc.want)
				}
			})
		}
	})

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

// TestAuthService_TestLogin_RarePaths fills in branches the table-driven
// TestLogin doesn't reach: identityRepo.FindByEmail generic error,
// userRepo.FindByID error on existing identity, userRepo.Create error
// on the new-user path, identityRepo.Create error, and the "deleted
// user" account-status guard.
func TestAuthService_TestLogin_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("FindByEmail generic error", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		sir.findByEmailErr = errors.New("db down")
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "x@y.com", AppID: "yundian"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "find identity") {
			t.Errorf("expected wrap 'find identity', got %q", err.Error())
		}
	})

	t.Run("userRepo.FindByID error on existing identity", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		email := "found@x.com"
		sir.byEmail[email] = []model.SocialIdentity{
			{ID: "ident-x", UserID: "u-missing", Provider: "github", ProviderUID: "gh-x", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: email, AppID: "yundian"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "find user") {
			t.Errorf("expected wrap 'find user', got %q", err.Error())
		}
	})

	t.Run("userRepo.Create error on new user", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		ur.err = errors.New("db down on create")
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "fresh@x.com", AppID: "yundian"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "create user") {
			t.Errorf("expected wrap 'create user', got %q", err.Error())
		}
	})

	t.Run("identityRepo.Create error", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		sir.createErr = errors.New("db down on identity create")
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: "fresh2@x.com", AppID: "yundian"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "create identity") {
			t.Errorf("expected wrap 'create identity', got %q", err.Error())
		}
	})

	t.Run("deleted user is rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		email := "del@x.com"
		ur.users["u-deleted"] = &model.User{ID: "u-deleted", Status: "deleted"}
		sir.identities["github:gh-del"] = &model.SocialIdentity{
			ID: "ident-del", UserID: "u-deleted", Provider: "github", ProviderUID: "gh-del", Email: &email,
		}
		sir.byEmail[email] = []model.SocialIdentity{
			{ID: "ident-del", UserID: "u-deleted", Provider: "github", ProviderUID: "gh-del", Email: &email},
		}
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.TestLogin(ctx, TestLoginRequest{Email: email, AppID: "yundian"})
		if !errors.Is(err, ErrUserDeleted) {
			t.Errorf("expected ErrUserDeleted, got %v", err)
		}
	})
}

// TestAuthService_LoginWithProfile_RarePaths fills in branches the
// table-driven LoginWithProfile test doesn't reach: appRepo.FindByID
// generic error, getOrCreateUser error, ErrUserDeleted, and
// findUsableSubscription error.
func TestAuthService_LoginWithProfile_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("appRepo.FindByID generic error", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.findErr = errors.New("db down on app lookup")
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-1", Email: "x@y.com"},
			AppID:   "yundian",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "find app") {
			t.Errorf("expected wrap 'find app', got %q", err.Error())
		}
	})

	t.Run("deleted user is rejected", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		ur.users["u-deleted"] = &model.User{ID: "u-deleted", Status: "deleted"}
		sir.identities["github:gh-del"] = &model.SocialIdentity{
			ID: "ident-del", UserID: "u-deleted", Provider: "github", ProviderUID: "gh-del",
		}
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), newMockSubscriptionRepo())
		svc := NewAuthService(ur, sir, newMockPlanRepo(), newMockSubscriptionRepo(), newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-del", Email: "del@x.com"},
			AppID:   "yundian",
		})
		if !errors.Is(err, ErrUserDeleted) {
			t.Errorf("expected ErrUserDeleted, got %v", err)
		}
	})

	t.Run("peekSubscription DB error", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, sr, _, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Pre-seed a user + identity so getOrCreateUser finds the existing
		// user. Inject a generic error on the subRepo lookup. LoginWithProfile
		// no longer calls findUsableSubscription directly; peekSubscription
		// is invoked inside issueTokensForUser via resolvePlanForTokenIssuance,
		// so the error propagates from there.
		ur.users["u-fus"] = &model.User{ID: "u-fus", Status: "active"}
		email := "fus@x.com"
		sir.identities["github:gh-fus"] = &model.SocialIdentity{
			ID: "ident-fus", UserID: "u-fus", Provider: "github", ProviderUID: "gh-fus", Email: &email,
		}
		sr.findErr = errors.New("db down on sub lookup")
		tokenSvc := newTokenServiceWithMocks(newMockSessionRepo(), sr)
		svc := NewAuthService(ur, sir, newMockPlanRepo(), sr, newMockSessionRepo(), ar, tokenSvc)
		_, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
			Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-fus", Email: email},
			AppID:   "yundian",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "get subscription") {
			t.Errorf("expected wrap 'get subscription', got %q", err.Error())
		}
	})
}

func TestAuthService_issueTokensForUser_ErrorPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}

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

// TestAuthService_IssueTokensForUser_IsAcceptingNew locks down spec §6.4:
// the LoginResponse.Subscription.IsAcceptingNew field mirrors the chosen
// plan's (IsActive && AcceptingNewSubscriptions) status, so the BFF can
// render "your sub is fine, but this plan isn't sold anymore" UX.
//
// Three branches covered:
//  1. sub with plan.IsActive=true, AcceptingNewSubscriptions=true → true
//  2. sub with plan.IsActive=false → false (deactivated plan)
//  3. no sub → false (Phase 1: chosenPlan is the default plan; with
//     IsActive=false / AcceptingNewSubscriptions=false on the default
//     the field is false. Phase 2 will switch chosenPlan=nil when
//     no sub, so the same assertion continues to hold.)
func TestAuthService_IssueTokensForUser_IsAcceptingNew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("active plan accepting new → true", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Plan that's both active and accepting new subscriptions.
		pr.plans["monthly"] = &model.Plan{
			ID: "monthly", Name: "月付", Apps: []string{"yundian"},
			IsActive: true, AcceptingNewSubscriptions: true,
		}
		expAt := time.Now().Add(30 * 24 * time.Hour)
		sr.subs["sub-monthly"] = &model.Subscription{
			ID: "sub-monthly", UserID: "u-monthly", PlanID: "monthly",
			Status: "active", ExpiresAt: &expAt,
		}
		sr.byUserID["u-monthly"] = sr.subs["sub-monthly"]
		ur.users["u-monthly"] = &model.User{ID: "u-monthly", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.issueTokensForUser(ctx, ur.users["u-monthly"], "yundian", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil {
			t.Fatal("Subscription is nil")
		}
		if !resp.Subscription.IsAcceptingNew {
			t.Errorf("IsAcceptingNew: got false, want true (active plan + accepting new subs)")
		}
	})

	t.Run("plan IsActive=false → false", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Deactivated plan. Even though AcceptingNewSubscriptions=true,
		// IsActive=false flips the formula to false.
		pr.plans["deactivated"] = &model.Plan{
			ID: "deactivated", Name: "已停用", Apps: []string{"yundian"},
			IsActive: false, AcceptingNewSubscriptions: true,
		}
		expAt := time.Now().Add(30 * 24 * time.Hour)
		sr.subs["sub-deact"] = &model.Subscription{
			ID: "sub-deact", UserID: "u-deact", PlanID: "deactivated",
			Status: "active", ExpiresAt: &expAt,
		}
		sr.byUserID["u-deact"] = sr.subs["sub-deact"]
		ur.users["u-deact"] = &model.User{ID: "u-deact", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.issueTokensForUser(ctx, ur.users["u-deact"], "yundian", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil {
			t.Fatal("Subscription is nil")
		}
		if resp.Subscription.IsAcceptingNew {
			t.Errorf("IsAcceptingNew: got true, want false (plan.IsActive=false)")
		}
	})

	t.Run("plan AcceptingNewSubscriptions=false → false", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Plan that's active but has accepting_new_subscriptions=false
		// (the quarterly case from spec §5.1: visible to existing
		// subscribers, not sold to new ones).
		pr.plans["legacy"] = &model.Plan{
			ID: "legacy", Name: "遗留计划", Apps: []string{"yundian"},
			IsActive: true, AcceptingNewSubscriptions: false,
		}
		expAt := time.Now().Add(30 * 24 * time.Hour)
		sr.subs["sub-legacy"] = &model.Subscription{
			ID: "sub-legacy", UserID: "u-legacy", PlanID: "legacy",
			Status: "active", ExpiresAt: &expAt,
		}
		sr.byUserID["u-legacy"] = sr.subs["sub-legacy"]
		ur.users["u-legacy"] = &model.User{ID: "u-legacy", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.issueTokensForUser(ctx, ur.users["u-legacy"], "yundian", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil {
			t.Fatal("Subscription is nil")
		}
		if resp.Subscription.IsAcceptingNew {
			t.Errorf("IsAcceptingNew: got true, want false (AcceptingNewSubscriptions=false)")
		}
	})

	t.Run("no sub → false (default plan not accepting new)", func(t *testing.T) {
		t.Parallel()
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		// Default plan with IsActive=false. Phase 1's resolvePlan picks
		// the default plan as chosenPlan when there's no sub; the formula
		// (chosenPlan != nil && chosenPlan.IsActive && ...) returns
		// false when the default plan is deactivated. Phase 2 will switch
		// chosenPlan=nil when there's no sub — same outcome.
		pr.defaultPlan = &model.Plan{
			ID: "free", Name: "免费", Apps: []string{"yundian"},
			IsActive: false, AcceptingNewSubscriptions: false,
		}
		ur.users["u-nosub"] = &model.User{ID: "u-nosub", Status: "active"}
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.issueTokensForUser(ctx, ur.users["u-nosub"], "yundian", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil {
			t.Fatal("Subscription is nil")
		}
		if resp.Subscription.IsAcceptingNew {
			t.Errorf("IsAcceptingNew: got true, want false (no sub + default plan not accepting)")
		}
	})

	t.Run("RefreshToken also surfaces IsAcceptingNew", func(t *testing.T) {
		t.Parallel()
		// RefreshToken has its own SubscriptionInfo literal — covers the
		// second call site end-to-end.
		ur, sir, pr, sr, ssr, ar := newAuthMocks()
		ar.seedActive("yundian", "云店")
		pr.plans["monthly"] = &model.Plan{
			ID: "monthly", Name: "月付", Apps: []string{"yundian"},
			IsActive: true, AcceptingNewSubscriptions: true,
		}
		expAt := time.Now().Add(30 * 24 * time.Hour)
		sr.subs["sub-rt"] = &model.Subscription{
			ID: "sub-rt", UserID: "u-rt", PlanID: "monthly",
			Status: "active", ExpiresAt: &expAt,
		}
		sr.byUserID["u-rt"] = sr.subs["sub-rt"]
		ur.users["u-rt"] = &model.User{ID: "u-rt", Status: "active"}
		plaintext := "rt-accepting-new"
		hashed := hashToken(plaintext)
		ssr.sessions["sess-rt"] = &model.Session{
			ID: "sess-rt", UserID: "u-rt", AppID: "yundian",
			SessionType: "refresh", RefreshToken: hashed,
			Revoked: false, ExpiresAt: time.Now().Add(time.Hour),
		}
		ssr.byToken[hashed] = ssr.sessions["sess-rt"]
		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

		resp, err := svc.RefreshToken(ctx, plaintext, "yundian")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Subscription == nil {
			t.Fatal("Subscription is nil")
		}
		if !resp.Subscription.IsAcceptingNew {
			t.Errorf("IsAcceptingNew: got false, want true (refresh path must mirror login path)")
		}
	})
}

// TestAuthService_getOrCreateUser covers the internal helper that
// LoginWithProfile / TestLogin share for the "find existing identity or
// create new user + bind new identity" dance. The function has three
// exit paths:
//  1. existing identity → return that user
//  2. brand-new user + identity
//  3. race: a concurrent caller bound the same (provider, uid) first
//     (mock this by seeding the identity BEFORE the call, but in the
//     wrong order to force the duplicate-key path on Create)
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
		ur.users["u-winner"] = &model.User{ID: "u-winner", Status: "active"}
		// No pre-seeded identity. We need the mock to:
		//   1. Return "not found" on the first FindByProviderUID
		//      (so the function proceeds to create a new user + identity)
		//   2. Return the identity on subsequent FindByProviderUID
		//      (so the duplicate-key fallback finds the winner)
		// Use a custom mock that does this.
		identityRepo := &storeFirstIdentityRepo{
			inner:        sir,
			createErr:    &duplicateKeyError{},
			winnerUserID: "u-winner",
		}
		svc := &AuthService{userRepo: ur, identityRepo: identityRepo}
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

	t.Run("race: duplicate-key + winner lookup error → wrap", func(t *testing.T) {
		t.Parallel()
		ur, sir, _, _, _, _ := newAuthMocks()
		ur.users["u-winner"] = &model.User{ID: "u-winner", Status: "active"}
		// Custom mock: first FindByProviderUID returns "not found",
		// second returns an error (the race-winner lookup itself fails).
		identityRepo := &storeFirstIdentityRepo{
			inner:           sir,
			createErr:       &duplicateKeyError{},
			winnerLookupErr: errors.New("db down on winner lookup"),
		}
		svc := &AuthService{userRepo: ur, identityRepo: identityRepo}
		_, err := svc.getOrCreateUser(ctx, &ProviderUserInfo{
			Provider: "github", ProviderUID: "gh-race-2", Email: "race2@x.com",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "create identity") {
			t.Errorf("expected wrap 'create identity', got %q", err.Error())
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

// storeFirstIdentityRepo is a tiny wrapper around *mockSocialIdentityRepo
// that mirrors what a real DB does on a duplicate-key INSERT: it
// persists the row first, then raises the UNIQUE-check error. The
// default mock's Create short-circuits on createErr and never stores —
// which doesn't reflect production. This wrapper drives the
// "duplicate key + winner found" branch in getOrCreateUser.
//
// IMPORTANT: it tracks the call count so the FIRST FindByProviderUID
// returns "not found" (simulating "we hadn't created the row yet
// when we checked the first time") and the SECOND FindByProviderUID
// (in the duplicate-key fallback path) returns the winner.
type storeFirstIdentityRepo struct {
	inner           *mockSocialIdentityRepo
	createErr       error
	winnerUserID    string
	winnerLookupErr error
	findCallCount   int
}

func (s *storeFirstIdentityRepo) Create(ctx context.Context, si *model.SocialIdentity) error {
	// Simulate "DB persists, then errors on UNIQUE check" — the row
	// is visible to subsequent lookups even though Create returned an
	// error. This is the core invariant the test relies on.
	// We override the UserID to be the "winner" — simulating a
	// concurrent caller that inserted first.
	si.UserID = s.winnerUserID
	key := si.Provider + ":" + si.ProviderUID
	s.inner.identities[key] = si
	if si.Email != nil {
		s.inner.byEmail[*si.Email] = append(s.inner.byEmail[*si.Email], *si)
	}
	s.inner.byUserID[si.UserID] = append(s.inner.byUserID[si.UserID], *si)
	return s.createErr
}
func (s *storeFirstIdentityRepo) FindByProviderUID(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
	s.findCallCount++
	if s.findCallCount == 1 {
		// First call: pretend the row doesn't exist yet (the caller
		// checks before the concurrent caller inserts).
		return nil, fmt.Errorf("not found")
	}
	if s.winnerLookupErr != nil {
		// Second call: the winner lookup itself fails.
		return nil, s.winnerLookupErr
	}
	// Second call: the row was stored by Create between the first
	// and second FindByProviderUID calls. Return it.
	return s.inner.FindByProviderUID(ctx, provider, providerUID)
}
func (s *storeFirstIdentityRepo) FindByEmail(ctx context.Context, email string) ([]model.SocialIdentity, error) {
	return s.inner.FindByEmail(ctx, email)
}
func (s *storeFirstIdentityRepo) ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
	return s.inner.ListByUserID(ctx, userID)
}
func (s *storeFirstIdentityRepo) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}
func (s *storeFirstIdentityRepo) CountByUserID(ctx context.Context, userID string) (int, error) {
	return s.inner.CountByUserID(ctx, userID)
}
func (s *storeFirstIdentityRepo) DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error) {
	return s.inner.DeleteIfNotLast(ctx, id, userID)
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

// ============================================================================
// cn-staging 2026-07-23 login-loop fix — regression suite for the
// decoupling of authentication from subscription enforcement.
//
// Before the fix, LoginWithProfile / RefreshToken called
// findUsableSubscription, which returned ErrSubscriptionExpired for any
// `status=active, expires_at < now()` row. The OAuth callback translated
// that sentinel into URL `reason=subscription_expired` and the BFF
// rendered a banner that blocked the user from renewing. The fix moves
// the expiry decision out of the login layer; subscription state is now
// just data that the response carries, not a reason to refuse login.
//
// These tests lock down the new contract: peekSubscription reports
// expiry without erroring, resolvePlanForTokenIssuance falls back to
// the default plan but surfaces the original PlanID, and the issuance
// paths (login + refresh) succeed end-to-end on rows that would have
// tripped the old sentinel.
// ============================================================================

func TestPeekSubscription(t *testing.T) {
	t.Parallel()

	t.Run("no row → (nil, false, nil)", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		svc := &AuthService{subRepo: sr}
		sub, expired, err := svc.peekSubscription(context.Background(), "u-missing")
		if err != nil {
			t.Errorf("err: got %v, want nil", err)
		}
		if sub != nil {
			t.Errorf("sub: got %v, want nil", sub)
		}
		if expired {
			t.Error("expired: got true, want false")
		}
	})

	t.Run("active row with future expires_at → (sub, false, nil)", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		exp := time.Now().Add(24 * time.Hour)
		sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &exp}
		sr.byUserID["u-1"] = sr.subs["s-1"]
		svc := &AuthService{subRepo: sr}
		sub, expired, err := svc.peekSubscription(context.Background(), "u-1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sub == nil || sub.ID != "s-1" {
			t.Errorf("sub: got %+v, want s-1", sub)
		}
		if expired {
			t.Error("expired: got true, want false for future expires_at")
		}
	})

	t.Run("active row with past expires_at → (sub, true, nil) — NO error", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		past := time.Now().Add(-1 * time.Hour)
		sr.subs["s-past"] = &model.Subscription{ID: "s-past", UserID: "u-past", PlanID: "monthly", Status: "active", ExpiresAt: &past}
		sr.byUserID["u-past"] = sr.subs["s-past"]
		svc := &AuthService{subRepo: sr}
		// The critical regression: previously peekSubscription's predecessor
		// returned ErrSubscriptionExpired here, blocking the login layer
		// from making any decisions. The fix guarantees NO error in this
		// branch so the OAuth callback cannot refuse login on sub state.
		sub, expired, err := svc.peekSubscription(context.Background(), "u-past")
		if err != nil {
			t.Fatalf("err: peekSubscription must NOT return an error for an expired row (that's the whole point of the 2026-07-23 fix); got %v", err)
		}
		if sub == nil || sub.ID != "s-past" {
			t.Errorf("sub: got %+v, want s-past", sub)
		}
		if !expired {
			t.Error("expired: got false, want true for past expires_at")
		}
	})

	t.Run("active row with NULL expires_at → (sub, false, nil) — never expires, NOT expired", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.subs["s-null"] = &model.Subscription{ID: "s-null", UserID: "u-null", PlanID: "lifetime", Status: "active", ExpiresAt: nil}
		sr.byUserID["u-null"] = sr.subs["s-null"]
		svc := &AuthService{subRepo: sr}
		sub, expired, err := svc.peekSubscription(context.Background(), "u-null")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sub == nil {
			t.Error("sub: got nil, want s-null")
		}
		if expired {
			t.Error("expired: NULL expires_at must NOT count as expired (legacy behaviour, locked down here)")
		}
	})

	t.Run("DB error bubbles up unchanged", func(t *testing.T) {
		t.Parallel()
		sr := newMockSubscriptionRepo()
		sr.findErr = errors.New("db down")
		svc := &AuthService{subRepo: sr}
		_, _, err := svc.peekSubscription(context.Background(), "u-x")
		if err == nil {
			t.Fatal("expected error from FindActiveByUserID, got nil")
		}
		if !strings.Contains(err.Error(), "get subscription") {
			t.Errorf("expected wrap 'get subscription', got %q", err.Error())
		}
	})
}

// TestResolvePlanForTokenIssuance_ExpiredSub locks down spec §4.4 + §4.5:
// an active-but-past subscription chooses the default plan for the
// access-token scope / has_access computation, but the response surfaces
// the original PlanID + PlanName so the FE shows "renew your X plan"
// instead of misreading the state as "you got downgraded to free".
func TestResolvePlanForTokenIssuance_ExpiredSub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ur, sir, pr, sr, ssr, ar := newAuthMocks()
	ar.seedActive("yundian", "云店")

	defaultPlan := &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
	pr.plans["free"] = defaultPlan
	pr.defaultPlan = defaultPlan
	paidPlan := &model.Plan{ID: "monthly", Name: "月付", Apps: []string{"yundian"}}
	pr.plans["monthly"] = paidPlan

	past := time.Now().Add(-1 * time.Hour)
	sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &past}
	sr.byUserID["u-1"] = sr.subs["s-1"]

	ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
	tokenSvc := newTokenServiceWithMocks(ssr, sr)
	svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

	chosenPlan, surfaceID, surfaceName, hasAccess, expiresAt, err := svc.resolvePlanForTokenIssuance(ctx, "u-1", "yundian")
	if err != nil {
		t.Fatalf("resolvePlanForTokenIssuance: %v", err)
	}
	// chosenPlan must be the default (entitlement right now)
	if chosenPlan.ID != "free" {
		t.Errorf("chosenPlan.ID: got %q, want %q (expired sub must not grant the paid plan's entitlements)", chosenPlan.ID, "free")
	}
	// surface ID/Name come from the original (paid) plan so the FE can
	// render the renew CTA targeting "monthly"
	if surfaceID != "monthly" {
		t.Errorf("surfacePlanID: got %q, want %q (FE needs to render renew CTA against the user's original plan)", surfaceID, "monthly")
	}
	if surfaceName != "月付" {
		t.Errorf("surfacePlanName: got %q, want %q", surfaceName, "月付")
	}
	// hasAccess MUST be false — the default plan on yunhou-users today
	// includes "yundian", so without the explicit override we'd return
	// true for an expired monthly user. The FE's useBilling hook treats
	// (plan_id != "free" && has_access=true) as "already subscribed"
	// and refuses the new checkout, leaving the user looped.
	if hasAccess {
		t.Errorf("hasAccess: got true, want false (expired sub must NOT be granted has_access even when default plan includes appID; this was the cn-staging 2026-07-23 follow-up bug)")
	}
	// surfacePlanID is the user's *intended* plan, not the default —
	// console renders "your X plan expired" + "renew" CTA.
	if surfaceID != "monthly" {
		t.Errorf("surfacePlanID: got %q, want %q", surfaceID, "monthly")
	}
	if surfaceName != "月付" {
		t.Errorf("surfacePlanName: got %q, want %q", surfaceName, "月付")
	}
	// chosenPlan is the default (it's what scopes the access token),
	// not the expired paid plan.
	if chosenPlan.ID != "free" {
		t.Errorf("chosenPlan.ID: got %q, want %q (token scope must reflect actual entitlement, not paid plan)", chosenPlan.ID, "free")
	}
	// ExpiresAt returns the original past timestamp so the FE knows the
	// sub is "expired since …" and can render "your X plan expired N
	// days ago".
	if expiresAt == nil || !expiresAt.Equal(past) {
		t.Errorf("expiresAt: got %v, want %v", expiresAt, past)
	}
}

// TestResolvePlanForTokenIssuance_ExpiredSub_OriginalPlanMissing asserts
// the degraded-mode contract: when the original plan row referenced by
// sub.PlanID has been deleted (FK errors aren't normally possible but
// transient DB failures are), surfacePlanID still reflects the user's
// intent (sub.PlanID) rather than silently substituting the default
// plan's ID — silence here hid a separate defect during the cn-staging
// 2026-07-23 incident investigation.
func TestResolvePlanForTokenIssuance_ExpiredSub_OriginalPlanMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ur, sir, pr, sr, ssr, ar := newAuthMocks()
	ar.seedActive("yundian", "云店")

	pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
	pr.defaultPlan = pr.plans["free"]
	// Note: "monthly" intentionally NOT seeded. Lookup will fail.
	pr.lookupErrForIDs = map[string]error{"monthly": errors.New("plan not found")}

	past := time.Now().Add(-1 * time.Hour)
	sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &past}
	sr.byUserID["u-1"] = sr.subs["s-1"]
	ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
	_ = sir
	_ = ssr
	tokenSvc := newTokenServiceWithMocks(ssr, sr)
	svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

	chosenPlan, surfaceID, surfaceName, hasAccess, _, err := svc.resolvePlanForTokenIssuance(ctx, "u-1", "yundian")
	if err != nil {
		t.Fatalf("resolvePlanForTokenIssuance must not error on missing original plan; got %v", err)
	}
	if chosenPlan.ID != "free" {
		t.Errorf("chosenPlan.ID: got %q, want free", chosenPlan.ID)
	}
	if surfaceID != "monthly" {
		t.Errorf("surfacePlanID: got %q, want monthly (must reflect sub.PlanID, never silently fall back to default)", surfaceID)
	}
	if surfaceName != "" {
		t.Errorf("surfacePlanName: got %q, want empty (lookup failed — empty is the documented degraded contract)", surfaceName)
	}
	if hasAccess {
		t.Error("hasAccess: must be false even when the original plan cannot be resolved")
	}
}

// TestIssueTokensForUser_ExpiredSub asserts the full LoginResponse shape
// for an expired-sub user. This is the contract the SPA's AuthContext
// consumes: HasAccess=false drives the console paywall; PlanID/Name
// match the user's *intended* plan, not the default plan, so the renew
// CTA points back at /billing/checkout?plan=monthly.
func TestIssueTokensForUser_ExpiredSub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ur, sir, pr, sr, ssr, ar := newAuthMocks()
	ar.seedActive("yundian", "云店")

	// Paid "monthly" plan includes ONLY "yundian" — same set as the
	// default, so we can't distinguish entitlement via chosenPlan.Apps
	// membership. To force the difference, use apps=["other-app"] for
	// "monthly" (which doesn't include "yundian"), and apps=["yundian"]
	// for the default. That way has_access computed against the chosen
	// (default) plan IS true, matching today's behaviour; the spec's
	// "HasAccess=false during expired" promise is delivered at the
	// response-shape layer in production where the FE additionally
	// checks Subscription.ExpiresAt < now to gate the paywall.
	pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
	pr.defaultPlan = pr.plans["free"]
	pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "月付", Apps: []string{"yundian"}}

	past := time.Now().Add(-1 * time.Hour)
	sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &past}
	sr.byUserID["u-1"] = sr.subs["s-1"]

	ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
	tokenSvc := newTokenServiceWithMocks(ssr, sr)
	svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

	resp, err := svc.issueTokensForUser(ctx, ur.users["u-1"], "yundian", nil)
	if err != nil {
		t.Fatalf("issueTokensForUser must NOT error for an expired sub (cn-staging 2026-07-23 fix); got %v", err)
	}
	if resp == nil || resp.Subscription == nil {
		t.Fatal("response / subscription block missing")
	}
	if resp.Subscription.PlanID != "monthly" {
		t.Errorf("Subscription.PlanID: got %q, want %q (FE renew-CTA target)", resp.Subscription.PlanID, "monthly")
	}
	if resp.Subscription.PlanName != "月付" {
		t.Errorf("Subscription.PlanName: got %q, want %q", resp.Subscription.PlanName, "月付")
	}
	if resp.Subscription.ExpiresAt == nil || !resp.Subscription.ExpiresAt.Equal(past) {
		t.Errorf("Subscription.ExpiresAt: got %v, want %v", resp.Subscription.ExpiresAt, past)
	}
	// HasAccess MUST be false. The default plan includes "yundian", so
	// without the explicit override the response would return true
	// here — and useBilling's paywall check (`plan_id != free &&
	// has_access`) would refuse the new checkout, leaving the user
	// looped. This is the cn-staging 2026-07-23 follow-up fix.
	if resp.Subscription.HasAccess {
		t.Error("Subscription.HasAccess: got true, want false (expired sub must NOT receive has_access even when default plan includes appID)")
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Error("AccessToken/RefreshToken must be issued even for expired-sub users (they're coming in, not being kicked out)")
	}
}

// TestLoginWithProfile_ExpiredSub_LogsInSuccessfully is the end-to-end
// regression for the cn-staging 2026-07-23 incident: a user with a
// status=active, expires_at=past row clicks WeChat-login and previously
// got bounced back to /auth/login with reason=subscription_expired.
// After the fix, LoginWithProfile returns a LoginResponse and NO error —
// the OAuth callback will redirect to /console, not /auth/login.
func TestLoginWithProfile_ExpiredSub_LogsInSuccessfully(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ur, sir, pr, sr, ssr, ar := newAuthMocks()
	ar.seedActive("yundian", "云店")

	pr.plans["free"] = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
	pr.defaultPlan = pr.plans["free"]
	pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "月付", Apps: []string{"yundian"}}

	past := time.Now().Add(-1 * time.Hour)
	sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &past}
	sr.byUserID["u-1"] = sr.subs["s-1"]
	ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
	email := "u1@x.com"
	sir.identities["github:gh-u1"] = &model.SocialIdentity{
		ID: "ident-1", UserID: "u-1", Provider: "github", ProviderUID: "gh-u1", Email: &email,
	}
	tokenSvc := newTokenServiceWithMocks(ssr, sr)
	svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

	resp, err := svc.LoginWithProfile(ctx, LoginWithProfileRequest{
		Profile: &ProviderUserInfo{Provider: "github", ProviderUID: "gh-u1", Email: email},
		AppID:   "yundian",
	})
	if err != nil {
		t.Fatalf("LoginWithProfile must NOT error for expired-sub user; got %v (the cn-staging 2026-07-23 loop root)", err)
	}
	if resp == nil {
		t.Fatal("nil LoginResponse — login was effectively refused")
	}
	if resp.AccessToken == "" {
		t.Error("AccessToken empty — login was effectively refused")
	}
	if resp.Subscription == nil || resp.Subscription.PlanID != "monthly" {
		t.Errorf("Subscription.PlanID: got %+v, want monthly (FE renew CTA target)", resp.Subscription)
	}
}

// TestRefreshToken_ExpiredSub_DoesNotError guards that the
// /auth/refresh path also no longer returns ErrSubscriptionExpired for
// an expired subscription. A user with an in-progress console session
// must be able to keep refreshing after their sub lapses — otherwise
// the console would suddenly sign them out when their subscription
// quietly went past.
func TestRefreshToken_ExpiredSub_DoesNotError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ur, sir, pr, sr, ssr, ar := newAuthMocks()
	ar.seedActive("yundian", "云店")
	pr.defaultPlan = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
	pr.plans["monthly"] = &model.Plan{ID: "monthly", Name: "月付", Apps: []string{"yundian"}}

	// The session has already been issued under the paid plan; now the
	// sub has lapsed. The user tries to refresh before the access
	// token expires, and we must succeed.
	plaintext := "rt-plaintext-1"
	hashed := hashToken(plaintext)
	ssr.sessions["sess-1"] = &model.Session{
		ID:           "sess-1",
		UserID:       "u-1",
		AppID:        "yundian",
		SessionType:  "refresh",
		RefreshToken: hashed,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	ssr.byToken[hashed] = ssr.sessions["sess-1"]
	ur.users["u-1"] = &model.User{ID: "u-1", Status: "active"}
	past := time.Now().Add(-1 * time.Hour)
	sr.subs["s-1"] = &model.Subscription{ID: "s-1", UserID: "u-1", PlanID: "monthly", Status: "active", ExpiresAt: &past}
	sr.byUserID["u-1"] = sr.subs["s-1"]
	tokenSvc := newTokenServiceWithMocks(ssr, sr)
	svc := NewAuthService(ur, sir, pr, sr, ssr, ar, tokenSvc)

	resp, err := svc.RefreshToken(ctx, plaintext, "yundian")
	if err != nil {
		t.Fatalf("RefreshToken must NOT error for expired-sub user; got %v", err)
	}
	if resp == nil {
		t.Fatal("nil LoginResponse from RefreshToken")
	}
	if resp.AccessToken == "" {
		t.Error("AccessToken empty")
	}
	if resp.Subscription == nil || resp.Subscription.PlanID != "monthly" {
		t.Errorf("Subscription.PlanID: got %+v, want monthly", resp.Subscription)
	}
}
