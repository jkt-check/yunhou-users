package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// --- validation (设计 §5: 能力矩阵/上限/生命周期/公开 ID vs 上游名) ---

// modelIDPattern mirrors the DB CHECK on inference_models.id: the stable
// public model ID is lowercase slug-ish text, deliberately NOT a UUID and
// NOT the upstream model name.
var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var providerCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// upstreamProtocols are the wire protocols a Deployment may speak.
// ProtocolKayaChat is client-facing only and never appears on an upstream.
var upstreamProtocols = map[domain.Protocol]bool{
	domain.ProtocolOpenAIChat:       true,
	domain.ProtocolOpenAIResponses:  true,
	domain.ProtocolAnthropicMessage: true,
}

// lifecycleTransitions is the allowed commercial lifecycle state machine.
// Retired is terminal. New models start at draft and are NOT sellable.
var lifecycleTransitions = map[domain.Lifecycle][]domain.Lifecycle{
	domain.LifecycleDraft:      {domain.LifecycleActive, domain.LifecycleDeprecated, domain.LifecycleRetired},
	domain.LifecycleActive:     {domain.LifecycleDeprecated, domain.LifecycleRetired},
	domain.LifecycleDeprecated: {domain.LifecycleActive, domain.LifecycleRetired},
	domain.LifecycleRetired:    {},
}

// ValidateModel checks the model invariants enforced at create/update time.
func ValidateModel(m *domain.Model) error {
	if !modelIDPattern.MatchString(m.ID) {
		return domain.NewError(domain.CodeInvalidInput,
			"model id must match ^[a-z0-9][a-z0-9._-]{0,63}$ (stable public id, not the upstream name)")
	}
	if strings.TrimSpace(m.DisplayName) == "" {
		return domain.NewError(domain.CodeInvalidInput, "display_name is required")
	}
	switch m.Lifecycle {
	case "", domain.LifecycleDraft: // default applied by create
	default:
		if _, ok := lifecycleTransitions[m.Lifecycle]; !ok {
			return domain.NewError(domain.CodeInvalidInput, "unknown lifecycle: "+string(m.Lifecycle))
		}
	}
	if m.ContextTokens <= 0 {
		return domain.NewError(domain.CodeInvalidInput, "context_tokens must be > 0")
	}
	if m.MaxOutputTokens <= 0 {
		return domain.NewError(domain.CodeInvalidInput, "max_output_tokens must be > 0 (unlimited output is not representable)")
	}
	if len(m.Protocols) == 0 {
		return domain.NewError(domain.CodeInvalidInput, "protocols must not be empty")
	}
	for _, p := range m.Protocols {
		if p != domain.ProtocolKayaChat && !upstreamProtocols[p] {
			return domain.NewError(domain.CodeInvalidInput, "unknown protocol: "+string(p))
		}
	}
	if len(m.InputModalities) == 0 || len(m.OutputModalities) == 0 {
		return domain.NewError(domain.CodeInvalidInput, "input/output modalities must not be empty")
	}
	return nil
}

// ValidateProvider checks the provider invariants.
func ValidateProvider(p *domain.Provider) error {
	if !providerCodePattern.MatchString(p.Code) {
		return domain.NewError(domain.CodeInvalidInput,
			"provider code must match ^[a-z0-9][a-z0-9_-]{0,63}$")
	}
	if strings.TrimSpace(p.DisplayName) == "" {
		return domain.NewError(domain.CodeInvalidInput, "display_name is required")
	}
	switch p.AccessType {
	case domain.AccessOfficialAPI, domain.AccessOAuthConnector, domain.AccessSelfHosted:
	default:
		return domain.NewError(domain.CodeInvalidInput, "unknown access_type: "+string(p.AccessType))
	}
	if p.Status != "" && p.Status != "active" && p.Status != "disabled" {
		return domain.NewError(domain.CodeInvalidInput, "unknown provider status: "+p.Status)
	}
	return nil
}

// metadataIP is the cloud link-local metadata endpoint that must never be
// an upstream target (设计 §5 地址校验). The full SSRF restriction set
// (DNS pinning, private-range policy for self-hosted allowlists) lands
// with Task 4; this guards the catalog boundary already.
var metadataIP = net.ParseIP("169.254.169.254")

