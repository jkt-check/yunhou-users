package accounting

import (
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestSplitFreeze_BonusFirst(t *testing.T) {
	cash, bonus, err := SplitFreeze(300, 500, 400)
	if err != nil {
		t.Fatalf("split freeze: %v", err)
	}
	if bonus != 300 || cash != 100 {
		t.Fatalf("want bonus=300 cash=100, got bonus=%d cash=%d", bonus, cash)
	}
	// 赠送不足时全量用现金
	cash, bonus, err = SplitFreeze(0, 500, 400)
	if err != nil || bonus != 0 || cash != 400 {
		t.Fatalf("want bonus=0 cash=400, got %d/%d err=%v", bonus, cash, err)
	}
}

func TestSplitFreeze_InsufficientBalance(t *testing.T) {
	_, _, err := SplitFreeze(100, 100, 201)
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	if domain.CodeOf(err) != domain.CodeInsufficientBalance {
		t.Fatalf("want code insufficient_balance, got %v", domain.CodeOf(err))
	}
	// 恰好够 = 放行（最后一份余额可冻结）
	if _, _, err := SplitFreeze(100, 100, 200); err != nil {
		t.Fatalf("exact-balance freeze must succeed: %v", err)
	}
	if _, _, err := SplitFreeze(0, 10, 0); err == nil {
		t.Fatal("zero freeze must reject")
	}
}

// 评审轮1 I1：派生余额为负（冲正已消费的 bonus 充值等诚实账本形态）时，
// 拆分两侧钳到零下界——不得产出负数拆分撞 hold CHECK 把现金充足的客户
// 全部 400；负缺口继续压总额度（raw 合计口径，不超扣）。
func TestSplitFreeze_NegativeDerivedBalancesClamp(t *testing.T) {
	// bonus 为负、现金充足：冻结成功，拆分非负，现金足额扣。
	cash, bonus, err := SplitFreeze(-300, 500, 200)
	if err != nil {
		t.Fatalf("negative bonus with ample cash must freeze: %v", err)
	}
	if bonus != 0 || cash != 200 {
		t.Fatalf("want bonus=0 cash=200, got bonus=%d cash=%d", bonus, cash)
	}
	// 负缺口压总可用量：-300 + 500 = 200，冻 201 必须拒绝（不超扣）。
	if _, _, err := SplitFreeze(-300, 500, 201); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("negative deficit must still gate the total, got %v", err)
	}
	// cash 为负（退款后消费）、bonus 充足：对称钳零。
	cash, bonus, err = SplitFreeze(500, -100, 400)
	if err != nil {
		t.Fatalf("negative cash with ample bonus must freeze: %v", err)
	}
	if bonus != 400 || cash != 0 {
		t.Fatalf("want bonus=400 cash=0, got bonus=%d cash=%d", bonus, cash)
	}
	// 两侧皆负 → 永远不足。
	if _, _, err := SplitFreeze(-1, -1, 1); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("both negative must be insufficient, got %v", err)
	}
}

func TestSplitConsume_WithinHold(t *testing.T) {
	// hold: cash=100 bonus=300；charge=250 → bonus 250, cash 0
	cash, bonus, extra, err := SplitConsume(100, 300, 250)
	if err != nil {
		t.Fatalf("split consume: %v", err)
	}
	if bonus != 250 || cash != 0 || extra != 0 {
		t.Fatalf("want bonus=250 cash=0 extra=0, got %d/%d/%d", bonus, cash, extra)
	}
	// charge=350 → bonus 300 + cash 50
	cash, bonus, extra, _ = SplitConsume(100, 300, 350)
	if bonus != 300 || cash != 50 || extra != 0 {
		t.Fatalf("want bonus=300 cash=50 extra=0, got %d/%d/%d", bonus, cash, extra)
	}
}

