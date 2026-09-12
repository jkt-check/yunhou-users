// Package gateway orchestrates ONE logical model call end to end
// (设计 §3 gateway: 一次逻辑调用的编排；§7.2 请求生命周期):
//
//	鉴权 principal → 权益解析(SelectEntitlement) → 目录快照 pin(一次调用
//	固定一个快照版本) → 配额预占(Admit, 同事务) → 路由(能力兼容部署 +
//	账号池/租约) → attempt 持久化(发送前落库) → 上游调用(流式/非流式) →
//	结算(Settle; 崩溃恢复归 Task 9).
//
// Transaction boundaries (设计 §7.2: 原子检查并锁定适用窗口，短事务提交后
// 才请求上游；不持有 DB 事务等待网络):
//
//  1. admission tx (quota.Service.Admit): account lock → window activation
//     → holds → request row (+ account concurrency lease) — committed BEFORE
//     any network I/O;
//  2. attempt tx: upstream-account lease + attempt row — committed before
//     the dispatch;
//  3. settlement tx: usage fact + ledger charge + reservation conversion +
//     request terminal state — runs on a detached, deadline-bounded context
//     so client cancellation never loses already-read usage (设计 §7.2:
//     结算使用独立且有截止时间的 context).
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
)

// SnapshotSource pins one catalog snapshot per call (设计 §5: 一次调用固定
// 使用一个快照，不混用新旧路由/价格). Satisfied by catalog.SnapshotCache.
type SnapshotSource interface {
	Current(ctx context.Context) (*catalog.Snapshot, error)
}

// SecretResolver decrypts the credential bound to an upstream account
// (Task 4 credentials.Service).
type SecretResolver interface {
	ResolveSecret(ctx context.Context, id string, pinGeneration *int64) ([]byte, *domain.Credential, error)
}

// Store is the persistence surface the gateway orchestration needs;
// satisfied by inference/postgres.Store. Methods taking a UnitOfWork
// participate in that transaction — no hidden second connection.
type Store interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	// Policy/price resolution (immutable versions, pinned at admission).
	GetPolicyVersion(ctx context.Context, id string) (*postgres.PolicyVersion, error)
	LatestPriceVersion(ctx context.Context, modelID, kind string, at time.Time) (*postgres.PriceVersion, error)
	// GetWalletByAccount resolves the account's wallet in a currency
	// (Task 14 wallet admission; CodeNotFound = no wallet → 余额不足).
	GetWalletByAccount(ctx context.Context, accountID, currency string) (*postgres.Wallet, error)
	// Attempt lifecycle.
	AcquireLeaseTx(ctx context.Context, w domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error)
	InsertAttemptTx(ctx context.Context, w domain.UnitOfWork, a *domain.Attempt) error
	UpdateRequestStatus(ctx context.Context, id string, status domain.RequestStatus, lastErr string) error
	FinishAttempt(ctx context.Context, id, status, errorKind, upstreamRequestID string, finishedAt time.Time) error
	// Settlement (domain.SettlementStore / QuotaStore release).
	Settle(ctx context.Context, w domain.UnitOfWork, cmd domain.SettleCommand) error
	MarkReconciliationRequired(ctx context.Context, w domain.UnitOfWork, requestID, reason string, deadline time.Time) error
	// EnqueueReconciliationJobTx upserts a reconciliation job in the
	// settlement transaction (Task 9: overage anomaly tracked atomically).
	EnqueueReconciliationJobTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.EnqueueReconciliationCommand) error
	Release(ctx context.Context, w domain.UnitOfWork, requestID string) error
}

// settleTimeout bounds the detached settlement context (设计 §7.2: 结算使
// 用独立且有截止时间的 context).
const settleTimeout = 15 * time.Second

// reconciliationDeadline is the recovery window for requests parked with
// unknown usage (设计 §7.2: 设定恢复时限、告警和人工调整入口，避免额度永
// 久悬挂). The recovery worker itself is Task 9.
const reconciliationDeadline = 24 * time.Hour

// maxAttemptsDefault caps the bounded failover (任务书: 有界重试限于可重试
// 错误且未开始客户端输出；重试不用于绕过账号容量/不重复扣费).
const maxAttemptsDefault = 3

// Service is the gateway orchestrator.
type Service struct {
	snapshots    SnapshotSource
	store        Store
	entitlements *access.EntitlementResolver
	quotaSvc     *quota.Service
	routingSvc   *routing.Service
	secrets      SecretResolver
	client       *http.Client
	egress       providers.EgressChecker
	clock        domain.Clock

	// sessions is the sticky-session binder (Task 12/13); nil = session
	// pinning not configured (session-keyed entry points fail closed).
	sessions *routing.SessionBinder

	// MaxAttempts bounds the failover loop (default 3, never above the
	// candidate count). Every attempt persists its own row; the customer
	// settles once per request.
	MaxAttempts int

	// OnSettleError receives settlement failures that park the request in
	// reconciliation (计费不可静默丢失 — this hook is the alarm; cmd/server
	// wires loud logging).
	OnSettleError func(requestID string, err error)
}

// NewService builds the gateway. A nil clock uses the system clock (UTC).
func NewService(snapshots SnapshotSource, store Store, entitlements *access.EntitlementResolver,
	quotaSvc *quota.Service, routingSvc *routing.Service, secrets SecretResolver,
	client *http.Client, egress providers.EgressChecker, clock domain.Clock) *Service {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &Service{
		snapshots: snapshots, store: store, entitlements: entitlements,
		quotaSvc: quotaSvc, routingSvc: routingSvc, secrets: secrets,
		client: client, egress: egress, clock: clock,
		MaxAttempts: maxAttemptsDefault,
		OnSettleError: func(requestID string, err error) {
			log.Printf("ERROR gateway: settlement failed for request %s: %v (parked for reconciliation)", requestID, err)
		},
	}
}

// SetSessionBinder wires the sticky-session binder (Task 13: Responses
// 会话链经 SessionBinding 钉住上游账号). Called once at assembly.
func (s *Service) SetSessionBinder(b *routing.SessionBinder) { s.sessions = b }

