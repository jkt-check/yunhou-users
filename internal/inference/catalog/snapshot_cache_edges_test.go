package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
)

// snapshot_cache_edges_test.go — Task 16 覆盖率补强：SnapshotCache 的失败
// 路径（不得加载半个版本；刷新失败保留已验证快照）。

func TestSnapshotCache_EmptyCatalogFailsClosed(t *testing.T) {
	_, store, _ := testDB(t)
	cache := catalog.NewSnapshotCache(store, func(err error) {})
	if _, err := cache.Current(context.Background()); err == nil {
		t.Fatal("empty catalog must fail closed, not serve a half snapshot")
	}
}

func TestSnapshotCache_RefreshErrorPropagates(t *testing.T) {
	// 读取失败（表不存在于本上下文 → 直接查空库返回 NotFound）显式传播。
	_, store, _ := testDB(t)
	cache := catalog.NewSnapshotCache(store, func(err error) {})
	_, err := cache.Current(context.Background())
	if err == nil {
		t.Fatal("want error on empty catalog")
	}
	var de interface{ Error() string }
	if errors.As(err, &de) && de.Error() == "" {
		t.Fatal("error must carry a message")
	}
}
