package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// ledger_repo.go — 表组 inference_ledger_entries/adjustments
// (migration 026) + domain.SettlementStore 实现。
//
// 账本规则（设计 §7.3）：追加 + 冲正，不 UPDATE 已结算分录；同一请求
// 至多一条 charge（部分唯一索引），重复结算 → CodeConflict。

// Settle implements domain.SettlementStore. One transaction does all of
// (设计 §7.2):
//
//  1. lock the request row FOR UPDATE (serializes against a concurrent
//     release/settlement of the same request — 并发释放均一致),
//  2. persist the normalized usage fact,
//  3. append the customer ledger charge (UNIQUE per request → idempotent),
//  4. convert held reservations — in the fixed lock order (windows
//     five_hour→weekly→monthly, then Key budget): windows reserved→used
//     (actual charged amount), key budget corrected by (charge − hold) so
//     an over-hold is returned, reservations marked settled with a
//     state='held' guard,
//  5. move the request to settled with its usage status.
func (s *Store) Settle(ctx context.Context, w domain.UnitOfWork, cmd domain.SettleCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if cmd.ChargeMicros < 0 {
		return domain.WrapError(domain.CodeInvalidInput, "settle: negative charge", domain.ErrNegativeValue)
	}

	// 1. Lock the request (fixed order: account is never needed here; the
	// request row is what release also locks). The charge carries the
	// request's PINNED price version (read from the locked row, never
	// caller-supplied) so a later price change can never rewrite which
	// version settled this request (设计 §7.1).
	var accountID, status string
	var priceVersionID *string
	if err := tx.QueryRowxContext(ctx,
		`SELECT billing_account_id, price_version_id, status FROM inference_requests WHERE id = $1 FOR UPDATE`,
		cmd.RequestID).Scan(&accountID, &priceVersionID, &status); err != nil {
		return mapError("settle: lock request", err)
	}
	if domain.RequestStatus(status) == domain.ReqSettled || domain.RequestStatus(status) == domain.ReqReleased {
		return domain.NewError(domain.CodeConflict,
			"settle: request already finalized ("+status+")")
	}

	// 2. Usage fact (UNIQUE(attempt_id, revision)).
	if err := insertUsageRecord(ctx, tx, &cmd.Usage); err != nil {
		return err
	}

	// 3. Ledger charge. The partial unique index on (request_id) WHERE
	// entry_type='charge' makes a duplicate settlement a conflict.
	if cmd.ChargeMicros > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_ledger_entries
			 (billing_account_id, request_id, entry_type, amount_micros, unit, price_version_id)
			 VALUES ($1,$2,'charge',$3,'microcredit',$4)`,
			accountID, cmd.RequestID, int64(cmd.ChargeMicros), priceVersionID); err != nil {
			return mapError("settle: ledger charge", err)
		}
	}

	// 4. Convert held reservations → used/settled, fixed lock order.
	rows, err := loadHeldReservations(ctx, tx, cmd.RequestID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return domain.NewError(domain.CodeConflict,
			"settle: no held reservations (already released or settled?)")
	}
	for _, r := range rows {
		switch r.TargetKind {
		case string(domain.TargetKeyBudget):
			// 与窗口的 reserved→used 转换对齐：预占时已 budget_used += hold，
			// 结算校正 budget_used += (charge − hold)，实际 < 预占的差额在此
			// 归还；守卫 budget_used 不得变负。
			res, err := tx.ExecContext(ctx,
				`UPDATE inference_api_keys
				 SET budget_used_micros = budget_used_micros - $2 + $3, updated_at = now()
				 WHERE id = $1 AND budget_used_micros - $2 + $3 >= 0`,
				*r.APIKeyID, r.Amount, int64(cmd.ChargeMicros))
			if err != nil {
				return mapError("settle: key budget convert", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.WrapError(domain.CodeConflict,
					"settle: key budget correction would go negative (concurrent settlement?)", nil)
			}
		default:
			// The same consumption lands in every applicable window
			// (设计 §6: 一次消费同时增加三个适用窗口的 used). The request
			// stays bound to the windows pinned at admission — settlement
			// never rebinds to a newer period (设计 §6/Task 7).
			res, err := tx.ExecContext(ctx,
				`UPDATE inference_quota_windows
				 SET reserved_micros = reserved_micros - $2,
				     used_micros = used_micros + $3
				 WHERE id = $1 AND state = 'active' AND reserved_micros >= $2`,
				*r.WindowID, r.Amount, int64(cmd.ChargeMicros))
			if err != nil {
				return mapError("settle: window convert", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.WrapError(domain.CodeConflict,
					"settle: window reservation no longer held (concurrent settlement?)", nil)
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE inference_reservations
			 SET state = 'settled', settled_at = now()
			 WHERE id = $1 AND state = 'held'`, r.ID)
		if err != nil {
			return mapError("settle: reservation", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.NewError(domain.CodeConflict,
				"settle: reservation already transitioned (concurrent release?)")
		}
	}

	// 5. Attempt cost (reported/estimated/allocated), optional.
	if cmd.AttemptID != nil && cmd.AttemptCost != nil && cmd.CostBasis != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE inference_attempts
			 SET cost_micros = $2, cost_currency = $3, cost_basis = $4
			 WHERE id = $1`,
			*cmd.AttemptID, cmd.AttemptCost.Micros, cmd.AttemptCost.Currency, string(*cmd.CostBasis)); err != nil {
			return mapError("settle: attempt cost", err)
		}
	}

	// 5. Request terminal state.
	settled := int64(cmd.ChargeMicros)
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_requests
		 SET status = 'settled', settled_micros = $2, usage_status = $3,
		     completed_at = $4, updated_at = now()
		 WHERE id = $1`,
		cmd.RequestID, settled, string(cmd.Usage.Source), cmd.SettledAt); err != nil {
		return mapError("settle: request", err)
	}
	return nil
}

