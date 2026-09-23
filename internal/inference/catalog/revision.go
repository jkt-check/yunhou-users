// Package catalog owns the publishable model/deployment catalog: draft
// CRUD with optimistic locking, validation, atomic publish/rollback of
// immutable config revisions, the immutable runtime Snapshot, the
// refresh-fail-safe snapshot cache, and the explicit idempotent
// LLM_PROVIDERS_JSON compatibility import (设计 §5, 基线报告差距 1/2).
//
// The package depends only on internal/inference/domain. The postgres
// store satisfies the Store interface defined in service.go.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// catalogPayloadSchemaVersion is the only payload schema this binary can
// build or parse. A revision carrying any other version is rejected on
// load — the cache then keeps serving the last verified snapshot instead
// of half-understanding a newer format.
const catalogPayloadSchemaVersion = 1

// Snapshot is the immutable, fully-parsed content of one published catalog
// revision. A gateway request pins ONE Snapshot for its whole lifetime
// (设计 §5: 一次调用固定使用一个快照); the object is read-only after
// ParseSnapshot returns and is safe for concurrent use.
type Snapshot struct {
	// RevisionID/Revision identify the source revision; PublishedAt is nil
	// for a revision that was never published.
	RevisionID  int64
	Revision    int
	PublishedAt *time.Time

	Models      map[string]domain.Model
	Providers   map[string]domain.Provider
	Deployments map[string]domain.Deployment
	// RoutesByModel holds enabled routes ordered by priority/weight.
	RoutesByModel map[string][]domain.ModelRoute
}

// Model resolves a public model ID or alias to the model. Aliases ('latest'
// and model-specific ones) resolve to the same stable entry — the DB never
// stores one row per alias (migration 024 comment).
func (s *Snapshot) Model(idOrAlias string) (*domain.Model, bool) {
	if m, ok := s.Models[idOrAlias]; ok {
		return &m, true
	}
	for _, m := range s.Models {
		for _, a := range m.Aliases {
			if a == idOrAlias {
				mm := m
				return &mm, true
			}
		}
	}
	return nil, false
}

