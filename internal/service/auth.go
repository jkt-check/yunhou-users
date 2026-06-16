package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
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

var defaultScope = []string{"app:read", "app:write"}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}


// AuthService handles OAuth callback logic.
type AuthService struct {
	userRepo    repo.UserRepo
	identityRepo repo.SocialIdentityRepo
	appRepo     repo.AppRepo
	subRepo     repo.SubscriptionRepo
	sessionRepo repo.SessionRepo
	tokenSvc    *TokenService
}

func NewAuthService(
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	appRepo repo.AppRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *TokenService,
) *AuthService {
	return &AuthService{
		userRepo:    userRepo,
		identityRepo: identityRepo,
		appRepo:     appRepo,
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

// AuthorizeOrCreate finds or creates a User + SocialIdentity, auto-creates free subscription if needed,
// and returns authorization code for token exchange.
func (s *AuthService) AuthorizeOrCreate(ctx context.Context, info ProviderUserInfo, appID string) (string, error) {
	app, err := s.appRepo.FindByID(ctx, appID)
	if err != nil {
		return "", fmt.Errorf("app not found: %w", err)
	}

	// Check if identity already exists
	existing, err := s.identityRepo.FindByProviderUID(ctx, info.Provider, info.ProviderUID)
	if err == nil && existing != nil {
		return s.createAuthCode(ctx, existing.UserID, app)
	}

	// Try email merge: find another identity with same email
	var userID string
	if info.Email != "" {
		byEmail, err := s.identityRepo.FindByEmail(ctx, info.Email)
		if err == nil && len(byEmail) > 0 {
			userID = byEmail[0].UserID
		}
	}

	// No merge — create new user
	if userID == "" {
		user := &model.User{
			ID:        GenerateUUID(),
			Nickname:  &info.Nickname,
			AvatarURL: &info.AvatarURL,
			Status:    "active",
		}
		if err := s.userRepo.Create(ctx, user); err != nil {
			return "", fmt.Errorf("create user: %w", err)
		}
		userID = user.ID
	}

	// Create social identity — catch duplicate key (race with concurrent login)
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
				return s.createAuthCode(ctx, existing.UserID, app)
			}
		}
		return "", fmt.Errorf("create identity: %w", err)
	}

	// Auto-create free subscription if app has free default plan
	if app.DefaultPlan == "free" {
		sub := &model.Subscription{
			ID:     GenerateUUID(),
			UserID: userID,
			AppID:  appID,
			Plan:   "free",
			Status: "active",
		}
		if err := s.subRepo.Create(ctx, sub); err != nil && !isDuplicateKey(err) {
			return "", fmt.Errorf("auto-create free subscription: %w", err)
		}
	}

	return s.createAuthCode(ctx, userID, app)
}

func (s *AuthService) createAuthCode(ctx context.Context, userID string, app *model.App) (string, error) {
	// Generate a short-lived authorization code
	raw, err := GenerateRefreshToken()
	if err != nil {
		return "", fmt.Errorf("generate code: %w", err)
	}
	code := raw[:32]

	// Store as temporary session to be exchanged later
	// In production, use Redis with TTL; here we store in sessions table with short expiry
	scope := defaultScope
	session := &model.Session{
		ID:           GenerateUUID(),
		UserID:       userID,
		AppID:        app.ID,
		RefreshToken: hashToken(code),
		Scope:        scope,
		Revoked:      false,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	}
	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	return code, nil
}

// ExchangeCode swaps authorization code for access + refresh tokens.
func (s *AuthService) ExchangeCode(ctx context.Context, code, appID, appSecret string) (accessToken, refreshToken string, err error) {
	app, err := s.appRepo.FindByID(ctx, appID)
	if err != nil {
		return "", "", fmt.Errorf("app not found")
	}
	if !util.CheckSecret(app.Secret, appSecret) {
		return "", "", fmt.Errorf("invalid app secret")
	}

	session, err := s.sessionRepo.FindByRefreshToken(ctx, hashToken(code))
	if err != nil {
		return "", "", fmt.Errorf("invalid or expired authorization code")
	}
	if session.AppID != appID {
		return "", "", fmt.Errorf("code was not issued for this app")
	}

	// Check active subscription before consuming the auth code
	sub, err := s.subRepo.FindByUserApp(ctx, session.UserID, session.AppID)
	if err != nil || sub == nil || sub.Status != "active" {
		return "", "", fmt.Errorf("subscription not active")
	}

	// Atomically revoke the auth code session to prevent replay
	revoked, err := s.sessionRepo.RevokeIfNotRevoked(ctx, session.ID)
	if err != nil {
		return "", "", fmt.Errorf("revoke auth code: %w", err)
	}
	if !revoked {
		return "", "", fmt.Errorf("authorization code already used")
	}

	accessToken, err = s.tokenSvc.SignAccessToken(session.UserID, session.AppID, session.Scope)
	if err != nil {
		return "", "", err
	}

	var tokenErr error
	refreshToken, tokenErr = GenerateRefreshToken()
	if tokenErr != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", tokenErr)
	}
	newSession := &model.Session{
		ID:           GenerateUUID(),
		UserID:       session.UserID,
		AppID:        session.AppID,
		RefreshToken: hashToken(refreshToken),
		Scope:        defaultScope,
		ExpiresAt:    time.Now().Add(parseDuration(s.tokenSvc.RefreshTTL)),
	}
	if err := s.sessionRepo.Create(ctx, newSession); err != nil {
		return "", "", err
	}

	return accessToken, refreshToken, nil
}
