package quota

// key_lease_test.go — Task 5：Key 级并发租约在准入编排里的接线。
//
// 钉住：Key 设了上限 → 准入事务内取一把 api_key 作用域的租约；没设限 → 不取；
// 账户租约与 Key 租约并存（请求级租约是一个列表，不是"两把锁"的硬编码）；
// 第二把租约拿不到 → 整笔准入回滚（不留半状态）。

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// TestAdmitTakesKeyLeaseWhenTheKeyCapsConcurrency: Key 设了上限 → 准入事务
// 内取一把 api_key 作用域的租约（与账户租约同一机制、同一事务）。
func TestAdmitTakesKeyLeaseWhenTheKeyCapsConcurrency(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.keyConcurrencyLimit = 2
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.leases) != 1 {
		t.Fatalf("leases = %d, want 1 (the Key lease)", len(fs.leases))
	}
	l := fs.leases[0]
	if l.Scope != domain.LeaseScopeAPIKey || l.ScopeID != *cmd.Request.APIKeyID {
		t.Errorf("lease scope = %s/%s, want api_key/%s", l.Scope, l.ScopeID, *cmd.Request.APIKeyID)
	}
	if l.Limit != 2 || l.RequestID != res.RequestID || l.OwnerToken == "" {
		t.Errorf("lease = %+v", l)
	}
	if len(res.Leases) != 1 || res.Leases[0].Scope != domain.LeaseScopeAPIKey {
		t.Errorf("admission leases = %+v, want the Key lease", res.Leases)
	}
}

// TestAdmitAccountAndKeyLeasesCoexist: 账户租约（策略并发上限）与 Key 租约
// 并存 —— 请求级租约是一个列表（finalizer 按列表释放，不硬编码两把锁）。
func TestAdmitAccountAndKeyLeasesCoexist(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	conc := 3
	cmd.Policy.ConcurrencyLimit = &conc
	fs.keyConcurrencyLimit = 2
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.leases) != 2 {
		t.Fatalf("leases = %d, want 2 (billing account + api key)", len(fs.leases))
	}
	if fs.leases[0].Scope != domain.LeaseScopeBillingAccount || fs.leases[0].ScopeID != cmd.Request.BillingAccountID {
		t.Errorf("first lease = %s/%s, want billing_account", fs.leases[0].Scope, fs.leases[0].ScopeID)
	}
	if fs.leases[1].Scope != domain.LeaseScopeAPIKey || fs.leases[1].ScopeID != *cmd.Request.APIKeyID {
		t.Errorf("second lease = %s/%s, want api_key", fs.leases[1].Scope, fs.leases[1].ScopeID)
	}
	if len(res.Leases) != 2 {
		t.Errorf("admission leases = %d, want 2", len(res.Leases))
	}
	if res.AccountLease() == nil {
		t.Error("the billing-account lease must stay reachable")
	}
}

// TestAdmitTakesNoKeyLeaseWhenTheKeyHasNoLimit: 没设限的 Key 不占任何槽。
func TestAdmitTakesNoKeyLeaseWhenTheKeyHasNoLimit(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.keyConcurrencyLimit = 0
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.leases) != 0 {
		t.Errorf("leases = %d, want 0 (缺省不加 Key 限制)", len(fs.leases))
	}
	if len(res.Leases) != 0 {
		t.Errorf("admission leases = %d, want 0", len(res.Leases))
	}
}

// TestAdmitKeyLeaseFailureRollsBackEverything: 第一把租约（账户）成功、第二把
// （Key）失败 → 整笔准入回滚：不提交、不留半状态。
func TestAdmitKeyLeaseFailureRollsBackEverything(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	conc := 3
	cmd.Policy.ConcurrencyLimit = &conc
	fs.keyConcurrencyLimit = 1
	fs.leaseErrs = []error{nil, domain.NewError(domain.CodeInsufficientCapacity, "lease: key full")}
	_, err := svc.Admit(context.Background(), cmd)
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("err = %v, want insufficient_capacity", err)
	}
	if fs.uow.commits != 0 || fs.uow.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want 0/1 (第二把租约失败必须整笔回滚)",
			fs.uow.commits, fs.uow.rollbacks)
	}
}
