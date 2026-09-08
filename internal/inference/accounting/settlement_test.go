package accounting

import (
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// settlement_test.go — 结算决策纯规则（Task 9）：Decide 归一+计价+超占检测、
// 保守估算记录、冲正/补差 delta、结算延迟。

func testPrice() PriceVersion {
	in, _ := domain.NewRate(1_000_000, 1)  // 1 credit/token input
	out, _ := domain.NewRate(2_000_000, 1) // 2 credits/token output
	cr, _ := domain.NewRate(100_000, 1)
	cw, _ := domain.NewRate(200_000, 1)
	return PriceVersion{
		ID: "pv1", ModelID: "m", Kind: PriceSaleCredit,
		Input: in, Output: out, CacheRead: cr, CacheWrite: cw,
		ExtraRates: map[string]domain.Rate{}, Revision: 1,
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func ptrI64(v int64) *int64 { return &v }

func TestDecide_NormalizesAndPrices(t *testing.T) {
	// OpenAI 口径：cached 含在 input、reasoning 含在 output。
	inc := Inclusion{ReasoningInOutput: true, CacheReadInInput: true}
	usage := domain.UsageRecord{
		Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{
			InputTokens: ptrI64(10), CacheReadTokens: ptrI64(4),
			OutputTokens: ptrI64(5), ReasoningTokens: ptrI64(2),
		},
	}
	d, err := Decide(usage, testPrice(), inc, 20_000_000)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	// billable input = 10 − 4 = 6；cache_read 4 单独计价；output 5
	// （reasoning 已在其中，不重复加算）。
	// charge = 6×1 + 4×0.1 + 5×2 = 16.4 credits = 16_400_000 micros。
	if got := int64(d.Charge.Credit); got != 16_400_000 {
		t.Errorf("charge = %d, want 16400000", got)
	}
	if *d.Record.Buckets.InputTokens != 6 {
		t.Errorf("billable input = %d, want 6 (cache subtracted once)", *d.Record.Buckets.InputTokens)
	}
	if d.OverageMicros() != 0 {
		t.Errorf("overage = %d, want 0 (charge fits hold)", d.OverageMicros())
	}
}

func TestDecide_UnknownRefusesToPrice(t *testing.T) {
	usage := domain.UsageRecord{Source: domain.UsageUnknown}
	_, err := Decide(usage, testPrice(), Inclusion{}, 100)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("err = %v, want ErrUnknownUsage", err)
	}
}

func TestDecide_OverageIsSurfacedNotClamped(t *testing.T) {
	usage := domain.UsageRecord{
		Source:  domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: ptrI64(100), OutputTokens: ptrI64(100)},
	}
	// charge = 100×1 + 100×2 = 300 credits = 300_000_000 micros；hold 只有 200_000_000。
	d, err := Decide(usage, testPrice(), Inclusion{}, 200_000_000)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Charge.Credit != 300_000_000 {
		t.Fatalf("charge = %d, want 300000000 (real amount, never clamped)", d.Charge.Credit)
	}
	if d.OverageMicros() != 100_000_000 {
		t.Errorf("overage = %d, want 100000000 (负差额不隐藏)", d.OverageMicros())
	}
}

func TestConservativeRecord_BoundsAndReservedBasis(t *testing.T) {
	reserved := domain.Microcredit(42_000)
	in, out := int64(120), int64(8192)
	req := &domain.Request{
		ID: "r1", ReservedMicros: &reserved,
		InputBoundTokens: &in, OutputCapTokens: &out,
		ExtraBounds: map[string]int64{"tool_call": 3},
	}
	rec := ConservativeRecord("r1", "a1", req, "crash_recovery", 1)
	if rec.Source != domain.UsageEstimated {
		t.Errorf("source = %q, want estimated", rec.Source)
	}
	if *rec.Buckets.InputTokens != 120 || *rec.Buckets.OutputTokens != 8192 {
		t.Errorf("buckets = %+v, want bounds 120/8192", rec.Buckets)
	}
	raw := string(rec.RawUsage.Raw)
	for _, want := range []string{"conservative_bounds", "crash_recovery", "42000", "tool_call"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw_usage %s missing %q (估算依据必须可审计)", raw, want)
		}
	}
}

func TestConservativeRecord_LegacyRowWithoutBounds(t *testing.T) {
	// 031 之前的行没有 bounds：不编造 token 桶，charge 仍按预占额（由调用方给）。
	reserved := domain.Microcredit(7_000)
	req := &domain.Request{ID: "r2", ReservedMicros: &reserved}
	rec := ConservativeRecord("r2", "a1", req, "crash_recovery", 1)
	if rec.Buckets.InputTokens != nil || rec.Buckets.OutputTokens != nil {
		t.Errorf("buckets = %+v, want nil (无依据不编造)", rec.Buckets)
	}
	if !strings.Contains(string(rec.RawUsage.Raw), "7000") {
		t.Errorf("raw = %s, want reserved basis recorded", rec.RawUsage.Raw)
	}
}

func TestCorrectionDelta(t *testing.T) {
	// 补差：修正 > 原值 → debit。
	dir, amt, err := CorrectionDelta(100, 250)
	if err != nil || dir != "debit" || amt != 150 {
		t.Errorf("debit: %s/%d/%v, want debit/150", dir, amt, err)
	}
	// 退还：修正 < 原值 → credit。
	dir, amt, err = CorrectionDelta(250, 100)
	if err != nil || dir != "credit" || amt != 150 {
		t.Errorf("credit: %s/%d/%v, want credit/150", dir, amt, err)
	}
	// 相等 = 非修正，拒绝（不追加空历史）。
	if _, _, err = CorrectionDelta(100, 100); err == nil {
		t.Error("equal amounts must reject (no-op corrections are not appended)")
	}
	// 负修正拒绝。
	if _, _, err = CorrectionDelta(100, -1); err == nil {
		t.Error("negative correction must reject")
	}
	// 零额原值 → 全额补差（无 reversal，仅 debit adjustment — repo 层语义）。
	dir, amt, err = CorrectionDelta(0, 80)
	if err != nil || dir != "debit" || amt != 80 {
		t.Errorf("zero original: %s/%d/%v, want debit/80", dir, amt, err)
	}
}

func TestSettlementLag(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if got := SettlementLag(now, nil); got != 0 {
		t.Errorf("nil oldest: lag = %v, want 0", got)
	}
	oldest := now.Add(-90 * time.Second)
	if got := SettlementLag(now, &oldest); got != 90*time.Second {
		t.Errorf("lag = %v, want 90s", got)
	}
	future := now.Add(time.Minute)
	if got := SettlementLag(now, &future); got != 0 {
		t.Errorf("clock skew: lag = %v, want 0 (clamped)", got)
	}
}
