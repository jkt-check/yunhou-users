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

// accountsCacheTTL 是 upstream 账号列表缓存 TTL：项目所有者裁定一律 1s、
// 不做主动失效，TTL 本身限制脏读窗口。
const accountsCacheTTL = time.Second

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
	// Images means the request carries image parts. A deployment whose
	// upstream protocol cannot carry images is NOT a candidate for it (T8:
	// 声明层/路由层/adapter 层必须一致 —— 不兼容的部署根本不参与选择，
	// 而不是等出站前才失败).
	Images bool
	// ReasoningEffort is the explicitly requested reasoning-effort level
	// ("" = the client asked for reasoning without naming a level). A
	// deployment whose protocol cannot EXPRESS that level is not a candidate
	// (T8: 档位不可表达 → 出站前明确拒绝，绝不折成别的档位).
	ReasoningEffort string
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

	// 账号池缓存：热路径上每请求/每条候选 route 一次的
	// ListActiveUpstreamAccounts 往返降为每 provider 每秒最多一次。
	acctMu    sync.RWMutex
	acctCache map[string]cachedAccounts // providerID → 账号列表 + 缓存时刻
}

type cachedAccounts struct {
	accounts []domain.UpstreamAccount
	cachedAt time.Time
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
		acctCache:  map[string]cachedAccounts{},
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
//   - the route is enabled, its pool strategy is inside the supported set
//     (常规池仅 round_robin —— 不支持的值是存量脏配置，绝不静默当 RR),
//     and its capability list covers the call's needs (an empty capability
//     list means "everything the model supports");
//   - the deployment is active, its provider is schedulable in the pinned
//     published snapshot, its protocol has a registered adapter, and the
//     adapter supports the call's needs (能力不兼容的部署不参与选择);
//   - the deployment's provider has at least one active, non-cooling
//     account.
//
// The route/provider/deployment half of that decision is the snapshot's
// single shared rule (catalog.Snapshot.DeploymentSchedulable) — the sticky
// path narrows THIS candidate set, it never re-derives schedulability on
// its own.
//
// An empty result means upstream_unavailable — never silently route to an
// incompatible deployment.
func (s *Service) Candidates(ctx context.Context, snap *catalog.Snapshot, modelID string, needs Needs) ([]Candidate, error) {
	out, consulted, err := s.candidates(ctx, snap, modelID, needs, false)
	if err != nil || len(out) > 0 || len(consulted) == 0 {
		return out, err
	}
	// 空候选可能建立在 1s TTL 账号缓存的过期额度/状态快照上（额度刚
	// reset、账号刚被恢复——绕过应用层的变更没有失效钩子）。「容量耗尽」
	// 的判定不该用陈旧快照：驱逐相关 provider 的缓存、绕过重查一次，
	// 仍为空才判定 upstream_unavailable。
	for providerID := range consulted {
		s.InvalidateAccounts(providerID)
	}
	out, _, err = s.candidates(ctx, snap, modelID, needs, true)
	return out, err
}

// candidates 是 Candidates 的实体；bypassCache=true 时账号池跳过缓存直查
// 并回写新鲜结果。consulted 记录走到账号池查询的 provider（其余空结果是
// 路由/部署/能力层面的结构性为空，重查无意义）。
func (s *Service) candidates(ctx context.Context, snap *catalog.Snapshot, modelID string, needs Needs, bypassCache bool) (out []Candidate, consulted map[string]struct{}, err error) {
	now := s.clock.Now()
	consulted = map[string]struct{}{}
	for _, cd := range s.CompatibleDeployments(snap, modelID, needs) {
		d := cd.Deployment
		consulted[d.ProviderID] = struct{}{}
		accounts, aerr := s.activeAccounts(ctx, d.ProviderID, bypassCache)
		if aerr != nil {
			return nil, nil, aerr
		}
		for _, a := range s.orderAccountsWithQuota(d.ProviderID, accounts, now) {
			out = append(out, Candidate{Route: cd.Route, Deployment: d, Account: a})
		}
	}
	return out, consulted, nil
}

// CompatibleDeployment is one (route, deployment) pair that passed every
// STRUCTURAL capability filter (route enabled, pool strategy supported,
// deployment + provider schedulable, protocol adapter registered, adapter
// able to carry the call's needs).
type CompatibleDeployment struct {
	Route      domain.ModelRoute
	Deployment domain.Deployment
}

// CompatibleDeployments returns the deployments that can structurally serve
// the call's needs, in failover order. It is the ONE place the capability
// filters live: the candidate builder, the gateway's pre-admission
// capability gate and the client-facing modality projections all call it,
// so they can never disagree.
//
// It performs NO account-pool lookup: the result is a capability statement,
// not a capacity statement. The gateway uses it to reject an unservable
// request BEFORE admission (zero hold, zero dispatch) — a request that no
// published deployment can carry is a client error, not a capacity gap.
func CompatibleDeployments(adapters map[domain.Protocol]providers.Adapter,
	snap *catalog.Snapshot, modelID string, needs Needs) []CompatibleDeployment {
	var out []CompatibleDeployment
	for _, route := range snap.RoutesByModel[modelID] {
		if !route.Enabled || !routeCovers(route, needs) {
			continue
		}
		// T7: 不支持的 pool_strategy（存量已发布快照里的 least_loaded /
		// 通用 session_sticky）被标记不可用 —— 绝不静默当成 round_robin：
		// 那条配置没有实现，当成 RR 就是「假装生效」。
		if !catalog.PoolStrategySupported(route.PoolStrategy) {
			continue
		}
		d, ok := snap.Deployments[route.DeploymentID]
		// The shared published-state decision: the deployment must be
		// active AND its provider must be schedulable in this snapshot.
		// A disabled provider takes every deployment behind it out of
		// rotation — the route stays enabled, but it is not a dispatchable
		// route.
		if !ok || !snap.DeploymentSchedulable(d) {
			continue
		}
		adapter, ok := adapters[d.Protocol]
		if !ok {
			continue
		}
		if !adapterServes(adapter.Capabilities(), needs) {
			continue
		}
		out = append(out, CompatibleDeployment{Route: route, Deployment: d})
	}
	return out
}

// CompatibleDeployments is the Service's view of the shared capability
// decision (same adapter set the dispatcher uses).
func (s *Service) CompatibleDeployments(snap *catalog.Snapshot, modelID string, needs Needs) []CompatibleDeployment {
	return CompatibleDeployments(s.adapters, snap, modelID, needs)
}

// SupportsImages reports whether the model has at least one published
// deployment that can carry image input — and at least one dispatchable
// deployment at all. It is the SAME decision the dispatch path uses, so a
// client-facing listing can never advertise an image the dispatcher refuses.
func SupportsImages(adapters map[domain.Protocol]providers.Adapter,
	snap *catalog.Snapshot, modelID string) bool {
	if len(CompatibleDeployments(adapters, snap, modelID, Needs{})) == 0 {
		return false // the model has no dispatchable deployment at all
	}
	return len(CompatibleDeployments(adapters, snap, modelID, Needs{Images: true})) > 0
}

// ImageCapabilityOracle answers whether a published model can serve image
// input AT ALL. The client-facing modality projections (GET /chat/models and
// the legacy /chat picker) must use it — advertising an image the dispatcher
// refuses is a capability lie.
//
// The implementation is the routing package's own decision; the interface
// exists so the projections do not have to own an adapter registry.
type ImageCapabilityOracle interface {
	SupportsImageInput(snap *catalog.Snapshot, modelID string) bool
}

// SupportsImageInput implements ImageCapabilityOracle with the service's own
// adapter set — identical to the one the dispatcher uses.
func (s *Service) SupportsImageInput(snap *catalog.Snapshot, modelID string) bool {
	return SupportsImages(s.adapters, snap, modelID)
}

// registryOracle adapts an adapter registry to the ImageCapabilityOracle.
type registryOracle map[domain.Protocol]providers.Adapter

// SupportsImageInput implements ImageCapabilityOracle.
func (r registryOracle) SupportsImageInput(snap *catalog.Snapshot, modelID string) bool {
	return SupportsImages(r, snap, modelID)
}

// ProductionImageCapability is the oracle over the PRODUCTION adapter
// registry (providers.RegisteredAdapters) — the same registry cmd/server
// wires into the routing service. It is the default for the client-facing
// projections, which have no routing service of their own.
func ProductionImageCapability() ImageCapabilityOracle {
	return registryOracle(providers.RegisteredAdapters())
}

// adapterServes reports whether one adapter's declared capabilities cover
// the call's needs. The adapter layer is the THIRD layer of the capability
// contract (model declaration → route capabilities → adapter support): a
// deployment whose protocol cannot carry the demand is not a candidate.
func adapterServes(caps domain.AdapterCapabilities, needs Needs) bool {
	if needs.Tools && !caps.Tools {
		return false
	}
	if needs.Reasoning && !caps.Reasoning {
		return false
	}
	if needs.Images && !caps.Images {
		return false
	}
	if needs.ReasoningEffort != "" && !supportsEffort(caps, needs.ReasoningEffort) {
		return false
	}
	return true
}

// supportsEffort reports whether the adapter's protocol can express the
// requested effort level. An adapter that declares no effort knob cannot
// express any level.
func supportsEffort(caps domain.AdapterCapabilities, effort string) bool {
	for _, e := range caps.ReasoningEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

// InvalidateAccounts 驱逐某 provider 的账号池缓存。供两类路径使用：
// Candidates 空结果重查（见上），以及 gateway 会话迁移冲突时发现缓存
// 候选里的账号在库里已失效（绕过应用层的变更没有失效钩子）。
func (s *Service) InvalidateAccounts(providerID string) {
	s.acctMu.Lock()
	delete(s.acctCache, providerID)
	s.acctMu.Unlock()
}

// activeAccounts 返回某 provider 的活跃账号池，带 1s TTL 进程内缓存。
// 错误不缓存；双重检查避免并发过期时同一 provider 的重复查库。fresh=true
// 跳过缓存读直查库并回写（用于空候选重查/迁移冲突后的刷新）。返回的
// 切片在 TTL 内被多请求共享——orderAccounts/配额过滤都只读它（各自构建
// 新切片），调用方不得原地修改。
func (s *Service) activeAccounts(ctx context.Context, providerID string, fresh bool) ([]domain.UpstreamAccount, error) {
	now := s.clock.Now()
	if !fresh {
		s.acctMu.RLock()
		e, ok := s.acctCache[providerID]
		s.acctMu.RUnlock()
		if ok && now.Sub(e.cachedAt) < accountsCacheTTL {
			return e.accounts, nil
		}
	}
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if !fresh {
		if e, ok := s.acctCache[providerID]; ok && now.Sub(e.cachedAt) < accountsCacheTTL {
			return e.accounts, nil
		}
	}
	accounts, err := s.store.ListActiveUpstreamAccounts(ctx, providerID)
	if err != nil {
		return nil, err
	}
	s.acctCache[providerID] = cachedAccounts{accounts: accounts, cachedAt: now}
	return accounts, nil
}

// routeCovers reports whether a route's capability list covers the call's
// needs. An empty list means "everything the model supports" (the model
// capability gate runs before routing); a non-empty list is restrictive —
// it enumerates the capabilities the route carries, and a demand it does
// not list is NOT served (T8: "image" is one of those keys).
func routeCovers(r domain.ModelRoute, needs Needs) bool {
	if len(r.Capabilities) == 0 {
		return true
	}
	has := map[string]bool{}
	for _, c := range r.Capabilities {
		has[c] = true
	}
	if needs.Tools && !has[domain.RouteCapabilityTools] {
		return false
	}
	if needs.Reasoning && !has[domain.RouteCapabilityReasoning] {
		return false
	}
	if needs.Images && !has[domain.RouteCapabilityImage] {
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
// ErrAccountInactiveAtDispatch 是派发时事务内复查（AcquireUpstreamLease）
// 发现账号已非 active 的哨兵错误：候选来自 1s TTL 账号池缓存，管理面停用
// /凭据吊销级联可能落在选型之后（pr-ci 第三轮 FullLifecycleE2E）。gateway
// 据此换下一个候选且不喂 60s 冷却（状态问题，非上游瞬时故障）。
var ErrAccountInactiveAtDispatch = domain.NewError(domain.CodeConflict,
	"upstream account not active at dispatch")

// accountActiveRecheckTx 是 store 的可选升级：派发事务内 FOR UPDATE 复查
// 账号状态。postgres.Store 实现；窄 fake 不实现则跳过复查（与 quotaStore /
// credentialLockTx 同一惯例）。
type accountActiveRecheckTx interface {
	GetUpstreamAccountStatusForUpdateTx(ctx context.Context, w domain.UnitOfWork, id string) (domain.UpstreamAccountStatus, error)
}

func (s *Service) AcquireUpstreamLease(ctx context.Context, w domain.UnitOfWork, account domain.UpstreamAccount, requestID string, now time.Time) (*domain.ConcurrencyLease, error) {
	limit := account.ConcurrencyLimit
	if limit <= 0 {
		// 0 = 备而不用（provisioned but unschedulable）：候选过滤
		// （orderAccountsWithQuota）应已排除；走到这里说明选型后被调成
		// 0 —— 按容量耗尽处理让调用方换下一个候选，绝不静默当 1 放行。
		return nil, domain.NewError(domain.CodeInsufficientCapacity,
			"upstream account parked (concurrency_limit 0)")
	}
	// 派发时事务内复查 status='active'：FOR SHARE 行锁与并发的管理面
	// 停用事务互斥，读取定序在两者之一之后。复查失败返回哨兵，调用方
	// failover 且不冷却。
	if rc, ok := s.store.(accountActiveRecheckTx); ok {
		status, err := rc.GetUpstreamAccountStatusForUpdateTx(ctx, w, account.ID)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				return nil, ErrAccountInactiveAtDispatch
			}
			return nil, domain.WrapError(domain.CodeInternal, "lease: account status recheck", err)
		}
		if status != domain.AccountActive {
			return nil, ErrAccountInactiveAtDispatch
		}
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