// ValidateDeployment checks the deployment invariants.
func ValidateDeployment(d *domain.Deployment) error {
	if d.ProviderID == "" {
		return domain.NewError(domain.CodeInvalidInput, "provider_id is required")
	}
	if strings.TrimSpace(d.UpstreamModel) == "" {
		return domain.NewError(domain.CodeInvalidInput, "upstream_model is required (the upstream-side name, distinct from the public model id)")
	}
	if !upstreamProtocols[d.Protocol] {
		return domain.NewError(domain.CodeInvalidInput,
			"deployment protocol must be one of openai_chat/openai_responses/anthropic_messages")
	}
	if err := validateBaseURL(d.BaseURL); err != nil {
		return err
	}
	if d.ConnectTimeout <= 0 || d.RequestTimeout <= 0 {
		return domain.NewError(domain.CodeInvalidInput, "connect/request timeouts must be > 0")
	}
	return nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return domain.NewError(domain.CodeInvalidInput,
			"base_url must be an absolute http(s) URL, got "+fmt.Sprintf("%q", raw))
	}
	if ip := net.ParseIP(strings.Trim(u.Hostname(), "[]")); ip != nil {
		if ip.Equal(metadataIP) || ip.IsLoopback() || ip.IsUnspecified() {
			return domain.NewError(domain.CodeInvalidInput,
				"base_url host "+ip.String()+" is not an allowed upstream target")
		}
	}
	return nil
}

// ValidateRoute checks the route invariants.
func ValidateRoute(r *domain.ModelRoute) error {
	if r.ModelID == "" || r.DeploymentID == "" {
		return domain.NewError(domain.CodeInvalidInput, "model_id and deployment_id are required")
	}
	if r.Weight <= 0 {
		return domain.NewError(domain.CodeInvalidInput, "route weight must be > 0")
	}
	switch r.PoolStrategy {
	case "", domain.PoolRoundRobin, domain.PoolLeastLoaded, domain.PoolSessionSticky:
	default:
		return domain.NewError(domain.CodeInvalidInput, "unknown pool strategy: "+string(r.PoolStrategy))
	}
	return nil
}

// ValidateLifecycleTransition reports whether from→to is allowed.
func ValidateLifecycleTransition(from, to domain.Lifecycle) error {
	for _, allowed := range lifecycleTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return domain.NewError(domain.CodeInvalidInput,
		fmt.Sprintf("lifecycle transition %s → %s is not allowed", from, to))
}

// --- store contract ---

// Store is the persistence contract of the catalog service. It is
// satisfied by internal/inference/postgres.Store (catalog_repo.go); the
// interface keeps this package free of sqlx.
type Store interface {
	// Models
	InsertModel(ctx context.Context, m *domain.Model) error
	GetModel(ctx context.Context, id string) (*domain.Model, error)
	ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error)
	UpdateModel(ctx context.Context, m *domain.Model) error
	DeleteModel(ctx context.Context, id string) error

	// Providers
	InsertProvider(ctx context.Context, p *domain.Provider) error
	GetProvider(ctx context.Context, id string) (*domain.Provider, error)
	GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error)
	ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error)
	UpdateProvider(ctx context.Context, p *domain.Provider) error
	DeleteProvider(ctx context.Context, id string) error

	// Deployments
	InsertDeployment(ctx context.Context, d *domain.Deployment) error
	GetDeployment(ctx context.Context, id string) (*domain.Deployment, error)
	FindDeployment(ctx context.Context, providerID, upstreamModel, baseURL string) (*domain.Deployment, error)
	ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error)
	UpdateDeployment(ctx context.Context, d *domain.Deployment) error
	DeleteDeployment(ctx context.Context, id string) error

	// Routes
	InsertRoute(ctx context.Context, r *domain.ModelRoute) error
	GetRoute(ctx context.Context, id string) (*domain.ModelRoute, error)
	ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error)
	UpdateRoute(ctx context.Context, r *domain.ModelRoute) error
	DeleteRoute(ctx context.Context, id string) error

	// Revisions
	InsertConfigRevision(ctx context.Context, rev *domain.ConfigRevision) error
	ActivateRevision(ctx context.Context, scope domain.ConfigScope, revision int) error
	ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error)
	ActiveRevisionHead(ctx context.Context, scope domain.ConfigScope) (int64, int, error)
	GetRevision(ctx context.Context, scope domain.ConfigScope, revision int) (*domain.ConfigRevision, error)
	ListRevisions(ctx context.Context, scope domain.ConfigScope) ([]domain.ConfigRevision, error)
	LatestRevision(ctx context.Context, scope domain.ConfigScope) (int, error)
}

