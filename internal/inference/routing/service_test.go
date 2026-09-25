package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

// fakeStore is the in-memory AccountStore for routing unit tests. The real
// cross-instance lease semantics are pinned by
// internal/inference/postgres quota_concurrency_test.go; here we only test
// selection/cooldown/keeper behavior.
type fakeStore struct {
	accounts    map[string][]domain.UpstreamAccount
	accountsErr error

	mu         sync.Mutex
	leases     map[string]*domain.ConcurrencyLease
	acquireErr map[string]error // scopeID → error
	released   []string
	renewed    []string
	checked    []string
	fencing    int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts:   map[string][]domain.UpstreamAccount{},
		leases:     map[string]*domain.ConcurrencyLease{},
		acquireErr: map[string]error{},
	}
}

func (s *fakeStore) ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error) {
	if s.accountsErr != nil {
		return nil, s.accountsErr
	}
	return s.accounts[providerID], nil
}

func (s *fakeStore) AcquireLeaseTx(ctx context.Context, w domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquireErr[cmd.ScopeID]; err != nil {
		return nil, err
	}
	s.fencing++
	l := &domain.ConcurrencyLease{
		ID: uuid.NewString(), Scope: cmd.Scope, ScopeID: cmd.ScopeID,
		RequestID: cmd.RequestID, OwnerToken: cmd.OwnerToken, FencingToken: s.fencing,
		State: domain.LeaseHeld, AcquiredAt: cmd.Now, ExpiresAt: cmd.Now.Add(cmd.TTL),
	}
	s.leases[l.ID] = l
	return l, nil
}

func (s *fakeStore) CheckLease(ctx context.Context, leaseID, ownerToken string, fencing int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, leaseID)
	l, ok := s.leases[leaseID]
	if !ok || l.State != domain.LeaseHeld || l.OwnerToken != ownerToken || l.FencingToken != fencing {
		return domain.NewError(domain.CodeConflict, "lease not held")
	}
	return nil
}

func (s *fakeStore) RenewLease(ctx context.Context, leaseID, ownerToken string, fencing int64, newExpiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewed = append(s.renewed, leaseID)
	l, ok := s.leases[leaseID]
	if !ok || l.State != domain.LeaseHeld || l.OwnerToken != ownerToken || l.FencingToken != fencing {
		return domain.NewError(domain.CodeConflict, "lease not held")
	}
	l.ExpiresAt = newExpiresAt
	return nil
}

func (s *fakeStore) ReleaseLease(ctx context.Context, leaseID, ownerToken string, fencing int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, leaseID)
	l, ok := s.leases[leaseID]
	if !ok || l.State != domain.LeaseHeld || l.OwnerToken != ownerToken || l.FencingToken != fencing {
		return domain.NewError(domain.CodeConflict, "lease not held")
	}
	l.State = domain.LeaseReleased
	return nil
}

// testSnapshot builds a one-model snapshot with the given routes.
func testSnapshot(modelID string, routes []domain.ModelRoute, deployments ...domain.Deployment) *catalog.Snapshot {
	snap := &catalog.Snapshot{
		Models:        map[string]domain.Model{},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{},
		RoutesByModel: map[string][]domain.ModelRoute{modelID: routes},
	}
	for _, d := range deployments {
		snap.Deployments[d.ID] = d
	}
	return snap
}

func deployment(id, providerID string, proto domain.Protocol) domain.Deployment {
	return domain.Deployment{
		ID: id, ProviderID: providerID, UpstreamModel: "up-1",
		BaseURL: "https://up.example.com", Protocol: proto,
		Status: domain.DeploymentActive,
	}
}

func account(id, providerID string, conc int) domain.UpstreamAccount {
	return domain.UpstreamAccount{
		ID: id, ProviderID: providerID, CredentialID: uuid.NewString(),
		Status: domain.AccountActive, ConcurrencyLimit: conc,
	}
}

func TestCandidates_CapabilityAndProtocolFilter(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{account("acct-1", "prov-a", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	routes := []domain.ModelRoute{
		{ID: "r1", ModelID: "m", DeploymentID: "dep-openai", Enabled: true, Weight: 1},
		{ID: "r2", ModelID: "m", DeploymentID: "dep-anthropic-no-adapter", Enabled: true, Weight: 1},
		{ID: "r3", ModelID: "m", DeploymentID: "dep-disabled", Enabled: true, Weight: 1},
		{ID: "r4", ModelID: "m", DeploymentID: "dep-openai", Enabled: false, Weight: 1},
		{ID: "r5", ModelID: "m", DeploymentID: "dep-no-tools", Enabled: true, Weight: 1,
			Capabilities: []string{"text"}},
	}
	snap := testSnapshot("m", routes,
		deployment("dep-openai", "prov-a", domain.ProtocolOpenAIChat),
		deployment("dep-anthropic-no-adapter", "prov-a", domain.ProtocolAnthropicMessage),
		deployment("dep-disabled", "prov-a", domain.ProtocolOpenAIChat),
		deployment("dep-no-tools", "prov-a", domain.ProtocolOpenAIChat),
	)
	snap.Deployments["dep-disabled"] = func() domain.Deployment {
		d := snap.Deployments["dep-disabled"]
		d.Status = domain.DeploymentDisabled
		return d
	}()

	// No needs: openai deployment only (adapter-less protocol and disabled
	// deployment excluded).
	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (r1 + r5 on dep-openai/dep-no-tools)", len(cands))
	}
	// Tools need: r5's capability list lacks "tools" → only r1 survives.
	cands, err = svc.Candidates(context.Background(), snap, "m", Needs{Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Deployment.ID != "dep-openai" {
		t.Fatalf("tools candidates = %+v, want dep-openai only", cands)
	}
}

func TestCandidates_NoAccountsMeansUnavailable(t *testing.T) {
	fs := newFakeStore() // no accounts at all
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)
	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r1", ModelID: "m", DeploymentID: "d1", Enabled: true, Weight: 1}},
		deployment("d1", "prov-x", domain.ProtocolOpenAIChat))
	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %d, want 0 (no active accounts)", len(cands))
	}
}

