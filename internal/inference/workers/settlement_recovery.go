// Package workers hosts the inference module's background tasks
// (设计 §3 workers/: 结算恢复、授权刷新、健康状态任务).
package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// runGuarded 执行一轮 worker pass 并兜底 panic：workers 在 cmd/server/main.go
// 以裸 goroutine 启动，未恢复的 panic 会终止整个 API 进程（连 HTTP 服务一起
// 带走）。panic 记 ALARM 后按 nil 错误返回，让 tick 循环继续——与 outbox
// worker 的 per-message recover 先例对齐（审查修复 Important-1）。
func runGuarded(name string, pass func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ALARM %s: pass panicked: %v — worker loop continues (panic 不再终止进程)", name, r)
			err = nil
		}
	}()
	return pass()
}

// settlement_recovery.go — 结算恢复 worker（Task 9，设计 §7.2：进程可能在
// "上游已执行、结果未落库"时崩溃；恢复以"保守估算 + 核对队列"为主，按供应
// 商能力例外逐笔核对）。
//
// 每轮（RunPass）按序执行：
//  1. 扫描滞留的在途请求（updated_at < now−grace 的非终态）——崩溃候选，
//     按"未发送 / 可能已发送 / 已知结果 / 未知费用"分类处置；
//  2. 处理核对队列中的待办任务（网关停放的 unknown usage、恢复结算后等待
//     证据窗口的任务）；
//  3. 期限告警：open 任务超过 deadline → escalated + 告警日志。禁止 TTL
//     到期自动视为零消费——升级只叫人，不释放、不记零；
//  4. 账本重建核对：活跃窗口 used ≡ Σ charge − Σ reversal ± 请求级
//     adjustment；差异入 reconciliation_jobs（ledger_mismatch），不自动修复；
//  5. 指标快照：队列积压、未知 usage、预占悬挂、结算延迟、账本差异，一行
//     结构化日志（仓库无 metrics 设施，控制者裁决用结构化日志计数）。
//
// 恢复动作全部幂等：释放/结算/入队都有守卫与唯一键，重复执行只产生一次
// 结果；与活请求竞争时守卫冲突按"已被并发收尾"跳过，绝不双扣。

// UsageVerifier is the provider-capability EXCEPTION path (设计 §7.2: 按供
// 应商能力例外地做逐笔上游核对). Mainstream Chat/Messages upstreams have no
// per-request execution query API, so production wires nil and recovery is
// always the conservative estimate; a provider that can look up an executed
// attempt implements this.
type UsageVerifier interface {
	// LookupUsage returns the upstream's own record of one executed attempt
	// (usage + the charge it prices to). ok=false means the provider cannot
	// look up executions — the caller falls back to the conservative
	// estimate. An error is a lookup failure, also falling back (loudly).
	LookupUsage(ctx context.Context, attempt domain.Attempt) (record domain.UsageRecord, charge domain.Microcredit, ok bool, err error)
}

// RecoveryStore is the persistence surface the worker needs; satisfied by
// *postgres.Store.
type RecoveryStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GetRequest(ctx context.Context, id string) (*domain.Request, error)
	ListAttempts(ctx context.Context, requestID string) ([]domain.Attempt, error)
	AttemptErrorKinds(ctx context.Context, requestID string) (map[string]string, error)
	LatestUsageRevision(ctx context.Context, attemptID string) (int, error)
	ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error)
	ListPendingReconciliationJobs(ctx context.Context, limit int) ([]postgres.ReconciliationJob, error)
	EscalateOverdueReconciliationJobs(ctx context.Context, now time.Time, limit int) ([]string, error)
	Release(ctx context.Context, w domain.UnitOfWork, requestID string) error
	ReleaseFromReconciliation(ctx context.Context, w domain.UnitOfWork, requestID string) error
	Settle(ctx context.Context, w domain.UnitOfWork, cmd domain.SettleCommand) error
	EnqueueReconciliationJobTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.EnqueueReconciliationCommand) error
	LatestPriceVersion(ctx context.Context, modelID, kind string, at time.Time) (*postgres.PriceVersion, error)
	ReconcileWindowAggregates(ctx context.Context) ([]accounting.WindowReconciliation, error)
	RecoveryMetrics(ctx context.Context, staleCutoff time.Time) (postgres.RecoveryMetrics, error)
}

