package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
)

// wallet_reads_test.go — Task 16 覆盖率补强：钱包读取面（余额列表/流水
// 分页/补偿幂等键查找/PAYG 配置往返）的真实库行为。

func TestWalletReads_BalancesEntriesAdjustmentPAYG(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	fundWallet(t, s, f.accountID, "CNY", 50_000, 8_000)
	fundWallet(t, s, f.accountID, "USD", 12_000, 0)

	// 余额列表：每 (账户, 币种) 一行，派生余额 = 分录合计。
	views, err := s.ListWalletBalances(ctx, f.accountID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("balances = %d, want 2 (CNY+USD 各一行)", len(views))
	}
	totals := map[string]int64{}
	for _, v := range views {
		totals[v.Balance.Currency] = v.Balance.CashAvailable + v.Balance.BonusAvailable
	}
	if totals["CNY"] != 58_000 || totals["USD"] != 12_000 {
		t.Errorf("derived totals = %v", totals)
	}
	// 单币种视图。
	one, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now().UTC())
	if err != nil || one.Balance.CashAvailable+one.Balance.BonusAvailable != 58_000 {
		t.Fatalf("single balance = %+v/%v", one, err)
	}

	// 流水：新→旧分页（afterID keyset），分录类型齐全。
	entries, err := s.ListWalletEntries(ctx, f.accountID, "CNY", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (topup+bonus)", len(entries))
	}
	kinds := map[string]bool{}
	for _, e := range entries {
		kinds[e.EntryType] = true
	}
	if !kinds["topup"] || !kinds["adjustment"] {
		t.Errorf("entry kinds = %v", kinds)
	}
	// 第二页（afterID=首行 ID）为空。
	entries2, err := s.ListWalletEntries(ctx, f.accountID, "CNY", entries[len(entries)-1].ID, 10)
	if err != nil || len(entries2) != 0 {
		t.Fatalf("entries page2 = %d/%v", len(entries2), err)
	}

	// 补偿幂等键查找。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyWalletAdjustmentTx(ctx, uow, WalletAdjustmentCommand{
		AccountID: f.accountID, Currency: "CNY",
		Source: accounting.WalletBonus, Direction: accounting.DirCredit,
		AmountMicros: 999, Reason: "comp", OperatorSubject: "user:ops@app:test",
		IdempotencyKey: "comp-key-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	adj, err := s.GetAdjustmentByIdempotencyKey(ctx, "comp-key-1")
	if err != nil || adj.AmountMicros != 999 {
		t.Fatalf("adjustment = %+v/%v", adj, err)
	}
	if _, err := s.GetAdjustmentByIdempotencyKey(ctx, "no-such-key"); err == nil {
		t.Fatal("missing key must error")
	}

	// PAYG 配置：未发布 → NotFound；发布 → 往返一致（含幂等更新）。
	if _, err := s.GetPAYGConfig(ctx); err == nil {
		t.Fatal("unpublished payg config must error")
	}
	pol := &PolicyVersion{
		Name: "payg-pol", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: micro(1000), WeeklyLimit: micro(2000), MonthlyLimit: micro(3000),
		Status: "published",
	}
	if err := s.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.PutPAYGConfig(ctx, pol.ID, []string{"glm-4.6"}, "user:ops@app:test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PolicyVersionID != pol.ID || len(cfg.ModelIDs) != 1 {
		t.Fatalf("payg config = %+v", cfg)
	}
	cfg2, err := s.GetPAYGConfig(ctx)
	if err != nil || cfg2.PolicyVersionID != pol.ID {
		t.Fatalf("payg get = %+v/%v", cfg2, err)
	}
}
