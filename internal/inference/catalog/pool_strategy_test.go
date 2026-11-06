package catalog_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// pool_strategy_test.go — T7「配置行为收口」：pool_strategy 的支持集。
//
// 口径：
//   - 写面（新建/修改/批量导入/环境导入/正式 publish）只支持 round_robin；
//     least_loaded 与通用 session_sticky 明确拒绝（不能保存后假装生效）。
//   - 存量已发布的旧值必须仍可读（旧快照可读），但路由被标记不可用 ——
//     绝不静默当 round_robin。
//   - 正式 publish 与发布预演点名对象 ID 与替代配置。

// legacyRouteSnapshot builds a published snapshot whose single route uses a
// legacy (known but unsupported) pool strategy — exactly what an older
// revision on disk carries.
func legacyRouteSnapshot(t *testing.T, strategy domain.PoolStrategy) *catalog.Snapshot {
	t.Helper()
	payload := catalog.BuildCatalogPayload(
		[]domain.Model{{ID: "m1", DisplayName: "M", Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"}}},
		[]domain.Provider{{ID: "p1", Code: "p", DisplayName: "P",
			AccessType: domain.AccessOfficialAPI, Status: "active"}},
		[]domain.Deployment{{ID: "d1", ProviderID: "p1", UpstreamModel: "up",
			BaseURL: "https://api.example.com", Protocol: domain.ProtocolOpenAIChat,
			Status: domain.DeploymentActive, ConnectTimeout: time.Second, RequestTimeout: time.Second}},
		[]domain.ModelRoute{{ID: "r1", ModelID: "m1", DeploymentID: "d1",
			Weight: 1, Enabled: true, PoolStrategy: strategy}},
	)
	rev := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: payload}, Status: domain.RevisionPublished}
	snap, err := catalog.ParseSnapshot(rev)
	if err != nil {
		t.Fatalf("a published snapshot with the legacy strategy %q must stay loadable: %v", strategy, err)
	}
	return snap
}

// TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable: 旧快照
// 可读（读不出来就是砖目录），但该路由不得出现在可调度部署里 —— 绝不静默
// 当 round_robin。
func TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable(t *testing.T) {
	for _, legacy := range []domain.PoolStrategy{domain.PoolLeastLoaded, domain.PoolSessionSticky} {
		snap := legacyRouteSnapshot(t, legacy)
		routes := snap.RoutesByModel["m1"]
		if len(routes) != 1 {
			t.Fatalf("%s: routes = %+v, want the legacy route to stay readable", legacy, routes)
		}
		if got := snap.ActiveDeployments("m1"); len(got) != 0 {
			t.Errorf("%s: ActiveDeployments = %+v, want 0 (unsupported pool strategy is not a dispatchable deployment)", legacy, got)
		}
	}
	// 支持集内的策略仍然照常可调度（保护证据）。
	ok := legacyRouteSnapshot(t, domain.PoolRoundRobin)
	if got := ok.ActiveDeployments("m1"); len(got) != 1 {
		t.Errorf("round_robin: ActiveDeployments = %+v, want 1", got)
	}
}

// TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject: 正式 publish
// 与发布预演都必须拒绝不支持的 pool_strategy，并点名对象 ID 与替代配置。
func TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject(t *testing.T) {
	_, store, svc := testDB(t)
	ctx := context.Background()

	prov := &domain.Provider{Code: "prov-legacy", DisplayName: "Prov", AccessType: domain.AccessOfficialAPI}
	if err := svc.CreateProvider(ctx, prov); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	m := sampleModel("legacy-model")
	if err := svc.CreateModel(ctx, &m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "up-legacy",
		BaseURL: "https://api.legacy.example.com/v1", Protocol: domain.ProtocolOpenAIChat,
		Status: domain.DeploymentActive,
	}
	if err := svc.CreateDeployment(ctx, dep); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// 存量数据：直接经存储写入一条 least_loaded 路由（写面已经拒绝它 ——
	// 只有历史数据才会以这种形态存在）。路由 ID 由存储生成，回读后按真实
	// ID 断言（发布错误必须点名**这条**路由）。
	legacy := &domain.ModelRoute{ModelID: m.ID, DeploymentID: dep.ID,
		Weight: 1, Enabled: true, PoolStrategy: domain.PoolLeastLoaded}
	if err := store.InsertRoute(ctx, legacy); err != nil {
		t.Fatalf("insert legacy route: %v", err)
	}
	routes, err := svc.ListRoutes(ctx, m.ID)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	var legacyID string
	for _, r := range routes {
		if r.PoolStrategy == domain.PoolLeastLoaded {
			legacyID = r.ID
		}
	}
	if legacyID == "" {
		t.Fatalf("legacy route not stored: %+v", routes)
	}

	// 正式 publish：必须失败。
	_, err = svc.Publish(ctx, "tester")
	if err == nil {
		t.Fatal("publish must refuse an unsupported pool strategy")
	}
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("publish: code = %v, want invalid_input", domain.CodeOf(err))
	}
	if !strings.Contains(err.Error(), legacyID) {
		t.Errorf("publish error must name the object ID: %v", err)
	}
	if !strings.Contains(err.Error(), string(domain.PoolRoundRobin)) {
		t.Errorf("publish error must name the replacement: %v", err)
	}

	// 发布预演：同一口径、同一错误（预演必须指出对象 ID 和替代配置）。
	_, err = svc.PublishPreview(ctx)
	if err == nil {
		t.Fatal("publish preview must refuse an unsupported pool strategy")
	}
	if !strings.Contains(err.Error(), legacyID) {
		t.Errorf("publish preview error must name the object ID: %v", err)
	}
	if !strings.Contains(err.Error(), string(domain.PoolRoundRobin)) {
		t.Errorf("publish preview error must name the replacement: %v", err)
	}
}

// TestPublish_SupportedPoolStrategyStillPublishes: 支持集内的目录照常发布
// （保护证据：新规则不得把正常发布也堵死）。
func TestPublish_SupportedPoolStrategyStillPublishes(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "publish-ok")
	rev, err := svc.Publish(ctx, "tester")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if rev <= 0 {
		t.Errorf("revision = %d, want > 0", rev)
	}
}
