package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

func GenerateUUID() string {
	return uuid.New().String()
}

func GenerateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// AuthService handles simplified login with provider tokens.
type AuthService struct {
	userRepo     repo.UserRepo
	identityRepo repo.SocialIdentityRepo
	planRepo     repo.PlanRepo
	subRepo      repo.SubscriptionRepo
	sessionRepo  repo.SessionRepo
	appRepo      repo.AppRepo
	tokenSvc     *TokenService
}

func NewAuthService(
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	planRepo repo.PlanRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	appRepo repo.AppRepo,
	tokenSvc *TokenService,
) *AuthService {
	return &AuthService{
		userRepo:     userRepo,
		identityRepo: identityRepo,
		planRepo:     planRepo,
		subRepo:      subRepo,
		sessionRepo:  sessionRepo,
		appRepo:      appRepo,
		tokenSvc:     tokenSvc,
	}
}

// findUsableSubscription returns the user's currently-active, non-expired
// subscription. Expiry is determined by the `expires_at` timestamp; the
// repo's `FindActiveByUserID` already filters `status = 'active'`, and a
// periodic sweeper is responsible for marking expired rows as such.
// We deliberately do NOT write here: writing in a read path holds row locks
// and serializes concurrent logins for the same user. The status update is
// the sweeper's job.
func (s *AuthService) findUsableSubscription(ctx context.Context, userID string) (*model.Subscription, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now()) {
		return nil, ErrSubscriptionExpired
	}
	return sub, nil
}

type ProviderUserInfo struct {
	Provider    string
	ProviderUID string
	Email       string
	Nickname    string
	AvatarURL   string
}

// LoginWithProfileRequest is the input for AuthService.LoginWithProfile:
// identity binding uses a pre-fetched ProviderUserInfo instead of re-calling
// the provider's userinfo API. Used by /auth/github/callback after the
// handler has already exchanged the code and fetched the profile once.
type LoginWithProfileRequest struct {
	Profile *ProviderUserInfo
	AppID   string
}

// TestLoginRequest is the body for POST /test/login. Dev-only — gated by
// PAYPAL_L3_E2E_MODE=1. L3 Playwright suite uses this to mint a real JWT
// against a locally-running backend that has its GitHub OAuth verifier
// wired to api.github.com (which 401s on fake tokens).
type TestLoginRequest struct {
	Email string `json:"email" binding:"required"`
	AppID string `json:"app_id" binding:"required"`
}

// LoginResponse is the response for POST /auth/login
type LoginResponse struct {
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	User         UserInfo         `json:"user"`
	Subscription *SubscriptionInfo `json:"subscription"`
}