// RecoveryConfig tunes the worker; zero values take the defaults.
type RecoveryConfig struct {
	// Interval between passes (default 30s).
	Interval time.Duration
	// BatchLimit caps each scan per pass (default 100).
	BatchLimit int
	// Grace is how long a request must be untouched before the sweep treats
	// it as a crash candidate (default 15min). It MUST exceed the worst live
	// phase of one request — attempt dispatch can run up to the deployment
	// RequestTimeout (default 10min), nginx lets the SSE relay run 700s, and
	// settlement adds up to 15s — otherwise the sweep would settle a LIVE
	// long request at its full hold and swallow the real usage arriving
	// later (审查修复 Critical 1). The floor is enforced by config.Validate;
	// the scan additionally skips any request with an attempt started inside
	// the grace window.
	Grace time.Duration
	// ReconciliationDeadline is the evidence window for jobs the worker
	// itself creates (default 24h, 设计 §7.2 恢复时限).
	ReconciliationDeadline time.Duration
}

func (c *RecoveryConfig) withDefaults() RecoveryConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = 30 * time.Second
	}
	if out.BatchLimit <= 0 {
		out.BatchLimit = 100
	}
	if out.Grace <= 0 {
		out.Grace = 15 * time.Minute
	}
	if out.ReconciliationDeadline <= 0 {
		out.ReconciliationDeadline = 24 * time.Hour
	}
	return out
}

// SettlementRecovery is the crash-recovery worker.
type SettlementRecovery struct {
	store    RecoveryStore
	clock    domain.Clock
	cfg      RecoveryConfig
	verifier UsageVerifier // nil = no provider supports execution lookup
}

// NewSettlementRecovery builds the worker; a nil clock uses the system
// clock (UTC). verifier is the provider-capability exception (nil for the
// mainstream upstreams — recovery then always estimates conservatively).
func NewSettlementRecovery(store RecoveryStore, clock domain.Clock, cfg RecoveryConfig, verifier UsageVerifier) *SettlementRecovery {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &SettlementRecovery{store: store, clock: clock, cfg: cfg.withDefaults(), verifier: verifier}
}

// Start runs one pass immediately (a restart is exactly when recovery is
// needed) and then ticks until ctx is done. It never panics the process:
// runGuarded 兜底每轮 pass 的 panic（ALARM 后继续 tick），失败的 pass 记
// ERROR 下一轮重试。
func (w *SettlementRecovery) Start(ctx context.Context) {
	log.Printf("inference settlement recovery worker started (interval=%s batch=%d grace=%s deadline=%s)",
		w.cfg.Interval, w.cfg.BatchLimit, w.cfg.Grace, w.cfg.ReconciliationDeadline)
	pass := func() error {
		_, err := w.RunPass(ctx)
		return err
	}
	if err := runGuarded("inference recovery", pass); err != nil {
		log.Printf("ERROR inference recovery: initial pass failed: %v", err)
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("inference settlement recovery worker stopped")
			return
		case <-ticker.C:
			if err := runGuarded("inference recovery", pass); err != nil {
				log.Printf("ERROR inference recovery: pass failed: %v", err)
			}
		}
	}
}

// PassStats counts one pass's outcomes — the audit trail of every recovery
// action (没有静默免费、双扣或无审计释放：每个动作计数并落结构化日志).
type PassStats struct {
	Scanned              int
	ReleasedNotSent      int // 未发送/全尝试确认零消费 → 释放（审计在册）
	SettledConservative  int // 未知费用 → 保守估算结算（= 预占额）
	SettledVerified      int // 供应商能力例外：逐笔核对成功 → 按真实证据结算
	RacingSkipped        int // 已被并发收尾（守卫冲突）— 幂等，非错误
	JobsProcessed        int
	JobsSettled          int
	JobsAwaitingEvidence int // 已结算/等待证据窗口或人工处置
	JobsNoEvidence       int // 无尝试无法估算 — 待人工（响亮）
	Escalated            int
	WindowMismatches     int
	Errors               int
}

