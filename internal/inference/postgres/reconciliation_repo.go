package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// reconciliation_repo.go — inference_reconciliation_jobs 队列、崩溃恢复扫描、
// 账本重建核对与估算修正（冲正/补差）的事务写入（Task 9，设计 §7.2/§7.3）。
//
// 核心不变式：
//   - 核对任务按请求唯一（UNIQUE request_id）；窗口级任务 request_id=NULL，
//     由部分唯一索引按 detail->>'window_id' 去重（migration 031）。
//   - 期限告警：pending/running 超过 deadline_at → escalated + 告警；禁止
//     TTL 到期自动视为零消费（没有任何路径把超时任务变成释放/记零）。
//   - 修正估算保留原记录：新 usage revision 追加 + reversal 冲正原 charge +
//     adjustment 分录补差；窗口/Key 预算按 delta 同事务校正；账本行永不 UPDATE。

// ReconciliationJob is one persisted recovery/audit task.
type ReconciliationJob struct {
	ID         string
	RequestID  *string // nil = 窗口级任务（ledger_mismatch）
	Reason     string
	Status     string
	Detail     json.RawMessage
	DeadlineAt time.Time
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// EnqueueReconciliationCommand adds or refreshes one job. RequestID nil
// creates a window-level job (WindowID required for dedup).
type EnqueueReconciliationCommand struct {
	RequestID *string
	WindowID  string // window-level dedup key (request_id IS NULL 时必填)
	Reason    string
	Detail    json.RawMessage
	Deadline  time.Time
}

// EnqueueReconciliationJobTx upserts one reconciliation job inside the
// caller's transaction. Request-scoped jobs dedup on UNIQUE(request_id);
// window-level jobs dedup on the partial (detail->>'window_id') index. A
// repeat delivery merges detail (jsonb ||, new keys win) and extends the
// deadline — never spawns a second job for the same subject.
func (s *Store) EnqueueReconciliationJobTx(ctx context.Context, w domain.UnitOfWork, cmd EnqueueReconciliationCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	detail := cmd.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{"schema_version":1}`)
	}
	if cmd.RequestID != nil {
		// Upsert: merge detail, extend the deadline. A new reason over a
		// resolved/escalated job is NEW information (e.g. the correction that
		// resolved crash_recovery just detected an overage) → reopen pending
		// under the new reason; the merged detail keeps the prior history.
		// Same-reason re-delivery over an open job is the idempotent merge.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO inference_reconciliation_jobs (id, request_id, reason, detail, deadline_at)
			 VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (request_id) DO UPDATE
			 SET detail = inference_reconciliation_jobs.detail || EXCLUDED.detail,
			     deadline_at = GREATEST(inference_reconciliation_jobs.deadline_at, EXCLUDED.deadline_at),
			     status = CASE
			         WHEN inference_reconciliation_jobs.status IN ('resolved','escalated')
			          AND inference_reconciliation_jobs.reason <> EXCLUDED.reason
			         THEN 'pending' ELSE inference_reconciliation_jobs.status END,
			     reason = CASE
			         WHEN inference_reconciliation_jobs.status IN ('resolved','escalated')
			          AND inference_reconciliation_jobs.reason <> EXCLUDED.reason
			         THEN EXCLUDED.reason ELSE inference_reconciliation_jobs.reason END,
			     resolved_at = CASE
			         WHEN inference_reconciliation_jobs.status IN ('resolved','escalated')
			          AND inference_reconciliation_jobs.reason <> EXCLUDED.reason
			         THEN NULL ELSE inference_reconciliation_jobs.resolved_at END,
			     updated_at = now()`,
			uuid.NewString(), *cmd.RequestID, cmd.Reason, detail, cmd.Deadline)
		return mapError("reconciliation: enqueue request job", err)
	}
	if cmd.WindowID == "" {
		return domain.NewError(domain.CodeInvalidInput,
			"reconciliation: window-level job needs window_id for dedup")
	}
	// Window-level: the partial unique index dedups open jobs; a resolved/
	// escalated predecessor must NOT block a fresh finding, so update first.
	// Refresh merges detail ONLY and keeps the ORIGINAL deadline (审查修复
	// Important 2): pushing the deadline forward on every pass would make a
	// persistent drift unescalatable — 期限告警语义必须在漂移持续期间成立.
	windowed := mergeWindowDetail(detail, cmd.WindowID)
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_reconciliation_jobs
		 SET detail = inference_reconciliation_jobs.detail || $1, updated_at = now()
		 WHERE request_id IS NULL AND reason = $2
		   AND detail->>'window_id' = $3 AND status IN ('pending','running')`,
		windowed, cmd.Reason, cmd.WindowID)
	if err != nil {
		return mapError("reconciliation: refresh window job", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_reconciliation_jobs (id, request_id, reason, detail, deadline_at)
		 VALUES ($1,NULL,$2,$3,$4)
		 ON CONFLICT ((detail->>'window_id')) WHERE request_id IS NULL AND reason = 'ledger_mismatch' AND status IN ('pending','running')
		 DO NOTHING`,
		uuid.NewString(), cmd.Reason, windowed, cmd.Deadline); err != nil {
		return mapError("reconciliation: enqueue window job", err)
	}
	return nil
}