// BindSession pins (sessionKey, modelID) to the account that served the
// chain's first turn (called by the protocol surface AFTER a successful
// unbound response). A CodeConflict means a parallel first-turn already
// bound it — the caller logs and moves on (binding is affinity, not
// correctness).
func (s *Service) BindSession(ctx context.Context, sessionKey, modelID, accountID string) error {
	if s.sessions == nil {
		return domain.NewError(domain.CodeInternal, "gateway: sticky sessions not configured")
	}
	_, err := s.sessions.Bind(ctx, sessionKey, modelID, accountID)
	return err
}

// detached is the settlement context factory: decoupled from the (possibly
// canceled) request context, with its own deadline (设计 §7.2).
func detached(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// Outcome is one admitted call's result. Exactly one of Payload / Stream is
// set. The caller (httpapi handler / Kaya facade) serves Payload or relays
// Stream, then finalizes the stream with Stream.Finish.
type Outcome struct {
	RequestID string
	ModelID   string
	// AccountID is the upstream account that served the call (the winning
	// attempt) — the sticky-session binder pins on it (Task 13).
	AccountID string
	// Inclusion is the winning adapter's usage overlap declaration (cache
	// read inside input or not); client-surface translators normalize their
	// native usage shapes from it (Task 13).
	Inclusion accounting.Inclusion
	// Payload is the OpenAI-shaped chat.completion body (non-streaming).
	Payload []byte
	// Stream is the streaming result (OpenAI-shaped SSE + usage tap).
	Stream *StreamBody
}

// StreamEnd classifies how the client-facing relay finished.
type StreamEnd int

const (
	// EndCompleted: protocol end marker observed ([DONE]/message_stop).
	EndCompleted StreamEnd = iota
	// EndUpstreamBroke: the upstream stream ended WITHOUT the end marker
	// (mid-stream EOF / read error / fencing stop).
	EndUpstreamBroke
	// EndClientGone: the client disconnected mid-stream (已读用量保留,
	// 结算继续 — 设计 §7.2).
	EndClientGone
)

// StreamBody is the streaming half of an Outcome: a tee reader feeding the
// usage tap, the terminal-state probe, and the exactly-once settlement.
type StreamBody struct {
	reader   io.ReadCloser // tee into tap + fencing guard
	tap      providers.UsageTap
	cancel   context.CancelFunc
	keeper   *routing.LeaseKeeper
	finalize func(end StreamEnd) error
	once     sync.Once
	finErr   error
}

// Read implements io.Reader. Every read feeds the usage tap before the
// caller sees the bytes; a fencing conflict (lease reclaimed) aborts the
// stream instead of running ungated.
func (s *StreamBody) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

// Close stops the upstream connection. If the caller never finished the
// stream (facade relay teardown), Close finalizes as client-gone — usage
// already read is preserved and settled (设计 §7.2).
func (s *StreamBody) Close() error {
	err := s.reader.Close()
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.Finish(EndClientGone)
	return err
}

// Terminal reports whether the protocol end marker was observed.
func (s *StreamBody) Terminal() bool { return s.tap.Result().Terminal }

// Tap exposes the metering state (the estimate path and tests).
func (s *StreamBody) Tap() providers.TapResult { return s.tap.Result() }

// Finish settles the request exactly once with the relay's end state.
func (s *StreamBody) Finish(end StreamEnd) error {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.keeper != nil {
			ctx, cancel := detached(settleTimeout)
			defer cancel()
			s.keeper.Stop(ctx)
		}
		s.finErr = s.finalize(end)
	})
	return s.finErr
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// ChatCompletions runs the full §7.2 lifecycle for one logical call.
// clientProto is the protocol the CALLER speaks (openai_chat for /v1,
// kaya_chat for the facade); req.Model is the public model id.
//
// Every post-admission exit finalizes the reservation exactly once:
// released (confirmed zero consumption), settled (reported/estimated), or
// reconciliation_required (unknown) — never a dangling hold.
func (s *Service) ChatCompletions(ctx context.Context, p *domain.Principal, key *domain.APIKey,
	clientProto domain.Protocol, req *providers.ChatRequest) (*Outcome, error) {
	return s.run(ctx, p, key, clientProto, req, "")
}

// ChatCompletionsSticky is ChatCompletions with a sticky-session key (Task
// 13: Responses 会话链): the call dispatches ONLY to the account bound to
// (sessionKey, model). Binding semantics follow Task 12 — a dead binding is
// never silently continued on another account: a temporarily unschedulable
// bound account fails the call retryable; an invalidated account migrates
// EXPLICITLY (SessionBinder.Migrate, 新会话语义重建——协议层携带完整
// transcript 回放).
func (s *Service) ChatCompletionsSticky(ctx context.Context, p *domain.Principal, key *domain.APIKey,
	clientProto domain.Protocol, req *providers.ChatRequest, sessionKey string) (*Outcome, error) {
	return s.run(ctx, p, key, clientProto, req, sessionKey)
}