// RunPass executes one recovery pass. Exported for tests and for the
// startup pass.
//
// 各阶段独立执行：单阶段失败计入 PassStats.Errors 并聚合返回，不跳过后续
// 阶段——阶段 3 的到期升级是卡死资金的安全网，不能被前序故障连带跳过
// （审查修复 Minor-6）。
func (w *SettlementRecovery) RunPass(ctx context.Context) (PassStats, error) {
	stats := PassStats{}
	now := w.clock.Now()
	cutoff := now.Add(-w.cfg.Grace)
	var errs []error

	// 1. Stale in-flight requests (crash candidates).
	stale, err := w.store.ListStaleOpenRequests(ctx, cutoff, w.cfg.BatchLimit)
	if err != nil {
		stats.Errors++
		errs = append(errs, fmt.Errorf("scan stale requests: %w", err))
	} else {
		stats.Scanned = len(stale)
		for i := range stale {
			w.recoverRequest(ctx, &stale[i], &stats)
		}
	}

	// 2. Pending reconciliation jobs.
	jobs, err := w.store.ListPendingReconciliationJobs(ctx, w.cfg.BatchLimit)
	if err != nil {
		stats.Errors++
		errs = append(errs, fmt.Errorf("list reconciliation jobs: %w", err))
	} else {
		for _, job := range jobs {
			w.processJob(ctx, job, &stats)
		}
	}

	// 3. Deadline escalation (告警 — never auto-zero, never release by TTL).
	escalated, err := w.store.EscalateOverdueReconciliationJobs(ctx, now, w.cfg.BatchLimit)
	if err != nil {
		stats.Errors++
		errs = append(errs, fmt.Errorf("escalate overdue jobs: %w", err))
	} else {
		stats.Escalated = len(escalated)
		for _, id := range escalated {
			log.Printf("ALARM inference recovery: reconciliation job %s past deadline — escalated for manual handling (禁止 TTL 到期视为零消费)", id)
		}
	}

	// 4. Ledger rebuild vs window aggregates.
	recon, err := w.store.ReconcileWindowAggregates(ctx)
	if err != nil {
		stats.Errors++
		errs = append(errs, fmt.Errorf("reconcile window aggregates: %w", err))
	} else {
		for _, m := range accounting.Mismatches(recon) {
			stats.WindowMismatches++
			log.Printf("ALARM inference recovery: ledger mismatch window %s (%s): stored=%d rebuilt=%d diff=%d — reconciliation job enqueued",
				m.WindowID, m.Kind, m.StoredUsed, m.RebuiltUsed, m.Diff())
			w.enqueueWindowMismatch(ctx, m, now)
		}
	}

	// 5. Metrics snapshot (结构化日志计数).
	metrics, merr := w.store.RecoveryMetrics(ctx, cutoff)
	if merr != nil {
		log.Printf("ERROR inference recovery: metrics snapshot: %v", merr)
	}
	lag := accounting.SettlementLag(now, metrics.OldestOpenUpdatedAt)
	log.Printf("inference_recovery pass: scanned=%d released_not_sent=%d settled_conservative=%d settled_verified=%d racing_skipped=%d "+
		"jobs_processed=%d jobs_settled=%d jobs_awaiting_evidence=%d jobs_no_evidence=%d escalated=%d "+
		"backlog=%d unknown_usage=%d dangling_holds=%d open_requests=%d settlement_lag_s=%.0f ledger_mismatches=%d errors=%d",
		stats.Scanned, stats.ReleasedNotSent, stats.SettledConservative, stats.SettledVerified, stats.RacingSkipped,
		stats.JobsProcessed, stats.JobsSettled, stats.JobsAwaitingEvidence, stats.JobsNoEvidence, stats.Escalated,
		metrics.ReconciliationBacklog, metrics.UnknownUsageRequests, metrics.DanglingHolds,
		metrics.OpenRequests, lag.Seconds(), stats.WindowMismatches, stats.Errors)
	return stats, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// 分类：未发送 / 可能已发送 / 已知结果 / 未知费用
// ---------------------------------------------------------------------------

// recoveryClass is the recovery decision for one stranded request.
type recoveryClass int

const (
	// classNotSent: provably zero upstream consumption — the request row
	// committed at admission, and either no attempt intent ever committed
	// (attempts persist BEFORE dispatch, so no row = nothing was sent) or
	// every attempt failed in a way that proves zero consumption.
	classNotSent recoveryClass = iota
	// classUnknownCharge: possibly executed, or executed with the outcome
	// lost (已知结果但未知费用). The conservative estimate settles it.
	classUnknownCharge
)

// zeroConsumptionErrorKinds are attempt failure kinds that PROVE the
// upstream never served the request: payload/egress/credential/config/
// lease/storage failures happen before any byte is dispatched; "transport"
// on a 'failed' attempt is the definitely-not-executed half of
// classifyTransport (DNS/connect-refused — unknown-execution transports are
// persisted as attempt status 'unknown' instead); "http_*" is an upstream
// rejection — the request was received and REFUSED, no consumption.
func zeroConsumptionErrorKind(kind string) bool {
	if strings.HasPrefix(kind, "http_") {
		return true
	}
	switch kind {
	case "payload", "egress_rejected", "credential", "config", "lease_lost", "storage", "transport":
		return true
	}
	return false
}

// classify decides the recovery path from the persisted facts. The anchor
// is the latest attempt whose outcome is NOT proven-zero — the usage fact
// of a conservative settlement attaches to it.
func classify(attempts []domain.Attempt, errorKinds map[string]string) (recoveryClass, *domain.Attempt) {
	var anchor *domain.Attempt
	for i := range attempts {
		a := &attempts[i]
		if a.Status == "failed" && zeroConsumptionErrorKind(errorKinds[a.ID]) {
			continue // 确认零消费
		}
		anchor = a
	}
	if anchor == nil {
		return classNotSent, nil
	}
	return classUnknownCharge, anchor
}

// recoverRequest disposes of one stranded in-flight request.
func (w *SettlementRecovery) recoverRequest(ctx context.Context, req *domain.Request, stats *PassStats) {
	attempts, kinds, err := w.loadEvidence(ctx, req.ID)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: load evidence for %s: %v", req.ID, err)
		return
	}
	cls, anchor := classify(attempts, kinds)
	if cls == classNotSent {
		w.releaseProvenZero(ctx, req, false, stats)
		return
	}
	w.settleUnknownCharge(ctx, req, anchor, "crash_recovery", stats)
}