// AccessCheck decides whether one caller may see/use one published model.
// Task 5/6 wire the real entitlement check; until then callers compose it
// from the service's snapshot and their own authorization data. Returning
// an error fails the listing (fail-closed, never silently over-grant).
type AccessCheck func(ctx context.Context, modelID string) (bool, error)

// PriceCheck reports whether a sellable price is bound to a model. Task 6
// wires the real PriceVersion lookup; nil means "not priced" (新模型未绑定
// 售价时不可售 — deny by default, never grant by default).
type PriceCheck func(ctx context.Context, modelID string) (bool, error)

// Service is the catalog application service: draft CRUD with optimistic
// locking, validation, atomic publish/rollback of immutable revisions and
// the published-model listing with price/access hooks.
type Service struct {
	store Store

	// priceCheck is the sellable gate for pricing; nil denies everything
	// (fail-closed until Task 6 binds real prices).
	priceCheck PriceCheck
}

// NewService builds the catalog service over a store.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetPriceCheck wires the pricing hook (Task 6). Until wired, no model is
// sellable — the default must be deny.
func (s *Service) SetPriceCheck(pc PriceCheck) { s.priceCheck = pc }

func (s *Service) Store() Store { return s.store }

// --- model CRUD ---

// CreateModel validates and inserts a draft model. Empty modality lists
// default to text→text, mirroring the DB column default.
func (s *Service) CreateModel(ctx context.Context, m *domain.Model) error {
	if m.Lifecycle == "" {
		m.Lifecycle = domain.LifecycleDraft
	}
	defaultModalities(m)
	if err := ValidateModel(m); err != nil {
		return err
	}
	return s.store.InsertModel(ctx, m)
}

// GetModel fetches one model.
func (s *Service) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	return s.store.GetModel(ctx, id)
}

// ListModels lists models with filter + keyset pagination.
func (s *Service) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	return s.store.ListModels(ctx, filter)
}

// UpdateModel validates and applies an edit. The caller passes the model
// as previously read (its UpdatedAt is the optimistic-lock token); a
// concurrent edit surfaces as CodeConflict.
func (s *Service) UpdateModel(ctx context.Context, m *domain.Model) error {
	if m.Lifecycle == "" {
		m.Lifecycle = domain.LifecycleDraft
	}
	if m.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeInvalidInput,
			"updated_at version token is required (read the model first)")
	}
	defaultModalities(m)
	if err := ValidateModel(m); err != nil {
		return err
	}
	return s.store.UpdateModel(ctx, m)
}

// SetModelLifecycle moves a model through the commercial lifecycle.
func (s *Service) SetModelLifecycle(ctx context.Context, id string, to domain.Lifecycle) error {
	m, err := s.store.GetModel(ctx, id)
	if err != nil {
		return err
	}
	from := m.Lifecycle
	if from == "" {
		from = domain.LifecycleDraft
	}
	if err := ValidateLifecycleTransition(from, to); err != nil {
		return err
	}
	m.Lifecycle = to
	return s.store.UpdateModel(ctx, m)
}

func defaultModalities(m *domain.Model) {
	if len(m.InputModalities) == 0 {
		m.InputModalities = []string{"text"}
	}
	if len(m.OutputModalities) == 0 {
		m.OutputModalities = []string{"text"}
	}
}

// DeleteModel removes a model and its routes.
func (s *Service) DeleteModel(ctx context.Context, id string) error {
	return s.store.DeleteModel(ctx, id)
}

// --- provider CRUD ---

