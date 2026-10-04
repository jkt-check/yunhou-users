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
