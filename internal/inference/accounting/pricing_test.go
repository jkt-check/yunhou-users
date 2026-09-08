package accounting

import (
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// pricing_test.go — 价格版本与 token 规范化纯规则测试（注入时间，不依赖
// DB）：三套口径独立版本化、取整方向、缓存/推理 token 包含关系、未定价
// 拒绝、estimated/unknown 与 reported 分离。

func mustRate(t *testing.T, num, den int64) domain.Rate {
	t.Helper()
	r, err := domain.NewRate(num, den)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// perMtok builds the exact rate for a per-million-tokens storage price.
func perMtok(t *testing.T, micros int64) domain.Rate {
	t.Helper()
	r, err := domain.RatePerMillion(micros)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func i64(v int64) *int64 { return &v }

func t0() time.Time { return time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC) }

func creditPrice(t *testing.T, id string, rev int, from time.Time, to *time.Time, inPerMtok, outPerMtok int64) PriceVersion {
	t.Helper()
	return PriceVersion{
		ID: id, ModelID: "glm-4.6", Kind: PriceSaleCredit, Revision: rev,
		Input: perMtok(t, inPerMtok), Output: perMtok(t, outPerMtok),
		EffectiveFrom: from, EffectiveTo: to,
	}
}

func TestResolvePrice_IndependentKindsAndBoundaries(t *testing.T) {
	from := t0()
	mid := from.Add(time.Hour)
	versions := []PriceVersion{
		creditPrice(t, "v1", 1, from, &mid, 1_000_000, 2_000_000),
		creditPrice(t, "v2", 2, mid, nil, 3_000_000, 4_000_000),
	}

	// [from, to)：at==from 命中 v1；at==to（即 mid）命中 v2。
	v, err := ResolvePrice(versions, PriceSaleCredit, from)
	if err != nil || v.ID != "v1" {
		t.Errorf("at from: %v %v", v, err)
	}
	v, err = ResolvePrice(versions, PriceSaleCredit, mid)
	if err != nil || v.ID != "v2" {
		t.Errorf("at to boundary must hit the NEW revision: %v %v", v, err)
	}
	// from 之前无生效版本 → 未定价拒绝。
	_, err = ResolvePrice(versions, PriceSaleCredit, from.Add(-time.Second))
	if !errors.Is(err, ErrUnpriced) {
		t.Errorf("before first price: err = %v, want ErrUnpriced", err)
	}
	if domain.CodeOf(err) != domain.CodeUnpricedCapability {
		t.Errorf("code = %v, want unpriced_capability", domain.CodeOf(err))
	}

	// 三套口径分别版本化：sale_money 无版本不影响 sale_credit 解析。
	if _, err := ResolvePrice(versions, PriceSaleMoney, from); !errors.Is(err, ErrUnpriced) {
		t.Errorf("unpriced kind must reject independently: %v", err)
	}
}

func TestQuote_ExactMathAndRoundingDirection(t *testing.T) {
	pv := PriceVersion{
		ID: "p1", ModelID: "m", Kind: PriceSaleCredit, Revision: 1,
		Input: perMtok(t, 1_000_000), Output: perMtok(t, 2_000_000),
		CacheRead: perMtok(t, 100_000), EffectiveFrom: t0(),
	}
	usage := domain.UsageRecord{
		Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{
			InputTokens: i64(1500), OutputTokens: i64(500), CacheReadTokens: i64(200),
		},
	}
	c, err := pv.Quote(usage, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1500×1 + 500×2 + 200×0.1 = 1500 + 1000 + 20 = 2520 microcredit。
	if c.Credit != 2520 {
		t.Errorf("credit = %d, want 2520", c.Credit)
	}
	if c.Basis != domain.UsageReported || c.PriceVersionID != "p1" {
		t.Errorf("charge meta: %+v", c)
	}

	// 客户口径向上取整：1 token @ 1 micro/mtok → 1 microcredit。
	tiny := PriceVersion{ID: "p2", ModelID: "m", Kind: PriceSaleCredit, Revision: 1,
		Input: perMtok(t, 1), EffectiveFrom: t0()}
	c, err = tiny.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{InputTokens: i64(1)},
	}, nil)
	if err != nil || c.Credit != 1 {
		t.Errorf("customer charge must round UP: %v %v", c, err)
	}

	// 上游成本向下取整：同样的微小量 → 0（不高估成本）。
	cost := PriceVersion{ID: "p3", ModelID: "m", Kind: PriceUpstreamCost, Revision: 1,
		Currency: "USD", Input: perMtok(t, 1), EffectiveFrom: t0()}
	cc, err := cost.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{InputTokens: i64(1)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Money == nil || cc.Money.Micros != 0 || cc.Money.Currency != "USD" {
		t.Errorf("upstream cost must round DOWN: %+v", cc.Money)
	}
}

// TestBillableBuckets_ReasoningNotDoubleCounted 钉牢设计 §7.1：推理 token
// 已含在输出总数中时不重复加算；未包含时恰好并入一次。
func TestBillableBuckets_ReasoningNotDoubleCounted(t *testing.T) {
	pv := PriceVersion{ID: "p", ModelID: "m", Kind: PriceSaleCredit, Revision: 1,
		Output: perMtok(t, 1_000_000), EffectiveFrom: t0()} // 1 micro/token

	// 上游输出总数已含推理：输出 100（含推理 40）→ 计费 100，不是 140。
	raw := domain.UsageBuckets{OutputTokens: i64(100), ReasoningTokens: i64(40)}
	b, err := BillableBuckets(raw, Inclusion{ReasoningInOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if *b.OutputTokens != 100 {
		t.Errorf("billable output = %d, want 100 (reasoning already included)", *b.OutputTokens)
	}
	c, err := pv.Quote(domain.UsageRecord{Source: domain.UsageReported, Buckets: b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Credit != 100 {
		t.Errorf("charge = %d, want 100 — reasoning must NOT be added again", c.Credit)
	}

	// 推理单列（上游输出不含推理）：并入输出恰好一次 → 140。
	b, err = BillableBuckets(raw, Inclusion{ReasoningInOutput: false})
	if err != nil {
		t.Fatal(err)
	}
	if *b.OutputTokens != 140 {
		t.Errorf("billable output = %d, want 140 (reasoning folded once)", *b.OutputTokens)
	}
	c, err = pv.Quote(domain.UsageRecord{Source: domain.UsageReported, Buckets: b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Credit != 140 {
		t.Errorf("charge = %d, want 140", c.Credit)
	}

	// 即使调用方失误把已规范化的桶再带推理明细，Quote 也永不再加算推理。
	c, err = pv.Quote(domain.UsageRecord{
		Source:  domain.UsageReported,
		Buckets: domain.UsageBuckets{OutputTokens: i64(140), ReasoningTokens: i64(40)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Credit != 140 {
		t.Errorf("charge = %d, want 140 (normalized output is authoritative)", c.Credit)
	}
}

func TestBillableBuckets_CacheInclusion(t *testing.T) {
	// OpenAI 风格：prompt_tokens 已含 cached_tokens。
	raw := domain.UsageBuckets{InputTokens: i64(1000), CacheReadTokens: i64(300)}
	b, err := BillableBuckets(raw, Inclusion{CacheReadInInput: true})
	if err != nil {
		t.Fatal(err)
	}
	if *b.InputTokens != 700 || *b.CacheReadTokens != 300 {
		t.Errorf("billable = in %d cache %d, want 700/300", *b.InputTokens, *b.CacheReadTokens)
	}

	// Anthropic 风格：缓存桶与输入本就分离，不裁剪。
	b, err = BillableBuckets(raw, Inclusion{})
	if err != nil {
		t.Fatal(err)
	}
	if *b.InputTokens != 1000 {
		t.Errorf("disjoint buckets must pass through: in %d", *b.InputTokens)
	}

	// 标志与数值不一致（裁剪成负）→ 拒绝。
	bad := domain.UsageBuckets{InputTokens: i64(100), CacheReadTokens: i64(300)}
	if _, err := BillableBuckets(bad, Inclusion{CacheReadInInput: true}); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("inconsistent inclusion: err = %v, want invalid_input", err)
	}

	// nil 桶保持 nil：未知不塌缩成 0。
	b, err = BillableBuckets(domain.UsageBuckets{CacheReadTokens: i64(10)}, Inclusion{CacheReadInInput: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.InputTokens != nil {
		t.Errorf("nil input must stay nil, got %d", *b.InputTokens)
	}
}

func TestQuote_UnpricedAndUnknownPaths(t *testing.T) {
	pv := PriceVersion{ID: "p", ModelID: "m", Kind: PriceSaleCredit, Revision: 1,
		Input: perMtok(t, 1_000_000), EffectiveFrom: t0(),
		ExtraRates: map[string]domain.Rate{"web_search": mustRate(t, 500, 1)},
	}

	// unknown：不得计价（保留预占进入核对，不记零）。
	_, err := pv.Quote(domain.UsageRecord{Source: domain.UsageUnknown}, nil)
	if !errors.Is(err, ErrUnknownUsage) {
		t.Errorf("unknown usage: err = %v, want ErrUnknownUsage", err)
	}

	// 全 nil 桶：缺失用量不是零消费。
	_, err = pv.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{},
	}, nil)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("empty buckets: err = %v, want invalid_input", err)
	}

	// estimated 可计价但与 reported 分开（basis 透传）。
	c, err := pv.Quote(domain.UsageRecord{
		Source: domain.UsageEstimated, Buckets: domain.UsageBuckets{InputTokens: i64(10)},
	}, nil)
	if err != nil || c.Basis != domain.UsageEstimated {
		t.Errorf("estimated charge: %v %+v", err, c)
	}

	// 未定价扩展项且实际使用 → 拒绝（未定价且需扣费的能力）。
	_, err = pv.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{InputTokens: i64(10)},
	}, map[string]int64{"code_interpreter": 2})
	if domain.CodeOf(err) != domain.CodeUnpricedCapability {
		t.Errorf("unpriced extra: err = %v, want unpriced_capability", err)
	}

	// 已定价扩展项按单位计价；未使用（0）的未定价项不拒绝。
	c, err = pv.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{InputTokens: i64(10)},
	}, map[string]int64{"web_search": 3, "code_interpreter": 0})
	if err != nil {
		t.Fatal(err)
	}
	// 10 input×1 + 3×500 = 1510。
	if c.Credit != 1510 {
		t.Errorf("credit = %d, want 1510", c.Credit)
	}
}

// TestPriceChangeDoesNotAffectPinnedRequest 验收硬项的规则侧：请求钉住旧
// 版本后，发布新价不影响已钉版本的计价结果。
func TestPriceChangeDoesNotAffectPinnedRequest(t *testing.T) {
	from := t0()
	mid := from.Add(time.Hour)
	v1 := creditPrice(t, "v1", 1, from, &mid, 1_000_000, 2_000_000)
	usage := domain.UsageRecord{
		Source:  domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: i64(100), OutputTokens: i64(50)},
	}
	before, err := v1.Quote(usage, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 新价发布（v2 更贵）；v1 版本对象不可变，重算同额。
	v2 := creditPrice(t, "v2", 2, mid, nil, 9_000_000, 9_000_000)
	after, err := v1.Quote(usage, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Credit != after.Credit {
		t.Errorf("pinned version re-priced: %d → %d", before.Credit, after.Credit)
	}

	// 解析层：admitted_at 在旧区间 → 仍解析到 v1；新时刻 → v2。
	got, err := ResolvePrice([]PriceVersion{v1, v2}, PriceSaleCredit, from.Add(30*time.Minute))
	if err != nil || got.ID != "v1" {
		t.Errorf("old instant must still resolve v1: %v %v", got, err)
	}
	got, err = ResolvePrice([]PriceVersion{v1, v2}, PriceSaleCredit, mid)
	if err != nil || got.ID != "v2" {
		t.Errorf("new instant must resolve v2: %v %v", got, err)
	}
}

func TestPriceVersion_Validate(t *testing.T) {
	// 额度价目不得带币种；金额价目必须有合法币种。
	credit := creditPrice(t, "c", 1, t0(), nil, 1, 1)
	credit.Currency = "USD"
	if err := credit.Validate(); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("credit with currency: %v", err)
	}
	money := PriceVersion{Kind: PriceSaleMoney, Revision: 1, EffectiveFrom: t0()}
	if err := money.Validate(); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("money without currency: %v", err)
	}
	money.Currency = "USD"
	if err := money.Validate(); err != nil {
		t.Errorf("valid money list: %v", err)
	}
	bad := creditPrice(t, "b", 0, t0(), nil, 1, 1)
	if err := bad.Validate(); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("revision 0: %v", err)
	}
	unknown := PriceVersion{Kind: "mystery", Revision: 1, EffectiveFrom: t0()}
	if err := unknown.Validate(); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestDecodeExtraRates(t *testing.T) {
	rates, err := DecodeExtraRates([]byte(`{"schema_version":1,"rates":{"web_search":500}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := rates["web_search"]; r.Num != 500 || r.Den != 1 {
		t.Errorf("rate = %v, want 500/1", r)
	}
	if _, err := DecodeExtraRates([]byte(`{"schema_version":2}`)); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("unsupported schema: %v", err)
	}
	if _, err := DecodeExtraRates([]byte(`{"schema_version":1,"rates":{"x":-1}}`)); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("negative rate: %v", err)
	}
	empty, err := DecodeExtraRates(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("nil raw: %v %v", empty, err)
	}
}