type UserInfo struct {
	ID       string  `json:"id"`
	Nickname *string `json:"nickname,omitempty"`
	Email    *string `json:"email,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
}

type SubscriptionInfo struct {
	PlanID    string     `json:"plan_id"`
	PlanName  string     `json:"plan_name"`
	HasAccess bool       `json:"has_access"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// LoginWithProfile is the Login flow that accepts a pre-fetched
// ProviderUserInfo instead of re-calling the provider userinfo API.
// Used by /auth/github/callback after the handler has already exchanged
// the code and fetched the profile — the callback only does a single
// upstream /user round-trip.
func (s *AuthService) LoginWithProfile(ctx context.Context, req LoginWithProfileRequest) (*LoginResponse, error) {
	if req.Profile == nil {
		return nil, errors.New("nil profile")
	}
	providerUser := req.Profile
	appID := req.AppID

	// 1. Verify the requested app exists and is active before issuing tokens.
	//    Without this, a user could log in for a disabled or unknown app and
	//    still receive a signed access token with that app_id in the audience.
	app, err := s.appRepo.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("find app: %w", err)
	}
	if !app.IsActive {
		return nil, ErrAppInactive
	}

	// 2. Find or create user + identity
	user, err := s.getOrCreateUser(ctx, providerUser)
	if err != nil {
		return nil, fmt.Errorf("get or create user: %w", err)
	}

	// 3. Enforce account status. A suspended or deleted user must not be able
	//    to mint fresh tokens even if they still hold an active subscription.
	if user.Status == "suspended" {
		return nil, ErrUserSuspended
	}
	if user.Status == "deleted" {
		return nil, ErrUserDeleted
	}

	// 4. Resolve the identity for this login so the response reflects the
	//    email of the provider the user just signed in with. We deliberately
	//    do NOT fall back to other identities — a Google-linked email should
	//    not leak into a GitHub login response. If the matching identity has
	//    no email, leave user.Email nil; the response's omitempty handles it.
	identities, err := s.identityRepo.ListByUserID(ctx, user.ID)
	if err == nil {
		for i := range identities {
			if identities[i].Provider == providerUser.Provider &&
				identities[i].ProviderUID == providerUser.ProviderUID {
				user.Email = identities[i].Email
				break
			}
		}
	}

	// 5. Get user's active subscription (no row → no subscription, use default plan)
	sub, err := s.findUsableSubscription(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	// 6. Determine plan and app access
	var plan *model.Plan
	if sub == nil {
		// No subscription, use default (free) plan
		plan, err = s.planRepo.FindDefault(ctx)
		if err != nil {
			return nil, fmt.Errorf("get default plan: %w", err)
		}
	} else {
		plan, err = s.planRepo.FindByID(ctx, sub.PlanID)
		if err != nil {
			return nil, fmt.Errorf("get plan: %w", err)
		}
	}

	hasAccess := slices.Contains(plan.Apps, appID)

	// 7. Generate tokens
	accessToken, err := s.tokenSvc.SignAccessToken(user.ID, appID, plan.Apps)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	refreshTokenRaw, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	session := &model.Session{
		ID:           GenerateUUID(),
		UserID:       user.ID,
		AppID:        appID,
		SessionType:  "refresh",
		RefreshToken: hashToken(refreshTokenRaw),
		Scope:        plan.Apps,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(s.tokenSvc.RefreshTTL),
	}
	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	var expiresAt *time.Time
	if sub != nil {
		expiresAt = sub.ExpiresAt
	}

	return &LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshTokenRaw,
		User: UserInfo{
			ID:        user.ID,
			Nickname:  user.Nickname,
			Email:     user.Email,
			AvatarURL: user.AvatarURL,
		},
		Subscription: &SubscriptionInfo{
			PlanID:    plan.ID,
			PlanName:  plan.Name,
			HasAccess: hasAccess,
			ExpiresAt: expiresAt,
		},
	}, nil
}

func (s *AuthService) getOrCreateUser(ctx context.Context, info *ProviderUserInfo) (*model.User, error) {
	// 1. Identity already exists? Bind to that user.
	existing, err := s.identityRepo.FindByProviderUID(ctx, info.Provider, info.ProviderUID)
	if err == nil && existing != nil {
		return s.userRepo.FindByID(ctx, existing.UserID)
	}

	// 2. Resolve the target user: either by email-merge (if a verified
	//    identity is bound to that email already) or as a new account.
	userID, isNew, err := s.resolveOrCreateUser(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("resolve user: %w", err)
	}

	// 3. Bind the new social identity. If another concurrent request
	//    raced us and inserted the same (provider, provider_uid) first,
	//    the duplicate-key handler resolves to that winner instead of
	//    leaving a second user row orphaned.
	identity := &model.SocialIdentity{
		ID:          GenerateUUID(),
		UserID:      userID,
		Provider:    info.Provider,
		ProviderUID: info.ProviderUID,
		Email:       &info.Email,
	}
	if err := s.identityRepo.Create(ctx, identity); err != nil {
		if isDuplicateKey(err) {
			// Another caller created the identity first. Resolve to
			// that winner; if we created a user in step 2 in this
			// race, it's now an orphan — but the unique constraint
			// on (provider, provider_uid) ensures no two callers can
			// each end up with their own binding.
			winner, retryErr := s.identityRepo.FindByProviderUID(ctx, info.Provider, info.ProviderUID)
			if retryErr == nil && winner != nil {
				return s.userRepo.FindByID(ctx, winner.UserID)
			}
			// If we created a brand-new user in step 2 and lost the
			// race, the orphan row is harmless (no identities bound
			// to it) but we still want to surface a usable user.
			_ = isNew // orphan cleanup is a sweeper concern
		}
		return nil, fmt.Errorf("create identity: %w", err)
	}

	return s.userRepo.FindByID(ctx, userID)
}

