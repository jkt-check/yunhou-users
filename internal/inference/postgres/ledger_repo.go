package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
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
//     release/settlement of the same request — 并发释放均一致) and
//     CLASSIFY an already-terminal request (see below),
//  2. persist the normalized usage fact,
//  3. append the customer ledger charge (UNIQUE per request → idempotent),
//  4. convert held reservations — in the fixed lock order (windows
//     five_hour→weekly→monthly, then Key budget): windows reserved→used
//     (actual charged amount), key budget corrected by (charge − hold) so
//     an over-hold is returned, reservations marked settled with a
//     state='held' guard,
//  5. move the request to settled with its usage status.
//
// 已终态的投递必须**分类**，绝不一律当成功（设计 §7.2）：只有"已 settled 且
// 账本上存在唯一且一致的 charge/usage"才是幂等重复成功；released 之后才到的
// 非零用量、缺预占、金额不一致都是账务异常 —— 返回 typed 的
// domain.SettleError，由调用方告警并送进专门核对流程。
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
	var accountID, status, chargeSource string
	var priceVersionID *string
	var apiKeyID, win5h, winW, winM sql.NullString
	if err := tx.QueryRowxContext(ctx,
		`SELECT billing_account_id, price_version_id, status, charge_source,
		        api_key_id, window_five_hour_id, window_weekly_id, window_monthly_id
		 FROM inference_requests WHERE id = $1 FOR UPDATE`,
		cmd.RequestID).Scan(&accountID, &priceVersionID, &status, &chargeSource,
		&apiKeyID, &win5h, &winW, &winM); err != nil {
		return mapError("settle: lock request", err)
	}
	reqStatus := domain.RequestStatus(status)
	req := &domain.Request{
		ID: cmd.RequestID, APIKeyID: strFromNull(apiKeyID),
		WindowFiveHourID: strFromNull(win5h), WindowWeeklyID: strFromNull(winW),
		WindowMonthlyID: strFromNull(winM), ChargeSource: domain.ChargeSource(chargeSource),
	}
	// 已终态：分类（绝不一律当成功）。
	switch reqStatus {
	case domain.ReqSettled:
		return classifySettledDelivery(ctx, tx, cmd, reqStatus)
	case domain.ReqReleased:
		return &domain.SettleError{
			Kind: domain.SettleOutcomeReleased, Code: domain.CodeConflict,
			Message:               "settle: request already released — a late delivery must be reconciled, not dropped",
			Status:                reqStatus,
			Trivial:               cmd.ChargeMicros == 0 && usageBucketsEmpty(cmd.Usage.Buckets),
			RequestedChargeMicros: cmd.ChargeMicros,
		}
	}

	// 钱包路径（Task 14）：micromoney 结算 + 钱包冻结转消费，全程同一事务。
	if domain.ChargeSource(chargeSource) == domain.ChargeSourceWallet {
		return s.settleWallet(ctx, tx, cmd, accountID, priceVersionID, reqStatus)
	}

	// 2. The monetary holds the admission created. A request whose policy
	// has NO monetary window legitimately holds NOTHING (no window hold, no
	// Key budget hold) — it still settles, with a usage fact, a ledger
	// charge and a terminal state. A request that DOES expect holds but
	// cannot show them is an accounting anomaly (缺预占): never settle it,
	// never let it pass as a duplicate.
	rows, err := loadHeldReservations(ctx, tx, cmd.RequestID)
	if err != nil {
		return err
	}
	if err := reservationOutcome("settle", len(rows),
		len(domain.ExpectedMonetaryTargets(req)), reqStatus, cmd.ChargeMicros); err != nil {
		return err
	}
	// 数量对得上还不够：每条 held 预占必须是准入创建的那**一笔**（目标身份
	// 必须与请求行钉住的一致）。错绑的预占绝不能被结算 —— 那会把消费记进
	// 错误的窗口，而请求照常终态化（账被记到了不属于它的窗口）。
	if err := checkReservationIdentities("settle", rows, req, reqStatus, cmd.ChargeMicros); err != nil {
		return err
	}

	// 3. Usage fact (UNIQUE(attempt_id, revision)).
	if err := insertUsageRecord(ctx, tx, &cmd.Usage); err != nil {
		return err
	}

	// 3b. Rate reservation (Task 4): the TPM hold becomes the ACTUAL
	// normalized token consumption (an over-hold is counted in full — never
	// clamped to the hold). The RPM count is untouched: the request was
	// admitted and counts once.
	if err := s.settleRateTx(ctx, tx, cmd.RequestID, access.NormalizedTokens(cmd.Usage.Buckets)); err != nil {
		return err
	}

	// 4. Ledger charge — ALWAYS appended, including a zero charge (Task 9 /
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

	// 5. Convert held reservations → used/settled, fixed lock order.
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

