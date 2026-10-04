package catalog_test

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// snapshot_cache_ttl_test.go — head 探测 1s TTL（计数桩 + 手动时钟）：稳态
// 探测频率降到每秒约一次；加载新快照后的下一次请求照常探测。

func TestSnapshotCache_HeadProbeTTL(t *testing.T) {
	var headCalls, activeCalls int
	headID := int64(7)
	src := stubRevisionSource{
		headFn: func(context.Context, domain.ConfigScope) (int64, int, error) {
			headCalls++
			return headID, 3, nil
		},
		activeFn: func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
			activeCalls++
			return validCatalogRevision(headID, 3), nil
		},
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cache := catalog.NewSnapshotCache(src, func(error) {})
	cache.Now = func() time.Time { return now }
	ctx := context.Background()

	// 冷启动加载：一次探测 + 一次全量加载。
	snap, err := cache.Current(ctx)
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	if headCalls != 1 || activeCalls != 1 {
		t.Fatalf("cold start probes = %d loads = %d, want 1/1", headCalls, activeCalls)
	}
	// 加载后的下一次请求照常探测（命中现有快照，记录 TTL 起点），不得
	// 再次全量加载。
	if _, err := cache.Current(ctx); err != nil {
		t.Fatalf("second Current: %v", err)
	}
	if headCalls != 2 || activeCalls != 1 {
		t.Fatalf("post-load probes = %d loads = %d, want 2/1", headCalls, activeCalls)
	}
	// TTL 内直接复用 current，不再探测。
	for i := 0; i < 3; i++ {
		got, err := cache.Current(ctx)
		if err != nil || got != snap {
			t.Fatalf("in-TTL Current #%d: %v %p", i, err, got)
		}
	}
	if headCalls != 2 {
		t.Fatalf("in-TTL probes = %d, want still 2", headCalls)
	}
	// TTL 过期后重新探测（命中 → 不加载）。
	now = now.Add(2 * time.Second)
	if _, err := cache.Current(ctx); err != nil {
		t.Fatalf("post-TTL Current: %v", err)
	}
	if headCalls != 3 || activeCalls != 1 {
		t.Fatalf("post-TTL probes = %d loads = %d, want 3/1", headCalls, activeCalls)
	}

	// 发布新 revision：TTL 过期后被探测并加载。
	headID = 8
	now = now.Add(2 * time.Second)
	got, err := cache.Current(ctx)
	if err != nil {
		t.Fatalf("post-publish Current: %v", err)
	}
	if got.RevisionID != 8 || activeCalls != 2 || headCalls != 4 {
		t.Fatalf("post-publish revID = %d probes = %d loads = %d, want 8/4/2",
			got.RevisionID, headCalls, activeCalls)
	}
}
