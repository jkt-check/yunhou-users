package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// import.go — LLM_PROVIDERS_JSON compatibility import (基线报告差距 1:
// 环境变量目录只做兼容导入，运行期真相是数据库草稿 + 发布快照).
//
// Contract (任务书): the import is EXPLICIT (runs only when the operator
// sets LLM_PROVIDERS_JSON) and IDEMPOTENT. Existing rows are never updated
// or overwritten — the database is the operational truth and an env line
// must not clobber operator edits on every boot. Imported entities start as
// DRAFT and stay unsellable until an operator validates, prices, authorizes
// and publishes them (设计 §5).

// EnvCatalog is the decoded shape of LLM_PROVIDERS_JSON. Unknown fields are
// rejected (a typo'd field must fail loudly at startup, not silently drop a
// deployment).
type EnvCatalog struct {
	Providers []EnvProvider `json:"providers"`
	Models    []EnvModel    `json:"models"`
}

// EnvProvider is one vendor/upstream family entry.
type EnvProvider struct {
	Code        string            `json:"code"`
	DisplayName string            `json:"display_name"`
	AccessType  domain.AccessType `json:"access_type"`
	Status      string            `json:"status"`
}

// EnvModel is one public model entry plus its upstream deployments.
type EnvModel struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	ModelVersion     string   `json:"model_version"`
	Aliases          []string `json:"aliases"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	ContextTokens    int      `json:"context_tokens"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	Protocols        []string `json:"protocols"`
	// Capabilities folds the boolean capability matrix.
	Capabilities struct {
		Tools     bool `json:"tools"`
		Reasoning bool `json:"reasoning"`
	} `json:"capabilities"`
	Deployments []EnvDeployment `json:"deployments"`
}

// EnvDeployment is one upstream endpoint for an EnvModel. Provider refers
// to an EnvProvider code.
type EnvDeployment struct {
	Provider         string `json:"provider"`
	UpstreamModel    string `json:"upstream_model"`
	BaseURL          string `json:"base_url"`
	Protocol         string `json:"protocol"`
	Region           string `json:"region"`
	ConnectTimeoutMs int    `json:"connect_timeout_ms"`
	RequestTimeoutMs int    `json:"request_timeout_ms"`
}

// ImportResult summarizes one import run.
type ImportResult struct {
	ProvidersInserted   int `json:"providers_inserted"`
	ModelsInserted      int `json:"models_inserted"`
	DeploymentsInserted int `json:"deployments_inserted"`
	RoutesInserted      int `json:"routes_inserted"`
	// Skipped counts entities that already existed (by natural key) and
	// were left untouched — the idempotency signal.
	Skipped int `json:"skipped"`
}

// ParseEnvCatalog decodes and fully validates the env JSON. Every entry is
// converted to its domain shape and passed through the same validators as
// operator CRUD, so a bad env fails at startup before any row is written.
func ParseEnvCatalog(raw string) (*EnvCatalog, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var ec EnvCatalog
	if err := dec.Decode(&ec); err != nil {
		return nil, domain.WrapError(domain.CodeInvalidInput, "LLM_PROVIDERS_JSON is not valid catalog JSON", err)
	}
	for i := range ec.Providers {
		p := &ec.Providers[i]
		if err := ValidateProvider(&domain.Provider{
			Code: p.Code, DisplayName: p.DisplayName, AccessType: p.AccessType, Status: p.Status,
		}); err != nil {
			return nil, err
		}
	}
	providerCodes := make(map[string]bool, len(ec.Providers))
	for _, p := range ec.Providers {
		if providerCodes[p.Code] {
			return nil, domain.NewError(domain.CodeInvalidInput, "duplicate provider code: "+p.Code)
		}
		providerCodes[p.Code] = true
	}
	for i := range ec.Models {
		m := &ec.Models[i]
		if len(m.InputModalities) == 0 {
			m.InputModalities = []string{"text"}
		}
		if len(m.OutputModalities) == 0 {
			m.OutputModalities = []string{"text"}
		}
		dm := domain.Model{
			ID: m.ID, DisplayName: m.DisplayName, Lifecycle: domain.LifecycleDraft,
			ModelVersion: m.ModelVersion, Aliases: m.Aliases,
			InputModalities: m.InputModalities, OutputModalities: m.OutputModalities,
			ContextTokens: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens,
			SupportsTools: m.Capabilities.Tools, SupportsReasoning: m.Capabilities.Reasoning,
		}
		for _, proto := range m.Protocols {
			dm.Protocols = append(dm.Protocols, domain.Protocol(proto))
		}
		if err := ValidateModel(&dm); err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(m.Deployments))
		for j := range m.Deployments {
			d := &m.Deployments[j]
			if !providerCodes[d.Provider] {
				return nil, domain.NewError(domain.CodeInvalidInput,
					"model "+m.ID+": deployment references unknown provider "+d.Provider)
			}
			nk := d.Provider + "|" + d.UpstreamModel + "|" + d.BaseURL
			if seen[nk] {
				return nil, domain.NewError(domain.CodeInvalidInput,
					"model "+m.ID+": duplicate deployment "+nk)
			}
			seen[nk] = true
			dd := domain.Deployment{
				// ProviderID is resolved at import time; the parse-time
				// validation only needs a syntactically present value.
				ProviderID:    "00000000-0000-0000-0000-000000000000",
				UpstreamModel: d.UpstreamModel, BaseURL: d.BaseURL,
				Protocol:       domain.Protocol(d.Protocol),
				Region:         d.Region,
				ConnectTimeout: time.Duration(d.ConnectTimeoutMs) * time.Millisecond,
				RequestTimeout: time.Duration(d.RequestTimeoutMs) * time.Millisecond,
			}
			if dd.ConnectTimeout == 0 {
				dd.ConnectTimeout = 5 * time.Second
			}
			if dd.RequestTimeout == 0 {
				dd.RequestTimeout = 600 * time.Second
			}
			if err := ValidateDeployment(&dd); err != nil {
				return nil, domain.WrapError(domain.CodeInvalidInput, "model "+m.ID, err)
			}
		}
	}
	return &ec, nil
}

