package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
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

func (m *mockSocialIdentityRepo) DeleteIfNotLast(_ context.Context, id, userID string) (bool, error) {
	if m.deleteErr != nil {
		return false, m.deleteErr
	}
	identities := m.byUserID[userID]
	if len(identities) <= 1 {
		return false, nil
	}
	// Remove the identity from all indexes
	for i, si := range identities {
		if si.ID == id {
			m.byUserID[userID] = append(identities[:i], identities[i+1:]...)
			break
		}
	}
	key := ""
	for k, v := range m.identities {
		if v.ID == id {
			key = k
			break
		}
	}
	if key != "" {
		delete(m.identities, key)
	}
	return true, nil
}

// --- PlanRepo mock ---

// planRepoTxCall records a tx-scoped write to mockPlanRepo. Used by
// tests that assert on call ordering (e.g. InsertTx must precede
// DeleteTx in DeletePlan so the plan_change_log.plan_id FK is still
// satisfied at INSERT time).
type planRepoTxCall struct {
	op     string // "CreateTx" | "UpdateTx" | "DeleteTx"
	planID string
	at     time.Time
}

type mockPlanRepo struct {
	plans     map[string]*model.Plan
	err       error
	deleteErr error // forces Delete/DeleteTx to fail (e.g. FK violation 23503)
	txCalls   []planRepoTxCall
	// lookupErrForIDs forces FindByID to return the named error for the
	// listed IDs even if a row was previously seeded into plans. Used by
	// tests that model "plan row exists in DB but the lookup observed a
	// transient failure" — currently the
	// TestResolvePlanForTokenIssuance_ExpiredSub_OriginalPlanMissing
	// degraded-mode case.
	lookupErrForIDs map[string]error
	// findByIDForShareTxFn, if non-nil, replaces the default
	// FindByIDForShareTx implementation. Lets tests that exercise the
	// TOCTOU path return a plan-state mismatch between the lock step
	// and the eligibility decision (e.g. plan became inactive between
	// the prior FindByID and the lock) without forcing the mock to model
	// ordered call sequences.
	findByIDForShareTxFn func(ctx context.Context, tx *sqlx.Tx, id string) (*model.Plan, error)
	// findByIDForShareTxErr, if non-nil, makes FindByIDForShareTx return
	// this error verbatim (when findByIDForShareTxFn is nil). Lets tests
	// drive the eligibility-tx error paths (e.g. lock acquire fails).
	findByIDForShareTxErr error
	// withTxFn, if non-nil, replaces the default WithTx implementation.
	// Lets tests inject custom tx-scoped error sequences (e.g. the
	// plan-deactivated-during-tx test fires ErrPlanInactive from the
	// FindByIDForShareTx callback).
	withTxFn func(ctx context.Context, fn func(*sqlx.Tx) error) error
}

func newMockPlanRepo() *mockPlanRepo {
	return &mockPlanRepo{plans: make(map[string]*model.Plan)}
}

func (m *mockPlanRepo) FindAll(_ context.Context) ([]model.Plan, error) {
	if m.err != nil {
		return nil, m.err
	}
	var result []model.Plan
	for _, p := range m.plans {
		result = append(result, *p)
	}
	return result, nil
}

