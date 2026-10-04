package routing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

// accounts_cache_test.go — 路由候选 upstream 账号池 1s TTL 缓存（计数
// fake store + 手动时钟；错误不缓存）。

// countingAccountStore 在 fakeStore 上记录 ListActiveUpstreamAccounts 次数。
type countingAccountStore struct {
	*fakeStore
	calls int
}

func (s *countingAccountStore) ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error) {
	s.calls++
	return s.fakeStore.ListActiveUpstreamAccounts(ctx, providerID)
}

func candidatesSvc(t *testing.T, fs *countingAccountStore, clock domain.Clock) *Service {
	t.Helper()
	return NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, clock)
}

func TestCandidates_AccountsCacheTTL(t *testing.T) {
	fs := &countingAccountStore{fakeStore: newFakeStore()}
	fs.accounts["prov"] = []domain.UpstreamAccount{account("a1", "prov", 1)}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clock := &domain.FixedClock{T: now}
	svc := candidatesSvc(t, fs, clock)

	route := domain.ModelRoute{ID: "r1", ModelID: "m", DeploymentID: "dep1", Enabled: true, Weight: 1}
	snap := testSnapshot("m", []domain.ModelRoute{route}, deployment("dep1", "prov", domain.ProtocolOpenAIChat))
	ctx := context.Background()

	if _, err := svc.Candidates(ctx, snap, "m", Needs{}); err != nil {
		t.Fatalf("first Candidates: %v", err)
	}
	if fs.calls != 1 {
		t.Fatalf("calls = %d, want 1", fs.calls)
	}
	// TTL 内命中缓存，不再查库。
	if _, err := svc.Candidates(ctx, snap, "m", Needs{}); err != nil {
		t.Fatalf("cached Candidates: %v", err)
	}
	if fs.calls != 1 {
		t.Fatalf("in-TTL calls = %d, want still 1", fs.calls)
	}

	// TTL 过期后重查，且候选反映新账号。
	clock.T = now.Add(2 * time.Second)
	fs.accounts["prov"] = append(fs.accounts["prov"], account("a2", "prov", 1))
	out, err := svc.Candidates(ctx, snap, "m", Needs{})
	if err != nil {
		t.Fatalf("post-TTL Candidates: %v", err)
	}
	if fs.calls != 2 {
		t.Fatalf("post-TTL calls = %d, want 2", fs.calls)
	}
	if len(out) != 2 {
		t.Fatalf("post-TTL candidates = %d, want 2 (new account visible)", len(out))
	}
}

func TestCandidates_AccountsCacheErrorsNotCached(t *testing.T) {
	fs := &countingAccountStore{fakeStore: newFakeStore()}
	fs.accounts["prov"] = []domain.UpstreamAccount{account("a1", "prov", 1)}
	boom := errors.New("db gone")
	fs.accountsErr = boom
	svc := candidatesSvc(t, fs, nil)

	route := domain.ModelRoute{ID: "r1", ModelID: "m", DeploymentID: "dep1", Enabled: true, Weight: 1}
	snap := testSnapshot("m", []domain.ModelRoute{route}, deployment("dep1", "prov", domain.ProtocolOpenAIChat))
	ctx := context.Background()

	if _, err := svc.Candidates(ctx, snap, "m", Needs{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want store error", err)
	}
	// 错误不缓存：恢复后同一 TTL 窗口内立即重查成功。
	fs.accountsErr = nil
	out, err := svc.Candidates(ctx, snap, "m", Needs{})
	if err != nil {
		t.Fatalf("recovered Candidates: %v", err)
	}
	if len(out) != 1 || fs.calls != 2 {
		t.Fatalf("candidates = %d calls = %d, want 1/2", len(out), fs.calls)
	}
}

// TestCandidates_EmptyRefreshesStaleCache: 空候选不得建立在 1s TTL 缓存的
// 过期额度快照上（2026-10 pr-ci 回归：TestDrill_UpstreamQuotaExhausted 在
// quota reset 直改库存行后仍被缓存里的旧 reset 挡成 503）。空结果路径必须
// 驱逐缓存绕过重查一次，仍为空才判定 upstream_unavailable。
func TestCandidates_EmptyRefreshesStaleCache(t *testing.T) {
	fs := &countingAccountStore{fakeStore: newFakeStore()}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clock := &domain.FixedClock{T: now}
	zero := domain.Microcredit(0)
	resetFuture := now.Add(time.Hour)
	exhausted := account("a1", "prov", 1)
	exhausted.Quota.RemainingMicros = &zero
	exhausted.Quota.ResetAt = &resetFuture
	fs.accounts["prov"] = []domain.UpstreamAccount{exhausted}
	svc := candidatesSvc(t, fs, clock)

	route := domain.ModelRoute{ID: "r1", ModelID: "m", DeploymentID: "dep1", Enabled: true, Weight: 1}
	snap := testSnapshot("m", []domain.ModelRoute{route}, deployment("dep1", "prov", domain.ProtocolOpenAIChat))
	ctx := context.Background()

	// 第一次：账号额度耗尽（reset 未到）→ 空候选；空结果路径驱逐缓存并
	// 绕过重查一次（仍空）——共两次查库。
	out, err := svc.Candidates(ctx, snap, "m", Needs{})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("candidates = %d, want 0 (quota exhausted)", len(out))
	}
	if fs.calls != 2 {
		t.Fatalf("calls = %d, want 2 (miss + bypass refetch on empty)", fs.calls)
	}

	// quota reset 已过（测试直改库存行，绕过应用层无失效钩子）：TTL 内的
	// 第二次调用命中缓存仍得空 → 再次驱逐重查 → 看到恢复后的账号。
	resetPast := now.Add(-time.Minute)
	recovered := account("a1", "prov", 1)
	recovered.Quota.RemainingMicros = &zero
	recovered.Quota.ResetAt = &resetPast
	fs.accounts["prov"] = []domain.UpstreamAccount{recovered}
	out, err = svc.Candidates(ctx, snap, "m", Needs{})
	if err != nil {
		t.Fatalf("post-reset Candidates: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("post-reset candidates = %d, want 1 (reset passed → schedulable)", len(out))
	}
	if fs.calls != 3 {
		t.Fatalf("calls = %d, want 3 (stale cache hit → invalidate → refetch)", fs.calls)
	}
}

// TestCandidates_StructurallyEmptyNoRefetch: 路由/部署/能力层面的结构性为空
// （没有可用 route）不触发重查——没有账号池被咨询过，重查无意义。
func TestCandidates_StructurallyEmptyNoRefetch(t *testing.T) {
	fs := &countingAccountStore{fakeStore: newFakeStore()}
	fs.accounts["prov"] = []domain.UpstreamAccount{account("a1", "prov", 1)}
	svc := candidatesSvc(t, fs, nil)

	snap := testSnapshot("m", nil, deployment("dep1", "prov", domain.ProtocolOpenAIChat))
	out, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("candidates = %d, want 0 (no routes)", len(out))
	}
	if fs.calls != 0 {
		t.Fatalf("calls = %d, want 0 (structural empty must not refetch)", fs.calls)
	}
}
