package management

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// catalog_manager_test.go — CatalogManager 的纯逻辑覆盖（Task 16 补强）：
// 每个写操作必须带动作正确的审计记录、egress URL 校验接入部署写路径、
// 错误透传不产生审计。catalog.Service 的存储用内存 fake（行为级验证在
// catalog 与 postgres 包）。

// fakeCatalogStore is a map-backed catalog.Store sufficient for manager
// pass-through tests (validation/publish semantics live in catalog tests).
type fakeCatalogStore struct {
	models      map[string]*domain.Model
	providers   map[string]*domain.Provider
	deployments map[string]*domain.Deployment
	routes      map[string]*domain.ModelRoute
	revisions   []*domain.ConfigRevision
	// publishErr 注入发布事务失败（安全审查 M-2：激活失败模拟）。
	publishErr error
	// txReads 统计经发布事务快照完成的读次数；directListCalls 统计绕过
	// 事务的直接列表调用（安全审查 M-3 测试断言发布全程只走事务）。
	txReads         int
	directListCalls int
}

func newFakeCatalogStore() *fakeCatalogStore {
	return &fakeCatalogStore{
		models: map[string]*domain.Model{}, providers: map[string]*domain.Provider{},
		deployments: map[string]*domain.Deployment{}, routes: map[string]*domain.ModelRoute{},
	}
}

