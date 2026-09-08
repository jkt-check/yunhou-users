package domain

import (
	"errors"
	"math"
	"testing"
)

func TestMicrocreditAddOverflow(t *testing.T) {
	max := Microcredit(math.MaxInt64)
	if _, err := max.Add(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64+1: err = %v, want ErrOverflow", err)
	}
	if _, err := max.Add(max); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64+MaxInt64: err = %v, want ErrOverflow", err)
	}
	min := Microcredit(math.MinInt64)
	if _, err := min.Add(-1); !errors.Is(err, ErrOverflow) {
		t.Errorf("MinInt64-1: err = %v, want ErrOverflow", err)
	}
	if _, err := min.Add(min); !errors.Is(err, ErrOverflow) {
		t.Errorf("MinInt64+MinInt64: err = %v, want ErrOverflow", err)
	}
	got, err := Microcredit(40).Add(2)
	if err != nil || got != 42 {
		t.Errorf("40+2 = %d, %v; want 42, nil", got, err)
	}
	// Opposite signs can never overflow.
	got, err = max.Add(min)
	if err != nil || got != -1 {
		t.Errorf("max+min = %d, %v; want -1, nil", got, err)
	}
}

func TestMicrocreditSubOverflow(t *testing.T) {
	min := Microcredit(math.MinInt64)
	if _, err := Microcredit(0).Sub(min); !errors.Is(err, ErrOverflow) {
		t.Errorf("0-MinInt64: err = %v, want ErrOverflow", err)
	}
	if _, err := Microcredit(-5).Sub(Microcredit(math.MaxInt64)); !errors.Is(err, ErrOverflow) {
		t.Errorf("-5-MaxInt64: err = %v, want ErrOverflow", err)
	}
	got, err := Microcredit(10).Sub(4)
	if err != nil || got != 6 {
		t.Errorf("10-4 = %d, %v; want 6, nil", got, err)
	}
	// Negative intermediate results are legal at the type level, but
	// NonNegative must catch them before they reach a CHECK'd column.
	r, err := got.Sub(100)
	if err != nil {
		t.Fatalf("6-100 should not overflow: %v", err)
	}
	if err := r.NonNegative(); !errors.Is(err, ErrNegativeValue) {
		t.Errorf("NonNegative(-94): err = %v, want ErrNegativeValue", err)
	}
}

func TestMoneyCurrencyMismatch(t *testing.T) {
	cny, err := NewMoney(100, "CNY")
	if err != nil {
		t.Fatalf("NewMoney CNY: %v", err)
	}
	usd, err := NewMoney(100, "USD")
	if err != nil {
		t.Fatalf("NewMoney USD: %v", err)
	}
	if _, err := cny.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("CNY+USD: err = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := cny.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("CNY-USD: err = %v, want ErrCurrencyMismatch", err)
	}
	sum, err := cny.Add(Money{Micros: 50, Currency: "CNY"})
	if err != nil || sum.Micros != 150 {
		t.Errorf("CNY 100+50 = %+v, %v; want 150", sum, err)
	}
}

func TestMoneyValidation(t *testing.T) {
	for _, bad := range []string{"", "cny", "CN", "CNYY", "C1Y", "CN "} {
		if _, err := NewMoney(1, bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("NewMoney(_, %q): err = %v, want ErrInvalidCurrency", bad, err)
		}
	}
	if _, err := NewMoney(-1, "CNY"); !errors.Is(err, ErrNegativeValue) {
		t.Errorf("NewMoney(-1): err = %v, want ErrNegativeValue", err)
	}
}

func TestMoneyOverflowAndNoHiddenOverdraft(t *testing.T) {
	big1 := Money{Micros: math.MaxInt64 - 10, Currency: "CNY"}
	big2 := Money{Micros: 20, Currency: "CNY"}
	if _, err := big1.Add(big2); !errors.Is(err, ErrOverflow) {
		t.Errorf("Add overflow: err = %v, want ErrOverflow", err)
	}
	// 设计 §7.2: 不隐藏负差额 — Sub below zero is an explicit error.
	a := Money{Micros: 5, Currency: "CNY"}
	b := Money{Micros: 10, Currency: "CNY"}
	if _, err := a.Sub(b); !errors.Is(err, ErrNegativeValue) {
		t.Errorf("5-10: err = %v, want ErrNegativeValue", err)
	}
}

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in       string
		num, den int64
	}{
		{"12", 12, 1},
		{"12.34", 1234, 100},
		{"0.000001", 1, 1_000_000},
		{".5", 5, 10},
		{"5.", 5, 1},
		{"007", 7, 1},
	}
	for _, c := range cases {
		r, err := ParseDecimal(c.in)
		if err != nil {
			t.Errorf("ParseDecimal(%q): %v", c.in, err)
			continue
		}
		if r.Num != c.num || r.Den != c.den {
			t.Errorf("ParseDecimal(%q) = %d/%d, want %d/%d", c.in, r.Num, r.Den, c.num, c.den)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "-1", "1e3", "1,2", "  "} {
		if _, err := ParseDecimal(bad); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseDecimal(%q): err = %v, want ErrInvalidDecimal", bad, err)
		}
	}
	// Exactness: 0.6 truncates to 0 in RoundDown (float64 would risk
	// 0.599999… artifacts; rational math is exact by construction).
	fifth, _ := NewRate(1, 5)
	got, err := fifth.MicrosForUnits(3, RoundDown) // 3 * 1/5 = 0.6 → 0
	if err != nil || got != 0 {
		t.Errorf("3/5 RoundDown = %d, %v; want 0", got, err)
	}
}

