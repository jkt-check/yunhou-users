package accounting

import (
	"errors"
	"math"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestCheckWalletSpend_IntegerBoundaries(t *testing.T) {
	limit := int64(math.MaxInt64)
	settings := OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit}
	for _, tc := range []struct {
		name  string
		spent int64
		hold  int64
		want  error
	}{
		{"overflow exceeds cap", math.MaxInt64 - 5, 10, ErrSpendLimitExceeded},
		{"exact cap", math.MaxInt64 - 5, 5, nil},
		{"zero at cap", math.MaxInt64, 0, nil},
		{"one above cap", math.MaxInt64, 1, ErrSpendLimitExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckWalletSpend(settings, tc.spent, tc.hold)
			if !errors.Is(err, tc.want) {
				t.Fatalf("spend gate = %v, want %v", err, tc.want)
			}
			if tc.want != nil && domain.CodeOf(err) != domain.CodeQuotaExceeded {
				t.Fatalf("spend gate code = %s, want quota_exceeded", domain.CodeOf(err))
			}
		})
	}
}

func TestSplitFreeze_IntegerBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                string
		bonus, cash, amount int64
		wantCash, wantBonus int64
		wantInsufficient    bool
	}{
		{"positive total exceeds int64", math.MaxInt64, math.MaxInt64, 1, 0, 1, false},
		{"maximum bonus first", math.MaxInt64, 1, math.MaxInt64, 0, math.MaxInt64, false},
		{"two deficits underflow", -math.MaxInt64, -math.MaxInt64, 1, 0, 0, true},
		{"minimum and zero", math.MinInt64, 0, 1, 0, 0, true},
		{"opposite sources insufficient", math.MaxInt64, math.MinInt64, 1, 0, 0, true},
		{"bonus deficit exact net", -math.MaxInt64 + 1, math.MaxInt64, 1, 1, 0, false},
		{"cash deficit exact net", math.MaxInt64, -math.MaxInt64 + 1, 1, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cash, bonus, err := SplitFreeze(tc.bonus, tc.cash, tc.amount)
			if tc.wantInsufficient {
				if !errors.Is(err, ErrInsufficientBalance) || cash != 0 || bonus != 0 {
					t.Fatalf("freeze = %d/%d, %v; want insufficient with no split", cash, bonus, err)
				}
				return
			}
			if err != nil || cash != tc.wantCash || bonus != tc.wantBonus {
				t.Fatalf("freeze = %d/%d, %v; want %d/%d", cash, bonus, err, tc.wantCash, tc.wantBonus)
			}
		})
	}
}

func TestDeriveWalletBalance_IntegerBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		sums WalletSums
	}{
		{"cash underflow", WalletSums{CashDebit: math.MaxInt64, HeldCash: 2}},
		{"bonus underflow", WalletSums{BonusDebit: math.MaxInt64, HeldBonus: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			balance, err := DeriveWalletBalance("CNY", tc.sums)
			if !errors.Is(err, domain.ErrOverflow) {
				t.Fatalf("derive = %+v, %v; want ErrOverflow, never a wrapped positive balance", balance, err)
			}
		})
	}
	t.Run("minimum signed availability is representable", func(t *testing.T) {
		balance, err := DeriveWalletBalance("CNY", WalletSums{
			CashDebit: math.MaxInt64, HeldCash: 1,
			BonusDebit: math.MaxInt64, HeldBonus: 1,
		})
		if err != nil || balance.CashAvailable != math.MinInt64 || balance.BonusAvailable != math.MinInt64 {
			t.Fatalf("derive = %+v, %v; want both available balances MinInt64", balance, err)
		}
	})
}