func (m *mockPlanRepo) FindByID(_ context.Context, id string) (*model.Plan, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.lookupErrForIDs != nil {
		if e, ok := m.lookupErrForIDs[id]; ok {
			return nil, e
		}
	}
	p, ok := m.plans[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return p, nil
}

func (m *mockPlanRepo) FindByApp(_ context.Context, appID string) ([]model.Plan, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []model.Plan
	for _, p := range m.plans {
		if !p.IsActive {
			continue
		}
		for _, a := range p.Apps {
			if a == appID {
				out = append(out, *p)
				break
			}
		}
	}
	// Mirror the production SQL ORDER BY (display_order ASC, created_at
	// ASC, id ASC) so service-level tests that depend on display_order
	// ordering see deterministic results without a real DB. The mock
	// does NOT enforce is_listed (the production SQL does); callers
	// that need is_listed filtering must model it explicitly.
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayOrder != out[j].DisplayOrder {
			return out[i].DisplayOrder < out[j].DisplayOrder
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *mockPlanRepo) Create(_ context.Context, p *model.Plan) error {
	if m.err != nil {
		return m.err
	}
	m.plans[p.ID] = p
	return nil
}

func (m *mockPlanRepo) Update(_ context.Context, p *model.Plan) error {
	if m.err != nil {
		return m.err
	}
	m.plans[p.ID] = p
	return nil
}

func (m *mockPlanRepo) Delete(_ context.Context, id string) error {
	if m.err != nil {
		return m.err
	}
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.plans, id)
	return nil
}

// CreateTx / UpdateTx / DeleteTx are the tx-scoped counterparts of
// Create / Update / Delete. The mock treats them the same as the
// non-tx variants — there is no separate tx state to model. Service
// tests verify call sequences via the captured state in m.plans +
// changeLogRepo.calls + m.txCalls (the latter is appended in
// timestamp order so tests that need to assert InsertTx happens
// before DeleteTx can sort both mock slices by `at`).
func (m *mockPlanRepo) CreateTx(_ context.Context, _ *sqlx.Tx, p *model.Plan) error {
	m.txCalls = append(m.txCalls, planRepoTxCall{op: "CreateTx", planID: p.ID, at: time.Now()})
	return m.Create(context.Background(), p)
}
func (m *mockPlanRepo) UpdateTx(_ context.Context, _ *sqlx.Tx, p *model.Plan) error {
	m.txCalls = append(m.txCalls, planRepoTxCall{op: "UpdateTx", planID: p.ID, at: time.Now()})
	return m.Update(context.Background(), p)
}
func (m *mockPlanRepo) DeleteTx(_ context.Context, _ *sqlx.Tx, id string) error {
	m.txCalls = append(m.txCalls, planRepoTxCall{op: "DeleteTx", planID: id, at: time.Now()})
	return m.Delete(context.Background(), id)
}

// FindByIDForShareTx is the D8 mock for the eligibility-tx plan lookup.
// Default behaviour: returns the same plan as FindByID but lets tests
// drive an "activated → deactivated between FindByID and lock" race via
// findByIDForShareTxFn / findByIDForShareTxErr. The mock treats the tx
// as a black box — it does not model rollback semantics.
func (m *mockPlanRepo) FindByIDForShareTx(ctx context.Context, tx *sqlx.Tx, id string) (*model.Plan, error) {
	if m.findByIDForShareTxFn != nil {
		return m.findByIDForShareTxFn(ctx, tx, id)
	}
	if m.findByIDForShareTxErr != nil {
		return nil, m.findByIDForShareTxErr
	}
	return m.FindByID(ctx, id)
}

// WithTx is the D8 mock for the planner mutation transaction. The
// real implementation BeginTx'es, runs fn against the tx, then commits.
// The mock passes nil to fn — PlanService's tx-bound writes go through
// interface methods (CreateTx / UpdateTx / DeleteTx / InsertTx) that
// ignore the tx, so fn runs end-to-end against the mock's
// capture-only state.
//
// Tests that need a fully-controlled fn body (e.g. to drive a
// FOR SHARE step error) supply withTxFn to bypass this dispatch.
func (m *mockPlanRepo) WithTx(ctx context.Context, fn func(*sqlx.Tx) error) error {
	if m.withTxFn != nil {
		return m.withTxFn(ctx, fn)
	}
	return fn(nil)
}

// --- PlanChangeLogRepo mock ---

// planChangeLogCall records one Insert / InsertTx invocation. The `at`
// timestamp lets tests that span multiple mocks (e.g. asserting
// InsertTx happens before DeleteTx in DeletePlan) correlate across
// mockPlanChangeLogRepo.calls and mockPlanRepo.txCalls.
type planChangeLogCall struct {
	planID     string
	actorID    string
	changeType string
	before     *model.Plan
	after      *model.Plan
	at         time.Time
}

type mockPlanChangeLogRepo struct {
	insertFunc func(context.Context, string, string, string, *model.Plan, *model.Plan) error
	calls      []planChangeLogCall
}

func newMockPlanChangeLogRepo() *mockPlanChangeLogRepo {
	return &mockPlanChangeLogRepo{}
}

func (m *mockPlanChangeLogRepo) Insert(ctx context.Context, planID, actorID, changeType string, before, after *model.Plan) error {
	m.calls = append(m.calls, planChangeLogCall{
		planID:     planID,
		actorID:    actorID,
		changeType: changeType,
		before:     before,
		after:      after,
		at:         time.Now(),
	})
	if m.insertFunc != nil {
		return m.insertFunc(ctx, planID, actorID, changeType, before, after)
	}
	return nil
}

// InsertTx mirrors Insert but accepts a *sqlx.Tx (ignored by the mock).
// Used by CreatePlan/UpdatePlan/DeletePlan to participate in the
// PlanRepo.WithTx wrapper. Service-level tests only need to verify the
// call sequence; rollback semantics require a real test DB.
func (m *mockPlanChangeLogRepo) InsertTx(ctx context.Context, _ *sqlx.Tx, planID, actorID, changeType string, before, after *model.Plan) error {
	m.calls = append(m.calls, planChangeLogCall{
		planID:     planID,
		actorID:    actorID,
		changeType: changeType,
		before:     before,
		after:      after,
		at:         time.Now(),
	})
	if m.insertFunc != nil {
		return m.insertFunc(ctx, planID, actorID, changeType, before, after)
	}
	return nil
}

// --- SubscriptionRepo mock ---

type mockSubscriptionRepo struct {
	subs      map[string]*model.Subscription // key: id
	byUserID  map[string]*model.Subscription // key: userID (only active)
	createErr error
	findErr   error
	updateErr error
	renewErr  error
}

func newMockSubscriptionRepo() *mockSubscriptionRepo {
	return &mockSubscriptionRepo{
		subs:     make(map[string]*model.Subscription),
		byUserID: make(map[string]*model.Subscription),
	}
}

func (m *mockSubscriptionRepo) Create(_ context.Context, s *model.Subscription) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.subs[s.ID] = s
	if s.Status == "active" {
		m.byUserID[s.UserID] = s
	}
	return nil
}

