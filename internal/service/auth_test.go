package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

func TestAuthorizeOrCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	hashedSecret, err := util.HashSecret("app-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	appFree := &model.App{
		ID:          "app-free",
		Secret:      hashedSecret,
		Name:        "FreeApp",
		DefaultPlan: "free",
	}
	appNoFree := &model.App{
		ID:          "app-paid",
		Secret:      hashedSecret,
		Name:        "PaidApp",
		DefaultPlan: "pro",
	}

	existingIdentity := &model.SocialIdentity{
		ID:          "ident-1",
		UserID:      "user-existing",
		Provider:    "github",
		ProviderUID: "12345",
		Email:       stringPtr("existing@example.com"),
	}

	existingUser := &model.User{
		ID:     "user-existing",
		Status: "active",
	}

	mergeIdentity := &model.SocialIdentity{
		ID:          "ident-2",
		UserID:      "user-merge",
		Provider:    "github",
		ProviderUID: "99999",
		Email:       stringPtr("merge@example.com"),
	}

	tests := []struct {
		name        string
		info        ProviderUserInfo
		appID       string
		setup       func(*mockUserRepo, *mockSocialIdentityRepo, *mockAppRepo, *mockSubscriptionRepo, *mockSessionRepo)
		wantErr     bool
		errContains string
		validate    func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo)
	}{
		{
			name: "existing identity login",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "12345",
				Email:       "existing@example.com",
				Nickname:    "existinguser",
				AvatarURL:   "https://img.example.com/avatar.png",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				sir.identities["github:12345"] = existingIdentity
				ur.users["user-existing"] = existingUser
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(ur.users) != 1 {
					t.Errorf("expected 1 user, got %d", len(ur.users))
				}
			},
		},
		{
			name: "new user creation with free subscription",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "67890",
				Email:       "newuser@example.com",
				Nickname:    "newuser",
				AvatarURL:   "https://img.example.com/new.png",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(ur.users) != 1 {
					t.Errorf("expected 1 user, got %d", len(ur.users))
				}
				if len(sir.identities) != 1 {
					t.Errorf("expected 1 identity, got %d", len(sir.identities))
				}
				if len(sr.subs) != 1 {
					t.Errorf("expected 1 subscription, got %d", len(sr.subs))
				}
				for _, sub := range sr.subs {
					if sub.Plan != "free" || sub.Status != "active" {
						t.Errorf("expected free active subscription, got plan=%s status=%s", sub.Plan, sub.Status)
					}
				}
			},
		},
		{
			name: "new user no free plan no subscription created",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "67891",
				Email:       "newuser2@example.com",
				Nickname:    "newuser2",
				AvatarURL:   "https://img.example.com/new2.png",
			},
			appID: "app-paid",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-paid"] = appNoFree
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(ur.users) != 1 {
					t.Errorf("expected 1 user, got %d", len(ur.users))
				}
				if len(sr.subs) != 0 {
					t.Errorf("expected 0 subscriptions, got %d", len(sr.subs))
				}
			},
		},
		{
			name: "email merge with existing identity",
			info: ProviderUserInfo{
				Provider:    "google",
				ProviderUID: "google-111",
				Email:       "merge@example.com",
				Nickname:    "mergeduser",
				AvatarURL:   "https://img.example.com/merge.png",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				sir.identities["github:99999"] = mergeIdentity
				sir.byEmail["merge@example.com"] = []model.SocialIdentity{*mergeIdentity}
				sir.byUserID["user-merge"] = []model.SocialIdentity{*mergeIdentity}
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(ur.users) != 0 {
					t.Errorf("expected 0 new users (merged), got %d", len(ur.users))
				}
				ident, ok := sir.identities["google:google-111"]
				if !ok {
					t.Error("expected new identity to be created")
				} else if ident.UserID != "user-merge" {
					t.Errorf("expected identity linked to user-merge, got %s", ident.UserID)
				}
			},
		},
		{
			name: "app not found",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "00000",
				Email:       "notfound@example.com",
				Nickname:    "notfound",
				AvatarURL:   "",
			},
			appID:       "nonexistent-app",
			setup:       func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {},
			wantErr:     true,
			errContains: "app not found",
		},
		{
			name: "user creation failure",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "54321",
				Email:       "fail@example.com",
				Nickname:    "failuser",
				AvatarURL:   "",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				ur.err = fmt.Errorf("db connection failed")
			},
			wantErr:     true,
			errContains: "create user",
		},
		{
			name: "identity creation failure",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "55555",
				Email:       "identfail@example.com",
				Nickname:    "identfail",
				AvatarURL:   "",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				sir.createErr = fmt.Errorf("identity insert failed")
			},
			wantErr:     true,
			errContains: "create identity",
		},
		{
			name: "session creation failure",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "88888",
				Email:       "sessionfail@example.com",
				Nickname:    "sessionfail",
				AvatarURL:   "",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				ssr.createErr = fmt.Errorf("session insert failed")
			},
			wantErr:     true,
			errContains: "create session",
		},
		{
			name: "no email provided — skip email merge",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "noemail-1",
				Email:       "",
				Nickname:    "noemailuser",
				AvatarURL:   "",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(ur.users) != 1 {
					t.Errorf("expected 1 user, got %d", len(ur.users))
				}
			},
		},
		{
			name: "free subscription already exists for existing identity user",
			info: ProviderUserInfo{
				Provider:    "github",
				ProviderUID: "12345",
				Email:       "existing@example.com",
				Nickname:    "existinguser",
				AvatarURL:   "https://img.example.com/avatar.png",
			},
			appID: "app-free",
			setup: func(ur *mockUserRepo, sir *mockSocialIdentityRepo, ar *mockAppRepo, sr *mockSubscriptionRepo, ssr *mockSessionRepo) {
				ar.apps["app-free"] = appFree
				sir.identities["github:12345"] = existingIdentity
				ur.users["user-existing"] = existingUser
				existingSub := &model.Subscription{
					ID:     "sub-1",
					UserID: "user-existing",
					AppID:  "app-free",
					Plan:   "free",
					Status: "active",
				}
				sr.subs["sub-1"] = existingSub
				sr.byUserApp["user-existing:app-free"] = existingSub
			},
			wantErr: false,
			validate: func(t *testing.T, code string, ur *mockUserRepo, sir *mockSocialIdentityRepo, sr *mockSubscriptionRepo) {
				if code == "" {
					t.Error("expected non-empty auth code")
				}
				if len(sr.subs) != 1 {
					t.Errorf("expected 1 subscription (existing), got %d", len(sr.subs))
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ur := newMockUserRepo()
			sir := newMockSocialIdentityRepo()
			ar := newMockAppRepo()
			sr := newMockSubscriptionRepo()
			ssr := newMockSessionRepo()

			tc.setup(ur, sir, ar, sr, ssr)

			tokenSvc := newTokenServiceWithKeys(ssr, sr)
			authSvc := NewAuthService(ur, sir, ar, sr, ssr, tokenSvc)

			code, err := authSvc.AuthorizeOrCreate(ctx, tc.info, tc.appID)

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
				tc.validate(t, code, ur, sir, sr)
			}
		})
	}
}

