package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

// --- UserRepo mock ---

type mockUserRepo struct {
	users map[string]*model.User
	err   error
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{users: make(map[string]*model.User)}
}

func (m *mockUserRepo) Create(_ context.Context, u *model.User) error {
	if m.err != nil {
		return m.err
	}
	m.users[u.ID] = u
	return nil
}

func (m *mockUserRepo) FindByID(_ context.Context, id string) (*model.User, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.users[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return u, nil
}

func (m *mockUserRepo) Update(_ context.Context, u *model.User) error {
	if m.err != nil {
		return m.err
	}
	m.users[u.ID] = u
	return nil
}

// --- SocialIdentityRepo mock ---

type mockSocialIdentityRepo struct {
	identities     map[string]*model.SocialIdentity // key: provider:providerUID
	byEmail        map[string][]model.SocialIdentity
	byUserID       map[string][]model.SocialIdentity
	createErr      error
	findErr        error
	findByEmailErr error
	deleteErr      error
	countByUserErr error
}

func newMockSocialIdentityRepo() *mockSocialIdentityRepo {
	return &mockSocialIdentityRepo{
		identities: make(map[string]*model.SocialIdentity),
		byEmail:    make(map[string][]model.SocialIdentity),
		byUserID:   make(map[string][]model.SocialIdentity),
	}
}

func (m *mockSocialIdentityRepo) Create(_ context.Context, si *model.SocialIdentity) error {
	if m.createErr != nil {
		return m.createErr
	}
	key := si.Provider + ":" + si.ProviderUID
	m.identities[key] = si
	if si.Email != nil && *si.Email != "" {
		m.byEmail[*si.Email] = append(m.byEmail[*si.Email], *si)
	}
	m.byUserID[si.UserID] = append(m.byUserID[si.UserID], *si)
	return nil
}

func (m *mockSocialIdentityRepo) FindByProviderUID(_ context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	key := provider + ":" + providerUID
	si, ok := m.identities[key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return si, nil
}

func (m *mockSocialIdentityRepo) FindByEmail(_ context.Context, email string) ([]model.SocialIdentity, error) {
	if m.findByEmailErr != nil {
		return nil, m.findByEmailErr
	}
	return m.byEmail[email], nil
}

func (m *mockSocialIdentityRepo) ListByUserID(_ context.Context, userID string) ([]model.SocialIdentity, error) {
	return m.byUserID[userID], nil
}

func (m *mockSocialIdentityRepo) Delete(_ context.Context, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	return nil
}

func (m *mockSocialIdentityRepo) CountByUserID(_ context.Context, userID string) (int, error) {
	if m.countByUserErr != nil {
		return 0, m.countByUserErr
	}
	return len(m.byUserID[userID]), nil
}

// --- AppRepo mock ---

type mockAppRepo struct {
	apps map[string]*model.App
	err  error
}

func newMockAppRepo() *mockAppRepo {
	return &mockAppRepo{apps: make(map[string]*model.App)}
}

func (m *mockAppRepo) Create(_ context.Context, a *model.App) error {
	if m.err != nil {
		return m.err
	}
	m.apps[a.ID] = a
	return nil
}

func (m *mockAppRepo) FindByID(_ context.Context, id string) (*model.App, error) {
	if m.err != nil {
		return nil, m.err
	}
	a, ok := m.apps[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return a, nil
}

func (m *mockAppRepo) Update(_ context.Context, a *model.App) error {
	if m.err != nil {
		return m.err
	}
	m.apps[a.ID] = a
	return nil
}

// --- SubscriptionRepo mock ---

type mockSubscriptionRepo struct {
	subs      map[string]*model.Subscription // key: id
	byUserApp map[string]*model.Subscription // key: userID:appID
	createErr error
	findErr   error
	updateErr error
	renewErr  error
}

func newMockSubscriptionRepo() *mockSubscriptionRepo {
	return &mockSubscriptionRepo{
		subs:      make(map[string]*model.Subscription),
		byUserApp: make(map[string]*model.Subscription),
	}
}

func (m *mockSubscriptionRepo) Create(_ context.Context, s *model.Subscription) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.subs[s.ID] = s
	m.byUserApp[s.UserID+":"+s.AppID] = s
	return nil
}

func (m *mockSubscriptionRepo) FindByUserApp(_ context.Context, userID, appID string) (*model.Subscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	key := userID + ":" + appID
	s, ok := m.byUserApp[key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return s, nil
}

func (m *mockSubscriptionRepo) FindByID(_ context.Context, id string) (*model.Subscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	s, ok := m.subs[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return s, nil
}

func (m *mockSubscriptionRepo) ListByUserID(_ context.Context, userID string) ([]model.Subscription, error) {
	var result []model.Subscription
	for _, s := range m.subs {
		if s.UserID == userID {
			result = append(result, *s)
		}
	}
	return result, nil
}

func (m *mockSubscriptionRepo) UpdateStatus(_ context.Context, id, status string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	s, ok := m.subs[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	s.Status = status
	return nil
}

func (m *mockSubscriptionRepo) Renew(_ context.Context, id string, expiresAt interface{}) error {
	if m.renewErr != nil {
		return m.renewErr
	}
	s, ok := m.subs[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	s.Status = "active"
	if t, ok := expiresAt.(*time.Time); ok {
		s.ExpiresAt = t
	}
	return nil
}

// --- SessionRepo mock ---

type mockSessionRepo struct {
	sessions    map[string]*model.Session // key: id
	byToken     map[string]*model.Session // key: refreshToken
	createErr   error
	findErr     error
	revokeErr   error
	createCount int
	failAfter   int // if >0, fail the Nth Create call
}

func newMockSessionRepo() *mockSessionRepo {
	return &mockSessionRepo{
		sessions: make(map[string]*model.Session),
		byToken:  make(map[string]*model.Session),
	}
}

func (m *mockSessionRepo) Create(_ context.Context, s *model.Session) error {
	m.createCount++
	if m.failAfter > 0 && m.createCount >= m.failAfter {
		return fmt.Errorf("session create failed on call %d", m.createCount)
	}
	if m.createErr != nil {
		return m.createErr
	}
	m.sessions[s.ID] = s
	m.byToken[s.RefreshToken] = s
	return nil
}

func (m *mockSessionRepo) FindByRefreshToken(_ context.Context, token string) (*model.Session, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	s, ok := m.byToken[token]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	if s.Revoked {
		return nil, fmt.Errorf("revoked")
	}
	if s.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("expired")
	}
	return s, nil
}

func (m *mockSessionRepo) Revoke(_ context.Context, id string) error {
	if m.revokeErr != nil {
		return m.revokeErr
	}
	s, ok := m.sessions[id]
	if ok {
		s.Revoked = true
	}
	return nil
}

// --- Test RSA key pair helpers ---

func generateTestRSAKeyPair() (*rsa.PrivateKey, *rsa.PublicKey) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("failed to generate test RSA key: %v", err))
	}
	return priv, &priv.PublicKey
}

func newTokenServiceWithKeys(sessionRepo *mockSessionRepo, subRepo *mockSubscriptionRepo) *TokenService {
	priv, pub := generateTestRSAKeyPair()
	return &TokenService{
		PrivateKey:  priv,
		PublicKey:   pub,
		AccessTTL:   "15m",
		RefreshTTL:  "168h",
		SessionRepo: sessionRepo,
		SubRepo:     subRepo,
	}
}

// stringPtr returns a pointer to the given string.
func stringPtr(s string) *string { return &s }

// timeNow returns the current time. Can be overridden for deterministic tests if needed.
var timeNow = func() time.Time { return time.Now() }

// Ensure util package is referenced (used in auth_test.go)
var _ = util.HashSecret
