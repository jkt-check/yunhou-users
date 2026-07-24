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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

func GenerateUUID() string {
	return uuid.New().String()
}

// refreshTokenReader is a package-level indirection for rand.Read.
// Tests can override it (via refreshTokenReader = failingReader) to
// drive the error path without touching the system entropy source.
var refreshTokenReader = rand.Read

func GenerateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := refreshTokenReader(b); err != nil {
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

// peekSubscription returns the user's status='active' subscription plus a
// boolean flag indicating whether its `expires_at` (if set) is in the past.
// It does NOT raise an error for an expired row — the decision of whether
// an expired subscription blocks login or merely downgrades the response
// is the caller's. The historical `findUsableSubscription` returned
// ErrSubscriptionExpired and propagated it all the way to the OAuth
// callback's redirect, which kicked the user back to /auth/login with a
// banner — a coupling between identity and ability that the user
// rejected on 2026-07-23. The split between authentication (who you are)
// and subscription enforcement (what you can do) lives here: peek is
// pure-read; the policy is up to the caller.
func (s *AuthService) peekSubscription(ctx context.Context, userID string) (*model.Subscription, bool, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get subscription: %w", err)
	}
	expired := sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now())
	return sub, expired, nil
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
	Email  string `json:"email" binding:"required"`
	AppID  string `json:"app_id" binding:"required"`
	PlanID string `json:"-"`
}

// LoginResponse is the response returned by every successful login path
// (the GitHub OAuth redirect flow's callback and the dev-only /test/login).
type LoginResponse struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token"`
	User         UserInfo          `json:"user"`
	Subscription *SubscriptionInfo `json:"subscription"`
}

