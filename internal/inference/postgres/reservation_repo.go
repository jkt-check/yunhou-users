package postgres

import (
	"context"
	"database/sql"
	"sort"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

// reservation_repo.go — inference_reservations (migration 026) 的释放路径
// 与五小时窗口安全撤销（Task 7，设计 §6/§7.2）。
//
// 固定锁序与 quota_repo.go 一致：账户 → 请求 → 窗口（five_hour→weekly→
// monthly）→ Key 预算 → 预占行；所有预占状态迁移带 state='held' 守卫，
// 重复释放/与并发结算竞态 → CodeConflict（并发释放均一致，任务书验收）。

// heldReservationRow is one held reservation awaiting release/settlement.
type heldReservationRow struct {
	ID         string  `db:"id"`
	TargetKind string  `db:"target_kind"`
	WindowID   *string `db:"window_id"`
	APIKeyID   *string `db:"api_key_id"`
	Amount     int64   `db:"amount_micros"`
}

// loadHeldReservations reads the request's held reservations sorted into
// the fixed lock order (windows before key budget).
func loadHeldReservations(ctx context.Context, tx *sqlx.Tx, requestID string) ([]heldReservationRow, error) {
	var rows []heldReservationRow
	if err := tx.SelectContext(ctx, &rows,
		`SELECT id, target_kind, window_id, api_key_id, amount_micros
		 FROM inference_reservations
		 WHERE request_id = $1 AND state = 'held'`, requestID); err != nil {
		return nil, mapError("load held reservations", err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return windowOrderKey(domain.ReservationTargetKind(rows[i].TargetKind)) <
			windowOrderKey(domain.ReservationTargetKind(rows[j].TargetKind))
	})
	return rows, nil
}

// reservationIdentity is the admission fact ONE held reservation must
// match: the target kind PLUS the concrete target the admission bound it
// to (a window id, a Key id; the wallet freeze carries neither).
//
// The kind alone is not enough. A held row of the right kind but bound to a
// DIFFERENT window is not the hold the admission created: settling it books
// the consumption into the wrong window, releasing it returns another
// request's hold — and the request still reaches a terminal state.
type reservationIdentity struct {
	kind     domain.ReservationTargetKind
	windowID string
	apiKeyID string
}

// expectedReservationIdentities returns the identities the held reservations
// of a request must match — one per monetary target the admission created.
// It is derived from the SAME source of truth as the count check
// (domain.ExpectedMonetaryTargets), so "how many targets" and "which
// targets" can never disagree.
func expectedReservationIdentities(req *domain.Request) []reservationIdentity {
	if req == nil {
		return nil
	}
	kinds := domain.ExpectedMonetaryTargets(req)
	out := make([]reservationIdentity, 0, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case domain.TargetWindowFiveHour:
			out = append(out, reservationIdentity{kind: kind, windowID: idOrEmpty(req.WindowFiveHourID)})
		case domain.TargetWindowWeekly:
			out = append(out, reservationIdentity{kind: kind, windowID: idOrEmpty(req.WindowWeeklyID)})
		case domain.TargetWindowMonthly:
			out = append(out, reservationIdentity{kind: kind, windowID: idOrEmpty(req.WindowMonthlyID)})
		case domain.TargetKeyBudget:
			out = append(out, reservationIdentity{kind: kind, apiKeyID: idOrEmpty(req.APIKeyID)})
		case domain.TargetWallet:
			// 钱包冻结不挂窗口/Key：目标种类一致即身份一致。
			out = append(out, reservationIdentity{kind: kind})
		}
	}
	return out
}