// CreateProvider validates and inserts a provider.
func (s *Service) CreateProvider(ctx context.Context, p *domain.Provider) error {
	if err := ValidateProvider(p); err != nil {
		return err
	}
	return s.store.InsertProvider(ctx, p)
}

func (s *Service) GetProvider(ctx context.Context, id string) (*domain.Provider, error) {
	return s.store.GetProvider(ctx, id)
}

func (s *Service) GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error) {
	return s.store.GetProviderByCode(ctx, code)
}

func (s *Service) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	return s.store.ListProviders(ctx, afterID, limit)
}

func (s *Service) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	if p.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeInvalidInput,
			"updated_at version token is required (read the provider first)")
	}
	if err := ValidateProvider(p); err != nil {
		return err
	}
	return s.store.UpdateProvider(ctx, p)
}

func (s *Service) DeleteProvider(ctx context.Context, id string) error {
	return s.store.DeleteProvider(ctx, id)
}

// --- deployment CRUD ---

// CreateDeployment validates and inserts a deployment (status draft unless
// set; both connect and request timeouts must be positive).
func (s *Service) CreateDeployment(ctx context.Context, d *domain.Deployment) error {
	if d.Status == "" {
		d.Status = domain.DeploymentDraft
	}
	if d.ConnectTimeout == 0 {
		d.ConnectTimeout = 5 * time.Second
	}
	if d.RequestTimeout == 0 {
		d.RequestTimeout = 600 * time.Second
	}
	if err := ValidateDeployment(d); err != nil {
		return err
	}
	return s.store.InsertDeployment(ctx, d)
}

func (s *Service) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	return s.store.GetDeployment(ctx, id)
}

func (s *Service) ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	return s.store.ListDeployments(ctx, f)
}

// UpdateDeployment validates and applies an edit; ConfigVersion is the
// optimistic-lock token and is bumped on success.
func (s *Service) UpdateDeployment(ctx context.Context, d *domain.Deployment) error {
	if d.ConfigVersion <= 0 {
		return domain.NewError(domain.CodeInvalidInput,
			"config_version version token is required (read the deployment first)")
	}
	if err := ValidateDeployment(d); err != nil {
		return err
	}
	return s.store.UpdateDeployment(ctx, d)
}

func (s *Service) DeleteDeployment(ctx context.Context, id string) error {
	return s.store.DeleteDeployment(ctx, id)
}

// --- route CRUD ---

// CreateRoute validates and inserts a Model→Deployment mapping.
func (s *Service) CreateRoute(ctx context.Context, r *domain.ModelRoute) error {
	if r.PoolStrategy == "" {
		r.PoolStrategy = domain.PoolRoundRobin
	}
	if err := ValidateRoute(r); err != nil {
		return err
	}
	if _, err := s.store.GetModel(ctx, r.ModelID); err != nil {
		return domain.WrapError(domain.CodeInvalidInput, "route references unknown model "+r.ModelID, err)
	}
	if _, err := s.store.GetDeployment(ctx, r.DeploymentID); err != nil {
		return domain.WrapError(domain.CodeInvalidInput, "route references unknown deployment "+r.DeploymentID, err)
	}
	return s.store.InsertRoute(ctx, r)
}

func (s *Service) GetRoute(ctx context.Context, id string) (*domain.ModelRoute, error) {
	return s.store.GetRoute(ctx, id)
}

// ListRoutes returns all routes of a model, enabled or not.
func (s *Service) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return s.store.ListRoutes(ctx, modelID)
}

func (s *Service) UpdateRoute(ctx context.Context, r *domain.ModelRoute) error {
	if r.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeInvalidInput,
			"updated_at version token is required (read the route first)")
	}
	if err := ValidateRoute(r); err != nil {
		return err
	}
	return s.store.UpdateRoute(ctx, r)
}

func (s *Service) DeleteRoute(ctx context.Context, id string) error {
	return s.store.DeleteRoute(ctx, id)
}

// --- publish / rollback (设计 §5: 草稿 → 校验 → 原子发布; 回滚不改历史) ---

// publishPageSize is the keyset page size used when draining the catalog
// for a publish. The old hardcoded single-page Limit: 500 silently
// truncated catalogs with more than 500 entities (审查修复 Important #1:
// 发布出不完整快照且不报错，违反"不得加载半个版本") — Publish now pages
// through the ENTIRE catalog.
const publishPageSize = 500

