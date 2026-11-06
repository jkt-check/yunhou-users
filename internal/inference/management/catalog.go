// Package management hosts the operator-facing use cases of the inference
// module and — since Task 11 — the customer self-service READ views
// (quota_view / usage_view / subscription_view, backing /user/model-*).
// Each write carries an actor (the authenticated operator subject
// once Task 4 wires operator auth); today the actor is recorded as
// revision attribution (created_by) and handed to the catalog service.
// Task 4 adds the authorization gate and the audit trail without changing
// these signatures.
package management

import (
	"context"
	"fmt"
	"strings"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// CatalogManager is the operator façade over the catalog service.
type CatalogManager struct {
	svc      *catalog.Service
	recorder AuditRecorder
	// validateURL is the SSRF/egress policy applied to deployment base URLs
	// on write (设计 §5). Nil rejects every non-empty base URL (fail closed).
	validateURL func(ctx context.Context, rawURL string) error
}

// NewCatalogManager builds the manager over a catalog service. recorder may
// be nil (writes then skip the audit trail — unit tests only; production
// always wires one). validateURL may be nil to disable egress checks in
// tests that don't exercise deployment URLs.
func NewCatalogManager(svc *catalog.Service, recorder AuditRecorder, validateURL func(context.Context, string) error) *CatalogManager {
	return &CatalogManager{svc: svc, recorder: recorder, validateURL: validateURL}
}

// Service exposes the underlying catalog service (read paths that do not
// need operator attribution, e.g. the published-model listing).
func (m *CatalogManager) Service() *catalog.Service { return m.svc }

// ParseActor splits the combined actor attribution "user:<uid>@app:<appid>"
// produced by the admin authorization middleware into its user and service
// parts. A bare string lands entirely in ActorUser.
func ParseActor(actor string) (userID, appID string) {
	userID, appID = actor, ""
	if before, after, ok := strings.Cut(actor, "@app:"); ok {
		userID, appID = strings.TrimPrefix(before, "user:"), after
	}
	return userID, appID
}

// firstReason 取操作员理由。写方法的 reason 是可变参数位——httpapi 层已
// 透传（body reason 字段或 ?reason=），未传时为空串；传入时写入审计事件
// （审查修复 Important-4：operator Reason 此前从未被采集）。
func firstReason(reason []string) string {
	if len(reason) > 0 {
		return reason[0]
	}
	return ""
}

// record writes one audit event for a catalog mutation, attributing both
// the operator user and the verifying service identity.
//
// 变更已先于审计提交（catalog.Service 无同事务审计面），因此审计写失败
// 不对操作员谎报失败：审计缺失由 AuditAlertHook 响亮告警（默认
// AUDIT_ALERT 结构化 ERROR 日志，生产应接 on-call 通道——安全审查
// I-7），操作本身照常返回——Publish/Rollback 返回错误会诱导重试产生重复
// 修订（审查修复 Important-3）。这一契约是刻意的：变更已生效是事实，
// 审计缺失是需要人工补录的告警，不是可回滚的状态；「需人工补录」语义封装
// 在 cause 里，告警日志陈述始终属实（I-6 复用同一通道上报生命周期命中，
// 不得谎报 AUDIT_WRITE_FAILED）。
func (m *CatalogManager) record(ctx context.Context, actor, action, objectType, objectID, reason string, detail map[string]any) {
	if m.recorder == nil {
		return
	}
	userID, appID := ParseActor(actor)
	ev := AuditEvent{
		Action: action, ObjectType: objectType, ObjectID: objectID, Reason: reason,
		ActorUser: userID, ActorApp: appID, Detail: SanitizeDetail(detail),
	}
	if err := m.recorder.Record(ctx, ev); err != nil {
		AuditAlertHook(ctx, ev, fmt.Errorf("变更已生效但审计缺失，需人工补录: %w", err))
	}
}

// validateDeploymentURL applies the egress policy to a deployment base URL.
// Empty base URLs (draft deployments) are allowed; any non-empty URL must
// pass the policy.
func (m *CatalogManager) validateDeploymentURL(ctx context.Context, baseURL string) error {
	if baseURL == "" {
		return nil
	}
	if m.validateURL == nil {
		return domain.NewError(domain.CodeInvalidInput, "egress validation not configured")
	}
	if err := m.validateURL(ctx, baseURL); err != nil {
		return domain.NewError(domain.CodeInvalidInput, "deployment base_url rejected by egress policy: "+err.Error())
	}
	return nil
}

// --- models ---

func (m *CatalogManager) CreateModel(ctx context.Context, actor string, mod *domain.Model, reason ...string) error {
	if err := m.svc.CreateModel(ctx, mod); err != nil {
		return err
	}
	m.record(ctx, actor, "model.create", "model", mod.ID, firstReason(reason), map[string]any{"display_name": mod.DisplayName})
	return nil
}

func (m *CatalogManager) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	return m.svc.GetModel(ctx, id)
}

