package quota

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// service.go — 准入编排服务（Task 7）。把 Task 6 的纯规则（窗口解析、准入
// 评估）与 Task 1 的事务骨架（共享 UnitOfWork 的 Reserve/Release）组装成
// 一次逻辑调用的额度闸门：
//
//   - 一个事务内按固定顺序锁定：账户 →（请求）→ 窗口(five_hour→weekly→
//     monthly) → Key 预算（repo 层实现，控制者裁决）；检查并创建请求/预占，
//     提交后才允许任何网络调用——绝不持有 DB 事务等待上游（设计 §7.2）。
//   - 三窗口 + Key 预算同时预占成功才放行；任一失败整个事务回滚，不遗留
//     部分占用；请求/预占唯一键防同一内部请求重复预占。
//   - 首次消费在同事务内激活窗口；多请求并发首消费由账户行锁序列化 +
//     唯一键/EXCLUDE 兜底，冲突时整体重试（ErrWindowActivationConflict）。
//   - 存储不可用时 Admit 返回错误、拒绝放行（fail-closed）；已提交的预占
//     状态可恢复，在途恢复 worker 归 Task 9。

// ErrWindowActivationConflict marks a concurrent first-consumption window
// creation collision (unique key / EXCLUDE backstop fired despite the
// account serialization). The admission transaction is rolled back and the
// whole admit retried: the winner's window row is then visible and reused.
var ErrWindowActivationConflict = domain.NewError(domain.CodeConflict,
	"quota: concurrent window activation — retry admission")

// maxActivationRetries bounds the admission retry loop. With the account
// row lock serializing same-account admissions, a conflict means the
// pre-lock timestamp raced a concurrently committed window; the next
// attempt reads the committed row under the lock and binds to it, so a
// handful of attempts converges with headroom. Exhaustion fails closed.
const maxActivationRetries = 5

// Store is the persistence surface the admission orchestration needs;
// satisfied by inference/postgres.Store. Every mutating method participates
// in the caller's UnitOfWork — no hidden second connection (任务书).
type Store interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	// ActivateWindowsTx locks the billing account FOR UPDATE (fixed lock
	// order step 1), reads the entitlement's active windows, persists any
	// missing window the consumption at At activates, and returns the
	// resolved window rows (with IDs) per enabled kind.
	ActivateWindowsTx(ctx context.Context, uow domain.UnitOfWork, cmd ActivateWindowsCommand) ([]domain.QuotaWindow, error)
	// Reserve atomically applies the four holds and persists the request
	// (domain.QuotaStore contract, 设计 §7.2).
	Reserve(ctx context.Context, uow domain.UnitOfWork, cmd domain.ReserveCommand) (*domain.Admission, error)
	// Release drops every held reservation of a confirmed-zero-consumption
	// request and safely voids an unused five-hour window in-transaction.
	Release(ctx context.Context, uow domain.UnitOfWork, requestID string) error
	// AcquireLeaseTx takes one concurrency lease inside the shared tx
	// (ownership + fencing token + TTL, Task 7).
	AcquireLeaseTx(ctx context.Context, uow domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error)
}

// ActivateWindowsCommand is the input of in-transaction window resolution
// and activation. Limits carries the ENABLED windows only (a disabled
// window is an explicit policy fact, never unlimited, 设计 §9.1); the
// store iterates them in the fixed lock order.
type ActivateWindowsCommand struct {
	BillingAccountID string
	EntitlementID    string
	Anchor           time.Time
	At               time.Time
	Limits           map[domain.WindowKind]domain.Microcredit
}

// AdmitCommand is one logical call entering the quota gate. The caller
// (gateway, Task 8) has already authenticated the principal, resolved the
// entitlement and pinned the price/policy revisions.
type AdmitCommand struct {
	Request domain.Request
	// Entitlement is THE ONE selected entitlement (access.SelectEntitlement)
	// — the consumption subject the windows key on.
	Entitlement domain.Entitlement
	// Policy is the pinned quota policy revision (pure shape).
	Policy Policy
	// CreditPrice is the pinned sale_credit price version the reservation
	// amount is computed from.
	CreditPrice accounting.PriceVersion
	// Model carries the hard bounds (context / max output tokens).
	Model domain.Model
	// EstimatedInputTokens is the gateway's prompt estimate; the safe input
	// bound narrows it to the model context limit.
	EstimatedInputTokens int64
	// ClientMaxTokens is the client-declared output cap, if any; the forced
	// cap is min(client, model hard limit) — unlimited output is refused.
	ClientMaxTokens *int64
	// ExtraBounds are upper bounds of other billable items (tools etc.);
	// each positive bound must have a rate in CreditPrice.ExtraRates.
	ExtraBounds map[string]int64
	// At pins admitted_at; zero reads the service clock (server time).
	At time.Time
}