func (s *Service) run(ctx context.Context, p *domain.Principal, key *domain.APIKey,
	clientProto domain.Protocol, req *providers.ChatRequest, sessionKey string) (*Outcome, error) {
	if p == nil || p.BillingAccountID == "" {
		return nil, domain.NewError(domain.CodeInvalidKey, "gateway: unauthenticated caller (fail closed)")
	}

	// 1. Pin ONE catalog snapshot for the whole call (设计 §5).
	snap, err := s.snapshots.Current(ctx)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "gateway: catalog snapshot unavailable", err)
	}
	m, ok := snap.Model(req.Model)
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "model "+req.Model+" not found")
	}
	if !modelSpeaks(m, clientProto) {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"model "+m.ID+" is not served on protocol "+string(clientProto))
	}
	// 2. Capability validation BEFORE any spend (设计: 不支持的工具/模态明
	// 确报错). Modality/field-shape validation happened at the API edge.
	thinking := req.ThinkingEnabled != nil && *req.ThinkingEnabled
	if len(req.Tools) > 0 && !m.SupportsTools {
		return nil, domain.NewError(domain.CodeInvalidInput, "model "+m.ID+" does not support tools")
	}
	if thinking && !m.SupportsReasoning {
		return nil, domain.NewError(domain.CodeInvalidInput, "model "+m.ID+" does not support reasoning")
	}
	// Key narrowing can never widen beyond the entitlement set (设计 §4.2).
	if key != nil && len(key.ModelAllow) > 0 && !contains(key.ModelAllow, m.ID) {
		return nil, domain.NewError(domain.CodeModelNotAllowed, "model "+m.ID+" is not allowed for this API key")
	}

	// 3. Entitlement resolution (显式套餐优先，赠送不叠加/不兜底；无套餐时
	// 显式 PAYG 权益兜底 — Task 6/14).
	now := s.clock.Now()
	ent, err := s.entitlements.Resolve(ctx, p.BillingAccountID, m.ID, now)
	if err != nil {
		return nil, err // CodeModelNotAllowed when nothing grants the model
	}
	// 4. Pin the immutable policy revision; the charge source is selected
	// AT ENTRY and never switched mid-request (控制者裁决 5: 首版不在请求
	// 中途隐式切换套餐/余额).
	polRow, err := s.store.GetPolicyVersion(ctx, ent.PolicyVersionID)
	if err != nil {
		return nil, err
	}
	policy := polRow.Pure()

	var keyID *string
	if key != nil {
		keyID = &key.ID
	} else if p.APIKeyID != "" {
		keyID = &p.APIKeyID
	}
	newRequest := func() domain.Request {
		return domain.Request{
			BillingAccountID: p.BillingAccountID, APIKeyID: keyID,
			EntitlementID: ent.ID, ModelID: m.ID,
			Protocol: clientProto, Stream: req.Stream,
		}
	}

	var adm *quota.AdmissionResult
	var price accounting.PriceVersion // the pinned revision this call reserves & settles under
	if ent.SourceType == domain.SourcePAYG {
		// 无套餐按量（裁决 6）：钱包路径，权益/限流/并发照常。
		adm, price, err = s.admitWallet(ctx, newRequest(), *ent, policy, m, req, now)
		if err != nil {
			return nil, err
		}
	} else {
		// 5. Plan path — quota admission commits BEFORE any upstream call.
		creditPrice, err := s.pinCreditPrice(ctx, m.ID, now)
		if err != nil {
			return nil, err
		}
		price = creditPrice
		adm, err = s.quotaSvc.Admit(ctx, quota.AdmitCommand{
			Request:              newRequest(),
			Entitlement:          *ent,
			Policy:               policy,
			CreditPrice:          creditPrice,
			Model:                *m,
			EstimatedInputTokens: EstimateInputTokens(req),
			ClientMaxTokens:      req.MaxTokens,
		})
		if err != nil {
			// 套餐耗尽且策略允许套餐外 + 客户已显式开启钱包消费 → 本次请求
			// 固定走钱包（裁决 4/5）；钱包门控失败时按原 quota_exceeded 答复
			// （未开启超额时不动余额，也不静默切换）。
			if policy.Overage != quota.OverageAllowOverage || domain.CodeOf(err) != domain.CodeQuotaExceeded {
				return nil, err
			}
			wadm, wprice, werr := s.admitWallet(ctx, newRequest(), *ent, policy, m, req, now)
			if werr != nil {
				return nil, err
			}
			adm, price = wadm, wprice
		}
	}
	requestID := adm.RequestID
	// From here on, every exit path finalizes the reservation.

	// 6. Route: capability-compatible candidates only (设计 §5).
	needs := routing.Needs{Tools: len(req.Tools) > 0, Reasoning: thinking}
	cands, err := s.routingSvc.Candidates(ctx, snap, m.ID, needs)
	if err != nil {
		s.releaseAdmission(requestID, adm.AccountLease)
		return nil, domain.WrapError(domain.CodeInternal, "gateway: route candidates", err)
	}
	if len(cands) == 0 {
		s.releaseAdmission(requestID, adm.AccountLease)
		return nil, domain.NewError(domain.CodeUpstreamUnavailable,
			"no compatible deployment with capacity for model "+m.ID)
	}
	// Sticky session: narrow the candidate set to the bound account (Task
	// 13). Failures here release the reservation — no upstream spend.
	if sessionKey != "" {
		cands, err = s.pinSessionCandidates(ctx, sessionKey, m.ID, cands)
		if err != nil {
			s.releaseAdmission(requestID, adm.AccountLease)
			return nil, err
		}
	}

	return s.dispatchLoop(ctx, requestID, adm, req, price, cands)
}

// pinCreditPrice resolves the effective sale_credit revision (plan path).
func (s *Service) pinCreditPrice(ctx context.Context, modelID string, now time.Time) (accounting.PriceVersion, error) {
	priceRow, err := s.store.LatestPriceVersion(ctx, modelID, string(accounting.PriceSaleCredit), now)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return accounting.PriceVersion{}, accounting.ErrUnpriced
		}
		return accounting.PriceVersion{}, err
	}
	return priceRow.Pure()
}

// admitWallet runs the wallet-funded admission (PAYG entry or explicit
// overage fallback): pin the effective sale_money revision (其币种选定钱
// 包), then freeze the safe upper bound against the derived balance. The
// customer must have explicitly enabled wallet spend with a monthly limit
// (裁决 4) — otherwise the wallet is never touched.
func (s *Service) admitWallet(ctx context.Context, request domain.Request, ent domain.Entitlement,
	policy quota.Policy, m *domain.Model, req *providers.ChatRequest, now time.Time) (*quota.AdmissionResult, accounting.PriceVersion, error) {

	priceRow, err := s.store.LatestPriceVersion(ctx, m.ID, string(accounting.PriceSaleMoney), now)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, accounting.PriceVersion{}, accounting.ErrUnpriced
		}
		return nil, accounting.PriceVersion{}, err
	}
	moneyPrice, err := priceRow.Pure()
	if err != nil {
		return nil, accounting.PriceVersion{}, err
	}
	wallet, err := s.store.GetWalletByAccount(ctx, request.BillingAccountID, moneyPrice.Currency)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			// 无钱包 = 从未充值/开启：按余额不足答复（429），不泄露内部结构。
			return nil, accounting.PriceVersion{}, accounting.ErrInsufficientBalance
		}
		return nil, accounting.PriceVersion{}, err
	}
	adm, err := s.quotaSvc.AdmitWallet(ctx, quota.AdmitWalletCommand{
		Request:              request,
		Entitlement:          ent,
		Policy:               policy,
		MoneyPrice:           moneyPrice,
		WalletID:             wallet.ID,
		Model:                *m,
		EstimatedInputTokens: EstimateInputTokens(req),
		ClientMaxTokens:      req.MaxTokens,
		At:                   now,
	})
	if err != nil {
		return nil, accounting.PriceVersion{}, err
	}
	return adm, moneyPrice, nil
}