// checkReservationIdentities verifies that every held reservation IS the
// hold the admission created: the same target kind AND the same target
// identity (the pinned window id / the pinned Key id).
//
// A count-only comparison cannot see a mis-bound hold — the same number of
// rows bound to the WRONG window settles or releases money in the wrong
// place while the request still reaches a terminal state. A wrong kind or a
// wrong id is therefore a typed reservation_drift: the caller must be able
// to tell it apart from a benign race (an ordinary conflict would be
// skipped as one).
//
// Settle and Release share this one helper so the two paths can never drift
// apart in what they accept as "the admission's hold".
func checkReservationIdentities(op string, rows []heldReservationRow, req *domain.Request,
	status domain.RequestStatus, charge domain.Microcredit) error {
	expected := expectedReservationIdentities(req)
	for _, r := range rows {
		kind := domain.ReservationTargetKind(r.TargetKind)
		want, ok := lookupReservationIdentity(expected, kind)
		if !ok {
			return driftOutcome(op,
				"a held reservation targets "+string(kind)+", which this request was never admitted with (reservation drift)",
				status, charge)
		}
		switch kind {
		case domain.TargetKeyBudget:
			if r.APIKeyID == nil || *r.APIKeyID != want.apiKeyID {
				return driftOutcome(op,
					"the held Key budget reservation is bound to another Key (reservation drift)",
					status, charge)
			}
		case domain.TargetWallet:
			// 钱包冻结不挂窗口/Key：种类一致即身份一致。
		default:
			if r.WindowID == nil || *r.WindowID != want.windowID {
				return driftOutcome(op,
					"the held "+string(kind)+" reservation is bound to another window (reservation drift)",
					status, charge)
			}
		}
	}
	return nil
}

// lookupReservationIdentity finds the admission identity of one target kind.
func lookupReservationIdentity(expected []reservationIdentity, kind domain.ReservationTargetKind) (reservationIdentity, bool) {
	for _, e := range expected {
		if e.kind == kind {
			return e, true
		}
	}
	return reservationIdentity{}, false
}

// idOrEmpty dereferences an optional id ("" = absent).
func idOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Release implements domain.QuotaStore: every held reservation of a
// request whose upstream consumption is confirmed zero is released inside
// the shared tx and the request moves to 'released' (设计 §7.2 reserved →
// released).
//
// Task 7 additions:
//
//   - the transaction locks in the fixed order: account → request
//     (FOR UPDATE) → windows → key budget, so a release serializes with
//     concurrent admissions on the account anchor and with concurrent
//     settlement on the request row;
//   - a request already in a terminal state (settled/released) or parked
//     in reconciliation REFUSES to release — reconciliation keeps its
//     reservation (设计 §7.2: 禁止仅凭 TTL/未知状态释放全部预占);
//   - after the last hold of a five-hour window is released AND the window
//     has zero consumption AND no other valid reservation remains, the
//     unused window is voided in-transaction (设计 §6: “全部请求确认无消费
//     且无其他有效预占”时可在事务内安全撤销未使用窗口). The account lock
//     held by this transaction makes the check-and-void atomic against
//     concurrent admissions, which activate windows under the same lock.
func (s *Store) Release(ctx context.Context, w domain.UnitOfWork, requestID string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return s.releaseLocked(ctx, tx, w, requestID, false)
}

// ReleaseFromReconciliation releases a PARKED (reconciliation_required)
// request — only the recovery worker calls it, and only with PROOF of zero
// upstream consumption (every attempt failed pre-execution; Task 9 分类
// "未发送/全尝试确认零消费"). This is evidence-driven, never TTL-driven:
// the guard that keeps Release away from unknown-state requests stays
// intact. The request's open reconciliation job resolves in the same
// transaction (无"已释放但任务悬挂"窗口).
func (s *Store) ReleaseFromReconciliation(ctx context.Context, w domain.UnitOfWork, requestID string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return s.releaseLocked(ctx, tx, w, requestID, true)
}