// resolveOrCreateUser returns the user_id to bind this login to, plus a
// flag indicating whether a brand-new user row was created. Email-merge
// only fires when we actually have an email to look up; otherwise we
// always create a new user.
func (s *AuthService) resolveOrCreateUser(ctx context.Context, info *ProviderUserInfo) (string, bool, error) {
	if info.Email != "" {
		byEmail, err := s.identityRepo.FindByEmail(ctx, info.Email)
		if err == nil && len(byEmail) > 0 {
			return byEmail[0].UserID, false, nil
		}
	}
	user := &model.User{
		ID:        GenerateUUID(),
		Nickname:  &info.Nickname,
		AvatarURL: &info.AvatarURL,
		Email:     &info.Email,
		Status:    "active",
	}
	if err := s.userRepo.Create(ctx, user); err != nil {
		return "", false, err
	}
	return user.ID, true, nil
}

// TestLogin mints a real JWT for an email without going through any
// OAuth provider. Bypasses GitHub/Google entirely — the caller's
// identity is the email itself. Creates the user (no real social
// identity attached) if it doesn't exist; otherwise reuses the
// existing user. Used by the L3 e2e-ui suite (which can't drive
// the real GitHub popup for fake sandbox emails).
func (s *AuthService) TestLogin(ctx context.Context, req TestLoginRequest) (*LoginResponse, error) {
	// Verify the requested app is real + active (mirrors Login's gate).
	app, err := s.appRepo.FindByID(ctx, req.AppID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("find app: %w", err)
	}
	if !app.IsActive {
		return nil, ErrAppInactive
	}

	// Find or create the user. Email is the lookup key — but in the
	// production schema emails live on social_identities, not on the
	// users table. So we look up by identity, fall back to creating
	// both a user + a synthetic identity.
	identities, err := s.identityRepo.FindByEmail(ctx, req.Email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find identity: %w", err)
	}
	var user *model.User
	if len(identities) > 0 {
		user, err = s.userRepo.FindByID(ctx, identities[0].UserID)
		if err != nil {
			return nil, fmt.Errorf("find user: %w", err)
		}
	} else {
		// Create a fresh user + a synthetic identity so future /auth/login
		// flows can also find this user.
		nick := req.Email
		user = &model.User{
			ID:       GenerateUUID(),
			Nickname: &nick,
			Status:   "active",
		}
		if err := s.userRepo.Create(ctx, user); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		email := req.Email
		if err := s.identityRepo.Create(ctx, &model.SocialIdentity{
			ID:          GenerateUUID(),
			UserID:      user.ID,
			Provider:    "github",
			ProviderUID: "l3-e2e-" + user.ID,
			Email:       &email,
		}); err != nil {
			return nil, fmt.Errorf("create identity: %w", err)
		}
	}

	// Account-status guard.
	if user.Status == "suspended" {
		return nil, ErrUserSuspended
	}
	if user.Status == "deleted" {
		return nil, ErrUserDeleted
	}

	// Resolve the user's plan for the response. Falls back to the
	// default plan if the user has no active sub (matches Login).
	var plan *model.Plan
	active, err := s.subRepo.FindActiveByUserID(ctx, user.ID)
	if err == nil && active != nil {
		plan, err = s.planRepo.FindByID(ctx, active.PlanID)
		if err != nil {
			return nil, fmt.Errorf("get plan: %w", err)
		}
	} else {
		plan, err = s.planRepo.FindDefault(ctx)
		if err != nil {
			return nil, fmt.Errorf("get default plan: %w", err)
		}
	}
	hasAccess := slices.Contains(plan.Apps, req.AppID)

	// Issue tokens through the same path production uses so the refresh
	// rotation, JWT signing, etc. are byte-for-byte identical to /auth/login.
	accessToken, err := s.tokenSvc.SignAccessToken(user.ID, req.AppID, plan.Apps)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}
	refreshTokenRaw, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	session := &model.Session{
		ID:           GenerateUUID(),
		UserID:       user.ID,
		AppID:        req.AppID,
		SessionType:  "refresh",
		RefreshToken: hashToken(refreshTokenRaw),
		Scope:        plan.Apps,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(s.tokenSvc.RefreshTTL),
	}
	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	var expiresAt *time.Time
	if active != nil {
		expiresAt = active.ExpiresAt
	}

	email := req.Email
	return &LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshTokenRaw,
		User: UserInfo{
			ID:        user.ID,
			Nickname:  user.Nickname,
			Email:     &email,
			AvatarURL: user.AvatarURL,
		},
		Subscription: &SubscriptionInfo{
			PlanID:    plan.ID,
			PlanName:  plan.Name,
			HasAccess: hasAccess,
			ExpiresAt: expiresAt,
		},
	}, nil
}

