// overage.go — 显式套餐外消费 / PAYG 的纯门控规则（Task 14）。
//
// 裁决 4: 套餐外消费默认关——客户必须显式开启 + 设支出上限；未开启时套餐
// 耗尽即停（不动余额）。开启状态与上限的修改写 inference_wallet_audits。
// 裁决 6: 无套餐的 pay-as-you-go 需要显式 PAYG 权益记录（source_type=
// 'payg'），钱包扣费仍需通过本文件的支出门控。
//
// 支出上限按 UTC 自然月评估（MonthBoundsUTC），已支出口径 = 当月 consume
// 借方分录 − 当月对 consume 分录的冲正贷方（账本派生，无缓存计数器）。

package accounting

import (
	"github.com/yunhou/users/internal/inference/domain"
)

// OverageSettings is the customer-visible wallet spend gate, stored on the
// wallet row (per account + currency).
type OverageSettings struct {
	// Enabled: the customer explicitly opted into out-of-plan / PAYG
	// wallet spend (默认关).
	Enabled bool
	// MonthlySpendLimitMicros caps wallet consumption per UTC calendar
	// month; REQUIRED while enabled (开启必须设上限).
	MonthlySpendLimitMicros *int64
}

// Validate enforces "enabled ⇒ limit set" (migration 030 CHECK mirrors it).
func (s OverageSettings) Validate() error {
	if s.Enabled && s.MonthlySpendLimitMicros == nil {
		return domain.NewError(domain.CodeInvalidInput,
			"accounting: enabling overage requires a monthly spend limit (开启套餐外消费必须设支出上限)")
	}
	if s.MonthlySpendLimitMicros != nil && *s.MonthlySpendLimitMicros < 0 {
		return domain.WrapError(domain.CodeInvalidInput,
			"accounting: spend limit must be >= 0", domain.ErrNegativeValue)
	}
	return nil
}

// ErrOverageDisabled marks wallet spend attempted while the customer never
// enabled it — the caller keeps the plan's quota_exceeded as the
// customer-facing answer (未开启超额时不动余额).
var ErrOverageDisabled = domain.NewError(domain.CodeQuotaExceeded,
	"accounting: out-of-plan wallet spend is not enabled for this account (套餐外消费默认关)")

// ErrSpendLimitExceeded marks a hold that would push the UTC-month wallet
// spend past the configured limit.
var ErrSpendLimitExceeded = domain.NewError(domain.CodeQuotaExceeded,
	"accounting: monthly wallet spend limit reached")

// CheckWalletSpend gates ONE wallet hold against the overage settings and
// the month's already-spent amount. monthSpent is ledger-derived (consume
// debits net of their reversals within MonthBoundsUTC(now)); hold is the
// safe upper bound about to be frozen. Pure — the repo supplies the sums
// under the wallet row lock.
func CheckWalletSpend(s OverageSettings, monthSpent, hold int64) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Enabled {
		return ErrOverageDisabled
	}
	if hold < 0 || monthSpent < 0 {
		return domain.WrapError(domain.CodeInvalidInput, "accounting: negative spend figure", domain.ErrNegativeValue)
	}
	if monthSpent+hold > *s.MonthlySpendLimitMicros {
		return domain.WrapError(domain.CodeQuotaExceeded,
			"accounting: monthly spend limit would be exceeded", ErrSpendLimitExceeded)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 设置变更审计（裁决 4: 开启状态与上限的修改要审计）
// ---------------------------------------------------------------------------

// Overage audit actions (mirror the inference_wallet_audits CHECK).
const (
	WalletAuditCreate         = "create"
	WalletAuditEnableOverage  = "enable_overage"
	WalletAuditDisableOverage = "disable_overage"
	WalletAuditSetSpendLimit  = "set_spend_limit"
)

// OverageAuditSpec is one settings-change audit row. Old/New carry the
// pre/post state; the repo writes it in the SAME transaction as the change.
type OverageAuditSpec struct {
	Action         string
	ChangedBy      string
	OldEnabled     *bool
	NewEnabled     *bool
	OldLimitMicros *int64
	NewLimitMicros *int64
}

// AuditForChange computes the audit rows for one settings transition; a
// create emits its own row. No-op transitions (same enabled + same limit)
// emit nothing — but the WALLET row write is still idempotent.
func AuditForChange(exists bool, old OverageSettings, new OverageSettings, changedBy string) []OverageAuditSpec {
	var out []OverageAuditSpec
	if !exists {
		en, lim := new.Enabled, new.MonthlySpendLimitMicros
		out = append(out, OverageAuditSpec{
			Action: WalletAuditCreate, ChangedBy: changedBy,
			NewEnabled: &en, NewLimitMicros: lim,
		})
	}
	if old.Enabled != new.Enabled {
		action := WalletAuditEnableOverage
		if !new.Enabled {
			action = WalletAuditDisableOverage
		}
		oldEn, newEn := old.Enabled, new.Enabled
		out = append(out, OverageAuditSpec{
			Action: action, ChangedBy: changedBy,
			OldEnabled: &oldEn, NewEnabled: &newEn,
			OldLimitMicros: old.MonthlySpendLimitMicros, NewLimitMicros: new.MonthlySpendLimitMicros,
		})
	}
	if !sameInt64Ptr(old.MonthlySpendLimitMicros, new.MonthlySpendLimitMicros) {
		out = append(out, OverageAuditSpec{
			Action: WalletAuditSetSpendLimit, ChangedBy: changedBy,
			OldEnabled: ptrBool(old.Enabled), NewEnabled: ptrBool(new.Enabled),
			OldLimitMicros: old.MonthlySpendLimitMicros, NewLimitMicros: new.MonthlySpendLimitMicros,
		})
	}
	return out
}

func sameInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ptrBool(v bool) *bool { return &v }

// PAYGEntitlementSourceID is the idempotent source id of one account's
// explicit pay-as-you-go record: one account has at most one PAYG
// entitlement lineage (UNIQUE(source_type, source_id, revision)).
func PAYGEntitlementSourceID(billingAccountID string) string { return "payg:" + billingAccountID }
