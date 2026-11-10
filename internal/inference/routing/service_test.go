package routing

import (
	"context"
	"errors"
	"strings"
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

	// accountQueries counts ListActiveUpstreamAccounts calls per provider so
	// a test can assert that a non-schedulable provider is never even
	// consulted (no account-pool round trip for a disabled provider).
	accountQueries map[string]int

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
		accounts:       map[string][]domain.UpstreamAccount{},
		accountQueries: map[string]int{},
		leases:         map[string]*domain.ConcurrencyLease{},
		acquireErr:     map[string]error{},
	}
}

func (s *fakeStore) ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error) {
	s.mu.Lock()
	s.accountQueries[providerID]++
	s.mu.Unlock()
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

// testSnapshot builds a one-model snapshot with the given routes. Every
// deployment's provider is registered as an ACTIVE provider so the snapshot
// mirrors a published catalog (发布快照里 provider 必然在场); the
// disabled-provider cases override the provider status explicitly.
func testSnapshot(modelID string, routes []domain.ModelRoute, deployments ...domain.Deployment) *catalog.Snapshot {
	snap := &catalog.Snapshot{
		Models:        map[string]domain.Model{},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{},
		RoutesByModel: map[string][]domain.ModelRoute{modelID: routes},
	}
	for _, d := range deployments {
		snap.Deployments[d.ID] = d
		if _, ok := snap.Providers[d.ProviderID]; !ok {
			snap.Providers[d.ProviderID] = domain.Provider{
				ID: d.ProviderID, Code: d.ProviderID, DisplayName: d.ProviderID,
				AccessType: domain.AccessOfficialAPI, Status: "active",
			}
		}
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

// statusRecheckFakeStore 给 fakeStore 加上可选的派发时事务内状态复查接口
// （accountActiveRecheckTx），statuses 缺省（无行）按 CodeNotFound 处理。
type statusRecheckFakeStore struct {
	*fakeStore
	statuses map[string]domain.UpstreamAccountStatus
}

func (s *statusRecheckFakeStore) GetUpstreamAccountStatusForUpdateTx(_ context.Context, _ domain.UnitOfWork, id string) (domain.UpstreamAccountStatus, error) {
	st, ok := s.statuses[id]
	if !ok {
		return "", domain.NewError(domain.CodeNotFound, "not found")
	}
	return st, nil
}

// TestAcquireUpstreamLease_DispatchStatusRecheck: 派发时事务内复查
// status='active'（pr-ci 第三轮 FullLifecycleE2E：候选来自 1s TTL 缓存，
// 选型之后账号被管理面停用必须拦在派发前）。非 active / 行不存在 →
// ErrAccountInactiveAtDispatch（gateway 据此 failover 且不冷却）；active →
// 照常签发租约。
func TestAcquireUpstreamLease_DispatchStatusRecheck(t *testing.T) {
	fs := &statusRecheckFakeStore{fakeStore: newFakeStore(), statuses: map[string]domain.UpstreamAccountStatus{
		"a-disabled": domain.AccountDisabled,
		"a-active":   domain.AccountActive,
	}}
	svc := NewService(fs, nil, nil)
	ctx := context.Background()
	now := time.Now()

	_, err := svc.AcquireUpstreamLease(ctx, nil, account("a-disabled", "prov", 1), "r1", now)
	if !errors.Is(err, ErrAccountInactiveAtDispatch) {
		t.Fatalf("disabled account: err = %v, want ErrAccountInactiveAtDispatch", err)
	}
	_, err = svc.AcquireUpstreamLease(ctx, nil, account("a-gone", "prov", 1), "r2", now)
	if !errors.Is(err, ErrAccountInactiveAtDispatch) {
		t.Fatalf("missing account: err = %v, want ErrAccountInactiveAtDispatch", err)
	}
	lease, err := svc.AcquireUpstreamLease(ctx, nil, account("a-active", "prov", 1), "r3", now)
	if err != nil {
		t.Fatalf("active account: %v", err)
	}
	if lease == nil || lease.ScopeID != "a-active" {
		t.Fatalf("lease = %+v, want scope a-active", lease)
	}
}

// TestAcquireUpstreamLease_RecheckOptional: store 不实现复查接口（窄 fake）
// 时签发行为与之前一致（可选升级惯例，同 quotaStore）。
func TestAcquireUpstreamLease_RecheckOptional(t *testing.T) {
	svc := NewService(newFakeStore(), nil, nil)
	lease, err := svc.AcquireUpstreamLease(context.Background(), nil,
		account("a1", "prov", 1), "r1", time.Now())
	if err != nil || lease == nil {
		t.Fatalf("AcquireUpstreamLease without recheck support = %v/%+v", err, lease)
	}
}

// --- T3: provider 停用与「已发布 route/provider/deployment 可调度」判定 ---

// setProviderStatus 覆盖快照里某个 provider 的已发布状态。
func setProviderStatus(snap *catalog.Snapshot, providerID, status string) {
	p := snap.Providers[providerID]
	p.Status = status
	snap.Providers[providerID] = p
}

// candidateSummary 把候选集压成 "provider/account" 列表（失败信息只留
// 判据，不刷整份结构体）。
func candidateSummary(cands []Candidate) string {
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		parts = append(parts, c.Deployment.ProviderID+"/"+c.Account.ID)
	}
	return strings.Join(parts, ",")
}

// TestCandidates_DisabledProviderExcluded：发布快照里 provider 被停用时，
// 它的部署不再产生任何候选（不得出站）；同一模型挂在另一个 active
// provider 上的部署仍然可用（停用一个 provider 不得停掉整个模型）。
func TestCandidates_DisabledProviderExcluded(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-off"] = []domain.UpstreamAccount{account("acct-off", "prov-off", 4)}
	fs.accounts["prov-on"] = []domain.UpstreamAccount{account("acct-on", "prov-on", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	snap := testSnapshot("m",
		[]domain.ModelRoute{
			{ID: "r-off", ModelID: "m", DeploymentID: "dep-off", Enabled: true, Weight: 1},
			{ID: "r-on", ModelID: "m", DeploymentID: "dep-on", Enabled: true, Weight: 1},
		},
		deployment("dep-off", "prov-off", domain.ProtocolOpenAIChat),
		deployment("dep-on", "prov-on", domain.ProtocolOpenAIChat),
	)
	setProviderStatus(snap, "prov-off", "disabled")

	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Account.ID != "acct-on" {
		t.Fatalf("candidates = [%s], want only acct-on (停用的 provider 不得出站)", candidateSummary(cands))
	}
	for _, c := range cands {
		if c.Deployment.ProviderID == "prov-off" {
			t.Fatalf("a disabled provider produced a candidate: %+v", c)
		}
	}
	// 停用的 provider 连账号池都不必查（结构性空，不是容量问题）。
	fs.mu.Lock()
	offQueries := fs.accountQueries["prov-off"]
	onQueries := fs.accountQueries["prov-on"]
	fs.mu.Unlock()
	if offQueries != 0 {
		t.Errorf("disabled provider account pool queried %d times, want 0", offQueries)
	}
	if onQueries == 0 {
		t.Error("the active provider must still be consulted")
	}
}

// TestCandidates_DisabledProviderOnlyMeansNoCandidates：模型只挂在被停用的
// provider 上时没有任何候选（网关据此回 upstream_unavailable），不得回退
// 到停用 provider 的账号。
func TestCandidates_DisabledProviderOnlyMeansNoCandidates(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-off"] = []domain.UpstreamAccount{account("acct-off", "prov-off", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r-off", ModelID: "m", DeploymentID: "dep-off", Enabled: true, Weight: 1}},
		deployment("dep-off", "prov-off", domain.ProtocolOpenAIChat),
	)
	setProviderStatus(snap, "prov-off", "disabled")

	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = [%s], want 0 (只有 disabled provider 的模型不可调度)", candidateSummary(cands))
	}
}

