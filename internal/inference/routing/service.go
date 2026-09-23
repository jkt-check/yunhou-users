// Package routing owns upstream selection for one admitted request:
// capability-compatible route filtering, the account pool with per-account
// concurrency leases, and the in-process cooldown memory (设计 §3 routing:
// 账号选择、并发租约、冷却和会话绑定; §5: 路由只在能力兼容的部署间选择).
//
// The cross-instance concurrency protocol is Task 7's database lease
// (Acquire/Check/Renew/Release with owner+fencing tokens); the candidate
// branch's internal/llm/keypool.go contributes the round-robin + cooldown
// selection shape as the single-process starting point (基线报告 §3.2:
// 多实例/DB 协调为 Task 7 新增，本文件是它的消费点).
package routing

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

// cooldownAfterRetryable is how long an account is skipped in-process after
// a retryable upstream failure (429/5xx/transport). One minute matches
// typical per-minute rate windows (candidate keypool semantics). This is a
// scheduling hint only — it never touches the customer ledger, and it never
// reduces an account's DB state.
const cooldownAfterRetryable = 60 * time.Second

// AccountStore is the persistence surface routing needs; satisfied by
// inference/postgres.Store.
type AccountStore interface {
	// ListActiveUpstreamAccounts is the account pool of one provider (only
	// status='active' rows are schedulable, 设计 §8).
	ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error)
	// The lease protocol (Task 7). Acquire happens inside the gateway's
	// attempt transaction; Check gates the actual dispatch; Renew/Release
	// are the in-flight lifecycle.
	AcquireLeaseTx(ctx context.Context, w domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error)
	CheckLease(ctx context.Context, leaseID, ownerToken string, fencing int64, now time.Time) error
	RenewLease(ctx context.Context, leaseID, ownerToken string, fencing int64, newExpiresAt time.Time) error
	ReleaseLease(ctx context.Context, leaseID, ownerToken string, fencing int64) error
}

// Candidate is one dispatchable (route, deployment, account) triple. The
// order of a candidate slice IS the failover order (priority/weight from
// the pinned snapshot, then round-robin within the same rank).
type Candidate struct {
	Route      domain.ModelRoute
	Deployment domain.Deployment
	Account    domain.UpstreamAccount
}

// Needs are the capability demands of one logical call, derived from the
// request before routing (设计: 请求字段按能力校验；路由只在能力兼容的部署
// 之间选择).
type Needs struct {
	Tools     bool
	Reasoning bool
	// Protocol is the client-facing protocol the request arrived on; the
	// deployment's protocol only has to be servable by an adapter, the
	// client-protocol gate is the model's Protocols list (checked by the
	// gateway).
}

// Service selects candidates and coordinates their concurrency leases.
type Service struct {
	store    AccountStore
	adapters map[domain.Protocol]providers.Adapter
	clock    domain.Clock

	// OwnerToken identifies this process as the lease owner (Task 7 fencing:
	// a reclaimed lease's old owner can never reassert authorization).
	OwnerToken string
	// LeaseTTL is the upstream-lease lifetime; renewals extend it in flight.
	LeaseTTL time.Duration

	mu     sync.Mutex
	cooled map[string]time.Time // accountID → skip until
	rr     map[string]int       // providerID → round-robin cursor
}

// NewService builds the routing service over the account store. A nil clock
// uses the system clock (UTC).
func NewService(store AccountStore, adapters map[domain.Protocol]providers.Adapter, clock domain.Clock) *Service {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &Service{
		store: store, adapters: adapters, clock: clock,
		OwnerToken: uuid.NewString(),
		LeaseTTL:   10 * time.Minute,
		cooled:     map[string]time.Time{},
		rr:         map[string]int{},
	}
}

// AdapterFor returns the registered adapter of a deployment protocol.
func (s *Service) AdapterFor(p domain.Protocol) (providers.Adapter, bool) {
	a, ok := s.adapters[p]
	return a, ok
}