func TestExchangeCode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	plainSecret := "app-secret-xyz"
	hashedSecret, err := util.HashSecret(plainSecret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	app := &model.App{
		ID:          "app-1",
		Secret:      hashedSecret,
		Name:        "TestApp",
		DefaultPlan: "free",
	}

	validCode := "valid-auth-code-1"
	validCodeHash := hashToken(validCode)

	// Helper to create a fresh session copy for each test (avoids shared pointer mutation)
	newValidSession := func() *model.Session {
		return &model.Session{
			ID:           "sess-auth-1",
			UserID:       "user-1",
			AppID:        "app-1",
			RefreshToken: validCodeHash,
			Scope:        []string{"app:read"},
			Revoked:      false,
			ExpiresAt:    timeNow().Add(10 * time.Minute),
		}
	}

	tests := []struct {
		name        string
		code        string
		appID       string
		appSecret   string
		setup       func(*mockAppRepo, *mockSessionRepo, *mockSubscriptionRepo)
		wantErr     bool
		errContains string
	}{
		{
			name:      "valid exchange",
			code:      validCode,
			appID:     "app-1",
			appSecret: plainSecret,
			setup: func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {
				ar.apps["app-1"] = app
				sess := newValidSession()
				ssr.sessions[sess.ID] = sess
				ssr.byToken[validCodeHash] = sess
			},
			wantErr: false,
		},
		{
			name:        "wrong app secret",
			code:        validCode,
			appID:       "app-1",
			appSecret:   "wrong-secret",
			setup: func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {
				ar.apps["app-1"] = app
			},
			wantErr:     true,
			errContains: "invalid app secret",
		},
		{
			name:        "invalid code",
			code:        "nonexistent-code",
			appID:       "app-1",
			appSecret:   plainSecret,
			setup: func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {
				ar.apps["app-1"] = app
			},
			wantErr:     true,
			errContains: "invalid or expired authorization code",
		},
		{
			name:        "code for wrong app",
			code:        validCode,
			appID:       "app-other",
			appSecret:   plainSecret,
			setup: func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {
				ar.apps["app-1"] = app
				ar.apps["app-other"] = &model.App{
					ID:     "app-other",
					Secret: hashedSecret,
					Name:   "OtherApp",
				}
				sess := newValidSession()
				ssr.sessions[sess.ID] = sess
				ssr.byToken[validCodeHash] = sess
			},
			wantErr:     true,
			errContains: "code was not issued for this app",
		},
		{
			name:        "app not found",
			code:        validCode,
			appID:       "nonexistent",
			appSecret:   plainSecret,
			setup:       func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {},
			wantErr:     true,
			errContains: "app not found",
		},
		{
			name:        "new session creation failure during exchange",
			code:        validCode,
			appID:       "app-1",
			appSecret:   plainSecret,
			setup: func(ar *mockAppRepo, ssr *mockSessionRepo, sr *mockSubscriptionRepo) {
				ar.apps["app-1"] = app
				sess := newValidSession()
				ssr.sessions[sess.ID] = sess
				ssr.byToken[validCodeHash] = sess
				// The auth code session was pre-seeded (not via Create). The next Create
				// call will be for the new refresh token session — make that fail.
				ssr.failAfter = 1
			},
			wantErr:     true,
			errContains: "session create failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ar := newMockAppRepo()
			ssr := newMockSessionRepo()
			sr := newMockSubscriptionRepo()

			tc.setup(ar, ssr, sr)

			tokenSvc := newTokenServiceWithKeys(ssr, sr)
			authSvc := NewAuthService(newMockUserRepo(), newMockSocialIdentityRepo(), ar, sr, ssr, tokenSvc)

			accessToken, refreshToken, err := authSvc.ExchangeCode(ctx, tc.code, tc.appID, tc.appSecret)

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
			if accessToken == "" {
				t.Error("expected non-empty access token")
			}
			if refreshToken == "" {
				t.Error("expected non-empty refresh token")
			}

			// Verify the access token is valid
			claims, err := tokenSvc.VerifyAccessToken(accessToken)
			if err != nil {
				t.Errorf("access token verification failed: %v", err)
			}
			if claims.Subject != "user-1" {
				t.Errorf("expected subject user-1, got %s", claims.Subject)
			}
			if claims.AppID != "app-1" {
				t.Errorf("expected app_id app-1, got %s", claims.AppID)
			}
		})
	}
}