func TestCandidates_CooldownSkipsAccount(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{
		account("acct-1", "prov-a", 4),
		account("acct-2", "prov-a", 4),
	}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, domain.FixedClock{T: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)})
	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r1", ModelID: "m", DeploymentID: "d1", Enabled: true, Weight: 1}},
		deployment("d1", "prov-a", domain.ProtocolOpenAIChat))

	svc.CooldownAfterFailure("acct-1")
	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Account.ID != "acct-2" {
		t.Fatalf("candidates = %+v, want acct-2 only (acct-1 cooling)", cands)
	}
}

func TestCandidates_RoundRobin(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{
		account("acct-1", "prov-a", 4),
		account("acct-2", "prov-a", 4),
	}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)
	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r1", ModelID: "m", DeploymentID: "d1", Enabled: true, Weight: 1}},
		deployment("d1", "prov-a", domain.ProtocolOpenAIChat))
	first := map[string]int{}
	for i := 0; i < 4; i++ {
		cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) != 2 {
			t.Fatalf("candidates = %d, want 2", len(cands))
		}
		first[cands[0].Account.ID]++
	}
	if first["acct-1"] != 2 || first["acct-2"] != 2 {
		t.Errorf("round-robin distribution = %v, want 2/2", first)
	}
}

func TestLeaseLifecycle_AcquireCheckRenewRelease(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, nil, nil)
	lease, err := svc.AcquireUpstreamLease(context.Background(), nil,
		account("acct-1", "prov-a", 2), "req-1", svc.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckLease(context.Background(), lease); err != nil {
		t.Fatalf("check: %v", err)
	}
	if err := fs.RenewLease(context.Background(), lease.ID, lease.OwnerToken, lease.FencingToken, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := svc.ReleaseLease(context.Background(), lease); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A released lease fails Check (Task 7 fencing: 旧持有者不得再断言授权).
	if err := svc.CheckLease(context.Background(), lease); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("check after release = %v, want conflict", err)
	}
	// Releasing again is a tolerated no-op (fencing already covers it).
	if err := svc.ReleaseLease(context.Background(), lease); err != nil {
		t.Errorf("double release = %v, want nil (conflict tolerated)", err)
	}
}

func TestAcquireUpstreamLease_ZeroConcurrencyRejected(t *testing.T) {
	// 0 = 备而不用：租约层是候选过滤之外的兜底,按容量耗尽拒绝,
	// 绝不静默把 0 当 1 放行。
	fs := newFakeStore()
	svc := NewService(fs, nil, nil)
	_, err := svc.AcquireUpstreamLease(context.Background(), nil,
		account("acct-1", "prov-a", 0), "req-1", svc.clock.Now())
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("code = %s, want insufficient_capacity (err=%v)", domain.CodeOf(err), err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.leases) != 0 {
		t.Fatalf("parked account must not take a lease, got %d", len(fs.leases))
	}
}

func TestLeaseKeeper_RenewsAndReleases(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, nil, nil)
	svc.LeaseTTL = 60 * time.Millisecond
	lease, err := svc.AcquireUpstreamLease(context.Background(), nil,
		account("acct-1", "prov-a", 2), "req-1", svc.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	keeper := svc.NewLeaseKeeper(lease)
	time.Sleep(100 * time.Millisecond) // at least one renew tick (ttl/3 = 20ms)
	keeper.Stop(context.Background())
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.renewed) == 0 {
		t.Error("keeper never renewed the lease")
	}
	if len(fs.released) != 1 {
		t.Errorf("released = %v, want exactly 1 release", fs.released)
	}
	if fs.leases[lease.ID].State != domain.LeaseReleased {
		t.Errorf("lease state = %s, want released", fs.leases[lease.ID].State)
	}
}

func TestLeaseKeeper_FencingConflictSurfaces(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, nil, nil)
	svc.LeaseTTL = 30 * time.Millisecond
	lease, err := svc.AcquireUpstreamLease(context.Background(), nil,
		account("acct-1", "prov-a", 2), "req-1", svc.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	keeper := svc.NewLeaseKeeper(lease)
	defer keeper.Stop(context.Background())
	// Reclaim the lease behind the keeper's back (timeout reclamation): the
	// next renew must conflict and the keeper must report it exactly once.
	fs.mu.Lock()
	fs.leases[lease.ID].State = domain.LeaseExpired
	fs.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for keeper.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if keeper.Err() == nil {
		t.Fatal("keeper did not surface the fencing conflict")
	}
	if domain.CodeOf(keeper.Err()) != domain.CodeConflict {
		t.Errorf("keeper err = %v, want conflict", keeper.Err())
	}
}

func TestCandidates_StoreErrorPropagates(t *testing.T) {
	fs := newFakeStore()
	fs.accountsErr = errors.New("db down")
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)
	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r1", ModelID: "m", DeploymentID: "d1", Enabled: true, Weight: 1}},
		deployment("d1", "prov-a", domain.ProtocolOpenAIChat))
	_, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err == nil {
		t.Fatal("store error must propagate (fail closed)")
	}
}
