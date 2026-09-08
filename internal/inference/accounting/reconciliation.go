package accounting

import (
	"github.com/yunhou/users/internal/inference/domain"
)

// reconciliation.go — 由账本重建聚合值的纯规则（Task 9，设计 §7.3：
// 账本采用追加记录及冲正；验收：由账本重建聚合值可与窗口核对）。
//
// 重建口径：窗口 used_micros ≡ 绑定到该窗口的全部请求在账本上的净额 —
//   Σ charge − Σ reversal + Σ 请求级 adjustment（debit + / credit −）。
// 只计 unit='microcredit' 的分录（额度窗口；金额口径与账户级调整不影响窗口）。
// 估算修正（reversal + adjustment delta）与窗口 used 的同事务校正
// （CorrectSettlement）保证该等式在修正后依然成立。

// LedgerFact is one ledger entry's contribution to the rebuild: charge
// adds, reversal subtracts, a request-linked adjustment adds (debit) or
// subtracts (credit).
type LedgerFact struct {
	Type         domain.LedgerEntryType
	AmountMicros int64
	// Direction matters only for adjustment entries ("debit" | "credit").
	Direction string
}

// RebuildUsedMicros rebuilds a window's used aggregate from ledger facts.
// The result is clamped at ≥ 0 only in the sense that inputs are validated:
// a negative rebuild indicates ledger corruption and is returned as-is so
// the caller flags a mismatch (never silently repaired).
func RebuildUsedMicros(facts []LedgerFact) (int64, error) {
	var total int64
	for _, f := range facts {
		if f.AmountMicros < 0 {
			return 0, domain.WrapError(domain.CodeInvalidInput,
				"accounting: negative ledger amount in rebuild", domain.ErrNegativeValue)
		}
		switch f.Type {
		case domain.LedgerCharge:
			total += f.AmountMicros
		case domain.LedgerReversal:
			total -= f.AmountMicros
		case domain.LedgerAdjustment:
			switch f.Direction {
			case "debit":
				total += f.AmountMicros
			case "credit":
				total -= f.AmountMicros
			default:
				return 0, domain.NewError(domain.CodeInvalidInput,
					"accounting: adjustment entry without direction in rebuild")
			}
		default:
			return 0, domain.NewError(domain.CodeInvalidInput,
				"accounting: unknown ledger entry type "+string(f.Type))
		}
	}
	return total, nil
}

// WindowReconciliation compares one window's stored used aggregate with the
// ledger-rebuilt value.
type WindowReconciliation struct {
	WindowID    string
	Kind        domain.WindowKind
	StoredUsed  int64
	RebuiltUsed int64
}

// Diff is rebuilt − stored; non-zero is a ledger mismatch (差异进
// reconciliation_jobs — never auto-repaired, 账本不重写).
func (r WindowReconciliation) Diff() int64 { return r.RebuiltUsed - r.StoredUsed }

// Mismatches filters the windows whose stored aggregate disagrees with the
// ledger rebuild.
func Mismatches(all []WindowReconciliation) []WindowReconciliation {
	var out []WindowReconciliation
	for _, r := range all {
		if r.Diff() != 0 {
			out = append(out, r)
		}
	}
	return out
}
