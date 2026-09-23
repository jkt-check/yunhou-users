// Package accounting holds the pure pricing rules of the inference module
// (设计 §7.1, Task 6): three independently versioned price lists (customer
// credit / pay-as-you-go sale / upstream cost), token-overlap
// normalization, and explicit rounding positions. No database, no wall
// clock — persistence lives in internal/inference/postgres.
package accounting

import (
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// PriceKind enumerates the three independently versioned price lists
// (设计 §7.1: 客户售价、额度消耗表、采购价格分别版本化).
type PriceKind string

const (
	// PriceSaleCredit prices usage in microcredits — the quota consumption
	// table a plan charge draws against.
	PriceSaleCredit PriceKind = "sale_credit"
	// PriceSaleMoney is the pay-as-you-go sale price (micromoney +
	// currency); 首期默认不启用（设计 §4.3）, 但价目与规则在此定义.
	PriceSaleMoney PriceKind = "sale_money"
	// PriceUpstreamCost is the procurement cost list (micromoney +
	// currency).
	PriceUpstreamCost PriceKind = "upstream_cost"
)

// ErrUnknownUsage rejects pricing of usage whose source is unknown: an
// unknown consumption is never charged nor zeroed — the reservation is
// held and the request enters reconciliation (设计 §7.2: 不记零、不重复扣;
// §7.1: estimated/unknown 与 reported 分开).
var ErrUnknownUsage = domain.NewError(domain.CodeInvalidInput,
	"accounting: usage unknown — cannot price; hold the reservation and reconcile")

// ErrUnpriced rejects a chargeable capability with no effective price
// version (设计 §7.1/Task 6: 拒绝未定价且需扣费的能力).
var ErrUnpriced = domain.NewError(domain.CodeUnpricedCapability,
	"accounting: no effective price version for a chargeable capability")

// RoundingFor pins the rounding direction at the point a rational price
// materializes into integer micro-units (设计 §7.1: 明确取整位置和方向):
//
//   - customer-facing lists (sale_credit, sale_money) round UP — the
//     customer never pays less than the exact rational price;
//   - upstream_cost rounds DOWN — a recorded cost never overstates the
//     exact rational estimate.
func RoundingFor(kind PriceKind) domain.RoundingMode {
	if kind == PriceUpstreamCost {
		return domain.RoundDown
	}
	return domain.RoundUp
}

// PriceVersion is the pure shape of one immutable price revision. Rates
// are exact rationals in micro-units of the list's unit (microcredit, or
// micromoney of Currency) per token. A revision is immutable once stored:
// publishing a new price NEVER re-prices an already-pinned request
// (设计 §7.1: 运营修改价格只影响生效后的请求).
type PriceVersion struct {
	ID       string
	ModelID  string
	Kind     PriceKind
	Currency string // ISO-4217 for money lists; empty for sale_credit
	// Per-token exact rates; storage carries micro-units per 1M tokens and
	// the repo converts via domain.RatePerMillion.
	Input      domain.Rate
	CacheRead  domain.Rate
	CacheWrite domain.Rate
	Output     domain.Rate
	// ExtraRates prices other billable items (tools etc.) per single unit;
	// decoded from the schema-versioned extra_rates document.
	ExtraRates    map[string]domain.Rate
	Revision      int
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
}

// Validate enforces the price-version invariants the DB CHECKs mirror.
func (p *PriceVersion) Validate() error {
	switch p.Kind {
	case PriceSaleCredit:
		if p.Currency != "" {
			return domain.NewError(domain.CodeInvalidInput, "accounting: credit price list must not carry a currency")
		}
	case PriceSaleMoney, PriceUpstreamCost:
		if _, err := domain.NewMoney(0, p.Currency); err != nil {
			return domain.WrapError(domain.CodeInvalidInput, "accounting: money price list needs a valid currency", err)
		}
	default:
		return domain.NewError(domain.CodeInvalidInput, "accounting: unknown price kind "+string(p.Kind))
	}
	if p.Revision <= 0 {
		return domain.NewError(domain.CodeInvalidInput, "accounting: revision must be > 0")
	}
	if p.EffectiveTo != nil && !p.EffectiveTo.After(p.EffectiveFrom) {
		return domain.NewError(domain.CodeInvalidInput, "accounting: effective range must be (from, to] with to > from")
	}
	return nil
}

// EffectiveAt reports [from, to) containment — a price change at the exact
// boundary instant belongs to the NEW revision.
func (p *PriceVersion) EffectiveAt(at time.Time) bool {
	t := at.UTC()
	if t.Before(p.EffectiveFrom.UTC()) {
		return false
	}
	return p.EffectiveTo == nil || t.Before(p.EffectiveTo.UTC())
}

// ResolvePrice picks the highest revision of `kind` effective at `at`
// ([from, to) semantics). The three kinds are versioned independently;
// callers resolve each list separately. No effective revision → ErrUnpriced
// (未定价且需扣费的能力必须拒绝).
func ResolvePrice(versions []PriceVersion, kind PriceKind, at time.Time) (*PriceVersion, error) {
	var best *PriceVersion
	for i := range versions {
		v := &versions[i]
		if v.Kind != kind || !v.EffectiveAt(at) {
			continue
		}
		if best == nil || v.Revision > best.Revision {
			best = v
		}
	}
	if best == nil {
		return nil, ErrUnpriced
	}
	return best, nil
}

// Inclusion declares which overlaps a RAW upstream usage payload carries,
// so the adapter and the accounting layer share ONE normalization rule
// (设计 §7.1: 适配器负责规范化重叠语义).
type Inclusion struct {
	// ReasoningInOutput: the upstream's output total already contains the
	// reasoning tokens (e.g. OpenAI completion_tokens includes
	// reasoning_tokens) — they must NOT be added again (推理 token 已含在
	// 输出总数时不重复加算).
	ReasoningInOutput bool
	// CacheReadInInput / CacheWriteInInput: the input total already
	// contains the cached buckets (e.g. OpenAI prompt_tokens includes
	// cached_tokens) — they are subtracted out of billable input so cached
	// tokens price only at their own (cheaper) rate.
	CacheReadInInput  bool
	CacheWriteInInput bool
}

// BillableBuckets normalizes possibly-overlapping raw buckets into the
// disjoint chargeable shape:
//
//   - reasoning already inside output → output unchanged, reasoning stays
//     informational-only;
//   - reasoning reported separately → folded INTO billable output exactly
//     once (normalized OutputTokens always carries reasoning);
//   - cache buckets flagged as inside input → subtracted from billable
//     input so they price once, at the cache rate.
//
// nil stays nil — unknown never collapses into zero (设计 §7.1). Flags
// inconsistent with the bucket values (subtracting more than present) are
// rejected as invalid input.
func BillableBuckets(raw domain.UsageBuckets, inc Inclusion) (domain.UsageBuckets, error) {
	out := raw
	var err error
	if inc.CacheReadInInput {
		if out.InputTokens, err = subBucket(raw.InputTokens, raw.CacheReadTokens); err != nil {
			return domain.UsageBuckets{}, err
		}
	}
	if inc.CacheWriteInInput {
		if out.InputTokens, err = subBucket(out.InputTokens, raw.CacheWriteTokens); err != nil {
			return domain.UsageBuckets{}, err
		}
	}
	if !inc.ReasoningInOutput && raw.ReasoningTokens != nil {
		out.OutputTokens = addBucket(raw.OutputTokens, raw.ReasoningTokens)
	}
	return out, nil
}

// subBucket returns base - part; nil base passes through (nothing known to
// subtract from), nil part subtracts nothing.
func subBucket(base, part *int64) (*int64, error) {
	if base == nil || part == nil {
		return base, nil
	}
	if *part > *base {
		return nil, domain.WrapError(domain.CodeInvalidInput,
			"accounting: inclusion flags inconsistent with buckets (part exceeds total)", domain.ErrNegativeValue)
	}
	v := *base - *part
	return &v, nil
}

// addBucket returns a + b tolerating nils (nil contributes nothing known).
func addBucket(a, b *int64) *int64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil:
		v := *a
		return &v
	default:
		v := *a + *b
		return &v
	}
}

