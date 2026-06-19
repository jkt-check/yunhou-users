package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

func TestAuthService_Login(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	plans := map[string]*model.Plan{
		"free":    {ID: "free", Name: "免费", Apps: []string{"yundian"}},
		"monthly": {ID: "monthly", Name: "按月订阅", Apps: []string{"yundian", "yundash"}},
	}

	tests := []struct {
		name        string
		req         LoginRequest
		setup       func(*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo, *mockSubscriptionRepo, *mockSessionRepo)
		wantErr     bool
		errContains string
		validate    func(t *testing.T, resp *LoginResponse)
	}{
		{
			name: "new user login with free plan",
			req: LoginRequest{
				Provider:      "github",
				ProviderToken: "newuser1",
				AppID:        "yundian",
			},
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
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
				if !resp.Subscription.HasAccess {
					t.Error("expected has_access=true for yundian on free plan")
				}
			},
		},
		{
			name: "new user login to paid app without subscription",
			req: LoginRequest{
				Provider:      "github",
				ProviderToken: "newuser2",
				AppID:        "yundash",
			},
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
			},
			wantErr: false,
			validate: func(t *testing.T, resp *LoginResponse) {
				if resp.Subscription.HasAccess {
					t.Error("expected has_access=false for yundash on free plan")
				}
			},
		},
		{
			name: "existing user with paid subscription",
			req: LoginRequest{
				Provider:      "github",
				ProviderToken: "existing",  // yields ProviderUID "github_existing"
				AppID:        "yundash",
			},
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				pr.plans["free"] = plans["free"]
				pr.plans["monthly"] = plans["monthly"]
				pr.defaultPlan = plans["free"]

				existingUser := &model.User{ID: "user-existing", Status: "active"}
				ur.users["user-existing"] = existingUser

				existingIdentity := &model.SocialIdentity{
					ID:          "ident-1",
					UserID:      "user-existing",
					Provider:    "github",
					ProviderUID: "github_existing",
				}
				sir.identities["github:github_existing"] = existingIdentity

				expiresAt := time.Now().Add(30 * 24 * time.Hour)
				existingSub := &model.Subscription{
					ID:        "sub-1",
					UserID:    "user-existing",
					PlanID:    "monthly",
					Status:    "active",
					ExpiresAt: &expiresAt,
				}
				sr.subs["sub-1"] = existingSub
				sr.byUserID["user-existing"] = existingSub
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
			name: "unsupported provider",
			req: LoginRequest{
				Provider:      "unknown",
				ProviderToken: "token",
				AppID:        "yundian",
			},
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				pr.plans["free"] = plans["free"]
				pr.defaultPlan = plans["free"]
			},
			wantErr:     true,
			errContains: "unsupported provider",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ur := newMockUserRepo()
			sir := newMockSocialIdentityRepo()
			pr := newMockPlanRepo()
			sr := newMockSubscriptionRepo()
			ssr := newMockSessionRepo()

			tc.setup(ur, sir, pr, sr, ssr)

			tokenSvc := newTokenServiceWithMocks(ssr, sr)
			authSvc := NewAuthService(ur, sir, pr, sr, ssr, tokenSvc)

			resp, err := authSvc.Login(ctx, tc.req)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if !strings.Contains(err.Error(), tc.errContains) {
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

func TestAuthService_Logout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("valid logout", func(t *testing.T) {
		ur := newMockUserRepo()
		sir := newMockSocialIdentityRepo()
		pr := newMockPlanRepo()
		sr := newMockSubscriptionRepo()
		ssr := newMockSessionRepo()

		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		authSvc := NewAuthService(ur, sir, pr, sr, ssr, tokenSvc)

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

		tokenSvc := newTokenServiceWithMocks(ssr, sr)
		authSvc := NewAuthService(ur, sir, pr, sr, ssr, tokenSvc)

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
		setup       func(*mockUserRepo, *mockSocialIdentityRepo, *mockPlanRepo, *mockSubscriptionRepo, *mockSessionRepo)
		wantErr     bool
		errContains string
		validate    func(t *testing.T, resp *LoginResponse)
	}{
		{
			name: "refresh with valid token",
			refreshToken: "valid-refresh-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
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
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
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
			},
			wantErr:     true,
			errContains: "invalid refresh token",
		},
		{
			name: "refresh with revoked token",
			refreshToken: "revoked-token",
			appID:       "yundian",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
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
			},
			wantErr:     true,
			errContains: "invalid refresh token",
		},
		{
			name: "refresh for paid app with subscription",
			refreshToken: "paid-user-token",
			appID:       "yundash",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, pr *mockPlanRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ur := newMockUserRepo()
			sir := newMockSocialIdentityRepo()
			pr := newMockPlanRepo()
			sr := newMockSubscriptionRepo()
			ssr := newMockSessionRepo()

			tc.setup(ur, sir, pr, sr, ssr)

			tokenSvc := newTokenServiceWithMocks(ssr, sr)
			authSvc := NewAuthService(ur, sir, pr, sr, ssr, tokenSvc)

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