// MarkReconciliationRequired implements domain.SettlementStore: parks the
// request in the reconciliation queue with a recovery deadline (设计 §7.2:
// 恢复时限、告警；禁止仅凭 TTL 释放全部预占).
func (s *Store) MarkReconciliationRequired(ctx context.Context, w domain.UnitOfWork, requestID string, reason string, deadline time.Time) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if err := s.updateRequestStatus(ctx, tx, requestID, domain.ReqReconciliationRequired, reason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_reconciliation_jobs (id, request_id, reason, deadline_at)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (request_id) DO UPDATE
		 SET status = 'pending', deadline_at = EXCLUDED.deadline_at, updated_at = now()`,
		uuid.NewString(), requestID, reason, deadline); err != nil {
		return mapError("reconciliation: enqueue", err)
	}
	return nil
}

// AppendAdjustment writes an operator compensation and its ledger entry in
// one transaction (设计 §9.2: 有原因、对象、金额/额度、操作者和幂等键).
func (s *Store) AppendAdjustment(ctx context.Context, w domain.UnitOfWork, adj Adjustment) (*Adjustment, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if adj.ID == "" {
		adj.ID = uuid.NewString()
	}
	unit := adj.Unit
	if unit == "" {
		unit = "microcredit"
	}
	var currency interface{}
	if adj.Currency != "" {
		currency = adj.Currency
	}
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_adjustments
		 (id, billing_account_id, request_id, reason, amount_micros, direction,
		  unit, currency, operator_subject, service_subject, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING created_at`,
		adj.ID, adj.BillingAccountID, strPtr(adj.RequestID), adj.Reason, adj.AmountMicros, adj.Direction,
		unit, currency, adj.OperatorSubject, adj.ServiceSubject, adj.IdempotencyKey).
		Scan(&adj.CreatedAt)
	if err != nil {
		return nil, mapError("adjustment: insert", err)
	}
	// The matching ledger entry (adjustment type); charge uniqueness does
	// not apply to adjustments.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, unit, currency,
		  adjustment_id, created_by)
		 VALUES ($1,$2,'adjustment',$3,$4,$5,$6,$7)`,
		adj.BillingAccountID, adj.RequestID, adj.AmountMicros, unit, currency,
		adj.ID, "operator:"+adj.OperatorSubject); err != nil {
		return nil, mapError("adjustment: ledger", err)
	}
	adj.Unit = unit
	return &adj, nil
}

// Adjustment is the operator compensation shape (骨架; Task 15 扩展).
type Adjustment struct {
	ID               string
	BillingAccountID string
	RequestID        *string
	Reason           string
	AmountMicros     int64
	Direction        string // credit | debit
	Unit             string // microcredit | micromoney
	Currency         string
	OperatorSubject  string
	ServiceSubject   string
	IdempotencyKey   string
	CreatedAt        time.Time
}
