package quota

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// admit_wallet_test.go — Task 16 覆盖率补强：AdmitWallet 钱包门控编排
// （fake store；真实库的冻结/结算语义在 postgres/wallet_repo_test.go）。
// 钉住：有界预占按 sale_money 计价、准入上界随请求持久化、并发租约、
// fail-closed（任一失败零部分状态）。

func walletAdmitCmd(t *testing.T) AdmitWalletCommand {
	t.Helper()
	return AdmitWalletCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: uuid.NewString(),
			EntitlementID: uuid.NewString(), ModelID: "glm-4.6",
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
			ChargeSource: domain.ChargeSourceWallet,
		},
		Entitlement: domain.Entitlement{ID: uuid.NewString(), PolicyVersionID: "pol-1"},
		// charge_source 在入场（gateway）固定；quota 层透传不改写。

		Policy: Policy{Name: "payg", Revision: 1, ModelIDs: []string{"glm-4.6"}},
		MoneyPrice: func() accounting.PriceVersion {
			p := creditPrice(t, 100_000, 200_000, nil)
			p.Kind = accounting.PriceSaleMoney
			p.Currency = "CNY"
			return p
		}(),
		WalletID:             uuid.NewString(),
		Model:                domain.Model{ID: "glm-4.6", ContextTokens: 200_000, MaxOutputTokens: 8192},
		EstimatedInputTokens: 100,
	}
}

func TestAdmitWallet_HappyPathPersistsBoundsAndLease(t *testing.T) {
	fs := &fakeStore{}
	svc := NewService(fs, domain.FixedClock{T: time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)})
	cmd := walletAdmitCmd(t)
	outCap := int64(50)
	cmd.ClientMaxTokens = &outCap

	adm, err := svc.AdmitWallet(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if adm.RequestID != cmd.Request.ID {
		t.Fatalf("request id = %s", adm.RequestID)
	}
	if len(fs.walletCmds) != 1 {
		t.Fatalf("wallet reserve calls = %d, want 1 (无窗口/预算预占)", len(fs.walletCmds))
	}
	wc := fs.walletCmds[0]
	// 预占金额：输入 100×0.1 + 输出 cap 50×0.2 = 10+10 = 20 micromoney。
	if wc.HoldMicros != 20 {
		t.Errorf("hold = %d, want 20 (sale_money 有界预占)", wc.HoldMicros)
	}
	if wc.Request.ChargeSource != domain.ChargeSourceWallet {
		t.Errorf("charge_source = %q, want wallet（入场固定、quota 透传不改写）", wc.Request.ChargeSource)
	}
	// 准入上界随请求持久化（恢复估算依据）。
	if wc.Request.InputBoundTokens == nil || *wc.Request.InputBoundTokens != 100 ||
		wc.Request.OutputCapTokens == nil || *wc.Request.OutputCapTokens != 50 {
		t.Errorf("bounds not persisted: %+v", wc.Request)
	}
	if wc.Request.PriceVersionID == nil || *wc.Request.PriceVersionID != "pv-1" {
		t.Errorf("price version not pinned: %+v", wc.Request.PriceVersionID)
	}
	// 无并发限制 → 无租约。
	if adm.AccountLease != nil {
		t.Errorf("unexpected account lease: %+v", adm.AccountLease)
	}
	if fs.uow.commits != 1 || fs.uow.rollbacks != 0 {
		t.Errorf("uow = %+v, want one commit no rollback", fs.uow)
	}
}

func TestAdmitWallet_ConcurrencyLeaseTakenWhenPolicySetsLimit(t *testing.T) {
	fs := &fakeStore{}
	svc := NewService(fs, nil)
	cmd := walletAdmitCmd(t)
	conc := 1
	cmd.Policy.ConcurrencyLimit = &conc

	adm, err := svc.AdmitWallet(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if adm.AccountLease == nil {
		t.Fatal("policy concurrency limit must take the account lease")
	}
	if len(fs.leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(fs.leases))
	}
}

func TestAdmitWallet_FailClosedOnEveryStep(t *testing.T) {
	newSvc := func(fs *fakeStore) *Service { return NewService(fs, nil) }

	// 模型无上下文上限 → 输入无法有界 → 校验失败零痕迹。
	fs := &fakeStore{}
	cmd := walletAdmitCmd(t)
	cmd.Model.ContextTokens = 0
	if _, err := newSvc(fs).AdmitWallet(context.Background(), cmd); err == nil {
		t.Error("context-less model must fail the bound computation")
	}
	if len(fs.walletCmds) != 0 {
		t.Error("wallet touched on validation failure (部分状态!)")
	}

	// 钱包冻结失败 → 回滚零痕迹。
	fs = &fakeStore{walletErr: domain.NewError(domain.CodeInsufficientBalance, "not enough")}
	cmd = walletAdmitCmd(t)
	if _, err := newSvc(fs).AdmitWallet(context.Background(), cmd); err == nil ||
		domain.CodeOf(err) != domain.CodeInsufficientBalance {
		t.Errorf("wallet failure = %v, want insufficient_balance", err)
	}
	if fs.uow == nil || fs.uow.rollbacks != 1 || fs.uow.commits != 0 {
		t.Errorf("wallet failure must roll back: %+v", fs.uow)
	}

	// 租约失败 → 回滚（预占不残留）。
	fs = &fakeStore{leaseErr: domain.NewError(domain.CodeInsufficientCapacity, "busy")}
	cmd = walletAdmitCmd(t)
	conc := 1
	cmd.Policy.ConcurrencyLimit = &conc
	if _, err := newSvc(fs).AdmitWallet(context.Background(), cmd); err == nil {
		t.Error("lease failure must fail the admission")
	}
	if fs.uow == nil || fs.uow.rollbacks != 1 {
		t.Errorf("lease failure must roll back the wallet hold too: %+v", fs.uow)
	}

	// Begin 失败（存储不可用）→ fail-closed。
	fs = &fakeStore{beginErr: domain.NewError(domain.CodeInternal, "db down")}
	cmd = walletAdmitCmd(t)
	if _, err := newSvc(fs).AdmitWallet(context.Background(), cmd); err == nil {
		t.Error("storage-unavailable must not admit")
	}
}

// ReserveAmountMoney 纯规则补充：sale_credit 价目喂给钱包路径 → 显式拒绝。
func TestReserveAmountMoney_RequiresSaleMoney(t *testing.T) {
	p := creditPrice(t, 100_000, 200_000, nil) // Kind = sale_credit
	if _, err := ReserveAmountMoney(p, ReserveBounds{InputBoundTokens: 1, OutputCapTokens: 1}); err == nil {
		t.Fatal("credit price must not price a wallet reservation")
	}
	// 正常 sale_money 换算 + 币种钉住。
	p.Kind = accounting.PriceSaleMoney
	p.Currency = "USD"
	m, err := ReserveAmountMoney(p, ReserveBounds{InputBoundTokens: 10, OutputCapTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if m.Currency != "USD" || m.Micros <= 0 {
		t.Fatalf("money = %+v", m)
	}
}