func TestRateRoundingDirections(t *testing.T) {
	half, err := NewRate(1, 2) // 0.5 micro per unit
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		mode RoundingMode
		want int64
	}{
		{RoundDown, 0},
		{RoundUp, 1},
		{RoundHalfUp, 1},
	}
	for _, c := range cases {
		got, err := half.MicrosForUnits(1, c.mode)
		if err != nil || got != c.want {
			t.Errorf("0.5 micros for 1 unit, mode=%d: got %d, %v; want %d", c.mode, got, err, c.want)
		}
	}
	// Half-up ties away from zero; 1/4 at 2 units = 0.5 → up.
	quarter, _ := NewRate(1, 4)
	got, err := quarter.MicrosForUnits(2, RoundHalfUp)
	if err != nil || got != 1 {
		t.Errorf("0.5 tie RoundHalfUp = %d, %v; want 1", got, err)
	}
	got, err = quarter.MicrosForUnits(1, RoundHalfUp) // 0.25 → 0
	if err != nil || got != 0 {
		t.Errorf("0.25 RoundHalfUp = %d, %v; want 0", got, err)
	}
	// Unknown mode is an explicit error.
	if _, err := half.MicrosForUnits(1, RoundingMode(99)); !errors.Is(err, ErrRoundingMode) {
		t.Errorf("mode 99: err = %v, want ErrRoundingMode", err)
	}
	// Negative units rejected.
	if _, err := half.MicrosForUnits(-1, RoundDown); !errors.Is(err, ErrNegativeValue) {
		t.Errorf("units -1: err = %v, want ErrNegativeValue", err)
	}
}

func TestRateOverflowExtremes(t *testing.T) {
	// units × Num overflows int64 but the exact rational result fits —
	// big.Int intermediates must not lose it.
	r, err := NewRate(math.MaxInt64, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.MicrosForUnits(2, RoundDown)
	if err != nil {
		t.Fatalf("MaxInt64/2*2: %v", err)
	}
	if got != math.MaxInt64 { // (MaxInt64 × 2) / 2 == MaxInt64 exactly
		t.Errorf("got %d, want %d", got, int64(math.MaxInt64))
	}
	// True overflow: result exceeds int64.
	r2, _ := NewRate(math.MaxInt64, 1)
	if _, err := r2.MicrosForUnits(2, RoundDown); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64*2: err = %v, want ErrOverflow", err)
	}
	if _, err := NewRate(1, 0); !errors.Is(err, ErrInvalidRate) {
		t.Errorf("den=0: err = %v, want ErrInvalidRate", err)
	}
	if _, err := NewRate(-1, 2); !errors.Is(err, ErrInvalidRate) {
		t.Errorf("num=-1: err = %v, want ErrInvalidRate", err)
	}
}

func TestRatePerMillion(t *testing.T) {
	// Storage convention: micros per 1M tokens (inference_price_versions).
	r, err := RatePerMillion(2_000_000) // 2 credits per 1M tokens
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ChargeUnits(500_000, RoundDown) // half a million → 1 credit
	if err != nil || got != 1_000_000 {
		t.Errorf("500k tokens = %d microcredits, %v; want 1000000", got, err)
	}
	m, err := r.ChargeUnitsMoney(500_000, "USD", RoundUp)
	if err != nil {
		t.Fatal(err)
	}
	if m.Currency != "USD" || m.Micros != 1_000_000 {
		t.Errorf("money = %+v, want 1000000 USD micros", m)
	}
	if _, err := r.ChargeUnitsMoney(1, "usd", RoundUp); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("lowercase currency: err = %v, want ErrInvalidCurrency", err)
	}
}