// processJob drives one pending reconciliation job.
func (w *SettlementRecovery) processJob(ctx context.Context, job postgres.ReconciliationJob, stats *PassStats) {
	stats.JobsProcessed++
	if job.RequestID == nil {
		// 窗口级账本差异：无自动修复 — 等人工（核对不自动重写账本）。
		stats.JobsAwaitingEvidence++
		return
	}
	req, err := w.store.GetRequest(ctx, *job.RequestID)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: job %s request %s: %v", job.ID, *job.RequestID, err)
		return
	}
	switch req.Status {
	case domain.ReqSettled, domain.ReqReleased:
		// 已终态：任务是证据窗口/异常记录，等期限或人工；期限到 → 升级告警。
		stats.JobsAwaitingEvidence++
		return
	}
	attempts, kinds, err := w.loadEvidence(ctx, req.ID)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: job %s evidence %s: %v", job.ID, req.ID, err)
		return
	}
	if len(attempts) == 0 && req.Status == domain.ReqReconciliationRequired {
		// 停放却无任何尝试意图：无法构造估算锚点，响亮待人工 — 不猜。
		stats.JobsNoEvidence++
		log.Printf("ALARM inference recovery: job %s request %s parked with no attempts — cannot estimate; awaiting manual handling",
			job.ID, req.ID)
		return
	}
	cls, anchor := classify(attempts, kinds)
	if cls == classNotSent {
		// 停放期间拿到零消费证明 → released（证据驱动，非 TTL）。
		w.releaseProvenZero(ctx, req, true, stats)
		return
	}
	before := stats.SettledConservative + stats.SettledVerified
	w.settleUnknownCharge(ctx, req, anchor, "reconciliation:"+job.Reason, stats)
	if stats.SettledConservative+stats.SettledVerified > before {
		stats.JobsSettled++
	}
}

