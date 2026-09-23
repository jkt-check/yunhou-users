package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// wallet_repo_test.go — Task 14 验收测试（真实 PostgreSQL）：
//   - 并发抢最后一份余额不双花（多连接并发冻结，DB 层原子守卫）；
//   - 未开启套餐外时冻结被拒绝且余额一分不动；
//   - 调价不影响在途冻结（结算按入场钉住的价格版本）；
//   - 充值/消费/退款/调整借贷平衡，客户展示 = 账本派生；
//   - 重复回调/退款幂等（业务键）；
//   - 赠送余额不得现金退款；
//   - 冲正一条分录至多一次；
//   - PAYG 显式权益记录的建立/复活/未配置拒绝。

// seedMoneyPrice inserts one sale_money price revision (1 token = 1 money
// micro of CNY) and returns its id.
func seedMoneyPrice(t *testing.T, s *Store, modelID, currency string, revision int, from time.Time) string {
	t.Helper()
	pv := &PriceVersion{
		ModelID: modelID, Kind: PriceSaleMoney, Unit: "micromoney", Currency: currency,
		InputPerMtok: 1_000_000, OutputPerMtok: 1_000_000, Revision: revision,
		EffectiveFrom: from,
	}
	if err := s.InsertPriceVersion(context.Background(), pv); err != nil {
		t.Fatalf("insert money price: %v", err)
	}
	return pv.ID
}

// fundWallet credits cash (topup) and bonus (adjustment) balances.
func fundWallet(t *testing.T, s *Store, accountID, currency string, cashMicros, bonusMicros int64) {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cashMicros > 0 {
		if err := s.CreditTopupTx(ctx, uow, WalletTopupCommand{
			AccountID: accountID, Currency: currency, AmountMicros: cashMicros,
			PaymentID: "pay-" + uuid.NewString(), OrderID: uuid.NewString(),
		}); err != nil {
			t.Fatalf("topup: %v", err)
		}
	}
	if bonusMicros > 0 {
		if _, err := s.ApplyWalletAdjustmentTx(ctx, uow, WalletAdjustmentCommand{
			AccountID: accountID, Currency: currency,
			Source: accounting.WalletBonus, Direction: accounting.DirCredit,
			AmountMicros: bonusMicros, Reason: "welcome gift",
			OperatorSubject: "user:ops@app:test", IdempotencyKey: "gift-" + uuid.NewString(),
		}); err != nil {
			t.Fatalf("bonus grant: %v", err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// enableOverage flips the wallet's explicit spend gate.
func enableOverage(t *testing.T, s *Store, accountID, currency string, limit int64) {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOverageTx(ctx, uow, accountID, currency, true, &limit, "user:test"); err != nil {
		t.Fatalf("enable overage: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// reserveWalletCmd builds a wallet admission for `holdMicros` money micros.
func reserveWalletCmd(t *testing.T, s *Store, f fixture, walletID, priceID string, holdMicros int64) domain.ReserveWalletCommand {
	t.Helper()
	at := time.Now().UTC()
	start, end := accounting.MonthBoundsUTC(at)
	return domain.ReserveWalletCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, PolicyVersionID: f.policyID,
		},
		WalletID: walletID, HoldMicros: holdMicros, PriceVersionID: priceID,
		AdmittedAt: at, MonthStart: start, MonthEnd: end,
	}
}

// TestWalletFreezeConcurrentLastBalance_NoDoubleSpend: 余额恰好够 N 份冻
// 结，2N 个并发事务（多连接）抢——恰好 N 个成功，派生余额归零不为负，
// 冻结总额 = N×hold（裁决 8：并发双花守卫在 DB 层原子）。
func TestWalletFreezeConcurrentLastBalance_NoDoubleSpend(t *testing.T) {
	db, s := testDB(t)
	db.SetMaxOpenConns(16)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))

	const hold = int64(1_000_000) // 1 CNY
	const n = 5
	fundWallet(t, s, f.accountID, "CNY", n*hold, 0) // 恰好 n 份
	enableOverage(t, s, f.accountID, "CNY", 100*hold)

	w, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2*n)
	for i := 0; i < 2*n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uow, err := s.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			_, err = s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, hold))
			if err != nil {
				_ = uow.Rollback(ctx)
				results <- err
				return
			}
			if err := uow.Commit(ctx); err != nil {
				results <- err
				return
			}
			results <- nil
		}()
	}
	wg.Wait()
	close(results)

	var ok, insufficient int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, accounting.ErrInsufficientBalance):
			insufficient++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != n || insufficient != n {
		t.Fatalf("ok=%d insufficient=%d, want %d/%d (不超扣)", ok, insufficient, n, n)
	}

	// 派生余额：现金净额 n×hold，冻结 n×hold → 可用 0，不为负。
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 0 || view.Balance.CashHeld != n*hold {
		t.Fatalf("derived balance = %+v, want cash available 0 held %d", view.Balance, n*hold)
	}
	// 再冻结一份也必须拒绝（最后一份余额已被抢完）。
	uow, _ := s.Begin(ctx)
	_, err = s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, hold))
	_ = uow.Rollback(ctx)
	if !errors.Is(err, accounting.ErrInsufficientBalance) {
		t.Fatalf("post-exhaustion freeze must reject, got %v", err)
	}
}

