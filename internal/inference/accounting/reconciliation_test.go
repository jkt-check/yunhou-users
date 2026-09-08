package accounting

import (
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// reconciliation_test.go — 账本重建纯规则（Task 9 验收：由账本重建聚合值可
// 与窗口核对）。

func TestRebuildUsedMicros(t *testing.T) {
	facts := []LedgerFact{
		{Type: domain.LedgerCharge, AmountMicros: 100},                        // 原结算
		{Type: domain.LedgerCharge, AmountMicros: 0},                          // 零消费结算也落行
		{Type: domain.LedgerReversal, AmountMicros: 100},                      // 冲正原 charge
		{Type: domain.LedgerAdjustment, AmountMicros: 60, Direction: "debit"}, // 补差
		{Type: domain.LedgerCharge, AmountMicros: 25},                         // 另一请求
	}
	got, err := RebuildUsedMicros(facts)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got != 85 {
		t.Errorf("rebuilt = %d, want 85 (100 − 100 + 60 + 25)", got)
	}
}

func TestRebuildUsedMicros_CreditAndValidation(t *testing.T) {
	got, err := RebuildUsedMicros([]LedgerFact{
		{Type: domain.LedgerCharge, AmountMicros: 100},
		{Type: domain.LedgerAdjustment, AmountMicros: 30, Direction: "credit"},
	})
	if err != nil || got != 70 {
		t.Errorf("credit: %d/%v, want 70", got, err)
	}
	// 调整无 direction / 未知类型 / 负额 —— 全部拒绝而不是猜。
	for _, facts := range [][]LedgerFact{
		{{Type: domain.LedgerAdjustment, AmountMicros: 30}},
		{{Type: "bogus", AmountMicros: 1}},
		{{Type: domain.LedgerCharge, AmountMicros: -1}},
	} {
		if _, err := RebuildUsedMicros(facts); err == nil {
			t.Errorf("facts %+v must reject", facts)
		}
	}
}

func TestMismatches(t *testing.T) {
	all := []WindowReconciliation{
		{WindowID: "w1", Kind: domain.WindowFiveHour, StoredUsed: 100, RebuiltUsed: 100},
		{WindowID: "w2", Kind: domain.WindowWeekly, StoredUsed: 100, RebuiltUsed: 40},
		{WindowID: "w3", Kind: domain.WindowMonthly, StoredUsed: 0, RebuiltUsed: 10},
	}
	mm := Mismatches(all)
	if len(mm) != 2 || mm[0].WindowID != "w2" || mm[1].WindowID != "w3" {
		t.Fatalf("mismatches = %+v, want w2 and w3", mm)
	}
	if mm[0].Diff() != -60 || mm[1].Diff() != 10 {
		t.Errorf("diffs = %d/%d, want -60/10", mm[0].Diff(), mm[1].Diff())
	}
}