// mergeWindowDetail stamps the dedup key into the detail document.
func mergeWindowDetail(detail json.RawMessage, windowID string) json.RawMessage {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(detail, &doc); err != nil || doc == nil {
		doc = map[string]json.RawMessage{}
	}
	if _, ok := doc["schema_version"]; !ok {
		doc["schema_version"] = json.RawMessage("1")
	}
	w, _ := json.Marshal(windowID)
	doc["window_id"] = w
	out, err := json.Marshal(doc)
	if err != nil {
		return detail
	}
	return out
}

// openRequestStatuses are the non-terminal §7.2 states a crash can strand.
// reconciliation_required is deliberately EXCLUDED: those requests are
// driven by their jobs (期限告警), not by the staleness sweep.
var openRequestStatuses = []string{
	string(domain.ReqReserved), string(domain.ReqDispatching),
	string(domain.ReqStreaming), string(domain.ReqNonStreaming),
	string(domain.ReqSettling),
}

// ListStaleOpenRequests returns in-flight requests whose last state change
// is older than the cutoff — crash candidates. Two guards keep the sweep off
// LIVE requests (审查修复 Critical 1):
//
//  1. updated_at < cutoff — state-transition recency. NOT sufficient alone:
//     a live request can sit in ONE state far longer than any naive grace
//     (attempt dispatch runs up to the deployment RequestTimeout, default
//     10min; nginx lets /v1/chat/completions SSE run 700s), so
//  2. NOT EXISTS an attempt started within the grace window — attempt intent
//     persists BEFORE dispatch, so a recent started_at means the request may
//     legitimately still be mid-flight. Grace (> worst live phase) then
//     guarantees any attempt older than grace has exceeded its timeout.
//
// Sweeping a live request would settle it at the FULL hold while its stream
// is still producing — the real usage arriving later would hit the settled
// guard and be swallowed as a "duplicate" (多扣). These guards make that
// structurally impossible; the grace floor is enforced in config.Validate.
func (s *Store) ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error) {
	if limit <= 0 {
		limit = 100
	}
	var ids []string
	if err := s.db.SelectContext(ctx, &ids,
		`SELECT id FROM inference_requests r
		 WHERE r.status = ANY($1) AND r.updated_at < $2
		   AND NOT EXISTS (
		       SELECT 1 FROM inference_attempts a
		       WHERE a.request_id = r.id AND a.started_at IS NOT NULL AND a.started_at >= $2)
		 ORDER BY r.updated_at LIMIT $3`,
		openRequestStatuses, cutoff.UTC(), limit); err != nil {
		return nil, mapError("recovery: scan stale open", err)
	}
	out := make([]domain.Request, 0, len(ids))
	for _, id := range ids {
		r, err := s.GetRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// ListPendingReconciliationJobs returns open jobs oldest-first.
func (s *Store) ListPendingReconciliationJobs(ctx context.Context, limit int) ([]ReconciliationJob, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.selectJobs(ctx,
		`SELECT id, request_id, reason, status, detail, deadline_at, created_at, resolved_at
		 FROM inference_reconciliation_jobs
		 WHERE status = 'pending' ORDER BY created_at LIMIT $1`, limit)
}

func (s *Store) selectJobs(ctx context.Context, query string, args ...interface{}) ([]ReconciliationJob, error) {
	var rows []struct {
		ID         string          `db:"id"`
		RequestID  sql.NullString  `db:"request_id"`
		Reason     string          `db:"reason"`
		Status     string          `db:"status"`
		Detail     json.RawMessage `db:"detail"`
		DeadlineAt time.Time       `db:"deadline_at"`
		CreatedAt  time.Time       `db:"created_at"`
		ResolvedAt *time.Time      `db:"resolved_at"`
	}
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("reconciliation: list jobs", err)
	}
	out := make([]ReconciliationJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReconciliationJob{
			ID: r.ID, RequestID: strFromNull(r.RequestID), Reason: r.Reason,
			Status: r.Status, Detail: r.Detail, DeadlineAt: r.DeadlineAt,
			CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
		})
	}
	return out, nil
}

