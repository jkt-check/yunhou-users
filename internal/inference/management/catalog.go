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
	"log"
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

// firstReason 取操作员理由。写方法的 reason 是可变参数位——httpapi 层尚未
// 透传时为空串（参数位保留给 httpapi 接线），调用方传入时写入审计事件
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
// 不对操作员谎报失败：记 ERROR 日志并照常返回——Publish/Rollback 返回错误
// 会诱导重试产生重复修订（审查修复 Important-3）。审计缺失由 ERROR 日志
// 告警，需人工补录。
func (m *CatalogManager) record(ctx context.Context, actor, action, objectType, objectID, reason string, detail map[string]any) {
	if m.recorder == nil {
		return
	}
	userID, appID := ParseActor(actor)
	if err := m.recorder.Record(ctx, AuditEvent{
		Action: action, ObjectType: objectType, ObjectID: objectID, Reason: reason,
		ActorUser: userID, ActorApp: appID, Detail: SanitizeDetail(detail),
	}); err != nil {
		log.Printf("ERROR catalog audit write failed action=%s object=%s/%s actor=%s: %v (变更已生效，审计缺失需人工补录)",
			action, objectType, objectID, actor, err)
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

// ListRevisions returns the catalog revision history, newest first.
func (m *CatalogManager) ListRevisions(ctx context.Context) ([]domain.ConfigRevision, error) {
	return m.svc.ListRevisions(ctx)
}

// GetRevision returns one immutable revision.
func (m *CatalogManager) GetRevision(ctx context.Context, revision int) (*domain.ConfigRevision, error) {
	return m.svc.Store().GetRevision(ctx, domain.ScopeCatalog, revision)
}