func (m *CatalogManager) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	return m.svc.ListModels(ctx, filter)
}

func (m *CatalogManager) UpdateModel(ctx context.Context, actor string, mod *domain.Model, reason ...string) error {
	if err := m.svc.UpdateModel(ctx, mod); err != nil {
		return err
	}
	m.record(ctx, actor, "model.update", "model", mod.ID, firstReason(reason), nil)
	return nil
}

func (m *CatalogManager) SetModelLifecycle(ctx context.Context, actor, id string, to domain.Lifecycle, reason ...string) error {
	if err := m.svc.SetModelLifecycle(ctx, id, to); err != nil {
		return err
	}
	m.record(ctx, actor, "model.lifecycle", "model", id, firstReason(reason), map[string]any{"lifecycle": string(to)})
	return nil
}

func (m *CatalogManager) DeleteModel(ctx context.Context, actor, id string, reason ...string) error {
	if err := m.svc.DeleteModel(ctx, id); err != nil {
		return err
	}
	m.record(ctx, actor, "model.delete", "model", id, firstReason(reason), nil)
	return nil
}

// --- providers ---

func (m *CatalogManager) CreateProvider(ctx context.Context, actor string, p *domain.Provider, reason ...string) error {
	if err := m.svc.CreateProvider(ctx, p); err != nil {
		return err
	}
	m.record(ctx, actor, "provider.create", "provider", p.ID, firstReason(reason), map[string]any{"code": p.Code})
	return nil
}

func (m *CatalogManager) GetProvider(ctx context.Context, id string) (*domain.Provider, error) {
	return m.svc.GetProvider(ctx, id)
}

func (m *CatalogManager) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	return m.svc.ListProviders(ctx, afterID, limit)
}

func (m *CatalogManager) UpdateProvider(ctx context.Context, actor string, p *domain.Provider, reason ...string) error {
	if err := m.svc.UpdateProvider(ctx, p); err != nil {
		return err
	}
	m.record(ctx, actor, "provider.update", "provider", p.ID, firstReason(reason), nil)
	return nil
}

func (m *CatalogManager) DeleteProvider(ctx context.Context, actor, id string, reason ...string) error {
	if err := m.svc.DeleteProvider(ctx, id); err != nil {
		return err
	}
	m.record(ctx, actor, "provider.delete", "provider", id, firstReason(reason), nil)
	return nil
}

// --- deployments ---

func (m *CatalogManager) CreateDeployment(ctx context.Context, actor string, d *domain.Deployment, reason ...string) error {
	if err := m.validateDeploymentURL(ctx, d.BaseURL); err != nil {
		return err
	}
	if err := m.svc.CreateDeployment(ctx, d); err != nil {
		return err
	}
	m.record(ctx, actor, "deployment.create", "deployment", d.ID, firstReason(reason), map[string]any{"provider_id": d.ProviderID})
	return nil
}

func (m *CatalogManager) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	return m.svc.GetDeployment(ctx, id)
}

func (m *CatalogManager) ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	return m.svc.ListDeployments(ctx, f)
}

func (m *CatalogManager) UpdateDeployment(ctx context.Context, actor string, d *domain.Deployment, reason ...string) error {
	if err := m.validateDeploymentURL(ctx, d.BaseURL); err != nil {
		return err
	}
	if err := m.svc.UpdateDeployment(ctx, d); err != nil {
		return err
	}
	m.record(ctx, actor, "deployment.update", "deployment", d.ID, firstReason(reason), nil)
	return nil
}

func (m *CatalogManager) DeleteDeployment(ctx context.Context, actor, id string, reason ...string) error {
	if err := m.svc.DeleteDeployment(ctx, id); err != nil {
		return err
	}
	m.record(ctx, actor, "deployment.delete", "deployment", id, firstReason(reason), nil)
	return nil
}

// --- routes ---