// AdmissionResult is a committed admission: the request may now be
// dispatched upstream. Windows/price/policy are pinned at AdmittedAt and
// never rebound at settlement (设计 §6: 跨重置时刻结束仍结算到原窗口).
type AdmissionResult struct {
	RequestID string
	// HoldMicros is the single-consumption safe upper bound mirrored into
	// every target (三窗口 + Key 预算各镜像一份，客户只结算一次).
	HoldMicros   domain.Microcredit
	AdmittedAt   time.Time
	WindowIDs    map[domain.WindowKind]string
	OutputCap    int64
	InputBound   int64
	PolicyID     string
	PriceID      string
	AccountLease *domain.ConcurrencyLease
}

// Service is the quota admission orchestrator.
type Service struct {
	store Store
	clock domain.Clock
	// AccountLeaseTTL is the lifetime of the per-account concurrency lease
	// taken with each admission when the policy sets a concurrency limit.
	AccountLeaseTTL time.Duration
	// OwnerToken identifies this service instance as lease owner.
	OwnerToken string
}

// NewService builds the admission service; a nil clock uses the system
// clock (UTC).
func NewService(store Store, clock domain.Clock) *Service {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &Service{
		store:           store,
		clock:           clock,
		AccountLeaseTTL: 10 * time.Minute,
		OwnerToken:      uuid.NewString(),
	}
}