func (f *fakeCatalogStore) InsertModel(ctx context.Context, m *domain.Model) error {
	cp := *m
	f.models[m.ID] = &cp
	return nil
}
func (f *fakeCatalogStore) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	if m, ok := f.models[id]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, domain.NewError(domain.CodeNotFound, "model")
}
func (f *fakeCatalogStore) listModels(filter domain.ModelFilter) ([]domain.Model, error) {
	out := []domain.Model{}
	for _, m := range f.models {
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeCatalogStore) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	f.directListCalls++
	return f.listModels(filter)
}
func (f *fakeCatalogStore) UpdateModel(ctx context.Context, m *domain.Model) error {
	return f.InsertModel(ctx, m)
}
func (f *fakeCatalogStore) DeleteModel(ctx context.Context, id string) error {
	delete(f.models, id)
	return nil
}
func (f *fakeCatalogStore) InsertProvider(ctx context.Context, p *domain.Provider) error {
	cp := *p
	f.providers[p.ID] = &cp
	return nil
}
func (f *fakeCatalogStore) GetProvider(ctx context.Context, id string) (*domain.Provider, error) {
	if p, ok := f.providers[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, domain.NewError(domain.CodeNotFound, "provider")
}
func (f *fakeCatalogStore) GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error) {
	for _, p := range f.providers {
		if p.Code == code {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.CodeNotFound, "provider")
}
func (f *fakeCatalogStore) listProviders() []domain.Provider {
	out := []domain.Provider{}
	for _, p := range f.providers {
		out = append(out, *p)
	}
	return out
}

func (f *fakeCatalogStore) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	f.directListCalls++
	return f.listProviders(), nil
}
func (f *fakeCatalogStore) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	return f.InsertProvider(ctx, p)
}
func (f *fakeCatalogStore) DeleteProvider(ctx context.Context, id string) error {
	delete(f.providers, id)
	return nil
}
func (f *fakeCatalogStore) InsertDeployment(ctx context.Context, d *domain.Deployment) error {
	cp := *d
	f.deployments[d.ID] = &cp
	return nil
}
func (f *fakeCatalogStore) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	if d, ok := f.deployments[id]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, domain.NewError(domain.CodeNotFound, "deployment")
}
func (f *fakeCatalogStore) FindDeployment(ctx context.Context, providerID, upstreamModel, baseURL string) (*domain.Deployment, error) {
	for _, d := range f.deployments {
		if d.ProviderID == providerID && d.UpstreamModel == upstreamModel && d.BaseURL == baseURL {
			cp := *d
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.CodeNotFound, "deployment")
}
func (f *fakeCatalogStore) listDeployments() []domain.Deployment {
	out := []domain.Deployment{}
	for _, d := range f.deployments {
		out = append(out, *d)
	}
	return out
}

func (f *fakeCatalogStore) ListDeployments(ctx context.Context, flt domain.DeploymentFilter) ([]domain.Deployment, error) {
	f.directListCalls++
	return f.listDeployments(), nil
}
func (f *fakeCatalogStore) UpdateDeployment(ctx context.Context, d *domain.Deployment) error {
	return f.InsertDeployment(ctx, d)
}
func (f *fakeCatalogStore) DeleteDeployment(ctx context.Context, id string) error {
	delete(f.deployments, id)
	return nil
}
func (f *fakeCatalogStore) InsertRoute(ctx context.Context, r *domain.ModelRoute) error {
	if r.ID == "" {
		r.ID = r.ModelID + "|" + r.DeploymentID
	}
	cp := *r
	f.routes[r.ID] = &cp
	return nil
}
func (f *fakeCatalogStore) GetRoute(ctx context.Context, id string) (*domain.ModelRoute, error) {
	if r, ok := f.routes[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.NewError(domain.CodeNotFound, "route")
}
func (f *fakeCatalogStore) listRoutes(modelID string) []domain.ModelRoute {
	out := []domain.ModelRoute{}
	for _, r := range f.routes {
		if r.ModelID == modelID {
			out = append(out, *r)
		}
	}
	return out
}

func (f *fakeCatalogStore) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	f.directListCalls++
	return f.listRoutes(modelID), nil
}
func (f *fakeCatalogStore) UpdateRoute(ctx context.Context, r *domain.ModelRoute) error {
	cp := *r
	f.routes[r.ID] = &cp
	return nil
}
func (f *fakeCatalogStore) DeleteRoute(ctx context.Context, id string) error {
	delete(f.routes, id)
	return nil
}
func (f *fakeCatalogStore) InsertConfigRevision(ctx context.Context, rev *domain.ConfigRevision) error {
	f.revisions = append(f.revisions, rev)
	return nil
}
func (f *fakeCatalogStore) ActivateRevision(ctx context.Context, scope domain.ConfigScope, revision int) error {
	for _, r := range f.revisions {
		if r.Revision == revision {
			r.Status = domain.RevisionPublished
			r.IsActive = true
		} else if r.IsActive {
			r.Status = domain.RevisionSuperseded
			r.IsActive = false
		}
	}
	return nil
}
func (f *fakeCatalogStore) ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error) {
	if len(f.revisions) == 0 {
		return nil, domain.NewError(domain.CodeNotFound, "revision")
	}
	return f.revisions[len(f.revisions)-1], nil
}
func (f *fakeCatalogStore) ActiveRevisionHead(ctx context.Context, scope domain.ConfigScope) (int64, int, error) {
	return 0, len(f.revisions), nil
}
func (f *fakeCatalogStore) GetRevision(ctx context.Context, scope domain.ConfigScope, revision int) (*domain.ConfigRevision, error) {
	for _, r := range f.revisions {
		if r.Revision == revision {
			return r, nil
		}
	}
	return nil, domain.NewError(domain.CodeNotFound, "revision")
}
func (f *fakeCatalogStore) ListRevisionMetas(ctx context.Context, scope domain.ConfigScope, afterRevision, limit int) ([]domain.RevisionMeta, error) {
	out := []domain.RevisionMeta{}
	for _, r := range f.revisions {
		if afterRevision > 0 && r.Revision >= afterRevision {
			continue
		}
		out = append(out, domain.RevisionMeta{
			ID: r.ID, Scope: r.Scope, Revision: r.Revision, Status: r.Status,
			IsActive: r.IsActive, PublishedAt: r.PublishedAt,
			CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}
func (f *fakeCatalogStore) ActiveRevisionMeta(ctx context.Context, scope domain.ConfigScope) (*domain.RevisionMeta, error) {
	if len(f.revisions) == 0 {
		return nil, domain.NewError(domain.CodeNotFound, "revision")
	}
	r := f.revisions[len(f.revisions)-1]
	return &domain.RevisionMeta{
		ID: r.ID, Scope: r.Scope, Revision: r.Revision, Status: r.Status,
		IsActive: r.IsActive, PublishedAt: r.PublishedAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}, nil
}
func (f *fakeCatalogStore) LatestRevision(ctx context.Context, scope domain.ConfigScope) (int, error) {
	return len(f.revisions), nil
}

// fakePublishTx models catalog.PublishTx：InsertAndActivateRevision 先失败后
// 追加（模拟同一事务的提交/回滚原子性——失败路径不留任何修订行）。读方法
// 全部委托给 store 并计入 txReads（安全审查 M-3：发布抽取必须走事务快照）。
type fakePublishTx struct {
	store      *fakeCatalogStore
	committed  bool
	rolledBack bool
}

func (f *fakeCatalogStore) BeginPublish(ctx context.Context) (catalog.PublishTx, error) {
	return &fakePublishTx{store: f}, nil
}

func (t *fakePublishTx) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	t.store.txReads++
	return t.store.listModels(filter)
}

func (t *fakePublishTx) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	t.store.txReads++
	return t.store.listProviders(), nil
}

func (t *fakePublishTx) ListDeployments(ctx context.Context, flt domain.DeploymentFilter) ([]domain.Deployment, error) {
	t.store.txReads++
	return t.store.listDeployments(), nil
}

func (t *fakePublishTx) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	t.store.txReads++
	return t.store.listRoutes(modelID), nil
}

