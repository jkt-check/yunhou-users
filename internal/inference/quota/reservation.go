package quota

import (
	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// reservation.go — 预占金额安全上界的纯规则（Task 7，设计 §7.2：预占按
// 输入安全上界、强制输出上限和其他可计费项目的上界计算。不允许无限输出）。
// 全部为确定性整数/有理数运算：无数据库、无时钟、无 float。

// ReserveBounds are the per-request upper bounds a safe reservation is
// computed from. Every bound is a hard ceiling, never an estimate that the
// upstream could exceed:
//
//   - InputBoundTokens: safe upper bound of billable input tokens —
//     min(estimated prompt tokens, model context hard limit). Cache buckets
//     are bounded by the same input ceiling at their own (cheaper) rates,
//     so input+read+write can never exceed context tokens in total.
//   - OutputCapTokens: the FORCED output ceiling — unlimited output is not
//     sellable (设计 §7.2: 不允许无限输出；无法约束成本的能力先不开放硬预算
//     售卖). See EffectiveOutputCap.
//   - ExtraBounds: upper bounds of other billable items (e.g. high-cost
//     tool calls). Every item with a positive bound MUST have a configured
//     rate in the price version — otherwise the capability is unpriced and
//     the request rejects (CodeUnpricedCapability, 校验高成本工具等额外计
//     费项目上界).
type ReserveBounds struct {
	InputBoundTokens int64
	OutputCapTokens  int64
	ExtraBounds      map[string]int64
}

// EffectiveOutputCap derives the output ceiling forwarded to the upstream
// and used for the reservation:
//
//   - the model's hard MaxOutputTokens is the absolute ceiling; a model
//     without one (<= 0) cannot bound cost and is rejected — selling a hard
//     budget against unbounded output is forbidden (设计 §7.2);
//   - a client-declared cap above the model ceiling narrows to the model
//     ceiling (the upstream physically cannot emit more);
//   - no client cap → the model ceiling applies (forced cap, 不允许无限输出).
//
// This is NOT the §7.2 quota clamp (下调预占需客户端显式声明) — it is the
// protocol-level output bound every admission must have before quota is
// even evaluated.
func EffectiveOutputCap(clientMaxTokens *int64, modelMaxOutputTokens int) (int64, error) {
	if modelMaxOutputTokens <= 0 {
		return 0, domain.NewError(domain.CodeInvalidInput,
			"quota: model has no hard output limit — unbounded output cannot be reserved (不允许无限输出)")
	}
	cap := int64(modelMaxOutputTokens)
	if clientMaxTokens != nil {
		if *clientMaxTokens <= 0 {
			return 0, domain.WrapError(domain.CodeInvalidInput,
				"quota: client max_tokens must be > 0", domain.ErrNegativeValue)
		}
		if *clientMaxTokens < cap {
			cap = *clientMaxTokens
		}
	}
	return cap, nil
}

// InputBound derives the safe input upper bound: min(estimated prompt
// tokens, model context hard limit). A model without a positive context
// limit cannot bound its input and rejects. The estimate itself must be
// non-negative.
func InputBound(estimatedPromptTokens int64, modelContextTokens int) (int64, error) {
	if modelContextTokens <= 0 {
		return 0, domain.NewError(domain.CodeInvalidInput,
			"quota: model has no context limit — input cannot be bounded")
	}
	if estimatedPromptTokens < 0 {
		return 0, domain.WrapError(domain.CodeInvalidInput,
			"quota: negative prompt estimate", domain.ErrNegativeValue)
	}
	bound := estimatedPromptTokens
	if c := int64(modelContextTokens); bound > c {
		bound = c
	}
	return bound, nil
}

// ReserveAmount prices the safe upper bound of ONE consumption in
// microcredits under the pinned sale_credit price version (设计 §7.2: 预占
// 按输入安全上界 + 强制输出上限 + 其他可计费项目上界). Rounding is RoundUp
// per line — the reservation never under-covers the exact rational bound.
// Overflow rejects rather than wrapping.
//
// The same amount mirrors into every applicable target (three windows +
// Key budget); the customer is still settled exactly once (设计 §6).
func ReserveAmount(price accounting.PriceVersion, b ReserveBounds) (domain.Microcredit, error) {
	if price.Kind != accounting.PriceSaleCredit {
		return 0, domain.NewError(domain.CodeInvalidInput,
			"quota: reservation must be priced from the sale_credit list, got "+string(price.Kind))
	}
	if b.InputBoundTokens < 0 || b.OutputCapTokens <= 0 {
		return 0, domain.WrapError(domain.CodeInvalidInput,
			"quota: bounds must be input >= 0 and output cap > 0", domain.ErrNegativeValue)
	}
	var total domain.Microcredit
	add := func(units int64, rate domain.Rate) error {
		m, err := rate.ChargeUnits(units, domain.RoundUp)
		if err != nil {
			return err
		}
		sum, err := total.Add(m)
		if err != nil {
			return err
		}
		total = sum
		return nil
	}
	// Cache read/write are bounded by the same input ceiling: any cached
	// token is also an input token, so bound×(input+read+write rates) is a
	// safe upper bound of every cache combination.
	for _, line := range []struct {
		units int64
		rate  domain.Rate
	}{
		{b.InputBoundTokens, price.Input},
		{b.InputBoundTokens, price.CacheRead},
		{b.InputBoundTokens, price.CacheWrite},
		{b.OutputCapTokens, price.Output},
	} {
		if err := add(line.units, line.rate); err != nil {
			return 0, err
		}
	}
	for name, units := range b.ExtraBounds {
		if units < 0 {
			return 0, domain.WrapError(domain.CodeInvalidInput,
				"quota: negative extra bound for "+name, domain.ErrNegativeValue)
		}
		rate, ok := price.ExtraRates[name]
		if !ok {
			if units > 0 {
				return 0, domain.NewError(domain.CodeUnpricedCapability,
					"quota: billable item "+name+" has no rate in the pinned price version (高成本项目无上界价目)")
			}
			continue
		}
		if err := add(units, rate); err != nil {
			return 0, err
		}
	}
	if total <= 0 {
		return 0, domain.NewError(domain.CodeInvalidInput,
			"quota: reservation amount must be > 0 (zero-priced bound would admit unbounded free load)")
	}
	return total, nil
}