// EscalateOverdueReconciliationJobs moves open jobs past their deadline to
// 'escalated' and returns their IDs for the alarm log (设计 §7.2: 设定恢复
// 时限、告警和人工调整入口，避免额度永久悬挂). Escalation NEVER releases or
// zeroes anything — it only pages a human.
func (s *Store) EscalateOverdueReconciliationJobs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	var ids []string
	if err := s.db.SelectContext(ctx, &ids,
		`UPDATE inference_reconciliation_jobs
		 SET status = 'escalated', updated_at = now()
		 WHERE id IN (
		     SELECT id FROM inference_reconciliation_jobs
		     WHERE status IN ('pending','running') AND deadline_at <= $1
		     ORDER BY deadline_at LIMIT $2)
		 RETURNING id`, now.UTC(), limit); err != nil {
		return nil, mapError("reconciliation: escalate", err)
	}
	return ids, nil
}

// ResolveReconciliationJob closes a job with a note (operator action or
// evidence-driven correction). Only an open job resolves.
func (s *Store) ResolveReconciliationJob(ctx context.Context, id string, note string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_reconciliation_jobs
		 SET status = 'resolved', resolved_at = now(), updated_at = now(),
		     detail = detail || $2
		 WHERE id = $1 AND status IN ('pending','running','escalated')`,
		id, mustJSON(map[string]interface{}{"schema_version": 1, "resolution": note}))
	if err != nil {
		return mapError("reconciliation: resolve", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"reconciliation: job already resolved or missing")
	}
	return nil
}

func mustJSON(v interface{}) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// ---------------------------------------------------------------------------
// CorrectSettlement — 估算修正：reversal + adjustment 分录，保留原记录
// ---------------------------------------------------------------------------

// CorrectionCommand replaces a settled estimate with real evidence. The
// original charge and usage revision stay untouched (设计 §7.3).
type CorrectionCommand struct {
	RequestID string
	// Corrected is the new usage fact (Source/Buckets); AttemptID must name
	// the attempt it revises — the revision number is assigned as max+1.
	Corrected domain.UsageRecord
	// CorrectedMicros is the re-priced customer consumption (microcredit).
	CorrectedMicros domain.Microcredit
	Reason          string
	// OperatorSubject + IdempotencyKey follow the adjustment contract
	// (设计 §9.2: 有原因、对象、金额/额度、操作者和幂等键). Recovery-driven
	// corrections use operator_subject "system:recovery".
	OperatorSubject string
	IdempotencyKey  string
	At              time.Time
}

// CorrectSettlement applies an estimate correction in ONE transaction.
// 分录模式（Task 9 裁定 — 补差 delta 语义，冲正保留给全额作废）:
//
//  1. lock the request (must be settled — an unsettled request settles
//     first, corrections never replace the first settlement);
//  2. the delta base is the request's CURRENT settled amount
//     (settled_micros), so chained corrections compose:
//     settle A → correct to B → correct to C leaves net C;
//  3. append the corrected usage fact as the attempt's next revision
//     (original revision preserved — 不重写历史);
//  4. corrected == 0 且 base > 0: append a REVERSAL of the current charge
//     effect (entry_type='reversal', amount=base, reverses the charge row,
//     linked to the compensation row) — 冲正分录;
//     corrected != 0: append an ADJUSTMENT entry of |corrected − base|
//     (debit = 补差 / credit = 退还) — 补差分录;
//     either way the inference_adjustments row carries reason + operator +
//     idempotency key (设计 §9.2; 重复投递撞唯一键只生效一次);
//  5. correct every bound window's used and the Key budget by the SAME
//     signed delta (guards forbid a negative aggregate);
//  6. update the request's settled amount/usage status;
//  7. resolve the request's open reconciliation job (evidence landed);
//  8. re-check the hold: corrected > reserved enqueues settlement_overage
//     (超出预占显示异常,不隐藏负差额).
//
// 账本重建口径随之成立：窗口 used ≡ Σ charge − Σ reversal ± Σ 请求级
// adjustment（debit + / credit −）。
//
// A replayed IdempotencyKey hits the UNIQUE key → CodeConflict: the caller
// treats it as already-applied.
func (s *Store) CorrectSettlement(ctx context.Context, w domain.UnitOfWork, cmd CorrectionCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if cmd.IdempotencyKey == "" || cmd.OperatorSubject == "" || cmd.Reason == "" {
		return domain.NewError(domain.CodeInvalidInput,
			"correction: reason, operator subject and idempotency key are required")
	}

	// Idempotency first: a replayed delivery is recognized as already-applied
	// BEFORE the no-op check — after the first application the stored delta
	// base has moved, so the same command would otherwise misread as a no-op.
	var seen string
	if err := tx.QueryRowxContext(ctx,
		`SELECT id FROM inference_adjustments WHERE idempotency_key = $1`,
		cmd.IdempotencyKey).Scan(&seen); err == nil {
		return domain.NewError(domain.CodeConflict,
			"correction: idempotency key already applied")
	} else if domain.CodeOf(mapError("correction: idempotency lookup", err)) != domain.CodeNotFound {
		return mapError("correction: idempotency lookup", err)
	}

	// 1. Lock + require settled. The delta base is the CURRENT settled
	// amount, never the immutable original charge.
	var status, accountID string
	var reserved, settled sql.NullInt64
	var apiKeyID sql.NullString
	var win5h, winW, winM sql.NullString
	if err := tx.QueryRowxContext(ctx,
		`SELECT status, billing_account_id, reserved_micros, settled_micros, api_key_id,
		        window_five_hour_id, window_weekly_id, window_monthly_id
		 FROM inference_requests WHERE id = $1 FOR UPDATE`, cmd.RequestID).
		Scan(&status, &accountID, &reserved, &settled, &apiKeyID, &win5h, &winW, &winM); err != nil {
		return mapError("correction: lock request", err)
	}
	if domain.RequestStatus(status) != domain.ReqSettled {
		return domain.NewError(domain.CodeConflict,
			"correction: request not settled ("+status+") — settle first, corrections revise a settlement")
	}
	if !settled.Valid {
		return domain.NewError(domain.CodeConflict,
			"correction: settled request without settled_micros (ledger drift — reconcile first)")
	}
	base := domain.Microcredit(settled.Int64)
	direction, delta, err := accounting.CorrectionDelta(base, cmd.CorrectedMicros)
	if err != nil {
		return err
	}

	// 2. The charge row (reversal target when voiding).
	var chargeID sql.NullInt64
	if err := tx.QueryRowxContext(ctx,
		`SELECT id FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, cmd.RequestID).
		Scan(&chargeID); err != nil {
		return mapError("correction: load charge", err)
	}

	// 3. Corrected usage revision (original preserved).
	usage := cmd.Corrected
	usage.RequestID = cmd.RequestID
	var maxRev sql.NullInt64
	if err := tx.QueryRowxContext(ctx,
		`SELECT MAX(revision) FROM inference_usage_records WHERE attempt_id = $1`,
		usage.AttemptID).Scan(&maxRev); err != nil {
		return mapError("correction: usage revision", err)
	}
	usage.Revision = 1
	if maxRev.Valid {
		usage.Revision = int(maxRev.Int64) + 1
	}
	if err := insertUsageRecord(ctx, tx, &usage); err != nil {
		return err
	}

	// 4. Compensation row (idempotency key lives here) + ledger entry:
	//    void → reversal of the current effect; otherwise delta adjustment.
	adjID := uuid.NewString()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_adjustments
		 (id, billing_account_id, request_id, reason, amount_micros, direction,
		  unit, operator_subject, service_subject, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,$6,'microcredit',$7,'inference-reconciliation',$8)`,
		adjID, accountID, cmd.RequestID, cmd.Reason, int64(delta), direction,
		cmd.OperatorSubject, cmd.IdempotencyKey); err != nil {
		return mapError("correction: adjustment", err)
	}
	if cmd.CorrectedMicros == 0 && base > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_ledger_entries
			 (billing_account_id, request_id, entry_type, amount_micros, unit,
			  reverses_entry_id, adjustment_id, created_by)
			 VALUES ($1,$2,'reversal',$3,'microcredit',$4,$5,$6)`,
			accountID, cmd.RequestID, int64(base), chargeID.Int64, adjID,
			"system:"+cmd.OperatorSubject); err != nil {
			return mapError("correction: reversal", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_ledger_entries
			 (billing_account_id, request_id, entry_type, amount_micros, unit, adjustment_id, created_by)
			 VALUES ($1,$2,'adjustment',$3,'microcredit',$4,$5)`,
			accountID, cmd.RequestID, int64(delta), adjID, "system:"+cmd.OperatorSubject); err != nil {
			return mapError("correction: adjustment entry", err)
		}
	}

	// 6. Correct windows + key budget by the signed delta, in the fixed
	// lock order; a credit larger than the aggregate refuses loudly
	// (aggregates never go negative silently).
	signed := int64(delta)
	if direction == "credit" {
		signed = -signed
	}
	for _, wID := range []sql.NullString{win5h, winW, winM} {
		if !wID.Valid {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE inference_quota_windows
			 SET used_micros = used_micros + $2
			 WHERE id = $1 AND used_micros + $2 >= 0`, wID.String, signed)
		if err != nil {
			return mapError("correction: window delta", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.NewError(domain.CodeConflict,
				"correction: window used would go negative (ledger/window drift — reconcile first)")
		}
	}
	if apiKeyID.Valid {
		res, err := tx.ExecContext(ctx,
			`UPDATE inference_api_keys
			 SET budget_used_micros = budget_used_micros + $2, updated_at = now()
			 WHERE id = $1 AND budget_used_micros + $2 >= 0`, apiKeyID.String, signed)
		if err != nil {
			return mapError("correction: key budget delta", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.NewError(domain.CodeConflict,
				"correction: key budget would go negative (ledger/budget drift — reconcile first)")
		}
	}

	// 7. Request reflects the correction.
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_requests
		 SET settled_micros = $2, usage_status = $3, updated_at = now()
		 WHERE id = $1`,
		cmd.RequestID, int64(cmd.CorrectedMicros), string(usage.Source)); err != nil {
		return mapError("correction: request", err)
	}

	// 8. Evidence landed: the request's open EVIDENCE jobs resolve — only
	// crash_recovery/unknown_usage (审查修复 Important 1): an open
	// settlement_overage anomaly is NOT evidence-resolved; blindly resolving
	// it here and re-enqueueing it in step 9 would leave a same-reason job
	// resolved and invisible while the request is still over its hold.
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_reconciliation_jobs
		 SET status = 'resolved', resolved_at = now(), updated_at = now(),
		     detail = detail || $2
		 WHERE request_id = $1 AND status IN ('pending','running')
		   AND reason IN ('crash_recovery','unknown_usage')`,
		cmd.RequestID, mustJSON(map[string]string{
			"resolution": "corrected by real evidence: " + cmd.Reason,
		})); err != nil {
		return mapError("correction: resolve job", err)
	}

	// 9. Overage re-check against the hold (不隐藏负差额). Still over →
	// enqueue/refresh the anomaly (the open job merges detail and STAYS
	// pending); back within the hold → the anomaly is addressed by this
	// correction, so an open overage job resolves with the note.
	if reserved.Valid && cmd.CorrectedMicros > domain.Microcredit(reserved.Int64) {
		detail := mustJSON(map[string]interface{}{
			"schema_version":  1,
			"reserved_micros": reserved.Int64,
			"charge_micros":   int64(cmd.CorrectedMicros),
			"over_micros":     int64(cmd.CorrectedMicros) - reserved.Int64,
			"origin":          "correction",
		})
		deadline := cmd.At.UTC().Add(24 * time.Hour)
		if cmd.At.IsZero() {
			deadline = time.Now().UTC().Add(24 * time.Hour)
		}
		if err := s.EnqueueReconciliationJobTx(ctx, w, EnqueueReconciliationCommand{
			RequestID: &cmd.RequestID, Reason: "settlement_overage",
			Detail: detail, Deadline: deadline,
		}); err != nil {
			return err
		}
	} else if reserved.Valid {
		if _, err := tx.ExecContext(ctx,
			`UPDATE inference_reconciliation_jobs
			 SET status = 'resolved', resolved_at = now(), updated_at = now(),
			     detail = detail || $2
			 WHERE request_id = $1 AND status IN ('pending','running')
			   AND reason = 'settlement_overage'`,
			cmd.RequestID, mustJSON(map[string]string{
				"resolution": "correction brought the charge within the hold: " + cmd.Reason,
			})); err != nil {
			return mapError("correction: resolve overage job", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 账本重建核对（设计 §7.3 + Task 9 验收：由账本重建聚合值可与窗口核对）
// ---------------------------------------------------------------------------

// ReconcileWindowAggregates rebuilds every ACTIVE window's used aggregate
// from the ledger (charges − reversals ± request-linked adjustments of
// requests bound to that window) and returns the comparison. Voided windows
// provably saw zero consumption and are skipped. A mismatch is NEVER
// auto-repaired — it enqueues a ledger_mismatch job (账本不重写历史).
func (s *Store) ReconcileWindowAggregates(ctx context.Context) ([]accounting.WindowReconciliation, error) {
	var rows []struct {
		WindowID   string `db:"window_id"`
		Kind       string `db:"kind"`
		StoredUsed int64  `db:"used_micros"`
		Rebuilt    int64  `db:"rebuilt"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT w.id AS window_id, w.kind, w.used_micros,
		        COALESCE(SUM(CASE
		            WHEN le.entry_type = 'charge' THEN le.amount_micros
		            WHEN le.entry_type = 'reversal' THEN -le.amount_micros
		            WHEN le.entry_type = 'adjustment' AND adj.direction = 'debit' THEN le.amount_micros
		            WHEN le.entry_type = 'adjustment' AND adj.direction = 'credit' THEN -le.amount_micros
		        END), 0) AS rebuilt
		 FROM inference_quota_windows w
		 LEFT JOIN inference_requests r
		   ON r.window_five_hour_id = w.id
		   OR r.window_weekly_id = w.id
		   OR r.window_monthly_id = w.id
		 LEFT JOIN inference_ledger_entries le
		   ON le.request_id = r.id AND le.unit = 'microcredit'
		 LEFT JOIN inference_adjustments adj ON adj.id = le.adjustment_id
		 WHERE w.state = 'active'
		 GROUP BY w.id, w.kind, w.used_micros
		 ORDER BY w.created_at`); err != nil {
		return nil, mapError("reconciliation: rebuild windows", err)
	}
	out := make([]accounting.WindowReconciliation, 0, len(rows))
	for _, r := range rows {
		out = append(out, accounting.WindowReconciliation{
			WindowID: r.WindowID, Kind: domain.WindowKind(r.Kind),
			StoredUsed: r.StoredUsed, RebuiltUsed: r.Rebuilt,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 指标（Task 9：队列积压、未知 usage、预占悬挂、结算延迟、账本差异）
// ---------------------------------------------------------------------------

// RecoveryMetrics is one snapshot of the recovery/ledger health gauges.
// The worker logs it every pass (仓库无 metrics 设施 — 结构化日志计数 +
// 后台查询视图, 控制者裁决).
type RecoveryMetrics struct {
	// ReconciliationBacklog: open (pending/running) reconciliation jobs.
	ReconciliationBacklog int64
	// UnknownUsageRequests: requests whose metering fact is not persisted
	// (usage_status='unknown' — parked) plus stale-open requests still
	// pending usage.
	UnknownUsageRequests int64
	// DanglingHolds: held reservations older than the grace cutoff.
	DanglingHolds int64
	// OpenRequests: requests in a non-terminal §7.2 state.
	OpenRequests int64
	// OldestOpenUpdatedAt feeds SettlementLag (nil when none are open).
	OldestOpenUpdatedAt *time.Time
	// EscalatedJobs: jobs past their deadline awaiting a human.
	EscalatedJobs int64
}

// RecoveryMetrics snapshots the gauges; staleCutoff bounds "dangling".
func (s *Store) RecoveryMetrics(ctx context.Context, staleCutoff time.Time) (RecoveryMetrics, error) {
	var m RecoveryMetrics
	if err := s.db.QueryRowxContext(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM inference_reconciliation_jobs
		     WHERE status IN ('pending','running')),
		   (SELECT COUNT(*) FROM inference_requests
		     WHERE usage_status = 'unknown'
		        OR (status = ANY($2) AND usage_status = 'pending')),
		   (SELECT COUNT(*) FROM inference_reservations
		     WHERE state = 'held' AND created_at < $1),
		   (SELECT COUNT(*) FROM inference_requests WHERE status = ANY($2)),
		   (SELECT MIN(updated_at) FROM inference_requests WHERE status = ANY($2)),
		   (SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE status = 'escalated')`,
		staleCutoff.UTC(), openRequestStatuses).
		Scan(&m.ReconciliationBacklog, &m.UnknownUsageRequests, &m.DanglingHolds,
			&m.OpenRequests, &m.OldestOpenUpdatedAt, &m.EscalatedJobs); err != nil {
		return RecoveryMetrics{}, mapError("recovery: metrics", err)
	}
	return m, nil
}
