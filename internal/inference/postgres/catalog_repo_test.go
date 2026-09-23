package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// repoCatalogChain seeds provider + model + deployment + route directly
// through the store (below the catalog service layer).
func repoCatalogChain(t *testing.T, s *Store) (modelID, deploymentID string) {
	t.Helper()
	ctx := context.Background()
	prov := &domain.Provider{Code: "repo-prov", DisplayName: "P", AccessType: domain.AccessOfficialAPI}
	if err := s.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	modelID = "repo-model"
	if err := s.InsertModel(ctx, &domain.Model{
		ID: modelID, DisplayName: "M", ContextTokens: 100, MaxOutputTokens: 10,
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "up", BaseURL: "https://x.example.com",
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 5000000000, RequestTimeout: 600000000000, // 5s / 600s in ns
	}
	if err := s.InsertDeployment(ctx, dep); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	if err := s.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("insert route: %v", err)
	}
	return modelID, dep.ID
}

func TestRepoModelOptimisticUpdateAndDelete(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	modelID, _ := repoCatalogChain(t, s)

	m, err := s.GetModel(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *m
	m.DisplayName = "Updated"
	if err := s.UpdateModel(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !m.UpdatedAt.After(stale.UpdatedAt) {
		t.Errorf("updated_at not bumped: %s → %s", stale.UpdatedAt, m.UpdatedAt)
	}

	// Stale token → conflict; unknown id → not found.
	stale.DisplayName = "Lost"
	if err := s.UpdateModel(ctx, &stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale update: got %v, want conflict", err)
	}
	ghost := *m
	ghost.ID = "ghost-model"
	if err := s.UpdateModel(ctx, &ghost); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("ghost update: got %v, want not_found", err)
	}

	// Delete cascades the routes.
	if err := s.DeleteModel(ctx, modelID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	routes, err := s.ListRoutes(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Errorf("routes survived model delete: %+v", routes)
	}
	if err := s.DeleteModel(ctx, modelID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("second delete: got %v, want not_found", err)
	}
}

func TestRepoProviderGuards(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	_, deploymentID := repoCatalogChain(t, s)

	prov, err := s.GetProviderByCode(ctx, "repo-prov")
	if err != nil {
		t.Fatal(err)
	}

	// Delete blocked while deployments reference the provider.
	if err := s.DeleteProvider(ctx, prov.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("delete referenced provider: got %v, want conflict", err)
	}

	// Optimistic conflict on update.
	stale := *prov
	prov.DisplayName = "Renamed"
	if err := s.UpdateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	stale.DisplayName = "Lost"
	if err := s.UpdateProvider(ctx, &stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale provider update: got %v, want conflict", err)
	}

	// Remove the deployment, then the provider deletes cleanly.
	if err := s.DeleteRoute(ctx, mustRouteID(t, s, "repo-model", deploymentID)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDeployment(ctx, deploymentID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, prov.ID); err != nil {
		t.Fatalf("delete unreferenced provider: %v", err)
	}
}

func mustRouteID(t *testing.T, s *Store, modelID, deploymentID string) string {
	t.Helper()
	routes, err := s.ListRoutes(context.Background(), modelID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.DeploymentID == deploymentID {
			return r.ID
		}
	}
	t.Fatalf("no route %s → %s", modelID, deploymentID)
	return ""
}

func TestRepoDeploymentUpdateAndGuards(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	_, deploymentID := repoCatalogChain(t, s)

	dep, err := s.GetDeployment(ctx, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ConfigVersion != 1 {
		t.Fatalf("initial config_version = %d", dep.ConfigVersion)
	}
	dep.Region = "eu"
	if err := s.UpdateDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if dep.ConfigVersion != 2 {
		t.Errorf("config_version = %d, want 2", dep.ConfigVersion)
	}

	dep.ConfigVersion = 1 // stale
	dep.Region = "us"
	if err := s.UpdateDeployment(ctx, dep); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale deployment update: got %v, want conflict", err)
	}

	// Delete blocked while a route references the deployment.
	if err := s.DeleteDeployment(ctx, deploymentID); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("delete referenced deployment: got %v, want conflict", err)
	}
}

func TestRepoDeploymentFiltersAndPagination(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	modelID, _ := repoCatalogChain(t, s)

	prov2 := &domain.Provider{Code: "repo-prov-2", DisplayName: "P2", AccessType: domain.AccessSelfHosted}
	if err := s.InsertProvider(ctx, prov2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		dep := &domain.Deployment{
			ProviderID: prov2.ID, UpstreamModel: "up-" + uuid.NewString()[:8],
			BaseURL:        "https://h" + uuid.NewString()[:6] + ".example.com",
			Protocol:       domain.ProtocolOpenAIChat,
			ConnectTimeout: 5000000000, RequestTimeout: 600000000000,
		}
		if err := s.InsertDeployment(ctx, dep); err != nil {
			t.Fatal(err)
		}
	}

	// Status filter.
	active, err := s.ListDeployments(ctx, domain.DeploymentFilter{Status: domain.DeploymentDraft})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 4 { // seeded draft + 3 above
		t.Errorf("draft deployments = %d, want 4", len(active))
	}
	none, err := s.ListDeployments(ctx, domain.DeploymentFilter{Status: domain.DeploymentActive})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("active deployments = %d, want 0", len(none))
	}

	// Provider filter.
	byProvider, err := s.ListDeployments(ctx, domain.DeploymentFilter{ProviderID: prov2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byProvider) != 3 {
		t.Errorf("provider deployments = %d, want 3", len(byProvider))
	}

	// Keyset pagination over the provider's three.
	page1, err := s.ListDeployments(ctx, domain.DeploymentFilter{ProviderID: prov2.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 = %d rows", len(page1))
	}
	page2, err := s.ListDeployments(ctx, domain.DeploymentFilter{ProviderID: prov2.ID, AfterID: page1[1].ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Errorf("page2 = %d rows, want 1", len(page2))
	}
	_ = modelID
}

func TestRepoRevisionReads(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	repoCatalogChain(t, s)

	if n, err := s.LatestRevision(ctx, domain.ScopeCatalog); err != nil || n != 0 {
		t.Fatalf("latest on empty scope = %d, %v; want 0", n, err)
	}
	for _, rev := range []int{1, 2} {
		if err := s.InsertConfigRevision(ctx, &domain.ConfigRevision{
			Scope: domain.ScopeCatalog, Revision: rev, CreatedBy: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 1); err != nil {
		t.Fatal(err)
	}

	id, rev, err := s.ActiveRevisionHead(ctx, domain.ScopeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Errorf("head revision = %d, want 1", rev)
	}
	active, err := s.ActiveRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != id || !active.IsActive || active.Status != domain.RevisionPublished {
		t.Errorf("active = %+v", active)
	}

	revs, err := s.ListRevisions(ctx, domain.ScopeCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Revision != 2 {
		t.Errorf("list = %+v", revs)
	}

	if _, err := s.GetRevision(ctx, domain.ScopeCatalog, 99); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("get missing revision: got %v", err)
	}

	// Activation is atomic: rev 2 active, rev 1 superseded.
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 2); err != nil {
		t.Fatal(err)
	}
	one, err := s.GetRevision(ctx, domain.ScopeCatalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Status != domain.RevisionSuperseded || one.IsActive {
		t.Errorf("revision 1 after switch = %+v", one)
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 42); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("activate missing revision: got %v", err)
	}
}
