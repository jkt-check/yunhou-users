package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/routing"
)

// finish_order_test.go — 评审轮2 B1：StreamBody.Finish 的顺序不变式——
// finalize（settle 落账、请求终态化）必须先于 keeper.Stop（释放租约），
// 使"持有租约"与"未终态"严格同区间；用测试替身记录调用序。

// leaseOrderStub is a routing.AccountStore that only records the lease
// lifecycle calls the keeper makes (renew/release) in order.
type leaseOrderStub struct {
	mu       sync.Mutex
	released []string
	renewed  int
}

func (s *leaseOrderStub) ListActiveUpstreamAccounts(context.Context, string) ([]domain.UpstreamAccount, error) {
	return nil, nil
}
func (s *leaseOrderStub) AcquireLeaseTx(context.Context, domain.UnitOfWork, domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error) {
	return nil, nil
}
func (s *leaseOrderStub) CheckLease(context.Context, string, string, int64, time.Time) error {
	return nil
}
func (s *leaseOrderStub) RenewLease(context.Context, string, string, int64, time.Time) error {
	s.mu.Lock()
	s.renewed++
	s.mu.Unlock()
	return nil
}
func (s *leaseOrderStub) ReleaseLease(_ context.Context, leaseID, _ string, _ int64) error {
	s.mu.Lock()
	s.released = append(s.released, leaseID)
	s.mu.Unlock()
	return nil
}
func (s *leaseOrderStub) releasedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.released...)
}

// 评审轮2 B1：Finish 内 finalize 先于租约释放——settle 提交（finalize 内
// 完成）前租约恒 held（stub 未观察到任何 ReleaseLease），扫在 finalize
// 前 vs 后的窗口被消除。
func TestStreamBodyFinish_FinalizeBeforeLeaseRelease(t *testing.T) {
	stub := &leaseOrderStub{}
	routingSvc := routing.NewService(stub, nil, nil)
	upstreamLease := &domain.ConcurrencyLease{
		ID: "lease-upstream", Scope: domain.LeaseScopeUpstreamAccount, ScopeID: "acct-1",
		RequestID: "req-1", OwnerToken: "owner", FencingToken: 1,
		State: domain.LeaseHeld, ExpiresAt: time.Now().Add(time.Minute),
	}
	accountLease := &domain.ConcurrencyLease{
		ID: "lease-account", Scope: domain.LeaseScopeBillingAccount, ScopeID: "acct-1",
		RequestID: "req-1", OwnerToken: "owner", FencingToken: 2,
		State: domain.LeaseHeld, ExpiresAt: time.Now().Add(time.Minute),
	}
	keeper := routingSvc.NewLeaseKeeper(upstreamLease, accountLease)

	finalized := false
	body := &StreamBody{
		keeper: keeper,
		finalize: func(end StreamEnd) error {
			// settle 提交点：此刻双租约必须仍 held（没有任何释放发生）。
			if got := stub.releasedIDs(); len(got) != 0 {
				t.Errorf("lease released BEFORE finalize committed (B1 TOCTOU): %v", got)
			}
			finalized = true
			return nil
		},
	}
	if err := body.Finish(EndCompleted); err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Fatal("finalize did not run")
	}
	got := stub.releasedIDs()
	if len(got) != 2 {
		t.Fatalf("released = %v, want both leases released after finalize", got)
	}
	// 幂等：再次 Finish 不重复 finalize/释放。
	if err := body.Finish(EndCompleted); err != nil {
		t.Fatal(err)
	}
	if got := stub.releasedIDs(); len(got) != 2 {
		t.Fatalf("second Finish released again: %v", got)
	}
}