func (m *CatalogManager) CreateRoute(ctx context.Context, actor string, r *domain.ModelRoute, reason ...string) error {
	if err := m.svc.CreateRoute(ctx, r); err != nil {
		return err
	}
	m.record(ctx, actor, "route.create", "route", r.ID, firstReason(reason), map[string]any{"model_id": r.ModelID, "deployment_id": r.DeploymentID})
	return nil
}

func (m *CatalogManager) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return m.svc.ListRoutes(ctx, modelID)
}

// GetRoute reads one route (httpapi 更新路径先读后写：校验/乐观锁需要完整
// 对象——Task 16 实测 PATCH /admin/routes/:id 因缺 model_id/deployment_id
// 永远 400 的修复).
func (m *CatalogManager) GetRoute(ctx context.Context, id string) (*domain.ModelRoute, error) {
	return m.svc.GetRoute(ctx, id)
}

func (m *CatalogManager) UpdateRoute(ctx context.Context, actor string, r *domain.ModelRoute, reason ...string) error {
	if err := m.svc.UpdateRoute(ctx, r); err != nil {
		return err
	}
	m.record(ctx, actor, "route.update", "route", r.ID, firstReason(reason), nil)
	return nil
}

func (m *CatalogManager) DeleteRoute(ctx context.Context, actor, id string, reason ...string) error {
	if err := m.svc.DeleteRoute(ctx, id); err != nil {
		return err
	}
	m.record(ctx, actor, "route.delete", "route", id, firstReason(reason), nil)
	return nil
}

// --- publish / rollback / history ---

// Publish validates the draft catalog and atomically activates a new
// revision, returning the new revision number.
func (m *CatalogManager) Publish(ctx context.Context, actor string, reason ...string) (int, error) {
	rev, err := m.svc.Publish(ctx, actor)
	if err != nil {
		return 0, err
	}
	m.record(ctx, actor, "catalog.publish", "catalog", "", firstReason(reason), map[string]any{"revision": rev})
	return rev, nil
}

// providerStatusActive is the only provider status that may carry traffic
// under a published catalog (T3's shared published-state rule). An empty
// status means "not declared" and is treated as active — the DB column is
// NOT NULL DEFAULT 'active'.
const providerStatusActive = "active"

// PublishDryRunReport 是 publish ?dry_run=1 的 advisory 报告（R7-N7）：
// 发布候选集摘要 + 每个将生效（active）deployment 的上游探测结论。本路径
// 零写库、零审计事件；探测失败只在报告内如实标注，绝不返回错误（运营判断
// 是否继续正式发布；正式 publish 路径不跑探测、不受探测结果影响）。
//
// T9：报告追加了**可判定问题**清单（issues）与判定摘要（summary）。HTTP
// 200 只说明「预演报告生成成功」——**不等于**每个 probe.ok 都是 true；
// 逐条结论在 probes[] 与 summary 里。
type PublishDryRunReport struct {
	WouldPublish PublishPreviewSummary `json:"would_publish"`
	Probes       []ProbeResult         `json:"probes"`
	// Issues 汇总发布前可判定的问题：配置问题（blocker）与网络探测提示
	// （hint）分开标注。T3 携带的必修发现：停用 provider 背后的部署**不
	// 算** active、也**不**被探测。
	Issues []PublishIssue `json:"issues"`
	// Summary 是判定摘要（探测结论分桶 + blocker/hint 计数）。
	Summary PublishDryRunSummary `json:"summary"`
}

// PublishIssueSeverity 区分两类问题。
const (
	// PublishIssueBlocker 是**确定性配置问题**：发布这份目录后，某个模型
	// 或路由跑起来不是配的那个（停用 provider、无可用账号、路由能力收窄、
	// 模型无可调度路由）。运营要么修好再发，要么明确接受。
	PublishIssueBlocker = "blocker"
	// PublishIssueHint 是**网络探测提示**：advisory，永远不阻断发布，也
	// **不代表**模型故障（供应商不提供 /models 时同样只是一条提示）。
	PublishIssueHint = "hint"
)

// PublishIssue 是发布预演里的一条可判定问题。Message 是给运营看的人话，
// 绝不含凭据材料。
type PublishIssue struct {
	// Code 是问题的稳定符号名（客户端按 code 匹配，不解析人话）。
	Code string `json:"code"`
	// Severity 是 blocker（配置问题）或 hint（网络探测提示）。
	Severity string `json:"severity"`
	// Subject 是问题对象的主键（provider / deployment / model / route ID）。
	Subject string `json:"subject"`
	// Message 是运营可读的一句话。
	Message string `json:"message"`
}