// reservationOutcome classifies the difference between the held
// reservations of a request and the monetary targets its admission created:
//
//   - fewer held than expected → missing_reservation: the monetary hold the
//     admission created is gone — an accounting anomaly the caller must be
//     able to RECOGNIZE (设计 §7.2: 本应存在的金额预占被删除必须可识别);
//   - more held than expected → reservation_drift.
//
// The comparison is on the loaded reservations only: a storage failure is
// never mapped to either outcome — it stays an error.
func reservationOutcome(op string, held, expected int, status domain.RequestStatus, charge domain.Microcredit) error {
	switch {
	case held < expected:
		return &domain.SettleError{
			Kind: domain.SettleOutcomeMissingReservation, Code: domain.CodeConflict,
			Message: op + ": the expected monetary reservation is missing",
			Status:  status, RequestedChargeMicros: charge,
		}
	case held > expected:
		return &domain.SettleError{
			Kind: domain.SettleOutcomeReservationDrift, Code: domain.CodeConflict,
			Message: op + ": more held reservations than the admission created (reservation drift)",
			Status:  status, RequestedChargeMicros: charge,
		}
	}
	return nil
}

// missingReservationOutcome builds the typed result for "the monetary hold
// this request was admitted with is gone" — a recognizable accounting
// anomaly, never an ordinary not-found/conflict the caller would swallow.
func missingReservationOutcome(op, message string, status domain.RequestStatus, charge domain.Microcredit) error {
	return &domain.SettleError{
		Kind: domain.SettleOutcomeMissingReservation, Code: domain.CodeConflict,
		Message: op + ": " + message, Status: status, RequestedChargeMicros: charge,
	}
}

// driftOutcome builds the typed result for "the held reservations do not
// match what the admission created".
func driftOutcome(op, message string, status domain.RequestStatus, charge domain.Microcredit) error {
	return &domain.SettleError{
		Kind: domain.SettleOutcomeReservationDrift, Code: domain.CodeConflict,
		Message: op + ": " + message, Status: status, RequestedChargeMicros: charge,
	}
}

// checkWalletHolds verifies the wallet request's monetary hold BEFORE any
// usage/rate/ledger write: the wallet admission created exactly ONE
// monetary reservation (the mirrored wallet reservation) plus the freeze row
// it mirrors.
//
//   - the mirrored reservation is gone → missing_reservation;
//   - more than one held reservation → reservation_drift;
//   - the held reservation is not the wallet hold → reservation_drift;
//   - the freeze row is gone → missing_reservation (the money the request
//     was admitted with is gone);
//   - the freeze row exists but is NOT held → reservation_drift (the row is
//     there, but the money is no longer frozen: the hold drifted, and an
//     ordinary conflict would be indistinguishable from a benign race).
//
// A storage failure is never mapped to either outcome: it stays an error.
func checkWalletHolds(ctx context.Context, tx *sqlx.Tx, requestID string, status domain.RequestStatus, charge domain.Microcredit) error {
	rows, err := loadHeldReservations(ctx, tx, requestID)
	if err != nil {
		return err
	}
	walletReq := &domain.Request{ChargeSource: domain.ChargeSourceWallet}
	expected := len(domain.ExpectedMonetaryTargets(walletReq))
	if err := reservationOutcome("settle wallet", len(rows), expected, status, charge); err != nil {
		return err
	}
	if err := checkReservationIdentities("settle wallet", rows, walletReq, status, charge); err != nil {
		return err
	}
	return checkWalletFreeze(ctx, tx, "settle wallet", requestID, status, charge)
}