// publishMaxEntities is the defensive upper bound of the paged drain: past
// this many entities of one kind the publish fails loudly with a clear
// error instead of publishing a snapshot nobody can reason about.
const publishMaxEntities = 20000

// drainPages walks a keyset-paginated listing until the store returns a
// short page, collecting every entity. page must return the next page for
// the given cursor and report each entity's cursor key.
func drainPages[T any](ctx context.Context, page func(after string, limit int) ([]T, error), key func(T) string) ([]T, error) {
	out := make([]T, 0, publishPageSize)
	after := ""
	for {
		items, err := page(after, publishPageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) < publishPageSize {
			return out, nil
		}
		if len(out) >= publishMaxEntities {
			return nil, domain.NewError(domain.CodeInvalidInput,
				fmt.Sprintf("catalog too large to publish: more than %d entities", publishMaxEntities))
		}
		after = key(items[len(items)-1])
	}
}

func (s *Service) allModels(ctx context.Context) ([]domain.Model, error) {
	return drainPages(ctx,
		func(after string, limit int) ([]domain.Model, error) {
			return s.store.ListModels(ctx, domain.ModelFilter{AfterID: after, Limit: limit})
		},
		func(m domain.Model) string { return m.ID })
}

func (s *Service) allProviders(ctx context.Context) ([]domain.Provider, error) {
	return drainPages(ctx,
		func(after string, limit int) ([]domain.Provider, error) {
			return s.store.ListProviders(ctx, after, limit)
		},
		func(p domain.Provider) string { return p.ID })
}

func (s *Service) allDeployments(ctx context.Context) ([]domain.Deployment, error) {
	return drainPages(ctx,
		func(after string, limit int) ([]domain.Deployment, error) {
			return s.store.ListDeployments(ctx, domain.DeploymentFilter{AfterID: after, Limit: limit})
		},
		func(d domain.Deployment) string { return d.ID })
}

// Publish validates the current draft catalog, appends an immutable
// revision and atomically switches the active pointer to it. The returned
// number is the new revision. Validation happens BEFORE anything is
// written; the revision insert + activation is the atomic switch
// (ActivateRevision runs in a single DB transaction with the partial
// unique index as the safety net). The catalog is drained with keyset
// pagination so no entity is ever silently dropped from the snapshot.
func (s *Service) Publish(ctx context.Context, createdBy string) (int, error) {
	models, err := s.allModels(ctx)
	if err != nil {
		return 0, err
	}
	providers, err := s.allProviders(ctx)
	if err != nil {
		return 0, err
	}
	deployments, err := s.allDeployments(ctx)
	if err != nil {
		return 0, err
	}
	if err := validateCatalogForPublish(models, providers, deployments); err != nil {
		return 0, err
	}
	var routes []domain.ModelRoute
	for _, m := range models {
		rs, err := s.store.ListRoutes(ctx, m.ID)
		if err != nil {
			return 0, err
		}
		routes = append(routes, rs...)
	}

	latest, err := s.store.LatestRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		return 0, err
	}
	rev := &domain.ConfigRevision{
		Scope:     domain.ScopeCatalog,
		Revision:  latest + 1,
		Payload:   domain.ExtensionConfig{SchemaVersion: 1, Raw: BuildCatalogPayload(models, providers, deployments, routes)},
		Status:    domain.RevisionDraft,
		CreatedBy: createdBy,
	}
	if err := s.store.InsertConfigRevision(ctx, rev); err != nil {
		return 0, err // UNIQUE(scope, revision) → CodeConflict on racing publishers
	}
	if err := s.store.ActivateRevision(ctx, domain.ScopeCatalog, rev.Revision); err != nil {
		return 0, err
	}
	return rev.Revision, nil
}