// loadEvidence reads the attempt rows and their error kinds.
func (w *SettlementRecovery) loadEvidence(ctx context.Context, requestID string) ([]domain.Attempt, map[string]string, error) {
	attempts, err := w.store.ListAttempts(ctx, requestID)
	if err != nil {
		return nil, nil, err
	}
	kinds, err := w.store.AttemptErrorKinds(ctx, requestID)
	if err != nil {
		return nil, nil, err
	}
	return attempts, kinds, nil
}

// releaseProvenZero releases the holds of a request whose upstream
// consumption is PROVEN zero (设计 §7.2 reserved → released). From
// reconciliation_required the release is evidence-driven (never TTL); the
// job resolves in the same transaction.
func (w *SettlementRecovery) releaseProvenZero(ctx context.Context, req *domain.Request, fromReconciliation bool, stats *PassStats) {
	uow, err := w.store.Begin(ctx)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: release begin %s: %v", req.ID, err)
		return
	}
	release := w.store.Release
	if fromReconciliation {
		release = w.store.ReleaseFromReconciliation
	}
	if err := release(ctx, uow, req.ID); err != nil {
		_ = uow.Rollback(ctx)
		w.noteConflictOrError(req.ID, "release", err, stats)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: release commit %s: %v", req.ID, err)
		return
	}
	stats.ReleasedNotSent++
	log.Printf("inference recovery: request %s released — zero upstream consumption proven (audit: reservations released%s)",
		req.ID, map[bool]string{true: ", reconciliation job resolved", false: ""}[fromReconciliation])
}

// settleUnknownCharge settles a request whose consumption is UNKNOWN: the
// provider-capability exception runs first (Verifier, nil in production for
// the mainstream upstreams); otherwise the conservative estimate settles at
// the full reserved hold (设计 §7.2 补充段：估算为主 — 估算须可审计并可被
// 后续真实证据冲正；永不静默免费、永不双扣、禁止仅凭 TTL 释放).
func (w *SettlementRecovery) settleUnknownCharge(ctx context.Context, req *domain.Request, anchor *domain.Attempt, reason string, stats *PassStats) {
	if req.ReservedMicros == nil {
		stats.Errors++
		log.Printf("ALARM inference recovery: request %s has no reserved amount — cannot estimate; needs manual handling", req.ID)
		return
	}

	// 例外路径：供应商支持按请求执行查询时按真实证据结算。
	if w.verifier != nil && anchor.UpstreamRequestID != "" {
		record, charge, ok, err := w.verifier.LookupUsage(ctx, *anchor)
		switch {
		case err != nil:
			log.Printf("WARN inference recovery: usage lookup for %s (attempt %s) failed: %v — falling back to conservative estimate",
				req.ID, anchor.ID, err)
		case ok:
			record.RequestID = req.ID
			w.commitSettlement(ctx, req, anchor, record, charge, reason, stats, true)
			return
		}
	}

	revision, err := w.store.LatestUsageRevision(ctx, anchor.ID)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: usage revision %s: %v", req.ID, err)
		return
	}
	record := accounting.ConservativeRecord(req.ID, anchor.ID, req, reason, revision+1)
	w.commitSettlement(ctx, req, anchor, record, *req.ReservedMicros, reason, stats, false)
}

