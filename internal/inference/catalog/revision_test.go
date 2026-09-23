package catalog_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

func sampleModel(id string) domain.Model {
	return domain.Model{
		ID: id, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleDraft,
		ModelVersion: "4.6", Aliases: []string{"latest"},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}
}

func sampleDeployment() domain.Deployment {
	return domain.Deployment{
		ID: "dep-1", ProviderID: "prov-1",
		UpstreamModel: "glm-4.6", BaseURL: "https://api.example.com/v1",
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 5 * time.Second, RequestTimeout: 600 * time.Second,
		ConfigVersion: 1, Status: domain.DeploymentActive,
	}
}

func sampleRoute() domain.ModelRoute {
	return domain.ModelRoute{
		ID: "route-1", ModelID: "glm-4.6", DeploymentID: "dep-1",
		Priority: 0, Weight: 1, Enabled: true,
		PoolStrategy: domain.PoolRoundRobin,
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	m := sampleModel("glm-4.6")
	m.Lifecycle = domain.LifecycleActive
	prov := domain.Provider{ID: "prov-1", Code: "example", DisplayName: "Example", AccessType: domain.AccessOfficialAPI, Status: "active"}
	dep := sampleDeployment()
	route := sampleRoute()

	raw := catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})
	rev := &domain.ConfigRevision{ID: 7, Revision: 3, Payload: domain.ExtensionConfig{Raw: raw}, PublishedAt: nil}
	snap, err := catalog.ParseSnapshot(rev)
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if snap.RevisionID != 7 || snap.Revision != 3 {
		t.Errorf("snapshot revision = %d/%d, want 7/3", snap.RevisionID, snap.Revision)
	}
	got, ok := snap.Model("glm-4.6")
	if !ok || got.DisplayName != "GLM 4.6" || got.ContextTokens != 200000 {
		t.Errorf("model roundtrip mismatch: %+v", got)
	}
	// Alias resolution: 'latest' resolves to the same stable entry.
	byAlias, ok := snap.Model("latest")
	if !ok || byAlias.ID != "glm-4.6" {
		t.Errorf("alias resolution failed: %+v", byAlias)
	}
	if _, ok := snap.Model("nope"); ok {
		t.Error("unknown id resolved to a model")
	}
	deps := snap.ActiveDeployments("glm-4.6")
	if len(deps) != 1 || deps[0].UpstreamModel != "glm-4.6" {
		t.Errorf("ActiveDeployments = %+v", deps)
	}
	if !snap.Sellable("glm-4.6") {
		t.Error("active model with active deployment must be sellable")
	}
}

func TestSellableRequiresLifecycleAndActiveDeployment(t *testing.T) {
	m := sampleModel("glm-4.6")
	prov := domain.Provider{ID: "prov-1", Code: "example", DisplayName: "Example", AccessType: domain.AccessOfficialAPI}
	dep := sampleDeployment()
	route := sampleRoute()
	raw := catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})

	parse := func() *catalog.Snapshot {
		snap, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: raw}})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return snap
	}

	// Draft lifecycle → not sellable.
	draft := m
	raw = catalog.BuildCatalogPayload([]domain.Model{draft}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})
	if parse().Sellable("glm-4.6") {
		t.Error("draft model must not be sellable")
	}

	// Disabled deployment → not sellable (model itself active).
	m.Lifecycle = domain.LifecycleActive
	disabled := dep
	disabled.Status = domain.DeploymentDisabled
	raw = catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{disabled}, []domain.ModelRoute{route})
	if parse().Sellable("glm-4.6") {
		t.Error("model routed only to a disabled deployment must not be sellable")
	}

	// No routes at all → not sellable.
	raw = catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, nil)
	if parse().Sellable("glm-4.6") {
		t.Error("model without routes must not be sellable")
	}
}

func TestParseRejectsHalfVersions(t *testing.T) {
	m := sampleModel("glm-4.6")
	prov := domain.Provider{ID: "prov-1", Code: "example", DisplayName: "Example", AccessType: domain.AccessOfficialAPI}
	dep := sampleDeployment()
	route := sampleRoute()
	good := catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})

	// Unknown top-level field.
	var p map[string]any
	if err := json.Unmarshal(good, &p); err != nil {
		t.Fatal(err)
	}
	p["surprise"] = true
	bad, _ := json.Marshal(p)
	if _, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: bad}}); err == nil {
		t.Error("unknown field: want error")
	}

	// Wrong schema version.
	p = map[string]any{}
	_ = json.Unmarshal(good, &p)
	p["schema_version"] = 2
	bad, _ = json.Marshal(p)
	if _, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: bad}}); err == nil {
		t.Error("wrong schema_version: want error")
	}

	// Broken reference: route points at a missing deployment.
	var payload struct {
		SchemaVersion int              `json:"schema_version"`
		Models        []any            `json:"models"`
		Providers     []any            `json:"providers"`
		Deployments   []any            `json:"deployments"`
		Routes        []map[string]any `json:"routes"`
	}
	if err := json.Unmarshal(good, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Routes[0]["deployment_id"] = "00000000-0000-0000-0000-000000000000"
	bad, _ = json.Marshal(payload)
	if _, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: bad}}); err == nil {
		t.Error("dangling deployment reference: want error")
	}

	// Invalid entity inside the payload (max_output_tokens = 0).
	var payload2 struct {
		SchemaVersion int              `json:"schema_version"`
		Models        []map[string]any `json:"models"`
		Providers     []any            `json:"providers"`
		Deployments   []any            `json:"deployments"`
		Routes        []any            `json:"routes"`
	}
	if err := json.Unmarshal(good, &payload2); err != nil {
		t.Fatal(err)
	}
	payload2.Models[0]["max_output_tokens"] = 0
	bad, _ = json.Marshal(payload2)
	if _, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: bad}}); err == nil {
		t.Error("invalid entity: want error")
	}

	// Truncated garbage.
	if _, err := catalog.ParseSnapshot(&domain.ConfigRevision{Payload: domain.ExtensionConfig{Raw: json.RawMessage(`{"schema_version":1,`)}}); err == nil {
		t.Error("truncated payload: want error")
	}
}

func TestBuildPayloadDeterministic(t *testing.T) {
	m := sampleModel("glm-4.6")
	prov := domain.Provider{ID: "prov-1", Code: "example", DisplayName: "Example", AccessType: domain.AccessOfficialAPI}
	dep := sampleDeployment()
	route := sampleRoute()
	a := catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})
	b := catalog.BuildCatalogPayload([]domain.Model{m}, []domain.Provider{prov}, []domain.Deployment{dep}, []domain.ModelRoute{route})
	if string(a) != string(b) {
		t.Error("equal catalogs must serialize byte-identically")
	}
}
