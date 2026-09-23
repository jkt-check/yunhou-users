package domain

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Microcredit is the integer micro-credit unit for quota (设计 §7.1:
// 额度使用整数微额度). 1 credit = 1_000_000 microcredits. All quota
// accumulation (used/reserved/budget) happens in this type; float64 must
// never appear on a credit path.
type Microcredit int64

// MicrocreditsPerCredit is the scale factor between one whole credit and
// its integer micro representation.
const MicrocreditsPerCredit = int64(1_000_000)

// Add returns a+b with overflow detection.
func (a Microcredit) Add(b Microcredit) (Microcredit, error) {
	if (b > 0 && a > Microcredit(math.MaxInt64)-b) || (b < 0 && a < Microcredit(math.MinInt64)-b) {
		return 0, ErrOverflow
	}
	return a + b, nil
}

// Sub returns a-b with overflow detection.
func (a Microcredit) Sub(b Microcredit) (Microcredit, error) {
	if b == Microcredit(math.MinInt64) {
		return 0, ErrOverflow
	}
	return a.Add(-b)
}

// NonNegative rejects negative credits (used/reserved/limit semantics).
func (a Microcredit) NonNegative() error {
	if a < 0 {
		return fmt.Errorf("%w: %d", ErrNegativeValue, int64(a))
	}
	return nil
}

// Money is a fixed-point micro-amount carrying its currency (设计 §7.1:
// 金额使用带币种的定点微金额). 1 unit of currency = 1_000_000 micros.
// The zero value has an empty currency and is invalid for arithmetic.
type Money struct {
	Micros   int64
	Currency string
}

// NewMoney validates the currency (ISO-4217 uppercase triple) and returns
// the Money. Negative amounts are rejected: direction is carried by the
// ledger entry type, not by the sign.
func NewMoney(micros int64, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, currency)
	}
	if micros < 0 {
		return Money{}, fmt.Errorf("%w: %d", ErrNegativeValue, micros)
	}
	return Money{Micros: micros, Currency: currency}, nil
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// Add sums two amounts of the same currency with overflow detection.
// Mixed currencies are a hard error (设计 §7.1: 跨币种统计必须显式兑换).
func (a Money) Add(b Money) (Money, error) {
	if a.Currency != b.Currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	if b.Micros > math.MaxInt64-a.Micros {
		return Money{}, ErrOverflow
	}
	return Money{Micros: a.Micros + b.Micros, Currency: a.Currency}, nil
}

// Sub subtracts b from a (same currency). The result may not go negative:
// the ledger never hides an overdraft (设计 §7.2 不隐藏负差额).
func (a Money) Sub(b Money) (Money, error) {
	if a.Currency != b.Currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	if b.Micros > a.Micros {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrNegativeValue, a.Micros, b.Micros)
	}
	return Money{Micros: a.Micros - b.Micros, Currency: a.Currency}, nil
}

// RoundingMode pins the rounding direction at the exact point a rational
// price is materialized into integer micro-units (设计 §7.1: 明确取整位置
// 和方向).
type RoundingMode int

const (
	// RoundDown truncates toward zero (conservative for cost estimates).
	RoundDown RoundingMode = iota
	// RoundUp rounds away from zero (conservative for customer charges:
	// the customer never pays less than the exact price).
	RoundUp
	// RoundHalfUp rounds half away from zero (nearest, ties up).
	RoundHalfUp
)

// Rate is an exact rational price: Num/Den micro-units per unit of usage
// (e.g. microcredits per token). Invariants: Den > 0, Num >= 0.
// Rational arithmetic keeps every intermediate exact — no float64, ever.
type Rate struct {
	Num int64
	Den int64
}

// NewRate validates the invariants.
func NewRate(num, den int64) (Rate, error) {
	if den <= 0 || num < 0 {
		return Rate{}, fmt.Errorf("%w: %d/%d", ErrInvalidRate, num, den)
	}
	return Rate{Num: num, Den: den}, nil
}

// RatePerMillion builds a Rate from a per-million-units price (the storage
// convention of inference_price_versions: micros per 1M tokens).
func RatePerMillion(microsPerMtok int64) (Rate, error) {
	return NewRate(microsPerMtok, 1_000_000)
}

// ParseDecimal parses a non-negative decimal literal ("12", "12.34",
// "0.000001") into an exact Rate. Exponent notation and signs are
// rejected — prices are plain decimals.
func ParseDecimal(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rate{}, fmt.Errorf("%w: empty", ErrInvalidDecimal)
	}
	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Rate{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
		}
	}
	if intPart == "" && fracPart == "" {
		return Rate{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	digits := intPart + fracPart
	for _, r := range digits {
		if r < '0' || r > '9' {
			return Rate{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
		}
	}
	num, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Rate{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fracPart))), nil)
	if !num.IsInt64() || !den.IsInt64() {
		return Rate{}, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	return NewRate(num.Int64(), den.Int64())
}

// MicrosForUnits computes ceil/floor/round(units × Num / Den) in exact
// rational arithmetic and range-checks the result into int64.
// units must be >= 0 (usage counts are non-negative).
func (r Rate) MicrosForUnits(units int64, mode RoundingMode) (int64, error) {
	if units < 0 {
		return 0, fmt.Errorf("%w: units %d", ErrNegativeValue, units)
	}
	if r.Den <= 0 || r.Num < 0 {
		return 0, fmt.Errorf("%w: %d/%d", ErrInvalidRate, r.Num, r.Den)
	}
	num := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(units))
	den := big.NewInt(r.Den)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	switch mode {
	case RoundDown:
		// q is already truncated toward zero (all operands non-negative).
	case RoundUp:
		if rem.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
	case RoundHalfUp:
		doubled := new(big.Int).Lsh(rem, 1)
		if doubled.Cmp(den) >= 0 {
			q.Add(q, big.NewInt(1))
		}
	default:
		return 0, fmt.Errorf("%w: %d", ErrRoundingMode, int(mode))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%w: %d units at %d/%d", ErrOverflow, units, r.Num, r.Den)
	}
	return q.Int64(), nil
}

// ChargeUnits prices a usage count in microcredits.
func (r Rate) ChargeUnits(units int64, mode RoundingMode) (Microcredit, error) {
	micros, err := r.MicrosForUnits(units, mode)
	if err != nil {
		return 0, err
	}
	return Microcredit(micros), nil
}

// ChargeUnitsMoney prices a usage count in micro-money of the given
// currency.
func (r Rate) ChargeUnitsMoney(units int64, currency string, mode RoundingMode) (Money, error) {
	micros, err := r.MicrosForUnits(units, mode)
	if err != nil {
		return Money{}, err
	}
	return NewMoney(micros, currency)
}