// commitSettlement runs the one-transaction settlement (usage fact + ledger
// charge + reserved→used + request terminal state) plus, for conservative
// estimates, the crash_recovery verification job in the SAME transaction —
// a recovered request is settled AND tracked in one commit.
func (w *SettlementRecovery) commitSettlement(ctx context.Context, req *domain.Request, anchor *domain.Attempt,
	record domain.UsageRecord, charge domain.Microcredit, reason string, stats *PassStats, verified bool) {

	cmd := domain.SettleCommand{
		RequestID: req.ID, Usage: record, ChargeMicros: charge, SettledAt: w.clock.Now(),
	}
	// Upstream cost with basis when a cost list exists (所有尝试成本可追溯);
	// no cost list → no fabricated cost. Verified evidence is reported cost,
	// the estimate is estimated cost (设计 §7.1 cost_basis).
	if cost, basis, cerr := w.estimateCost(ctx, req.ModelID, record, verified); cerr != nil {
		log.Printf("WARN inference recovery: cost for %s: %v (cost left unset, charge unaffected)", req.ID, cerr)
	} else if cost != nil {
		cmd.AttemptID = &anchor.ID
		cmd.AttemptCost = cost
		cmd.CostBasis = basis
	}

	uow, err := w.store.Begin(ctx)
	if err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: settle begin %s: %v", req.ID, err)
		return
	}
	if err := w.store.Settle(ctx, uow, cmd); err != nil {
		_ = uow.Rollback(ctx)
		w.noteConflictOrError(req.ID, "settle", err, stats)
		return
	}
	if !verified {
		// 核对队列 + 期限告警：保守估算保留证据窗口（可被后续真实证据冲正）；
		// 期限到 → 升级叫人。结算与任务同一事务提交 — 无"已结算但没任务"窗口。
		detail, _ := json.Marshal(map[string]interface{}{
			"schema_version": 1,
			"recovery":       "conservative_estimate",
			"basis":          reason,
			"charge_micros":  int64(charge),
			"attempt_id":     anchor.ID,
		})
		if err := w.store.EnqueueReconciliationJobTx(ctx, uow, postgres.EnqueueReconciliationCommand{
			RequestID: &req.ID, Reason: "crash_recovery",
			Detail: detail, Deadline: w.clock.Now().Add(w.cfg.ReconciliationDeadline),
		}); err != nil {
			_ = uow.Rollback(ctx)
			stats.Errors++
			log.Printf("ERROR inference recovery: enqueue verification job %s: %v (settlement rolled back — retry next pass)", req.ID, err)
			return
		}
	}
	if err := uow.Commit(ctx); err != nil {
		stats.Errors++
		log.Printf("ERROR inference recovery: settle commit %s: %v", req.ID, err)
		return
	}
	if verified {
		stats.SettledVerified++
	} else {
		stats.SettledConservative++
	}
	log.Printf("inference recovery: request %s settled (%s) charge=%d microcredits basis=%s attempt=%s",
		req.ID, map[bool]string{true: "verified", false: "conservative_estimate"}[verified], int64(charge), reason, anchor.ID)
}

// estimateCost prices the recovery settlement's upstream cost under the
// effective upstream_cost list. Basis: verified evidence → reported,
// conservative estimate → estimated (设计 §7.1).
func (w *SettlementRecovery) estimateCost(ctx context.Context, modelID string, record domain.UsageRecord, verified bool) (*domain.Money, *domain.CostBasis, error) {
	row, err := w.store.LatestPriceVersion(ctx, modelID, string(accounting.PriceUpstreamCost), w.clock.Now())
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, nil, nil
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
	basis := domain.CostEstimated
	if verified {
		basis = domain.CostReported
	}
	return charge.Money, &basis, nil
}

// noteConflictOrError: a guard conflict means a concurrent live path
// finalized the request between the scan and our transaction — idempotent,
// not an error (重复工作投递只产生一次结果).
func (w *SettlementRecovery) noteConflictOrError(requestID, op string, err error, stats *PassStats) {
	if domain.CodeOf(err) == domain.CodeConflict {
		stats.RacingSkipped++
		return
	}
	stats.Errors++
	log.Printf("ERROR inference recovery: %s %s: %v", op, requestID, err)
}

// enqueueWindowMismatch files a window-level ledger_mismatch job.
func (w *SettlementRecovery) enqueueWindowMismatch(ctx context.Context, m accounting.WindowReconciliation, now time.Time) {
	detail, _ := json.Marshal(map[string]interface{}{
		"schema_version": 1,
		"kind":           string(m.Kind),
		"stored_used":    m.StoredUsed,
		"rebuilt_used":   m.RebuiltUsed,
		"diff_micros":    m.Diff(),
	})
	uow, err := w.store.Begin(ctx)
	if err != nil {
		log.Printf("ERROR inference recovery: mismatch job begin: %v", err)
		return
	}
	if err := w.store.EnqueueReconciliationJobTx(ctx, uow, postgres.EnqueueReconciliationCommand{
		WindowID: m.WindowID, Reason: "ledger_mismatch",
		Detail: detail, Deadline: now.Add(w.cfg.ReconciliationDeadline),
	}); err != nil {
		_ = uow.Rollback(ctx)
		log.Printf("ERROR inference recovery: mismatch job %s: %v", m.WindowID, err)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		log.Printf("ERROR inference recovery: mismatch job commit %s: %v", m.WindowID, err)
	}
}
