package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// wallet_repo.go — 表组 inference_wallets / inference_wallet_entries /
// inference_wallet_holds / inference_wallet_audits / inference_payg_config
// (migration 030, Task 14)。
//
// 不变式（控制者裁决）:
//   - 无缓存余额列：一切余额由分录派生（credit − debit − held），客户展示
//     与账本一致；冻结额由 held 状态的 hold 行派生。
//   - 固定锁序：账户锚行 → 钱包行（FOR UPDATE），所有变动（冻结/充值/退
//     款/调整/冲正）在同一锁序下串行，并发最后余额不超扣（DB 层原子）。
//   - 幂等：业务键唯一（topup/consume/refund/adjustment/reversal）；请求级
//     冻结由 holds.request_id 唯一键兜底。
//   - 分录来源隔离：cash 可退，bonus 只能消费/调整；退款 CHECK 兜底
//     debit+cash。

// Wallet is one account+currency wallet row (settings only — no balance
// cache; balance is ledger-derived).
type Wallet struct {
	ID                      string
	BillingAccountID        string
	Currency                string
	OverageEnabled          bool
	MonthlySpendLimitMicros *int64
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Settings projects the overage gate shape for the pure rules.
func (w *Wallet) Settings() accounting.OverageSettings {
	return accounting.OverageSettings{
		Enabled:                 w.OverageEnabled,
		MonthlySpendLimitMicros: w.MonthlySpendLimitMicros,
	}
}

// WalletBalanceView is the customer-facing wallet read: settings + the
// ledger-derived balance + the current UTC-month spend figure.
type WalletBalanceView struct {
	Wallet           Wallet
	Balance          accounting.WalletBalance
	MonthSpentMicros int64
	MonthStart       time.Time
	MonthEnd         time.Time
}

// WalletEntry is one wallet ledger row (read shape).
type WalletEntry struct {
	ID              int64
	WalletID        string
	EntryType       string
	Direction       string
	Source          string
	AmountMicros    int64
	Currency        string
	RequestID       *string
	HoldID          *string
	PaymentID       *string
	RefundID        *string
	AdjustmentID    *string
	ReversesEntryID *int64
	BusinessKey     string
	CreatedBy       string
	CreatedAt       time.Time
}

type walletRow struct {
	ID        string        `db:"id"`
	AccountID string        `db:"billing_account_id"`
	Currency  string        `db:"currency"`
	Overage   bool          `db:"overage_enabled"`
	Limit     sql.NullInt64 `db:"monthly_spend_limit_micros"`
	CreatedAt time.Time     `db:"created_at"`
	UpdatedAt time.Time     `db:"updated_at"`
}

func (r walletRow) toWallet() Wallet {
	w := Wallet{
		ID: r.ID, BillingAccountID: r.AccountID, Currency: r.Currency,
		OverageEnabled: r.Overage, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.Limit.Valid {
		l := r.Limit.Int64
		w.MonthlySpendLimitMicros = &l
	}
	return w
}

// ---------------------------------------------------------------------------
// 读取面
// ---------------------------------------------------------------------------

// GetWalletByAccount loads the account's wallet in one currency.
func (s *Store) GetWalletByAccount(ctx context.Context, accountID, currency string) (*Wallet, error) {
	var row walletRow
	if err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_wallets WHERE billing_account_id = $1 AND currency = $2`,
		accountID, currency); err != nil {
		return nil, mapError("get wallet", err)
	}
	w := row.toWallet()
	return &w, nil
}

// walletSumsLocked aggregates the ledger + held split of one wallet. The
// caller MUST hold the wallet row lock (or a stronger serialization) when
// the sums gate a mutation.
func walletSumsLocked(ctx context.Context, tx *sqlx.Tx, walletID string) (accounting.WalletSums, error) {
	var sums accounting.WalletSums
	var rows []struct {
		Source    string `db:"source"`
		Direction string `db:"direction"`
		Total     int64  `db:"total"`
	}
	if err := tx.SelectContext(ctx, &rows,
		`SELECT source, direction, COALESCE(SUM(amount_micros), 0) AS total
		   FROM inference_wallet_entries WHERE wallet_id = $1
		  GROUP BY source, direction`, walletID); err != nil {
		return sums, mapError("wallet sums: entries", err)
	}
	for _, r := range rows {
		switch {
		case r.Source == string(accounting.WalletCash) && r.Direction == string(accounting.DirCredit):
			sums.CashCredit = r.Total
		case r.Source == string(accounting.WalletCash):
			sums.CashDebit = r.Total
		case r.Direction == string(accounting.DirCredit):
			sums.BonusCredit = r.Total
		default:
			sums.BonusDebit = r.Total
		}
	}
	if err := tx.QueryRowxContext(ctx,
		`SELECT COALESCE(SUM(cash_micros), 0), COALESCE(SUM(bonus_micros), 0)
		   FROM inference_wallet_holds WHERE wallet_id = $1 AND state = 'held'`,
		walletID).Scan(&sums.HeldCash, &sums.HeldBonus); err != nil {
		return sums, mapError("wallet sums: holds", err)
	}
	return sums, nil
}

// monthSpendLocked is the ledger-derived UTC-month wallet spend: consume
// debits minus reversal credits of consume entries within [start, end).
func monthSpendLocked(ctx context.Context, tx *sqlx.Tx, walletID string, start, end time.Time) (int64, error) {
	var spent int64
	err := tx.QueryRowxContext(ctx,
		`SELECT COALESCE(SUM(CASE
		       WHEN e.entry_type = 'consume' AND e.direction = 'debit' THEN e.amount_micros
		       WHEN e.entry_type = 'reversal' AND e.direction = 'credit'
		            AND o.entry_type = 'consume' THEN -e.amount_micros
		       ELSE 0 END), 0)
		   FROM inference_wallet_entries e
		   LEFT JOIN inference_wallet_entries o ON e.reverses_entry_id = o.id
		  WHERE e.wallet_id = $1
		    AND e.created_at >= $2 AND e.created_at < $3`,
		walletID, start, end).Scan(&spent)
	if err != nil {
		return 0, mapError("wallet month spend", err)
	}
	return spent, nil
}

// WalletBalance derives the account's wallet view in one currency (客户展
// 示 = 账本派生). No wallet row → CodeNotFound (the caller renders the
// zero state, never an invented zero row).
func (s *Store) WalletBalance(ctx context.Context, accountID, currency string, at time.Time) (*WalletBalanceView, error) {
	w, err := s.GetWalletByAccount(ctx, accountID, currency)
	if err != nil {
		return nil, err
	}
	sums, err := s.walletSums(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	bal, err := accounting.DeriveWalletBalance(w.Currency, sums)
	if err != nil {
		return nil, err
	}
	start, end := accounting.MonthBoundsUTC(at)
	spent, err := s.monthSpend(ctx, w.ID, start, end)
	if err != nil {
		return nil, err
	}
	return &WalletBalanceView{
		Wallet: *w, Balance: bal, MonthSpentMicros: spent,
		MonthStart: start, MonthEnd: end,
	}, nil
}

// ListWalletBalances derives every wallet of the account (per currency).
func (s *Store) ListWalletBalances(ctx context.Context, accountID string, at time.Time) ([]WalletBalanceView, error) {
	var rows []walletRow
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_wallets WHERE billing_account_id = $1 ORDER BY currency`, accountID); err != nil {
		return nil, mapError("list wallets", err)
	}
	out := make([]WalletBalanceView, 0, len(rows))
	for _, r := range rows {
		v, err := s.WalletBalance(ctx, accountID, r.Currency, at)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *Store) walletSums(ctx context.Context, walletID string) (accounting.WalletSums, error) {
	var sums accounting.WalletSums
	var rows []struct {
		Source    string `db:"source"`
		Direction string `db:"direction"`
		Total     int64  `db:"total"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT source, direction, COALESCE(SUM(amount_micros), 0) AS total
		   FROM inference_wallet_entries WHERE wallet_id = $1
		  GROUP BY source, direction`, walletID); err != nil {
		return sums, mapError("wallet sums: entries", err)
	}
	for _, r := range rows {
		switch {
		case r.Source == string(accounting.WalletCash) && r.Direction == string(accounting.DirCredit):
			sums.CashCredit = r.Total
		case r.Source == string(accounting.WalletCash):
			sums.CashDebit = r.Total
		case r.Direction == string(accounting.DirCredit):
			sums.BonusCredit = r.Total
		default:
			sums.BonusDebit = r.Total
		}
	}
	if err := s.db.QueryRowxContext(ctx,
		`SELECT COALESCE(SUM(cash_micros), 0), COALESCE(SUM(bonus_micros), 0)
		   FROM inference_wallet_holds WHERE wallet_id = $1 AND state = 'held'`,
		walletID).Scan(&sums.HeldCash, &sums.HeldBonus); err != nil {
		return sums, mapError("wallet sums: holds", err)
	}
	return sums, nil
}

func (s *Store) monthSpend(ctx context.Context, walletID string, start, end time.Time) (int64, error) {
	var spent int64
	err := s.db.QueryRowxContext(ctx,
		`SELECT COALESCE(SUM(CASE
		       WHEN e.entry_type = 'consume' AND e.direction = 'debit' THEN e.amount_micros
		       WHEN e.entry_type = 'reversal' AND e.direction = 'credit'
		            AND o.entry_type = 'consume' THEN -e.amount_micros
		       ELSE 0 END), 0)
		   FROM inference_wallet_entries e
		   LEFT JOIN inference_wallet_entries o ON e.reverses_entry_id = o.id
		  WHERE e.wallet_id = $1
		    AND e.created_at >= $2 AND e.created_at < $3`,
		walletID, start, end).Scan(&spent)
	if err != nil {
		return 0, mapError("wallet month spend", err)
	}
	return spent, nil
}

// ListWalletEntries is the keyset-paginated statement read (newest first;
// afterID is the last seen entry id).
func (s *Store) ListWalletEntries(ctx context.Context, accountID, currency string, afterID int64, limit int) ([]WalletEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	w, err := s.GetWalletByAccount(ctx, accountID, currency)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID              int64          `db:"id"`
		WalletID        string         `db:"wallet_id"`
		EntryType       string         `db:"entry_type"`
		Direction       string         `db:"direction"`
		Source          string         `db:"source"`
		AmountMicros    int64          `db:"amount_micros"`
		Currency        string         `db:"currency"`
		RequestID       sql.NullString `db:"request_id"`
		HoldID          sql.NullString `db:"hold_id"`
		PaymentID       sql.NullString `db:"payment_id"`
		RefundID        sql.NullString `db:"refund_id"`
		AdjustmentID    sql.NullString `db:"adjustment_id"`
		ReversesEntryID sql.NullInt64  `db:"reverses_entry_id"`
		BusinessKey     string         `db:"business_key"`
		CreatedBy       string         `db:"created_by"`
		CreatedAt       time.Time      `db:"created_at"`
	}
	err = s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_wallet_entries
		  WHERE wallet_id = $1 AND ($2 = 0 OR id < $2)
		  ORDER BY id DESC LIMIT `+itoa(limit),
		w.ID, afterID)
	if err != nil {
		return nil, mapError("list wallet entries", err)
	}
	out := make([]WalletEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, WalletEntry{
			ID: r.ID, WalletID: r.WalletID, EntryType: r.EntryType, Direction: r.Direction,
			Source: r.Source, AmountMicros: r.AmountMicros, Currency: r.Currency,
			RequestID: strFromNull(r.RequestID), HoldID: strFromNull(r.HoldID),
			PaymentID: strFromNull(r.PaymentID), RefundID: strFromNull(r.RefundID),
			AdjustmentID: strFromNull(r.AdjustmentID), ReversesEntryID: int64FromNullPtr(r.ReversesEntryID),
			BusinessKey: r.BusinessKey, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func int64FromNullPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// ---------------------------------------------------------------------------
// 钱包建立与设置（审计）
// ---------------------------------------------------------------------------

// getOrCreateWalletLocked returns the wallet row, creating it (with the
// 'create' audit row) when missing. The caller holds the account anchor
// lock, serializing the create against concurrent wallet mutations.
func getOrCreateWalletLocked(ctx context.Context, tx *sqlx.Tx, accountID, currency, changedBy string) (*Wallet, bool, error) {
	var row walletRow
	err := tx.QueryRowxContext(ctx,
		`INSERT INTO inference_wallets (billing_account_id, currency)
		 VALUES ($1, $2)
		 ON CONFLICT (billing_account_id, currency) DO NOTHING
		 RETURNING *`, accountID, currency).StructScan(&row)
	switch {
	case err == nil:
		if _, aerr := tx.ExecContext(ctx,
			`INSERT INTO inference_wallet_audits (wallet_id, action, changed_by, new_overage_enabled)
			 VALUES ($1, 'create', $2, false)`, row.ID, changedBy); aerr != nil {
			return nil, false, mapError("wallet audit: create", aerr)
		}
		w := row.toWallet()
		return &w, true, nil
	case errors.Is(err, sql.ErrNoRows):
		// Lost the create race — read the winner.
	default:
		return nil, false, mapError("create wallet", err)
	}
	if err := tx.QueryRowxContext(ctx,
		`SELECT * FROM inference_wallets
		  WHERE billing_account_id = $1 AND currency = $2`, accountID, currency).StructScan(&row); err != nil {
		return nil, false, mapError("read wallet", err)
	}
	w := row.toWallet()
	return &w, false, nil
}

// lockWalletTx takes the wallet row FOR UPDATE (fixed lock order step 2,
// after the account anchor).
func lockWalletTx(ctx context.Context, tx *sqlx.Tx, walletID string) (*Wallet, error) {
	var row walletRow
	if err := tx.QueryRowxContext(ctx,
		`SELECT * FROM inference_wallets WHERE id = $1 FOR UPDATE`, walletID).StructScan(&row); err != nil {
		return nil, mapError("lock wallet", err)
	}
	w := row.toWallet()
	return &w, nil
}

// SetOverageTx changes the overage switch / monthly spend limit with the
// audit trail in the same transaction (裁决 4: 开启状态与上限的修改要审计).
// The wallet row is created on first use. changedBy is the server-verified
// identity ("user:<id>" / "operator:<id>").
func (s *Store) SetOverageTx(ctx context.Context, w domain.UnitOfWork, accountID, currency string, enabled bool, limit *int64, changedBy string) (*Wallet, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	next := accounting.OverageSettings{Enabled: enabled, MonthlySpendLimitMicros: limit}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := lockAccountTx(ctx, tx, accountID); err != nil {
		return nil, err
	}
	wallet, created, err := getOrCreateWalletLocked(ctx, tx, accountID, currency, changedBy)
	if err != nil {
		return nil, err
	}
	if _, err := lockWalletTx(ctx, tx, wallet.ID); err != nil {
		return nil, err
	}
	old := wallet.Settings()
	for _, spec := range accounting.AuditForChange(created, old, next, changedBy) {
		if spec.Action == accounting.WalletAuditCreate {
			continue // already written by getOrCreateWalletLocked
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_wallet_audits
			 (wallet_id, action, changed_by, old_overage_enabled, new_overage_enabled,
			  old_monthly_spend_limit_micros, new_monthly_spend_limit_micros)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			wallet.ID, spec.Action, spec.ChangedBy,
			boolPtr(spec.OldEnabled), boolPtr(spec.NewEnabled),
			int64PtrVal(spec.OldLimitMicros), int64PtrVal(spec.NewLimitMicros)); err != nil {
			return nil, mapError("wallet audit: settings", err)
		}
	}
	var row walletRow
	if err := tx.QueryRowxContext(ctx,
		`UPDATE inference_wallets
		    SET overage_enabled = $2, monthly_spend_limit_micros = $3, updated_at = now()
		  WHERE id = $1
		  RETURNING *`, wallet.ID, enabled, int64PtrVal(limit)).StructScan(&row); err != nil {
		return nil, mapError("set overage", err)
	}
	out := row.toWallet()
	return &out, nil
}

func boolPtr(b *bool) interface{} {
	if b == nil {
		return nil
	}
	return *b
}

func int64PtrVal(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// ---------------------------------------------------------------------------
// 入场冻结（钱包准入，设计 §7.2 同口径：完整有界预占）
// ---------------------------------------------------------------------------

// ReserveWallet atomically admits one wallet-funded call:
//
//  1. lock the billing account (anchor) then the wallet row — the fixed
//     lock order every wallet mutation follows;
//  2. gate on the explicit overage settings + the UTC-month spend limit
//     (未开启 = ErrOverageDisabled，余额一分不动);
//  3. derive the balance from the ledger and split the freeze bonus-first;
//     insufficient → ErrInsufficientBalance. Because every mutation takes
//     the same two locks, the check-and-freeze is atomic against
//     concurrent admissions (并发最后余额不双花);
//  4. persist the request (charge_source='wallet', pinned price), the
//     wallet hold (request_id unique = 幂等业务键) and the mirrored
//     reservation row — all in the caller's transaction.
func (s *Store) ReserveWallet(ctx context.Context, w domain.UnitOfWork, cmd domain.ReserveWalletCommand) (*domain.Admission, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if cmd.HoldMicros <= 0 {
		return nil, domain.WrapError(domain.CodeInvalidInput, "reserve wallet: hold must be > 0", domain.ErrNegativeValue)
	}

	// 1. Fixed lock order: account → wallet.
	if err := lockAccountTx(ctx, tx, cmd.Request.BillingAccountID); err != nil {
		return nil, err
	}
	wallet, err := lockWalletTx(ctx, tx, cmd.WalletID)
	if err != nil {
		return nil, err
	}
	if wallet.BillingAccountID != cmd.Request.BillingAccountID {
		return nil, domain.NewError(domain.CodeInvalidInput, "reserve wallet: wallet does not belong to the account")
	}

	// 2. Spend gate (explicit opt-in + monthly cap).
	spent, err := monthSpendLocked(ctx, tx, wallet.ID, cmd.MonthStart, cmd.MonthEnd)
	if err != nil {
		return nil, err
	}
	if err := accounting.CheckWalletSpend(wallet.Settings(), spent, cmd.HoldMicros); err != nil {
		return nil, err
	}

	// 3. Derived balance + bonus-first freeze split.
	sums, err := walletSumsLocked(ctx, tx, wallet.ID)
	if err != nil {
		return nil, err
	}
	bal, err := accounting.DeriveWalletBalance(wallet.Currency, sums)
	if err != nil {
		return nil, err
	}
	cash, bonus, err := accounting.SplitFreeze(bal.BonusAvailable, bal.CashAvailable, cmd.HoldMicros)
	if err != nil {
		return nil, err
	}

	// 4. Persist request + hold + mirrored reservation. The pinned
	// sale_money price version lands on the request row (结算只读钉住的版
	// 本，调价不改在途冻结).
	req := cmd.Request
	req.Status = domain.ReqReserved
	req.AdmittedAt = &cmd.AdmittedAt
	req.ChargeSource = domain.ChargeSourceWallet
	if cmd.PriceVersionID != "" {
		req.PriceVersionID = &cmd.PriceVersionID
	}
	hold := domain.Microcredit(cmd.HoldMicros)
	req.ReservedMicros = &hold
	if err := insertRequest(ctx, tx, &req); err != nil {
		return nil, err
	}
	holdID := uuid.NewString()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_wallet_holds
		 (id, wallet_id, request_id, amount_micros, cash_micros, bonus_micros, price_version_id, state)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'held')`,
		holdID, wallet.ID, req.ID, cmd.HoldMicros, cash, bonus, cmd.PriceVersionID); err != nil {
		return nil, mapError("reserve wallet: hold", err)
	}
	reservation := domain.Reservation{
		ID: uuid.NewString(), RequestID: req.ID, TargetKind: domain.TargetWallet,
		Amount: hold, State: domain.ReservationHeld,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_reservations (id, request_id, target_kind, amount_micros, state)
		 VALUES ($1,$2,'wallet',$3,'held')`,
		reservation.ID, req.ID, cmd.HoldMicros); err != nil {
		return nil, mapError("reserve wallet: reservation", err)
	}
	return &domain.Admission{RequestID: req.ID, Holds: []domain.Reservation{reservation}}, nil
}

// ---------------------------------------------------------------------------
// 结算 / 释放（ledger_repo.Settle 与 reservation_repo.releaseLocked 的钱包分支；
// 全部在调用方事务内、请求行锁已持）
// ---------------------------------------------------------------------------

// settleWalletLocked converts the request's wallet hold into consume
// entries: bonus-first within the frozen composition; over-hold excess
// debits cash honestly (超占真实入账，不隐藏负差额). Duplicate delivery
// hits the per-request charge unique key in the caller.
func (s *Store) settleWalletLocked(ctx context.Context, tx *sqlx.Tx, requestID string, charge domain.Money) error {
	var holdID, walletID, currency string
	var amount, cash, bonus int64
	if err := tx.QueryRowxContext(ctx,
		`SELECT h.id, h.wallet_id, w.currency, h.amount_micros, h.cash_micros, h.bonus_micros
		   FROM inference_wallet_holds h
		   JOIN inference_wallets w ON w.id = h.wallet_id
		  WHERE h.request_id = $1 FOR UPDATE OF h`, requestID).
		Scan(&holdID, &walletID, &currency, &amount, &cash, &bonus); err != nil {
		return mapError("settle wallet: lock hold", err)
	}
	if charge.Currency != currency {
		return domain.WrapError(domain.CodeInvalidInput,
			"settle wallet: charge currency "+charge.Currency+" != wallet currency "+currency, domain.ErrCurrencyMismatch)
	}
	consumeCash, consumeBonus, extraCash, err := accounting.SplitConsume(cash, bonus, charge.Micros)
	if err != nil {
		return err
	}
	// Per-source consume entries (零额不落行 — entries CHECK amount > 0；
	// 但账本 charge 行恒落，见 ledger_repo).
	write := func(source accounting.WalletSource, micros int64) error {
		if micros == 0 {
			return nil
		}
		m, err := domain.NewMoney(micros, currency)
		if err != nil {
			return err
		}
		spec := accounting.WalletEntrySpec{
			Type: accounting.EntryConsume, Direction: accounting.DirDebit,
			Source: source, Amount: m,
		}
		if err := accounting.ValidateWalletEntry(spec); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO inference_wallet_entries
			 (wallet_id, entry_type, direction, source, amount_micros, currency,
			  request_id, hold_id, business_key, created_by)
			 VALUES ($1,'consume','debit',$2,$3,$4,$5,$6,$7,'system')`,
			walletID, string(source), micros, currency, requestID, holdID,
			accounting.WalletConsumeKey(requestID, source))
		return mapError("settle wallet: consume entry", err)
	}
	if err := write(accounting.WalletBonus, consumeBonus); err != nil {
		return err
	}
	if err := write(accounting.WalletCash, consumeCash+extraCash); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_wallet_holds
		    SET state = 'settled', settled_at = now()
		  WHERE id = $1 AND state = 'held'`, holdID)
	if err != nil {
		return mapError("settle wallet: hold", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"settle wallet: hold already transitioned (concurrent release?)")
	}
	return nil
}

// releaseWalletLocked drops the request's wallet hold (confirmed zero
// consumption): a hold-state transition, never a ledger rewrite (与额度
// 预占释放同口径).
func (s *Store) releaseWalletLocked(ctx context.Context, tx *sqlx.Tx, requestID string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_wallet_holds
		    SET state = 'released', released_at = now()
		  WHERE request_id = $1 AND state = 'held'`, requestID)
	if err != nil {
		return mapError("release wallet: hold", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"release wallet: hold not held (concurrent settlement?)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 支付驱动的充值 / 退款（worker 消费 outbox；幂等业务键兜底重复回调）
// ---------------------------------------------------------------------------

// WalletTopupCommand credits cash from one settled payment.
type WalletTopupCommand struct {
	AccountID    string
	Currency     string
	AmountMicros int64
	PaymentID    string
	OrderID      string
}

// CreditTopupTx credits the cash topup (wallet:topup:{payment_id}).
// The wallet row is created on first topup (audit 'create').
//
// Replay semantics: a duplicate business key makes the INSERT conflict —
// the error is returned as CodeConflict and the CALLER rolls the
// transaction back (a failed statement poisons a Postgres tx anyway).
// The worker treats CodeConflict as "already applied" and marks the
// outbox message delivered in a fresh transaction (幂等收敛).
func (s *Store) CreditTopupTx(ctx context.Context, w domain.UnitOfWork, cmd WalletTopupCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	m, err := domain.NewMoney(cmd.AmountMicros, cmd.Currency)
	if err != nil {
		return err
	}
	if m.Micros <= 0 {
		return domain.WrapError(domain.CodeInvalidInput, "wallet topup: amount must be > 0", domain.ErrNegativeValue)
	}
	if err := accounting.ValidateWalletEntry(accounting.WalletEntrySpec{
		Type: accounting.EntryTopup, Direction: accounting.DirCredit, Source: accounting.WalletCash, Amount: m,
	}); err != nil {
		return err
	}
	if err := lockAccountTx(ctx, tx, cmd.AccountID); err != nil {
		return err
	}
	wallet, _, err := getOrCreateWalletLocked(ctx, tx, cmd.AccountID, cmd.Currency, "payment:"+cmd.PaymentID)
	if err != nil {
		return err
	}
	if _, err := lockWalletTx(ctx, tx, wallet.ID); err != nil {
		return err
	}
	// 重复回调撞业务键唯一约束 → CodeConflict；调用方回滚本事务后按
	// "已入账"收敛（失败语句已污染 Postgres 事务，必须回滚）。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_wallet_entries
		 (wallet_id, entry_type, direction, source, amount_micros, currency,
		  payment_id, business_key, created_by)
		 VALUES ($1,'topup','credit','cash',$2,$3,$4,$5,$6)`,
		wallet.ID, m.Micros, m.Currency, cmd.PaymentID,
		accounting.WalletTopupKey(cmd.PaymentID), "payment:"+cmd.PaymentID); err != nil {
		return mapError("wallet topup", err)
	}
	return nil
}

// WalletRefundCommand debits cash back out (原路退; 只允许现金来源).
type WalletRefundCommand struct {
	AccountID    string
	Currency     string
	AmountMicros int64
	RefundID     string
	PaymentID    string
}

// RefundWalletTx debits the cash refund (wallet:refund:{refund_id}). The
// refund always debits CASH and may drive the derived cash balance
// negative when the topped-up funds were already spent — the honest ledger
// (不隐藏负差额); it NEVER touches the bonus source (赠送不得伪装现金退
// 款，DB CHECK 兜底). A duplicate refund delivery conflicts on the business
// key → CodeConflict, and the caller rolls back then converges as replayed.
func (s *Store) RefundWalletTx(ctx context.Context, w domain.UnitOfWork, cmd WalletRefundCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	m, err := domain.NewMoney(cmd.AmountMicros, cmd.Currency)
	if err != nil {
		return err
	}
	if m.Micros <= 0 {
		return domain.WrapError(domain.CodeInvalidInput, "wallet refund: amount must be > 0", domain.ErrNegativeValue)
	}
	if err := accounting.ValidateWalletEntry(accounting.WalletEntrySpec{
		Type: accounting.EntryRefund, Direction: accounting.DirDebit, Source: accounting.WalletCash, Amount: m,
	}); err != nil {
		return err
	}
	if err := lockAccountTx(ctx, tx, cmd.AccountID); err != nil {
		return err
	}
	var row walletRow
	if err := tx.QueryRowxContext(ctx,
		`SELECT * FROM inference_wallets
		  WHERE billing_account_id = $1 AND currency = $2 FOR UPDATE`,
		cmd.AccountID, cmd.Currency).StructScan(&row); err != nil {
		// 退款的充值尚未入账（消息乱序）→ 明确错误，调用方重投；
		// 绝不静默丢弃一笔退款。
		return mapError("wallet refund: wallet", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_wallet_entries
		 (wallet_id, entry_type, direction, source, amount_micros, currency,
		  payment_id, refund_id, business_key, created_by)
		 VALUES ($1,'refund','debit','cash',$2,$3,$4,$5,$6,$7)`,
		row.ID, m.Micros, m.Currency, cmd.PaymentID, cmd.RefundID,
		accounting.WalletRefundKey(cmd.RefundID), "refund:"+cmd.RefundID); err != nil {
		return mapError("wallet refund", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 运营调整与冲正（admin_adjustments 端点；设计 §9.2 人员及服务双重归因）
// ---------------------------------------------------------------------------

// WalletAdjustmentCommand is one operator money adjustment. Direction
// credit grants (bonus 赠送或现金补偿), debit claws back; both write an
// inference_adjustments row (unit='micromoney') and the wallet entry in
// one transaction. IdempotencyKey is operator-supplied (重复补偿投递只生
// 效一次).
type WalletAdjustmentCommand struct {
	AccountID       string
	Currency        string
	Source          accounting.WalletSource
	Direction       accounting.WalletDirection
	AmountMicros    int64
	Reason          string
	OperatorSubject string
	ServiceSubject  string
	IdempotencyKey  string
}

// ApplyWalletAdjustmentTx applies one operator adjustment atomically. A
// replayed idempotency key conflicts on inference_adjustments'
// UNIQUE(idempotency_key) → CodeConflict; the caller rolls back (the failed
// statement poisons the tx) and re-reads the stored row for the response —
// 重复补偿投递只生效一次.
func (s *Store) ApplyWalletAdjustmentTx(ctx context.Context, w domain.UnitOfWork, cmd WalletAdjustmentCommand) (*Adjustment, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if cmd.Reason == "" || cmd.OperatorSubject == "" || cmd.IdempotencyKey == "" {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"wallet adjustment: reason, operator and idempotency key are required")
	}
	m, err := domain.NewMoney(cmd.AmountMicros, cmd.Currency)
	if err != nil {
		return nil, err
	}
	if m.Micros <= 0 {
		return nil, domain.WrapError(domain.CodeInvalidInput, "wallet adjustment: amount must be > 0", domain.ErrNegativeValue)
	}
	if err := accounting.ValidateWalletEntry(accounting.WalletEntrySpec{
		Type: accounting.EntryAdjustment, Direction: cmd.Direction, Source: cmd.Source, Amount: m,
	}); err != nil {
		return nil, err
	}
	if err := lockAccountTx(ctx, tx, cmd.AccountID); err != nil {
		return nil, err
	}
	wallet, _, err := getOrCreateWalletLocked(ctx, tx, cmd.AccountID, cmd.Currency, "operator:"+cmd.OperatorSubject)
	if err != nil {
		return nil, err
	}
	if _, err := lockWalletTx(ctx, tx, wallet.ID); err != nil {
		return nil, err
	}

	adj := Adjustment{
		ID: uuid.NewString(), BillingAccountID: cmd.AccountID,
		Reason: cmd.Reason, AmountMicros: m.Micros,
		Direction: string(cmd.Direction), Unit: "micromoney", Currency: m.Currency,
		Source:          string(cmd.Source),
		OperatorSubject: cmd.OperatorSubject, ServiceSubject: cmd.ServiceSubject,
		IdempotencyKey: cmd.IdempotencyKey,
	}
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_adjustments
		 (id, billing_account_id, reason, amount_micros, direction,
		  unit, currency, source, operator_subject, service_subject, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,'micromoney',$6,$7,$8,$9,$10)
		 RETURNING created_at`,
		adj.ID, adj.BillingAccountID, adj.Reason, adj.AmountMicros, adj.Direction,
		adj.Currency, adj.Source, adj.OperatorSubject, adj.ServiceSubject, adj.IdempotencyKey).
		Scan(&adj.CreatedAt)
	if err != nil {
		return nil, mapError("wallet adjustment: insert", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_wallet_entries
		 (wallet_id, entry_type, direction, source, amount_micros, currency,
		  adjustment_id, business_key, created_by)
		 VALUES ($1,'adjustment',$2,$3,$4,$5,$6,$7,$8)`,
		wallet.ID, string(cmd.Direction), string(cmd.Source), m.Micros, m.Currency,
		adj.ID, accounting.WalletAdjustmentKey(cmd.IdempotencyKey), "operator:"+cmd.OperatorSubject); err != nil {
		return nil, mapError("wallet adjustment: entry", err)
	}
	adj.Unit = "micromoney"
	return &adj, nil
}

// GetAdjustmentByIdempotencyKey re-reads a stored adjustment (幂等重放的
// 响应重读).
func (s *Store) GetAdjustmentByIdempotencyKey(ctx context.Context, key string) (*Adjustment, error) {
	var row struct {
		ID        string         `db:"id"`
		AccountID string         `db:"billing_account_id"`
		RequestID sql.NullString `db:"request_id"`
		Reason    string         `db:"reason"`
		Amount    int64          `db:"amount_micros"`
		Direction string         `db:"direction"`
		Unit      string         `db:"unit"`
		Currency  sql.NullString `db:"currency"`
		Source    sql.NullString `db:"source"`
		Operator  string         `db:"operator_subject"`
		Service   string         `db:"service_subject"`
		Key       string         `db:"idempotency_key"`
		CreatedAt time.Time      `db:"created_at"`
	}
	if err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_adjustments WHERE idempotency_key = $1`, key); err != nil {
		return nil, mapError("get adjustment by key", err)
	}
	return &Adjustment{
		ID: row.ID, BillingAccountID: row.AccountID, Reason: row.Reason,
		AmountMicros: row.Amount, Direction: row.Direction, Unit: row.Unit,
		Currency: row.Currency.String, Source: row.Source.String, OperatorSubject: row.Operator,
		ServiceSubject: row.Service, IdempotencyKey: row.Key, CreatedAt: row.CreatedAt,
	}, nil
}

// ReverseWalletEntryTx appends the reversal of one earlier entry (追加 +
// 冲正，不重写已结算事实). One entry is reversed at most once (部分唯一索
// 引 + 业务键双兜底); a duplicate attempt conflicts → CodeConflict (caller
// rolls back, then reports the replay).
func (s *Store) ReverseWalletEntryTx(ctx context.Context, w domain.UnitOfWork, accountID string, entryID int64, operatorSubject string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if err := lockAccountTx(ctx, tx, accountID); err != nil {
		return err
	}
	var orig struct {
		ID         int64  `db:"id"`
		WalletID   string `db:"wallet_id"`
		EntryType  string `db:"entry_type"`
		Direction  string `db:"direction"`
		Source     string `db:"source"`
		Amount     int64  `db:"amount_micros"`
		Currency   string `db:"currency"`
		AccountRow string `db:"billing_account_id"`
	}
	if err := tx.QueryRowxContext(ctx,
		`SELECT e.id, e.wallet_id, e.entry_type, e.direction, e.source,
		        e.amount_micros, e.currency, w.billing_account_id
		   FROM inference_wallet_entries e
		   JOIN inference_wallets w ON w.id = e.wallet_id
		  WHERE e.id = $1 FOR UPDATE`, entryID).StructScan(&orig); err != nil {
		return mapError("wallet reversal: load original", err)
	}
	if orig.AccountRow != accountID {
		return domain.NewError(domain.CodeInvalidInput, "wallet reversal: entry does not belong to the account")
	}
	m, err := domain.NewMoney(orig.Amount, orig.Currency)
	if err != nil {
		return err
	}
	rev, err := accounting.ReversalOf(accounting.WalletEntrySpec{
		Type:      accounting.WalletEntryType(orig.EntryType),
		Direction: accounting.WalletDirection(orig.Direction),
		Source:    accounting.WalletSource(orig.Source), Amount: m,
	})
	if err != nil {
		return err
	}
	if _, err := lockWalletTx(ctx, tx, orig.WalletID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_wallet_entries
		 (wallet_id, entry_type, direction, source, amount_micros, currency,
		  reverses_entry_id, business_key, created_by)
		 VALUES ($1,'reversal',$2,$3,$4,$5,$6,$7,$8)`,
		orig.WalletID, string(rev.Direction), string(rev.Source), m.Micros, m.Currency,
		entryID, accounting.WalletReversalKey(entryID), "operator:"+operatorSubject); err != nil {
		return mapError("wallet reversal: insert", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PAYG：发布配置 + 显式权益记录（裁决 6）
// ---------------------------------------------------------------------------

// PAYGConfig is the operator-published pay-as-you-go default: which policy
// version and explicit model set a customer opt-in receives.
type PAYGConfig struct {
	PolicyVersionID string
	ModelIDs        []string
	UpdatedBy       string
	UpdatedAt       time.Time
}

// GetPAYGConfig reads the singleton config; CodeNotFound = 未配置（不可开启）.
func (s *Store) GetPAYGConfig(ctx context.Context) (*PAYGConfig, error) {
	var row struct {
		ID              string         `db:"id"`
		PolicyVersionID string         `db:"policy_version_id"`
		ModelIDs        pq.StringArray `db:"model_ids"`
		UpdatedBy       string         `db:"updated_by"`
		UpdatedAt       time.Time      `db:"updated_at"`
	}
	if err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_payg_config WHERE id = 'default'`); err != nil {
		return nil, mapError("get payg config", err)
	}
	return &PAYGConfig{
		PolicyVersionID: row.PolicyVersionID, ModelIDs: []string(row.ModelIDs),
		UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt,
	}, nil
}

// getPAYGConfigTx is GetPAYGConfig on the caller's transaction — callers
// already holding a tx must read through it (评审轮1 M3：持 tx 期间用 s.db
// 第二连接读配置在池饱和时可死锁).
func (s *Store) getPAYGConfigTx(ctx context.Context, tx *sqlx.Tx) (*PAYGConfig, error) {
	var row struct {
		ID              string         `db:"id"`
		PolicyVersionID string         `db:"policy_version_id"`
		ModelIDs        pq.StringArray `db:"model_ids"`
		UpdatedBy       string         `db:"updated_by"`
		UpdatedAt       time.Time      `db:"updated_at"`
	}
	if err := tx.GetContext(ctx, &row,
		`SELECT * FROM inference_payg_config WHERE id = 'default'`); err != nil {
		return nil, mapError("get payg config", err)
	}
	return &PAYGConfig{
		PolicyVersionID: row.PolicyVersionID, ModelIDs: []string(row.ModelIDs),
		UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt,
	}, nil
}

// PutPAYGConfig publishes the PAYG default.已知残余竞态(评审轮4
// finding 4,可接受):published 校验不在策略行锁内,与并发 retire 交错
// 时配置可能落在刚退役的版本上——后果是下次开启 PAYG 被
// PolicyRetiredError 响亮拒绝(可见、可恢复),不会静默发放。The policy version must exist
// and be published (没有配置的商品保持不可购买，设计 §4.3).
func (s *Store) PutPAYGConfig(ctx context.Context, policyVersionID string, modelIDs []string, updatedBy string) (*PAYGConfig, error) {
	if updatedBy == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "payg config: updated_by required")
	}
	pol, err := s.GetPolicyVersion(ctx, policyVersionID)
	if err != nil {
		return nil, err
	}
	if pol.Status != "published" {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"payg config: policy version must be published (got "+pol.Status+")")
	}
	if modelIDs == nil {
		modelIDs = []string{}
	}
	var row struct {
		UpdatedAt time.Time `db:"updated_at"`
	}
	if err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_payg_config (id, policy_version_id, model_ids, updated_by)
		 VALUES ('default', $1, $2, $3)
		 ON CONFLICT (id) DO UPDATE
		 SET policy_version_id = EXCLUDED.policy_version_id,
		     model_ids = EXCLUDED.model_ids,
		     updated_by = EXCLUDED.updated_by, updated_at = now()
		 RETURNING updated_at`,
		policyVersionID, strArr(modelIDs), updatedBy).Scan(&row.UpdatedAt); err != nil {
		return nil, mapError("put payg config", err)
	}
	return &PAYGConfig{
		PolicyVersionID: policyVersionID, ModelIDs: modelIDs,
		UpdatedBy: updatedBy, UpdatedAt: row.UpdatedAt,
	}, nil
}

// EnsurePAYGEntitlementTx creates (or returns) the account's explicit PAYG
// entitlement from the published config (客户显式开启按量 → 显式权益记录).
// Idempotent: the UNIQUE(source_type, source_id, revision) key absorbs
// replays; a retired record is revived in place (同一消费主体). The caller
// holds the transaction; the account anchor lock is taken inside.
func (s *Store) EnsurePAYGEntitlementTx(ctx context.Context, w domain.UnitOfWork, accountID string, at time.Time) (*domain.Entitlement, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if err := lockAccountTx(ctx, tx, accountID); err != nil {
		return nil, err
	}
	// 评审轮1 M3：配置与既有权益都经同一 tx 读（不得用 s.db 第二连接——
	// 池饱和时第二连接等待即死锁）。
	cfg, err := s.getPAYGConfigTx(ctx, tx)
	if err != nil {
		return nil, err // CodeNotFound: 运营未发布 PAYG 配置 → 不可开启
	}
	srcID := accounting.PAYGEntitlementSourceID(accountID)
	existing, err := s.getLatestEntitlementBySourceTx(ctx, tx, domain.SourcePAYG, srcID)
	if err == nil {
		if existing.Status == domain.EntitlementActive {
			return existing, nil
		}
		// 曾被停用：原地复活（补丁只带乐观版本守卫；规格跟随当前配置由
		// 下次同步/运营修订处理——与 Task 10 收敛同口径，复活本身是修订）。
		return s.ReviveEntitlementTx(ctx, w, existing.ID, domain.EntitlementPatch{ExpectedRevision: existing.Revision})
	}
	if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	}
	ent, err := access.GrantFromPlan(access.PlanRevision{
		SourceType:      domain.SourcePAYG,
		SourceID:        srcID,
		ModelIDs:        cfg.ModelIDs,
		PolicyVersionID: cfg.PolicyVersionID,
	}, at, at, nil)
	if err != nil {
		return nil, err
	}
	ent.BillingAccountID = accountID
	// 评审轮2 N-1：INSERT ... ON CONFLICT DO NOTHING + 同 tx 读回赢家——
	// 不让 unique_violation 发生（一旦发生事务即 25P02 中止，同事务读回
	// 必败，并发开启的输家确定性 500）。
	return s.insertEntitlementOrReadWinnerTx(ctx, tx, ent)
}
