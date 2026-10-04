package access

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// entitlement_cache_test.go — 权益解析 1s TTL 缓存（计数 fake store + 手动
// 时钟；零 at 走缓存，显式非零 at 绕过缓存直查）。

// countingEntitlementStore 记录 ListActiveEntitlements 调用次数。
type countingEntitlementStore struct {
	ents  []domain.Entitlement
	calls int
}

func (s *countingEntitlementStore) ListActiveEntitlements(_ context.Context, _ string, _ time.Time) ([]domain.Entitlement, error) {
	s.calls++
	return s.ents, nil
}

func TestEntitlementResolver_CacheTTL(t *testing.T) {
	now := entUTC(2026, 10, 4, 12)
	clock := &domain.FixedClock{T: now}
	store := &countingEntitlementStore{ents: []domain.Entitlement{
		mkEnt(domain.SourceGrant, "gift-1", []string{"kimi-k2"}, now.Add(-time.Hour)),
	}}
	r := NewEntitlementResolver(store, clock)
	ctx := context.Background()

	// 首次零 at 解析查库并填充缓存。
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("calls = %d, want 1", store.calls)
	}
	// TTL 内命中缓存，不再查库。
	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
			t.Fatalf("cached Resolve #%d: %v", i, err)
		}
	}
	if store.calls != 1 {
		t.Fatalf("in-TTL calls = %d, want still 1", store.calls)
	}

	// 显式非零 at 绕过缓存直查（测试/对账语义）。
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", now); err != nil {
		t.Fatalf("explicit-at Resolve: %v", err)
	}
	if store.calls != 2 {
		t.Fatalf("explicit-at calls = %d, want 2", store.calls)
	}
	// 直查不回填缓存：随后的零 at 仍命中旧缓存。
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
		t.Fatalf("post-explicit cached Resolve: %v", err)
	}
	if store.calls != 2 {
		t.Fatalf("explicit at must not populate the cache: calls = %d", store.calls)
	}

	// TTL 过期后重查。
	clock.T = now.Add(entitlementCacheTTL + time.Second)
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
		t.Fatalf("post-TTL Resolve: %v", err)
	}
	if store.calls != 3 {
		t.Fatalf("post-TTL calls = %d, want 3", store.calls)
	}
}

func TestEntitlementResolver_CacheReflectsExpiry(t *testing.T) {
	now := entUTC(2026, 10, 4, 12)
	clock := &domain.FixedClock{T: now}
	store := &countingEntitlementStore{ents: []domain.Entitlement{
		mkEnt(domain.SourceGrant, "gift-1", []string{"kimi-k2"}, now.Add(-time.Hour)),
	}}
	r := NewEntitlementResolver(store, clock)
	ctx := context.Background()

	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// 权益消失：TTL 内仍按缓存放行（脏读窗口由 TTL 限制），过期后拒绝。
	store.ents = nil
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); err != nil {
		t.Fatalf("in-TTL must serve the cached view: %v", err)
	}
	clock.T = now.Add(entitlementCacheTTL + time.Second)
	if _, err := r.Resolve(ctx, "acct-1", "kimi-k2", time.Time{}); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("post-TTL must re-read and reject: %v", err)
	}
}