// pinSessionCandidates narrows the failover set to the session's bound
// account following the Task 12 binder semantics:
//   - Resolve OK: dispatch only to the bound account. When routing's live
//     view excludes it (in-process cooldown / quota race) the call fails
//     RETRYABLE — transient fullness never rebinds.
//   - CodeNotFound: bind to the first candidate's account (first turn of a
//     chain) and dispatch there.
//   - CodeConflict (account invalidated — reauth/disabled/quota-exhausted):
//     explicit Migrate to the first candidate (同事务终止旧绑定+建新), never
//     a silent account switch. The protocol layer carries the full
//     transcript, so the migration IS the protocol-level session rebuild.
func (s *Service) pinSessionCandidates(ctx context.Context, sessionKey, modelID string, cands []routing.Candidate) ([]routing.Candidate, error) {
	if s.sessions == nil {
		return nil, domain.NewError(domain.CodeInternal, "gateway: sticky sessions not configured")
	}
	for tries := 0; tries < 2; tries++ {
		binding, _, err := s.sessions.Resolve(ctx, sessionKey, modelID)
		switch {
		case err == nil:
			pinned := filterCandidatesByAccount(cands, binding.AccountID)
			if len(pinned) == 0 {
				return nil, domain.NewError(domain.CodeUpstreamUnavailable,
					"session-bound account is temporarily unavailable; retry shortly")
			}
			return pinned, nil
		case domain.CodeOf(err) == domain.CodeNotFound:
			if _, berr := s.sessions.Bind(ctx, sessionKey, modelID, cands[0].Account.ID); berr != nil {
				if domain.CodeOf(berr) == domain.CodeConflict {
					continue // parallel first-turn binding — re-resolve
				}
				return nil, domain.WrapError(domain.CodeInternal, "gateway: bind session", berr)
			}
			return filterCandidatesByAccount(cands, cands[0].Account.ID), nil
		case domain.CodeOf(err) == domain.CodeConflict:
			if _, merr := s.sessions.Migrate(ctx, sessionKey, modelID, cands[0].Account.ID); merr != nil {
				if domain.CodeOf(merr) == domain.CodeConflict {
					continue // concurrent migration — re-resolve
				}
				return nil, domain.WrapError(domain.CodeInternal, "gateway: migrate session", merr)
			}
			return filterCandidatesByAccount(cands, cands[0].Account.ID), nil
		default:
			return nil, domain.WrapError(domain.CodeInternal, "gateway: resolve session", err)
		}
	}
	return nil, domain.NewError(domain.CodeUpstreamUnavailable, "session binding raced; retry")
}

// filterCandidatesByAccount keeps only the triples served by accountID.
func filterCandidatesByAccount(cands []routing.Candidate, accountID string) []routing.Candidate {
	out := make([]routing.Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Account.ID == accountID {
			out = append(out, c)
		}
	}
	return out
}

// dispatchLoop walks the candidates with the bounded-retry policy. Every
// attempt persists its intent + lease BEFORE dispatch; retries never bill
// the customer twice (settlement happens once, on the winning attempt).
func (s *Service) dispatchLoop(ctx context.Context, requestID string, adm *quota.AdmissionResult,
	req *providers.ChatRequest, price accounting.PriceVersion, cands []routing.Candidate) (*Outcome, error) {

	maxAttempts := s.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = maxAttemptsDefault
	}
	if maxAttempts > len(cands) {
		maxAttempts = len(cands)
	}

	var lastErr error
	executedUnknown := false
	for i := 0; i < maxAttempts; i++ {
		cand := cands[i]
		adapter, ok := s.routingSvc.AdapterFor(cand.Deployment.Protocol)
		if !ok {
			continue
		}
		outcome, err := s.attempt(ctx, requestID, i+1, adm, req, price, cand, adapter)
		if err == nil {
			return outcome, nil
		}
		// Client/shape errors (*domain.Error, e.g. an Anthropic-illegal
		// history) are NOT upstream failures: nothing was sent, the
		// reservation releases, and the caller sees the real 400.
		var de *providers.DispatchError
		if !errorsAsDispatch(err, &de) {
			s.releaseAdmission(requestID, adm.AccountLease)
			return nil, err
		}
		lastErr = de
		if de.ExecutedUnknown {
			executedUnknown = true
		}
		switch {
		case providers.IsCanceled(de.Err):
			// The caller is gone: stop spending upstream (设计 §7.2 客户端
			// 取消后停止上游连接). No consumption is confirmed, so the
			// reservation releases — unless execution was already unknown.
			if executedUnknown {
				return nil, s.holdForReconciliation(requestID, de)
			}
			s.releaseAdmission(requestID, adm.AccountLease)
			return nil, domain.WrapError(domain.CodeUpstreamUnavailable, "caller canceled mid-dispatch", de)
		case !de.Retryable():
			// Non-retryable upstream rejection (e.g. 400/401/403): the
			// upstream answered without serving — zero consumption.
			s.releaseAdmission(requestID, adm.AccountLease)
			return nil, domain.WrapError(domain.CodeUpstreamUnavailable,
				"upstream rejected the request", de)
		case domain.CodeOf(de.Err) == domain.CodeInsufficientCapacity:
			// Account at its concurrency ceiling: try the next candidate,
			// never cool it (fullness is transient, not a failure) and never
			// retry INSIDE the account to dodge its capacity (设计 §8).
			continue
		default:
			// Retryable (429/5xx/transport): cool the account and fail over.
			s.routingSvc.CooldownAfterFailure(cand.Account.ID)
			continue
		}
	}

	// All attempts failed before any client output. Unknown execution holds
	// the reservation for reconciliation (禁止静默释放可能已发生的消费);
	// confirmed zero consumption releases (设计 §7.2 reserved → released).
	if executedUnknown {
		return nil, s.holdForReconciliation(requestID, lastErr)
	}
	s.releaseAdmission(requestID, adm.AccountLease)
	return nil, domain.WrapError(domain.CodeUpstreamUnavailable,
		"all compatible upstreams failed", lastErr)
}