// TestCandidates_ProviderAbsentFromSnapshotIsNotSchedulable：发布快照是
// 权威事实源——快照里没有的 provider 没有已发布状态，其部署不得被当成
// 可调度（fail-closed，不得当成 active 放行）。
func TestCandidates_ProviderAbsentFromSnapshotIsNotSchedulable(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-gone"] = []domain.UpstreamAccount{account("acct-gone", "prov-gone", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	snap := testSnapshot("m",
		[]domain.ModelRoute{{ID: "r-gone", ModelID: "m", DeploymentID: "dep-gone", Enabled: true, Weight: 1}},
		deployment("dep-gone", "prov-gone", domain.ProtocolOpenAIChat),
	)
	delete(snap.Providers, "prov-gone")

	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = [%s], want 0 (provider 不在已发布快照里 → 不可调度)", candidateSummary(cands))
	}
}

// ---------------------------------------------------------------------------
// T7 — 配置行为收口：pool_strategy 支持集 + weight 语义
// ---------------------------------------------------------------------------

// TestCandidates_UnsupportedPoolStrategyIsNotACandidate：存量已发布快照里的
// 不支持 pool_strategy（least_loaded / 通用 session_sticky）必须被标记
// 不可用 —— 路由不得把它静默当成 round_robin。
func TestCandidates_UnsupportedPoolStrategyIsNotACandidate(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{account("acct-1", "prov-a", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	routes := []domain.ModelRoute{
		{ID: "r-least", ModelID: "m", DeploymentID: "dep-a", Enabled: true, Weight: 1,
			PoolStrategy: domain.PoolLeastLoaded},
		{ID: "r-sticky", ModelID: "m", DeploymentID: "dep-b", Enabled: true, Weight: 1,
			PoolStrategy: domain.PoolSessionSticky},
		{ID: "r-rr", ModelID: "m", DeploymentID: "dep-c", Enabled: true, Weight: 1,
			PoolStrategy: domain.PoolRoundRobin},
	}
	snap := testSnapshot("m", routes,
		deployment("dep-a", "prov-a", domain.ProtocolOpenAIChat),
		deployment("dep-b", "prov-a", domain.ProtocolOpenAIChat),
		deployment("dep-c", "prov-a", domain.ProtocolOpenAIChat),
	)

	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Route.ID != "r-rr" {
		t.Fatalf("candidates = [%s], want only the round_robin route (不支持的 pool_strategy 不得静默当 RR)",
			candidateSummary(cands))
	}
}

// TestCandidates_WeightOrdersWithinEqualPriorityAndNeverSplits：weight 只做
// 「同 priority 下的候选排序」，不是概率分流 —— 候选一个都不能少。
func TestCandidates_WeightOrdersWithinEqualPriorityAndNeverSplits(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{account("acct-1", "prov-a", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)

	mkDep := func(id string) domain.Deployment {
		return domain.Deployment{
			ID: id, ProviderID: "prov-a", UpstreamModel: "up-1",
			BaseURL: "https://up.example.com", Protocol: domain.ProtocolOpenAIChat,
			Status: domain.DeploymentActive, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		}
	}
	payload := catalog.BuildCatalogPayload(
		[]domain.Model{{ID: "m", DisplayName: "M", Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"}}},
		[]domain.Provider{{ID: "prov-a", Code: "prov-a", DisplayName: "P",
			AccessType: domain.AccessOfficialAPI, Status: "active"}},
		[]domain.Deployment{mkDep("dep-low"), mkDep("dep-mid"), mkDep("dep-high")},
		[]domain.ModelRoute{
			{ID: "r-low", ModelID: "m", DeploymentID: "dep-low", Priority: 0, Weight: 1, Enabled: true},
			{ID: "r-high", ModelID: "m", DeploymentID: "dep-high", Priority: 0, Weight: 10, Enabled: true},
			{ID: "r-mid", ModelID: "m", DeploymentID: "dep-mid", Priority: 0, Weight: 5, Enabled: true},
		},
	)
	snap, err := catalog.ParseSnapshot(&domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: payload}})
	if err != nil {
		t.Fatal(err)
	}

	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3 (weight 只排序,不做概率分流:候选不得被丢)", len(cands))
	}
	for i, want := range []string{"r-high", "r-mid", "r-low"} {
		if cands[i].Route.ID != want {
			t.Errorf("candidate[%d] = %s, want %s (同 priority 下按 weight 降序)", i, cands[i].Route.ID, want)
		}
	}
}

// ---------------------------------------------------------------------------
// T8 — 能力路由（图片 / 推理档位）
//
// 路由层是能力契约的第二层：它必须与 adapter 的能力声明一致 ——
// 不兼容的部署**根本不参与选择**，而不是等出站前才失败。
// ---------------------------------------------------------------------------

// TestCandidates_ImageNeedSkipsIncompatibleProtocol：带图请求只选兼容的
// 上游协议；第一候选（openai_responses，本期不支持图片）被跳过。
func TestCandidates_ImageNeedSkipsIncompatibleProtocol(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{account("acct-1", "prov-a", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:      providers.NewOpenAIChat(),
		domain.ProtocolOpenAIResponses: providers.NewOpenAIResponses(),
	}, nil)
	routes := []domain.ModelRoute{
		{ID: "r-responses", ModelID: "m", DeploymentID: "dep-responses", Enabled: true, Weight: 1},
		{ID: "r-chat", ModelID: "m", DeploymentID: "dep-chat", Enabled: true, Weight: 1},
	}
	snap := testSnapshot("m", routes,
		deployment("dep-responses", "prov-a", domain.ProtocolOpenAIResponses),
		deployment("dep-chat", "prov-a", domain.ProtocolOpenAIChat),
	)
	// 纯文本：两个候选都在（能力路由不得收窄文本调度面）。
	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cands[0].Deployment.ID != "dep-responses" {
		t.Fatalf("text candidates = %+v, want both (responses first)", cands)
	}
	// 带图：只剩 chat 候选。
	cands, err = svc.Candidates(context.Background(), snap, "m", Needs{Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Deployment.ID != "dep-chat" {
		t.Fatalf("image candidates = %+v, want dep-chat only", cands)
	}
	// CompatibleDeployments 是同一个判定（准入前的能力闸门用它）。
	compat := svc.CompatibleDeployments(snap, "m", Needs{Images: true})
	if len(compat) != 1 || compat[0].Deployment.ID != "dep-chat" {
		t.Fatalf("compatible deployments = %+v, want dep-chat only", compat)
	}
}

// TestCandidates_ReasoningEffortNeedSkipsProtocolWithoutKnob：显式推理档位
// 只选**能表达该档位**的上游协议；anthropic_messages 没有 effort 旋钮，
// 因此被跳过（绝不折成别的档位）。
func TestCandidates_ReasoningEffortNeedSkipsProtocolWithoutKnob(t *testing.T) {
	fs := newFakeStore()
	fs.accounts["prov-a"] = []domain.UpstreamAccount{account("acct-1", "prov-a", 4)}
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}, nil)
	routes := []domain.ModelRoute{
		{ID: "r-anthropic", ModelID: "m", DeploymentID: "dep-anthropic", Enabled: true, Weight: 1},
		{ID: "r-chat", ModelID: "m", DeploymentID: "dep-chat", Enabled: true, Weight: 1},
	}
	snap := testSnapshot("m", routes,
		deployment("dep-anthropic", "prov-a", domain.ProtocolAnthropicMessage),
		deployment("dep-chat", "prov-a", domain.ProtocolOpenAIChat),
	)
	// 只要推理（不指定档位）：两个协议都能承载。
	cands, err := svc.Candidates(context.Background(), snap, "m", Needs{Reasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("reasoning candidates = %d, want 2", len(cands))
	}
	// 指定档位：anthropic 无 effort 旋钮 → 跳过。
	for _, effort := range []string{"low", "medium", "high", "xhigh"} {
		cands, err = svc.Candidates(context.Background(), snap, "m",
			Needs{Reasoning: true, ReasoningEffort: effort})
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) != 1 || cands[0].Deployment.ID != "dep-chat" {
			t.Fatalf("effort %q candidates = %+v, want dep-chat only", effort, cands)
		}
	}
}

// ---------------------------------------------------------------------------
// T8 — 路由能力清单的两种存量形态
//
// 空清单 = 全部能力（存量默认形态，DB 默认 '{}'）；非空清单是**限制性**的：
// 它列出这条路由承载哪些能力，缺哪一项就不承载那一项。T8 起 "image" 也
// 是一把钥匙 —— 存量非空清单（例如 ["tools","reasoning"]）因此**不再**
// 承载带图请求：这是有意的收窄（见兼容性报告），运营要恢复图片能力必须
// 显式加上 "image"。
// ---------------------------------------------------------------------------

func TestRouteCovers_LegacyCapabilityLists(t *testing.T) {
	empty := domain.ModelRoute{ModelID: "m", DeploymentID: "d", Weight: 1, Enabled: true}
	if !routeCovers(empty, Needs{Images: true}) {
		t.Error("an empty capability list must keep serving images (legacy default)")
	}
	if !routeCovers(empty, Needs{Tools: true}) {
		t.Error("an empty capability list must keep serving tools (legacy default)")
	}
	restrictive := domain.ModelRoute{ModelID: "m", DeploymentID: "d", Weight: 1, Enabled: true,
		Capabilities: []string{"tools", "reasoning"}}
	if routeCovers(restrictive, Needs{Images: true}) {
		t.Error("a restrictive list without image must not serve images (deliberate narrowing)")
	}
	if !routeCovers(restrictive, Needs{Tools: true}) {
		t.Error("a restrictive list must still serve the capabilities it declares")
	}
	if !routeCovers(restrictive, Needs{Reasoning: true}) {
		t.Error("a restrictive list must still serve the capabilities it declares")
	}
	withImage := domain.ModelRoute{ModelID: "m", DeploymentID: "d", Weight: 1, Enabled: true,
		Capabilities: []string{"tools", "reasoning", "image"}}
	if !routeCovers(withImage, Needs{Images: true}) {
		t.Error("a list declaring image must serve images")
	}
}