// PublishPreviewSummary 是发布候选集的内容摘要（计数 + next revision 预告）。
type PublishPreviewSummary struct {
	NextRevision      int `json:"next_revision"`
	Models            int `json:"models"`
	Providers         int `json:"providers"`
	Deployments       int `json:"deployments"`
	ActiveDeployments int `json:"active_deployments"`
	Routes            int `json:"routes"`
	// DisabledProviders 是草稿里被停用的 provider 数量（停用 = 该 provider
	// 及其全部部署退出轮转，T3 的发布态语义）。
	DisabledProviders int `json:"disabled_providers"`
	// DeploymentsBehindDisabledProviders 是「部署本身 active、但其 provider
	// 已停用」的部署数：它们**不会**承接流量，也**不**被探测。
	DeploymentsBehindDisabledProviders int `json:"deployments_behind_disabled_providers"`
}

// PublishDryRunSummary 是预演的判定摘要：HTTP 200 不等于每个探测都成功。
type PublishDryRunSummary struct {
	// ProbesTotal / ProbesOK / ProbesFailed / ProbesUnsupported /
	// ProbesUnconfirmed 把探测结论分桶（unsupported = 供应商不提供该探测
	// 端点；unconfirmed = 没得到答复）。
	ProbesTotal       int `json:"probes_total"`
	ProbesOK          int `json:"probes_ok"`
	ProbesFailed      int `json:"probes_failed"`
	ProbesUnsupported int `json:"probes_unsupported"`
	ProbesUnconfirmed int `json:"probes_unconfirmed"`
	// Blockers 是确定性配置问题的条数；Hints 是网络探测提示的条数。
	Blockers int `json:"blockers"`
	Hints    int `json:"hints"`
	// AllProbesOK 只有在每个探测都 verified 时才为 true。
	AllProbesOK bool `json:"all_probes_ok"`
	// AccountInventoryChecked 报告「无账号」判定是否真的查过账号清单。
	// false = 清单读面不可用，该判定被**跳过**（报告说未知，不是无账号）。
	AccountInventoryChecked bool `json:"account_inventory_checked"`
}

