package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// publish_dryrun_t9_test.go — T9 的发布预演契约：
//
//   - **停用 provider 的部署既不算 active，也不被探测**（T3 携带的必修
//     发现：预演只按 deployment 状态过滤，把停用 provider 背后的部署当成
//     「会生效」并真的出站探测）；
//   - 预演汇总**可判定问题**（停用 provider、无账号、路由能力收窄、模型
//     无可调度路由），并标明哪些是**阻断发布**的配置问题、哪些只是**网络
//     探测提示**；
//   - HTTP 200 不等于每个 probe.ok 都是 true —— 结论分桶统计在 summary 里。
//
// 断言落在**已存在**的 API 表面（report 的 JSON 形状 + 真实的出站探测），
// 因此在未改动的生产代码上是行为断言失败（RED），不是编译错误。

// t9DryRunProber 是 DeploymentProber 的手搓 fake：记录被探测的部署，按 id
// 返回预设结果。
type t9DryRunProber struct {
	probed  []string
	results map[string]ProbeResult
}

func (p *t9DryRunProber) ProbeDeployment(ctx context.Context, id string) (*ProbeResult, error) {
	p.probed = append(p.probed, id)
	if r, ok := p.results[id]; ok {
		cp := r
		return &cp, nil
	}
	return &ProbeResult{DeploymentID: id, OK: true, Status: 200}, nil
}

// t9DryRunReportJSON 是发布预演报告的线上形状（RED 阶段新字段还不存在）。
type t9DryRunReportJSON struct {
	WouldPublish struct {
		NextRevision      int `json:"next_revision"`
		Models            int `json:"models"`
		Providers         int `json:"providers"`
		Deployments       int `json:"deployments"`
		ActiveDeployments int `json:"active_deployments"`
		Routes            int `json:"routes"`
		DisabledProviders int `json:"disabled_providers"`
		DeploymentsBehind int `json:"deployments_behind_disabled_providers"`
	} `json:"would_publish"`
	Probes []struct {
		DeploymentID string `json:"deployment_id"`
		OK           bool   `json:"ok"`
		ProbeOutcome string `json:"probe_outcome"`
	} `json:"probes"`
	Issues []struct {
		Code     string `json:"code"`
		Severity string `json:"severity"`
		Subject  string `json:"subject"`
		Message  string `json:"message"`
	} `json:"issues"`
	Summary struct {
		ProbesTotal       int  `json:"probes_total"`
		ProbesOK          int  `json:"probes_ok"`
		ProbesFailed      int  `json:"probes_failed"`
		ProbesUnsupported int  `json:"probes_unsupported"`
		ProbesUnconfirmed int  `json:"probes_unconfirmed"`
		Blockers          int  `json:"blockers"`
		Hints             int  `json:"hints"`
		AllProbesOK       bool `json:"all_probes_ok"`
		AccountInventory  bool `json:"account_inventory_checked"`
	} `json:"summary"`
}

func t9DryRunJSON(t *testing.T, report *PublishDryRunReport) t9DryRunReportJSON {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal dry-run report: %v", err)
	}
	var out t9DryRunReportJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode dry-run report: %v (%s)", err, raw)
	}
	return out
}

func t9DryRunIssueCodes(j t9DryRunReportJSON) map[string]string {
	out := map[string]string{}
	for _, is := range j.Issues {
		out[is.Code] = is.Severity
	}
	return out
}