// Candidates lists the dispatchable triples for one call in failover order.
// A route/deployment/account is eligible only when ALL of:
//
//   - the route is enabled and its capability list covers the call's needs
//     (an empty capability list means "everything the model supports");
//   - the deployment is active, its protocol has a registered adapter, and
//     the adapter supports the call's needs (能力不兼容的部署不参与选择);
//   - the deployment's provider has at least one active, non-cooling
//     account.
//
// An empty result means upstream_unavailable — never silently route to an
// incompatible deployment.
func (s *Service) Candidates(ctx context.Context, snap *catalog.Snapshot, modelID string, needs Needs) ([]Candidate, error) {
	now := s.clock.Now()
	var out []Candidate
	for _, route := range snap.RoutesByModel[modelID] {
		if !route.Enabled || !routeCovers(route, needs) {
			continue
		}
		d, ok := snap.Deployments[route.DeploymentID]
		if !ok || d.Status != domain.DeploymentActive {
			continue
		}
		adapter, ok := s.adapters[d.Protocol]
		if !ok {
			continue
		}
		caps := adapter.Capabilities()
		if (needs.Tools && !caps.Tools) || (needs.Reasoning && !caps.Reasoning) {
			continue
		}
		accounts, err := s.store.ListActiveUpstreamAccounts(ctx, d.ProviderID)
		if err != nil {
			return nil, err
		}
		for _, a := range s.orderAccountsWithQuota(d.ProviderID, accounts, now) {
			out = append(out, Candidate{Route: route, Deployment: d, Account: a})
		}
	}
	return out, nil
}

// routeCovers reports whether a route's capability list covers the call's
// needs. An empty list means "everything the model supports" (the model
// capability gate runs before routing); a non-empty list is restrictive.
func routeCovers(r domain.ModelRoute, needs Needs) bool {
	if len(r.Capabilities) == 0 {
		return true
	}
	has := map[string]bool{}
	for _, c := range r.Capabilities {
		has[c] = true
	}
	if needs.Tools && !has["tools"] {
		return false
	}
	if needs.Reasoning && !has["reasoning"] {
		return false
	}
	return true
}

// orderAccounts round-robins the active accounts of one provider, skipping
// in-process cooldowns. The cursor advance is per Call so consecutive
// requests rotate; accounts cooling down at `now` are omitted entirely.
func (s *Service) orderAccounts(providerID string, accounts []domain.UpstreamAccount, now time.Time) []domain.UpstreamAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make([]domain.UpstreamAccount, 0, len(accounts))
	for _, a := range accounts {
		if until, ok := s.cooled[a.ID]; ok && until.After(now) {
			continue
		}
		live = append(live, a)
	}
	if len(live) <= 1 {
		return live
	}
	start := s.rr[providerID] % len(live)
	s.rr[providerID] = start + 1
	out := make([]domain.UpstreamAccount, 0, len(live))
	for i := 0; i < len(live); i++ {
		out = append(out, live[(start+i)%len(live)])
	}
	return out
}

// Cool skips an account in-process until `until` — the scheduling response
// to a retryable upstream failure. Fail-open by design: cooling never
// blocks the last resort path (Candidates simply omits cooled accounts, and
// the gateway's exhausted-candidate error is upstream_unavailable).
func (s *Service) Cool(accountID string, until time.Time) {
	s.mu.Lock()
	s.cooled[accountID] = until
	s.mu.Unlock()
}

// CooldownAfterFailure is the standard skip window for a retryable failure.
func (s *Service) CooldownAfterFailure(accountID string) {
	s.Cool(accountID, s.clock.Now().Add(cooldownAfterRetryable))
}

// ---------------------------------------------------------------------------
// Lease lifecycle (Task 7 消费点接线)
// ---------------------------------------------------------------------------

// AcquireUpstreamLease takes the per-account concurrency lease inside the
// caller's transaction (the gateway persists the attempt row in the same
// tx, so lease and attempt intent commit or roll back together).
func (s *Service) AcquireUpstreamLease(ctx context.Context, w domain.UnitOfWork, account domain.UpstreamAccount, requestID string, now time.Time) (*domain.ConcurrencyLease, error) {
	limit := account.ConcurrencyLimit
	if limit <= 0 {
		limit = 1
	}
	return s.store.AcquireLeaseTx(ctx, w, domain.AcquireLeaseCommand{
		Scope:      domain.LeaseScopeUpstreamAccount,
		ScopeID:    account.ID,
		RequestID:  requestID,
		OwnerToken: s.OwnerToken,
		Limit:      limit,
		TTL:        s.LeaseTTL,
		Now:        now,
	})
}