type UserInfo struct {
	ID        string  `json:"id"`
	Nickname  *string `json:"nickname,omitempty"`
	Email     *string `json:"email,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
}

type SubscriptionInfo struct {
	PlanID         string     `json:"plan_id"`
	PlanName       string     `json:"plan_name"`
	HasAccess      bool       `json:"has_access"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	IsAcceptingNew bool       `json:"is_accepting_new"`
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

	// 5. Plan + access + token issuance via the shared tail so the
	// GitHub flow and /test/login are byte-for-byte identical at the
	// bottom. The historical call to findUsableSubscription here was
	// deliberately thrown away (_ = sub); its only purpose was to
	// translate expired subscriptions into a login error and bounce
	// the user to /auth/login. The cn-staging 2026-07-23 incident
	// proved that conflation wrong — login and subscription are
	// independent concerns, so peekSubscription is invoked inside
	// issueTokensForUser only.
	return s.issueTokensForUser(ctx, user, appID, nil)
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
//
// Security: identities whose provider_uid carries the dev-only TestLogin
// marker (l3-e2e-...) are excluded from the merge set. Otherwise an
// attacker who can reach the gated /test/login endpoint could
// pre-emptively register a victim's email as a synthetic github identity
// and absorb the victim's real GitHub login into their account on the
// victim's next OAuth round-trip. The TestLogin endpoint stays gated
// behind PAYPAL_L3_E2E_MODE=1 in production; this filter is defence in
// depth in case the env is set by mistake.
func (s *AuthService) resolveOrCreateUser(ctx context.Context, info *ProviderUserInfo) (string, bool, error) {
	if info.Email != "" {
		byEmail, err := s.identityRepo.FindByEmail(ctx, info.Email)
		if err == nil {
			for _, ident := range byEmail {
				if isTestIdentityProviderUID(ident.ProviderUID) {
					continue
				}
				return ident.UserID, false, nil
			}
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

// isTestIdentityProviderUID reports whether a social_identities row was
// minted by the dev-only /test/login endpoint. Those rows must never be
// used as merge targets for real OAuth logins.
func isTestIdentityProviderUID(providerUID string) bool {
	return strings.HasPrefix(providerUID, "l3-e2e-")
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

	var requestedPlan *model.Plan
	if req.PlanID != "" {
		requestedPlan, err = s.planRepo.FindByID(ctx, req.PlanID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrPlanNotFound
			}
			return nil, fmt.Errorf("find plan: %w", err)
		}
		if !requestedPlan.IsActive {
			return nil, ErrPlanInactive
		}
		if !requestedPlan.AcceptingNewSubscriptions {
			return nil, ErrPlanNotAcceptingNew
		}
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
		// Create a fresh user + a synthetic identity so future logins
		// (GitHub OAuth or /test/login) can also find this user.
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

	return s.issueTokensForUserWithPlan(ctx, user, req.AppID, &req.Email, requestedPlan)
}

// issueTokensForUser is the shared token-issuance tail used by every
// login path (LoginWithProfile for OAuth callbacks and TestLogin for the
// dev-only /test/login endpoint). Keeping a single implementation guarantees
// refresh-token rotation, JWT claims, session schema, and the (plan,
// has_access) view stay identical across paths.
//
// Callers are responsible for the app existence/active check, user account
// status guard, and email binding. This tail starts at "we have a verified
// user; issue tokens for this app".
//
// Plan selection for the response and access-token scope:
//   - Active, unexpired subscription: surface that plan; its apps become scope
//     only while the plan remains active.
//   - No subscription: no chosen plan and an empty scope.
//   - Expired subscription: preserve the historical plan in the response for
//     renewal UI, but force the token scope and has_access to empty/false.
//   - TestLogin may supply an explicit requested plan for a user with no
//     subscription; this is not a default-plan fallback.
func (s *AuthService) issueTokensForUser(ctx context.Context, user *model.User, appID string, overrideEmail *string) (*LoginResponse, error) {
	return s.issueTokensForUserWithPlan(ctx, user, appID, overrideEmail, nil)
}

func (s *AuthService) issueTokensForUserWithPlan(ctx context.Context, user *model.User, appID string, overrideEmail *string, requestedPlan *model.Plan) (*LoginResponse, error) {
	chosenPlan, surfacePlanID, surfacePlanName, hasAccess, expiresAt, err := s.resolvePlanForTokenIssuanceWithPlan(ctx, user.ID, appID, requestedPlan)
	if err != nil {
		return nil, err
	}

	scope := scopeForTokenIssuance(chosenPlan, expiresAt)
	accessToken, err := s.tokenSvc.SignAccessToken(user.ID, appID, scope)
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
		Scope:        scope,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(s.tokenSvc.RefreshTTL),
	}
	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	// Pick the email for the response. LoginWithProfile leaves it as
	// user.Email (already set from the matching identity). TestLogin
	// passes the email it looked up via overrideEmail because the
	// synthetic identity's email column is what /test/login's caller
	// actually authenticated against.
	respEmail := user.Email
	if overrideEmail != nil {
		respEmail = overrideEmail
	}

	return &LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshTokenRaw,
		User: UserInfo{
			ID:        user.ID,
			Nickname:  user.Nickname,
			Email:     respEmail,
			AvatarURL: user.AvatarURL,
		},
		Subscription: &SubscriptionInfo{
			PlanID:         surfacePlanID,
			PlanName:       surfacePlanName,
			HasAccess:      hasAccess,
			ExpiresAt:      expiresAt,
			IsAcceptingNew: chosenPlan != nil && chosenPlan.IsActive && chosenPlan.AcceptingNewSubscriptions,
		},
	}, nil
}

// resolvePlanForTokenIssuance collapses the three-state plan decision shared
// by login and refresh. chosenPlan is nil when no active subscription exists;
// an expired subscription preserves its historical plan for the renewal CTA,
// while scopeForTokenIssuance prevents that plan from granting token scope.
func (s *AuthService) resolvePlanForTokenIssuance(ctx context.Context, userID, appID string) (chosenPlan *model.Plan, surfacePlanID, surfacePlanName string, hasAccess bool, expiresAt *time.Time, err error) {
	return s.resolvePlanForTokenIssuanceWithPlan(ctx, userID, appID, nil)
}

// resolvePlanForTokenIssuanceWithPlan additionally accepts the explicit plan
// selected by the dev-only TestLogin path. It is used only when the user has no
// subscription and is not a replacement for the retired default-plan lookup.
func (s *AuthService) resolvePlanForTokenIssuanceWithPlan(ctx context.Context, userID, appID string, requestedPlan *model.Plan) (chosenPlan *model.Plan, surfacePlanID, surfacePlanName string, hasAccess bool, expiresAt *time.Time, err error) {
	sub, expired, err := s.peekSubscription(ctx, userID)
	if err != nil {
		return nil, "", "", false, nil, err
	}

	if sub == nil {
		if requestedPlan == nil {
			return nil, "", "", false, nil, nil
		}
		hasAccess = requestedPlan.IsActive && slices.Contains(requestedPlan.Apps, appID)
		return requestedPlan, requestedPlan.ID, requestedPlan.Name, hasAccess, nil, nil
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		if expired {
			// Keep login/refresh available if a historical plan lookup is
			// temporarily unavailable. The placeholder carries only the
			// subscription id; scopeForTokenIssuance fails closed because
			// the placeholder is not active.
			return &model.Plan{ID: sub.PlanID}, sub.PlanID, "", false, sub.ExpiresAt, nil
		}
		return nil, "", "", false, nil, fmt.Errorf("get plan: %w", err)
	}
	if expired {
		return plan, plan.ID, plan.Name, false, sub.ExpiresAt, nil
	}

	hasAccess = plan.IsActive && slices.Contains(plan.Apps, appID)
	return plan, plan.ID, plan.Name, hasAccess, sub.ExpiresAt, nil
}

// scopeForTokenIssuance returns the currently authorized plan apps. Expired
// subscriptions retain a chosen plan only for response rendering, and inactive
// plans are historical records rather than active authorization grants.
func scopeForTokenIssuance(chosenPlan *model.Plan, expiresAt *time.Time) []string {
	if chosenPlan == nil || !chosenPlan.IsActive || (expiresAt != nil && expiresAt.Before(time.Now())) {
		return []string{}
	}
	return chosenPlan.Apps
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

	chosenPlan, surfacePlanID, surfacePlanName, hasAccess, expiresAt, err := s.resolvePlanForTokenIssuance(ctx, user.ID, appID)
	if err != nil {
		return nil, err
	}

	scope := scopeForTokenIssuance(chosenPlan, expiresAt)
	accessToken, err := s.tokenSvc.SignAccessToken(user.ID, appID, scope)
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
		Scope:        scope,
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
			PlanID:         surfacePlanID,
			PlanName:       surfacePlanName,
			HasAccess:      hasAccess,
			ExpiresAt:      expiresAt,
			IsAcceptingNew: chosenPlan != nil && chosenPlan.IsActive && chosenPlan.AcceptingNewSubscriptions,
		},
	}, nil
}