// ImportEnvCatalog imports a parsed env catalog into the database. It is
// idempotent and non-destructive: every existing row (matched by natural
// key) is skipped untouched, so re-running on every boot is a no-op once
// the catalog has been imported, and operator edits made afterwards are
// never overwritten by the env.
func (s *Service) ImportEnvCatalog(ctx context.Context, raw string) (*ImportResult, error) {
	ec, err := ParseEnvCatalog(raw)
	if err != nil {
		return nil, err
	}
	res := &ImportResult{}

	// Providers: matched by code, never updated.
	providerIDs := make(map[string]string, len(ec.Providers))
	for i := range ec.Providers {
		ep := &ec.Providers[i]
		existing, err := s.store.GetProviderByCode(ctx, ep.Code)
		if err == nil {
			providerIDs[ep.Code] = existing.ID
			res.Skipped++
			continue
		}
		if domain.CodeOf(err) != domain.CodeNotFound {
			return nil, err
		}
		p := &domain.Provider{
			Code: ep.Code, DisplayName: ep.DisplayName, AccessType: ep.AccessType,
			Status: firstNonEmpty(ep.Status, "active"),
		}
		if err := s.store.InsertProvider(ctx, p); err != nil {
			return nil, err
		}
		providerIDs[ep.Code] = p.ID
		res.ProvidersInserted++
	}

	for i := range ec.Models {
		em := &ec.Models[i]
		deploymentIDs := make([]string, 0, len(em.Deployments))

		// Model: matched by public id, never updated.
		_, err := s.store.GetModel(ctx, em.ID)
		if err == nil {
			res.Skipped++
		} else if domain.CodeOf(err) == domain.CodeNotFound {
			dm := &domain.Model{
				ID: em.ID, DisplayName: em.DisplayName, Lifecycle: domain.LifecycleDraft,
				ModelVersion: em.ModelVersion, Aliases: em.Aliases,
				InputModalities: em.InputModalities, OutputModalities: em.OutputModalities,
				ContextTokens: em.ContextTokens, MaxOutputTokens: em.MaxOutputTokens,
				SupportsTools: em.Capabilities.Tools, SupportsReasoning: em.Capabilities.Reasoning,
			}
			for _, proto := range em.Protocols {
				dm.Protocols = append(dm.Protocols, domain.Protocol(proto))
			}
			if err := s.store.InsertModel(ctx, dm); err != nil {
				return nil, err
			}
			res.ModelsInserted++
		} else {
			return nil, err
		}

		for j := range em.Deployments {
			ed := &em.Deployments[j]
			pid := providerIDs[ed.Provider]
			existing, err := s.store.FindDeployment(ctx, pid, ed.UpstreamModel, ed.BaseURL)
			if err == nil {
				deploymentIDs = append(deploymentIDs, existing.ID)
				res.Skipped++
				continue
			}
			if domain.CodeOf(err) != domain.CodeNotFound {
				return nil, err
			}
			dd := &domain.Deployment{
				ProviderID: pid, UpstreamModel: ed.UpstreamModel, BaseURL: ed.BaseURL,
				Protocol:       domain.Protocol(ed.Protocol),
				Region:         ed.Region,
				ConnectTimeout: time.Duration(ed.ConnectTimeoutMs) * time.Millisecond,
				RequestTimeout: time.Duration(ed.RequestTimeoutMs) * time.Millisecond,
				Status:         domain.DeploymentDraft,
			}
			if dd.ConnectTimeout == 0 {
				dd.ConnectTimeout = 5 * time.Second
			}
			if dd.RequestTimeout == 0 {
				dd.RequestTimeout = 600 * time.Second
			}
			if err := s.store.InsertDeployment(ctx, dd); err != nil {
				return nil, err
			}
			deploymentIDs = append(deploymentIDs, dd.ID)
			res.DeploymentsInserted++
		}

		// Routes: matched by (model, deployment), never updated.
		existingRoutes, err := s.store.ListRoutes(ctx, em.ID)
		if err != nil {
			return nil, err
		}
		have := make(map[string]bool, len(existingRoutes))
		for _, r := range existingRoutes {
			have[r.DeploymentID] = true
		}
		for _, depID := range deploymentIDs {
			if have[depID] {
				res.Skipped++
				continue
			}
			mr := &domain.ModelRoute{
				ModelID: em.ID, DeploymentID: depID,
				Weight: 1, Enabled: true,
				PoolStrategy: domain.PoolRoundRobin,
			}
			if err := s.store.InsertRoute(ctx, mr); err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					res.Skipped++ // racing importer created it — still idempotent
					continue
				}
				return nil, err
			}
			res.RoutesInserted++
		}
	}
	return res, nil
}

func firstNonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