// TestWalletOverageDisabled_NothingMoves: 未开启套餐外消费时冻结被拒绝
// （ErrOverageDisabled），余额与 hold 一分不动（裁决 4：默认关）。
func TestWalletOverageDisabled_NothingMoves(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 10_000_000, 5_000_000)

	w, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}
	uow, _ := s.Begin(ctx)
	_, err = s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 1_000_000))
	_ = uow.Rollback(ctx)
	if !errors.Is(err, accounting.ErrOverageDisabled) {
		t.Fatalf("want ErrOverageDisabled, got %v", err)
	}

	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 10_000_000 || view.Balance.BonusAvailable != 5_000_000 ||
		view.Balance.CashHeld != 0 || view.Balance.BonusHeld != 0 {
		t.Fatalf("wallet must be untouched, got %+v", view.Balance)
	}
	var holds int
	if err := s.db.Get(&holds, `SELECT COUNT(*) FROM inference_wallet_holds`); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("no hold row may exist, got %d", holds)
	}
}

// TestWalletSettle_PricePinnedAtFreeze: 冻结按价格版本 r1；发布后生效的
// r2（价格翻倍）不改变在途冻结的结算价——账本 charge 记录 r1 且金额按 r1
// 费率（裁决 5/设计 §7.1）。
func TestWalletSettle_PricePinnedAtFreeze(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	now := time.Now().UTC()
	r1 := seedMoneyPrice(t, s, f.modelID, "CNY", 1, now.Add(-time.Hour)) // 1 micro/token
	fundWallet(t, s, f.accountID, "CNY", 100_000_000, 0)
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}

	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, r1, 2_000_000))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 调价：r2 起生效，费率 4×（micros/mtok）。
	seedMoneyPrice(t, s, f.modelID, "CNY", 2, now.Add(-time.Minute))
	latest, err := s.LatestPriceVersion(ctx, f.modelID, string(accounting.PriceSaleMoney), now)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID == r1 {
		t.Fatal("r2 should now be the effective revision")
	}

	// 结算：网关在入场时已 pin r1；实际用量 1000 in / 500 out，按 r1 =
	// 1500 微金额（r2 会是 6000——调价不得改写已 pin 的结算）。
	in, out := int64(1000), int64(500)
	pv1, err := s.GetPriceVersion(ctx, r1)
	if err != nil {
		t.Fatal(err)
	}
	pure1, err := pv1.Pure()
	if err != nil {
		t.Fatal(err)
	}
	charge, err := pure1.Quote(domain.UsageRecord{
		RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if charge.Money == nil || charge.Money.Micros != 1500 {
		t.Fatalf("r1 quote = %+v, want 1500 CNY micros", charge)
	}

	uow2, _ := s.Begin(ctx)
	err = s.Settle(ctx, uow2, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: domain.Microcredit(charge.Money.Micros),
		WalletCharge: charge.Money,
		SettledAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 账本 charge：micromoney + 币种 + 钉住的 r1。
	var unit, currency, pvID string
	var amount int64
	if err := s.db.QueryRow(
		`SELECT unit, currency, price_version_id, amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, adm.RequestID).
		Scan(&unit, &currency, &pvID, &amount); err != nil {
		t.Fatal(err)
	}
	if unit != "micromoney" || currency != "CNY" || pvID != r1 || amount != 1500 {
		t.Fatalf("ledger charge = %s %s %s %d, want micromoney/CNY/r1/1500", unit, currency, pvID, amount)
	}
	// 钱包：消费 1500（现金），冻结释放其余；派生余额 = 100_000_000-1500。
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 100_000_000-1500 || view.Balance.CashHeld != 0 {
		t.Fatalf("post-settle balance = %+v", view.Balance)
	}
	// 请求终态：charge_source=wallet，settled 为微金额。
	req, err := s.GetRequest(ctx, adm.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.ChargeSource != domain.ChargeSourceWallet || req.Status != domain.ReqSettled ||
		req.SettledMicros == nil || *req.SettledMicros != 1500 {
		t.Fatalf("request = %+v", req)
	}
	// 重复结算撞唯一键（幂等）。
	uow3, _ := s.Begin(ctx)
	err = s.Settle(ctx, uow3, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out}, Revision: 2,
		},
		ChargeMicros: 1500, WalletCharge: charge.Money, SettledAt: time.Now().UTC(),
	})
	_ = uow3.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate settle must conflict, got %v", err)
	}
}

// TestWalletLedgerBalance_DisplayEqualsLedger: 充值/赠送/消费/退款/调整全
// 部入账后，客户展示（WalletBalance）与逐条分录手工汇总一致（裁决 3：
// 客户展示 = 账本派生，禁止独立缓存余额）。
func TestWalletLedgerBalance_DisplayEqualsLedger(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 50_000_000, 10_000_000) // 50 CNY 现金 + 10 CNY 赠送
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	// 消费一笔（hold 2 CNY，结算 1.5 CNY：赠送先扣）。
	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 2_000_000))
	if err != nil {
		t.Fatal(err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(1000), int64(500)
	chargeMoney, _ := domain.NewMoney(1_500_000, "CNY")
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 1_500_000, WalletCharge: &chargeMoney, SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 现金退款 5 CNY（原路退）。
	uow2, _ := s.Begin(ctx)
	if err := s.RefundWalletTx(ctx, uow2, WalletRefundCommand{
		AccountID: f.accountID, Currency: "CNY", AmountMicros: 5_000_000,
		RefundID: "ref-" + uuid.NewString(), PaymentID: "pay-x",
	}); err != nil {
		t.Fatalf("refund: %v", err)
	}
	// 运营现金扣回 1 CNY（调整借方）。
	if _, err := s.ApplyWalletAdjustmentTx(ctx, uow2, WalletAdjustmentCommand{
		AccountID: f.accountID, Currency: "CNY",
		Source: accounting.WalletCash, Direction: accounting.DirDebit,
		AmountMicros: 1_000_000, Reason: "clawback",
		OperatorSubject: "user:ops@app:test", IdempotencyKey: "adj-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 手工汇总（账本派生口径）：bonus 消费 1.5（赠送先扣），现金未动消费；
	// 现金：+50 −5(refund) −1(adj) = 44；赠送：+10 −1.5 = 8.5。
	entries, err := s.ListWalletEntries(ctx, f.accountID, "CNY", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var cashCr, cashDr, bonusCr, bonusDr int64
	for _, e := range entries {
		if e.Currency != "CNY" {
			t.Fatalf("cross-currency entry leaked: %+v", e)
		}
		switch {
		case e.Source == "cash" && e.Direction == "credit":
			cashCr += e.AmountMicros
		case e.Source == "cash":
			cashDr += e.AmountMicros
		case e.Direction == "credit":
			bonusCr += e.AmountMicros
		default:
			bonusDr += e.AmountMicros
		}
	}
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != cashCr-cashDr || view.Balance.BonusAvailable != bonusCr-bonusDr {
		t.Fatalf("display %v != ledger sums cash %d-%d bonus %d-%d",
			view.Balance, cashCr, cashDr, bonusCr, bonusDr)
	}
	if view.Balance.CashAvailable != 44_000_000 || view.Balance.BonusAvailable != 8_500_000 {
		t.Fatalf("want cash 44.0 / bonus 8.5 CNY, got %+v", view.Balance)
	}
	// 消费分录来源拆分：bonus 1.5 CNY，无 cash consume（赠送先扣）。
	var bonusConsume, cashConsume int64
	for _, e := range entries {
		if e.EntryType == "consume" && e.Source == "bonus" {
			bonusConsume += e.AmountMicros
		}
		if e.EntryType == "consume" && e.Source == "cash" {
			cashConsume += e.AmountMicros
		}
	}
	if bonusConsume != 1_500_000 || cashConsume != 0 {
		t.Fatalf("consume split = bonus %d cash %d, want 1500000/0", bonusConsume, cashConsume)
	}
}

// TestWalletTopupRefund_IdempotentReplay: 重复回调（同一 payment 两次充值
// 投递、同一 refund 两次退款投递）只生效一次（裁决 3/7 幂等业务键）。
func TestWalletTopupRefund_IdempotentReplay(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 每笔移动一个事务（与 worker 的消息处理同构）：业务键冲突 =
	// 重放，回滚后按"已入账"收敛。
	applyTopup := func() (replayed bool) {
		uow, _ := s.Begin(ctx)
		err := s.CreditTopupTx(ctx, uow, WalletTopupCommand{
			AccountID: f.accountID, Currency: "CNY", AmountMicros: 30_000_000,
			PaymentID: "pay-dup", OrderID: uuid.NewString(),
		})
		if err != nil {
			_ = uow.Rollback(ctx)
			if domain.CodeOf(err) == domain.CodeConflict {
				return true
			}
			t.Fatalf("topup: %v", err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return false
	}
	applyRefund := func() (replayed bool) {
		uow, _ := s.Begin(ctx)
		err := s.RefundWalletTx(ctx, uow, WalletRefundCommand{
			AccountID: f.accountID, Currency: "CNY", AmountMicros: 5_000_000,
			RefundID: "ref-dup", PaymentID: "pay-dup",
		})
		if err != nil {
			_ = uow.Rollback(ctx)
			if domain.CodeOf(err) == domain.CodeConflict {
				return true
			}
			t.Fatalf("refund: %v", err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return false
	}
	if applyTopup() || applyRefund() {
		t.Fatal("first delivery must apply")
	}
	if !applyTopup() || !applyRefund() {
		t.Fatal("duplicate delivery must be a replay")
	}
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 25_000_000 {
		t.Fatalf("cash = %d, want 25 CNY (30 topup − 5 refund, 各一次)", view.Balance.CashAvailable)
	}
}

// TestWalletRefund_BonusNeverCashRefundable: 赠送余额不得伪装现金退款——
// 纯规则拒绝 + DB CHECK 兜底（裁决 7）。
func TestWalletRefund_BonusNeverCashRefundable(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	fundWallet(t, s, f.accountID, "CNY", 0, 10_000_000) // 只有赠送余额
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	// DB 层兜底：直接插入 bonus 退款分录必须撞 CHECK。
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inference_wallet_entries
		 (wallet_id, entry_type, direction, source, amount_micros, currency, business_key)
		 VALUES ($1, 'refund', 'debit', 'bonus', 1000, 'CNY', $2)`,
		w.ID, "wallet:refund:forged-"+uuid.NewString())
	if err == nil {
		t.Fatal("bonus refund insert must hit the CHECK constraint")
	}
	// 赠送余额原样未动。
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.BonusAvailable != 10_000_000 {
		t.Fatalf("bonus = %d, want untouched 10 CNY", view.Balance.BonusAvailable)
	}
}

// TestWalletReversal_OncePerEntry: 冲正追加反向分录；一条分录至多被冲正
// 一次（部分唯一索引 + 业务键双兜底）。
func TestWalletReversal_OncePerEntry(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	fundWallet(t, s, f.accountID, "CNY", 10_000_000, 0)

	entries, err := s.ListWalletEntries(ctx, f.accountID, "CNY", 0, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v %v", entries, err)
	}
	topupID := entries[0].ID

	uow, _ := s.Begin(ctx)
	if err := s.ReverseWalletEntryTx(ctx, uow, f.accountID, topupID, "user:ops@app:test"); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 重复冲正 → CodeConflict，不叠加效果。
	uow2, _ := s.Begin(ctx)
	err = s.ReverseWalletEntryTx(ctx, uow2, f.accountID, topupID, "user:ops@app:test")
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second reversal must conflict, got %v", err)
	}
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 0 {
		t.Fatalf("cash = %d, want 0 (topup 被冲正一次)", view.Balance.CashAvailable)
	}
}

// TestWalletFreeze_NegativeDerivedBonus_ClampedSplit: 评审轮1 I1 链路——
// 冲正一笔已被部分消费的 bonus 赠送后派生 bonus 为负；后续冻结必须成功
// （拆分钳零下界：bonus=0、现金足额扣），不得再因负数拆分撞 hold CHECK
// 把现金充足的客户全部 400；负缺口留在派生余额如实展示。
func TestWalletFreeze_NegativeDerivedBonus_ClampedSplit(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	now := time.Now().UTC()
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, now.Add(-time.Hour)) // 1 micro/token

	// 现金 5 CNY + 赠送 2 CNY（adjustment credit bonus，记录 entry id 供冲正）。
	fundWallet(t, s, f.accountID, "CNY", 5_000_000, 0)
	uowG, _ := s.Begin(ctx)
	if _, err := s.ApplyWalletAdjustmentTx(ctx, uowG, WalletAdjustmentCommand{
		AccountID: f.accountID, Currency: "CNY",
		Source: accounting.WalletBonus, Direction: accounting.DirCredit,
		AmountMicros: 2_000_000, Reason: "welcome gift",
		OperatorSubject: "user:ops@app:test", IdempotencyKey: "gift-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("bonus grant: %v", err)
	}
	if err := uowG.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var bonusEntryID int64
	if err := s.db.QueryRow(
		`SELECT id FROM inference_wallet_entries WHERE source = 'bonus' AND entry_type = 'adjustment'`).
		Scan(&bonusEntryID); err != nil {
		t.Fatal(err)
	}
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}

	// 消费赠送：hold 1.5 CNY 全部从 bonus 冻结并结算消费掉 → bonus 剩 0.5。
	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 1_500_000))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	attID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(1_000_000), int64(500_000)
	pv, err := s.GetPriceVersion(ctx, priceID)
	if err != nil {
		t.Fatal(err)
	}
	pure, err := pv.Pure()
	if err != nil {
		t.Fatal(err)
	}
	charge, err := pure.Quote(domain.UsageRecord{
		RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: domain.Microcredit(charge.Money.Micros),
		WalletCharge: charge.Money, SettledAt: now,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 冲正整笔 2 CNY 赠送（其中 1.5 已被消费）→ 派生 bonus = 0.5 − 2 = −1.5。
	uowR, _ := s.Begin(ctx)
	if err := s.ReverseWalletEntryTx(ctx, uowR, f.accountID, bonusEntryID, "user:ops@app:test"); err != nil {
		t.Fatalf("reverse bonus: %v", err)
	}
	if err := uowR.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.BonusAvailable != -1_500_000 {
		t.Fatalf("derived bonus = %d, want -1_500_000 (负缺口如实展示)", view.Balance.BonusAvailable)
	}

	// 后续冻结：现金充足必须放行，拆分非负、现金足额扣。
	uow2, _ := s.Begin(ctx)
	adm2, err := s.ReserveWallet(ctx, uow2, reserveWalletCmd(t, s, f, w.ID, priceID, 1_000_000))
	if err != nil {
		t.Fatalf("freeze with negative derived bonus must succeed (I1): %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var holdCash, holdBonus int64
	if err := s.db.QueryRow(
		`SELECT cash_micros, bonus_micros FROM inference_wallet_holds WHERE request_id = $1`, adm2.RequestID).
		Scan(&holdCash, &holdBonus); err != nil {
		t.Fatal(err)
	}
	if holdBonus != 0 || holdCash != 1_000_000 {
		t.Fatalf("hold split = cash %d bonus %d, want 1_000_000/0 (钳零下界、现金足额扣)", holdCash, holdBonus)
	}
}

// TestWalletSpendLimitGate: 月支出上限按 UTC 自然月账本派生；到达上限后
// 冻结拒绝（ErrSpendLimitExceeded）。
func TestWalletSpendLimitGate(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 100_000_000, 0)
	enableOverage(t, s, f.accountID, "CNY", 3_000_000) // 月上限 3 CNY
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	// 第一笔：hold 2 CNY → 放行。
	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 2_000_000))
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 结算 2 CNY（全消费）→ 月支出 = 2。
	attID := uuid.NewString()
	uow1, _ := s.Begin(ctx)
	if err := insertAttempt(ctx, mustTx(t, uow1), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	cm, _ := domain.NewMoney(2_000_000, "CNY")
	in := int64(2000)
	if err := s.Settle(ctx, uow1, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in},
		},
		ChargeMicros: 2_000_000, WalletCharge: &cm, SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 第二笔 hold 2 CNY：2+2 > 3 上限 → 拒绝。
	uow2, _ := s.Begin(ctx)
	_, err = s.ReserveWallet(ctx, uow2, reserveWalletCmd(t, s, f, w.ID, priceID, 2_000_000))
	_ = uow2.Rollback(ctx)
	if !errors.Is(err, accounting.ErrSpendLimitExceeded) {
		t.Fatalf("want ErrSpendLimitExceeded, got %v", err)
	}
	// 恰好到顶的 1 CNY 放行。
	uow3, _ := s.Begin(ctx)
	if _, err = s.ReserveWallet(ctx, uow3, reserveWalletCmd(t, s, f, w.ID, priceID, 1_000_000)); err != nil {
		_ = uow3.Rollback(ctx)
		t.Fatalf("exact-limit reserve must pass: %v", err)
	}
	if err := uow3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestWalletRelease_ConfirmedZero: 确认零消费 → 冻结释放，派生余额不变
// （释放不是账本行，与额度预占同口径）。
func TestWalletRelease_ConfirmedZero(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 10_000_000, 10_000_000)
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 6_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 冻结拆分：赠送先扣 → bonus 6 CNY。
	mid, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mid.Balance.BonusHeld != 6_000_000 || mid.Balance.CashHeld != 0 {
		t.Fatalf("held = %+v, want bonus held 6 CNY", mid.Balance)
	}

	uow2, _ := s.Begin(ctx)
	if err := s.Release(ctx, uow2, adm.RequestID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 10_000_000 || view.Balance.BonusAvailable != 10_000_000 ||
		view.Balance.CashHeld != 0 || view.Balance.BonusHeld != 0 {
		t.Fatalf("balance after release = %+v, want fully restored", view.Balance)
	}
	req, _ := s.GetRequest(ctx, adm.RequestID)
	if req.Status != domain.ReqReleased {
		t.Fatalf("request status = %s, want released", req.Status)
	}
	// 重复释放 → 冲突（幂等防重）。
	uow3, _ := s.Begin(ctx)
	err = s.Release(ctx, uow3, adm.RequestID)
	_ = uow3.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate release must conflict, got %v", err)
	}
}