// PublishDryRun validates the draft catalog exactly like Publish（校验失败原样
// 返回，与正式发布同一错误）并探测每个 active deployment，WITHOUT publishing。
// 草稿/停用部署不上线、不探测（避免半编辑状态噪声）；**停用 provider 背后
// 的部署同样不上线、不探测**（T3 携带的必修发现：预演曾把它们当成「会生效」
// 并真的出站探测）。探测器自身的硬错误也折叠为 in-band 失败条目，绝不阻断
// 报告。
//
// 报告同时汇总发布前**可判定**的问题（见 PublishIssue）：停用 provider、
// 无可用账号、路由能力收窄、模型无可调度路由（blocker），以及探测结论
// （hint）。策略/路由**不支持**的配置由发布闸本身拒绝（与正式发布同一错误），
// 因此它出现在错误里而不是 200 报告里。
func (m *CatalogManager) PublishDryRun(ctx context.Context, prober DeploymentProber) (*PublishDryRunReport, error) {
	if prober == nil {
		return nil, domain.NewError(domain.CodeInternal, "probe service not configured")
	}
	prev, err := m.svc.PublishPreview(ctx)
	if err != nil {
		return nil, err
	}
	report := &PublishDryRunReport{
		WouldPublish: PublishPreviewSummary{
			NextRevision: prev.NextRevision,
			Models:       len(prev.Models),
			Providers:    len(prev.Providers),
			Deployments:  len(prev.Deployments),
			Routes:       len(prev.Routes),
		},
		Probes: []ProbeResult{},
		Issues: []PublishIssue{},
	}

	// provider 的发布态：停用的 provider 及其全部部署退出轮转（T3）。
	providerActive := make(map[string]bool, len(prev.Providers))
	for i := range prev.Providers {
		p := &prev.Providers[i]
		active := p.Status == "" || p.Status == providerStatusActive
		providerActive[p.ID] = active
		if !active {
			report.WouldPublish.DisabledProviders++
			report.Issues = append(report.Issues, PublishIssue{
				Code: "provider_disabled", Severity: PublishIssueBlocker, Subject: p.ID,
				Message: "provider " + p.Code + " is disabled: its deployments will not carry traffic and are not probed",
			})
		}
	}

	// 账号清单：provider 没有可用上游账号 = 这条部署无法出站（可判定）。
	accountless := map[string]bool{}
	if inv, ok := prober.(AccountInventory); ok {
		report.Summary.AccountInventoryChecked = true
		checked := map[string]bool{}
		for i := range prev.Deployments {
			d := &prev.Deployments[i]
			if d.Status != domain.DeploymentActive || !providerActive[d.ProviderID] || checked[d.ProviderID] {
				continue
			}
			checked[d.ProviderID] = true
			accts, aerr := inv.ListActiveUpstreamAccounts(ctx, d.ProviderID)
			if aerr != nil {
				report.Issues = append(report.Issues, PublishIssue{
					Code: "account_inventory_unavailable", Severity: PublishIssueHint, Subject: d.ProviderID,
					Message: "the upstream account inventory of provider " + d.ProviderID + " could not be read — the account check was skipped (unknown, not empty)",
				})
				continue
			}
			accountless[d.ProviderID] = len(accts) == 0
		}
	}

	for i := range prev.Deployments {
		d := &prev.Deployments[i]
		if d.Status != domain.DeploymentActive {
			continue
		}
		if !providerActive[d.ProviderID] {
			// 停用 provider 背后的部署：不会承接流量 → 不算 active、不探测。
			report.WouldPublish.DeploymentsBehindDisabledProviders++
			continue
		}
		report.WouldPublish.ActiveDeployments++
		if accountless[d.ProviderID] {
			report.Issues = append(report.Issues, PublishIssue{
				Code: "deployment_no_account", Severity: PublishIssueBlocker, Subject: d.ID,
				Message: "deployment " + d.ID + " has no usable upstream account — this route cannot dispatch",
			})
		}
		res, perr := prober.ProbeDeployment(ctx, d.ID)
		if perr != nil {
			// 探测器自身失败（如部署在抽取后被并发删除）同样不阻断。
			res = &ProbeResult{
				DeploymentID:    d.ID,
				CredentialState: ProbeCredentialUnusable,
				ProbeMethod:     ProbeMethodNone,
				ProbeOutcome:    ProbeOutcomeUnconfirmed,
				ErrorSummary:    safeProbeErrorSummary("probe failed", perr),
			}
		}
		report.Probes = append(report.Probes, *res)
		switch res.ProbeOutcome {
		case ProbeOutcomeVerified:
			report.Summary.ProbesOK++
		case ProbeOutcomeFailed:
			report.Summary.ProbesFailed++
			report.Issues = append(report.Issues, PublishIssue{
				Code: "probe_failed", Severity: PublishIssueHint, Subject: d.ID,
				Message: "the connectivity probe of deployment " + d.ID + " did not succeed: " + probeIssueDetail(res),
			})
		case ProbeOutcomeUnsupported:
			report.Summary.ProbesUnsupported++
			report.Issues = append(report.Issues, PublishIssue{
				Code: "probe_unsupported", Severity: PublishIssueHint, Subject: d.ID,
				Message: "the vendor does not expose a model-list endpoint for deployment " + d.ID +
					": this probe method cannot confirm connectivity — it is NOT evidence of an unhealthy model (a base_url missing a path segment yields the same 404)",
			})
		default:
			report.Summary.ProbesUnconfirmed++
			report.Issues = append(report.Issues, PublishIssue{
				Code: "probe_unconfirmed", Severity: PublishIssueHint, Subject: d.ID,
				Message: "the connectivity probe of deployment " + d.ID + " could not confirm anything: " + probeIssueDetail(res),
			})
		}
	}
	report.Summary.ProbesTotal = len(report.Probes)
	report.Summary.AllProbesOK = report.Summary.ProbesTotal == report.Summary.ProbesOK

	m.collectCatalogIssues(prev, providerActive, report)
	for _, is := range report.Issues {
		if is.Severity == PublishIssueBlocker {
			report.Summary.Blockers++
		} else {
			report.Summary.Hints++
		}
	}
	return report, nil
}