// validateCatalogForPublish is the pre-publish gate: every entity must be
// individually valid and cross-references must resolve. Draft/disabled
// entities are publishable (the snapshot marks them non-routable) — the
// gate rejects only structurally broken catalogs.
func validateCatalogForPublish(models []domain.Model, providers []domain.Provider, deployments []domain.Deployment) error {
	providerIDs := make(map[string]bool, len(providers))
	for i := range providers {
		if err := ValidateProvider(&providers[i]); err != nil {
			return err
		}
		providerIDs[providers[i].ID] = true
	}
	for i := range models {
		if err := ValidateModel(&models[i]); err != nil {
			return err
		}
	}
	for i := range deployments {
		if err := ValidateDeployment(&deployments[i]); err != nil {
			return err
		}
		if !providerIDs[deployments[i].ProviderID] {
			return domain.NewError(domain.CodeInvalidInput,
				"deployment "+deployments[i].ID+" references unknown provider "+deployments[i].ProviderID)
		}
	}
	return nil
}

// Rollback publishes a NEW revision carrying the content of an older one
// (设计 §5/§10: 回滚不改历史账单语义). The history stays immutable: the
// target revision is only READ, and the new revision gets the next
// revision number. Only revisions that were published at some point may be
// rollback targets.
func (s *Service) Rollback(ctx context.Context, toRevision int, createdBy string) (int, error) {
	target, err := s.store.GetRevision(ctx, domain.ScopeCatalog, toRevision)
	if err != nil {
		return 0, err
	}
	if target.Status != domain.RevisionPublished && target.Status != domain.RevisionSuperseded {
		return 0, domain.NewError(domain.CodeInvalidInput,
			fmt.Sprintf("revision %d was never published and cannot be a rollback target", toRevision))
	}
	// The payload must be loadable before we publish it again — rolling
	// forward to garbage would brick the catalog.
	if _, err := ParseSnapshot(target); err != nil {
		return 0, domain.WrapError(domain.CodeInvalidInput, "rollback target is not a parseable snapshot", err)
	}
	latest, err := s.store.LatestRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		return 0, err
	}
	rev := &domain.ConfigRevision{
		Scope:     domain.ScopeCatalog,
		Revision:  latest + 1,
		Payload:   domain.ExtensionConfig{SchemaVersion: 1, Raw: append(json.RawMessage(nil), target.Payload.Raw...)},
		Status:    domain.RevisionDraft,
		CreatedBy: createdBy,
	}
	if err := s.store.InsertConfigRevision(ctx, rev); err != nil {
		return 0, err
	}
	if err := s.store.ActivateRevision(ctx, domain.ScopeCatalog, rev.Revision); err != nil {
		return 0, err
	}
	return rev.Revision, nil
}

// ListRevisions returns the catalog revision history, newest first.
func (s *Service) ListRevisions(ctx context.Context) ([]domain.ConfigRevision, error) {
	return s.store.ListRevisions(ctx, domain.ScopeCatalog)
}

// LoadSnapshot loads and parses the currently active catalog revision
// into an immutable Snapshot. Callers that need per-request pinning build
// their own SnapshotCache; a one-off LoadSnapshot always reflects the
// freshest active revision at call time.
func (s *Service) LoadSnapshot(ctx context.Context) (*Snapshot, error) {
	rev, err := s.store.ActiveRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		return nil, err
	}
	return ParseSnapshot(rev)
}

// ListPublishedModels returns the models a caller may see in the
// published model list (设计 §9.1 GET /v1/models): lifecycle active, at
// least one enabled route to an active deployment, a bound sellable price,
// and the caller's access check. Price and access are fail-closed hooks —
// until Task 5/6 wire the real resolvers, the defaults deny everything.
// A nil access check is an explicit error, never a panic and never an
// implicit allow (审查修复 Important #2).
func (s *Service) ListPublishedModels(ctx context.Context, access AccessCheck) ([]domain.Model, error) {
	if access == nil {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"access check is required to list published models (refusing to answer without an authorization hook)")
	}
	snap, err := s.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Model, 0, len(snap.Models))
	for _, id := range snap.ModelIDs() {
		if !snap.Sellable(id) {
			continue
		}
		if s.priceCheck == nil {
			continue // 未绑定售价不可售 — deny by default
		}
		ok, err := s.priceCheck(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		ok, err = access(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		m := snap.Models[id]
		out = append(out, m)
	}
	return out, nil
}