// Charge is the priced result of ONE consumption under ONE pinned price
// version. Exactly one of Credit / Money is set, by the list's unit.
type Charge struct {
	Kind PriceKind
	// Basis mirrors the usage source — reported or estimated, NEVER unknown
	// (estimated charges stay distinguishable from reported ones, 设计 §7.1).
	Basis domain.UsageSource
	// Credit is the quota draw for sale_credit lists.
	Credit domain.Microcredit
	// Money is the amount for sale_money / upstream_cost lists.
	Money *domain.Money
	// PriceVersionID + Revision identify the immutable revision used.
	PriceVersionID string
	Revision       int
	// Billable echoes the normalized buckets the charge was computed from.
	Billable domain.UsageBuckets
}

// Quote prices one usage record under this pinned price version. Buckets
// MUST already be normalized (disjoint — see BillableBuckets); Quote never
// adds ReasoningTokens on top of OutputTokens again. Rounding applies per
// price line (bucket × rate) before lines sum (取整位置), in the direction
// RoundingFor pins. extras prices other billable items per unit; using an
// item with no configured rate is an unpriced capability and rejects
// (CodeUnpricedCapability).
func (p *PriceVersion) Quote(usage domain.UsageRecord, extras map[string]int64) (*Charge, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	switch usage.Source {
	case domain.UsageReported, domain.UsageEstimated:
		// priced; the basis carries through so estimated stays separate
		// from reported (设计 §7.1).
	case domain.UsageUnknown:
		return nil, ErrUnknownUsage
	default:
		return nil, domain.NewError(domain.CodeInvalidInput,
			"accounting: usage source "+string(usage.Source)+" is not chargeable")
	}
	b := usage.Buckets
	if b.InputTokens == nil && b.CacheReadTokens == nil && b.CacheWriteTokens == nil &&
		b.OutputTokens == nil && b.ReasoningTokens == nil && len(extras) == 0 {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"accounting: usage record carries no buckets (missing usage is never zero)")
	}

	mode := RoundingFor(p.Kind)
	var total int64
	addLine := func(units int64, rate domain.Rate) error {
		m, err := rate.MicrosForUnits(units, mode)
		if err != nil {
			return err
		}
		sum, err := domain.Microcredit(total).Add(domain.Microcredit(m))
		if err != nil {
			return err
		}
		total = int64(sum)
		return nil
	}
	lines := []struct {
		units *int64
		rate  domain.Rate
	}{
		{b.InputTokens, p.Input},
		{b.CacheReadTokens, p.CacheRead},
		{b.CacheWriteTokens, p.CacheWrite},
		{b.OutputTokens, p.Output},
		// ReasoningTokens is deliberately NOT a line: after normalization
		// output already carries reasoning exactly once.
	}
	for _, l := range lines {
		if l.units == nil {
			continue
		}
		if err := addLine(*l.units, l.rate); err != nil {
			return nil, err
		}
	}
	for name, units := range extras {
		if units < 0 {
			return nil, domain.WrapError(domain.CodeInvalidInput,
				"accounting: negative extra usage for "+name, domain.ErrNegativeValue)
		}
		rate, ok := p.ExtraRates[name]
		if !ok {
			if units > 0 {
				return nil, domain.NewError(domain.CodeUnpricedCapability,
					"accounting: billable item "+name+" has no rate in this price version")
			}
			continue
		}
		if err := addLine(units, rate); err != nil {
			return nil, err
		}
	}

	c := &Charge{
		Kind: p.Kind, Basis: usage.Source,
		PriceVersionID: p.ID, Revision: p.Revision, Billable: b,
	}
	if p.Kind == PriceSaleCredit {
		c.Credit = domain.Microcredit(total)
		return c, nil
	}
	m, err := domain.NewMoney(total, p.Currency)
	if err != nil {
		return nil, err
	}
	c.Money = &m
	return c, nil
}

// extraRatesDoc is the schema-versioned storage shape of
// inference_price_versions.extra_rates: micro-units per single unit of the
// named billable item.
type extraRatesDoc struct {
	SchemaVersion int              `json:"schema_version"`
	Rates         map[string]int64 `json:"rates"`
}

// DecodeExtraRates parses the schema-versioned extension price list. Only
// schema_version 1 is understood; anything else is rejected rather than
// silently mispriced.
func DecodeExtraRates(raw json.RawMessage) (map[string]domain.Rate, error) {
	out := make(map[string]domain.Rate)
	if len(raw) == 0 {
		return out, nil
	}
	var doc extraRatesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, domain.WrapError(domain.CodeInvalidInput, "accounting: decode extra_rates", err)
	}
	if doc.SchemaVersion != 1 {
		return nil, domain.NewError(domain.CodeInvalidInput, "accounting: unsupported extra_rates schema_version")
	}
	for name, micros := range doc.Rates {
		r, err := domain.NewRate(micros, 1)
		if err != nil {
			return nil, domain.WrapError(domain.CodeInvalidInput, "accounting: extra rate "+name, err)
		}
		out[name] = r
	}
	return out, nil
}