func TestSplitConsume_OverHoldDebitsCash(t *testing.T) {
	// 超占（charge > hold）：超出部分从现金诚实借记（可透支为负，阻断后续冻结）
	cash, bonus, extra, err := SplitConsume(100, 300, 500)
	if err != nil {
		t.Fatalf("split consume: %v", err)
	}
	if bonus != 300 || cash != 100 || extra != 100 {
		t.Fatalf("want bonus=300 cash=100 extra=100, got %d/%d/%d", bonus, cash, extra)
	}
}

func TestSplitRelease_Symmetry(t *testing.T) {
	cash, bonus, err := SplitRelease(100, 300, 50, 250)
	if err != nil {
		t.Fatalf("split release: %v", err)
	}
	if cash != 50 || bonus != 50 {
		t.Fatalf("want cash=50 bonus=50, got %d/%d", cash, bonus)
	}
	// 消费超过冻结组成 → 拒绝（调用方 bug 不得静默归零）
	if _, _, err := SplitRelease(100, 300, 101, 0); err == nil {
		t.Fatal("consume beyond hold composition must reject")
	}
}

func TestValidateWalletEntry_MirrorsChecks(t *testing.T) {
	must := func(micros int64) domain.Money {
		m, err := domain.NewMoney(micros, "CNY")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ok := []WalletEntrySpec{
		{Type: EntryTopup, Direction: DirCredit, Source: WalletCash, Amount: must(1)},
		{Type: EntryBonus, Direction: DirCredit, Source: WalletBonus, Amount: must(1)},
		{Type: EntryConsume, Direction: DirDebit, Source: WalletBonus, Amount: must(1)},
		{Type: EntryRefund, Direction: DirDebit, Source: WalletCash, Amount: must(1)},
		{Type: EntryAdjustment, Direction: DirCredit, Source: WalletBonus, Amount: must(1)},
	}
	for i, e := range ok {
		if err := ValidateWalletEntry(e); err != nil {
			t.Fatalf("case %d should pass: %v", i, err)
		}
	}
	// 赠送不得现金退款
	err := ValidateWalletEntry(WalletEntrySpec{Type: EntryRefund, Direction: DirDebit, Source: WalletBonus, Amount: must(1)})
	if !errors.Is(err, ErrBonusNotRefundable) {
		t.Fatalf("want ErrBonusNotRefundable, got %v", err)
	}
	// topup 不能借方/不能赠送来源
	if err := ValidateWalletEntry(WalletEntrySpec{Type: EntryTopup, Direction: DirDebit, Source: WalletCash, Amount: must(1)}); err == nil {
		t.Fatal("debit topup must reject")
	}
	// 零额拒绝
	if err := ValidateWalletEntry(WalletEntrySpec{Type: EntryConsume, Direction: DirDebit, Source: WalletCash, Amount: must(0)}); err == nil {
		t.Fatal("zero amount must reject")
	}
}

func TestReversalOf_OppositeDirection(t *testing.T) {
	m, _ := domain.NewMoney(42, "USD")
	orig := WalletEntrySpec{Type: EntryConsume, Direction: DirDebit, Source: WalletCash, Amount: m}
	rev, err := ReversalOf(orig)
	if err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if rev.Type != EntryReversal || rev.Direction != DirCredit || rev.Source != WalletCash || rev.Amount != m {
		t.Fatalf("bad reversal: %+v", rev)
	}
	if err := ValidateWalletEntry(rev); err != nil {
		t.Fatalf("reversal must validate: %v", err)
	}
	// 冲正不可再冲正（用调整纠正）
	if _, err := ReversalOf(rev); err == nil {
		t.Fatal("reversal of reversal must reject")
	}
}

func TestDeriveWalletBalance(t *testing.T) {
	b, err := DeriveWalletBalance("CNY", WalletSums{
		CashCredit: 1000, CashDebit: 300, HeldCash: 100,
		BonusCredit: 500, BonusDebit: 200, HeldBonus: 50,
	})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if b.CashAvailable != 600 || b.BonusAvailable != 250 || b.CashHeld != 100 || b.BonusHeld != 50 {
		t.Fatalf("bad derived balance: %+v", b)
	}
	// 现金退款落在资金已消费/冻结后可转负 —— 如实呈现，不隐藏负差额
	b, _ = DeriveWalletBalance("CNY", WalletSums{CashCredit: 100, CashDebit: 300})
	if b.CashAvailable != -200 {
		t.Fatalf("negative cash must show as-is, got %d", b.CashAvailable)
	}
	if _, err := DeriveWalletBalance("cny", WalletSums{}); err == nil {
		t.Fatal("invalid currency must reject")
	}
}

func TestMicrosFromDecimalMajor_Exact(t *testing.T) {
	cases := map[string]int64{
		"29.99":   29_990_000,
		"0":       0,
		"1":       1_000_000,
		"0.000001": 1,
		"100":     100_000_000,
	}
	for dec, want := range cases {
		got, err := MicrosFromDecimalMajor(dec, "CNY")
		if err != nil {
			t.Fatalf("%s: %v", dec, err)
		}
		if got != want {
			t.Fatalf("%s: want %d got %d", dec, want, got)
		}
	}
	// 亚微精度拒绝（严格转换，不四舍五入）
	if _, err := MicrosFromDecimalMajor("0.0000001", "CNY"); err == nil {
		t.Fatal("sub-micro precision must reject")
	}
	if _, err := MicrosFromDecimalMajor("-1", "CNY"); err == nil {
		t.Fatal("negative must reject")
	}
	if _, err := MicrosFromDecimalMajor("1.0", "cny"); err == nil {
		t.Fatal("invalid currency must reject")
	}
}

func TestMonthBoundsUTC(t *testing.T) {
	start, end := MonthBoundsUTC(time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC))
	if start != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) || end != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("bad month bounds: %v %v", start, end)
	}
	// 跨年为界的月份推进
	_, end = MonthBoundsUTC(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if end != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("year boundary: %v", end)
	}
}

