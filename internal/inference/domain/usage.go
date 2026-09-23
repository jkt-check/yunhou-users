package domain

import (
	"context"
	"time"
)

// UsageSource is the provenance of a metering fact (设计 §7.1). Missing
// usage is NEVER recorded as zero: it enters the estimated/unknown path
// and reconciliation.
type UsageSource string

const (
	UsagePending   UsageSource = "pending"
	UsageReported  UsageSource = "reported"
	UsageEstimated UsageSource = "estimated"
	UsageUnknown   UsageSource = "unknown"
)

// UsageBuckets are the normalized token buckets (设计 §7.1: 普通输入、
// 缓存读取、缓存写入、输出及可用的推理明细). Adapters normalize
// overlapping semantics — e.g. reasoning tokens already included in the
// output total are NOT added twice.
//
// Every bucket is a pointer: nil means "not reported" and maps to SQL
// NULL. Unknown must never collapse into 0.
type UsageBuckets struct {
	InputTokens      *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	OutputTokens     *int64
	ReasoningTokens  *int64
}

// TotalTokens sums the known buckets; nil buckets are skipped.
func (u UsageBuckets) TotalTokens() int64 {
	var total int64
	for _, p := range []*int64{u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens, u.ReasoningTokens} {
		if p != nil {
			total += *p
		}
	}
	return total
}

// UsageRecord is one normalized metering fact for one attempt. Revision
// increments when a correction (e.g. real evidence replacing an estimate)
// is appended — history is kept, never rewritten (设计 §7.3).
type UsageRecord struct {
	ID        string
	RequestID string
	AttemptID string
	Source    UsageSource
	Buckets   UsageBuckets
	// RawUsage is the schema-versioned original usage payload (prompts and
	// completions are NOT retained, 设计 §7.1).
	RawUsage   ExtensionConfig
	Revision   int
	RecordedAt time.Time
}

// RequestStatus is the lifecycle of 设计 §7.2:
//
//	authenticated → reserved → dispatching → streaming/non_streaming
//	                              → settling → settled
//	                              → reconciliation_required
//	reserved → released (确认没有发生上游消费)
type RequestStatus string

const (
	ReqAuthenticated          RequestStatus = "authenticated"
	ReqReserved               RequestStatus = "reserved"
	ReqDispatching            RequestStatus = "dispatching"
	ReqStreaming              RequestStatus = "streaming"
	ReqNonStreaming           RequestStatus = "non_streaming"
	ReqSettling               RequestStatus = "settling"
	ReqSettled                RequestStatus = "settled"
	ReqReleased               RequestStatus = "released"
	ReqReconciliationRequired RequestStatus = "reconciliation_required"
	ReqFailed                 RequestStatus = "failed"
)

// WindowKind enumerates the three quota windows (设计 §6).
type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowMonthly  WindowKind = "monthly"
)

// WindowOrder is the fixed lock order for atomic reservation (设计 §7.2:
// 原子检查并锁定适用窗口，顺序固定).
var WindowOrder = []WindowKind{WindowFiveHour, WindowWeekly, WindowMonthly}

// QuotaWindow is one consumption window. Intervals are half-open
// [Start, End); a request landing exactly on End belongs to the next
// window (设计 §6).
type QuotaWindow struct {
	ID            string
	EntitlementID string
	Kind          WindowKind
	Start         time.Time
	End           time.Time
	Limit         Microcredit
	Used          Microcredit
	Reserved      Microcredit
	// Voided marks a five-hour window revoked in-transaction after every
	// related request confirmed zero consumption (设计 §6).
	Voided    bool
	CreatedAt time.Time
}

// Available is max(0, limit - used - reserved) (设计 §6).
func (w QuotaWindow) Available() Microcredit {
	avail := w.Limit - w.Used - w.Reserved
	if avail < 0 {
		return 0
	}
	return avail
}

// ReservationState mirrors the DB CHECK.
type ReservationState string

const (
	ReservationHeld     ReservationState = "held"
	ReservationSettled  ReservationState = "settled"
	ReservationReleased ReservationState = "released"
)

// ReservationTargetKind: one of the three windows, or the Key budget.
type ReservationTargetKind string

const (
	TargetWindowFiveHour ReservationTargetKind = "window_five_hour"
	TargetWindowWeekly   ReservationTargetKind = "window_weekly"
	TargetWindowMonthly  ReservationTargetKind = "window_monthly"
	TargetKeyBudget      ReservationTargetKind = "key_budget"
	// TargetWallet is the money hold of a wallet-funded request (Task 14):
	// the reservation row mirrors the inference_wallet_holds freeze so the
	// unified audit surface / unique key still guard it; Amount carries
	// MICROMONEY of the wallet currency for this target (not microcredits).
	TargetWallet ReservationTargetKind = "wallet"
)