func (m *mockSubscriptionRepo) FindActiveByUserID(_ context.Context, userID string) (*model.Subscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	s, ok := m.byUserID[userID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	// Defensive: a nil entry in byUserID returns (nil, nil) — drives
	// the "sub == nil" defensive branch in callers.
	if s == nil {
		return nil, nil
	}
	// Expiry is decided by AuthService.peekSubscription in production.
	// The mock returns the row verbatim so callers can drive both the
	// "active, not expired" and "active, past expires_at" branches
	// deterministically without the mock time-gating the result.
	return s, nil
}

func (m *mockSubscriptionRepo) FindActiveByUserIDTx(_ context.Context, _ *sqlx.Tx, userID string) (*model.Subscription, error) {
	return m.FindActiveByUserID(context.Background(), userID)
}

func (m *mockSubscriptionRepo) FindByID(_ context.Context, id string) (*model.Subscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	s, ok := m.subs[id]
	if !ok {
		return nil, sql.ErrNoRows
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
	if status != "active" {
		delete(m.byUserID, s.UserID)
	}
	return nil
}

func (m *mockSubscriptionRepo) Renew(_ context.Context, id string, expiresAt *time.Time) error {
	if m.renewErr != nil {
		return m.renewErr
	}
	s, ok := m.subs[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	s.Status = "active"
	s.ExpiresAt = expiresAt
	m.byUserID[s.UserID] = s
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

func (m *mockSessionRepo) FindByRefreshToken(_ context.Context, token string, sessionType string) (*model.Session, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	s, ok := m.byToken[token]
	if !ok {
		return nil, sql.ErrNoRows
	}
	// Real SQL filters `WHERE revoked = false AND expires_at > now()`, so
	// revoked or expired sessions surface as sql.ErrNoRows.
	if s.Revoked {
		return nil, sql.ErrNoRows
	}
	if s.ExpiresAt.Before(time.Now()) {
		return nil, sql.ErrNoRows
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

func (m *mockSessionRepo) RevokeIfNotRevoked(_ context.Context, id string) (bool, error) {
	if m.revokeErr != nil {
		return false, m.revokeErr
	}
	s, ok := m.sessions[id]
	if !ok || s.Revoked {
		return false, nil
	}
	s.Revoked = true
	return true, nil
}

func (m *mockSessionRepo) RotateRefresh(_ context.Context, oldID string, newSession *model.Session) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.createCount++
	if m.failAfter > 0 && m.createCount >= m.failAfter {
		return fmt.Errorf("session create failed on call %d", m.createCount)
	}
	s, ok := m.sessions[oldID]
	if !ok || s.Revoked {
		return fmt.Errorf("session already revoked")
	}
	s.Revoked = true
	m.sessions[newSession.ID] = newSession
	m.byToken[newSession.RefreshToken] = newSession
	return nil
}

func (m *mockSessionRepo) ExchangeAuthCode(_ context.Context, oldID string, newSession *model.Session) (bool, error) {
	if m.createErr != nil {
		return false, m.createErr
	}
	m.createCount++
	if m.failAfter > 0 && m.createCount >= m.failAfter {
		return false, fmt.Errorf("session create failed on call %d", m.createCount)
	}
	s, ok := m.sessions[oldID]
	if !ok || s.Revoked {
		return false, nil
	}
	s.Revoked = true
	m.sessions[newSession.ID] = newSession
	m.byToken[newSession.RefreshToken] = newSession
	return true, nil
}

func (m *mockSessionRepo) RevokeFamilyByUserApp(_ context.Context, userID, appID string) error {
	for _, s := range m.sessions {
		if s.UserID == userID && s.AppID == appID && !s.Revoked {
			s.Revoked = true
		}
	}
	return nil
}

// --- AppRepo mock ---
//
// The AuthService now verifies the requested app exists and is active before
// issuing tokens. Tests need to seed apps for the requests they drive.

type mockAppRepo struct {
	apps      map[string]*model.App
	findErr   error
	listErr   error
	createErr error
	updateErr error
}

func newMockAppRepo() *mockAppRepo {
	return &mockAppRepo{apps: make(map[string]*model.App)}
}

func (m *mockAppRepo) seedActive(appID, name string) {
	m.apps[appID] = &model.App{AppID: appID, Name: name, IsActive: true}
}

func (m *mockAppRepo) seedInactive(appID, name string) {
	m.apps[appID] = &model.App{AppID: appID, Name: name, IsActive: false}
}

func (m *mockAppRepo) FindByID(_ context.Context, id string) (*model.App, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	a, ok := m.apps[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return a, nil
}

func (m *mockAppRepo) Create(_ context.Context, a *model.App) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.apps[a.AppID] = a
	return nil
}

func (m *mockAppRepo) Update(_ context.Context, a *model.App) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.apps[a.AppID] = a
	return nil
}

func (m *mockAppRepo) List(_ context.Context) ([]model.App, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]model.App, 0, len(m.apps))
	for _, a := range m.apps {
		out = append(out, *a)
	}
	return out, nil
}

func (m *mockAppRepo) ListUnhashed(_ context.Context) ([]model.App, error) {
	out := make([]model.App, 0, len(m.apps))
	for _, a := range m.apps {
		if a.SecretHash == "" {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (m *mockAppRepo) RotateSecretHash(_ context.Context, appID, newHash string) error {
	a, ok := m.apps[appID]
	if !ok {
		return sql.ErrNoRows
	}
	a.SecretHash = newHash
	return nil
}

func (m *mockAppRepo) BackfillSecretHash(_ context.Context, appID, newHash string) (bool, error) {
	a, ok := m.apps[appID]
	if !ok {
		return false, sql.ErrNoRows
	}
	if a.SecretHash != "" {
		// Mimic the production guard: a concurrent rotate already populated
		// the hash; skip rather than overwrite.
		return true, nil
	}
	a.SecretHash = newHash
	return false, nil
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
		AccessTTL:   15 * time.Minute,
		RefreshTTL:  168 * time.Hour,
		SessionRepo: sessionRepo,
		SubRepo:     subRepo,
	}
}

func newTokenServiceWithMocks(sessionRepo *mockSessionRepo, subRepo *mockSubscriptionRepo) *TokenService {
	priv, pub := generateTestRSAKeyPair()
	return &TokenService{
		PrivateKey:  priv,
		PublicKey:   pub,
		AccessTTL:   15 * time.Minute,
		RefreshTTL:  168 * time.Hour,
		SessionRepo: sessionRepo,
		SubRepo:     subRepo,
	}
}

// stringPtr returns a pointer to the given string.
func stringPtr(s string) *string { return &s }

// Ensure util package is referenced (used in auth_test.go)
var _ = util.HashSecret

// duplicateKeyError is a test double that satisfies isDuplicateKey.
type duplicateKeyError struct{}

func (d *duplicateKeyError) Error() string      { return "duplicate key" }
func (d *duplicateKeyError) DuplicateKey() bool { return true }