// holdForReconciliation parks the request with its reservation intact and
// reports the dispatch failure.
func (s *Service) holdForReconciliation(requestID string, cause error) error {
	s.markReconciliation(requestID, "unknown_usage",
		fmt.Sprintf("dispatch failed with unknown execution state: %v", cause))
	return domain.WrapError(domain.CodeUpstreamUnavailable,
		"upstream execution state unknown; reservation held for reconciliation", cause)
}

// attempt runs ONE upstream try: persist intent + lease (one tx) → fencing
// check → egress re-validation → secret resolution → dispatch. A nil error
// returns the live outcome whose finalization belongs to the caller. A
// *domain.Error result is a client/shape failure (release + 400); a
// *providers.DispatchError feeds the retry policy.
func (s *Service) attempt(ctx context.Context, requestID string, attemptNo int, adm *quota.AdmissionResult,
	req *providers.ChatRequest, price accounting.PriceVersion, cand routing.Candidate,
	adapter providers.Adapter) (*Outcome, error) {

	// Attempt intent + upstream concurrency lease commit in ONE tx —
	// before any byte is sent upstream (设计 §7.2).
	now := s.clock.Now()
	attempt := &domain.Attempt{
		ID: uuid.NewString(), RequestID: requestID, AttemptNo: attemptNo,
		DeploymentID:      &cand.Deployment.ID,
		UpstreamAccountID: &cand.Account.ID,
		Status:            "dispatching",
		StartedAt:         &now,
	}
	lease, err := s.persistAttempt(ctx, attempt, cand, now)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeInsufficientCapacity {
			return nil, &providers.DispatchError{Err: err}
		}
		return nil, domain.WrapError(domain.CodeInternal, "gateway: persist attempt", err)
	}
	releaseLease := func() {
		rctx, rcancel := detached(settleTimeout)
		defer rcancel()
		if err := s.routingSvc.ReleaseLease(rctx, lease); err != nil {
			log.Printf("gateway: release upstream lease %s: %v", lease.ID, err)
		}
	}
	failAttempt := func(status, kind string) {
		s.finishAttempt(attempt.ID, status, kind, "")
		releaseLease()
	}

	// Fencing: the lease must still be a live authorization right before
	// the resource is used (Task 7: 超时回收不得与仍活跃请求重叠授权).
	if err := s.routingSvc.CheckLease(ctx, lease); err != nil {
		failAttempt("failed", "lease_lost")
		return nil, &providers.DispatchError{Err: err}
	}

	// Dispatch-time egress re-validation (Task 4: DNS-rebinding defense).
	if s.egress != nil {
		if err := s.egress.ValidateURL(ctx, cand.Deployment.BaseURL); err != nil {
			failAttempt("failed", "egress_rejected")
			return nil, &providers.DispatchError{Err: err}
		}
	}

	// Secret resolution (Task 4). A rotated/disabled credential rejects
	// this candidate, not the whole request.
	secret, _, err := s.secrets.ResolveSecret(ctx, cand.Account.CredentialID, nil)
	if err != nil {
		failAttempt("failed", "credential")
		return nil, &providers.DispatchError{Err: err}
	}

	extraHeaders, err := domain.ExtensionHeaders(cand.Deployment.Config)
	if err != nil {
		failAttempt("failed", "config")
		return nil, domain.WrapError(domain.CodeInternal, "gateway: deployment config", err)
	}

	if err := s.store.UpdateRequestStatus(ctx, requestID, domain.ReqDispatching, ""); err != nil {
		failAttempt("failed", "storage")
		return nil, domain.WrapError(domain.CodeInternal, "gateway: mark dispatching", err)
	}

	timeout := cand.Deployment.RequestTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	// The keeper renews BOTH leases while the dispatch is in flight (评审轮1
	// M1: the billing-account lease is request-scoped and must be renewed on
	// the non-streaming path too — previously only the streaming path renewed
	// it via the stream keeper, an asymmetric lapse window). A renewal
	// conflict fences the attempt — the stream guard then stops the upstream
	// call. Neither lease is released by the keeper: the upstream lease ends
	// per attempt (explicit release below), the account lease is released by
	// the finalizer (settle/release), never per attempt.
	keeper := s.routingSvc.NewLeaseKeeper(lease, adm.AccountLease)

	disp, err := providers.Dispatch(attemptCtx, s.client, adapter, &providers.Call{
		Deployment:   &cand.Deployment,
		Secret:       secret,
		RequestID:    requestID,
		Request:      req,
		OutputCap:    adm.OutputCap,
		ExtraHeaders: extraHeaders,
	})
	if err != nil {
		cancel()
		// Pause (no release): the account lease survives for the retry/
		// finalizer; only the attempt-scoped upstream lease is released
		// (releaseLease below / inside failAttempt).
		keeper.Pause()
		var de *providers.DispatchError
		if !errorsAsDispatch(err, &de) {
			// Build-time shape error (invalid_input): nothing was sent.
			failAttempt("failed", "payload")
			return nil, err
		}
		kind := "transport"
		if de.StatusCode != 0 {
			kind = fmt.Sprintf("http_%d", de.StatusCode)
		}
		status := "failed"
		if de.ExecutedUnknown {
			status = "unknown" // 上游是否执行未知 — 尝试状态如实记录
		}
		s.finishAttempt(attempt.ID, status, kind, "")
		releaseLease()
		return nil, de
	}

	var outcome *Outcome
	if req.Stream {
		outcome, err = s.onStreamDispatch(requestID, attempt, disp, keeper, cancel, adm, price, adapter)
	} else {
		outcome, err = s.onNonStreamDispatch(requestID, attempt, disp, keeper, cancel, adm, price, adapter)
	}
	if err == nil && outcome != nil {
		outcome.AccountID = cand.Account.ID
		outcome.Inclusion = adapter.Inclusion()
	}
	return outcome, err
}