// Reservation is one held amount against one target. A request holds at
// most one reservation per target kind; the UNIQUE(request_id,
// target_kind) key makes a duplicate reserve fail (设计 §7.2).
type Reservation struct {
	ID         string
	RequestID  string
	TargetKind ReservationTargetKind
	WindowID   *string
	APIKeyID   *string
	Amount     Microcredit
	State      ReservationState
	CreatedAt  time.Time
	SettledAt  *time.Time
	ReleasedAt *time.Time
}

// Attempt is one upstream try of a logical request. Retries never bill
// the customer twice; every attempt's cost is traceable (设计 §7.2).
type Attempt struct {
	ID                string
	RequestID         string
	AttemptNo         int
	DeploymentID      *string
	UpstreamAccountID *string
	Status            string // dispatching|streaming|completed|failed|cancelled|unknown
	UpstreamRequestID string
	// Cost is the upstream cost of THIS attempt; Basis distinguishes
	// reported/estimated/allocated (设计 §7.1 cost_basis).
	Cost       *Money
	Basis      *CostBasis
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// CostBasis marks how an upstream cost figure was obtained.
type CostBasis string

const (
	CostReported  CostBasis = "reported"
	CostEstimated CostBasis = "estimated"
	CostAllocated CostBasis = "allocated"
)

// ChargeSource pins WHERE a request is charged, fixed at admission
// (Task 14, 控制者裁决: 入场时选定 plan entitlement 或 wallet，首版不在
// 请求中途隐式切换).
type ChargeSource string

const (
	// ChargeSourcePlan charges the plan entitlement's quota windows
	// (microcredits, sale_credit price list).
	ChargeSourcePlan ChargeSource = "plan"
	// ChargeSourceWallet charges the prepaid wallet (micromoney of the
	// wallet currency, sale_money price list): explicit overage fallback
	// or pay-as-you-go.
	ChargeSourceWallet ChargeSource = "wallet"
)

// Request is one logical model call. PriceVersionID/PolicyVersionID and
// the window bindings are pinned at admission; a request finishing after
// a reset still settles into its original windows (设计 §6).
type Request struct {
	ID               string
	BillingAccountID string
	APIKeyID         *string
	EntitlementID    string
	ModelID          string
	Protocol         Protocol
	Stream           bool
	Status           RequestStatus
	AdmittedAt       *time.Time
	PriceVersionID   *string
	PolicyVersionID  string
	WindowFiveHourID *string
	WindowWeeklyID   *string
	WindowMonthlyID  *string
	// ReservedMicros 是这次请求的单次消费预占上界：同一次消费镜像进
	// 全部目标（三窗口 + Key 预算），请求行只记一次，不等于各 hold 之和。
	// ChargeSource='wallet' 时它是钱包币种微金额（money micros）。
	ReservedMicros *Microcredit
	SettledMicros  *Microcredit
	// ChargeSource 是入场固定的扣费来源（migration 030）；'plan' 行走额度
	// 窗口，'wallet' 行走钱包冻结，二者绝不中途切换。
	ChargeSource ChargeSource
	UsageStatus  UsageSource
	// InputBoundTokens/OutputCapTokens/ExtraBounds 是准入时的安全上界
	// （Task 9, migration 031）：崩溃恢复的保守估算以此入账，可审计、可冲正。
	InputBoundTokens *int64
	OutputCapTokens  *int64
	ExtraBounds      map[string]int64
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
}

// LedgerEntry is one append-only ledger fact (设计 §7.3: 追加 + 冲正，
// 不重写已结算事实). Amount is always positive; direction comes from
// EntryType.
type LedgerEntry struct {
	ID               int64
	BillingAccountID string
	RequestID        string
	EntryType        LedgerEntryType
	AmountMicros     int64
	// Unit "microcredit" (currency empty) or "micromoney" (currency set).
	Unit            string
	Currency        string
	PriceVersionID  *string
	ReversesEntryID *int64
	AdjustmentID    *string
	CreatedBy       string
	CreatedAt       time.Time
}

// LedgerEntryType mirrors the DB CHECK.
type LedgerEntryType string

const (
	LedgerCharge     LedgerEntryType = "charge"
	LedgerReversal   LedgerEntryType = "reversal"
	LedgerAdjustment LedgerEntryType = "adjustment"
)

// ---------------------------------------------------------------------------
// Concurrency leases (Task 7: 数据库租约协调账户及上游并发)
// ---------------------------------------------------------------------------

// LeaseScope mirrors the inference_concurrency_leases.scope CHECK: what a
// lease coordinates — customer billing account, upstream account,
// deployment, or customer Key.
type LeaseScope string

const (
	LeaseScopeBillingAccount  LeaseScope = "billing_account"
	LeaseScopeUpstreamAccount LeaseScope = "upstream_account"
	LeaseScopeDeployment      LeaseScope = "deployment"
	LeaseScopeAPIKey          LeaseScope = "api_key"
)

// LeaseState mirrors the DB CHECK.
type LeaseState string

const (
	LeaseHeld     LeaseState = "held"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

// ConcurrencyLease is one database-coordinated concurrency slot. Ownership
// is the (OwnerToken, FencingToken) pair: OwnerToken identifies the holder
// instance, FencingToken is a per-scope monotonically increasing token
// issued at acquisition — never reused, even across reclamations. A
// reclaimed (timed-out) lease can never reassert authorization: renewal/
// release/check all require the exact pair AND the held state, so timeout
// reclamation never overlaps authorization with a still-active earlier
// request (设计 §7.2/Task 7). Concurrent live leases of a multi-slot scope
// do not fence each other — the token orders acquisition history, it is
// not a mutual-exclusion gate.
type ConcurrencyLease struct {
	ID           string
	Scope        LeaseScope
	ScopeID      string
	RequestID    string
	OwnerToken   string
	FencingToken int64
	State        LeaseState
	AcquiredAt   time.Time
	ExpiresAt    time.Time
	ReleasedAt   *time.Time
}

// AcquireLeaseCommand is the input of one lease acquisition. Limit is the
// maximum number of simultaneously held (unexpired) leases of the scope;
// Now is the server time the expiry is computed from (injected clock —
// all metering time comes from the server, 设计 §6).
type AcquireLeaseCommand struct {
	Scope      LeaseScope
	ScopeID    string
	RequestID  string
	OwnerToken string
	Limit      int
	TTL        time.Duration
	Now        time.Time
}

// ---------------------------------------------------------------------------
// Transactional contracts (Task 1 core: 预占与结算共享同一事务)
// ---------------------------------------------------------------------------

// UnitOfWork is an open database transaction. Every repo method taking a
// UnitOfWork MUST participate in that same transaction — implementations
// are forbidden from opening a hidden second connection (任务书:
// 不得隐藏调用另一个数据库连接). Commit/Rollback are terminal; using a
// UnitOfWork afterwards is an error.
type UnitOfWork interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// HoldSpec is one atomic hold inside a reservation: a window hold, the Key
// budget hold, or a wallet money hold. Exactly one of WindowID / APIKeyID /
// WalletID is set, matching TargetKind. For TargetWallet, Amount carries
// MICROMONEY of the wallet's currency.
type HoldSpec struct {
	TargetKind ReservationTargetKind
	WindowID   *string
	APIKeyID   *string
	// WalletID is set for TargetWallet holds (Task 14).
	WalletID *string
	Amount   Microcredit
}

// ReserveCommand is the input of an atomic reservation. The caller (quota
// service) has already computed the safe upper-bound amounts; the store
// enforces limits and records everything atomically.
type ReserveCommand struct {
	Request Request
	Holds   []HoldSpec
	// AdmittedAt binds windows and price (设计 §6/§7.2).
	AdmittedAt time.Time
}

// ReserveWalletCommand is the input of an atomic wallet-funded admission
// (Task 14): one money hold frozen against the account's wallet in the
// pinned sale_money price's currency. The request's charge source is
// pinned to wallet by the store implementation (入场固定扣费来源).
type ReserveWalletCommand struct {
	Request Request
	// WalletID is the account's wallet in the pinned price's currency.
	WalletID string
	// HoldMicros is the safe upper bound in MICROMONEY of the wallet
	// currency (priced under PriceVersionID at admission).
	HoldMicros     int64
	PriceVersionID string
	AdmittedAt     time.Time
	// MonthStart/MonthEnd bound the UTC calendar month the spend limit is
	// evaluated over (derived from AdmittedAt by the caller).
	MonthStart time.Time
	MonthEnd   time.Time
}

// Admission is the result of a successful reservation.
type Admission struct {
	RequestID string
	Windows   []QuotaWindow
	Holds     []Reservation
}

// SettleCommand is the input of an idempotent settlement (设计 §7.2:
// 最终事件、客户账本和窗口 used/reserved 在同一事务结算).
type SettleCommand struct {
	RequestID string
	Usage     UsageRecord
	// ChargeMicros is the customer-consumption amount in microcredits
	// (plan path). For wallet requests it mirrors WalletCharge.Micros so
	// crash recovery (which knows no currency) can re-supply the
	// conservative charge (= the reserved hold) without re-pricing.
	ChargeMicros Microcredit
	// WalletCharge is the priced money charge for charge_source='wallet'
	// requests (Task 14): micromoney + currency, quoted under the price
	// version pinned at admission. Nil on the plan path.
	WalletCharge *Money
	// AttemptCost (optional) updates the attempt's cost with its basis.
	AttemptID   *string
	AttemptCost *Money
	CostBasis   *CostBasis
	SettledAt   time.Time
}

// QuotaStore owns the quota gate: three windows + Key budget reserved in
// ONE transaction (任务书: 三窗口 + Key 预算同一事务原子预占). Failure
// leaves no partial holds.
type QuotaStore interface {
	// Begin opens a transaction usable by BOTH QuotaStore and
	// SettlementStore methods.
	Begin(ctx context.Context) (UnitOfWork, error)
	// Reserve atomically: locks the billing account and key, checks and
	// bumps every applicable window's reserved plus the key budget, and
	// persists the request (status=reserved) with its reservations.
	// Everything stays inside uow: on error the caller MUST roll back the
	// uow; since nothing commits before uow.Commit, a failed reserve can
	// never leave partial holds visible (设计 §7.2).
	Reserve(ctx context.Context, uow UnitOfWork, cmd ReserveCommand) (*Admission, error)
	// Release drops every held reservation of a request whose upstream
	// consumption is confirmed zero (设计 §7.2 reserved → released).
	Release(ctx context.Context, uow UnitOfWork, requestID string) error
	// LoadWindows reads windows without activating the five-hour window
	// (设计 §6: 读取额度接口不激活窗口).
	LoadWindows(ctx context.Context, entitlementID string, kinds []WindowKind) ([]QuotaWindow, error)
}

// SettlementStore owns idempotent settlement. Unique keys make repeated
// delivery a single effect (设计 §7.2: 唯一键防止重复结算).
type SettlementStore interface {
	// Begin opens a transaction usable by BOTH QuotaStore and
	// SettlementStore methods.
	Begin(ctx context.Context) (UnitOfWork, error)
	// Settle in one transaction: persists the usage fact, appends the
	// ledger charge, converts held reservations into used, and moves the
	// request to settled. A duplicate settlement hits the
	// UNIQUE(request_id)-where-charge key and returns CodeConflict.
	Settle(ctx context.Context, uow UnitOfWork, cmd SettleCommand) error
	// MarkReconciliationRequired parks a request whose usage is
	// estimated/unknown into the reconciliation queue with a recovery
	// deadline (设计 §7.2: 禁止仅凭 TTL 释放全部预占).
	MarkReconciliationRequired(ctx context.Context, uow UnitOfWork, requestID string, reason string, deadline time.Time) error
}

// ---------------------------------------------------------------------------
// Provider adapter contract (Task 1: 骨架；流式实现属 Task 8)
// ---------------------------------------------------------------------------

// AdapterCapabilities declares what an upstream protocol can do.
type AdapterCapabilities struct {
	// StreamingUsage means the adapter explicitly requests stream usage
	// (e.g. stream_options.include_usage) and the upstream honors it.
	StreamingUsage bool
	Tools          bool
	Reasoning      bool
}

// ProviderAdapter normalizes one upstream protocol. It is deliberately
// small in Task 1: request building and SSE translation land with Task 8.
type ProviderAdapter interface {
	Protocol() Protocol
	Capabilities() AdapterCapabilities
	// NormalizeUsage converts a schema-versioned raw upstream usage
	// payload into normalized buckets. It returns UsageEstimated or
	// UsageUnknown ONLY when the upstream genuinely cannot provide usage
	// (设计 §7.1) — never because the adapter failed to ask for it.
	NormalizeUsage(raw ExtensionConfig) (UsageBuckets, UsageSource, error)
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock is the injectable time source (窄接口). All metering time comes
// from the server; the DB stores UTC (设计 §6).
type Clock interface {
	Now() time.Time
}

// SystemClock reads the real clock, normalized to UTC.
type SystemClock struct{}

// Now returns time.Now().UTC().
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is a deterministic clock for tests and pure rules.
type FixedClock struct {
	T time.Time
}

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.T }