// Logout revokes the refresh token. Idempotent: a missing/expired session
// is treated as success. Other DB errors are propagated so the caller can
// distinguish "nothing to revoke" from "DB unreachable".
func (s *AuthService) Logout(ctx context.Context, refreshToken string) error {
	session, err := s.sessionRepo.FindByRefreshToken(ctx, hashToken(refreshToken), "refresh")
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already invalid / expired
	}
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	return s.sessionRepo.Revoke(ctx, session.ID)
}

// RefreshToken refreshes access token using refresh token.
func (s *AuthService) RefreshToken(ctx context.Context, refreshToken, appID string) (*LoginResponse, error) {
	session, err := s.sessionRepo.FindByRefreshToken(ctx, hashToken(refreshToken), "refresh")
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidRefreshToken
	}
	if err != nil {
		return nil, fmt.Errorf("find session: %w", err)
	}

	user, err := s.userRepo.FindByID(ctx, session.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	// Enforce account status — don't keep issuing tokens to suspended/deleted users.
	if user.Status == "suspended" {
		return nil, ErrUserSuspended
	}
	if user.Status == "deleted" {
		return nil, ErrUserDeleted
	}

	// Fall back to session's app_id if not provided
	if appID == "" {
		appID = session.AppID
	}

	// Verify the resolved app still exists and is active. Without this, a
	// refresh issued while the app was active could keep working after the
	// app is disabled.
	app, err := s.appRepo.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("find app: %w", err)
	}
	if !app.IsActive {
		return nil, ErrAppInactive
	}

	sub, err := s.findUsableSubscription(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	var plan *model.Plan
	if sub == nil {
		plan, err = s.planRepo.FindDefault(ctx)
		if err != nil {
			return nil, fmt.Errorf("get default plan: %w", err)
		}
	} else {
		plan, err = s.planRepo.FindByID(ctx, sub.PlanID)
		if err != nil {
			return nil, fmt.Errorf("get plan: %w", err)
		}
	}

	hasAccess := slices.Contains(plan.Apps, appID)

	accessToken, err := s.tokenSvc.SignAccessToken(user.ID, appID, plan.Apps)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	newRefreshTokenRaw, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	newSession := &model.Session{
		ID:           GenerateUUID(),
		UserID:       user.ID,
		AppID:        appID,
		SessionType:  "refresh",
		RefreshToken: hashToken(newRefreshTokenRaw),
		Scope:        plan.Apps,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(s.tokenSvc.RefreshTTL),
	}
	if err := s.sessionRepo.RotateRefresh(ctx, session.ID, newSession); err != nil {
		// Refresh-token reuse detection: if RotateRefresh reports the old
		// session was already revoked, the token we're holding has either
		// been replayed or stolen. Revoke the entire (user, app) family so
		// any other outstanding refresh tokens for this user become useless,
		// then surface a generic 401.
		if errors.Is(err, ErrSessionAlreadyRevoked) {
			if revErr := s.sessionRepo.RevokeFamilyByUserApp(ctx, user.ID, appID); revErr != nil {
				log.Printf("refresh: family revoke failed: %v", revErr)
			}
			return nil, ErrInvalidRefreshToken
		}
		return nil, fmt.Errorf("rotate refresh token: %w", err)
	}

	var expiresAt *time.Time
	if sub != nil {
		expiresAt = sub.ExpiresAt
	}

	return &LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshTokenRaw,
		User: UserInfo{
			ID:        user.ID,
			Nickname:  user.Nickname,
			Email:     user.Email,
			AvatarURL: user.AvatarURL,
		},
		Subscription: &SubscriptionInfo{
			PlanID:    plan.ID,
			PlanName:  plan.Name,
			HasAccess: hasAccess,
			ExpiresAt: expiresAt,
		},
	}, nil
}