// TestPublishDryRun_DisabledProviderDeploymentIsNotCountedNorProbed 是 T3
// 携带到 T9 的必修发现：停用 provider 背后的部署既**不算** active，也
// **不被探测**（预演不得对一条永远不会承接流量的部署出站）。
func TestPublishDryRun_DisabledProviderDeploymentIsNotCountedNorProbed(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()

	off := dryRunValidProvider("p-off")
	off.Status = "disabled"
	if err := fs.InsertProvider(ctx, off); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertProvider(ctx, dryRunValidProvider("p-on")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertModel(ctx, dryRunValidModel("glm-4.6")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertDeployment(ctx, dryRunValidDeployment("dep-off", "p-off", domain.DeploymentActive)); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertDeployment(ctx, dryRunValidDeployment("dep-on", "p-on", domain.DeploymentActive)); err != nil {
		t.Fatal(err)
	}
	prober := &t9DryRunProber{}

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, id := range prober.probed {
		if id == "dep-off" {
			t.Fatalf("a deployment behind a disabled provider must not be probed: %v", prober.probed)
		}
	}
	j := t9DryRunJSON(t, report)
	if j.WouldPublish.ActiveDeployments != 1 {
		t.Errorf("active_deployments = %d, want 1 (停用 provider 的部署不算会生效的部署)",
			j.WouldPublish.ActiveDeployments)
	}
	if j.WouldPublish.DisabledProviders != 1 {
		t.Errorf("disabled_providers = %d, want 1 (停用 provider 必须被如实汇总)", j.WouldPublish.DisabledProviders)
	}
	if j.WouldPublish.DeploymentsBehind != 1 {
		t.Errorf("deployments_behind_disabled_providers = %d, want 1", j.WouldPublish.DeploymentsBehind)
	}
	codes := t9DryRunIssueCodes(j)
	if sev, ok := codes["provider_disabled"]; !ok {
		t.Errorf("issues must report provider_disabled, got %v", codes)
	} else if sev != "blocker" {
		t.Errorf("provider_disabled severity = %q, want blocker (发布后这份目录跑起来不是配的那个)", sev)
	}
	if j.Summary.Blockers < 1 {
		t.Errorf("summary.blockers = %d, want ≥1", j.Summary.Blockers)
	}
	if len(fs.revisions) != 0 {
		t.Fatalf("dry run must not publish, revisions=%d", len(fs.revisions))
	}
}

// TestPublishDryRun_IssuesSeparateBlockersFromNetworkHints 钉住：配置问题
// 是 blocker，网络探测提示是 hint；两者分开统计，HTTP 200 不等于每个
// probe.ok 都是 true。
//
// 这里用**真实的探测服务**打 httptest 上游（上游对 /models 返回 404 =
// 供应商不提供该端点），因此报告里的 unsupported 结论是真实链路产生的，
// 不是手搓的假结果。
func TestPublishDryRun_IssuesSeparateBlockersFromNetworkHints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()

	if err := fs.InsertProvider(ctx, dryRunValidProvider("p1")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertModel(ctx, dryRunValidModel("glm-4.6")); err != nil {
		t.Fatal(err)
	}
	dep := dryRunValidDeployment("dep-1", "p1", domain.DeploymentActive)
	// localhost 通过 base_url 校验（非字面 IP）。
	dep.BaseURL = strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1)
	if err := fs.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}

	probeStore := &fakeProbeStore{
		deployments: map[string]domain.Deployment{"dep-1": *dep},
		credentials: []domain.Credential{
			{ID: "cred-1", ProviderID: "p1", AuthType: "api_key", Status: "active", Generation: 1},
		},
	}
	resolver := &fakeBearerResolver{
		tokens:    map[string]string{"cred-1": "sk-probe-secret"},
		authTypes: map[string]string{"cred-1": "api_key"},
	}
	prober := NewProbeService(probeStore, resolver, permissiveURL, &http.Client{})

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	j := t9DryRunJSON(t, report)
	codes := t9DryRunIssueCodes(j)
	if sev, ok := codes["deployment_no_account"]; !ok {
		t.Errorf("issues must report deployment_no_account, got %v", codes)
	} else if sev != "blocker" {
		t.Errorf("deployment_no_account severity = %q, want blocker", sev)
	}
	if sev, ok := codes["probe_unsupported"]; !ok {
		t.Errorf("issues must report the network hint probe_unsupported, got %v", codes)
	} else if sev != "hint" {
		t.Errorf("probe_unsupported severity = %q, want hint (网络探测提示不阻断发布)", sev)
	}
	if j.Summary.ProbesTotal != 1 || j.Summary.ProbesOK != 0 || j.Summary.ProbesUnsupported != 1 {
		t.Errorf("summary probe buckets = %+v (want total=1 ok=0 unsupported=1)", j.Summary)
	}
	if j.Summary.AllProbesOK {
		t.Error("all_probes_ok must be false when a probe did not verify")
	}
	if j.Summary.Hints < 1 {
		t.Errorf("summary.hints = %d, want ≥1", j.Summary.Hints)
	}
	if !j.Summary.AccountInventory {
		t.Error("account_inventory_checked must be true when the inventory was read")
	}
}