// persistAttempt commits the attempt row and the upstream-account lease in
// one transaction.
func (s *Service) persistAttempt(ctx context.Context, a *domain.Attempt, cand routing.Candidate, now time.Time) (*domain.ConcurrencyLease, error) {
	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	lease, err := s.routingSvc.AcquireUpstreamLease(ctx, uow, cand.Account, a.RequestID, now)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	if err := s.store.InsertAttemptTx(ctx, uow, a); err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	return lease, nil
}

// onNonStreamDispatch settles immediately and returns the payload. Both
// leases end with the request: upstream explicitly after the keeper pauses,
// account by the finalizer below.
func (s *Service) onNonStreamDispatch(requestID string, attempt *domain.Attempt, disp *providers.DispatchResult,
	keeper *routing.LeaseKeeper, cancel context.CancelFunc, adm *quota.AdmissionResult,
	price accounting.PriceVersion, adapter providers.Adapter) (*Outcome, error) {
	cancel()
	kctx, kcancel := detached(settleTimeout)
	// Pause (no release), then release ONLY the attempt-scoped upstream
	// lease; the account lease stays held until the finalizer below (M1).
	keeper.Pause()
	for _, l := range keeper.Leases() {
		if adm.AccountLease != nil && l.ID == adm.AccountLease.ID {
			continue
		}
		if err := s.routingSvc.ReleaseLease(kctx, l); err != nil {
			log.Printf("gateway: release upstream lease %s: %v", l.ID, err)
		}
	}
	kcancel()
	s.finishAttempt(attempt.ID, "completed", "", disp.UpstreamRequestID)
	if err := s.store.UpdateRequestStatus(context.Background(), requestID, domain.ReqNonStreaming, ""); err != nil {
		log.Printf("gateway: mark non_streaming %s: %v", requestID, err)
	}

	// Usage: reported when the upstream provided it; when the upstream
	// genuinely has no usage on this path, the estimated path applies
	// (设计 §7.1 — never zero). The estimate uses the VISIBLE content of
	// the payload we hold (same 口径 as the streaming tap), so an upstream
	// that omits usage on a 200 cannot deterministically under-charge output.
	usage := disp.Usage
	source := domain.UsageReported
	raw := disp.UsageRaw
	if !disp.UsageReported || emptyBuckets(usage) {
		source = domain.UsageEstimated
		usage = s.estimateUsage(adm, disp.ContentBytes)
		raw = nil
	}
	sctx, scancel := detached(settleTimeout)
	defer scancel()
	s.settle(sctx, requestID, attempt.ID, usage, source, raw, price, adapter, adm)
	if adm.AccountLease != nil {
		if err := s.routingSvc.ReleaseLease(sctx, adm.AccountLease); err != nil {
			log.Printf("gateway: release account lease %s: %v", adm.AccountLease.ID, err)
		}
	}
	return &Outcome{RequestID: requestID, ModelID: price.ModelID, Payload: disp.Payload}, nil
}

// onStreamDispatch hands the live stream to the caller; settlement runs in
// StreamBody.Finish.
func (s *Service) onStreamDispatch(requestID string, attempt *domain.Attempt, disp *providers.DispatchResult,
	keeper *routing.LeaseKeeper, cancel context.CancelFunc, adm *quota.AdmissionResult,
	price accounting.PriceVersion, adapter providers.Adapter) (*Outcome, error) {
	if err := s.store.UpdateRequestStatus(context.Background(), requestID, domain.ReqStreaming, ""); err != nil {
		log.Printf("gateway: mark streaming %s: %v", requestID, err)
	}
	// The dispatch keeper pauses WITHOUT releasing; the stream keeper takes
	// over both leases for the whole stream lifetime (the dispatch keeper
	// already carries the request-scoped account lease since M1 — do NOT
	// append it twice).
	keeper.Pause()
	streamKeeper := s.routingSvc.NewLeaseKeeper(keeper.Leases()...)
	body := &StreamBody{
		tap:    disp.Stream.Tap,
		cancel: cancel,
		keeper: streamKeeper,
	}
	// The fencing guard watches the STREAM keeper — the one actually
	// renewing for the stream's whole lifetime and therefore the only one
	// that can observe a renewal conflict (a reclaimed slot aborts the
	// stream instead of running ungated, Task 7).
	body.reader = &guardedTeeReader{
		body:   disp.Stream.TeeBody(),
		keeper: streamKeeper,
	}
	body.finalize = func(end StreamEnd) error {
		// A keeper conflict mid-stream means the lease was reclaimed and the
		// guard cut the stream: classify as upstream-broken, never completed
		// (无论 tap 是否恰好看到终止标记 — 授权已失,按中断语义结算).
		if streamKeeper.Err() != nil && end == EndCompleted {
			end = EndUpstreamBroke
		}
		attemptStatus, kind := "completed", ""
		switch end {
		case EndUpstreamBroke:
			attemptStatus, kind = "failed", "stream_interrupted"
		case EndClientGone:
			attemptStatus, kind = "cancelled", "client_gone"
		}
		s.finishAttempt(attempt.ID, attemptStatus, kind, disp.UpstreamRequestID)

		tap := body.tap.Result()
		sctx, scancel := detached(settleTimeout)
		defer scancel()
		switch {
		case !tap.SawUsage && !tap.Terminal:
			// Interrupted with NO usage read: hold the reservation, park for
			// reconciliation — 不记零、不静默释放 (设计 §7.2).
			s.markReconciliation(requestID, "unknown_usage",
				"stream interrupted before any usage was reported")
			return nil
		case tap.SawUsage && tap.Terminal && !emptyBuckets(tap.Buckets):
			// Clean end with usage: reported.
			s.settle(sctx, requestID, attempt.ID,
				tap.Buckets, domain.UsageReported, tap.Raw, price, adapter, adm)
			return nil
		default:
			// 中断但已读用量 (estimated = 已知实际量), 或完整结束但上游确实
			// 不提供 usage → estimated 估算路径 (设计 §7.1/§7.2).
			usage := tap.Buckets
			raw := tap.Raw
			if !tap.SawUsage || emptyBuckets(usage) {
				usage = s.estimateUsage(adm, tap.ContentBytes)
				raw = nil
			}
			s.settle(sctx, requestID, attempt.ID,
				usage, domain.UsageEstimated, raw, price, adapter, adm)
			return nil
		}
	}
	return &Outcome{RequestID: requestID, ModelID: price.ModelID, Stream: body}, nil
}

