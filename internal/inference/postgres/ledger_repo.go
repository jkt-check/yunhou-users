package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/domain"
)

// ledger_repo.go — 表组 inference_ledger_entries/adjustments
// (migration 026 + 031) + domain.SettlementStore 实现。
//
// 账本规则（设计 §7.3）：追加 + 冲正，不 UPDATE 已结算分录；同一请求
// 至多一条 charge（部分唯一索引），重复结算 → CodeConflict。Task 9 起零
// 消费结算也落 charge 行（amount=0，migration 031 放宽 CHECK）：唯一键
// 统一保护全部结算投递，"已结算(0)"与"未结算"在账本层可区分。

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
	var chargeSource string
	if err := tx.QueryRowxContext(ctx,
		`SELECT billing_account_id, price_version_id, status, charge_source
		 FROM inference_requests WHERE id = $1 FOR UPDATE`,
		cmd.RequestID).Scan(&accountID, &priceVersionID, &status, &chargeSource); err != nil {
		return mapError("settle: lock request", err)
	}
	if domain.RequestStatus(status) == domain.ReqSettled || domain.RequestStatus(status) == domain.ReqReleased {
		return domain.NewError(domain.CodeConflict,
			"settle: request already finalized ("+status+")")
	}

	// 钱包路径（Task 14）：micromoney 结算 + 钱包冻结转消费，全程同一事务。
	if domain.ChargeSource(chargeSource) == domain.ChargeSourceWallet {
		return s.settleWallet(ctx, tx, cmd, accountID, priceVersionID)
	}

	// 2. Usage fact (UNIQUE(attempt_id, revision)).
	if err := insertUsageRecord(ctx, tx, &cmd.Usage); err != nil {
		return err
	}

	// 3. Ledger charge — ALWAYS appended, including a zero charge (Task 9 /
	// migration 031): the partial unique index on (request_id) WHERE
	// entry_type='charge' then guards EVERY settlement delivery uniformly
	// (duplicate → conflict), and "已结算(0)" is distinguishable from
	// "未结算" at the ledger level — a zero row never affects window sums.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, unit, price_version_id)
		 VALUES ($1,$2,'charge',$3,'microcredit',$4)`,
		accountID, cmd.RequestID, int64(cmd.ChargeMicros), priceVersionID); err != nil {
		return mapError("settle: ledger charge", err)
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

// settleWallet is the charge_source='wallet' settlement (Task 14): same
// one-transaction shape as the plan path — usage fact + ledger charge
// (micromoney + currency, pinned price version) + wallet hold → consume
// entries + reservation conversion + request terminal state. The request
// row lock is already held by the caller.
//
// Crash recovery calls this with cmd.WalletCharge == nil: the conservative
// charge then equals the RESERVED hold (cmd.ChargeMicros) in the wallet's
// own currency (Task 9 口径: 未知费用 → 按预占额保守入账，可冲正).
func (s *Store) settleWallet(ctx context.Context, tx *sqlx.Tx, cmd domain.SettleCommand, accountID string, priceVersionID *string) error {
	if cmd.ChargeMicros < 0 {
		return domain.WrapError(domain.CodeInvalidInput, "settle wallet: negative charge", domain.ErrNegativeValue)
	}

	// 2. Usage fact (UNIQUE(attempt_id, revision)).
	if err := insertUsageRecord(ctx, tx, &cmd.Usage); err != nil {
		return err
	}

	// Resolve the charge money: priced by the gateway under the pinned
	// sale_money revision; recovery re-supplies the reserved hold.
	charge := cmd.WalletCharge
	if charge == nil {
		var currency string
		if err := tx.QueryRowxContext(ctx,
			`SELECT w.currency FROM inference_wallet_holds h
			 JOIN inference_wallets w ON w.id = h.wallet_id
			 WHERE h.request_id = $1`, cmd.RequestID).Scan(&currency); err != nil {
			return mapError("settle wallet: currency", err)
		}
		m, err := domain.NewMoney(int64(cmd.ChargeMicros), currency)
		if err != nil {
			return err
		}
		charge = &m
	}
	if charge.Micros != int64(cmd.ChargeMicros) {
		return domain.NewError(domain.CodeInvalidInput,
			"settle wallet: wallet charge and charge_micros disagree (调用方必须同源)")
	}

	// 3. Ledger charge — micromoney, pinned price version, per-request
	// unique key (恒落，含零额：与 plan 路径同一可区分口径).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, unit, currency, price_version_id)
		 VALUES ($1,$2,'charge',$3,'micromoney',$4,$5)`,
		accountID, cmd.RequestID, charge.Micros, charge.Currency, priceVersionID); err != nil {
		return mapError("settle wallet: ledger charge", err)
	}

	// 4. Wallet hold → consume entries (bonus-first; over-hold debits cash
	// honestly).
	if err := s.settleWalletLocked(ctx, tx, cmd.RequestID, *charge); err != nil {
		return err
	}

	// 5. Flip the mirrored wallet reservation (guarded, fixed order is
	// trivial here — the wallet hold is the only target).
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_reservations
		 SET state = 'settled', settled_at = now()
		 WHERE request_id = $1 AND target_kind = 'wallet' AND state = 'held'`, cmd.RequestID)
	if err != nil {
		return mapError("settle wallet: reservation", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"settle wallet: reservation already transitioned (concurrent release?)")
	}

	// 6. Attempt cost (same optional block as the plan path).
	if cmd.AttemptID != nil && cmd.AttemptCost != nil && cmd.CostBasis != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE inference_attempts
			 SET cost_micros = $2, cost_currency = $3, cost_basis = $4
			 WHERE id = $1`,
			*cmd.AttemptID, cmd.AttemptCost.Micros, cmd.AttemptCost.Currency, string(*cmd.CostBasis)); err != nil {
			return mapError("settle wallet: attempt cost", err)
		}
	}

	// 7. Request terminal state; settled_micros carries money micros
	// (charge_source='wallet' disambiguates the unit).
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_requests
		 SET status = 'settled', settled_micros = $2, usage_status = $3,
		     completed_at = $4, updated_at = now()
		 WHERE id = $1`,
		cmd.RequestID, charge.Micros, string(cmd.Usage.Source), cmd.SettledAt); err != nil {
		return mapError("settle wallet: request", err)
	}
	return nil
}

// MarkReconciliationRequired implements domain.SettlementStore: parks the
// request in the reconciliation queue with a recovery deadline (设计 §7.2:
// 恢复时限、告警；禁止仅凭 TTL 释放全部预占). Task 9: parking also marks
// usage_status='unknown' — a parked request has NO persisted metering fact
// (usage records only ever commit inside Settle), so unknown is the honest
// ledger-level state until recovery settles or corrects it.
//
// 评审轮1 I2 终态守卫：mark 与并发成功 settle 交错时绝不覆盖终态——UPDATE
// 带 status NOT IN ('settled','released') 谓词，0 行且请求存在 = 已被并发
// 终态化，静默收敛（与 Release 的对称守卫同口径）：不覆盖 status、不把
// usage_status 改回 unknown、也不入队任务。任务 upsert 复用
// EnqueueReconciliationJobTx 的 CASE 语义：resolved/escalated 的任务不被
// 同理由投递无条件重开，只有新理由（新证据）才重开为 pending。
func (s *Store) MarkReconciliationRequired(ctx context.Context, w domain.UnitOfWork, requestID string, reason string, deadline time.Time) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_requests
		 SET status = $2, usage_status = 'unknown', last_error = $3, updated_at = now()
		 WHERE id = $1 AND status NOT IN ('settled','released')`,
		requestID, string(domain.ReqReconciliationRequired), reason)
	if err != nil {
		return mapError("mark reconciliation", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已被并发终态化（settle/release 先提交）→ 静默收敛；请求不存在才
		// 是调用方 bug，按 NotFound 报错。
		var exists bool
		if err := tx.QueryRowxContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM inference_requests WHERE id = $1)`, requestID).
			Scan(&exists); err != nil {
			return mapError("mark reconciliation", err)
		}
		if !exists {
			return mapError("mark reconciliation", sql.ErrNoRows)
		}
		return nil
	}
	return s.EnqueueReconciliationJobTx(ctx, w, EnqueueReconciliationCommand{
		RequestID: &requestID, Reason: reason, Deadline: deadline,
	})
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
// Source 是钱包调整（unit='micromoney'）的资金路由来源（cash|bonus，
// migration 036 起持久化）；额度调整（unit='microcredit'）无现金/赠送
// 概念，Source 为空。
type Adjustment struct {
	ID               string
	BillingAccountID string
	RequestID        *string
	Reason           string
	AmountMicros     int64
	Direction        string // credit | debit
	Unit             string // microcredit | micromoney
	Currency         string
	Source           string // cash | bonus（仅 micromoney 调整；036 回填自配对钱包分录）
	OperatorSubject  string
	ServiceSubject   string
	IdempotencyKey   string
	CreatedAt        time.Time
}