// classifySettledDelivery classifies a settlement delivery for a request
// that is ALREADY settled:
//
//   - the ledger holds exactly one charge and it matches the delivered
//     amount (and the metering fact of a re-delivered attempt matches) →
//     an idempotent replay (重复成功): the caller absorbs it silently;
//   - anything else (no charge row, a different amount, a contradicting
//     usage fact) → an accounting anomaly (double-billing risk): the
//     caller must alarm and reconcile.
//
// A **storage failure** while reading the recorded charge/usage is neither:
// it is propagated to the caller. A delivery whose consistency could not be
// checked is never an idempotent replay (设计 §7.2: 只有一致的 charge/usage
// 才算幂等成功；存储错误必须响亮).
//
// The request row is locked by the caller, so the recorded charge cannot
// change under this transaction.
func classifySettledDelivery(ctx context.Context, tx *sqlx.Tx, cmd domain.SettleCommand, status domain.RequestStatus) error {
	var amount int64
	err := tx.QueryRowxContext(ctx,
		`SELECT amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, cmd.RequestID).Scan(&amount)
	if err != nil {
		if domain.CodeOf(mapError("settle: load charge", err)) == domain.CodeNotFound {
			// 已结算却没有 charge 行 = 账本漂移，绝不当作重复成功（调用方
			// 必须告警并核对，不能当"良性竞态"跳过）。
			return &domain.SettleError{
				Kind: domain.SettleOutcomeLedgerDrift, Code: domain.CodeConflict,
				Message: "settle: request is settled but the ledger holds no charge (ledger drift)",
				Status:  status, RequestedChargeMicros: cmd.ChargeMicros,
			}
		}
		return mapError("settle: load charge", err)
	}
	existing := domain.Microcredit(amount)
	if existing != cmd.ChargeMicros {
		return &domain.SettleError{
			Kind: domain.SettleOutcomeChargeMismatch, Code: domain.CodeConflict,
			Message: "settle: request is settled with a different charge (inconsistent duplicate)",
			Status:  status, ExistingChargeMicros: &existing,
			RequestedChargeMicros: cmd.ChargeMicros,
		}
	}
	consistent, err := usageFactConsistent(ctx, tx, cmd.Usage)
	if err != nil {
		// 存储故障：响亮地向上抛。usage 的一致性无从判断，"账本金额一致"
		// 不足以证明这是一次幂等重投 —— 绝不把它当成重复成功吞掉。
		return err
	}
	if !consistent {
		return &domain.SettleError{
			Kind: domain.SettleOutcomeChargeMismatch, Code: domain.CodeConflict,
			Message: "settle: request is settled with a different usage fact (inconsistent duplicate)",
			Status:  status, ExistingChargeMicros: &existing,
			RequestedChargeMicros: cmd.ChargeMicros,
		}
	}
	return &domain.SettleError{
		Kind: domain.SettleOutcomeDuplicate, Code: domain.CodeConflict,
		Message: "settle: request already settled (consistent duplicate delivery)",
		Status:  status, ExistingChargeMicros: &existing,
		RequestedChargeMicros: cmd.ChargeMicros,
	}
}

// usageFactConsistent compares the delivered metering fact with the one
// already recorded for the same attempt revision. A delivery for an attempt
// that has no recorded fact cannot be compared — it is left consistent (the
// charge amount is the customer-facing fact that decides the outcome).
//
// A **storage failure** is NOT a consistent fact and is returned to the
// caller: only sql.ErrNoRows (no comparable revision — the deliberate
// compatibility) keeps the delivery comparable. Any other error means the
// usage fact could not be read, so "the ledger amount matches" proves
// nothing about idempotency and the failure must surface loudly instead of
// being absorbed as a duplicate.
func usageFactConsistent(ctx context.Context, tx *sqlx.Tx, u domain.UsageRecord) (bool, error) {
	if u.AttemptID == "" {
		return true, nil
	}
	revision := u.Revision
	if revision == 0 {
		revision = 1
	}
	var in, out, cacheRead, cacheWrite, reasoning sql.NullInt64
	err := tx.QueryRowxContext(ctx,
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens
		 FROM inference_usage_records WHERE attempt_id = $1 AND revision = $2`,
		u.AttemptID, revision).Scan(&in, &out, &cacheRead, &cacheWrite, &reasoning)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 没有可比的 revision（刻意兼容）：交给金额裁决。
			return true, nil
		}
		return false, mapError("settle: load usage fact", err)
	}
	return sameTokens(in, u.Buckets.InputTokens) &&
		sameTokens(out, u.Buckets.OutputTokens) &&
		sameTokens(cacheRead, u.Buckets.CacheReadTokens) &&
		sameTokens(cacheWrite, u.Buckets.CacheWriteTokens) &&
		sameTokens(reasoning, u.Buckets.ReasoningTokens), nil
}

// sameTokens compares a stored (nullable) bucket with a delivered one: an
// unreported bucket is NULL, never zero.
func sameTokens(stored sql.NullInt64, delivered *int64) bool {
	if !stored.Valid {
		return delivered == nil
	}
	return delivered != nil && stored.Int64 == *delivered
}

// usageBucketsEmpty reports whether the delivered metering fact carries no
// token at all (nothing to book).
func usageBucketsEmpty(b domain.UsageBuckets) bool {
	return b.InputTokens == nil && b.OutputTokens == nil && b.CacheReadTokens == nil &&
		b.CacheWriteTokens == nil && b.ReasoningTokens == nil
}

