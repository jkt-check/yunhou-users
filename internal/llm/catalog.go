// Package llm is the multi-model gateway's leaf library: provider/model
// catalog (parsed from LLM_PROVIDERS_JSON), per-provider API key pools,
// and the two upstream protocol adapters (OpenAI Chat Completions and
// Anthropic Messages). It imports nothing from the project except
// internal/model (request shapes) so it stays independently testable.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Upstream wire protocols. The service layer speaks one of these two to the
// provider; everything downstream of the service (relay, audit log, usage
// extraction) sees OpenAI chat.completion.chunk SSE only.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// Provider is one upstream API account group: a protocol, an origin, and a
// pool of interchangeable API keys. Headers are extra static headers sent on
// every request (some coding-plan endpoints require custom auth headers).
type Provider struct {
	Protocol string            `json:"protocol"`
	BaseURL  string            `json:"base_url"`
	APIKeys  []string          `json:"api_keys"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// Model is one logical built-in model entry. InputPerMtok/OutputPerMtok are
// CNY per 1M tokens used for cost accounting; 0 = unmetered price (the row is
// still recorded, with cost 0). The identity cost_micros = tokens*price holds
// because ¥/Mtok × tokens / 1e6 × 1e6 µ¥/¥ = tokens × price.
type Model struct {
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstream_model"`
	DisplayName   string  `json:"display_name"`
	InputPerMtok  float64 `json:"input_price_per_mtok"`
	OutputPerMtok float64 `json:"output_price_per_mtok"`
	// MaxTokens is only sent to Anthropic-protocol providers (the API
	// requires it). 0 → provider default 8192 at translation time.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// Catalog is the parsed LLM_PROVIDERS_JSON.
type Catalog struct {
	DefaultModel string              `json:"default_model"`
	Providers    map[string]Provider `json:"providers"`
	Models       map[string]Model    `json:"models"`
}

// ParseCatalog parses and validates LLM_PROVIDERS_JSON. Empty input is not an
// error: it returns (nil, nil) so the caller can fall back to the legacy
// DEEPSEEK_* envs (LegacyCatalog) or disable chat entirely.
func ParseCatalog(raw string) (*Catalog, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var c Catalog
	dec := json.NewDecoder(strings.NewReader(raw))
	// Typos in operator-authored JSON must fail at boot, not silently
	// misconfigure routing.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse LLM_PROVIDERS_JSON: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("LLM_PROVIDERS_JSON: %w", err)
	}
	return &c, nil
}

// Validate enforces referential integrity so misconfiguration fails at
// startup instead of surfacing as per-request 502s.
func (c *Catalog) Validate() error {
	if c == nil {
		return errors.New("catalog is nil")
	}
	if len(c.Providers) == 0 {
		return errors.New("providers must not be empty")
	}
	if len(c.Models) == 0 {
		return errors.New("models must not be empty")
	}
	for name, p := range c.Providers {
		if p.Protocol != ProtocolOpenAI && p.Protocol != ProtocolAnthropic {
			return fmt.Errorf("provider %q: protocol must be %q or %q", name, ProtocolOpenAI, ProtocolAnthropic)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q: base_url must be an absolute http(s) URL", name)
		}
		if len(p.APIKeys) == 0 {
			return fmt.Errorf("provider %q: api_keys must not be empty", name)
		}
		for i, k := range p.APIKeys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("provider %q: api_keys[%d] is empty", name, i)
			}
		}
	}
	for id, m := range c.Models {
		if strings.TrimSpace(id) == "" || len(id) > 64 {
			return fmt.Errorf("invalid model id %q (1-64 chars)", id)
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("model %q references unknown provider %q", id, m.Provider)
		}
		if m.UpstreamModel == "" {
			return fmt.Errorf("model %q: upstream_model is required", id)
		}
		if m.MaxTokens < 0 {
			return fmt.Errorf("model %q: max_tokens must be >= 0", id)
		}
		if m.InputPerMtok < 0 || m.OutputPerMtok < 0 {
			return fmt.Errorf("model %q: price must be >= 0", id)
		}
	}
	// Single-model catalogs get an implicit default so a one-model deployment
	// doesn't need the field; multi-model catalogs must say which is default.
	if c.DefaultModel == "" {
		if len(c.Models) == 1 {
			for id := range c.Models {
				c.DefaultModel = id
			}
		} else {
			return errors.New("default_model is required when more than one model is defined")
		}
	}
	if _, ok := c.Models[c.DefaultModel]; !ok {
		return fmt.Errorf("default_model %q not in models", c.DefaultModel)
	}
	return nil
}

// Resolve maps a client-supplied logical model id to its catalog entry.
// Empty logical resolves to the catalog default (back-compat: pre-multi-model
// clients never send a model field).
func (c *Catalog) Resolve(logical string) (string, Model, bool) {
	if logical == "" {
		logical = c.DefaultModel
	}
	m, ok := c.Models[logical]
	return logical, m, ok
}

// ModelIDs returns all logical model ids, sorted, for stable API output.
func (c *Catalog) ModelIDs() []string {
	ids := make([]string, 0, len(c.Models))
	for id := range c.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// LegacyCatalog synthesizes a single-provider, single-model catalog from the
// DEEPSEEK_API_KEY / DEEPSEEK_BASE_URL / DEEPSEEK_MODEL envs so existing
// deployments keep working unchanged. Empty apiKey → nil (chat disabled).
func LegacyCatalog(apiKey, baseURL, upstreamModel string) *Catalog {
	if apiKey == "" {
		return nil
	}
	return &Catalog{
		DefaultModel: upstreamModel,
		Providers: map[string]Provider{
			"deepseek": {Protocol: ProtocolOpenAI, BaseURL: baseURL, APIKeys: []string{apiKey}},
		},
		Models: map[string]Model{
			upstreamModel: {Provider: "deepseek", UpstreamModel: upstreamModel, DisplayName: upstreamModel},
		},
	}
}