func TestCheckWalletSpend_Gate(t *testing.T) {
	// 默认关：拒绝
	if err := CheckWalletSpend(OverageSettings{}, 0, 1); !errors.Is(err, ErrOverageDisabled) {
		t.Fatalf("disabled must reject with ErrOverageDisabled, got %v", err)
	}
	// 开启无上限 → 配置非法
	if err := (OverageSettings{Enabled: true}).Validate(); err == nil {
		t.Fatal("enabled without limit must reject")
	}
	limit := int64(1000)
	s := OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit}
	if err := CheckWalletSpend(s, 600, 400); err != nil {
		t.Fatalf("within limit must pass: %v", err)
	}
	if err := CheckWalletSpend(s, 600, 401); !errors.Is(err, ErrSpendLimitExceeded) {
		t.Fatalf("over limit must reject with ErrSpendLimitExceeded, got %v", err)
	}
	// 恰好到顶放行
	if err := CheckWalletSpend(s, 999, 1); err != nil {
		t.Fatalf("exact limit must pass: %v", err)
	}
}

func TestAuditForChange(t *testing.T) {
	limit := int64(100)
	// 新建 + 开启 + 设上限：create/enable/set_limit 三条
	rows := AuditForChange(false, OverageSettings{},
		OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit}, "user:u1")
	if len(rows) != 3 {
		t.Fatalf("want 3 audit rows, got %d (%+v)", len(rows), rows)
	}
	if rows[0].Action != WalletAuditCreate || rows[1].Action != WalletAuditEnableOverage || rows[2].Action != WalletAuditSetSpendLimit {
		t.Fatalf("bad audit actions: %+v", rows)
	}
	// 无变化 → 无审计行
	rows = AuditForChange(true, OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit},
		OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit}, "user:u1")
	if len(rows) != 0 {
		t.Fatalf("no-op must audit nothing, got %+v", rows)
	}
	// 关闭 → disable 一条
	rows = AuditForChange(true, OverageSettings{Enabled: true, MonthlySpendLimitMicros: &limit},
		OverageSettings{Enabled: false, MonthlySpendLimitMicros: &limit}, "user:u1")
	if len(rows) != 1 || rows[0].Action != WalletAuditDisableOverage {
		t.Fatalf("bad disable audit: %+v", rows)
	}
}
