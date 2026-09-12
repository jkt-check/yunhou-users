package management

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// bulk_import_test.go — 批量导入的纯规则验证（假存储；真库事务行为在
// postgres/operations_repo_test.go）:逐项错误、大小上限、task_id 形状、
// 有错误时绝不触碰写路径（不半发布的服务层闸）。

// fakeBulkStore answers "nothing exists" to every probe and fails the test
// if the write path is reached when it must not be.
type fakeBulkStore struct {
	t            *testing.T
	allowApply   bool
	applied      *BulkCatalog
	appliedActor string
	appliedTask  string
}

func (f *fakeBulkStore) Begin(ctx context.Context) (domain.UnitOfWork, error) { return nil, nil }
func (f *fakeBulkStore) GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error) {
	return nil, domain.NewError(domain.CodeNotFound, "nope")
}
func (f *fakeBulkStore) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	return nil, domain.NewError(domain.CodeNotFound, "nope")
}
func (f *fakeBulkStore) FindDeployment(ctx context.Context, providerID, upstreamModel, baseURL string) (*domain.Deployment, error) {
	return nil, domain.NewError(domain.CodeNotFound, "nope")
}
func (f *fakeBulkStore) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return nil, nil
}
func (f *fakeBulkStore) GetBulkImportByTaskID(ctx context.Context, taskID string) (*BulkImportResult, error) {
	return nil, domain.NewError(domain.CodeNotFound, "nope")
}
func (f *fakeBulkStore) ApplyBulkImportTx(ctx context.Context, w domain.UnitOfWork, taskID, actor string, plan *BulkCatalog) (*BulkImportResult, error) {
	if !f.allowApply {
		f.t.Fatalf("ApplyBulkImportTx reached while document has errors (半发布守卫失效)")
	}
	f.applied, f.appliedActor, f.appliedTask = plan, actor, taskID
	return &BulkImportResult{TaskID: taskID, Items: []BulkItemResult{}}, nil
}

func validBulkDoc() *BulkCatalog {
	return &BulkCatalog{
		Providers: []BulkProvider{{Code: "glm", DisplayName: "GLM", AccessType: "official_api"}},
		Models: []BulkModel{{
			ID: "glm-4.7", DisplayName: "GLM 4.7",
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols: []string{"openai_chat"},
			Deployments: []BulkDeployment{{
				ProviderCode: "glm", UpstreamModel: "glm-4.7-upstream",
				BaseURL: "https://api.glm.example.com", Protocol: "openai_chat",
			}},
		}},
	}
}

func TestBulkImport_DryRunWritesNothing(t *testing.T) {
	fs := &fakeBulkStore{t: t}
	svc := NewBulkImportService(fs, nil, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-1", validBulkDoc(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.HasErrors() || !res.DryRun || res.Committed {
		t.Fatalf("dry-run result = %+v", res)
	}
	// 1 provider + 1 model + 1 deployment + 1 derived route，全部 would_insert。
	if len(res.Items) != 4 || res.Inserted != 4 || res.Skipped != 0 {
		t.Fatalf("items = %+v (inserted=%d skipped=%d)", res.Items, res.Inserted, res.Skipped)
	}
	for _, it := range res.Items {
		if it.Status != BulkItemWouldInsert {
			t.Fatalf("item %s/%s status = %s, want would_insert", it.Kind, it.NaturalKey, it.Status)
		}
	}
	if fs.applied != nil {
		t.Fatal("dry-run must not reach the write path")
	}
}

