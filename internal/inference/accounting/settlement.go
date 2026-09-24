package accounting

import (
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// settlement.go — 结算决策的纯规则（Task 9，设计 §7.2 补充段"估算为主、
// 核对为例外"）。无数据库、无时钟：
//
//   - Decide：规范路径——重叠语义归一 + 钉版计价 + 预占超出检测。网关与
//     恢复 worker 共用同一份规则，不会两处口径漂移。
//   - ConservativeRecord：崩溃恢复的保守估算——以准入时持久化的安全上界
//     入账（charge = 预占额），可审计（source=estimated + raw 载明依据）、
//     可被后续真实证据冲正；永不记零、永不静默免费。
//   - Correction：估算被真实证据修正时的冲正/补差纯规则（保留原记录，
//     追加 reversal/adjustment，不重写历史，设计 §7.3）。

// Decision is the priced outcome of ONE settlement under the pinned price
// version, plus its relation to the reserved hold.
type Decision struct {
	// Record is the normalized usage fact to persist (buckets already
	// disjoint — BillableBuckets applied exactly once).
	Record domain.UsageRecord
	// Charge is the priced customer consumption.
	Charge *Charge
	// ReservedMicros is the hold this settlement converts.
	ReservedMicros domain.Microcredit
}

// OverageMicros is the part of the actual charge EXCEEDING the reserved
// hold (设计 §7.2/Task 9: 超出预占的实际费用显示异常并阻止继续透支，不隐
// 藏负差额). Zero when the charge fits the hold. The charge itself is NEVER
// clamped to the hold — clamping would silently free real consumption.
func (d *Decision) OverageMicros() domain.Microcredit {
	if d.Charge == nil {
		return 0
	}
	// Unit-agnostic: credit path carries Credit, wallet path carries Money
	// (Task 14); ReservedMicros is denominated in the same unit as the
	// charge by construction (admission fixed the source).
	charged := d.Charge.Credit
	if d.Charge.Money != nil {
		charged = domain.Microcredit(d.Charge.Money.Micros)
	}
	if charged <= d.ReservedMicros {
		return 0
	}
	return charged - d.ReservedMicros
}

// Decide prices one settlement: normalize overlapping semantics exactly
// once, then quote under the pinned sale_credit price version. Unknown
// usage refuses to price (ErrUnknownUsage) — it is held for reconciliation,
// never charged nor zeroed.
//
// 已知限制（本期口径，extras 计费属 Phase 2）：Decide 无 extras 参数，
// Quote 恒以 nil extras 计价，即本期结算不含工具等 extras 的实际用量；
// 而入场（quota/reservation.go）会把 ExtraBounds 计入预占并对无费率
// extras 拒绝。两侧口径不对称：入场预留是保守上界，结算按实际 token
// 桶计。因此接入 extras 计费通道之前不应为模型配置 ExtraRates——否则
// 平台永远收不到工具实际使用费，且客户每单多预留。
func Decide(usage domain.UsageRecord, price PriceVersion, inc Inclusion, reserved domain.Microcredit) (*Decision, error) {
	billable, err := BillableBuckets(usage.Buckets, inc)
	if err != nil {
		return nil, err
	}
	record := usage
	record.Buckets = billable
	charge, err := price.Quote(record, nil)
	if err != nil {
		return nil, err
	}
	return &Decision{Record: record, Charge: charge, ReservedMicros: reserved}, nil
}

// Bounds are the admission-time safe upper bounds persisted on the request
// row (migration 031): the recovery estimate's audit basis.
type Bounds struct {
	InputBoundTokens int64
	OutputCapTokens  int64
	ExtraBounds      map[string]int64
}

// BoundsOf extracts the persisted admission bounds; nil columns (rows that
// predate migration 031) yield false — the caller must not fabricate token
// buckets, though the reserved amount still prices the conservative charge.
func BoundsOf(r *domain.Request) (Bounds, bool) {
	if r.InputBoundTokens == nil || r.OutputCapTokens == nil {
		return Bounds{}, false
	}
	return Bounds{
		InputBoundTokens: *r.InputBoundTokens,
		OutputCapTokens:  *r.OutputCapTokens,
		ExtraBounds:      r.ExtraBounds,
	}, true
}

// recoveryRaw is the schema-versioned raw_usage payload of a recovery
// estimate (no prompts/completions, 设计 §7.1): it records WHY the estimate
// is what it is, so a later correction has the full audit trail.
type recoveryRaw struct {
	SchemaVersion int               `json:"schema_version"`
	Recovery      recoveryBasisJSON `json:"recovery"`
}

type recoveryBasisJSON struct {
	Basis            string           `json:"basis"` // conservative_bounds
	Reason           string           `json:"reason"`
	InputBoundTokens *int64           `json:"input_bound_tokens,omitempty"`
	OutputCapTokens  *int64           `json:"output_cap_tokens,omitempty"`
	ExtraBounds      map[string]int64 `json:"extra_bounds,omitempty"`
	ChargeMicros     int64            `json:"charge_micros"` // = reserved hold
}

// ConservativeRecord builds the crash-recovery usage fact: source=estimated
// with the admission bounds as buckets (when persisted). The settlement
// charge for this record is the RESERVED amount itself — the safe upper
// bound the customer already had held. Charging the full hold for an
// execution whose outcome is unknown is the design's conservative choice
// (设计 §7.2: 禁止仅凭 TTL 释放全部预占；估算须可审计并可被后续真实证据冲
// 正): never silently free, always correctable via CorrectSettlement.
func ConservativeRecord(requestID, attemptID string, req *domain.Request, reason string, revision int) domain.UsageRecord {
	record := domain.UsageRecord{
		RequestID: requestID, AttemptID: attemptID,
		Source: domain.UsageEstimated, Revision: revision,
	}
	basis := recoveryBasisJSON{Basis: "conservative_bounds", Reason: reason}
	if req.ReservedMicros != nil {
		basis.ChargeMicros = int64(*req.ReservedMicros)
	}
	if b, ok := BoundsOf(req); ok {
		in, out := b.InputBoundTokens, b.OutputCapTokens
		record.Buckets = domain.UsageBuckets{InputTokens: &in, OutputTokens: &out}
		basis.InputBoundTokens = &in
		basis.OutputCapTokens = &out
		if len(b.ExtraBounds) > 0 {
			basis.ExtraBounds = b.ExtraBounds
		}
	}
	raw, err := json.Marshal(recoveryRaw{SchemaVersion: 1, Recovery: basis})
	if err == nil {
		record.RawUsage = domain.ExtensionConfig{SchemaVersion: 1, Raw: raw}
	}
	return record
}

// CorrectionDirection computes the signed delta of a settlement correction
// (设计 §7.3: 冲正/补差分录). The ledger stays append-only: the original
// charge is reversed (reversal entry, amount = original) and the delta is
// appended as a debit/credit adjustment. A zero delta is a non-correction
// and rejects — never append empty history.
func CorrectionDelta(original, corrected domain.Microcredit) (direction string, amount domain.Microcredit, err error) {
	switch {
	case corrected < 0:
		return "", 0, domain.WrapError(domain.CodeInvalidInput,
			"accounting: corrected amount must be >= 0", domain.ErrNegativeValue)
	case corrected > original:
		return "debit", corrected - original, nil
	case corrected < original:
		return "credit", original - corrected, nil
	default:
		return "", 0, domain.NewError(domain.CodeInvalidInput,
			"accounting: correction equals the original charge (no-op corrections are not appended)")
	}
}

// SettlementLag measures how long the oldest unfinished request has been
// waiting — a pure helper so the metric's meaning is testable.
func SettlementLag(now time.Time, oldestOpenUpdatedAt *time.Time) time.Duration {
	if oldestOpenUpdatedAt == nil {
		return 0
	}
	d := now.Sub(oldestOpenUpdatedAt.UTC())
	if d < 0 {
		return 0
	}
	return d
}
