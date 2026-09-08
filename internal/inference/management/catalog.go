// Package management hosts the operator-facing use cases of the inference
// module. Each write carries an actor (the authenticated operator subject
// once Task 4 wires operator auth); today the actor is recorded as
// revision attribution (created_by) and handed to the catalog service.
// Task 4 adds the authorization gate and the audit trail without changing
// these signatures.
package management

import (
	"context"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// CatalogManager is the operator façade over the catalog service.
type CatalogManager struct {
	svc *catalog.Service
}

// NewCatalogManager builds the manager over a catalog service.
func NewCatalogManager(svc *catalog.Service) *CatalogManager {
	return &CatalogManager{svc: svc}
}

// Service exposes the underlying catalog service (read paths that do not
// need operator attribution, e.g. the published-model listing).
func (m *CatalogManager) Service() *catalog.Service { return m.svc }

// --- models ---

func (m *CatalogManager) CreateModel(ctx context.Context, actor string, mod *domain.Model) error {
	return m.svc.CreateModel(ctx, mod)
}

func (m *CatalogManager) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	return m.svc.GetModel(ctx, id)
}

func (m *CatalogManager) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	return m.svc.ListModels(ctx, filter)
}

func (m *CatalogManager) UpdateModel(ctx context.Context, actor string, mod *domain.Model) error {
	return m.svc.UpdateModel(ctx, mod)
}

func (m *CatalogManager) SetModelLifecycle(ctx context.Context, actor, id string, to domain.Lifecycle) error {
	return m.svc.SetModelLifecycle(ctx, id, to)
}

func (m *CatalogManager) DeleteModel(ctx context.Context, actor, id string) error {
	return m.svc.DeleteModel(ctx, id)
}

// --- providers ---

func (m *CatalogManager) CreateProvider(ctx context.Context, actor string, p *domain.Provider) error {
	return m.svc.CreateProvider(ctx, p)
}

func (m *CatalogManager) GetProvider(ctx context.Context, id string) (*domain.Provider, error) {
	return m.svc.GetProvider(ctx, id)
}

func (m *CatalogManager) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	return m.svc.ListProviders(ctx, afterID, limit)
}

func (m *CatalogManager) UpdateProvider(ctx context.Context, actor string, p *domain.Provider) error {
	return m.svc.UpdateProvider(ctx, p)
}

func (m *CatalogManager) DeleteProvider(ctx context.Context, actor, id string) error {
	return m.svc.DeleteProvider(ctx, id)
}

// --- deployments ---

func (m *CatalogManager) CreateDeployment(ctx context.Context, actor string, d *domain.Deployment) error {
	return m.svc.CreateDeployment(ctx, d)
}

func (m *CatalogManager) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	return m.svc.GetDeployment(ctx, id)
}

func (m *CatalogManager) ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	return m.svc.ListDeployments(ctx, f)
}

func (m *CatalogManager) UpdateDeployment(ctx context.Context, actor string, d *domain.Deployment) error {
	return m.svc.UpdateDeployment(ctx, d)
}

func (m *CatalogManager) DeleteDeployment(ctx context.Context, actor, id string) error {
	return m.svc.DeleteDeployment(ctx, id)
}

// --- routes ---

func (m *CatalogManager) CreateRoute(ctx context.Context, actor string, r *domain.ModelRoute) error {
	return m.svc.CreateRoute(ctx, r)
}

func (m *CatalogManager) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return m.svc.ListRoutes(ctx, modelID)
}

func (m *CatalogManager) UpdateRoute(ctx context.Context, actor string, r *domain.ModelRoute) error {
	return m.svc.UpdateRoute(ctx, r)
}

func (m *CatalogManager) DeleteRoute(ctx context.Context, actor, id string) error {
	return m.svc.DeleteRoute(ctx, id)
}

// --- publish / rollback / history ---

// Publish validates the draft catalog and atomically activates a new
// revision, returning the new revision number.
func (m *CatalogManager) Publish(ctx context.Context, actor string) (int, error) {
	return m.svc.Publish(ctx, actor)
}

// Rollback publishes a new revision carrying the content of toRevision;
// history is never rewritten.
func (m *CatalogManager) Rollback(ctx context.Context, actor string, toRevision int) (int, error) {
	return m.svc.Rollback(ctx, toRevision, actor)
}

// ListRevisions returns the catalog revision history, newest first.
func (m *CatalogManager) ListRevisions(ctx context.Context) ([]domain.ConfigRevision, error) {
	return m.svc.ListRevisions(ctx)
}

// GetRevision returns one immutable revision.
func (m *CatalogManager) GetRevision(ctx context.Context, revision int) (*domain.ConfigRevision, error) {
	return m.svc.Store().GetRevision(ctx, domain.ScopeCatalog, revision)
}
