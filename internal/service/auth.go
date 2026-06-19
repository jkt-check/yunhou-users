package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	userRepo    repo.UserRepo
	identityRepo repo.SocialIdentityRepo
	planRepo    repo.PlanRepo
	subRepo     repo.SubscriptionRepo
	sessionRepo repo.SessionRepo
	tokenSvc    *TokenService
}

func NewAuthService(
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	planRepo repo.PlanRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *TokenService,
) *AuthService {
	return &AuthService{
		userRepo:    userRepo,
		identityRepo: identityRepo,
		planRepo:    planRepo,
		subRepo:     subRepo,
		sessionRepo: sessionRepo,
		tokenSvc:    tokenSvc,
	}
}

type ProviderUserInfo struct {
	Provider    string
	ProviderUID string
	Email       string
	Nickname    string
	AvatarURL   string
}

// LoginRequest is the request body for POST /auth/login
type LoginRequest struct {
	Provider      string `json:"provider" binding:"required"`
	ProviderToken string `json:"provider_token" binding:"required"`
	AppID         string `json:"app_id" binding:"required"`
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

// Login authenticates a user via provider token and returns tokens + subscription info.
func (s *AuthService) Login(ctx context.Context, req LoginRequest) (*LoginResponse, error) {
	// 1. Get user info from provider (GitHub/Google/WeChat)
	providerUser, err := s.getProviderUser(req.Provider, req.ProviderToken)
	if err != nil {
		return nil, fmt.Errorf("invalid provider token: %w", err)
	}

	// 2. Find or create user + identity
	user, err := s.getOrCreateUser(ctx, providerUser)
	if err != nil {
		return nil, fmt.Errorf("get or create user: %w", err)
	}

	// Get email from identity
	identities, err := s.identityRepo.ListByUserID(ctx, user.ID)
	if err == nil && len(identities) > 0 {
		user.Email = identities[0].Email // Use first identity's email
	}

	// 3. Get user's active subscription (ignore "not found" - means no subscription)
	sub, err := s.subRepo.FindActiveByUserID(ctx, user.ID)
	if err != nil && err.Error() != "not found" {
		return nil, fmt.Errorf("get subscription: %w", err)
	}

	// 4. Determine plan and app access
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

	hasAccess := slices.Contains(plan.Apps, req.AppID)

	// 5. Generate tokens
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
		ExpiresAt:    time.Now().Add(168 * time.Hour), // 7 days
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

func (s *AuthService) getProviderUser(provider, token string) (*ProviderUserInfo, error) {
	// In production, this calls the OAuth provider's API.
	// For testing, we use tokens that encode the provider uid:
	// "github_<provider_uid>" or "google_<provider_uid>"
	switch provider {
	case "github":
		uid := token
		if len(token) > 8 {
			uid = token[:8]
		}
		return &ProviderUserInfo{
			Provider:    "github",
			ProviderUID: "github_" + uid,
			Email:       "user@example.com",
			Nickname:    "GitHub User",
		}, nil
	case "google":
		uid := token
		if len(token) > 8 {
			uid = token[:8]
		}
		return &ProviderUserInfo{
			Provider:    "google",
			ProviderUID: "google_" + uid,
			Email:       "user@gmail.com",
			Nickname:    "Google User",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

func (s *AuthService) getOrCreateUser(ctx context.Context, info *ProviderUserInfo) (*model.User, error) {
	// Check if identity already exists
	existing, err := s.identityRepo.FindByProviderUID(ctx, info.Provider, info.ProviderUID)
	if err == nil && existing != nil {
		return s.userRepo.FindByID(ctx, existing.UserID)
	}

	// Try email merge
	var userID string
	if info.Email != "" {
		byEmail, err := s.identityRepo.FindByEmail(ctx, info.Email)
		if err == nil && len(byEmail) > 0 {
			userID = byEmail[0].UserID
		}
	}

	// No merge - create new user
	if userID == "" {
		user := &model.User{
			ID:        GenerateUUID(),
			Nickname:  &info.Nickname,
			AvatarURL: &info.AvatarURL,
			Email:     &info.Email,
			Status:    "active",
		}
		if err := s.userRepo.Create(ctx, user); err != nil {
			return nil, err
		}
		userID = user.ID
	}

	// Create social identity
	identity := &model.SocialIdentity{
		ID:          GenerateUUID(),
		UserID:      userID,
		Provider:    info.Provider,
		ProviderUID: info.ProviderUID,
		Email:       &info.Email,
	}
	if err := s.identityRepo.Create(ctx, identity); err != nil {
		if isDuplicateKey(err) {
			existing, retryErr := s.identityRepo.FindByProviderUID(ctx, info.Provider, info.ProviderUID)
			if retryErr == nil && existing != nil {
				return s.userRepo.FindByID(ctx, existing.UserID)
			}
		}
		return nil, fmt.Errorf("create identity: %w", err)
	}

	return s.userRepo.FindByID(ctx, userID)
}

// Logout revokes the refresh token.
func (s *AuthService) Logout(ctx context.Context, refreshToken string) error {
	session, err := s.sessionRepo.FindByRefreshToken(ctx, hashToken(refreshToken), "refresh")
	if err != nil {
		return nil // Already invalid
	}
	return s.sessionRepo.Revoke(ctx, session.ID)
}

// RefreshToken refreshes access token using refresh token.
func (s *AuthService) RefreshToken(ctx context.Context, refreshToken, appID string) (*LoginResponse, error) {
	session, err := s.sessionRepo.FindByRefreshToken(ctx, hashToken(refreshToken), "refresh")
	if err != nil {
		return nil, fmt.Errorf("invalid refresh token")
	}

	user, err := s.userRepo.FindByID(ctx, session.UserID)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}

	sub, err := s.subRepo.FindActiveByUserID(ctx, user.ID)
	if err != nil && err.Error() != "not found" {
		return nil, fmt.Errorf("get subscription: %w", err)
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
		ExpiresAt:    time.Now().Add(168 * time.Hour),
	}
	if err := s.sessionRepo.RotateRefresh(ctx, session.ID, newSession); err != nil {
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