// ActiveDeployments returns the deployments serving a model through
// enabled routes, ordered by route priority then weight. Deployments whose
// status is not 'active' are skipped: an enabled route to a disabled
// upstream must not receive traffic.
func (s *Snapshot) ActiveDeployments(modelID string) []domain.Deployment {
	routes := s.RoutesByModel[modelID]
	out := make([]domain.Deployment, 0, len(routes))
	for _, r := range routes {
		if !r.Enabled {
			continue
		}
		d, ok := s.Deployments[r.DeploymentID]
		if !ok || d.Status != domain.DeploymentActive {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Sellable reports whether a model may appear in the published model list:
// lifecycle 'active' AND at least one enabled route to an 'active'
// deployment. Price and entitlement checks are layered on top by the
// caller via the service's price/access hooks (新模型未绑定售价/权限/有效
// 部署时不可售).
func (s *Snapshot) Sellable(modelID string) bool {
	m, ok := s.Models[modelID]
	if !ok || m.Lifecycle != domain.LifecycleActive {
		return false
	}
	return len(s.ActiveDeployments(modelID)) > 0
}

// ModelIDs returns the sorted model IDs — deterministic listing order.
func (s *Snapshot) ModelIDs() []string {
	ids := make([]string, 0, len(s.Models))
	for id := range s.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// --- payload build/parse (the immutable revision body) ---

type payloadModel struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Lifecycle         string   `json:"lifecycle"`
	ModelVersion      string   `json:"model_version"`
	Aliases           []string `json:"aliases"`
	InputModalities   []string `json:"input_modalities"`
	OutputModalities  []string `json:"output_modalities"`
	ContextTokens     int      `json:"context_tokens"`
	MaxOutputTokens   int      `json:"max_output_tokens"`
	Protocols         []string `json:"protocols"`
	SupportsTools     bool     `json:"supports_tools"`
	SupportsReasoning bool     `json:"supports_reasoning"`
}

type payloadProvider struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	AccessType  string `json:"access_type"`
	Status      string `json:"status"`
}

type payloadDeployment struct {
	ID               string          `json:"id"`
	ProviderID       string          `json:"provider_id"`
	UpstreamModel    string          `json:"upstream_model"`
	BaseURL          string          `json:"base_url"`
	Protocol         string          `json:"protocol"`
	Region           string          `json:"region"`
	ConnectTimeoutMs int64           `json:"connect_timeout_ms"`
	RequestTimeoutMs int64           `json:"request_timeout_ms"`
	ConfigVersion    int             `json:"config_version"`
	Config           json.RawMessage `json:"config"`
	Status           string          `json:"status"`
}

type payloadRoute struct {
	ID           string   `json:"id"`
	ModelID      string   `json:"model_id"`
	DeploymentID string   `json:"deployment_id"`
	Priority     int      `json:"priority"`
	Weight       int      `json:"weight"`
	Capabilities []string `json:"capabilities"`
	PoolStrategy string   `json:"pool_strategy"`
	Enabled      bool     `json:"enabled"`
}

// catalogPayload is the canonical on-disk shape of a catalog revision.
// BuildCatalogPayload sorts every slice by ID so equal catalogs always
// serialize byte-identically (content-hash friendly).
type catalogPayload struct {
	SchemaVersion int                 `json:"schema_version"`
	Models        []payloadModel      `json:"models"`
	Providers     []payloadProvider   `json:"providers"`
	Deployments   []payloadDeployment `json:"deployments"`
	Routes        []payloadRoute      `json:"routes"`
}

func strSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// BuildCatalogPayload assembles the canonical payload for a publish. All
// entities are required (nil slices are fine); ordering is deterministic.
func BuildCatalogPayload(models []domain.Model, providers []domain.Provider, deployments []domain.Deployment, routes []domain.ModelRoute) json.RawMessage {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID < deployments[j].ID })
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })

	p := catalogPayload{SchemaVersion: catalogPayloadSchemaVersion}
	for _, m := range models {
		protocols := make([]string, 0, len(m.Protocols))
		for _, proto := range m.Protocols {
			protocols = append(protocols, string(proto))
		}
		p.Models = append(p.Models, payloadModel{
			ID: m.ID, DisplayName: m.DisplayName, Lifecycle: string(m.Lifecycle),
			ModelVersion: m.ModelVersion, Aliases: strSlice(m.Aliases),
			InputModalities: strSlice(m.InputModalities), OutputModalities: strSlice(m.OutputModalities),
			ContextTokens: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens,
			Protocols: protocols, SupportsTools: m.SupportsTools,
			SupportsReasoning: m.SupportsReasoning,
		})
	}
	for _, pr := range providers {
		p.Providers = append(p.Providers, payloadProvider{
			ID: pr.ID, Code: pr.Code, DisplayName: pr.DisplayName,
			AccessType: string(pr.AccessType), Status: pr.Status,
		})
	}
	for _, d := range deployments {
		cfg := d.Config.Raw
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{"schema_version":1}`)
		}
		p.Deployments = append(p.Deployments, payloadDeployment{
			ID: d.ID, ProviderID: d.ProviderID, UpstreamModel: d.UpstreamModel,
			BaseURL: d.BaseURL, Protocol: string(d.Protocol), Region: d.Region,
			ConnectTimeoutMs: int64(d.ConnectTimeout / time.Millisecond),
			RequestTimeoutMs: int64(d.RequestTimeout / time.Millisecond),
			ConfigVersion:    d.ConfigVersion, Config: cfg, Status: string(d.Status),
		})
	}
	for _, r := range routes {
		p.Routes = append(p.Routes, payloadRoute{
			ID: r.ID, ModelID: r.ModelID, DeploymentID: r.DeploymentID,
			Priority: r.Priority, Weight: r.Weight,
			Capabilities: strSlice(r.Capabilities),
			PoolStrategy: string(r.PoolStrategy), Enabled: r.Enabled,
		})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		// Marshal of plain structs cannot fail; keep the signature simple.
		panic(fmt.Sprintf("catalog: build payload: %v", err))
	}
	return raw
}

// ParseSnapshot fully decodes one revision payload into an immutable
// Snapshot. It is all-or-nothing: any malformed entity, unknown field or
// wrong schema version fails the whole parse — the caller must keep
// serving the previously verified snapshot instead of a half-loaded one
// (设计 §5/§9: 不得加载半个版本).
func ParseSnapshot(rev *domain.ConfigRevision) (*Snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(rev.Payload.Raw))
	dec.DisallowUnknownFields()
	var p catalogPayload
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("catalog: parse revision %d payload: %w", rev.Revision, err)
	}
	if p.SchemaVersion != catalogPayloadSchemaVersion {
		return nil, fmt.Errorf("catalog: revision %d payload schema_version=%d, want %d",
			rev.Revision, p.SchemaVersion, catalogPayloadSchemaVersion)
	}

	snap := &Snapshot{
		RevisionID:    rev.ID,
		Revision:      rev.Revision,
		PublishedAt:   rev.PublishedAt,
		Models:        make(map[string]domain.Model, len(p.Models)),
		Providers:     make(map[string]domain.Provider, len(p.Providers)),
		Deployments:   make(map[string]domain.Deployment, len(p.Deployments)),
		RoutesByModel: make(map[string][]domain.ModelRoute, len(p.Models)),
	}
	for _, pm := range p.Models {
		protocols := make([]domain.Protocol, 0, len(pm.Protocols))
		for _, proto := range pm.Protocols {
			protocols = append(protocols, domain.Protocol(proto))
		}
		m := domain.Model{
			ID: pm.ID, DisplayName: pm.DisplayName,
			Lifecycle: domain.Lifecycle(pm.Lifecycle), ModelVersion: pm.ModelVersion,
			Aliases: pm.Aliases, InputModalities: pm.InputModalities,
			OutputModalities: pm.OutputModalities, ContextTokens: pm.ContextTokens,
			MaxOutputTokens: pm.MaxOutputTokens, Protocols: protocols,
			SupportsTools: pm.SupportsTools, SupportsReasoning: pm.SupportsReasoning,
		}
		if err := ValidateModel(&m); err != nil {
			return nil, fmt.Errorf("catalog: revision %d: %w", rev.Revision, err)
		}
		snap.Models[m.ID] = m
	}
	for _, pp := range p.Providers {
		snap.Providers[pp.ID] = domain.Provider{
			ID: pp.ID, Code: pp.Code, DisplayName: pp.DisplayName,
			AccessType: domain.AccessType(pp.AccessType), Status: pp.Status,
		}
	}
	for _, pd := range p.Deployments {
		d := domain.Deployment{
			ID: pd.ID, ProviderID: pd.ProviderID, UpstreamModel: pd.UpstreamModel,
			BaseURL: pd.BaseURL, Protocol: domain.Protocol(pd.Protocol), Region: pd.Region,
			ConnectTimeout: time.Duration(pd.ConnectTimeoutMs) * time.Millisecond,
			RequestTimeout: time.Duration(pd.RequestTimeoutMs) * time.Millisecond,
			ConfigVersion:  pd.ConfigVersion,
			Config:         domain.ExtensionConfig{SchemaVersion: 1, Raw: pd.Config},
			Status:         domain.DeploymentStatus(pd.Status),
		}
		if err := ValidateDeployment(&d); err != nil {
			return nil, fmt.Errorf("catalog: revision %d: %w", rev.Revision, err)
		}
		snap.Deployments[d.ID] = d
	}
	for _, pr := range p.Routes {
		route := domain.ModelRoute{
			ID: pr.ID, ModelID: pr.ModelID, DeploymentID: pr.DeploymentID,
			Priority: pr.Priority, Weight: pr.Weight, Capabilities: pr.Capabilities,
			PoolStrategy: domain.PoolStrategy(pr.PoolStrategy), Enabled: pr.Enabled,
		}
		if err := ValidateRoute(&route); err != nil {
			return nil, fmt.Errorf("catalog: revision %d: %w", rev.Revision, err)
		}
		if _, ok := snap.Models[route.ModelID]; !ok {
			return nil, fmt.Errorf("catalog: revision %d: route %s references unknown model %s",
				rev.Revision, route.ID, route.ModelID)
		}
		if _, ok := snap.Deployments[route.DeploymentID]; !ok {
			return nil, fmt.Errorf("catalog: revision %d: route %s references unknown deployment %s",
				rev.Revision, route.ID, route.DeploymentID)
		}
		snap.RoutesByModel[route.ModelID] = append(snap.RoutesByModel[route.ModelID], route)
	}
	// Deterministic route order for every consumer.
	for id := range snap.RoutesByModel {
		rs := snap.RoutesByModel[id]
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].Priority != rs[j].Priority {
				return rs[i].Priority < rs[j].Priority
			}
			if rs[i].Weight != rs[j].Weight {
				return rs[i].Weight > rs[j].Weight
			}
			return rs[i].ID < rs[j].ID
		})
		snap.RoutesByModel[id] = rs
	}
	return snap, nil
}

// revisionSource is the read side the snapshot cache needs: the cheap
// head probe plus the full revision load. Implemented by the postgres
// store (catalog_repo.go).
type revisionSource interface {
	ActiveRevisionHead(ctx context.Context, scope domain.ConfigScope) (int64, int, error)
	ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error)
}

// SnapshotCache serves the current active catalog snapshot with bounded
// publish delay and refresh-failure safety (设计 §5: 发布原子切换 active
// revision，各进程在有界延迟内加载完整快照；快照刷新失败继续用已验证
// 版本并告警；不得加载半个版本).
//
// Current first probes the cheap (id, revision) head; when it matches the
// cached snapshot the cached copy is returned without reading the payload.
// On a mismatch (or cold start) the full revision is loaded and parsed into
// a complete Snapshot BEFORE the atomic swap — a parse/build failure never
// publishes a partial snapshot. Any refresh failure with a verified
// snapshot in hand keeps serving it and invokes OnRefreshError (告警).
type SnapshotCache struct {
	src revisionSource

	// current is the last fully parsed, verified snapshot. Written only
	// after a complete successful ParseSnapshot.
	current atomic.Pointer[Snapshot]

	// OnRefreshError is invoked on every refresh failure that is absorbed
	// by serving the previous snapshot. Required — a silent absorb would
	// hide a stuck catalog. Wired to loud logging in cmd/server.
	OnRefreshError func(err error)
}

// NewSnapshotCache builds a cache over the revision source.
func NewSnapshotCache(src revisionSource, onRefreshError func(error)) *SnapshotCache {
	return &SnapshotCache{src: src, OnRefreshError: onRefreshError}
}

// Current returns the verified active snapshot. Cold start with no
// reachable/active revision returns an error — there is no verified
// version to fall back to, and serving an empty catalog would be a
// half-version in disguise.
func (c *SnapshotCache) Current(ctx context.Context) (*Snapshot, error) {
	headID, _, err := c.src.ActiveRevisionHead(ctx, domain.ScopeCatalog)
	if err != nil {
		if old := c.current.Load(); old != nil {
			c.absorb(err)
			return old, nil
		}
		return nil, fmt.Errorf("catalog: no verified snapshot and active revision unavailable: %w", err)
	}
	if old := c.current.Load(); old != nil && old.RevisionID == headID {
		return old, nil
	}
	rev, err := c.src.ActiveRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		if old := c.current.Load(); old != nil {
			c.absorb(err)
			return old, nil
		}
		return nil, err
	}
	// If a publish landed between the probe and this load, rev is already
	// the newer active row — revision ids are a monotonic BIGSERIAL, so
	// whatever ActiveRevision returns is the freshest truth.
	snap, err := ParseSnapshot(rev)
	if err != nil {
		if old := c.current.Load(); old != nil {
			c.absorb(err)
			return old, nil
		}
		return nil, err
	}
	c.current.Store(snap)
	return snap, nil
}

func (c *SnapshotCache) absorb(err error) {
	if c.OnRefreshError != nil {
		c.OnRefreshError(err)
	}
}
