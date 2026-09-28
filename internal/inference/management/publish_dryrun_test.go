package management

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// publish_dryrun_test.go — Task 2（R7-N7）失败测试先行：publish dry_run
// advisory 报告。钉住：dry_run 零写库（revisions 不变）、只探测会生效的
// active 部署、探测失败绝不阻断报告、目录校验失败原样返回、nil prober
// fail-closed。

// recordingProber 是 DeploymentProber 的手搓 fake：记录被探测的
// deployment 顺序，按 id 返回预设结果。
type recordingProber struct {
	probed  []string
	results map[string]ProbeResult
	errs    map[string]error
}

func (p *recordingProber) ProbeDeployment(ctx context.Context, id string) (*ProbeResult, error) {
	p.probed = append(p.probed, id)
	if err := p.errs[id]; err != nil {
		return nil, err
	}
	if r, ok := p.results[id]; ok {
		cp := r
		return &cp, nil
	}
	return &ProbeResult{DeploymentID: id, OK: true, Status: 200}, nil
}

func dryRunValidProvider(id string) *domain.Provider {
	return &domain.Provider{ID: id, Code: "prov-" + id, DisplayName: "Provider " + id,
		AccessType: domain.AccessOfficialAPI, Status: "active"}
}

func dryRunValidModel(id string) *domain.Model {
	return &domain.Model{
		ID: id, DisplayName: "Model " + id, Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}
}

func dryRunValidDeployment(id, providerID string, status domain.DeploymentStatus) *domain.Deployment {
	return &domain.Deployment{
		ID: id, ProviderID: providerID, UpstreamModel: "glm-upstream",
		BaseURL: "https://api.example.com/v1", Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: time.Second, RequestTimeout: 5 * time.Second,
		Status: status,
	}
}

func TestPublishDryRun_ProbesActiveDeploymentsOnly(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()
	if err := fs.InsertProvider(ctx, dryRunValidProvider("p1")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertModel(ctx, dryRunValidModel("glm-4.6")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertDeployment(ctx, dryRunValidDeployment("dep-active", "p1", domain.DeploymentActive)); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertDeployment(ctx, dryRunValidDeployment("dep-draft", "p1", domain.DeploymentDraft)); err != nil {
		t.Fatal(err)
	}
	prober := &recordingProber{results: map[string]ProbeResult{
		"dep-active": {DeploymentID: "dep-active", OK: true, Status: 200, LatencyMS: 42},
	}}

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.WouldPublish.NextRevision != 1 {
		t.Fatalf("next_revision = %d, want 1", report.WouldPublish.NextRevision)
	}
	if report.WouldPublish.Deployments != 2 || report.WouldPublish.ActiveDeployments != 1 {
		t.Fatalf("summary = %+v", report.WouldPublish)
	}
	if len(report.Probes) != 1 || report.Probes[0].DeploymentID != "dep-active" || !report.Probes[0].OK {
		t.Fatalf("probes = %+v", report.Probes)
	}
	// draft 部署不探测（不上线；避免半编辑状态噪声）。
	for _, id := range prober.probed {
		if id == "dep-draft" {
			t.Fatalf("draft deployment must not be probed: %v", prober.probed)
		}
	}
	// 零写库：没有任何 revision 产生。
	if len(fs.revisions) != 0 {
		t.Fatalf("dry run must not publish, revisions=%d", len(fs.revisions))
	}
}

// TestPublishDryRun_ProbeFailureNeverBlocks：探测失败（404 / 探测器自身
// 错误）都只进报告，绝不让 dry_run 返回错误。
func TestPublishDryRun_ProbeFailureNeverBlocks(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()
	_ = fs.InsertProvider(ctx, dryRunValidProvider("p1"))
	_ = fs.InsertDeployment(ctx, dryRunValidDeployment("dep-404", "p1", domain.DeploymentActive))
	_ = fs.InsertDeployment(ctx, dryRunValidDeployment("dep-boom", "p1", domain.DeploymentActive))
	prober := &recordingProber{
		results: map[string]ProbeResult{
			"dep-404": {DeploymentID: "dep-404", OK: false, Status: 404, ErrorSummary: "upstream returned HTTP 404"},
		},
		errs: map[string]error{"dep-boom": errors.New("prober exploded: internal detail")},
	}

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("probe failures must not fail the dry run: %v", err)
	}
	if len(report.Probes) != 2 {
		t.Fatalf("probes = %+v", report.Probes)
	}
	byID := map[string]ProbeResult{}
	for _, p := range report.Probes {
		byID[p.DeploymentID] = p
	}
	if byID["dep-404"].OK || byID["dep-404"].Status != 404 {
		t.Fatalf("dep-404 probe = %+v", byID["dep-404"])
	}
	if byID["dep-boom"].OK || byID["dep-boom"].ErrorSummary == "" {
		t.Fatalf("dep-boom probe must be an in-band failure entry, got %+v", byID["dep-boom"])
	}
	if len(fs.revisions) != 0 {
		t.Fatalf("dry run must not publish")
	}
}

func TestPublishDryRun_ValidationFailurePropagates(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()
	// 引用未知 provider 的 deployment → 与正式 publish 同一校验失败。
	_ = fs.InsertDeployment(ctx, dryRunValidDeployment("dep-x", "prov-missing", domain.DeploymentActive))
	prober := &recordingProber{}

	_, err := mgr.PublishDryRun(ctx, prober)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("want CodeInvalidInput, got %v", err)
	}
	if len(prober.probed) != 0 {
		t.Fatalf("validation failure must short-circuit before probing: %v", prober.probed)
	}
	if len(fs.revisions) != 0 {
		t.Fatalf("dry run must not publish")
	}
}

func TestPublishDryRun_NilProberFailsClosed(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	if _, err := mgr.PublishDryRun(context.Background(), nil); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("nil prober must fail closed with CodeInternal, got %v", err)
	}
}

// TestPublishDryRun_NextRevisionFollowsHistory：已有 revision 时
// next_revision 顺延（与正式 Publish 的 latest+1 口径一致）。
func TestPublishDryRun_NextRevisionFollowsHistory(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()
	_ = fs.InsertProvider(ctx, dryRunValidProvider("p1"))
	if _, err := mgr.Publish(ctx, "tester", "seed"); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	report, err := mgr.PublishDryRun(ctx, &recordingProber{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.WouldPublish.NextRevision != 2 {
		t.Fatalf("next_revision = %d, want 2", report.WouldPublish.NextRevision)
	}
	if len(fs.revisions) != 1 {
		t.Fatalf("dry run must not append revisions, got %d", len(fs.revisions))
	}
}