// guardedTeeReader aborts the stream when the lease keeper reports a
// fencing conflict (a reclaimed slot must stop the upstream call instead of
// running ungated, Task 7).
type guardedTeeReader struct {
	body   io.ReadCloser
	keeper *routing.LeaseKeeper
}

func (r *guardedTeeReader) Read(p []byte) (int, error) {
	if err := r.keeper.Err(); err != nil {
		_ = r.body.Close()
		return 0, fmt.Errorf("gateway: concurrency lease lost (fenced): %w", err)
	}
	return r.body.Read(p)
}

func (r *guardedTeeReader) Close() error { return r.body.Close() }

// ---------------------------------------------------------------------------
// Settlement
// ---------------------------------------------------------------------------

// settleMaxAttempts bounds the in-process settlement retry (transient
// storage errors, e.g. a dropped connection). The usage fact lives in
// process memory until it commits — retrying within the detached context
// budget keeps a transient failure from falling into the reconciliation
// queue (where recovery would have to ESTIMATE what we actually know).
const settleMaxAttempts = 3

// settleBackoff is the pause between in-process settlement retries.
const settleBackoff = 200 * time.Millisecond

// settle runs the idempotent settlement in one transaction. A failure never
// silently drops the charge: after the bounded in-process retry the request
// is parked in reconciliation and the OnSettleError alarm fires
// (设计 §7.2: 计费不可静默丢失). A duplicate settlement is idempotent by
// design (唯一键防重复结算) and not an alarm.
//
// Overage (charge > hold): the REAL charge settles — clamping would
// silently free the excess (不隐藏负差额). The excess enqueues a
// settlement_overage reconciliation job in the SAME transaction and alarms;
// 继续透支 is blocked structurally: window used exceeding the limit drives
// remaining ≤ 0, so subsequent admissions reject (max(0, …) 口径 §6).
func (s *Service) settle(ctx context.Context, requestID, attemptID string, usage domain.UsageBuckets,
	source domain.UsageSource, raw json.RawMessage, price accounting.PriceVersion,
	adapter providers.Adapter, adm *quota.AdmissionResult) {

	// Normalize overlapping semantics exactly once (设计 §7.1) and price
	// under the pinned revision — the shared pure rule, never a second
	// implementation (Task 9: accounting.Decide).
	decision, err := accounting.Decide(domain.UsageRecord{
		RequestID: requestID, AttemptID: attemptID, Source: source, Buckets: usage,
		RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: raw},
	}, price, adapter.Inclusion(), adm.HoldMicros)
	if err != nil {
		s.parkSettleFailure(requestID, fmt.Errorf("settle decision: %w", err))
		return
	}

	// Per-attempt upstream cost with its basis (任务书: 为每次尝试保存成本
	// 来源). Only when an upstream_cost price version exists — no cost list,
	// no fabricated cost.
	cmd := domain.SettleCommand{
		RequestID:    requestID,
		Usage:        decision.Record,
		ChargeMicros: decision.Charge.Credit,
		SettledAt:    s.clock.Now(),
	}
	if adm.ChargeSource == domain.ChargeSourceWallet {
		// 钱包路径（Task 14）：金额计费，货币随冻结钱包；charge 与
		// ChargeMicros 同源（settle 事务内校验一致）。
		if decision.Charge.Money == nil {
			s.parkSettleFailure(requestID, fmt.Errorf("settle decision: wallet path produced no money charge"))
			return
		}
		cmd.WalletCharge = decision.Charge.Money
		cmd.ChargeMicros = domain.Microcredit(decision.Charge.Money.Micros)
	}
	cost, basis, cerr := s.attemptCost(price.ModelID, decision.Record)
	if cerr != nil {
		log.Printf("gateway: attempt cost for %s: %v (cost left unset, charge unaffected)", attemptID, cerr)
	} else if cost != nil {
		cmd.AttemptID = &attemptID
		cmd.AttemptCost = cost
		cmd.CostBasis = basis
	}

	// Overage detail is appended to the SAME settlement transaction, so a
	// settled-but-over-hold request never exists without its anomaly job.
	var overDetail json.RawMessage
	if over := decision.OverageMicros(); over > 0 {
		overDetail, _ = json.Marshal(map[string]interface{}{
			"schema_version":  1,
			"reserved_micros": int64(adm.HoldMicros),
			"charge_micros":   int64(cmd.ChargeMicros),
			"over_micros":     int64(over),
			"origin":          "settlement",
		})
	}

	var lastErr error
	for try := 0; try < settleMaxAttempts; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				goto parked
			case <-time.After(settleBackoff):
			}
		}
		if lastErr = s.settleOnce(ctx, cmd, overDetail); lastErr == nil {
			if overDetail != nil {
				log.Printf("ALARM gateway: request %s charge %d exceeds hold %d by %d micros (charge_source=%s; settled real amount; overage tracked, further overdraft blocked)",
					requestID, int64(cmd.ChargeMicros), int64(adm.HoldMicros), int64(decision.OverageMicros()), adm.ChargeSource)
			}
			return
		}
		if domain.CodeOf(lastErr) == domain.CodeConflict {
			return // already settled — idempotent delivery, not an error
		}
	}
parked:
	s.parkSettleFailure(requestID, fmt.Errorf("settle: %w", lastErr))
}