func TestBulkImport_PerItemErrorsBlockCommit(t *testing.T) {
	doc := validBulkDoc()
	doc.Models = append(doc.Models, BulkModel{
		ID: "Bad Model!", // 非法 ID（catalog.ValidateModel 拒绝）
		Deployments: []BulkDeployment{{
			ProviderCode: "ghost", // 文档内不存在的 provider
			UpstreamModel: "x", Protocol: "openai_chat",
		}},
	})
	fs := &fakeBulkStore{t: t}
	svc := NewBulkImportService(fs, nil, func(context.Context, string) error { return nil })

	// commit：逐项错误 + 一行不写。
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-err", doc, false)
	if err != nil {
		t.Fatal(err) // 逐项错误不是 Go error — 是结果里的 per-item error
	}
	if !res.HasErrors() || res.Committed {
		t.Fatalf("commit-with-errors result = %+v", res)
	}
	var modelErr, depErr bool
	for _, it := range res.Items {
		if it.Kind == BulkKindModel && it.NaturalKey == "Bad Model!" && it.Status == BulkItemError {
			modelErr = true
		}
		if it.Kind == BulkKindDeployment && strings.Contains(it.NaturalKey, "ghost|") && it.Status == BulkItemError {
			depErr = true
		}
	}
	if !modelErr || !depErr {
		t.Fatalf("per-item errors missing: %+v", res.Items)
	}
	if fs.applied != nil {
		t.Fatal("commit with item errors must not reach the write path (绝不半发布)")
	}
}

func TestBulkImport_SizeCap(t *testing.T) {
	doc := &BulkCatalog{}
	for i := 0; i < MaxBulkImportItems+1; i++ {
		doc.Providers = append(doc.Providers, BulkProvider{
			Code: fmt.Sprintf("p%03d", i), DisplayName: "x", AccessType: "official_api",
		})
	}
	svc := NewBulkImportService(&fakeBulkStore{t: t}, nil, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-big", doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasErrors() || len(res.Items) != 1 || !strings.Contains(res.Items[0].Error, "exceeds limit") {
		t.Fatalf("size cap result = %+v", res)
	}
}

func TestBulkImport_TaskIDShape(t *testing.T) {
	svc := NewBulkImportService(&fakeBulkStore{t: t}, nil, func(context.Context, string) error { return nil })
	for _, bad := range []string{"", "-lead-dash", "with space", strings.Repeat("a", 200)} {
		if _, err := svc.Import(context.Background(), "user:op1@app:ops", bad, validBulkDoc(), true); err == nil {
			t.Fatalf("task_id %q accepted", bad)
		}
	}
}

func TestBulkImport_DryRunMarksExisting(t *testing.T) {
	// 存在性预演：provider 已存在 → would_skip。
	fs := &fakeBulkStore{t: t}
	// 手动覆盖一个 probe：fake 上 GetProviderByCode 返回存在行。
	fs2 := &probeBulkStore{fakeBulkStore: fs, provider: &domain.Provider{ID: "prov-1", Code: "glm"}}
	svc := NewBulkImportService(fs2, nil, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-2", validBulkDoc(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Inserted != 3 {
		t.Fatalf("dry-run preview = %+v (inserted=%d skipped=%d)", res.Items, res.Inserted, res.Skipped)
	}
}

// probeBulkStore overrides selected probes of fakeBulkStore.
type probeBulkStore struct {
	*fakeBulkStore
	provider *domain.Provider
}

func (p *probeBulkStore) GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error) {
	if p.provider != nil && p.provider.Code == code {
		return p.provider, nil
	}
	return p.fakeBulkStore.GetProviderByCode(ctx, code)
}

// --- Task 16 minor ①：批量导入审计与导入效果同事务（RecordTx），审计失败
// 或记录器无事务能力即整体回滚，绝不落下无审计的目录变更。 ---

type fakeUow struct{ committed, rolledBack bool }

func (u *fakeUow) Commit(ctx context.Context) error   { u.committed = true; return nil }
func (u *fakeUow) Rollback(ctx context.Context) error { u.rolledBack = true; return nil }

// uowBulkStore is fakeBulkStore with a real (fake) UnitOfWork.
type uowBulkStore struct {
	*fakeBulkStore
	uow *fakeUow
}

func (s *uowBulkStore) Begin(ctx context.Context) (domain.UnitOfWork, error) { return s.uow, nil }

// txFailRecorder implements AuditTxRecorder and always fails RecordTx.
type txFailRecorder struct{ err error }

func (r txFailRecorder) Record(ctx context.Context, ev AuditEvent) error { return r.err }
func (r txFailRecorder) RecordTx(ctx context.Context, w domain.UnitOfWork, ev AuditEvent) error {
	return r.err
}

// posthocOnlyRecorder implements only Record (no transactional support).
type posthocOnlyRecorder struct{}

func (posthocOnlyRecorder) Record(ctx context.Context, ev AuditEvent) error { return nil }