// TestPublishDryRun_AccountInventoryUnknownIsNotGuessed 钉住：账号清单读
// 不到时，预演**不猜**「无账号」——它明确说清单未知（hint），而不是谎称
// provider 没有账号。
func TestPublishDryRun_AccountInventoryUnknownIsNotGuessed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()

	if err := fs.InsertProvider(ctx, dryRunValidProvider("p1")); err != nil {
		t.Fatal(err)
	}
	dep := dryRunValidDeployment("dep-1", "p1", domain.DeploymentActive)
	// localhost 通过 base_url 校验（非字面 IP）。
	dep.BaseURL = strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1)
	if err := fs.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	probeStore := &fakeProbeStore{
		deployments: map[string]domain.Deployment{"dep-1": *dep},
		credentials: []domain.Credential{
			{ID: "cred-1", ProviderID: "p1", AuthType: "api_key", Status: "active", Generation: 1},
		},
		accountsErr: context.DeadlineExceeded,
	}
	resolver := &fakeBearerResolver{
		tokens:    map[string]string{"cred-1": "sk-probe-secret"},
		authTypes: map[string]string{"cred-1": "api_key"},
	}
	prober := NewProbeService(probeStore, resolver, permissiveURL, &http.Client{})

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	j := t9DryRunJSON(t, report)
	codes := t9DryRunIssueCodes(j)
	if _, ok := codes["deployment_no_account"]; ok {
		t.Error("an unreadable account inventory must NOT be reported as no account")
	}
	if _, ok := codes["account_inventory_unavailable"]; !ok {
		t.Errorf("issues must report account_inventory_unavailable, got %v", codes)
	}
}

// TestPublishDryRun_ModelWithoutDispatchableRouteIsABlocker 钉住：发布后
// 没有任何可调度路由的模型是**配置问题**（阻断级），不是网络提示。
func TestPublishDryRun_ModelWithoutDispatchableRouteIsABlocker(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()

	if err := fs.InsertProvider(ctx, dryRunValidProvider("p1")); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertModel(ctx, dryRunValidModel("glm-4.6")); err != nil {
		t.Fatal(err)
	}
	// 没有部署、也没有路由 → 该模型没有任何可调度路由。
	prober := &t9DryRunProber{}

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	j := t9DryRunJSON(t, report)
	codes := t9DryRunIssueCodes(j)
	if sev, ok := codes["model_not_dispatchable"]; !ok {
		t.Errorf("issues must report model_not_dispatchable, got %v", codes)
	} else if sev != "blocker" {
		t.Errorf("model_not_dispatchable severity = %q, want blocker", sev)
	}
}

// TestPublishDryRun_RouteCapabilityNarrowingIsReported 钉住：路由的能力清单
// 收窄了模型声明的能力（存量非空清单不含 image）时，预演必须点出来 ——
// 否则运营以为配了图片能力，实际带图请求会在准入前被拒。
func TestPublishDryRun_RouteCapabilityNarrowingIsReported(t *testing.T) {
	fs := newFakeCatalogStore()
	mgr := NewCatalogManager(catalog.NewService(fs), nil, nil)
	ctx := context.Background()

	if err := fs.InsertProvider(ctx, dryRunValidProvider("p1")); err != nil {
		t.Fatal(err)
	}
	m := dryRunValidModel("glm-vision")
	m.InputModalities = []string{"text", "image"}
	if err := fs.InsertModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertDeployment(ctx, dryRunValidDeployment("dep-1", "p1", domain.DeploymentActive)); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: "glm-vision", DeploymentID: "dep-1", Weight: 1, Enabled: true,
		Capabilities: []string{"tools", "reasoning"},
	}); err != nil {
		t.Fatal(err)
	}
	prober := &t9DryRunProber{}

	report, err := mgr.PublishDryRun(ctx, prober)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	j := t9DryRunJSON(t, report)
	codes := t9DryRunIssueCodes(j)
	if sev, ok := codes["route_capabilities_narrowed"]; !ok {
		t.Errorf("issues must report route_capabilities_narrowed, got %v", codes)
	} else if sev != "blocker" {
		t.Errorf("route_capabilities_narrowed severity = %q, want blocker", sev)
	}
}