// TestWalletFreeze_IdempotentPerRequest: 同一 request_id 重复冻结撞唯一
// 键（幂等业务键兜底）。
func TestWalletFreeze_IdempotentPerRequest(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 10_000_000, 0)
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	cmd := reserveWalletCmd(t, s, f, w.ID, priceID, 1_000_000)
	uow, _ := s.Begin(ctx)
	if _, err := s.ReserveWallet(ctx, uow, cmd); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 同一请求行重复插入撞请求主键（重复预占防撞唯一键）。
	uow2, _ := s.Begin(ctx)
	_, err := s.ReserveWallet(ctx, uow2, cmd)
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate freeze must conflict, got %v", err)
	}
}

// TestPAYGEntitlement_ExplicitRecord: 无套餐按量必须存在显式 PAYG 权益记
// 录；未发布配置不可开启；重复开启幂等；停用后复活（裁决 6）。
func TestPAYGEntitlement_ExplicitRecord(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 未配置 → 拒绝开启。
	uow, _ := s.Begin(ctx)
	_, err := s.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC())
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unconfigured PAYG must fail closed, got %v", err)
	}

	// 发布配置（策略须 published）。
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPAYGConfig(ctx, f.policyID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatalf("put payg config: %v", err)
	}
	// 草稿策略不可发布为 PAYG 配置。
	pol2 := &PolicyVersion{Name: "draft-pol", Revision: 1, ModelIDs: []string{f.modelID}}
	if err := s.InsertPolicyVersion(ctx, pol2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPAYGConfig(ctx, pol2.ID, []string{f.modelID}, "user:ops@app:test"); err == nil {
		t.Fatal("draft policy must not be publishable as PAYG config")
	}

	uow2, _ := s.Begin(ctx)
	ent, err := s.EnsurePAYGEntitlementTx(ctx, uow2, f.accountID, time.Now().UTC())
	if err != nil {
		t.Fatalf("enable payg: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ent.SourceType != domain.SourcePAYG || ent.SourceID != accounting.PAYGEntitlementSourceID(f.accountID) ||
		!ent.AllowsModel(f.modelID) || ent.Status != domain.EntitlementActive {
		t.Fatalf("payg entitlement = %+v", ent)
	}
	// 重复开启 → 同一记录（幂等）。
	uow3, _ := s.Begin(ctx)
	ent2, err := s.EnsurePAYGEntitlementTx(ctx, uow3, f.accountID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := uow3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ent2.ID != ent.ID {
		t.Fatalf("re-enable must return the same record, got %s vs %s", ent2.ID, ent.ID)
	}
}

// 评审轮1 M3：EnsurePAYGEntitlementTx 持 tx 期间的配置/既有权益读取必须
// 走同一连接——连接池只剩 1 个连接时（旧实现用 s.db 第二连接会等不到连
// 接而死锁）全流程仍须完成。
func TestPAYGEntitlement_SingleConnectionPool(t *testing.T) {
	db, s := testDB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPAYGConfig(ctx, f.policyID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := s.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC())
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("single-connection ensure: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ent.Status != domain.EntitlementActive {
		t.Fatalf("entitlement = %+v", ent)
	}
	// 已存在权益的读取路径（existing 分支）同样在单连接下完成。
	uow2, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ent2, err := s.EnsurePAYGEntitlementTx(ctx, uow2, f.accountID, time.Now().UTC())
	if err != nil {
		_ = uow2.Rollback(ctx)
		t.Fatalf("single-connection re-ensure: %v", err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ent2.ID != ent.ID {
		t.Fatalf("re-ensure = %s, want %s", ent2.ID, ent.ID)
	}
}

// 评审轮2 N-1：多个连接并发 EnablePAYG（双击/超时重试形态）——赢家创建、
// 输家在同 tx 读回（INSERT ON CONFLICT DO NOTHING 不产生 25P02 毒化），
// 双方都成功且权益恰一行（端点契约幂等）。
func TestPAYGEntitlement_ConcurrentEnable_Idempotent(t *testing.T) {
	db, s := testDB(t)
	db.SetMaxOpenConns(8)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPAYGConfig(ctx, f.policyID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}

	const racers = 6
	errs := make(chan error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uow, err := s.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			if _, err := s.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC()); err != nil {
				_ = uow.Rollback(ctx)
				errs <- err
				return
			}
			errs <- uow.Commit(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent enable must all succeed (N-1: no 25P02 poison): %v", err)
		}
	}
	var n int
	if err := s.db.Get(&n,
		`SELECT COUNT(*) FROM inference_entitlements
		 WHERE billing_account_id = $1 AND source_type = 'payg'`, f.accountID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("entitlements = %d, want exactly 1 (并发开启幂等)", n)
	}
}

// TestWalletAuditTrail: 开启/关闭/改上限全部写审计行（裁决 4）。
func TestWalletAuditTrail(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	enableOverage(t, s, f.accountID, "CNY", 1_000_000)
	// 改上限
	uow, _ := s.Begin(ctx)
	l2 := int64(2_000_000)
	if _, err := s.SetOverageTx(ctx, uow, f.accountID, "CNY", true, &l2, "user:test"); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 关闭
	uow2, _ := s.Begin(ctx)
	if _, err := s.SetOverageTx(ctx, uow2, f.accountID, "CNY", false, &l2, "user:test"); err != nil {
		t.Fatal(err)
	}
	if err := uow2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var actions []string
	if err := s.db.Select(&actions,
		`SELECT action FROM inference_wallet_audits a
		 JOIN inference_wallets w ON w.id = a.wallet_id
		 WHERE w.billing_account_id = $1 ORDER BY a.id`, f.accountID); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "enable_overage", "set_spend_limit", "set_spend_limit", "disable_overage"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	// 未设上限开启 → 拒绝（CHECK/纯规则一致）。
	uow3, _ := s.Begin(ctx)
	_, err := s.SetOverageTx(ctx, uow3, f.accountID, "CNY", true, nil, "user:test")
	_ = uow3.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("enable without limit must reject, got %v", err)
	}
}

// TestWalletSettle_RecoveryConservative: 崩溃恢复路径（WalletCharge=nil）
// 按预占额全额保守入账——钱包币种从冻结行取，金额 = reserved（Task 9 口
// 径在钱包路径的同构实现）。
func TestWalletSettle_RecoveryConservative(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 10_000_000, 6_000_000)
	enableOverage(t, s, f.accountID, "CNY", 100_000_000)
	w, _ := s.GetWalletByAccount(ctx, f.accountID, "CNY")

	uow, _ := s.Begin(ctx)
	adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, w.ID, priceID, 5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	attID := uuid.NewString()
	uow1, _ := s.Begin(ctx)
	if err := insertAttempt(ctx, mustTx(t, uow1), &domain.Attempt{ID: attID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	// 恢复估算：source=estimated，charge=reserved（5 CNY），WalletCharge=nil。
	in := int64(2000)
	if err := s.Settle(ctx, uow1, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attID, Source: domain.UsageEstimated,
			Buckets: domain.UsageBuckets{InputTokens: &in},
		},
		ChargeMicros: 5_000_000, SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("recovery settle: %v", err)
	}
	if err := uow1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 冻结拆分赠送先扣：bonus 5(全) → consume bonus 5；现金不动。
	view, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.BonusAvailable != 1_000_000 || view.Balance.CashAvailable != 10_000_000 ||
		view.Balance.CashHeld != 0 || view.Balance.BonusHeld != 0 {
		t.Fatalf("balance = %+v, want bonus 1 / cash 10, holds 0", view.Balance)
	}
	var currency string
	var amount int64
	if err := s.db.QueryRow(
		`SELECT currency, amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, adm.RequestID).Scan(&currency, &amount); err != nil {
		t.Fatal(err)
	}
	if currency != "CNY" || amount != 5_000_000 {
		t.Fatalf("ledger charge = %s/%d, want CNY/5000000", currency, amount)
	}
}