func (t *fakePublishTx) LatestRevision(ctx context.Context, scope domain.ConfigScope) (int, error) {
	return len(t.store.revisions), nil
}

func (t *fakePublishTx) InsertAndActivateRevision(ctx context.Context, rev *domain.ConfigRevision) error {
	if t.store.publishErr != nil {
		return t.store.publishErr // 模拟同事务回滚：一行不留
	}
	t.store.revisions = append(t.store.revisions, rev)
	return t.store.ActivateRevision(ctx, rev.Scope, rev.Revision)
}

func (t *fakePublishTx) Commit(ctx context.Context) error {
	t.committed = true
	return nil
}

func (t *fakePublishTx) Rollback(ctx context.Context) error {
	if !t.committed {
		t.rolledBack = true
	}
	return nil
}

// spyRecorder captures audit events.
type spyRecorder struct{ events []AuditEvent }

func (s *spyRecorder) Record(ctx context.Context, ev AuditEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func newManagerFixture() (*CatalogManager, *spyRecorder, *fakeCatalogStore) {
	fs := newFakeCatalogStore()
	rec := &spyRecorder{}
	mgr := NewCatalogManager(catalog.NewService(fs), rec,
		func(ctx context.Context, rawURL string) error {
			if strings.HasPrefix(rawURL, "https://") {
				return nil
			}
			return domain.NewError(domain.CodeInvalidInput, "egress: only https allowed")
		})
	return mgr, rec, fs
}

func managerActor() string { return "user:op-1@app:ops" }

func TestCatalogManager_CRUDRecordsAudit(t *testing.T) {
	mgr, rec, fs := newManagerFixture()
	ctx := context.Background()
	actor := managerActor()

	if err := mgr.CreateProvider(ctx, actor, &domain.Provider{
		ID: uuid.NewString(), Code: "glm", DisplayName: "GLM",
		AccessType: domain.AccessOfficialAPI, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	// 读回（fake 存储按 code 检索；管理面没有 GetProviderByCode）。
	prov, err := fs.GetProviderByCode(ctx, "glm")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateProvider(ctx, actor, &domain.Provider{
		ID: prov.ID, Code: "glm", DisplayName: "GLM 2", AccessType: domain.AccessOfficialAPI,
		Status: "active", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.CreateModel(ctx, actor, &domain.Model{
		ID: "glm-9", DisplayName: "GLM 9", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GetModel(ctx, "glm-9"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ListModels(ctx, domain.ModelFilter{}); err != nil {
		t.Fatal(err)
	}
	mod, _ := mgr.GetModel(ctx, "glm-9")
	mod.DisplayName = "GLM 9b"
	mod.UpdatedAt = time.Now().UTC()
	if err := mgr.UpdateModel(ctx, actor, mod); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModelLifecycle(ctx, actor, "glm-9", domain.LifecycleActive); err != nil {
		t.Fatal(err)
	}

	// 部署写路径的 egress 校验：https 放行。
	dep := &domain.Deployment{
		ID: uuid.NewString(),
		ProviderID: prov.ID, UpstreamModel: "up", BaseURL: "https://api.glm.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentDraft, ConfigVersion: 1,
	}
	if err := mgr.CreateDeployment(ctx, actor, dep); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GetDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ListDeployments(ctx, domain.DeploymentFilter{}); err != nil {
		t.Fatal(err)
	}
	dep.ConfigVersion++
	dep.Status = domain.DeploymentActive
	if err := mgr.UpdateDeployment(ctx, actor, dep); err != nil {
		t.Fatal(err)
	}
	// http（非 https）→ egress 拒绝且零新审计。
	auditBefore := len(rec.events)
	depBad := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "up2", BaseURL: "http://insecure.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentDraft,
	}
	if err := mgr.CreateDeployment(ctx, actor, depBad); err == nil {
		t.Fatal("insecure deployment URL must be rejected")
	}
	if len(rec.events) != auditBefore {
		t.Fatal("rejected write must not record audit")
	}

	// Route CRUD（UpdateRoute 需要完整对象 + 版本令牌）。
	rt := &domain.ModelRoute{ModelID: "glm-9", DeploymentID: dep.ID, Weight: 1, Enabled: true}
	if err := mgr.CreateRoute(ctx, actor, rt); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ListRoutes(ctx, "glm-9"); err != nil {
		t.Fatal(err)
	}
	rt2, err := mgr.GetRoute(ctx, rt.ID)
	if err != nil {
		t.Fatal(err)
	}
	rt2.Weight = 2
	rt2.UpdatedAt = time.Now().UTC()
	if err := mgr.UpdateRoute(ctx, actor, rt2); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeleteRoute(ctx, actor, rt.ID); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DeleteDeployment(ctx, actor, dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeleteModel(ctx, actor, "glm-9"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeleteProvider(ctx, actor, prov.ID); err != nil {
		t.Fatal(err)
	}

	// 审计动作序列：每个写操作一条、动作正确、人员/服务双腿归因。
	wantActions := []string{
		"provider.create", "provider.update",
		"model.create", "model.update", "model.lifecycle",
		"deployment.create", "deployment.update",
		"route.create", "route.update", "route.delete",
		"deployment.delete", "model.delete", "provider.delete",
	}
	if len(rec.events) != len(wantActions) {
		t.Fatalf("audit events = %d, want %d: %+v", len(rec.events), len(wantActions), rec.events)
	}
	for i, ev := range rec.events {
		if ev.Action != wantActions[i] {
			t.Errorf("event %d action = %s, want %s", i, ev.Action, wantActions[i])
		}
		if ev.ActorUser != "op-1" || ev.ActorApp != "ops" {
			t.Errorf("event %d attribution = %s/%s", i, ev.ActorUser, ev.ActorApp)
		}
	}
	// Service() 直通。
	if mgr.Service() == nil {
		t.Fatal("Service() must expose the catalog service")
	}
}

func TestCatalogManager_PublishRollbackAudit(t *testing.T) {
	mgr, rec, _ := newManagerFixture()
	ctx := context.Background()
	if _, err := mgr.Publish(ctx, managerActor()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Rollback(ctx, managerActor(), 1); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 2 || rec.events[0].Action != "catalog.publish" || rec.events[1].Action != "catalog.rollback" {
		t.Fatalf("publish/rollback audit = %+v", rec.events)
	}
}

func TestCatalogManager_ParseActor(t *testing.T) {
	u, a := ParseActor("user:u1@app:a1")
	if u != "u1" || a != "a1" {
		t.Fatalf("ParseActor = %q/%q", u, a)
	}
	// 非标准形状：尽力解析，不 panic。
	u, a = ParseActor("bare")
	if u != "" && u != "bare" {
		t.Fatalf("ParseActor bare = %q/%q", u, a)
	}
}

// --- 审查修复 Important-3/Important-4 ---------------------------------------

// failingRecorder 的 Record 永远失败（审计后端故障）。
type failingRecorder struct{ err error }

func (f failingRecorder) Record(ctx context.Context, ev AuditEvent) error { return f.err }

// operator Reason 被采集进审计事件；未传时为空串（参数位保留给 httpapi 接线）。
func TestCatalogManager_ReasonRecorded(t *testing.T) {
	mgr, rec, _ := newManagerFixture()
	ctx := context.Background()
	if err := mgr.CreateModel(ctx, managerActor(), &domain.Model{
		ID: "m-reason", DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}, "上架新模型"); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 1 || rec.events[0].Reason != "上架新模型" {
		t.Fatalf("reason not captured: %+v", rec.events)
	}
	if err := mgr.DeleteModel(ctx, managerActor(), "m-reason"); err != nil {
		t.Fatal(err)
	}
	if rec.events[1].Reason != "" {
		t.Fatalf("omitted reason = %q, want empty", rec.events[1].Reason)
	}
}

// 审计写失败对已生效变更返回成功（变更确已提交是事实）——返回错误会诱导
// 重试产生重复修订；审计缺失由 ERROR 日志告警。
func TestCatalogManager_AuditFailureStillSucceeds(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), failingRecorder{err: errors.New("audit boom")}, nil)
	ctx := context.Background()

	rev, err := mgr.Publish(ctx, managerActor())
	if err != nil || rev != 1 {
		t.Fatalf("publish = %d, %v — 审计失败不得对已生效变更返回错误", rev, err)
	}
	rev, err = mgr.Rollback(ctx, managerActor(), 1)
	if err != nil || rev != 2 {
		t.Fatalf("rollback = %d, %v — 审计失败不得对已生效变更返回错误", rev, err)
	}
	if err := mgr.CreateModel(ctx, managerActor(), &domain.Model{
		ID: "m-audit-fail", DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}); err != nil {
		t.Fatalf("create = %v — 审计失败不得对已生效变更返回错误", err)
	}
	if _, err := fs.GetModel(ctx, "m-audit-fail"); err != nil {
		t.Fatal("mutation must be effective despite audit failure")
	}
}

// 安全审查 M-2：发布事务内激活失败必须整体回滚——不得留下孤儿 draft 修订，
// 错误本身要干净透出（调用方重试是安全的）。
func TestCatalogManager_PublishActivationFailureLeavesNoOrphanDraft(t *testing.T) {
	fs := newFakeCatalogStore()
	fs.publishErr = errors.New("activation boom")
	mgr := NewCatalogManager(catalog.NewService(fs), &spyRecorder{}, nil)
	ctx := context.Background()

	if _, err := mgr.Publish(ctx, managerActor()); err == nil {
		t.Fatal("activation failure must surface as publish error")
	}
	if len(fs.revisions) != 0 {
		t.Fatalf("orphan draft revision left after failed activation: %+v", fs.revisions)
	}

	// 失败后的重试是安全的：恢复存储后同一发布成功落到 revision 1。
	fs.publishErr = nil
	rev, err := mgr.Publish(ctx, managerActor())
	if err != nil || rev != 1 {
		t.Fatalf("retry after transient failure = %d, %v; want 1, nil", rev, err)
	}
	if len(fs.revisions) != 1 || fs.revisions[0].Status != domain.RevisionPublished || !fs.revisions[0].IsActive {
		t.Fatalf("retried revision = %+v, want published+active", fs.revisions[0])
	}
}

// 安全审查 M-3：发布的目录抽取必须全部经由发布事务（REPEATABLE READ 快照，
// 见 postgres.BeginPublish），不得绕过事务直接读 store——否则并发编辑能在
// 抽取中途造出自相矛盾的 payload。
func TestCatalogManager_PublishDrainsThroughSingleTx(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), &spyRecorder{}, nil)
	ctx := context.Background()

	// 造一个最小可发布目录。
	if err := mgr.CreateProvider(ctx, managerActor(), &domain.Provider{
		ID: uuid.NewString(), Code: "snap", DisplayName: "S", AccessType: domain.AccessOfficialAPI,
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CreateModel(ctx, managerActor(), &domain.Model{
		ID: "snap-m", DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	fs.directListCalls = 0

	if _, err := mgr.Publish(ctx, managerActor()); err != nil {
		t.Fatal(err)
	}
	if fs.directListCalls != 0 {
		t.Errorf("publish drained %d reads outside the publish tx; all reads must share one snapshot", fs.directListCalls)
	}
	// models/providers/deployments/routes 四类抽取都走了事务（路由按模型逐个抽取）。
	if fs.txReads < 4 {
		t.Errorf("tx reads = %d, want ≥4 (models/providers/deployments/routes)", fs.txReads)
	}
}

// 安全审查 I-7：审计补写失败必须触发告警钩子（默认 AUDIT_WRITE_FAILED
// 结构化 ERROR 日志，生产接 on-call 通道），携带正确的动作/对象/归因；操作
// 结果本身不变——变更已提交是事实，审计缺失是告警而非可回滚状态。
func TestCatalogManager_AuditFailureInvokesAlertHook(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), failingRecorder{err: errors.New("audit backend down")}, nil)
	ctx := context.Background()

	var alerts []AuditEvent
	var causes []error
	prev := AuditAlertHook
	AuditAlertHook = func(ctx context.Context, ev AuditEvent, cause error) {
		alerts = append(alerts, ev)
		causes = append(causes, cause)
	}
	t.Cleanup(func() { AuditAlertHook = prev })

	if err := mgr.CreateModel(ctx, managerActor(), &domain.Model{
		ID: "m-alert", DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}, "probe"); err != nil {
		t.Fatalf("mutation must succeed despite audit failure: %v", err)
	}
	if _, err := fs.GetModel(ctx, "m-alert"); err != nil {
		t.Fatal("mutation must be effective despite audit failure")
	}

	if len(alerts) != 1 {
		t.Fatalf("alert hook invocations = %d, want 1", len(alerts))
	}
	ev := alerts[0]
	if ev.Action != "model.create" || ev.ObjectType != "model" || ev.ObjectID != "m-alert" {
		t.Errorf("alert event = %+v, want model.create/model/m-alert", ev)
	}
	if ev.ActorUser != "op-1" || ev.ActorApp != "ops" {
		t.Errorf("alert attribution = %s/%s", ev.ActorUser, ev.ActorApp)
	}
	if len(causes) != 1 || causes[0] == nil {
		t.Errorf("alert cause = %v, want the recorder error", causes)
	}

	// Publish 失败审计同样告警（catalog.publish 的 objectID 为空串是既定形状）。
	if _, err := mgr.Publish(ctx, managerActor()); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || alerts[1].Action != "catalog.publish" {
		t.Fatalf("publish audit failure must alert: %+v", alerts)
	}
}