// releaseLocked is the shared release core. fromReconciliation admits the
// reconciliation_required state (evidence-driven recovery release) and
// resolves the open job in the same tx.
func (s *Store) releaseLocked(ctx context.Context, tx *sqlx.Tx, w domain.UnitOfWork, requestID string, fromReconciliation bool) error {
	// Locate the owner account, then lock in the fixed order.
	var accountID, status, chargeSource string
	var apiKeyID, win5h, winW, winM sql.NullString
	if err := tx.QueryRowxContext(ctx,
		`SELECT billing_account_id, status, charge_source, api_key_id,
		        window_five_hour_id, window_weekly_id, window_monthly_id
		 FROM inference_requests WHERE id = $1`,
		requestID).Scan(&accountID, &status, &chargeSource, &apiKeyID, &win5h, &winW, &winM); err != nil {
		return mapError("release: load request", err)
	}
	if err := lockAccountTx(ctx, tx, accountID); err != nil {
		return err
	}
	// Re-read the request under the account lock and take the request row
	// lock; a terminal or reconciling request must not be released — unless
	// the caller is the evidence-driven recovery path.
	if err := tx.QueryRowxContext(ctx,
		`SELECT status FROM inference_requests WHERE id = $1 FOR UPDATE`,
		requestID).Scan(&status); err != nil {
		return mapError("release: lock request", err)
	}
	switch domain.RequestStatus(status) {
	case domain.ReqSettled, domain.ReqReleased:
		return domain.NewError(domain.CodeConflict,
			"release: request already finalized ("+status+")")
	case domain.ReqReconciliationRequired:
		if !fromReconciliation {
			return domain.NewError(domain.CodeConflict,
				"release: request in reconciliation (禁止仅凭 TTL/未知状态释放)")
		}
	}

	rows, err := loadHeldReservations(ctx, tx, requestID)
	if err != nil {
		return err
	}
	// A request whose policy has NO monetary window holds nothing at all —
	// releasing it is a no-op that still finalizes the request (confirmed
	// zero consumption). A request that DOES expect holds but cannot show
	// them is an accounting anomaly: the window's reserved amount would
	// stay frozen forever, so the release refuses loudly instead of
	// pretending the holds are gone.
	req := &domain.Request{
		APIKeyID:         strFromNull(apiKeyID),
		WindowFiveHourID: strFromNull(win5h), WindowWeeklyID: strFromNull(winW),
		WindowMonthlyID: strFromNull(winM),
		ChargeSource:    domain.ChargeSource(chargeSource),
	}
	expected := len(domain.ExpectedMonetaryTargets(req))
	switch {
	case len(rows) < expected:
		// 准入应当存在的金额预占不见了 = 账务异常：必须返回**可识别**的
		// typed 结果，恢复 worker 才能把它与"良性竞态"分开（响亮地停放/
		// 核对），而不是当成竞态静默跳过、每轮重复盲放。
		return &domain.SettleError{
			Kind: domain.SettleOutcomeMissingReservation, Code: domain.CodeConflict,
			Message: "release: the expected monetary reservation is missing (accounting anomaly)",
		}
	case len(rows) > expected:
		return &domain.SettleError{
			Kind: domain.SettleOutcomeReservationDrift, Code: domain.CodeConflict,
			Message: "release: more held reservations than the admission created (reservation drift)",
		}
	}
	// 数量对得上还不够：每条 held 预占必须是准入创建的那**一笔**（目标身份
	// 必须与请求行钉住的一致）。错绑的预占绝不能被释放 —— 那会归还错误的
	// 窗口（另一个请求的预占凭空消失），而本请求的五小时预占永远挂着。
	if err := checkReservationIdentities("release", rows, req,
		domain.RequestStatus(status), 0); err != nil {
		return err
	}
	if req.ChargeSource == domain.ChargeSourceWallet {
		if err := checkWalletFreeze(ctx, tx, "release wallet", requestID, domain.RequestStatus(status), 0); err != nil {
			return err
		}
	}

	var fiveHourWindow *string
	for _, r := range rows {
		switch r.TargetKind {
		case string(domain.TargetKeyBudget):
			res, err := tx.ExecContext(ctx,
				`UPDATE inference_api_keys
				 SET budget_used_micros = budget_used_micros - $2, updated_at = now()
				 WHERE id = $1 AND budget_used_micros >= $2`, *r.APIKeyID, r.Amount)
			if err != nil {
				return mapError("release: key budget", err)
			}
			// 与下方 window 分支同一守卫（评审轮1 Important-2）：预算已不
			// 足以归还这笔预占 = 并发结算/账本漂移，必须响亮冲突，否则
			// reservation 被标记 released 而 budget 保持膨胀（静默泄漏）。
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.NewError(domain.CodeConflict,
					"release: key budget no longer covers the hold (concurrent settlement?)")
			}
		case string(domain.TargetWallet):
			// The freeze was validated before any writes; retain the
			// guarded transition as a final concurrency backstop.
			if err := s.releaseWalletLocked(ctx, tx, requestID); err != nil {
				return err
			}
		default:
			res, err := tx.ExecContext(ctx,
				`UPDATE inference_quota_windows
				 SET reserved_micros = reserved_micros - $2
				 WHERE id = $1 AND reserved_micros >= $2`, *r.WindowID, r.Amount)
			if err != nil {
				return mapError("release: window", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.NewError(domain.CodeConflict,
					"release: window reservation no longer held (concurrent settlement?)")
			}
			if r.TargetKind == string(domain.TargetWindowFiveHour) {
				wid := *r.WindowID
				fiveHourWindow = &wid
			}
		}
		// Guarded transition: only a still-held reservation may release —
		// a concurrent settle/release of the same row conflicts.
		res, err := tx.ExecContext(ctx,
			`UPDATE inference_reservations
			 SET state = 'released', released_at = now()
			 WHERE id = $1 AND state = 'held'`, r.ID)
		if err != nil {
			return mapError("release: reservation", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.NewError(domain.CodeConflict,
				"release: reservation already transitioned (concurrent settlement?)")
		}
	}

	if fiveHourWindow != nil {
		if err := voidUnusedFiveHourWindow(ctx, tx, *fiveHourWindow); err != nil {
			return err
		}
	}
	// Rate reservation (Task 4): the TPM hold of a confirmed-zero-consumption
	// request is released. The RPM count is NOT touched — an admitted
	// request counts once even when the upstream refused it.
	if err := s.releaseRateTx(ctx, tx, requestID); err != nil {
		return err
	}
	if fromReconciliation {
		// Evidence-driven release resolves the open job atomically.
		if _, err := tx.ExecContext(ctx,
			`UPDATE inference_reconciliation_jobs
			 SET status = 'resolved', resolved_at = now(), updated_at = now(),
			     detail = detail || '{"schema_version":1,"resolution":"zero consumption proven by attempt evidence"}'::jsonb
			 WHERE request_id = $1 AND status IN ('pending','running')`, requestID); err != nil {
			return mapError("release: resolve reconciliation job", err)
		}
	}
	if err := s.updateRequestStatus(ctx, tx, requestID, domain.ReqReleased, ""); err != nil {
		return err
	}
	return nil
}

// voidUnusedFiveHourWindow revokes a five-hour window that provably saw no
// consumption: used_micros = 0 (no settled charge landed), reserved_micros
// = 0 and no other state='held' reservation references it (没有其他有效预
// 占). The caller's transaction holds the account anchor lock, so no
// concurrent admission can be mid-activation on this entitlement. The row
// is kept (state='voided') for audit; it no longer participates in the
// no-overlap EXCLUDE constraint, so the next consumption opens a fresh
// window (设计 §6).
func voidUnusedFiveHourWindow(ctx context.Context, tx *sqlx.Tx, windowID string) error {
	var used, reserved int64
	var state string
	if err := tx.QueryRowxContext(ctx,
		`SELECT used_micros, reserved_micros, state
		 FROM inference_quota_windows WHERE id = $1`, windowID).
		Scan(&used, &reserved, &state); err != nil {
		return mapError("void five-hour window: read", err)
	}
	if state != "active" || used != 0 || reserved != 0 {
		return nil // consumed or still reserved elsewhere: keep the window
	}
	var held int
	if err := tx.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM inference_reservations
		 WHERE window_id = $1 AND state = 'held'`, windowID).Scan(&held); err != nil {
		return mapError("void five-hour window: count held", err)
	}
	if held != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_quota_windows SET state = 'voided'
		 WHERE id = $1 AND state = 'active' AND used_micros = 0 AND reserved_micros = 0`,
		windowID); err != nil {
		return mapError("void five-hour window", err)
	}
	return nil
}

// LoadReservations reads all reservations of a request (any state), newest
// first — the audit surface for "重复预占/并发释放均一致" verification.
func (s *Store) LoadReservations(ctx context.Context, requestID string) ([]domain.Reservation, error) {
	var rows []struct {
		ID         string  `db:"id"`
		TargetKind string  `db:"target_kind"`
		WindowID   *string `db:"window_id"`
		APIKeyID   *string `db:"api_key_id"`
		Amount     int64   `db:"amount_micros"`
		State      string  `db:"state"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id, target_kind, window_id, api_key_id, amount_micros, state
		 FROM inference_reservations WHERE request_id = $1 ORDER BY target_kind`, requestID); err != nil {
		return nil, mapError("load reservations", err)
	}
	out := make([]domain.Reservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Reservation{
			ID: r.ID, RequestID: requestID,
			TargetKind: domain.ReservationTargetKind(r.TargetKind),
			WindowID:   r.WindowID, APIKeyID: r.APIKeyID,
			Amount: domain.Microcredit(r.Amount), State: domain.ReservationState(r.State),
		})
	}
	return out, nil
}
