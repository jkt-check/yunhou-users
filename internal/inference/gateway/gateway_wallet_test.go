// gateway_wallet_test.go — Task 14 网关级验收（真实 PostgreSQL + httptest
// 上游）：套餐外默认关不动余额 / 显式开启后钱包路径完整有界预占与金额结
// 算 / 无套餐 PAYG 需显式权益且并发限制照常。

package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// fundAndEnable credits the fixture account's CNY wallet and flips the
// explicit overage switch (裁决 4：默认关，必须显式开启 + 上限).
func fundAndEnable(t *testing.T, f *fixture, cashMicros, limitMicros int64) postgres.Wallet {
	t.Helper()
	ctx := context.Background()
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreditTopupTx(ctx, uow, postgres.WalletTopupCommand{
		AccountID: f.accountID, Currency: "CNY", AmountMicros: cashMicros,
		PaymentID: "pay-" + uuid.NewString(), OrderID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := f.store.SetOverageTx(ctx, uow, f.accountID, "CNY", true, &limitMicros, "user:"+f.userID); err != nil {
		t.Fatalf("enable overage: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := f.store.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}
	return *w
}

// seedWalletPrice publishes the sale_money list (3 micros/token input,
// 5 micros/token output — deliberately different from the credit list so a
// cross-contamination would show).
func seedWalletPrice(t *testing.T, f *fixture) string {
	t.Helper()
	pv := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceSaleMoney, Unit: "micromoney", Currency: "CNY",
		InputPerMtok: 3_000_000, OutputPerMtok: 5_000_000,
		Revision: 1, EffectiveFrom: time.Now().Add(-time.Hour),
	}
	if err := f.store.InsertPriceVersion(context.Background(), pv); err != nil {
		t.Fatalf("insert money price: %v", err)
	}
	return pv.ID
}

// shrinkPlanToExhausted re-points the fixture entitlement at a policy whose
// monthly window admits NOTHING and whose overage policy is allow_overage.
func shrinkPlanToExhausted(t *testing.T, f *fixture, overagePolicy string) {
	t.Helper()
	zero := domain.Microcredit(0)
	pol := &postgres.PolicyVersion{
		Name: "exhausted-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(0), WeeklyLimit: micro(0), MonthlyLimit: &zero,
		OveragePolicy: overagePolicy, Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatalf("insert exhausted policy: %v", err)
	}
	if _, err := f.db.Exec(
		`UPDATE inference_entitlements SET policy_version_id = $1 WHERE id = $2`, pol.ID, f.entID); err != nil {
		t.Fatal(err)
	}
}

// TestGatewayWallet_OverageDefaultOff_NothingMoves: 套餐耗尽 + 钱包有余额
// 但未显式开启 → 429 quota_exceeded，钱包余额/冻结一分不动（裁决 4）。
func TestGatewayWallet_OverageDefaultOff_NothingMoves(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	ctx := context.Background()

	// 钱包有余额 + 售价目存在，但 overage 未开启（且策略允许套餐外——
	// 双重门控中客户这关没过）。
	seedWalletPrice(t, f)
	fund := func() {
		uow, _ := f.store.Begin(ctx)
		if err := f.store.CreditTopupTx(ctx, uow, postgres.WalletTopupCommand{
			AccountID: f.accountID, Currency: "CNY", AmountMicros: 100_000_000,
			PaymentID: "pay-" + uuid.NewString(), OrderID: uuid.NewString(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fund()
	shrinkPlanToExhausted(t, f, "allow_overage")

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hello wallet"))
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("want quota_exceeded, got %v", err)
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream must never be called, got %d", up.calls.Load())
	}
	view, verr := f.store.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if verr != nil {
		t.Fatal(verr)
	}
	if view.Balance.CashAvailable != 100_000_000 || view.Balance.CashHeld != 0 {
		t.Fatalf("wallet must be untouched, got %+v", view.Balance)
	}
	var holds int
	if err := f.db.Get(&holds, `SELECT COUNT(*) FROM inference_wallet_holds`); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("no wallet hold may exist, got %d", holds)
	}
}

// TestGatewayWallet_OverageFallback_SettlesMoney: 套餐耗尽 + 策略
// allow_overage + 客户显式开启 → 本次请求固定走钱包：完整有界预占（冻结）
// → 按钉住的 sale_money 版本金额结算（账本 micromoney 分录 + 钱包消费分
// 录），未动套餐窗口。
func TestGatewayWallet_OverageFallback_SettlesMoney(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	ctx := context.Background()

	priceID := seedWalletPrice(t, f)
	fundAndEnable(t, f, 100_000_000, 100_000_000)
	shrinkPlanToExhausted(t, f, "allow_overage")

	p, key := f.principal()
	outcome, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hello wallet"))
	if err != nil {
		t.Fatalf("wallet call: %v", err)
	}
	drainStream(t, outcome, EndCompleted)

	// 请求：charge_source=wallet，价格钉 sale_money r1。
	req, err := f.store.GetRequest(ctx, outcome.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.ChargeSource != domain.ChargeSourceWallet || req.Status != domain.ReqSettled {
		t.Fatalf("request = charge_source %s status %s", req.ChargeSource, req.Status)
	}
	// 账本 charge：micromoney/CNY，金额 = 8×3 + 34×5 = 194 微（pin r1）。
	var unit, currency, pvID string
	var amount int64
	if err := f.db.QueryRow(
		`SELECT unit, currency, price_version_id, amount_micros FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, outcome.RequestID).
		Scan(&unit, &currency, &pvID, &amount); err != nil {
		t.Fatal(err)
	}
	if unit != "micromoney" || currency != "CNY" || pvID != priceID || amount != 194 {
		t.Fatalf("ledger charge = %s/%s/%s/%d, want micromoney/CNY/r1/194", unit, currency, pvID, amount)
	}
	// 钱包派生余额减少 194 微；冻结清零。
	view, err := f.store.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.CashAvailable != 100_000_000-194 || view.Balance.CashHeld != 0 {
		t.Fatalf("wallet = %+v, want cash %d held 0", view.Balance, 100_000_000-194)
	}
	// 套餐窗口一行未动（钱包消费不是套餐额度）。
	var windowUsed int64
	if err := f.db.Get(&windowUsed,
		`SELECT COALESCE(SUM(used_micros),0) FROM inference_quota_windows WHERE entitlement_id = $1`, f.entID); err != nil {
		t.Fatal(err)
	}
	if windowUsed != 0 {
		t.Fatalf("plan windows must stay untouched, used = %d", windowUsed)
	}
}

// TestGatewayWallet_PlanPolicyReject_NoFallback: 策略 overage=reject 时即
// 使客户已开启钱包消费也不回退（产品门 + 客户门缺一不可）。
func TestGatewayWallet_PlanPolicyReject_NoFallback(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	ctx := context.Background()
	seedWalletPrice(t, f)
	fundAndEnable(t, f, 100_000_000, 100_000_000)
	shrinkPlanToExhausted(t, f, "reject")

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hello"))
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("want quota_exceeded, got %v", err)
	}
	if up.calls.Load() != 0 {
		t.Fatal("upstream must not be called")
	}
	view, _ := f.store.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if view.Balance.CashHeld != 0 {
		t.Fatalf("no freeze allowed under reject policy, got %+v", view.Balance)
	}
}

// TestGatewayWallet_PAYG: 无套餐账户——无 PAYG 记录时 model_not_allowed；
// 显式开启（权益记录）+ 钱包充值后可调用，金额结算走 sale_money；并发限
// 制照常（裁决 6）。
func TestGatewayWallet_PAYG(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	ctx := context.Background()

	// 无套餐：移除 fixture 的订阅权益。
	if _, err := f.db.Exec(`DELETE FROM inference_entitlements`); err != nil {
		t.Fatal(err)
	}
	seedWalletPrice(t, f)

	p, key := f.principal()
	// 无 PAYG 记录 → model_not_allowed（无套餐不自动获得按量资格）。
	_, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("want model_not_allowed without PAYG record, got %v", err)
	}

	// 运营发布 PAYG 配置（并发上限 1）+ 客户显式开启。
	conc := 1
	paygPol := &postgres.PolicyVersion{
		Name: "payg-default", Revision: 1, ModelIDs: []string{f.modelID},
		ConcurrencyLimit: &conc, Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, paygPol); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutPAYGConfig(ctx, paygPol.ID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}
	uow, _ := f.store.Begin(ctx)
	ent, err := f.store.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC())
	if err != nil {
		t.Fatalf("enable payg: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ent.SourceType != domain.SourcePAYG {
		t.Fatalf("source = %s, want payg", ent.SourceType)
	}
	fundAndEnable(t, f, 100_000_000, 100_000_000)

	outcome, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi payg"))
	if err != nil {
		t.Fatalf("payg call: %v", err)
	}
	drainStream(t, outcome, EndCompleted)
	req, _ := f.store.GetRequest(ctx, outcome.RequestID)
	if req.ChargeSource != domain.ChargeSourceWallet || req.EntitlementID != ent.ID {
		t.Fatalf("payg request = source %s ent %s", req.ChargeSource, req.EntitlementID)
	}
	view, _ := f.store.WalletBalance(ctx, f.accountID, "CNY", time.Now())
	if view.Balance.CashAvailable != 100_000_000-194 {
		t.Fatalf("wallet = %+v", view.Balance)
	}

	// 并发限制照常（PAYG 策略 concurrency=1）：慢流占住租约，第二个并发
	// 调用必须 insufficient_capacity。响应头先 flush 让首个调用完成流建
	// 立（租约持有期），阻塞远短于请求超时。
	release := make(chan struct{})
	up.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(": hi\n\n"))
			fl.Flush()
		}
		<-release
		_, _ = w.Write([]byte(chunkUsage + chunkDone))
	})
	first, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hold the lease"))
	if err != nil {
		t.Fatalf("first concurrent call: %v", err)
	}
	// 等上游真的在流式（租约已持有）。
	deadline := time.Now().Add(5 * time.Second)
	for up.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_, err = f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "second"))
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("want insufficient_capacity under concurrency=1, got %v", err)
	}
	close(release)
	drainStream(t, first, EndCompleted)
}

// TestGatewayWallet_PAYG_NoPrice: 按量模型无 sale_money 价目 → 未定价拒
// 绝（未定价且需扣费的能力必须拒绝，设计 §7.1）。
func TestGatewayWallet_PAYG_NoPrice(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up)
	ctx := context.Background()
	if _, err := f.db.Exec(`DELETE FROM inference_entitlements`); err != nil {
		t.Fatal(err)
	}
	paygPol := &postgres.PolicyVersion{
		Name: "payg-default", Revision: 1, ModelIDs: []string{f.modelID}, Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, paygPol); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutPAYGConfig(ctx, paygPol.ID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}
	uow, _ := f.store.Begin(ctx)
	if _, err := f.store.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fundAndEnable(t, f, 100_000_000, 100_000_000)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUnpricedCapability {
		t.Fatalf("want unpriced_capability, got %v", err)
	}
	if up.calls.Load() != 0 {
		t.Fatal("upstream must not be called")
	}
}