func TestAuthorizeOrCreate_FreeSubDuplicateNotCreated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	hashedSecret, _ := util.HashSecret("secret")
	app := &model.App{ID: "app-1", Secret: hashedSecret, Name: "Test", DefaultPlan: "free"}

	ur := newMockUserRepo()
	sir := newMockSocialIdentityRepo()
	ar := newMockAppRepo()
	sr := newMockSubscriptionRepo()
	ssr := newMockSessionRepo()

	ar.apps["app-1"] = app

	userID := "user-known"
	ur.users[userID] = &model.User{ID: userID, Status: "active"}
	existingSub := &model.Subscription{ID: "sub-exist", UserID: userID, AppID: "app-1", Plan: "free", Status: "active"}
	sr.subs["sub-exist"] = existingSub
	sr.byUserApp[userID+":app-1"] = existingSub

	sir.identities["github:known-uid"] = &model.SocialIdentity{
		ID:          "ident-known",
		UserID:      userID,
		Provider:    "github",
		ProviderUID: "known-uid",
		Email:       stringPtr("known@example.com"),
	}

	tokenSvc := newTokenServiceWithKeys(ssr, sr)
	authSvc := NewAuthService(ur, sir, ar, sr, ssr, tokenSvc)

	code, err := authSvc.AuthorizeOrCreate(ctx, ProviderUserInfo{
		Provider:    "github",
		ProviderUID: "known-uid",
		Email:       "known@example.com",
		Nickname:    "known",
		AvatarURL:   "",
	}, "app-1")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code == "" {
		t.Error("expected non-empty auth code")
	}

	if len(sr.subs) != 1 {
		t.Errorf("expected 1 subscription, got %d", len(sr.subs))
	}
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

func TestGenerateRefreshToken_Unique(t *testing.T) {
	t.Parallel()

	tokens := make(map[string]bool)
	for i := 0; i < 100; i++ {
		tok := GenerateRefreshToken()
		if tok == "" {
			t.Error("expected non-empty refresh token")
		}
		if tokens[tok] {
			t.Error("generated duplicate refresh token")
		}
		tokens[tok] = true
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"1h", 1 * time.Hour},
		{"168h", 168 * time.Hour},
		{"invalid", 15 * time.Minute},
		{"", 15 * time.Minute},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := parseDuration(tc.input)
			if got != tc.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
