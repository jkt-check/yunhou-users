package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
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

// R7-N2：冷启动（无已验证快照兜底）三条失败路径都必须携带
// ErrNoVerifiedSnapshot 哨兵,上游 handler 据此映射 503。有缓存时刷新失败
// 继续返回旧快照,不得携带哨兵。

// stubRevisionSource 手搓桩：逐路径注入冷启动失败,替代真实 postgres store。
type stubRevisionSource struct {
	headFn   func(context.Context, domain.ConfigScope) (int64, int, error)
	activeFn func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error)
}

func (s stubRevisionSource) ActiveRevisionHead(ctx context.Context, scope domain.ConfigScope) (int64, int, error) {
	return s.headFn(ctx, scope)
}

func (s stubRevisionSource) ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error) {
	return s.activeFn(ctx, scope)
}

// validCatalogRevision 是能通过 ParseSnapshot 的最小合法修订（空目录也合法:
// 修订体完整即可,模型集合可为空）。
func validCatalogRevision(id int64, rev int) *domain.ConfigRevision {
	return &domain.ConfigRevision{
		ID:       id,
		Scope:    domain.ScopeCatalog,
		Revision: rev,
		IsActive: true,
		Payload: domain.ExtensionConfig{
			SchemaVersion: 1,
			Raw:           json.RawMessage(`{"schema_version":1,"models":[],"providers":[],"deployments":[],"routes":[]}`),
		},
	}
}

func TestSnapshotCache_ColdStartCarriesSentinel(t *testing.T) {
	errBoom := errors.New("db gone")
	cases := []struct {
		name string
		src  stubRevisionSource
	}{
		{"head probe fails", stubRevisionSource{
			headFn: func(context.Context, domain.ConfigScope) (int64, int, error) {
				return 0, 0, errBoom
			},
			activeFn: func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
				t.Fatal("ActiveRevision must not be reached when the head probe fails")
				return nil, nil
			},
		}},
		{"active revision load fails", stubRevisionSource{
			headFn: func(context.Context, domain.ConfigScope) (int64, int, error) {
				return 7, 3, nil
			},
			activeFn: func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
				return nil, errBoom
			},
		}},
		{"snapshot parse fails", stubRevisionSource{
			headFn: func(context.Context, domain.ConfigScope) (int64, int, error) {
				return 7, 3, nil
			},
			activeFn: func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
				rev := validCatalogRevision(7, 3)
				rev.Payload.Raw = json.RawMessage(`{"schema_version":999}`)
				return rev, nil
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := catalog.NewSnapshotCache(tc.src, func(error) {})
			_, err := cache.Current(context.Background())
			if err == nil {
				t.Fatal("cold start with no verified snapshot must fail")
			}
			if !errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
				t.Errorf("err = %v, want errors.Is(err, ErrNoVerifiedSnapshot)", err)
			}
		})
	}
}

func TestSnapshotCache_RefreshFailureKeepsSnapshotWithoutSentinel(t *testing.T) {
	errBoom := errors.New("db gone")
	healthy := true
	src := stubRevisionSource{
		headFn: func(context.Context, domain.ConfigScope) (int64, int, error) {
			if !healthy {
				return 0, 0, errBoom
			}
			return 7, 3, nil
		},
		activeFn: func(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
			if !healthy {
				return nil, errBoom
			}
			return validCatalogRevision(7, 3), nil
		},
	}
	absorbed := 0
	cache := catalog.NewSnapshotCache(src, func(error) { absorbed++ })

	first, err := cache.Current(context.Background())
	if err != nil {
		t.Fatalf("warm-up Current: %v", err)
	}
	healthy = false
	second, err := cache.Current(context.Background())
	if err != nil {
		t.Fatalf("refresh failure with a verified snapshot must be absorbed, got %v", err)
	}
	if second != first {
		t.Error("refresh failure must keep serving the previous verified snapshot")
	}
	if absorbed == 0 {
		t.Error("OnRefreshError must fire on the absorbed refresh failure")
	}
}
