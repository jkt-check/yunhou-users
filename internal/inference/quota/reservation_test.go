package quota

import (
	"errors"
	"testing"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// reservation_test.go — 预占金额安全上界纯规则（Task 7，设计 §7.2）。

func ratePerMtok(t *testing.T, micros int64) domain.Rate {
	t.Helper()
	r, err := domain.RatePerMillion(micros)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func creditPrice(t *testing.T, inputPerMtok, outputPerMtok int64, extras map[string]int64) accounting.PriceVersion {
	t.Helper()
	extraRates := map[string]domain.Rate{}
	for name, micros := range extras {
		r, err := domain.NewRate(micros, 1)
		if err != nil {
			t.Fatal(err)
		}
		extraRates[name] = r
	}
	return accounting.PriceVersion{
		ID: "pv-1", ModelID: "m", Kind: accounting.PriceSaleCredit,
		Input: ratePerMtok(t, inputPerMtok), Output: ratePerMtok(t, outputPerMtok),
		CacheRead: ratePerMtok(t, 0), CacheWrite: ratePerMtok(t, 0),
		ExtraRates: extraRates, Revision: 1,
	}
}

func TestEffectiveOutputCap(t *testing.T) {
	client := int64(4096)
	tooBig := int64(999_999)
	zero := int64(0)
	tests := []struct {
		name     string
		client   *int64
		modelMax int
		want     int64
		wantErr  bool
	}{
		{"client below model cap", &client, 8192, 4096, false},
		{"client above model cap narrows to model", &tooBig, 8192, 8192, false},
		{"no client cap → forced model cap", nil, 8192, 8192, false},
		{"model without hard cap rejects (不允许无限输出)", nil, 0, 0, true},
		{"zero client cap rejects", &zero, 8192, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveOutputCap(tc.client, tc.modelMax)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got cap %d", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("cap = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInputBound(t *testing.T) {
	// 估计低于上下文限制 → 估计即上界；超出 → 裁剪到硬上限。
	got, err := InputBound(50_000, 200_000)
	if err != nil || got != 50_000 {
		t.Errorf("got %d err %v, want 50000", got, err)
	}
	got, err = InputBound(500_000, 200_000)
	if err != nil || got != 200_000 {
		t.Errorf("got %d err %v, want 200000 (clamped to context hard limit)", got, err)
	}
	if _, err := InputBound(100, 0); err == nil {
		t.Error("model without context limit must reject (input unbounded)")
	}
	if _, err := InputBound(-1, 200_000); !errors.Is(err, domain.ErrNegativeValue) {
		t.Errorf("negative estimate: err = %v, want ErrNegativeValue", err)
	}
}

func TestReserveAmountComposition(t *testing.T) {
	// input 2 micro/token(Mtok=2_000_000), output 10 micro/token, 无 extras。
	p := creditPrice(t, 2_000_000, 10_000_000, nil)
	got, err := ReserveAmount(p, ReserveBounds{InputBoundTokens: 1000, OutputCapTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	// 1000×2 + 100×10 = 2000 + 1000 = 3000 micros。
	if got != 3000 {
		t.Errorf("reserve = %d, want 3000", got)
	}
}

func TestReserveAmountRoundsUpPerLine(t *testing.T) {
	// 1 micro/Mtok input：1 token → ceil(1/1e6) = 1 micro（客户侧向上取整，
	// 预占永不低于精确有理数上界）。
	p := creditPrice(t, 1, 1, nil)
	got, err := ReserveAmount(p, ReserveBounds{InputBoundTokens: 1, OutputCapTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("reserve = %d, want 2 (per-line RoundUp)", got)
	}
}

func TestReserveAmountExtras(t *testing.T) {
	p := creditPrice(t, 0, 1_000_000, map[string]int64{"web_search": 5000})
	// 有费率的高成本项目按上界计入。
	got, err := ReserveAmount(p, ReserveBounds{
		InputBoundTokens: 0, OutputCapTokens: 1,
		ExtraBounds: map[string]int64{"web_search": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1+3*5000 {
		t.Errorf("reserve = %d, want %d", got, 1+3*5000)
	}
	// 未定价的高成本项目 → CodeUnpricedCapability（校验额外计费项目上界）。
	_, err = ReserveAmount(p, ReserveBounds{
		InputBoundTokens: 0, OutputCapTokens: 1,
		ExtraBounds: map[string]int64{"code_exec": 1},
	})
	if domain.CodeOf(err) != domain.CodeUnpricedCapability {
		t.Errorf("unpriced extra: err = %v, want CodeUnpricedCapability", err)
	}
	// 上界为 0 的未定价项目不阻塞（没有使用）。
	if _, err := ReserveAmount(p, ReserveBounds{
		InputBoundTokens: 0, OutputCapTokens: 1,
		ExtraBounds: map[string]int64{"code_exec": 0},
	}); err != nil {
		t.Errorf("zero-bound unpriced extra must not block: %v", err)
	}
}

func TestReserveAmountRejectsDegenerate(t *testing.T) {
	p := creditPrice(t, 0, 0, nil)
	// 全零价目 → 预占额为 0 → 拒绝（零价上界会放行无约束的免费负载）。
	if _, err := ReserveAmount(p, ReserveBounds{InputBoundTokens: 1000, OutputCapTokens: 100}); err == nil {
		t.Error("zero-priced reservation must reject")
	}
	// 输出上限为 0 → 拒绝（不允许无限输出）。
	p2 := creditPrice(t, 1_000_000, 1_000_000, nil)
	if _, err := ReserveAmount(p2, ReserveBounds{InputBoundTokens: 1, OutputCapTokens: 0}); err == nil {
		t.Error("zero output cap must reject")
	}
	// 非 sale_credit 价目 → 拒绝。
	money := accounting.PriceVersion{Kind: accounting.PriceSaleMoney, Currency: "USD"}
	if _, err := ReserveAmount(money, ReserveBounds{InputBoundTokens: 1, OutputCapTokens: 1}); err == nil {
		t.Error("non-credit price list must reject for quota reservation")
	}
}

func TestReserveAmountOverflow(t *testing.T) {
	// 天文数字上界 × 费率 → 溢出必须报错而不是回绕。
	huge := accounting.PriceVersion{
		Kind:   accounting.PriceSaleCredit,
		Input:  domain.Rate{Num: 1 << 40, Den: 1},
		Output: domain.Rate{Num: 1 << 40, Den: 1},
	}
	if _, err := ReserveAmount(huge, ReserveBounds{
		InputBoundTokens: 1 << 40, OutputCapTokens: 1 << 40,
	}); !errors.Is(err, domain.ErrOverflow) {
		t.Errorf("err = %v, want ErrOverflow", err)
	}
}