// Admit runs the quota gate for one logical call. The reservation commits
// BEFORE any upstream network call (设计 §7.2: 短事务提交后才请求上游).
//
// Fail-closed: any store failure (including storage unavailable) returns an
// error and the request is NOT admitted — no path admits without a
// committed reservation. A committed admission whose process then crashes
// leaves a fully recoverable persisted state (request=reserved + held
// reservations + lease); the recovery worker is Task 9's job.
//
// admitted_at follows 设计 §7.2 逐字语义 — 按预占成功时绑定: unless the
// caller pins At (tests), EVERY attempt re-reads the server clock. An
// admission that queued on the account anchor lock may otherwise carry a
// timestamp older than a window committed meanwhile; retrying with a fresh
// timestamp converges onto that window instead of colliding with it
// forever.
func (s *Service) Admit(ctx context.Context, cmd AdmitCommand) (*AdmissionResult, error) {
	if cmd.Request.ID == "" {
		cmd.Request.ID = uuid.NewString()
	}
	inputBound, err := InputBound(cmd.EstimatedInputTokens, cmd.Model.ContextTokens)
	if err != nil {
		return nil, err
	}
	outputCap, err := EffectiveOutputCap(cmd.ClientMaxTokens, cmd.Model.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	hold, err := ReserveAmount(cmd.CreditPrice, ReserveBounds{
		InputBoundTokens: inputBound,
		OutputCapTokens:  outputCap,
		ExtraBounds:      cmd.ExtraBounds,
	})
	if err != nil {
		return nil, err
	}
	limits := make(map[domain.WindowKind]domain.Microcredit, len(domain.WindowOrder))
	for _, kind := range cmd.Policy.EnabledWindows() {
		l, _ := cmd.Policy.LimitFor(kind)
		limits[kind] = *l
	}

	var lastErr error
	for attempt := 0; attempt < maxActivationRetries; attempt++ {
		at := cmd.At.UTC()
		if at.IsZero() {
			at = s.clock.Now()
		}
		res, err := s.admitOnce(ctx, cmd, at, hold, inputBound, outputCap, limits)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// Only the window-activation collision is retriable; quota
		// exhaustion, validation and storage failures fail immediately
		// (closed). A duplicate request ID (CodeConflict on the request key)
		// is NOT retried — it is the idempotency signal.
		if !errors.Is(err, ErrWindowActivationConflict) {
			return nil, err
		}
	}
	return nil, lastErr
}

// admitOnce is one full admission attempt: one transaction, fixed lock
// order, all-or-nothing.
func (s *Service) admitOnce(ctx context.Context, cmd AdmitCommand, at time.Time, hold domain.Microcredit, inputBound, outputCap int64, limits map[domain.WindowKind]domain.Microcredit) (*AdmissionResult, error) {
	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err // fail-closed: storage unavailable → not admitted
	}
	// Every failure path rolls back: no partial holds survive (设计 §7.2).
	rollback := func(err error) (*AdmissionResult, error) {
		_ = uow.Rollback(ctx)
		return nil, err
	}

	windows, err := s.store.ActivateWindowsTx(ctx, uow, ActivateWindowsCommand{
		BillingAccountID: cmd.Request.BillingAccountID,
		EntitlementID:    cmd.Entitlement.ID,
		Anchor:           cmd.Entitlement.AnchorAt,
		At:               at,
		Limits:           limits,
	})
	if err != nil {
		return rollback(err)
	}

	// Pure admission evaluation for the full block detail (设计 §9.1: 多个
	// 窗口共同阻断时计算全部约束后的恢复时刻); the repo's conditional
	// UPDATEs re-enforce the same check atomically against concurrent
	// commits. 剩余可用 < 安全预占上界 → quota_exceeded + 缺口信息，不静默
	// 钳制（设计 §7.2 补充条款）。
	if err := EvaluateAdmission(cmd.Policy, windows, hold, at); err != nil {
		return rollback(withDeficit(err, cmd.Policy, windows, hold))
	}

	req := cmd.Request
	req.PolicyVersionID = firstNonEmpty(req.PolicyVersionID, cmd.Entitlement.PolicyVersionID)
	if pv := cmd.CreditPrice.ID; pv != "" {
		req.PriceVersionID = &pv
	}
	holds := make([]domain.HoldSpec, 0, len(windows)+1)
	windowIDs := make(map[domain.WindowKind]string, len(windows))
	for _, w := range windows {
		windowIDs[w.Kind] = w.ID
		h := domain.HoldSpec{Amount: hold, WindowID: &w.ID}
		switch w.Kind {
		case domain.WindowFiveHour:
			h.TargetKind = domain.TargetWindowFiveHour
		case domain.WindowWeekly:
			h.TargetKind = domain.TargetWindowWeekly
		case domain.WindowMonthly:
			h.TargetKind = domain.TargetWindowMonthly
		}
		holds = append(holds, h)
	}
	if req.APIKeyID != nil {
		holds = append(holds, domain.HoldSpec{
			TargetKind: domain.TargetKeyBudget, APIKeyID: req.APIKeyID, Amount: hold,
		})
	}
	adm, err := s.store.Reserve(ctx, uow, domain.ReserveCommand{
		Request: req, Holds: holds, AdmittedAt: at,
	})
	if err != nil {
		return rollback(err)
	}

	result := &AdmissionResult{
		RequestID: adm.RequestID, HoldMicros: hold, AdmittedAt: at,
		WindowIDs: windowIDs, OutputCap: outputCap, InputBound: inputBound,
		PolicyID: req.PolicyVersionID, PriceID: cmd.CreditPrice.ID,
	}

	// Account-scope concurrency lease (Task 7: 数据库租约协调账户并发).
	// Acquired in the SAME transaction: a lease failure rolls the
	// reservation back too — never a reserved-but-unleased half state.
	if cmd.Policy.ConcurrencyLimit != nil {
		lease, err := s.store.AcquireLeaseTx(ctx, uow, domain.AcquireLeaseCommand{
			Scope:      domain.LeaseScopeBillingAccount,
			ScopeID:    cmd.Request.BillingAccountID,
			RequestID:  adm.RequestID,
			OwnerToken: s.OwnerToken,
			Limit:      *cmd.Policy.ConcurrencyLimit,
			TTL:        s.AccountLeaseTTL,
			Now:        at,
		})
		if err != nil {
			return rollback(err)
		}
		result.AccountLease = lease
	}

	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// ReleaseAdmission confirms zero upstream consumption and drops every hold
// of the request in one transaction; an unused five-hour window is voided
// in-transaction when all its requests confirmed no consumption and no
// other valid hold remains (设计 §6).
func (s *Service) ReleaseAdmission(ctx context.Context, requestID string) error {
	uow, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	if err := s.store.Release(ctx, uow, requestID); err != nil {
		_ = uow.Rollback(ctx)
		return err
	}
	return uow.Commit(ctx)
}

// withDeficit attaches the deficit （缺口信息） to a quota_exceeded error:
// the smallest additional microcredits that would have admitted the
// request is the largest per-window shortfall (every window must be
// satisfied simultaneously, 设计 §6). Never silently clamps (§7.2).
func withDeficit(err error, p Policy, windows []domain.QuotaWindow, hold domain.Microcredit) error {
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		return err
	}
	var deficit domain.Microcredit
	for _, b := range qe.BlockedBy {
		avail := b.LimitMicros - b.UsedMicros - b.ReservedMicros
		if avail < 0 {
			avail = 0
		}
		if d := hold - avail; d > deficit {
			deficit = d
		}
	}
	if deficit > 0 && qe.DeficitMicros == nil {
		d := deficit
		qe.DeficitMicros = &d
	}
	return qe
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