// settleWallet is the charge_source='wallet' settlement (Task 14): same
// one-transaction shape as the plan path — usage fact + ledger charge
// (micromoney + currency, pinned price version) + wallet hold → consume
// entries + reservation conversion + request terminal state. The request
// row lock is already held by the caller.
//
// The wallet admission created ONE monetary hold (the mirrored wallet
// reservation + the freeze row it mirrors). It is checked FIRST, before any
// usage/rate/ledger write: a hold that went missing is an accounting
// anomaly and must be classified (missing_reservation / reservation_drift),
// never settle half of the request and never fall through to an ordinary
// not-found/conflict the caller would swallow (设计 §7.2: 本应存在的金额预占
// 被删除必须可识别).
//
// Crash recovery calls this with cmd.WalletCharge == nil: the conservative
// charge then equals the RESERVED hold (cmd.ChargeMicros) in the wallet's
// own currency (Task 9 口径: 未知费用 → 按预占额保守入账，可冲正).
func (s *Store) settleWallet(ctx context.Context, tx *sqlx.Tx, cmd domain.SettleCommand, accountID string, priceVersionID *string, status domain.RequestStatus) error {
	if cmd.ChargeMicros < 0 {
		return domain.WrapError(domain.CodeInvalidInput, "settle wallet: negative charge", domain.ErrNegativeValue)
	}

	// 1. The monetary hold the admission created — checked before ANY
	// write, so a missing hold leaves no partial effect behind.
	if err := checkWalletHolds(ctx, tx, cmd.RequestID, status, cmd.ChargeMicros); err != nil {
		return err
	}

	// 2. Usage fact (UNIQUE(attempt_id, revision)).
	if err := insertUsageRecord(ctx, tx, &cmd.Usage); err != nil {
		return err
	}

	// 2b. Rate reservation (Task 4): the TPM hold becomes the ACTUAL
	// normalized token consumption (same口径 as the plan path).
	if err := s.settleRateTx(ctx, tx, cmd.RequestID, access.NormalizedTokens(cmd.Usage.Buckets)); err != nil {
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
	if err := s.settleWalletLocked(ctx, tx, cmd.RequestID, *charge, status); err != nil {
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
// 终审 Finding 1 —— **速率预占的处置**：一个因为账务异常（缺预占 / 预占漂移 /
// 证据不足）而停放的请求**永远不会被结算**，它的速率预占若继续停在
// state='reserved' 就会永久占着账户的 TPM 额度（账户被永久 429 拦住，且没有
// 任何路径能把它取回来）。因此在**同一个事务**里把该请求的在途速率预占处置
// 为 state='anomaly'（语义见 inference_limit_repo.go 的 rateStateAnomaly）：
//
//   - 只有**只能人工处理**的原因才处置：证据驱动的原因（unknown_usage /
//     crash_recovery）保留预占 —— 恢复 worker 会按保守估算结算它，额度不能
//     提前归还；
//   - 处置**不动金额**：窗口/Key 的金额预占仍在，账本没有 charge，钱既没结
//     也没放；处置**不动证据**：核对任务的原因与 request.last_error 不变；
//   - RPM 事实保留：一次逻辑请求仍然计 1 次。
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
	// 只能人工处理的原因 → 在途速率预占显式处置为 anomaly（同一事务）：停放的
	// 异常不得永久占用账户 TPM 额度（终审 Finding 1）。
	if IsManualOnlyReconciliationReason(reason) {
		if err := s.parkRateReservationTx(ctx, tx, requestID, reason); err != nil {
			return err
		}
	}
	return s.EnqueueReconciliationJobTx(ctx, w, EnqueueReconciliationCommand{
		RequestID: &requestID, Reason: reason, Deadline: deadline,
	})
}

// ReportTerminalAnomaly records a settlement anomaly against a request whose
// terminal state must NOT be overwritten (a released request that receives a
// late usage): the reconciliation job is enqueued in the same transaction and
// the request's last_error carries the anomaly, but the terminal status
// (settled/released) stays exactly as it is — 终态不可改写，账务异常必须可
// 核对、可审计。
func (s *Store) ReportTerminalAnomaly(ctx context.Context, w domain.UnitOfWork, requestID, reason string, detail json.RawMessage, deadline time.Time) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_requests SET last_error = $2, updated_at = now() WHERE id = $1`,
		requestID, string(detail))
	if err != nil {
		return mapError("report terminal anomaly", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("report terminal anomaly", sql.ErrNoRows)
	}
	return s.EnqueueReconciliationJobTx(ctx, w, EnqueueReconciliationCommand{
		RequestID: &requestID, Reason: reason, Detail: detail, Deadline: deadline,
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