// CheckLease verifies the lease is still a live authorization right before
// the resource is used (Task 7: 超时回收不得与仍活跃请求重叠授权).
func (s *Service) CheckLease(ctx context.Context, l *domain.ConcurrencyLease) error {
	return s.store.CheckLease(ctx, l.ID, l.OwnerToken, l.FencingToken, s.clock.Now())
}

// ReleaseLease drops a lease under the ownership proof. A conflict means
// the lease was already reclaimed/fenced — the caller logs and moves on
// (the slot is gone either way); every other error is returned.
func (s *Service) ReleaseLease(ctx context.Context, l *domain.ConcurrencyLease) error {
	if l == nil {
		return nil
	}
	err := s.store.ReleaseLease(ctx, l.ID, l.OwnerToken, l.FencingToken)
	if err != nil && domain.CodeOf(err) == domain.CodeConflict {
		return nil
	}
	return err
}

// LeaseKeeper renews a batch of leases while a request is in flight
// (long-lived streams must not lose their slots mid-answer) and releases
// them at the end. A renewal conflict means the slot was reclaimed — the
// keeper reports it exactly once via Err() so the dispatcher can stop the
// upstream call instead of running ungated (fencing 的消费语义).
type LeaseKeeper struct {
	svc    *Service
	leases []*domain.ConcurrencyLease
	ttl    time.Duration

	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
	stopMu sync.Once
}

// NewLeaseKeeper starts the renewal loop; renew happens at ttl/3 intervals.
// A nil/empty lease list still works (nothing to renew; Stop releases
// nothing).
func (s *Service) NewLeaseKeeper(leases ...*domain.ConcurrencyLease) *LeaseKeeper {
	k := &LeaseKeeper{
		svc: s, ttl: s.LeaseTTL,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	for _, l := range leases {
		if l != nil {
			k.leases = append(k.leases, l)
		}
	}
	if len(k.leases) > 0 {
		go k.loop()
	} else {
		close(k.done)
	}
	return k
}

func (k *LeaseKeeper) loop() {
	defer close(k.done)
	interval := k.ttl / 3
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
			for _, l := range k.leases {
				err := k.svc.store.RenewLease(context.Background(), l.ID, l.OwnerToken,
					l.FencingToken, k.svc.clock.Now().Add(k.ttl))
				if err == nil {
					continue
				}
				if domain.CodeOf(err) == domain.CodeConflict {
					k.errMu.Lock()
					if k.err == nil {
						k.err = err
					}
					k.errMu.Unlock()
					return // fenced — stop renewing, the dispatcher must stop
				}
				// Transient store error: keep trying at the next tick.
			}
		}
	}
}

// Err reports the first fencing/renewal conflict, if any.
func (k *LeaseKeeper) Err() error {
	k.errMu.Lock()
	defer k.errMu.Unlock()
	return k.err
}

// Stop ends the renewal loop and releases every lease (best-effort;
// conflicts are already-covered by the fencing protocol).
func (k *LeaseKeeper) Stop(ctx context.Context) {
	k.stopMu.Do(func() {
		close(k.stop)
		<-k.done
		for _, l := range k.leases {
			_ = k.svc.ReleaseLease(ctx, l)
		}
	})
}

// Pause ends the renewal loop WITHOUT releasing — used when the leases move
// to a longer-lived keeper (attempt dispatch → request stream). The caller
// MUST hand the leases to another keeper or release them.
func (k *LeaseKeeper) Pause() {
	k.stopMu.Do(func() {
		close(k.stop)
		<-k.done
	})
}

// Leases returns the managed lease set (for hand-over via Pause).
func (k *LeaseKeeper) Leases() []*domain.ConcurrencyLease { return k.leases }

// SortCandidatesStable is exposed for tests: stable failover ordering by
// route priority then weight (the snapshot already sorts; this guards
// hand-built candidates).
func SortCandidatesStable(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Route.Priority != cs[j].Route.Priority {
			return cs[i].Route.Priority < cs[j].Route.Priority
		}
		if cs[i].Route.Weight != cs[j].Route.Weight {
			return cs[i].Route.Weight > cs[j].Route.Weight
		}
		return cs[i].Route.ID < cs[j].Route.ID
	})
}