// settleOnce is one settlement transaction: usage fact + ledger charge +
// reserved→used conversion + request terminal state (+ overage job).
func (s *Service) settleOnce(ctx context.Context, cmd domain.SettleCommand, overDetail json.RawMessage) error {
	uow, err := s.store.Begin(ctx)
	if err != nil {
		return fmt.Errorf("settle begin: %w", err)
	}
	if err := s.store.Settle(ctx, uow, cmd); err != nil {
		_ = uow.Rollback(ctx)
		return fmt.Errorf("settle: %w", err)
	}
	if overDetail != nil {
		if err := s.store.EnqueueReconciliationJobTx(ctx, uow, postgres.EnqueueReconciliationCommand{
			RequestID: &cmd.RequestID, Reason: "settlement_overage",
			Detail:   overDetail,
			Deadline: s.clock.Now().Add(reconciliationDeadline),
		}); err != nil {
			_ = uow.Rollback(ctx)
			return fmt.Errorf("settle overage job: %w", err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		return fmt.Errorf("settle commit: %w", err)
	}
	return nil
}

// attemptCost prices the attempt's upstream cost under the effective
// upstream_cost list. The basis mirrors the usage source: reported usage →
// reported cost, estimated usage → estimated cost (设计 §7.1 cost_basis).
func (s *Service) attemptCost(modelID string, record domain.UsageRecord) (*domain.Money, *domain.CostBasis, error) {
	row, err := s.store.LatestPriceVersion(context.Background(), modelID, string(accounting.PriceUpstreamCost), s.clock.Now())
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, nil, nil // no procurement list — cost stays unset
		}
		return nil, nil, err
	}
	pv, err := row.Pure()
	if err != nil {
		return nil, nil, err
	}
	charge, err := pv.Quote(record, nil)
	if err != nil {
		return nil, nil, err
	}
	basis := domain.CostReported
	if record.Source == domain.UsageEstimated {
		basis = domain.CostEstimated
	}
	return charge.Money, &basis, nil
}

// parkSettleFailure holds the reservation and enqueues reconciliation —
// the anti-silent-loss path (设计 §7.2).
func (s *Service) parkSettleFailure(requestID string, cause error) {
	log.Printf("gateway: settlement failed for %s: %v — parking in reconciliation", requestID, cause)
	s.markReconciliation(requestID, "unknown_usage", cause.Error())
	if s.OnSettleError != nil {
		s.OnSettleError(requestID, cause)
	}
}

// markReconciliation parks a request whose consumption is unknown, keeping
// the reservation (禁止仅凭 TTL 释放全部预占).
func (s *Service) markReconciliation(requestID, reason, detail string) {
	ctx, cancel := detached(settleTimeout)
	defer cancel()
	uow, err := s.store.Begin(ctx)
	if err != nil {
		log.Printf("ERROR gateway: reconciliation begin %s: %v", requestID, err)
		return
	}
	if err := s.store.MarkReconciliationRequired(ctx, uow, requestID, reason,
		s.clock.Now().Add(reconciliationDeadline)); err != nil {
		_ = uow.Rollback(ctx)
		log.Printf("ERROR gateway: reconciliation mark %s: %v", requestID, err)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		log.Printf("ERROR gateway: reconciliation commit %s: %v", requestID, err)
	}
}

// releaseAdmission confirms zero upstream consumption: every hold drops in
// one transaction (reserved → released) and an unused five-hour window is
// voided in-transaction (设计 §6/§7.2). A failure leaves the hold for
// Task 9 recovery — loud, never silent.
func (s *Service) releaseAdmission(requestID string, accountLease *domain.ConcurrencyLease) {
	ctx, cancel := detached(settleTimeout)
	defer cancel()
	if err := s.quotaSvc.ReleaseAdmission(ctx, requestID); err != nil {
		log.Printf("ERROR gateway: release admission %s: %v (reservation held; recovery reconciles)", requestID, err)
	}
	if accountLease != nil {
		if err := s.routingSvc.ReleaseLease(ctx, accountLease); err != nil {
			log.Printf("gateway: release account lease %s: %v", accountLease.ID, err)
		}
	}
}

// finishAttempt marks the attempt's terminal state (best-effort; the row
// was committed before dispatch, so the audit trail exists either way).
func (s *Service) finishAttempt(id, status, errorKind, upstreamRequestID string) {
	ctx, cancel := detached(settleTimeout)
	defer cancel()
	if err := s.store.FinishAttempt(ctx, id, status, errorKind, upstreamRequestID, s.clock.Now()); err != nil {
		log.Printf("gateway: finish attempt %s: %v", id, err)
	}
}

// estimateUsage is the §7.1 estimated path: used ONLY when the upstream
// genuinely provided no usage (the adapters always request it explicitly).
// Input follows the admission estimator; output derives from the relayed
// content bytes. The estimate is auditable (source=estimated on the usage
// record) and correctable by later real evidence.
func (s *Service) estimateUsage(adm *quota.AdmissionResult, contentBytes int64) domain.UsageBuckets {
	in := adm.InputBound
	out := contentBytes / 2
	if contentBytes > 0 && out == 0 {
		out = 1
	}
	return domain.UsageBuckets{InputTokens: &in, OutputTokens: &out}
}

// EstimateInputTokens is the admission input estimate: a conservative
// bytes-based upper bound (CJK ~3 bytes/token, English ~4 bytes/token; /2
// over-estimates both — a reservation is a SAFE UPPER BOUND, 设计 §7.2),
// narrowed to the model context limit by quota.InputBound.
func EstimateInputTokens(req *providers.ChatRequest) int64 {
	var b int64
	for _, m := range req.Messages {
		b += int64(len(m.Content)) + 8
		for _, tc := range m.ToolCalls {
			b += int64(len(tc.Function.Name)+len(tc.Function.Arguments)) + 16
		}
	}
	for _, t := range req.Tools {
		b += int64(len(t))
	}
	if len(req.ToolChoice) > 0 {
		b += int64(len(req.ToolChoice))
	}
	return b/2 + 16
}

func modelSpeaks(m *domain.Model, p domain.Protocol) bool {
	for _, mp := range m.Protocols {
		if mp == p {
			return true
		}
	}
	return false
}

func contains(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func emptyBuckets(b domain.UsageBuckets) bool {
	return b.InputTokens == nil && b.OutputTokens == nil && b.CacheReadTokens == nil &&
		b.CacheWriteTokens == nil && b.ReasoningTokens == nil
}

func errorsAsDispatch(err error, target **providers.DispatchError) bool {
	for err != nil {
		if de, ok := err.(*providers.DispatchError); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
