// wallet.go — 预付按量钱包的纯规则（Task 14，设计 §7.1/§7.3 钱包章节）。
//
// 本文件不碰数据库：分录不变式（与 migration 030 的 CHECK 逐条对齐）、
// 现金/赠送来源拆分、账本派生余额、冲正配对、支付边界的十进制→微金额
// 换算。持久化在 internal/inference/postgres/wallet_repo.go。
//
// 口径（控制者裁决）:
//   - 定点整数微金额 + 币种隔离；借贷分录追加 + 冲正，客户展示 = 账本派生，
//     禁止独立缓存余额。
//   - 分录区分现金(cash)/赠送(bonus)；退款只允许现金来源原路退。
//   - 冻结/释放是 hold 行状态迁移（与额度预占同口径，不是账本行）；
//     消费/退款/调整/冲正才是账本行，全部带幂等业务键。

package accounting

import (
	"strconv"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// WalletSource distinguishes the origin of wallet funds (Task 14 裁决 7:
// 分录须区分 cash/bonus；退款只允许现金来源原路退).
type WalletSource string

const (
	// WalletCash is paid-in money — the ONLY refundable source.
	WalletCash WalletSource = "cash"
	// WalletBonus is gifted/promotional balance — spendable, never
	// cash-refundable (赠送余额不得伪装现金退款).
	WalletBonus WalletSource = "bonus"
)

// WalletEntryType mirrors the CHECK on inference_wallet_entries.
type WalletEntryType string

const (
	// EntryTopup credits cash from a settled payment (business key
	// wallet:topup:{payment_id}).
	EntryTopup WalletEntryType = "topup"
	// EntryBonus credits gifted balance (运营赠送/补偿).
	EntryBonus WalletEntryType = "bonus"
	// EntryConsume debits a settled request charge (per source, business
	// key wallet:consume:{request_id}:{source}).
	EntryConsume WalletEntryType = "consume"
	// EntryRefund debits cash back out of the platform (原路退;
	// business key wallet:refund:{refund_id}).
	EntryRefund WalletEntryType = "refund"
	// EntryAdjustment is an operator correction (either direction, either
	// source; linked to inference_adjustments).
	EntryAdjustment WalletEntryType = "adjustment"
	// EntryReversal pairs one earlier entry with the opposite direction
	// (账本采用追加记录及冲正，不重写已结算事实).
	EntryReversal WalletEntryType = "reversal"
)

// WalletDirection mirrors the direction CHECK.
type WalletDirection string

const (
	DirDebit  WalletDirection = "debit"
	DirCredit WalletDirection = "credit"
)

// Idempotent business keys (控制者裁决 3: 充值/冻结/消费/释放/退款/冲正都写
// 幂等业务键；冻结/释放的业务键是 hold 行的 request_id 唯一键).
func WalletTopupKey(paymentID string) string { return "wallet:topup:" + paymentID }
func WalletRefundKey(refundID string) string { return "wallet:refund:" + refundID }
func WalletConsumeKey(requestID string, source WalletSource) string {
	return "wallet:consume:" + requestID + ":" + string(source)
}
func WalletAdjustmentKey(idempotencyKey string) string { return "wallet:adjustment:" + idempotencyKey }
func WalletReversalKey(entryID int64) string {
	return "wallet:reversal:" + strconv.FormatInt(entryID, 10)
}

// WalletEntrySpec is one wallet ledger entry before persistence. Amount is
// always positive; direction carries the sign.
type WalletEntrySpec struct {
	Type      WalletEntryType
	Direction WalletDirection
	Source    WalletSource
	Amount    domain.Money
}

// ErrBonusNotRefundable rejects a refund drawn on gifted balance (裁决 7).
var ErrBonusNotRefundable = domain.NewError(domain.CodeInvalidInput,
	"accounting: bonus balance is not cash-refundable (赠送余额不得伪装现金退款)")

// ErrInsufficientBalance rejects a freeze that exceeds the derived
// available balance (裁决 8: 不超扣).
var ErrInsufficientBalance = domain.NewError(domain.CodeInsufficientBalance,
	"accounting: wallet balance insufficient for the hold")

// ValidateWalletEntry enforces the invariants migration 030 mirrors as
// CHECK constraints — the app layer fails fast with domain errors instead
// of surfacing driver check violations.
func ValidateWalletEntry(e WalletEntrySpec) error {
	if e.Amount.Micros <= 0 {
		return domain.WrapError(domain.CodeInvalidInput, "accounting: wallet entry amount must be > 0", domain.ErrNegativeValue)
	}
	switch e.Source {
	case WalletCash, WalletBonus:
	default:
		return domain.NewError(domain.CodeInvalidInput, "accounting: unknown wallet source "+string(e.Source))
	}
	switch e.Type {
	case EntryTopup:
		if e.Direction != DirCredit || e.Source != WalletCash {
			return domain.NewError(domain.CodeInvalidInput, "accounting: topup must credit cash")
		}
	case EntryBonus:
		if e.Direction != DirCredit || e.Source != WalletBonus {
			return domain.NewError(domain.CodeInvalidInput, "accounting: bonus must credit bonus source")
		}
	case EntryConsume:
		if e.Direction != DirDebit {
			return domain.NewError(domain.CodeInvalidInput, "accounting: consume must debit")
		}
	case EntryRefund:
		if e.Direction != DirDebit || e.Source != WalletCash {
			return ErrBonusNotRefundable
		}
	case EntryAdjustment, EntryReversal:
		// either direction / either source (reversal inherits the original's).
	default:
		return domain.NewError(domain.CodeInvalidInput, "accounting: unknown wallet entry type "+string(e.Type))
	}
	return nil
}

// ReversalOf builds the reversal spec pairing an earlier entry: same
// source and amount, opposite direction (追加 + 冲正，不重写已结算事实).
func ReversalOf(orig WalletEntrySpec) (WalletEntrySpec, error) {
	if orig.Type == EntryReversal {
		return WalletEntrySpec{}, domain.NewError(domain.CodeInvalidInput,
			"accounting: a reversal cannot be reversed (correct it with an adjustment)")
	}
	rev := WalletEntrySpec{Type: EntryReversal, Source: orig.Source, Amount: orig.Amount}
	if orig.Direction == DirDebit {
		rev.Direction = DirCredit
	} else {
		rev.Direction = DirDebit
	}
	return rev, nil
}

// ---------------------------------------------------------------------------
// 冻结/结算拆分：赠送先扣
// ---------------------------------------------------------------------------

// SplitFreeze fixes the cash/bonus composition of one hold at freeze time:
// bonus first, then cash (赠送优先消耗，现金留作可退). The composition is
// persisted on the hold row; settlement and release follow it exactly.
// available figures are the DERIVED balances (ledger sums minus held).
// A derived balance can legitimately be NEGATIVE (e.g. a reversal of an
// already-consumed bonus credit, or a cash refund landing after the funds
// were spent — 不隐藏负差额); the split must then clamp the negative side
// to zero instead of producing a negative component that violates the hold
// row CHECK and hard-blocks every admission with a 400 (评审轮1 I1). The
// negative deficit stays visible in the derived balance and is filled by
// later credits; the total-availability gate below already accounts it
// (negative bonus reduces what can be frozen from cash).
func SplitFreeze(bonusAvailable, cashAvailable, amount int64) (cash, bonus int64, err error) {
	if amount <= 0 {
		return 0, 0, domain.WrapError(domain.CodeInvalidInput, "accounting: freeze amount must be > 0", domain.ErrNegativeValue)
	}
	if bonusAvailable+cashAvailable < amount {
		return 0, 0, ErrInsufficientBalance
	}
	if bonusAvailable < 0 {
		bonusAvailable = 0
	}
	if cashAvailable < 0 {
		cashAvailable = 0
	}
	bonus = min(amount, bonusAvailable)
	cash = amount - bonus
	return cash, bonus, nil
}

// SplitConsume maps the settled charge onto the hold's frozen composition:
// bonus first within the hold, then the hold's cash; any over-hold excess
// (charge > hold, the Task 9 超占 anomaly) debits CASH beyond the freeze —
// an honest overdraft that drives the derived balance negative and blocks
// further freezes (不隐藏负差额), never silently freed.
func SplitConsume(holdCash, holdBonus, charge int64) (consumeCash, consumeBonus, extraCash int64, err error) {
	if charge < 0 {
		return 0, 0, 0, domain.WrapError(domain.CodeInvalidInput, "accounting: negative charge", domain.ErrNegativeValue)
	}
	consumeBonus = min(charge, holdBonus)
	rem := charge - consumeBonus
	consumeCash = min(rem, holdCash)
	extraCash = rem - consumeCash
	return consumeCash, consumeBonus, extraCash, nil
}

// SplitRelease returns the unfrozen remainder per source. consumeCash must
// be the within-hold cash consumption (SplitConsume's consumeCash — the
// over-hold extraCash was never frozen and releases nothing).
func SplitRelease(holdCash, holdBonus, consumeCash, consumeBonus int64) (releaseCash, releaseBonus int64, err error) {
	if consumeCash > holdCash || consumeBonus > holdBonus || consumeCash < 0 || consumeBonus < 0 {
		return 0, 0, domain.NewError(domain.CodeInvalidInput,
			"accounting: consumed split exceeds the frozen composition")
	}
	return holdCash - consumeCash, holdBonus - consumeBonus, nil
}

// ---------------------------------------------------------------------------
// 账本派生余额（客户展示 = 本函数输出；无缓存列）
// ---------------------------------------------------------------------------

// WalletSums is the aggregated ledger state of one wallet: per-source
// credit/debit sums plus the per-source currently-held split.
type WalletSums struct {
	CashCredit  int64
	CashDebit   int64
	BonusCredit int64
	BonusDebit  int64
	HeldCash    int64
	HeldBonus   int64
}

// WalletBalance is the derived customer-facing balance. Cash figures may go
// negative when a cash refund lands after the funds were spent (honest
// ledger, 不隐藏负差额); availability gates further freezes.
type WalletBalance struct {
	Currency       string
	CashAvailable  int64
	BonusAvailable int64
	CashHeld       int64
	BonusHeld      int64
}

// DeriveWalletBalance computes available = net − held per source. Held
// never exceeds net by construction (freezes check availability under the
// wallet row lock), but a cash refund landing while funds are frozen can
// drive cashAvailable negative — that is the truth and must show as-is.
func DeriveWalletBalance(currency string, s WalletSums) (WalletBalance, error) {
	if _, err := domain.NewMoney(0, currency); err != nil {
		return WalletBalance{}, err
	}
	return WalletBalance{
		Currency:       currency,
		CashAvailable:  s.CashCredit - s.CashDebit - s.HeldCash,
		BonusAvailable: s.BonusCredit - s.BonusDebit - s.HeldBonus,
		CashHeld:       s.HeldCash,
		BonusHeld:      s.HeldBonus,
	}, nil
}

// ---------------------------------------------------------------------------
// 支付边界换算：十进制主单位 → 微金额（严格转换，设计 §7.1）
// ---------------------------------------------------------------------------

// MicrosPerMajor is the fixed-point scale of the wallet ledger
// (1 unit of currency = 1_000_000 micros).
const MicrosPerMajor = int64(1_000_000)

// MicrosFromDecimalMajor converts a non-negative decimal literal in MAJOR
// units ("29.99") into integer micros of `currency`. The conversion is
// exact rational arithmetic; a literal with more than 6 fractional digits
// (sub-micro precision) or a negative value rejects — 支付边界保留现有对
// 外金额契约，进入模型账本时严格转换.
func MicrosFromDecimalMajor(dec string, currency string) (int64, error) {
	if _, err := domain.NewMoney(0, currency); err != nil {
		return 0, err
	}
	rate, err := domain.ParseDecimal(dec)
	if err != nil {
		return 0, err
	}
	micros, err := rate.MicrosForUnits(MicrosPerMajor, domain.RoundDown)
	if err != nil {
		return 0, err
	}
	// Reject sub-micro fractions: exactness check via RoundUp comparison.
	up, err := rate.MicrosForUnits(MicrosPerMajor, domain.RoundUp)
	if err != nil {
		return 0, err
	}
	if up != micros {
		return 0, domain.NewError(domain.CodeInvalidInput,
			"accounting: amount "+dec+" has sub-micro precision; cannot convert exactly")
	}
	return micros, nil
}

// MonthBoundsUTC returns the UTC calendar-month window containing t
// ([start, end)); the wallet monthly spend limit is evaluated over it.
func MonthBoundsUTC(t time.Time) (start, end time.Time) {
	u := t.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}