// collectCatalogIssues 汇总草稿目录里**可判定**的配置问题：模型有没有可
// 调度路由、路由的能力清单有没有把模型声明的能力收窄掉。
//
// 「策略/路由不支持」（不支持的 pool_strategy、词表外的能力名）由**发布闸**
// 拒绝（与正式发布同一错误），因此它出现在错误里而不是这份 200 报告里 ——
// 预演与正式发布的判定口径必须一致。
func (m *CatalogManager) collectCatalogIssues(prev *catalog.PublishPreview, providerActive map[string]bool, report *PublishDryRunReport) {
	deployments := make(map[string]domain.Deployment, len(prev.Deployments))
	for _, d := range prev.Deployments {
		deployments[d.ID] = d
	}
	routesByModel := map[string][]domain.ModelRoute{}
	for _, r := range prev.Routes {
		routesByModel[r.ModelID] = append(routesByModel[r.ModelID], r)
	}
	for i := range prev.Models {
		model := &prev.Models[i]
		dispatchable := false
		for _, r := range routesByModel[model.ID] {
			if !r.Enabled || !catalog.PoolStrategySupported(r.PoolStrategy) {
				continue
			}
			d, ok := deployments[r.DeploymentID]
			if !ok || d.Status != domain.DeploymentActive || !providerActive[d.ProviderID] {
				continue
			}
			dispatchable = true
			// 路由的能力清单收窄了模型声明的图片能力：带图请求会在准入前
			// 被明确拒绝（零预占、零出站）—— 运营必须知道。
			if model.AcceptsImages() && !routeCarriesImage(r) {
				report.Issues = append(report.Issues, PublishIssue{
					Code: "route_capabilities_narrowed", Severity: PublishIssueBlocker, Subject: r.ID,
					Message: "route " + r.ID + " declares capabilities [" + strings.Join(r.Capabilities, ", ") +
						"] without \"image\": image-bearing requests for model " + model.ID +
						" are refused before dispatch on this route",
				})
			}
		}
		if model.Lifecycle == domain.LifecycleActive && !dispatchable {
			report.Issues = append(report.Issues, PublishIssue{
				Code: "model_not_dispatchable", Severity: PublishIssueBlocker, Subject: model.ID,
				Message: "model " + model.ID + " has no dispatchable route in this revision — it will not be servable after publishing",
			})
		}
	}
}

// routeCarriesImage reports whether the route's capability list covers image
// input. An EMPTY capability list means "everything the model supports" —
// it is not a narrowing.
func routeCarriesImage(r domain.ModelRoute) bool {
	if len(r.Capabilities) == 0 {
		return true
	}
	for _, c := range r.Capabilities {
		if c == domain.RouteCapabilityImage {
			return true
		}
	}
	return false
}

// probeIssueDetail renders the operator-facing detail of a probe that did
// not verify: the probe's own error summary when it has one, else its
// description. Never any credential material (the probe summary is already
// redacted).
func probeIssueDetail(res *ProbeResult) string {
	if res == nil {
		return "no detail"
	}
	if s := strings.TrimSpace(res.ErrorSummary); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Description); s != "" {
		return s
	}
	return "no detail"
}

// Rollback publishes a new revision carrying the content of toRevision;
// history is never rewritten.
func (m *CatalogManager) Rollback(ctx context.Context, actor string, toRevision int, reason ...string) (int, error) {
	rev, err := m.svc.Rollback(ctx, toRevision, actor)
	if err != nil {
		return 0, err
	}
	m.record(ctx, actor, "catalog.rollback", "catalog", "", firstReason(reason), map[string]any{"to_revision": toRevision, "revision": rev})
	return rev, nil
}

// ListRevisionMetas returns the catalog revision history (metadata only,
// payload blob never loaded), newest first, keyset-paginated by revision
// number: afterRevision is the cursor (0 = first page).
func (m *CatalogManager) ListRevisionMetas(ctx context.Context, afterRevision, limit int) ([]domain.RevisionMeta, error) {
	return m.svc.ListRevisionMetas(ctx, afterRevision, limit)
}

// ActiveRevisionMeta returns the active catalog revision's metadata.
func (m *CatalogManager) ActiveRevisionMeta(ctx context.Context) (*domain.RevisionMeta, error) {
	return m.svc.ActiveRevisionMeta(ctx)
}

// GetRevision returns one immutable revision.
func (m *CatalogManager) GetRevision(ctx context.Context, revision int) (*domain.ConfigRevision, error) {
	return m.svc.Store().GetRevision(ctx, domain.ScopeCatalog, revision)
}