// txSpyRecorder records whether RecordTx ran before Commit.
type txSpyRecorder struct {
	uow          *fakeUow
	txCalls      int
	committedAtCall bool
}

func (r *txSpyRecorder) Record(ctx context.Context, ev AuditEvent) error { return nil }
func (r *txSpyRecorder) RecordTx(ctx context.Context, w domain.UnitOfWork, ev AuditEvent) error {
	r.txCalls++
	r.committedAtCall = r.uow.committed
	return nil
}

func newUowFixture(t *testing.T) (*uowBulkStore, *fakeUow) {
	t.Helper()
	uow := &fakeUow{}
	return &uowBulkStore{fakeBulkStore: &fakeBulkStore{t: t, allowApply: true}, uow: uow}, uow
}

func TestBulkImport_AuditInsideTransaction(t *testing.T) {
	fs, uow := newUowFixture(t)
	rec := &txSpyRecorder{uow: uow}
	svc := NewBulkImportService(fs, rec, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-audit", validBulkDoc(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || !uow.committed || uow.rolledBack {
		t.Fatalf("commit state: committed=%v uow=%+v", res.Committed, uow)
	}
	if rec.txCalls != 1 || rec.committedAtCall {
		t.Fatalf("audit must run inside the tx before commit: calls=%d committedAtCall=%v",
			rec.txCalls, rec.committedAtCall)
	}
}

func TestBulkImport_AuditTxFailureRollsBack(t *testing.T) {
	fs, uow := newUowFixture(t)
	svc := NewBulkImportService(fs, txFailRecorder{err: domain.NewError(domain.CodeInternal, "audit boom")},
		func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-audit-fail", validBulkDoc(), false)
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("err = %v, want internal", err)
	}
	if res != nil || !uow.rolledBack || uow.committed {
		t.Fatalf("audit failure must roll the import back: res=%+v uow=%+v", res, uow)
	}
}

func TestBulkImport_PosthocOnlyRecorderRejected(t *testing.T) {
	fs, uow := newUowFixture(t)
	svc := NewBulkImportService(fs, posthocOnlyRecorder{}, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-audit-posthoc", validBulkDoc(), false)
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("err = %v, want internal (fail-closed)", err)
	}
	if res != nil || !uow.rolledBack || uow.committed {
		t.Fatalf("non-transactional recorder must roll the import back: res=%+v uow=%+v", res, uow)
	}
}

// Task 16 minor ⑤：缺 provider 引用的错误文案如实——不再谎称"apply 时解析
// 既有 provider"。
func TestBulkImport_MissingProviderMessageTruthful(t *testing.T) {
	doc := validBulkDoc()
	doc.Providers = nil // 文档不列 provider，部署引用 glm → 逐项错误
	svc := NewBulkImportService(&fakeBulkStore{t: t}, nil, func(context.Context, string) error { return nil })
	res, err := svc.Import(context.Background(), "user:op1@app:ops", "task-msg", doc, false)
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, it := range res.Items {
		if it.Kind == BulkKindDeployment && it.Status == BulkItemError {
			msg = it.Error
		}
	}
	if msg == "" {
		t.Fatalf("deployment error missing: %+v", res.Items)
	}
	if !strings.Contains(msg, "not auto-resolved") || strings.Contains(msg, "resolve at apply time") {
		t.Fatalf("contradictory message still present: %q", msg)
	}
}

// Task 16 minor ⑦：运营列表 limit 钳到 500（不静默回退默认 100）。
func TestClampOpsLimit(t *testing.T) {
	for in, want := range map[int]int{
		0: 100, -7: 100, // 未给/非正 → 默认
		1: 1, 200: 200, 500: 500, // 范围内原样
		501: 500, 100000: 500, // 超上限钳到 500（OpenAPI maximum）
	} {
		if got := clampOpsLimit(in); got != want {
			t.Errorf("clampOpsLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestPricingPreview_DiffSets(t *testing.T) {
	added, removed := diffStringSets([]string{"a", "b", "c"}, []string{"b", "d"})
	if len(added) != 2 || added[0] != "a" || added[1] != "c" {
		t.Fatalf("added = %v", added)
	}
	if len(removed) != 1 || removed[0] != "d" {
		t.Fatalf("removed = %v", removed)
	}
}
