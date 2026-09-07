# Multi-Model Gateway (内置模型多供应商路由) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the single-upstream `POST /chat` DeepSeek proxy into a multi-model gateway: the terminal picks a logical built-in model (e.g. `deepseek-flash`, `kimi-k3`, `glm-5.2`), and yunhou-users routes it to the configured upstream provider (OpenAI-compatible or Anthropic Messages protocol), holds all API keys server-side, meters tokens per request into a new `llm_usage_events` table, and gates models per plan.

**Architecture:** New leaf package `internal/llm/` owns the provider/model catalog (parsed from a single `LLM_PROVIDERS_JSON` env var), per-provider API key pools with cooldown, and the two protocol adapters (OpenAI Chat Completions pass-through-with-rewrite; Anthropic Messages request translation + SSE event translation back into OpenAI chunk shape). `ChatService` becomes protocol-agnostic: it resolves the logical model → route, runs the existing subscription gate extended with a per-plan model allowlist, performs the upstream call (with one cross-key retry on 429/5xx), and records usage after the relay. The handler keeps its existing SSE relay / audit trail untouched — the Anthropic translator emits OpenAI-shaped chunks so downstream code sees one protocol only.

**Tech Stack:** Go 1.25, stdlib `net/http` only for outbound calls (no new third-party deps), gin, sqlx + lib/pq, existing migration ledger (`migrations/022+`).

**Spec:** This plan is self-contained; it implements the design agreed in the 2026-09-07 session: logical-model catalog + provider registry, dual-protocol upstream support, server-side key custody with pooling, per-plan model gating, token-level usage metering (`llm_usage_events`), a `GET /chat/models` listing endpoint for the terminal's model picker, and an admin stats endpoint `GET /admin/stats/llm-usage`. Backward compatibility: existing `DEEPSEEK_*` envs keep working (synthesized single-model catalog) and requests without a `model` field use the catalog default.

## Global Constraints

- No new external dependencies. Outbound HTTP stays stdlib `net/http` (project rule; go.mod must not grow).
- All SQL parameterized; column names never interpolated from caller input.
- JSON-in-env config follows the `PLAN_AMOUNT_OVERRIDE_JSON` precedent; unknown JSON keys rejected (`DisallowUnknownFields`) so typos fail at boot.
- Metering must never break the chat path: usage insert errors are logged, not returned.
- Response shapes: errors `{"code":status,"data":null,"message":msg}`; success `{"code":0,"data":...}`.
- Sentinel errors matched with `errors.Is`, never string comparison.
- Commit after each task. Commit messages: `feat: ...` / `test: ...` conventional style, in English.
- TDD: failing test first, then implementation, then verify, then commit.
- Test commands: `go test ./internal/... -run <TestName> -v` for focused runs; `go build ./... && go test ./...` before each commit.

---

### Task 1: `internal/llm` — catalog + key pool

**Files:**
- Create: `internal/llm/catalog.go`
- Create: `internal/llm/keypool.go`
- Test: `internal/llm/catalog_test.go`
- Test: `internal/llm/keypool_test.go`

**Interfaces:**
- Consumes: nothing (leaf package; imports stdlib only).
- Produces (used by Tasks 2-9):
  - `const ProtocolOpenAI = "openai"`, `const ProtocolAnthropic = "anthropic"`
  - `type Provider struct { Protocol string; BaseURL string; APIKeys []string; Headers map[string]string }` (JSON tags: `protocol`, `base_url`, `api_keys`, `headers,omitempty`)
  - `type Model struct { Provider string; UpstreamModel string; DisplayName string; InputPerMtok float64; OutputPerMtok float64; MaxTokens int }` (JSON tags: `provider`, `upstream_model`, `display_name`, `input_price_per_mtok`, `output_price_per_mtok`, `max_tokens,omitempty`)
  - `type Catalog struct { DefaultModel string; Providers map[string]Provider; Models map[string]Model }` (JSON tags: `default_model`, `providers`, `models`)
  - `func ParseCatalog(raw string) (*Catalog, error)` — empty/whitespace raw → `(nil, nil)`
  - `func (c *Catalog) Validate() error`
  - `func (c *Catalog) Resolve(logical string) (resolvedID string, m Model, ok bool)` — empty logical → DefaultModel
  - `func (c *Catalog) ModelIDs() []string` — sorted
  - `func LegacyCatalog(apiKey, baseURL, upstreamModel string) *Catalog` — apiKey empty → nil
  - `const KeyCooldown = 60 * time.Second`
  - `type KeyPool`; `func NewKeyPool(keys []string) *KeyPool`; `func (p *KeyPool) Acquire(now time.Time) (idx int, key string)`; `func (p *KeyPool) Cool(idx int, until time.Time)`; `func (p *KeyPool) Len() int`

- [ ] **Step 1: Write the failing tests**

`internal/llm/catalog_test.go`:

```go
package llm

import (
	"strings"
	"testing"
)

const testCatalogJSON = `{
  "default_model": "deepseek-flash",
  "providers": {
    "deepseek": {"protocol": "openai", "base_url": "https://api.deepseek.com", "api_keys": ["sk-a", "sk-b"]},
    "kimi": {"protocol": "anthropic", "base_url": "https://api.kimi.com/coding", "api_keys": ["sk-kimi-1"], "headers": {"X-Region": "cn"}}
  },
  "models": {
    "deepseek-flash": {"provider": "deepseek", "upstream_model": "deepseek-chat", "display_name": "DeepSeek Flash", "input_price_per_mtok": 2.0, "output_price_per_mtok": 8.0},
    "kimi-k3": {"provider": "kimi", "upstream_model": "kimi-k3-latest", "display_name": "Kimi K3", "max_tokens": 16384}
  }
}`

func TestParseCatalog_Empty(t *testing.T) {
	c, err := ParseCatalog("  ")
	if err != nil || c != nil {
		t.Fatalf("ParseCatalog(empty) = (%v, %v), want (nil, nil)", c, err)
	}
}

func TestParseCatalog_Valid(t *testing.T) {
	c, err := ParseCatalog(testCatalogJSON)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if c.DefaultModel != "deepseek-flash" {
		t.Errorf("DefaultModel = %q", c.DefaultModel)
	}
	if got := c.Providers["kimi"].Headers["X-Region"]; got != "cn" {
		t.Errorf("kimi header = %q", got)
	}
	// Resolve with explicit id
	id, m, ok := c.Resolve("kimi-k3")
	if !ok || id != "kimi-k3" || m.UpstreamModel != "kimi-k3-latest" || m.MaxTokens != 16384 {
		t.Errorf("Resolve(kimi-k3) = %q, %+v, %v", id, m, ok)
	}
	// Resolve empty → default
	id, m, ok = c.Resolve("")
	if !ok || id != "deepseek-flash" || m.Provider != "deepseek" {
		t.Errorf("Resolve(\"\") = %q, %+v, %v", id, m, ok)
	}
	// Resolve unknown
	if _, _, ok = c.Resolve("nope"); ok {
		t.Errorf("Resolve(nope) ok = true, want false")
	}
	// ModelIDs sorted
	ids := c.ModelIDs()
	if len(ids) != 2 || ids[0] != "deepseek-flash" || ids[1] != "kimi-k3" {
		t.Errorf("ModelIDs = %v", ids)
	}
}

func TestParseCatalog_RejectsUnknownFields(t *testing.T) {
	raw := `{"default_model":"m","providers":{},"models":{},"bogus":1}`
	if _, err := ParseCatalog(raw); err == nil {
		t.Fatal("want error for unknown field")
	}
}

func TestCatalog_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Catalog)
		wantErr string
	}{
		{"no providers", func(c *Catalog) { c.Providers = nil }, "providers"},
		{"no models", func(c *Catalog) { c.Models = nil }, "models"},
		{"bad protocol", func(c *Catalog) { p := c.Providers["deepseek"]; p.Protocol = "grpc"; c.Providers["deepseek"] = p }, "protocol"},
		{"bad base url", func(c *Catalog) { p := c.Providers["deepseek"]; p.BaseURL = "not-a-url"; c.Providers["deepseek"] = p }, "base_url"},
		{"no keys", func(c *Catalog) { p := c.Providers["deepseek"]; p.APIKeys = nil; c.Providers["deepseek"] = p }, "api_keys"},
		{"model references unknown provider", func(c *Catalog) { m := c.Models["deepseek-flash"]; m.Provider = "ghost"; c.Models["deepseek-flash"] = m }, "unknown provider"},
		{"model without upstream_model", func(c *Catalog) { m := c.Models["deepseek-flash"]; m.UpstreamModel = ""; c.Models["deepseek-flash"] = m }, "upstream_model"},
		{"negative price", func(c *Catalog) { m := c.Models["deepseek-flash"]; m.InputPerMtok = -1; c.Models["deepseek-flash"] = m }, "price"},
		{"unknown default", func(c *Catalog) { c.DefaultModel = "ghost" }, "default_model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCatalog(testCatalogJSON)
			if err != nil {
				t.Fatalf("ParseCatalog: %v", err)
			}
			tc.mutate(c)
			err = c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCatalog_DefaultInferredFromSingleModel(t *testing.T) {
	raw := `{"providers":{"p":{"protocol":"openai","base_url":"https://x.example","api_keys":["k"]}},
		"models":{"only":{"provider":"p","upstream_model":"u"}}}`
	c, err := ParseCatalog(raw)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if c.DefaultModel != "only" {
		t.Errorf("DefaultModel = %q, want inferred \"only\"", c.DefaultModel)
	}
}

func TestLegacyCatalog(t *testing.T) {
	if c := LegacyCatalog("", "https://api.deepseek.com", "deepseek-chat"); c != nil {
		t.Errorf("LegacyCatalog with empty key = %v, want nil", c)
	}
	c := LegacyCatalog("sk-x", "https://api.deepseek.com", "deepseek-chat")
	if c == nil || c.DefaultModel != "deepseek-chat" {
		t.Fatalf("LegacyCatalog = %+v", c)
	}
	if _, _, ok := c.Resolve(""); !ok {
		t.Error("legacy catalog must resolve its single model")
	}
	if c.Providers["deepseek"].APIKeys[0] != "sk-x" {
		t.Errorf("legacy key not carried over: %+v", c.Providers["deepseek"])
	}
}
```

`internal/llm/keypool_test.go`:

```go
package llm

import (
	"testing"
	"time"
)

func TestKeyPool_RoundRobin(t *testing.T) {
	p := NewKeyPool([]string{"a", "b", "c"})
	now := time.Now()
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		_, k := p.Acquire(now)
		seen[k]++
	}
	for _, k := range []string{"a", "b", "c"} {
		if seen[k] != 1 {
			t.Errorf("key %q acquired %d times, want 1", k, seen[k])
		}
	}
}

func TestKeyPool_SkipsCooledKeys(t *testing.T) {
	p := NewKeyPool([]string{"a", "b"})
	now := time.Now()
	_, first := p.Acquire(now) // "a"
	p.Cool(0, now.Add(time.Minute))
	_, second := p.Acquire(now)
	if second != "b" || first != "a" {
		t.Errorf("acquire = %q then %q, want a then b", first, second)
	}
	// Cooldown expiry makes the key available again.
	_, third := p.Acquire(now.Add(2 * time.Minute))
	if third != "a" && third != "b" {
		t.Errorf("impossible key %q", third)
	}
}

func TestKeyPool_AllCooledFallsBackToSoonestExpiring(t *testing.T) {
	p := NewKeyPool([]string{"a", "b"})
	now := time.Now()
	p.Cool(0, now.Add(10*time.Second))
	p.Cool(1, now.Add(5*time.Second))
	idx, key := p.Acquire(now)
	if key != "b" || idx != 1 {
		t.Errorf("Acquire = (%d, %q), want (1, b) (soonest expiring cooldown)", idx, key)
	}
}

func TestKeyPool_SingleKey(t *testing.T) {
	p := NewKeyPool([]string{"only"})
	if p.Len() != 1 {
		t.Fatalf("Len = %d", p.Len())
	}
	p.Cool(0, time.Now().Add(time.Hour))
	if _, k := p.Acquire(time.Now()); k != "only" {
		t.Errorf("single key pool must always return its key, got %q", k)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/llm/ -v`
Expected: FAIL — `package github.com/yunhou/users/internal/llm: no Go files` (or undefined symbols).

- [ ] **Step 3: Implement `internal/llm/catalog.go`**

```go
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
```

`internal/llm/keypool.go`:

```go
package llm

import (
	"sync"
	"time"
)

// KeyCooldown is how long a key is skipped after the upstream rejects it
// with 429/5xx. One minute matches typical per-minute rate windows.
const KeyCooldown = 60 * time.Second

// KeyPool is a round-robin pool of interchangeable API keys for one
// provider, with per-key cooldown. It deliberately does NOT retry or sleep:
// the caller (ChatService) decides whether to try another key.
type KeyPool struct {
	mu          sync.Mutex
	keys        []string
	next        int
	cooledUntil map[int]time.Time
}

func NewKeyPool(keys []string) *KeyPool {
	return &KeyPool{keys: keys, cooledUntil: map[int]time.Time{}}
}

// Len reports the pool size (ChatService uses it to decide the retry budget).
func (p *KeyPool) Len() int { return len(p.keys) }

// Acquire returns the next non-cooled key in round-robin order. When every
// key is cooled down it returns the key whose cooldown expires soonest —
// an extra few seconds of 429 risk beats hard-failing the request.
func (p *KeyPool) Acquire(now time.Time) (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestUntil := -1, time.Time{}
	for i := 0; i < len(p.keys); i++ {
		idx := (p.next + i) % len(p.keys)
		until, cooled := p.cooledUntil[idx]
		if !cooled || !until.After(now) {
			p.next = idx + 1
			return idx, p.keys[idx]
		}
		if best == -1 || until.Before(bestUntil) {
			best, bestUntil = idx, until
		}
	}
	p.next = best + 1
	return best, p.keys[best]
}

// Cool marks key idx as unavailable until the given time. Out-of-range
// indexes are ignored (defensive; callers pass Acquire's return value).
func (p *KeyPool) Cool(idx int, until time.Time) {
	if idx < 0 || idx >= len(p.keys) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooledUntil[idx] = until
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/
git commit -m "feat(llm): provider/model catalog with validation and key pool"
```

---

### Task 2: `internal/llm` — OpenAI payload builder + stream usage extraction

**Files:**
- Create: `internal/llm/openai.go`
- Test: `internal/llm/openai_test.go`

**Interfaces:**
- Consumes: `model.ChatMessage`, `model.ToolCall` from `internal/model/chat.go` (read-only).
- Produces (used by Task 7 and Task 8):
  - `func BuildOpenAIPayload(upstreamModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error)`
  - `func ExtractStreamUsage(raw []byte) (inputTokens, outputTokens int, ok bool)`

- [ ] **Step 1: Write the failing test**

`internal/llm/openai_test.go`:

```go
package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestBuildOpenAIPayload_Minimal(t *testing.T) {
	body, err := BuildOpenAIPayload("deepseek-chat",
		[]model.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"model":"deepseek-chat"`, `"stream":true`, `"content":"hi"`, `"include_usage":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"tools"`) || strings.Contains(s, `"thinking"`) {
		t.Errorf("payload must not contain tools/thinking: %s", s)
	}
}

func TestBuildOpenAIPayload_ToolsAndThinking(t *testing.T) {
	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	body, err := BuildOpenAIPayload("m", []model.ChatMessage{{Role: "user", Content: "x"}}, tools, &thinking)
	if err != nil {
		t.Fatalf("BuildOpenAIPayload: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s: %s", want, s)
		}
	}
}

func TestExtractStreamUsage(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34,\"total_tokens\":46}}\n\n" +
		"data: [DONE]\n\n"
	in, out, ok := ExtractStreamUsage([]byte(raw))
	if !ok || in != 12 || out != 34 {
		t.Errorf("ExtractStreamUsage = (%d, %d, %v), want (12, 34, true)", in, out, ok)
	}
}

func TestExtractStreamUsage_Absent(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	if _, _, ok := ExtractStreamUsage([]byte(raw)); ok {
		t.Error("ok = true, want false when no usage chunk present")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/llm/ -run 'TestBuildOpenAI|TestExtractStreamUsage' -v`
Expected: FAIL — undefined: BuildOpenAIPayload / ExtractStreamUsage.

- [ ] **Step 3: Implement `internal/llm/openai.go`**

```go
package llm

import (
	"encoding/json"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// BuildOpenAIPayload builds the upstream chat.completions body for an
// OpenAI-compatible provider. stream_options.include_usage asks the upstream
// to end the stream with a usage chunk so token metering works — DeepSeek,
// Moonshot, GLM and MiniMax all honor it; providers that ignore it simply
// omit the chunk and the request is metered with zero tokens (the usage row
// is still written, so spend never goes unrecorded structurally).
func BuildOpenAIPayload(upstreamModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	payload := map[string]any{
		"model":          upstreamModel,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
	return json.Marshal(payload)
}

// ExtractStreamUsage scans a captured OpenAI-format SSE stream for the
// terminal usage chunk (choices: [], usage: {...}) and returns the token
// counts. The LAST usage chunk wins so cumulative-reporting providers are
// handled correctly. ok=false when no chunk carried usage.
func ExtractStreamUsage(raw []byte) (inputTokens, outputTokens int, ok bool) {
	for _, block := range strings.Split(string(raw), "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			inputTokens, outputTokens, ok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
		}
	}
	return inputTokens, outputTokens, ok
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/openai.go internal/llm/openai_test.go
git commit -m "feat(llm): OpenAI payload builder with usage chunk request and stream usage extraction"
```

---

### Task 3: `internal/llm` — Anthropic request translation

**Files:**
- Create: `internal/llm/anthropic.go`
- Test: `internal/llm/anthropic_test.go`

**Interfaces:**
- Consumes: `model.ChatMessage`, `model.ToolCall`; `ProtocolAnthropic` from Task 1.
- Produces (used by Task 7):
  - `func BuildAnthropicPayload(upstreamModel string, maxTokens int, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error)`
  - `const anthropicDefaultMaxTokens = 8192`, `const anthropicThinkingBudget = 4096` (unexported is fine)

Mapping rules (lock these with tests):
- `system` messages → top-level `system` string, joined with `\n\n` (Anthropic has no system role in messages).
- `user` → `{"role":"user","content":[{"type":"text","text":...}]}`.
- `assistant` → text block (when content non-empty) + one `tool_use` block per tool_call; `input` is the tool_call `arguments` JSON string parsed to an object (invalid JSON → `{}`). Assistant with empty content and no tool_calls is skipped (Anthropic rejects empty content blocks).
- `tool` → must become a `user` message whose content starts with `tool_result` blocks; consecutive tool messages merge into ONE user message (Anthropic requires tool_result blocks grouped at the start of a user turn).
- `tools` → accept both the OpenAI envelope `{"type":"function","function":{"name","description","parameters"}}` and an already-flat `{"name","description","input_schema"|"parameters"}`; emit `{"name","description","input_schema"}`.
- `thinking_enabled=true` → `"thinking":{"type":"enabled","budget_tokens":4096}` and `max_tokens` raised to at least 8192 (Anthropic requires max_tokens > budget_tokens).
- `max_tokens` is required by Anthropic: `maxTokens` arg, defaulting to 8192 when ≤ 0.
- Always `"stream": true`.

- [ ] **Step 1: Write the failing test**

`internal/llm/anthropic_test.go`:

```go
package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// decode is a test helper: unmarshal the payload into a generic map.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, body)
	}
	return m
}

func TestBuildAnthropicPayload_BasicAndSystem(t *testing.T) {
	body, err := BuildAnthropicPayload("kimi-k3", 0, []model.ChatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "system", Content: "answer in Chinese"},
		{Role: "user", Content: "hi"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	if p["model"] != "kimi-k3" {
		t.Errorf("model = %v", p["model"])
	}
	if p["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, want default 8192", p["max_tokens"])
	}
	if p["stream"] != true {
		t.Errorf("stream = %v", p["stream"])
	}
	if p["system"] != "be brief\n\nanswer in Chinese" {
		t.Errorf("system = %v", p["system"])
	}
	msgs := p["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1 (system hoisted)", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("role = %v", m0["role"])
	}
	content := m0["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hi" {
		t.Errorf("content = %v", content)
	}
}

func TestBuildAnthropicPayload_ToolLoop(t *testing.T) {
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.ToolCallFunction{Name: "run_shell", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", Content: "file_a\nfile_b", ToolCallID: "call_1"},
		{Role: "tool", Content: "done", ToolCallID: "call_2"},
		{Role: "assistant", Content: "here you go"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	msgs := p["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (two tool results merged into one user msg)", len(msgs))
	}
	// assistant turn: single tool_use block, input parsed from arguments string
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1].role = %v", asst["role"])
	}
	blocks := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %d, want 1 (empty text content skipped)", len(blocks))
	}
	tu := blocks[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "run_shell" {
		t.Errorf("tool_use block = %v", tu)
	}
	if tu["input"].(map[string]any)["cmd"] != "ls" {
		t.Errorf("tool_use input = %v", tu["input"])
	}
	// consecutive tool results merged into ONE user message
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool result must ride a user message, got %v", toolMsg["role"])
	}
	trs := toolMsg["content"].([]any)
	if len(trs) != 2 {
		t.Fatalf("merged tool_result blocks = %d, want 2", len(trs))
	}
	tr0 := trs[0].(map[string]any)
	if tr0["type"] != "tool_result" || tr0["tool_use_id"] != "call_1" || tr0["content"] != "file_a\nfile_b" {
		t.Errorf("tool_result = %v", tr0)
	}
}

func TestBuildAnthropicPayload_ToolsTranslation(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"run_shell","description":"run cmd","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}`),
		json.RawMessage(`{"name":"list_dir","input_schema":{"type":"object"}}`),
	}
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, tools, nil)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	ts := p["tools"].([]any)
	if len(ts) != 2 {
		t.Fatalf("tools len = %d", len(ts))
	}
	t0 := ts[0].(map[string]any)
	if t0["name"] != "run_shell" || t0["description"] != "run cmd" {
		t.Errorf("tool[0] = %v", t0)
	}
	schema := t0["input_schema"].(map[string]any)
	if schema["properties"] == nil {
		t.Errorf("tool[0] input_schema missing parameters passthrough: %v", schema)
	}
	t1 := ts[1].(map[string]any)
	if t1["name"] != "list_dir" || t1["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool[1] = %v", t1)
	}
	// OpenAI envelope keys must not leak through
	if strings.Contains(string(body), `"function"`) {
		t.Errorf("OpenAI tool envelope leaked into Anthropic payload: %s", body)
	}
}

func TestBuildAnthropicPayload_Thinking(t *testing.T) {
	thinking := true
	body, err := BuildAnthropicPayload("m", 0, []model.ChatMessage{{Role: "user", Content: "x"}}, nil, &thinking)
	if err != nil {
		t.Fatalf("BuildAnthropicPayload: %v", err)
	}
	p := decode(t, body)
	th := p["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(4096) {
		t.Errorf("thinking = %v", th)
	}
	if p["max_tokens"].(float64) <= th["budget_tokens"].(float64) {
		t.Errorf("max_tokens %v must exceed budget_tokens", p["max_tokens"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/llm/ -run TestBuildAnthropic -v`
Expected: FAIL — undefined: BuildAnthropicPayload.

- [ ] **Step 3: Implement `internal/llm/anthropic.go`**

```go
package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// anthropicDefaultMaxTokens is the floor for the Anthropic-required
// max_tokens field. anthropicThinkingBudget must stay strictly below it
// (Anthropic rejects max_tokens <= thinking.budget_tokens).
const (
	anthropicDefaultMaxTokens = 8192
	anthropicThinkingBudget   = 4096
)

// BuildAnthropicPayload translates the OpenAI-shaped chat request into an
// Anthropic Messages API body. Used for providers whose coding-plan endpoint
// speaks Anthropic protocol (Kimi for Coding, GLM Coding Plan, MiniMax
// /anthropic). The thinking flag maps to Anthropic's extended-thinking
// parameter with a fixed budget; the OpenAI "thinking":{"type":"enabled"}
// shape is never sent to an Anthropic endpoint.
func BuildAnthropicPayload(upstreamModel string, maxTokens int, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	var systemParts []string
	var msgs []map[string]any
	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": m.Content}},
			})
		case "assistant":
			var blocks []any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil || input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			// Anthropic rejects empty content arrays; an assistant turn that
			// carries neither text nor tool calls adds no information.
			if len(blocks) == 0 {
				continue
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}
			// Anthropic requires tool_result blocks at the start of a user
			// turn; consecutive OpenAI tool messages merge into one.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				if arr, ok := msgs[n-1]["content"].([]any); ok && len(arr) > 0 {
					if first, ok := arr[0].(map[string]any); ok && first["type"] == "tool_result" {
						msgs[n-1]["content"] = append(arr, block)
						continue
					}
				}
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{block}})
		default:
			return nil, fmt.Errorf("unsupported role %q for anthropic translation", m.Role)
		}
	}

	payload := map[string]any{
		"model":      upstreamModel,
		"max_tokens": maxTokens,
		"stream":     true,
		"messages":   msgs,
	}
	if len(systemParts) > 0 {
		payload["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(tools) > 0 {
		out := make([]any, 0, len(tools))
		for _, raw := range tools {
			var tool map[string]any
			if err := json.Unmarshal(raw, &tool); err != nil {
				return nil, fmt.Errorf("decode tool: %w", err)
			}
			// Accept both the OpenAI envelope ({"type":"function",
			// "function":{...}}) and an already-flat shape.
			fn := tool
			if f, ok := tool["function"].(map[string]any); ok {
				fn = f
			}
			entry := map[string]any{
				"name":         fn["name"],
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			}
			if d, ok := fn["description"]; ok {
				entry["description"] = d
			}
			if p, ok := fn["parameters"]; ok {
				entry["input_schema"] = p
			}
			if s, ok := fn["input_schema"]; ok {
				entry["input_schema"] = s
			}
			out = append(out, entry)
		}
		payload["tools"] = out
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		if maxTokens <= anthropicThinkingBudget {
			maxTokens = anthropicThinkingBudget + anthropicDefaultMaxTokens
			payload["max_tokens"] = maxTokens
		}
		payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": anthropicThinkingBudget}
	}
	return json.Marshal(payload)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/ -run TestBuildAnthropic -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/anthropic.go internal/llm/anthropic_test.go
git commit -m "feat(llm): Anthropic Messages request translation"
```

---

### Task 4: `internal/llm` — Anthropic SSE stream translation

**Files:**
- Create: `internal/llm/anthropic_stream.go`
- Test: `internal/llm/anthropic_stream_test.go`

**Interfaces:**
- Consumes: nothing beyond Task 1-3.
- Produces (used by Task 7):
  - `func TranslateAnthropicStream(body io.ReadCloser) io.ReadCloser` — returns a reader that yields OpenAI `chat.completion.chunk` SSE (`data: {...}\n\n` lines ending with a usage chunk and `data: [DONE]\n\n`). Closing it closes the pipe AND the underlying body.

Translation rules (lock with tests):
- `message_start` → emit chunk `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`; capture `message.usage.input_tokens`.
- `content_block_start` with `content_block.type == "tool_use"` → chunk with `delta.tool_calls: [{"index": <content_block index>, "id":..., "type":"function", "function":{"name":..., "arguments":""}}]`. (Anthropic block indexes may skip numbers when text blocks interleave; that's fine — OpenAI clients group by index, gaps are harmless.)
- `content_block_delta`: `text_delta` → `delta.content`; `thinking_delta` → `delta.reasoning_content` (DeepSeek-style reasoning field, what kaya already renders); `input_json_delta` → `delta.tool_calls:[{"index":..., "function":{"arguments": partial_json}}]`.
- `message_delta` → capture `usage.output_tokens` (cumulative: keep latest); when `delta.stop_reason` present emit the finish chunk with mapped reason: `end_turn`→`stop`, `tool_use`→`tool_calls`, `max_tokens`→`length`, anything else → `stop`.
- `message_stop` → emit usage chunk `{"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":I,"completion_tokens":O,"total_tokens":I+O}}` then `data: [DONE]\n\n`.
- `error` event → emit `{"error":{"message":"upstream anthropic error"}}` chunk (kaya parses the error key, same convention as the handler's injected upstream-broke event).
- `ping` and unknown events → skipped. Non-`data:` lines → skipped. Unparseable data JSON → skipped (keep the stream alive).
- Stream ending without `message_stop` (client cancel, upstream break): just EOF — the relay's existing semantics apply.

- [ ] **Step 1: Write the failing test**

`internal/llm/anthropic_stream_test.go`:

```go
package llm

import (
	"io"
	"strings"
	"testing"
)

const anthropicFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":25}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"，世界"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"run_shell"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":17}}

event: message_stop
data: {"type":"message_stop"}

`

func TestTranslateAnthropicStream_Full(t *testing.T) {
	src := io.NopCloser(strings.NewReader(anthropicFixture))
	out, err := io.ReadAll(TranslateAnthropicStream(src))
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"role":"assistant"`,     // message_start role chunk
		`"content":"你好"`,        // text delta
		`"content":"，世界"`,       // second text delta
		`"id":"toolu_1"`,         // tool_use start
		`"name":"run_shell"`,
		`"arguments":"{\"cmd\":"`, // first partial json
		`"arguments":"\"ls\"}"`,   // second partial json
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":25`,
		`"completion_tokens":17`,
		`"total_tokens":42`,
		"data: [DONE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("translated stream missing %s\n--- stream ---\n%s", want, s)
		}
	}
	// Must be OpenAI-chunk shaped: every data line except [DONE] parses as a
	// chat.completion.chunk object (usage chunk has empty choices).
	for _, block := range strings.Split(s, "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if !strings.HasPrefix(line, "data: {") {
			t.Errorf("non-chunk data line: %q", line)
		}
	}
}

func TestTranslateAnthropicStream_ThinkingDelta(t *testing.T) {
	src := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"hmm"`) {
		t.Errorf("thinking delta must map to reasoning_content: %s", out)
	}
}

func TestTranslateAnthropicStream_ErrorEvent(t *testing.T) {
	src := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"error"`) {
		t.Errorf("error event must surface as an error chunk: %s", out)
	}
}

func TestTranslateAnthropicStream_StopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"weird":      "stop",
	}
	for in, want := range cases {
		src := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + in + "\"},\"usage\":{\"output_tokens\":1}}\n\n"
		out, err := io.ReadAll(TranslateAnthropicStream(io.NopCloser(strings.NewReader(src))))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(out), `"finish_reason":"`+want+`"`) {
			t.Errorf("stop_reason %q → want %q in: %s", in, want, out)
		}
	}
}

func TestTranslateAnthropicStream_CloseClosesSource(t *testing.T) {
	closed := false
	src := &trackCloser{Reader: strings.NewReader(""), closed: &closed}
	r := TranslateAnthropicStream(src)
	_, _ = io.ReadAll(r)
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Error("closing the translated stream must close the underlying body")
	}
}

type trackCloser struct {
	*strings.Reader
	closed *bool
}

func (c *trackCloser) Close() error { *c.closed = true; return nil }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/llm/ -run TestTranslateAnthropic -v`
Expected: FAIL — undefined: TranslateAnthropicStream.

- [ ] **Step 3: Implement `internal/llm/anthropic_stream.go`**

```go
package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// TranslateAnthropicStream converts an Anthropic Messages SSE stream into
// the OpenAI chat.completion.chunk SSE shape, so the relay, the audit log
// and kaya see one protocol regardless of upstream. The translation runs in
// a goroutine feeding an io.Pipe; closing the returned reader stops the
// goroutine (pipe error) and closes the underlying upstream body.
func TranslateAnthropicStream(body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := translateAnthropicEvents(body, pw)
		// EOF with or without message_stop ends the pipe cleanly; a read
		// error propagates so the relay reports upstream-broken, matching
		// the OpenAI passthrough path's semantics.
		pw.CloseWithError(err)
	}()
	return &stackedReadCloser{r: pr, closers: []io.Closer{pr, body}}
}

// stackedReadCloser reads from r and closes every closer (pipe reader first,
// so the translator goroutine unblocks before the upstream body closes).
type stackedReadCloser struct {
	r       io.Reader
	closers []io.Closer
}

func (s *stackedReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *stackedReadCloser) Close() error {
	var first error
	for _, c := range s.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// anthropicEvent is the superset of the Anthropic Messages streaming event
// shapes we care about; unlisted fields are ignored.
type anthropicEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func translateAnthropicEvents(body io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(body)
	// Anthropic data lines carry full JSON events; 1 MiB covers pathological
	// tool_use inputs without unbounded allocation.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	inputTokens, outputTokens := 0, 0
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue // event:/comment/blank lines
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // unknown/malformed event — keep the stream alive
		}
		var err error
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				inputTokens = ev.Message.Usage.InputTokens
			}
			err = writeOpenAIChunk(w, map[string]any{"role": "assistant"}, "")
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.Index,
					"id":       ev.ContentBlock.ID,
					"type":     "function",
					"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
				}}}, "")
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				err = writeOpenAIChunk(w, map[string]any{"content": ev.Delta.Text}, "")
			case "thinking_delta":
				err = writeOpenAIChunk(w, map[string]any{"reasoning_content": ev.Delta.Thinking}, "")
			case "input_json_delta":
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.Index,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}}}, "")
			}
		case "message_delta":
			if ev.Usage != nil {
				outputTokens = ev.Usage.OutputTokens // cumulative — latest wins
			}
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				err = writeOpenAIChunk(w, map[string]any{}, mapAnthropicStopReason(ev.Delta.StopReason))
			}
		case "message_stop":
			err = writeOpenAIUsageAndDone(w, inputTokens, outputTokens)
		case "error":
			// kaya parses the {"error":...} chunk convention (same shape the
			// handler injects on upstream breaks).
			err = writeRawSSE(w, map[string]any{"error": map[string]any{"message": "upstream anthropic error"}})
		}
		if err != nil {
			return err // client (pipe reader) is gone
		}
	}
	return scanner.Err()
}

// writeOpenAIChunk emits one `data: {...}\n\n` chat.completion.chunk. Empty
// finish emits JSON null; non-empty emits the string.
func writeOpenAIChunk(w io.Writer, delta map[string]any, finish string) error {
	var finishReason any
	if finish != "" {
		finishReason = finish
	}
	return writeRawSSE(w, map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	})
}

// writeOpenAIUsageAndDone emits the terminal usage chunk (the shape
// ExtractStreamUsage looks for) followed by [DONE].
func writeOpenAIUsageAndDone(w io.Writer, inputTokens, outputTokens int) error {
	if err := writeRawSSE(w, map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func writeRawSSE(w io.Writer, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal translated chunk: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func mapAnthropicStopReason(r string) string {
	switch r {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/anthropic_stream.go internal/llm/anthropic_stream_test.go
git commit -m "feat(llm): Anthropic SSE stream translation to OpenAI chunks"
```

---

### Task 5: Migration 022 + LLM usage model & repo

**Files:**
- Create: `migrations/022_llm_usage_events.sql`
- Create: `internal/model/llm_usage.go`
- Create: `internal/repo/llm_usage_repo.go`
- Test: `internal/repo/llm_usage_repo_test.go`

**Interfaces:**
- Consumes: existing migration ledger (`internal/migrate`), `usage_events` conventions from `migrations/021_usage_events.sql`.
- Produces (used by Tasks 7, 9):
  - `type model.LLMUsageEvent struct { UserID string; AppID string; Model string; Provider string; UpstreamModel string; Status string; InputTokens int; OutputTokens int; CostMicros int64 }`
  - `type model.LLMUsageRow struct { Model string `db:"model" json:"model"`; Requests int `db:"requests" json:"requests"`; InputTokens int64 `db:"input_tokens" json:"input_tokens"`; OutputTokens int64 `db:"output_tokens" json:"output_tokens"`; CostMicros int64 `db:"cost_micros" json:"cost_micros"` }`
  - `type repo.LLMUsageRepo interface { InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error; SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) }`
  - `func repo.NewLLMUsageRepo(db *sqlx.DB) *llmUsageRepo`

- [ ] **Step 1: Write the migration**

`migrations/022_llm_usage_events.sql`:

```sql
-- 022_llm_usage_events.sql
-- Description: LLM 聊天计量流水表 — /chat 每完成一次上游调用落一行
-- 设计文档: docs/superpowers/plans/2026-09-07-multi-model-gateway.md
--
-- 口径:
--   - 一次 /chat 请求 = 一行;status 复用 handler 的 relay 结果
--     (ok / disconnected / upstream_error) —— 上游已 200 即计费,
--     断连/断流的已消耗 token 照常记录
--   - input_tokens/output_tokens 取自上游流末 usage 块(OpenAI
--     stream_options.include_usage;Anthropic 由翻译层合成);上游未上报
--     时记 0,行仍然落库(结构性不漏记)
--   - cost_micros = input_tokens*input_price_per_mtok
--     + output_tokens*output_price_per_mtok (微元,1e-6 CNY)
--
-- 隐私边界:不含消息内容(内容审计在 CHAT_LOG_PATH 的访问日志里,
-- 该日志可独立关闭);身份仅 user_id。
--
-- 幂等:不需要客户端幂等键 —— 每次真实上游调用都应计量,客户端重试
-- 产生的是第二次真实调用,两行都正确。

CREATE TABLE IF NOT EXISTS llm_usage_events (
    id             BIGSERIAL PRIMARY KEY,
    -- 删用户即删其计量数据,与 usage_events 一致
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id         TEXT NOT NULL REFERENCES apps(app_id),
    -- 逻辑模型 id(kaya 选的,如 deepseek-flash)+ 实际路由信息
    model          TEXT NOT NULL CHECK (length(model) <= 64),
    provider       TEXT NOT NULL CHECK (length(provider) <= 64),
    upstream_model TEXT NOT NULL CHECK (length(upstream_model) <= 128),
    status         TEXT NOT NULL CHECK (status IN ('ok', 'disconnected', 'upstream_error')),
    input_tokens   INT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens  INT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_micros    BIGINT NOT NULL DEFAULT 0 CHECK (cost_micros >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 单用户用量明细/额度核对
CREATE INDEX IF NOT EXISTS idx_llm_usage_events_user_created
    ON llm_usage_events (user_id, created_at);

-- 按时间+模型聚合(运营统计、成本核算)
CREATE INDEX IF NOT EXISTS idx_llm_usage_events_model_created
    ON llm_usage_events (model, created_at);
```

Apply it locally if a dev DB is available:

```bash
go run ./cmd/migrate   # respects DATABASE_URL
```

- [ ] **Step 2: Write the failing repo test**

`internal/repo/llm_usage_repo_test.go` (follows `usage_repo_test.go` conventions: real Postgres, `setupDB` skips when unavailable):

```go
package repo

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestLLMUsageRepo_InsertAndSum(t *testing.T) {
	db := setupDB(t)
	r := NewLLMUsageRepo(db)
	ctx := context.Background()
	uid := seedUsageUser(t, db)

	events := []model.LLMUsageEvent{
		{UserID: uid, AppID: usageTestApp, Model: "deepseek-flash", Provider: "deepseek", UpstreamModel: "deepseek-chat", Status: "ok", InputTokens: 100, OutputTokens: 50, CostMicros: 600},
		{UserID: uid, AppID: usageTestApp, Model: "deepseek-flash", Provider: "deepseek", UpstreamModel: "deepseek-chat", Status: "disconnected", InputTokens: 100, OutputTokens: 10, CostMicros: 280},
		{UserID: uid, AppID: usageTestApp, Model: "kimi-k3", Provider: "kimi", UpstreamModel: "kimi-k3-latest", Status: "ok", InputTokens: 200, OutputTokens: 80, CostMicros: 1040},
	}
	for _, ev := range events {
		if err := r.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	rows, err := r.SumByModel(ctx, "2000-01-01", "2100-01-01")
	if err != nil {
		t.Fatalf("SumByModel: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("SumByModel rows = %d, want 2: %+v", len(rows), rows)
	}
	byModel := map[string]model.LLMUsageRow{}
	for _, row := range rows {
		byModel[row.Model] = row
	}
	flash := byModel["deepseek-flash"]
	if flash.Requests != 2 || flash.InputTokens != 200 || flash.OutputTokens != 60 || flash.CostMicros != 880 {
		t.Errorf("deepseek-flash row = %+v", flash)
	}
	kimi := byModel["kimi-k3"]
	if kimi.Requests != 1 || kimi.CostMicros != 1040 {
		t.Errorf("kimi-k3 row = %+v", kimi)
	}
}

func TestLLMUsageRepo_SumByModelEmptyRange(t *testing.T) {
	db := setupDB(t)
	r := NewLLMUsageRepo(db)
	rows, err := r.SumByModel(context.Background(), "2000-01-01", "2000-01-02")
	if err != nil {
		t.Fatalf("SumByModel: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want empty", rows)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/repo/ -run TestLLMUsageRepo -v`
Expected: FAIL — undefined: NewLLMUsageRepo (and model.LLMUsageEvent).

- [ ] **Step 4: Implement**

`internal/model/llm_usage.go`:

```go
package model

// LLMUsageEvent is one metered /chat upstream call (migration 022). Status
// mirrors the handler's relay result: "ok" | "disconnected" |
// "upstream_error" — the row exists whenever the upstream returned 200, so
// partially-consumed tokens are still recorded.
type LLMUsageEvent struct {
	UserID       string
	AppID        string
	Model        string // logical model id the client picked
	Provider     string
	UpstreamModel string
	Status       string
	InputTokens  int
	OutputTokens int
	CostMicros   int64 // µ¥ = 1e-6 CNY, = tokens × price_per_mtok (see llm.Model)
}

// LLMUsageRow is the per-model aggregate returned by the admin stats
// endpoint (GET /admin/stats/llm-usage).
type LLMUsageRow struct {
	Model        string `db:"model" json:"model"`
	Requests     int    `db:"requests" json:"requests"`
	InputTokens  int64  `db:"input_tokens" json:"input_tokens"`
	OutputTokens int64  `db:"output_tokens" json:"output_tokens"`
	CostMicros   int64  `db:"cost_micros" json:"cost_micros"`
}
```

`internal/repo/llm_usage_repo.go`:

```go
package repo

import (
	"context"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// LLMUsageRepo is the storage surface for LLM token metering (migration
// 022): one insert per completed /chat upstream call, plus the per-model
// aggregate behind GET /admin/stats/llm-usage.
type LLMUsageRepo interface {
	// InsertEvent writes one metered chat call. No idempotency key: every
	// real upstream call is a real spend and must be counted, including
	// client retries (which produce a second real upstream call).
	InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error
	// SumByModel aggregates per logical model over the calendar range
	// [from, to] (YYYY-MM-DD, server timezone via created_at::date).
	SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error)
}

type llmUsageRepo struct{ db *sqlx.DB }

func NewLLMUsageRepo(db *sqlx.DB) *llmUsageRepo { return &llmUsageRepo{db: db} }

var _ LLMUsageRepo = (*llmUsageRepo)(nil)

func (r *llmUsageRepo) InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO llm_usage_events
		(user_id, app_id, model, provider, upstream_model, status, input_tokens, output_tokens, cost_micros)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ev.UserID, ev.AppID, ev.Model, ev.Provider, ev.UpstreamModel, ev.Status,
		ev.InputTokens, ev.OutputTokens, ev.CostMicros)
	return err
}

func (r *llmUsageRepo) SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	// Half-open range on raw TIMESTAMPTZ (sargable), same pattern as
	// usageRepo.CountNewUsers.
	const query = `SELECT model,
		COUNT(*) AS requests,
		COALESCE(SUM(input_tokens), 0)  AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cost_micros), 0)   AS cost_micros
		FROM llm_usage_events
		WHERE created_at >= $1::date AND created_at < $2::date + 1
		GROUP BY model ORDER BY cost_micros DESC`
	var rows []model.LLMUsageRow
	if err := r.db.SelectContext(ctx, &rows, query, from, to); err != nil {
		return nil, err
	}
	return rows, nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/repo/ -run TestLLMUsageRepo -v`
Expected: PASS (or SKIP when no Postgres — acceptable per setupDB convention).

- [ ] **Step 6: Commit**

```bash
git add migrations/022_llm_usage_events.sql internal/model/llm_usage.go internal/repo/llm_usage_repo.go internal/repo/llm_usage_repo_test.go
git commit -m "feat(llm): llm_usage_events metering table, model types, and repo"
```

---

### Task 6: Migration 023 + per-plan model allowlist on `model.Plan`

**Files:**
- Create: `migrations/023_plan_chat_models.sql`
- Modify: `internal/model/plan.go:10-25` (add field to `Plan`)

**Interfaces:**
- Consumes: `pq.StringArray` convention already used for `Plan.Apps`.
- Produces (used by Task 7): `Plan.ChatModels pq.StringArray \`db:"chat_models" json:"chat_models,omitempty"\`` — nil/empty = all catalog models allowed.

Notes for the implementer: all plan read queries use `SELECT * FROM plans` (`internal/repo/repo.go` FindAll/FindByID/FindByApp/FindByIDForShareTx), so the new column needs NO repo changes — sqlx maps it to the new struct field. INSERT/UPDATE statements name their columns explicitly and don't include `chat_models`, so existing writes keep NULL (= unrestricted). Admin editing of the allowlist is intentionally out of scope (SQL for now).

- [ ] **Step 1: Write the migration**

`migrations/023_plan_chat_models.sql`:

```sql
-- 023_plan_chat_models.sql
-- Description: plans.chat_models — 套餐级内置模型白名单
-- 设计文档: docs/superpowers/plans/2026-09-07-multi-model-gateway.md
--
-- NULL(默认) = 不限制,目录内所有模型可用;
-- 非空数组 = 仅列出的逻辑模型 id 可用(如 '{deepseek-flash,kimi-k3}')。
-- 运维初期通过 SQL 维护,后台管理界面后续另起。

ALTER TABLE plans ADD COLUMN IF NOT EXISTS chat_models TEXT[] NULL;

COMMENT ON COLUMN plans.chat_models IS '套餐可用的内置聊天模型 id 白名单;NULL = 不限制';
```

- [ ] **Step 2: Add the struct field + a compile-locking test**

Modify `internal/model/plan.go` — add after the `Apps` field:

```go
	Apps                      pq.StringArray `db:"apps" json:"apps"`
	// ChatModels gates which logical chat models (llm catalog ids) this
	// plan may use. NULL/empty = unrestricted. Backfilled as NULL by
	// migration 023; managed via SQL until an admin UI exists.
	ChatModels                pq.StringArray `db:"chat_models" json:"chat_models,omitempty"`
```

Add a repo integration test to `internal/repo/repo_test.go` (append; follows existing patterns in that file):

```go
func TestPlanRepo_ChatModelsRoundTrip(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	// plans SELECT * must map chat_models without error (column added by
	// migration 023); NULL → nil slice.
	plan, err := NewPlanRepo(db).FindByID(ctx, "free")
	if err != nil {
		t.Fatalf("FindByID(free): %v", err)
	}
	if len(plan.ChatModels) != 0 {
		t.Errorf("free.ChatModels = %v, want empty (NULL)", plan.ChatModels)
	}
	// Direct UPDATE (no repo support by design) then re-read.
	if _, err := db.ExecContext(ctx, `UPDATE plans SET chat_models = $1 WHERE id = 'free'`, pq.StringArray{"deepseek-flash"}); err != nil {
		t.Fatalf("set chat_models: %v", err)
	}
	plan, err = NewPlanRepo(db).FindByID(ctx, "free")
	if err != nil {
		t.Fatalf("FindByID(free) again: %v", err)
	}
	if len(plan.ChatModels) != 1 || plan.ChatModels[0] != "deepseek-flash" {
		t.Errorf("ChatModels = %v, want [deepseek-flash]", plan.ChatModels)
	}
}
```

- [ ] **Step 3: Run test to verify it passes**

Run: `go test ./internal/repo/ -run TestPlanRepo_ChatModelsRoundTrip -v && go build ./...`
Expected: PASS (or SKIP without Postgres).

- [ ] **Step 4: Commit**

```bash
git add migrations/023_plan_chat_models.sql internal/model/plan.go internal/repo/repo_test.go
git commit -m "feat(llm): per-plan chat model allowlist column"
```

---

### Task 7: ChatService refactor — catalog routing, key pool, protocol dispatch, metering

**Files:**
- Modify: `internal/service/errors.go` (add two sentinels)
- Rewrite: `internal/service/chat.go`
- Rewrite: `internal/service/chat_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces (used by Tasks 8-10):
  - `type ChatRoute struct { LogicalModel string; Provider string; Protocol string; UpstreamModel string; InputPerMtok float64; OutputPerMtok float64 }`
  - `type ChatModelInfo struct { ID string `json:"id"`; DisplayName string `json:"display_name"`; Provider string `json:"provider"`; Default bool `json:"default"` }`
  - `func NewChatService(catalog *llm.Catalog, subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, usageRepo repo.LLMUsageRepo) *ChatService` — nil catalog → chat disabled
  - `(s *ChatService) SetHTTPClient(c *http.Client)` (kept)
  - `func (s *ChatService) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error)`
  - `func (s *ChatService) RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int)` — never returns error (logs instead); nil usageRepo → no-op
  - `func (s *ChatService) AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error)`
  - New sentinels: `ErrChatUnknownModel` (handler → 400), `ErrChatModelNotAllowed` (handler → 403)

- [ ] **Step 1: Add the sentinels**

In `internal/service/errors.go`, extend the chat block:

```go
	// Chat proxy (POST /chat → LLM upstream, SSE).
	ErrChatNotEnabled       = errors.New("chat is not enabled")
	ErrChatNoAccess         = errors.New("active subscription with access to this app is required")
	ErrChatUnknownModel     = errors.New("unknown chat model")
	ErrChatModelNotAllowed  = errors.New("chat model is not allowed for the current plan")
	ErrChatRateLimited      = errors.New("chat upstream rate limit exceeded")
	ErrChatUpstreamError    = errors.New("chat upstream error")
	ErrChatUpstreamRejected = errors.New("chat request rejected by upstream")
```

- [ ] **Step 2: Rewrite the service tests first (they define the contract)**

Rewrite `internal/service/chat_test.go` completely:

```go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/model"
)

// mockLLMUsageRepo records InsertEvent calls for assertions.
type mockLLMUsageRepo struct {
	events []model.LLMUsageEvent
	err    error
}

func (m *mockLLMUsageRepo) InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error {
	if m.err != nil {
		return m.err
	}
	m.events = append(m.events, ev)
	return nil
}

func (m *mockLLMUsageRepo) SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	return nil, nil
}

// testCatalog builds a single-provider catalog pointing at baseURL
// (typically an httptest server).
func testCatalog(baseURL string) *llm.Catalog {
	return &llm.Catalog{
		DefaultModel: "deepseek-flash",
		Providers: map[string]llm.Provider{
			"deepseek": {Protocol: llm.ProtocolOpenAI, BaseURL: baseURL, APIKeys: []string{"test-key"}},
		},
		Models: map[string]llm.Model{
			"deepseek-flash": {Provider: "deepseek", UpstreamModel: "deepseek-v4-flash", InputPerMtok: 2, OutputPerMtok: 8},
		},
	}
}

// chatTestFixture wires a ChatService with mock repos and an optional
// upstream stub.
func chatTestFixture(t *testing.T, upstream http.Handler) (*ChatService, *mockSubscriptionRepo, *mockPlanRepo, *mockLLMUsageRepo) {
	t.Helper()
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	usageRepo := &mockLLMUsageRepo{}
	baseURL := "https://upstream.invalid"
	var srv *httptest.Server
	if upstream != nil {
		srv = httptest.NewServer(upstream)
		t.Cleanup(srv.Close)
		baseURL = srv.URL
	}
	svc := NewChatService(testCatalog(baseURL), subRepo, planRepo, usageRepo)
	return svc, subRepo, planRepo, usageRepo
}

// seedChatActiveSub adds an active, non-expired subscription for userID
// pointing at planID.
func seedChatActiveSub(repo *mockSubscriptionRepo, userID, planID string) {
	now := time.Now()
	future := now.Add(30 * 24 * time.Hour)
	repo.byUserID[userID] = &model.Subscription{
		ID:        "sub-" + userID,
		UserID:    userID,
		PlanID:    planID,
		Status:    "active",
		StartedAt: now,
		ExpiresAt: &future,
	}
}

func chatMessages() []model.ChatMessage {
	return []model.ChatMessage{{Role: "user", Content: "hi"}}
}

func TestChatService_NotEnabled(t *testing.T) {
	svc := NewChatService(nil, newMockSubscriptionRepo(), newMockPlanRepo(), &mockLLMUsageRepo{})
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want ErrChatNotEnabled", err)
	}
}

func TestChatService_UnknownModel(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "ghost-model", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatUnknownModel) {
		t.Fatalf("err = %v, want ErrChatUnknownModel", err)
	}
}

func TestChatService_ModelNotInPlanAllowlist(t *testing.T) {
	svc, subRepo, planRepo, _ := chatTestFixture(t, nil)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}, ChatModels: pq.StringArray{"kimi-k3"}}
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "deepseek-flash", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatModelNotAllowed) {
		t.Fatalf("err = %v, want ErrChatModelNotAllowed", err)
	}
	// Same plan, allowed model passes the gate (fails later at the
	// unreachable upstream, which proves the gate didn't block it).
	planRepo.plans["monthly"].ChatModels = pq.StringArray{"deepseek-flash"}
	_, _, err = svc.StreamChat(context.Background(), "u-1", "yunhou-website", "deepseek-flash", chatMessages(), nil, nil)
	if errors.Is(err, ErrChatModelNotAllowed) || errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("err = %v, want upstream-level error (gate passed)", err)
	}
}

func TestChatService_AccessGating(t *testing.T) {
	now := time.Now()
	activePlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	inactivePlan := &model.Plan{ID: "retired", IsActive: false, Apps: pq.StringArray{"yunhou-website"}}
	wrongAppPlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}

	cases := []struct {
		name    string
		subRepo *mockSubscriptionRepo
		plan    *model.Plan
		wantErr error
	}{
		{"no subscription", newMockSubscriptionRepo(), activePlan, ErrChatNoAccess},
		{"expired subscription", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			expiredAt := now.Add(-1 * time.Hour)
			r.byUserID["u-1"] = &model.Subscription{ID: "s1", UserID: "u-1", PlanID: "monthly", Status: "active", StartedAt: now.Add(-60 * 24 * time.Hour), ExpiresAt: &expiredAt}
			return r
		}(), activePlan, ErrChatNoAccess},
		{"plan inactive", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "retired")
			return r
		}(), inactivePlan, ErrChatNoAccess},
		{"plan lacks app", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "monthly")
			return r
		}(), wrongAppPlan, ErrChatNoAccess},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planRepo := newMockPlanRepo()
			planRepo.plans[tc.plan.ID] = tc.plan
			svc := NewChatService(testCatalog("https://upstream.invalid"), tc.subRepo, planRepo, &mockLLMUsageRepo{})
			_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestChatService_RepoError(t *testing.T) {
	subRepo := newMockSubscriptionRepo()
	subRepo.findErr = errors.New("db down")
	svc := NewChatService(testCatalog("https://upstream.invalid"), subRepo, newMockPlanRepo(), &mockLLMUsageRepo{})
	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err == nil || errors.Is(err, ErrChatNoAccess) || errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want a wrapped repo error", err)
	}
}

func TestChatService_StreamSuccess(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	var gotAuth string
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "",
		[]model.ChatMessage{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want Bearer test-key", gotAuth)
	}
	if route == nil || route.LogicalModel != "deepseek-flash" || route.Provider != "deepseek" ||
		route.Protocol != llm.ProtocolOpenAI || route.UpstreamModel != "deepseek-v4-flash" {
		t.Errorf("route = %+v", route)
	}
	body := string(gotBody)
	for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`, `"include_usage":true`, `"content":"hi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream body missing %s: %s", want, body)
		}
	}
	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(streamed) != sse {
		t.Errorf("stream = %q, want verbatim %q", streamed, sse)
	}
}

func TestChatService_ToolsAndThinkingRelay(t *testing.T) {
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	})

	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	thinking := true
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"run_shell"}}`)}
	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), tools, &thinking)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	body := string(gotBody)
	for _, want := range []string{`"tools"`, `run_shell`, `"thinking":{"type":"enabled"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream body missing %s: %s", want, body)
		}
	}
}

// TestChatService_KeyPoolFailover: a 429 from key A must cool A and retry
// on key B, succeeding transparently.
func TestChatService_KeyPoolFailover(t *testing.T) {
	var keyAuths []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyAuths = append(keyAuths, r.Header.Get("Authorization"))
		if len(keyAuths) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	})
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "m",
		Providers:    map[string]llm.Provider{"p": {Protocol: llm.ProtocolOpenAI, BaseURL: srv.URL, APIKeys: []string{"key-a", "key-b"}}},
		Models:       map[string]llm.Model{"m": {Provider: "p", UpstreamModel: "u"}},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	resp, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "m", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v (want transparent failover)", err)
	}
	resp.Body.Close()
	if len(keyAuths) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failover)", len(keyAuths))
	}
	if keyAuths[0] == keyAuths[1] {
		t.Errorf("failover must use the other key: %v", keyAuths)
	}
}

// TestChatService_SingleKey429NoRetry: with one key, a 429 maps to
// ErrChatRateLimited without a retry.
func TestChatService_SingleKey429NoRetry(t *testing.T) {
	var calls int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if !errors.Is(err, ErrChatRateLimited) {
		t.Fatalf("err = %v, want ErrChatRateLimited", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry with a single key)", calls)
	}
}

func TestChatService_UpstreamErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"rate limited", http.StatusTooManyRequests, ErrChatRateLimited},
		{"upstream 500", http.StatusInternalServerError, ErrChatUpstreamError},
		{"upstream 400", http.StatusBadRequest, ErrChatUpstreamRejected},
		{"upstream 401", http.StatusUnauthorized, ErrChatUpstreamRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":"boom"}`))
			})
			svc, subRepo, planRepo, _ := chatTestFixture(t, upstream)
			seedChatActiveSub(subRepo, "u-1", "monthly")
			planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

			_, _, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestChatService_AnthropicRoute: an anthropic-protocol provider gets the
// translated request at /v1/messages with the anthropic headers, and its SSE
// stream comes back translated to OpenAI chunks.
func TestChatService_AnthropicRoute(t *testing.T) {
	anthropicSSE := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var gotPath, gotAPIKey, gotVersion, gotBody string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(anthropicSSE))
	})
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	catalog := &llm.Catalog{
		DefaultModel: "kimi-k3",
		Providers:    map[string]llm.Provider{"kimi": {Protocol: llm.ProtocolAnthropic, BaseURL: srv.URL, APIKeys: []string{"sk-kimi-1"}}},
		Models:       map[string]llm.Model{"kimi-k3": {Provider: "kimi", UpstreamModel: "kimi-k3-latest"}},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	resp, route, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", "", chatMessages(), nil, nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-kimi-1" || gotVersion == "" {
		t.Errorf("anthropic headers: x-api-key=%q anthropic-version=%q", gotAPIKey, gotVersion)
	}
	if !strings.Contains(gotBody, `"max_tokens"`) || strings.Contains(gotBody, `"stream_options"`) {
		t.Errorf("anthropic payload wrong shape: %s", gotBody)
	}
	if route.Protocol != llm.ProtocolAnthropic {
		t.Errorf("route.Protocol = %q", route.Protocol)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	for _, want := range []string{`"content":"hi"`, `"prompt_tokens":9`, `"completion_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("translated stream missing %s: %s", want, s)
		}
	}
}

func TestChatService_RecordUsage(t *testing.T) {
	usageRepo := &mockLLMUsageRepo{}
	svc := NewChatService(testCatalog("https://upstream.invalid"), newMockSubscriptionRepo(), newMockPlanRepo(), usageRepo)
	route := &ChatRoute{LogicalModel: "deepseek-flash", Provider: "deepseek", Protocol: llm.ProtocolOpenAI, UpstreamModel: "deepseek-v4-flash", InputPerMtok: 2, OutputPerMtok: 8}
	// cost = 100*2 + 50*8 = 600 µ¥ (see llm.Model price identity)
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 100, 50)
	if len(usageRepo.events) != 1 {
		t.Fatalf("events = %d, want 1", len(usageRepo.events))
	}
	ev := usageRepo.events[0]
	if ev.UserID != "u-1" || ev.Model != "deepseek-flash" || ev.Provider != "deepseek" ||
		ev.UpstreamModel != "deepseek-v4-flash" || ev.Status != "ok" ||
		ev.InputTokens != 100 || ev.OutputTokens != 50 || ev.CostMicros != 600 {
		t.Errorf("event = %+v", ev)
	}

	// Repo error must be swallowed (metering never breaks chat).
	usageRepo.err = errors.New("db down")
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 1, 1)

	// nil route / nil usageRepo must not panic.
	svc.RecordUsage(context.Background(), "u-1", "yunhou-website", nil, "ok", 1, 1)
	svcNoRepo := NewChatService(testCatalog("https://upstream.invalid"), newMockSubscriptionRepo(), newMockPlanRepo(), nil)
	svcNoRepo.RecordUsage(context.Background(), "u-1", "yunhou-website", route, "ok", 1, 1)
}

func TestChatService_AllowedModels(t *testing.T) {
	catalog := &llm.Catalog{
		DefaultModel: "deepseek-flash",
		Providers: map[string]llm.Provider{
			"deepseek": {Protocol: llm.ProtocolOpenAI, BaseURL: "https://api.deepseek.com", APIKeys: []string{"k"}},
			"kimi":     {Protocol: llm.ProtocolAnthropic, BaseURL: "https://api.kimi.com/coding", APIKeys: []string{"k"}},
		},
		Models: map[string]llm.Model{
			"deepseek-flash": {Provider: "deepseek", UpstreamModel: "deepseek-chat", DisplayName: "DeepSeek Flash"},
			"kimi-k3":        {Provider: "kimi", UpstreamModel: "kimi-k3-latest", DisplayName: "Kimi K3"},
		},
	}
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	seedChatActiveSub(subRepo, "u-1", "monthly")
	svc := NewChatService(catalog, subRepo, planRepo, &mockLLMUsageRepo{})

	// Unrestricted plan → all models, default flagged.
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	models, err := svc.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if err != nil {
		t.Fatalf("AllowedModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	byID := map[string]ChatModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if !byID["deepseek-flash"].Default || byID["deepseek-flash"].DisplayName != "DeepSeek Flash" {
		t.Errorf("deepseek-flash info = %+v", byID["deepseek-flash"])
	}
	if byID["kimi-k3"].Default {
		t.Errorf("kimi-k3 must not be default")
	}

	// Allowlisted plan → filtered.
	planRepo.plans["monthly"].ChatModels = pq.StringArray{"kimi-k3"}
	models, err = svc.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if err != nil {
		t.Fatalf("AllowedModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "kimi-k3" {
		t.Errorf("models = %+v, want only kimi-k3", models)
	}

	// No subscription → ErrChatNoAccess.
	_, err = svc.AllowedModels(context.Background(), "u-nope", "yunhou-website")
	if !errors.Is(err, ErrChatNoAccess) {
		t.Errorf("err = %v, want ErrChatNoAccess", err)
	}
}
```

- [ ] **Step 3: Run service tests to verify they fail to compile**

Run: `go test ./internal/service/ -run TestChatService -v`
Expected: build failure (NewChatService signature, StreamChat returns, undefined mocks).

- [ ] **Step 4: Rewrite `internal/service/chat.go`**

Full new content:

```go
package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// chatUpstreamTimeout bounds one chat request end-to-end (headers + SSE
// stream). Long enough for a full streamed answer; short enough that a hung
// upstream can't pin a connection forever. The context is bound to the
// upstream connection, so the client disconnecting (gin request ctx cancel)
// also tears the stream down at the transport level.
const chatUpstreamTimeout = 5 * time.Minute

// chatAccessTimeout bounds the pre-upstream phase (subscription + plan DB
// reads). /chat skips the global 20s timeoutMiddleware so the SSE stream can
// run long — but that exemption also leaves these two DB calls without a
// server-side deadline, so a hung Postgres would pin the connection until
// the client gives up.
const chatAccessTimeout = 10 * time.Second

// chatUpstreamErrorBodyCap limits how much of an upstream error body we
// read before discarding — error payloads can be huge and are only used for
// logging.
const chatUpstreamErrorBodyCap = 8 << 10

// ChatRoute describes where one chat request was actually sent. The handler
// uses it for usage metering and the audit log; it is nil on error.
type ChatRoute struct {
	LogicalModel  string // catalog id the client picked (or the default)
	Provider      string
	Protocol      string // llm.ProtocolOpenAI | llm.ProtocolAnthropic
	UpstreamModel string
	InputPerMtok  float64
	OutputPerMtok float64
}

// ChatModelInfo is the public view of one catalog model, returned by
// AllowedModels and served at GET /chat/models for kaya's model picker.
type ChatModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	Default     bool   `json:"default"`
}

// ChatService routes POST /chat to the upstream LLM provider selected by the
// request's logical model id. All provider API keys live server-side (the
// llm.Catalog); consumer apps (kaya etc.) never see them — they authenticate
// with a user JWT and the server checks subscription-based access before
// spending upstream tokens. Downstream of this service everything speaks
// OpenAI chat.completion.chunk SSE; Anthropic-protocol providers are
// translated at the boundary (llm.TranslateAnthropicStream).
type ChatService struct {
	catalog    *llm.Catalog // nil = chat disabled (ErrChatNotEnabled)
	pools      map[string]*llm.KeyPool
	subRepo    repo.SubscriptionRepo
	planRepo   repo.PlanRepo
	usageRepo  repo.LLMUsageRepo // nil = metering disabled
	httpClient *http.Client
}

// NewChatService builds the multi-model chat router. catalog nil → every
// call returns ErrChatNotEnabled (handler maps to 404), mirroring the
// empty-webhook-secret convention for disabled channels.
func NewChatService(catalog *llm.Catalog, subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, usageRepo repo.LLMUsageRepo) *ChatService {
	s := &ChatService{
		catalog:   catalog,
		pools:     map[string]*llm.KeyPool{},
		subRepo:   subRepo,
		planRepo:  planRepo,
		usageRepo: usageRepo,
		// No client-level Timeout: the SSE stream length is bounded by ctx
		// (chatUpstreamTimeout). But the transport gets explicit dial and
		// response-header deadlines so a silently-hung upstream fails in
		// seconds instead of pinning the connection until the 5m ctx fires.
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
	if catalog != nil {
		for name, p := range catalog.Providers {
			s.pools[name] = llm.NewKeyPool(p.APIKeys)
		}
	}
	return s
}

// SetHTTPClient overrides the HTTP client. Tests inject httptest servers here.
func (s *ChatService) SetHTTPClient(c *http.Client) {
	s.httpClient = c
}

// StreamChat resolves the logical model, checks the caller's subscription
// access (including the plan's model allowlist), then opens a streaming
// request upstream. On success the returned *http.Response carries an
// OpenAI-shaped SSE stream (Anthropic upstreams are translated) and the
// caller owns closing Body. The response body is bound to ctx: cancelling
// ctx (client disconnect) closes the upstream connection and fails the read.
//
// The access decision mirrors resolvePlanForTokenIssuanceWithPlan: an active
// subscription whose plan is active, whose apps include appID, and whose
// chat_models (when non-NULL) include the resolved model.
func (s *ChatService) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error) {
	if s.catalog == nil {
		return nil, nil, ErrChatNotEnabled
	}
	resolvedID, m, ok := s.catalog.Resolve(logicalModel)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrChatUnknownModel, logicalModel)
	}
	provider := s.catalog.Providers[m.Provider]
	route := &ChatRoute{
		LogicalModel:  resolvedID,
		Provider:      m.Provider,
		Protocol:      provider.Protocol,
		UpstreamModel: m.UpstreamModel,
		InputPerMtok:  m.InputPerMtok,
		OutputPerMtok: m.OutputPerMtok,
	}

	// Bound the gate separately from the stream: /chat is exempt from the
	// global request timeout (see chatAccessTimeout), so without this a hung
	// DB would hold the request open indefinitely.
	accessCtx, accessCancel := context.WithTimeout(ctx, chatAccessTimeout)
	accessErr := s.checkAccess(accessCtx, userID, appID, resolvedID, time.Now())
	accessCancel()
	if accessErr != nil {
		return nil, nil, accessErr
	}

	var body []byte
	var err error
	switch provider.Protocol {
	case llm.ProtocolAnthropic:
		body, err = llm.BuildAnthropicPayload(m.UpstreamModel, m.MaxTokens, messages, tools, thinkingEnabled)
	default:
		body, err = llm.BuildOpenAIPayload(m.UpstreamModel, messages, tools, thinkingEnabled)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("encode chat request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, chatUpstreamTimeout)
	// NOTE: no defer cancel() here. The context is deliberately bound to the
	// response body's lifetime instead: the transport's readLoop watches
	// reqCtx.Done() and tears down the upstream connection on cancel, so
	// cancelling at function return would cut the SSE stream before the
	// caller (handler) has read anything. cancel fires via the
	// cancelOnCloseBody wrapper on Close, or via the timeout timer if the
	// body is never closed.

	// Retry budget: with multiple keys, one 429/5xx retries on another key
	// (the rejected key is cooled). A single-key provider never retries.
	pool := s.pools[m.Provider]
	attempts := 1
	if pool.Len() > 1 {
		attempts = 2
	}

	for attempt := 0; attempt < attempts; attempt++ {
		keyIdx, key := pool.Acquire(time.Now())
		resp, err := s.doUpstream(reqCtx, provider, key, body)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("%w: %v", ErrChatUpstreamError, err)
		}
		if resp.StatusCode == http.StatusOK {
			if provider.Protocol == llm.ProtocolAnthropic {
				resp.Body = llm.TranslateAnthropicStream(resp.Body)
			}
			// Bind cancel to the body's lifetime (see NOTE above).
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, route, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, chatUpstreamErrorBodyCap))
		resp.Body.Close()
		// 429/5xx may be a per-key quota or a transient provider issue: cool
		// the key and, if another key exists, retry once transparently.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			pool.Cool(keyIdx, time.Now().Add(llm.KeyCooldown))
			if attempt+1 < attempts {
				continue
			}
		}
		cancel()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatRateLimited, resp.StatusCode, errBody)
		}
		// Upstream 4xx (other than 429) rejects the request itself — a
		// permanent error that retrying will never fix.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamRejected, resp.StatusCode, errBody)
		}
		return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamError, resp.StatusCode, errBody)
	}
	// Unreachable: the loop's last attempt always returns. Kept so the
	// compiler sees a terminating statement.
	cancel()
	return nil, nil, ErrChatUpstreamError
}

// doUpstream performs one HTTP call against the provider. Path and auth
// headers follow the protocol: OpenAI providers get POST
// {base_url}/chat/completions with Bearer auth; Anthropic providers get POST
// {base_url}/v1/messages with both Bearer and x-api-key (Kimi/GLM-style
// endpoints accept Bearer, stock Anthropic wants x-api-key) plus the
// required anthropic-version header. Provider.Headers are applied last so an
// operator can override any default.
func (s *ChatService) doUpstream(ctx context.Context, p llm.Provider, apiKey string, body []byte) (*http.Response, error) {
	path := "/chat/completions"
	if p.Protocol == llm.ProtocolAnthropic {
		path = "/v1/messages"
	}
	// TrimSuffix: an operator-set base_url with a trailing slash would
	// otherwise produce "...//chat/completions" and a confusing upstream 404.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(p.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if p.Protocol == llm.ProtocolAnthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	return s.httpClient.Do(req)
}

// RecordUsage meters one completed upstream call. Metering must never break
// the chat path: insert errors are logged and swallowed. route nil (or a nil
// usageRepo) is a no-op. Cost identity: µ¥ = tokens × price_per_mtok.
func (s *ChatService) RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int) {
	if s.usageRepo == nil || route == nil {
		return
	}
	ev := model.LLMUsageEvent{
		UserID:        userID,
		AppID:         appID,
		Model:         route.LogicalModel,
		Provider:      route.Provider,
		UpstreamModel: route.UpstreamModel,
		Status:        status,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		CostMicros:    int64(float64(inputTokens)*route.InputPerMtok + float64(outputTokens)*route.OutputPerMtok),
	}
	if err := s.usageRepo.InsertEvent(ctx, ev); err != nil {
		log.Printf("chat: record usage (user=%s model=%s): %v", userID, route.LogicalModel, err)
	}
}

// AllowedModels returns the catalog models the caller's plan may use, for
// GET /chat/models (kaya's model picker). The subscription gate is the same
// as StreamChat's, minus the per-model check.
func (s *ChatService) AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error) {
	if s.catalog == nil {
		return nil, ErrChatNotEnabled
	}
	accessCtx, accessCancel := context.WithTimeout(ctx, chatAccessTimeout)
	plan, err := s.accessPlan(accessCtx, userID, appID, time.Now())
	accessCancel()
	if err != nil {
		return nil, err
	}
	allowed := func(id string) bool {
		return len(plan.ChatModels) == 0 || slices.Contains(plan.ChatModels, id)
	}
	out := make([]ChatModelInfo, 0, len(s.catalog.Models))
	for _, id := range s.catalog.ModelIDs() {
		if !allowed(id) {
			continue
		}
		m := s.catalog.Models[id]
		name := m.DisplayName
		if name == "" {
			name = id
		}
		out = append(out, ChatModelInfo{ID: id, DisplayName: name, Provider: m.Provider, Default: id == s.catalog.DefaultModel})
	}
	return out, nil
}

// cancelOnCloseBody runs cancel exactly once, when Close is called. The
// context timeout still fires on its own if the body is never closed.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// accessPlan is the shared subscription gate: active subscription, not
// expired, plan active, plan.apps contains appID. It returns the plan so
// callers can apply model-allowlist checks on top.
func (s *ChatService) accessPlan(ctx context.Context, userID, appID string, now time.Time) (*model.Plan, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChatNoAccess
		}
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	if sub == nil {
		return nil, ErrChatNoAccess
	}
	// NULL expires_at means "never expires" (pre-2026-07-27 rows); a
	// non-nil past expiry is an expired subscription.
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(now) {
		return nil, ErrChatNoAccess
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return nil, fmt.Errorf("get plan: %w", err)
	}
	if !plan.IsActive || !slices.Contains(plan.Apps, appID) {
		return nil, ErrChatNoAccess
	}
	return plan, nil
}

// checkAccess is the subscription gate plus the per-plan model allowlist
// (plans.chat_models; NULL/empty = unrestricted).
func (s *ChatService) checkAccess(ctx context.Context, userID, appID, logicalModel string, now time.Time) error {
	plan, err := s.accessPlan(ctx, userID, appID, now)
	if err != nil {
		return err
	}
	if len(plan.ChatModels) > 0 && !slices.Contains(plan.ChatModels, logicalModel) {
		return ErrChatModelNotAllowed
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/service/ -v && go build ./...`
Expected: chat tests PASS. Note: `internal/handler` will NOT compile yet (its `chatStreamer` interface still has the old signature) — that is Task 8; run service tests only: `go test ./internal/service/ ./internal/llm/`. Expect `go build ./internal/handler` to fail at this point; that's expected and resolved in Task 8.

- [ ] **Step 6: Commit**

```bash
git add internal/service/chat.go internal/service/chat_test.go internal/service/errors.go
git commit -m "feat(chat): catalog-driven multi-model routing with key pool failover and usage metering"
```

---

### Task 8: Handler — model field, post-relay usage recording, GET /chat/models

**Files:**
- Modify: `internal/model/chat.go` (add `Model` field + bound constant)
- Modify: `internal/handler/chat.go`
- Modify: `internal/handler/chat_test.go` (update mocks to the new interface)
- Modify: `internal/router/router.go:133-134` (register GET /chat/models)

**Interfaces:**
- Consumes: Task 7's `ChatRoute`, `ChatModelInfo`, `RecordUsage`, `AllowedModels`; Task 2's `llm.ExtractStreamUsage`.
- Produces (used by Task 10 wiring): `NewChatHandler(svc chatStreamer, accessLog *log.Logger)` — signature unchanged; the interface inside the package changes.

- [ ] **Step 1: Update `internal/model/chat.go`**

Add to `ChatRequest` (after `SessionID`):

```go
	// Model is the logical built-in model id (see GET /chat/models). Empty
	// selects the server-configured default — pre-multi-model clients never
	// send it and keep working unchanged.
	Model string `json:"model,omitempty"`
```

Add the bound constant near `ChatMaxSessionIDLen`:

```go
// ChatMaxModelLen bounds the optional model id — it must match a catalog
// entry, and catalog ids are themselves capped at 64 chars (llm.Validate).
const ChatMaxModelLen = 64
```

- [ ] **Step 2: Update `internal/handler/chat.go`**

1. Update the `chatStreamer` interface (top of file):

```go
// chatStreamer is the ChatService surface the handler needs. Defined as a
// local interface so handler tests can inject a hand-rolled mock without a
// real upstream.
type chatStreamer interface {
	StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error)
	// RecordUsage meters one completed upstream call; it never fails the
	// request (the service swallows and logs repo errors).
	RecordUsage(ctx context.Context, userID, appID string, route *service.ChatRoute, status string, inputTokens, outputTokens int)
	// AllowedModels backs GET /chat/models.
	AllowedModels(ctx context.Context, userID, appID string) ([]service.ChatModelInfo, error)
}
```

2. Add `Model` to the audit entry struct:

```go
	Model           string              `json:"model,omitempty"`
```

and change `logAccess` to accept it — new signature:

```go
func (h *ChatHandler) logAccess(started time.Time, userID, appID, modelID string, req model.ChatRequest, status, errMsg, output string)
```

Set `Model: modelID` in the entry. Update ALL existing call sites (the validation-failure paths pass `""` for modelID... no — pass `req.Model`; validation failures happen before resolution, so `req.Model` is the raw client value, which is what the audit should show; cap is enforced before use).

3. In `StreamChat`, after the session-id check, add the model bound check:

```go
	if len(req.Model) > model.ChatMaxModelLen {
		h.logAccess(started, userID, appID, req.Model, req, "error", "model id too long", "")
		writeChatError(c, http.StatusBadRequest, "model id too long")
		return
	}
```

4. Replace the service call and post-relay block:

```go
	resp, route, err := h.svc.StreamChat(c.Request.Context(), userID, appID, req.Model, req.Messages, req.Tools, req.ThinkingEnabled)
	if err != nil {
		status, msg := chatErrorMapping(err)
		h.logAccess(started, userID, appID, req.Model, req, "error", msg, "")
		writeChatError(c, status, msg)
		return
	}
	defer resp.Body.Close()
```

and after the relay, once `status` is computed, record usage before `logAccess`:

```go
	// Meter the upstream spend. Usage tokens come from the terminal usage
	// chunk (requested via stream_options for OpenAI providers; synthesized
	// by the Anthropic translator). context.WithoutCancel: the request ctx
	// is already cancelled when the client disconnected mid-stream, but the
	// consumed tokens are a real spend and must still be recorded.
	if route != nil {
		inputTokens, outputTokens, _ := llm.ExtractStreamUsage(raw)
		usageCtx, usageCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		h.svc.RecordUsage(usageCtx, userID, appID, route, status, inputTokens, outputTokens)
		usageCancel()
	}
	h.logAccess(started, userID, appID, routeModel(route, req.Model), req, status, errMsg, output)
```

with helper:

```go
// routeModel prefers the resolved (effective) model over the raw client
// value so the audit log shows what actually served the request.
func routeModel(route *service.ChatRoute, fallback string) string {
	if route != nil {
		return route.LogicalModel
	}
	return fallback
}
```

5. Extend `chatErrorMapping` with the two new sentinels, before the default branch:

```go
	case errors.Is(err, service.ErrChatUnknownModel):
		return http.StatusBadRequest, service.ErrChatUnknownModel.Error()
	case errors.Is(err, service.ErrChatModelNotAllowed):
		return http.StatusForbidden, service.ErrChatModelNotAllowed.Error()
```

6. Add the models-listing handler:

```go
// GetModels handles GET /chat/models: the catalog models the caller's plan
// may use, for kaya's model picker. Unlike StreamChat this is a plain JSON
// endpoint under the global 20s timeout (AllowedModels bounds its DB reads
// with chatAccessTimeout internally).
func (h *ChatHandler) GetModels(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	appID := c.GetString(middleware.ContextAppID)
	models, err := h.svc.AllowedModels(c.Request.Context(), userID, appID)
	if err != nil {
		status, msg := chatErrorMapping(err)
		writeChatError(c, status, msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"models": models}})
}
```

7. Add the import: `"github.com/yunhou/users/internal/llm"`.

- [ ] **Step 3: Update handler tests**

In `internal/handler/chat_test.go` (and any other handler test referencing the mock), update the mock streamer to the new interface. The existing mock looks like a hand-rolled struct with a `StreamChat` method returning `(*http.Response, error)`; change it to:

```go
type mockChatStreamer struct {
	streamFn      func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error)
	usageEvents   []usageCall
	allowedModels []service.ChatModelInfo
	allowedErr    error
}

type usageCall struct {
	userID, appID, status string
	route                 *service.ChatRoute
	inputTokens           int
	outputTokens          int
}

func (m *mockChatStreamer) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
	return m.streamFn(ctx, userID, appID, logicalModel, messages, tools, thinkingEnabled)
}

func (m *mockChatStreamer) RecordUsage(ctx context.Context, userID, appID string, route *service.ChatRoute, status string, inputTokens, outputTokens int) {
	m.usageEvents = append(m.usageEvents, usageCall{userID: userID, appID: appID, status: status, route: route, inputTokens: inputTokens, outputTokens: outputTokens})
}

func (m *mockChatStreamer) AllowedModels(ctx context.Context, userID, appID string) ([]service.ChatModelInfo, error) {
	return m.allowedModels, m.allowedErr
}
```

Adapt every existing test's streamFn to the new signature (add `logicalModel string` param, return a non-nil `*service.ChatRoute{LogicalModel: "deepseek-flash"}` on success). Then ADD these new tests:

```go
func TestChatHandler_RecordsUsageAfterStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	mock := &mockChatStreamer{
		streamFn: func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sse)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}, &service.ChatRoute{LogicalModel: "kimi-k3"}, nil
		},
	}
	// ... build gin context with middleware.ContextUserID/ContextAppID set,
	// POST {"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]},
	// invoke handler.StreamChat, then:
	if len(mock.usageEvents) != 1 {
		t.Fatalf("usage events = %d, want 1", len(mock.usageEvents))
	}
	ev := mock.usageEvents[0]
	if ev.inputTokens != 7 || ev.outputTokens != 2 || ev.status != "ok" || ev.route.LogicalModel != "kimi-k3" {
		t.Errorf("usage event = %+v", ev)
	}
}

func TestChatHandler_NoUsageRecordedWhenUpstreamCallFails(t *testing.T) {
	mock := &mockChatStreamer{
		streamFn: func(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *service.ChatRoute, error) {
			return nil, nil, service.ErrChatUpstreamError
		},
	}
	// ... invoke, expect 502 ...
	if len(mock.usageEvents) != 0 {
		t.Errorf("usage events = %d, want 0 (no upstream spend happened)", len(mock.usageEvents))
	}
}

func TestChatHandler_ModelTooLong(t *testing.T) {
	// POST with model = 65 chars → 400 "model id too long", streamFn never called.
}

func TestChatHandler_UnknownModelMapped(t *testing.T) {
	// streamFn returns ErrChatUnknownModel → 400; ErrChatModelNotAllowed → 403.
}

func TestChatHandler_GetModels(t *testing.T) {
	mock := &mockChatStreamer{allowedModels: []service.ChatModelInfo{
		{ID: "deepseek-flash", DisplayName: "DeepSeek Flash", Provider: "deepseek", Default: true},
	}}
	// GET with JWT ctx → 200 {"code":0,"data":{"models":[...]}}
	// assert body contains "deepseek-flash" and "\"default\":true"
}

func TestChatHandler_GetModelsNoAccess(t *testing.T) {
	mock := &mockChatStreamer{allowedErr: service.ErrChatNoAccess}
	// → 403 with ErrChatNoAccess message
}
```

(The handler test file already has helpers for building a gin context with the JWT claims set — reuse them; see how existing chat handler tests construct `c *gin.Context`.)

- [ ] **Step 4: Register the route**

In `internal/router/router.go`, after the existing chat route (line ~134):

```go
	engine.POST("/chat", chatLimiter, middleware.JWTAuth(tokenSvc), chatHandler.StreamChat)
	// Model picker for kaya: same bucket (cheap, but no reason to make it
	// easier to hammer than chat itself).
	engine.GET("/chat/models", chatLimiter, middleware.JWTAuth(tokenSvc), chatHandler.GetModels)
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/... -v 2>&1 | tail -30`
Expected: build OK; all chat handler/service/llm tests PASS. (router may not compile until Task 10 updates `router.Setup` args — if `Setup` is unchanged here, router compiles already. `cmd/server/main.go` still calls the OLD `NewChatService` signature, so `go build ./cmd/...` fails — expected; fixed in Task 10. Until then use `go build ./internal/...`.)

- [ ] **Step 6: Commit**

```bash
git add internal/model/chat.go internal/handler/chat.go internal/handler/chat_test.go internal/router/router.go
git commit -m "feat(chat): model selection, usage recording after relay, GET /chat/models"
```

---

### Task 9: Admin LLM usage stats endpoint

**Files:**
- Create: `internal/service/llm_usage.go`
- Create: `internal/service/llm_usage_test.go`
- Create: `internal/handler/llm_usage.go`
- Create: `internal/handler/llm_usage_test.go`
- Modify: `internal/router/router.go` (one line in admin group + `Setup` signature gains `llmUsageSvc *service.LLMUsageService`)

**Interfaces:**
- Consumes: `repo.LLMUsageRepo.SumByModel` (Task 5).
- Produces (used by Task 10): `service.NewLLMUsageService(r repo.LLMUsageRepo) *LLMUsageService`, `(s).StatsByModel(ctx, from, to string) ([]model.LLMUsageRow, error)`, `handler.NewLLMUsageHandler(svc) `, `(h).GetByModel(c *gin.Context)`.

- [ ] **Step 1: Write the failing service test**

`internal/service/llm_usage_test.go`:

```go
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestLLMUsageService_StatsByModelValidation(t *testing.T) {
	svc := NewLLMUsageService(&mockLLMUsageRepo{})
	cases := []struct{ name, from, to string }{
		{"bad from", "2026/01/01", "2026-01-31"},
		{"bad to", "2026-01-01", "yesterday"},
		{"from after to", "2026-02-01", "2026-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.StatsByModel(context.Background(), tc.from, tc.to)
			if !errors.Is(err, ErrUsageInvalidParam) {
				t.Errorf("err = %v, want ErrUsageInvalidParam", err)
			}
		})
	}
}

func TestLLMUsageService_StatsByModelOK(t *testing.T) {
	repo := &mockLLMUsageRepo{}
	svc := NewLLMUsageService(repo)
	rows, err := svc.StatsByModel(context.Background(), "2026-09-01", "2026-09-07")
	if err != nil {
		t.Fatalf("StatsByModel: %v", err)
	}
	if rows == nil {
		t.Error("rows must be non-nil empty slice on no data")
	}
}
```

Note: `mockLLMUsageRepo.SumByModel` from Task 7 returns `nil, nil` — the service must convert nil to an empty slice (`[]model.LLMUsageRow{}`) so the JSON response is `[]` not `null`.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/service/ -run TestLLMUsageService -v`
Expected: FAIL — undefined: NewLLMUsageService.

- [ ] **Step 3: Implement**

`internal/service/llm_usage.go`:

```go
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// LLMUsageService backs the admin LLM usage stats endpoint. Thin: date
// validation + repo delegation (mirrors UsageService's param handling).
type LLMUsageService struct {
	repo repo.LLMUsageRepo
}

func NewLLMUsageService(r repo.LLMUsageRepo) *LLMUsageService { return &LLMUsageService{repo: r} }

// StatsByModel returns per-model aggregates over [from, to] (YYYY-MM-DD,
// server timezone). Empty result is a non-nil slice so the JSON response is
// [] not null.
func (s *LLMUsageService) StatsByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	fromDate, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("%w: from must be YYYY-MM-DD", ErrUsageInvalidParam)
	}
	toDate, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("%w: to must be YYYY-MM-DD", ErrUsageInvalidParam)
	}
	if toDate.Before(fromDate) {
		return nil, fmt.Errorf("%w: to must be >= from", ErrUsageInvalidParam)
	}
	rows, err := s.repo.SumByModel(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.LLMUsageRow{}
	}
	return rows, nil
}
```

`internal/handler/llm_usage.go`:

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// LLMUsageHandler serves GET /admin/stats/llm-usage (per-model token/cost
// aggregates over llm_usage_events).
type LLMUsageHandler struct {
	svc *service.LLMUsageService
}

func NewLLMUsageHandler(svc *service.LLMUsageService) *LLMUsageHandler {
	return &LLMUsageHandler{svc: svc}
}

// GetByModel handles GET /admin/stats/llm-usage?from=YYYY-MM-DD&to=YYYY-MM-DD.
func (h *LLMUsageHandler) GetByModel(c *gin.Context) {
	rows, err := h.svc.StatsByModel(c.Request.Context(), c.Query("from"), c.Query("to"))
	if err != nil {
		if errors.Is(err, service.ErrUsageInvalidParam) {
			c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "data": nil, "message": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": http.StatusInternalServerError, "data": nil, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rows})
}
```

`internal/handler/llm_usage_test.go`: construct gin contexts against the handler with a stubbed service — but `LLMUsageHandler` takes the concrete service; follow the existing `usageHandler` test pattern if one exists (`internal/handler` may have usage tests using the real `UsageService` with a mock repo — mirror that: `service.NewLLMUsageService(&stubRepo{})` where stubRepo implements `repo.LLMUsageRepo`). Cover: missing/invalid from → 400; valid → 200 with rows JSON; repo error → 500.

In `internal/router/router.go`:
- `Setup` signature: append `llmUsageSvc *service.LLMUsageService` after `usageSvc *service.UsageService`.
- In the admin group after the usage stats routes:

```go
		// LLM token metering aggregates (migration 022).
		llmUsageHandler := handler.NewLLMUsageHandler(llmUsageSvc)
		adminGroup.GET("/stats/llm-usage", llmUsageHandler.GetByModel)
```

(The handler construction line goes with the other handler constructions near the top; only the route registration goes in the admin group. Match the file's existing structure.)

- [ ] **Step 4: Run tests**

Run: `go build ./internal/... && go test ./internal/service/ ./internal/handler/ -run 'LLMUsage' -v`
Expected: PASS. (`cmd/server` still broken until Task 10.)

- [ ] **Step 5: Commit**

```bash
git add internal/service/llm_usage.go internal/service/llm_usage_test.go internal/handler/llm_usage.go internal/handler/llm_usage_test.go internal/router/router.go
git commit -m "feat(llm): admin per-model usage stats endpoint"
```

---

### Task 10: Config + main wiring + docs

**Files:**
- Modify: `internal/config/config.go` (add `LLMProvidersJSON` field, load, validate)
- Modify: `cmd/server/main.go:175-176` (chat service construction) + router.Setup call site (~line 258-263)
- Modify: `.env.example`
- Modify: `docs/api-integration-guide.md` (chat section: model field, GET /chat/models, LLM_PROVIDERS_JSON)
- Create/Modify: `internal/config/config_test.go` (if it exists, extend; else create)

**Interfaces:**
- Consumes: all previous tasks.
- Produces: `Config.LLMProvidersJSON string` (env `LLM_PROVIDERS_JSON`).

- [ ] **Step 1: Config field + validation test**

In `internal/config/config.go`:
- Add field after `DeepSeekModel`:

```go
	// LLMProvidersJSON is the multi-model catalog (providers + logical
	// models) as one JSON object; parsed and validated at boot by
	// llm.ParseCatalog. When set it takes precedence over the legacy
	// DEEPSEEK_* triple; when empty those envs synthesize a one-model
	// catalog (back-compat). See docs/api-integration-guide.md.
	LLMProvidersJSON string
```

- In `Load()`, after the `DeepSeekModel` line: `LLMProvidersJSON: os.Getenv("LLM_PROVIDERS_JSON"),`
- In `Validate()`, after the DeepSeek block:

```go
	// Multi-model catalog: malformed JSON or a broken reference must kill
	// the process at boot, not surface as per-request 502s.
	if c.LLMProvidersJSON != "" {
		if _, err := llm.ParseCatalog(c.LLMProvidersJSON); err != nil {
			return err
		}
	}
```

Add import `"github.com/yunhou/users/internal/llm"` (no cycle: llm imports only stdlib + internal/model).

Config test (add to existing config test file or create `internal/config/config_test.go`):

```go
func TestValidate_LLMCatalog(t *testing.T) {
	base := func() *Config {
		c := Load()
		c.DatabaseURL = "postgres://x"
		c.OAuthStateSecret = "0123456789abcdef0123456789abcdef"
		return c
	}
	c := base()
	c.LLMProvidersJSON = `{"bogus": true}`
	if err := c.Validate(); err == nil {
		t.Error("want error for malformed LLM_PROVIDERS_JSON")
	}
	c = base()
	c.LLMProvidersJSON = `{"providers":{"p":{"protocol":"openai","base_url":"https://x.example","api_keys":["k"]}},"models":{"m":{"provider":"p","upstream_model":"u"}}}`
	if err := c.Validate(); err != nil {
		t.Errorf("valid catalog rejected: %v", err)
	}
}
```

(Check the existing config test conventions first — if a config_test.go exists with a helper for a minimal valid config, reuse it. The Validate() function requires several unrelated fields to be set; the test must satisfy them.)

- [ ] **Step 2: Run to verify fail → implement → pass**

Run: `go test ./internal/config/ -v`. Expect compile fail → add field/load/validate → PASS.

- [ ] **Step 3: Wire `cmd/server/main.go`**

Replace (line ~175-176):

```go
	// Chat proxy — server-side DeepSeek key; empty key = /chat returns 404.
	chatSvc := service.NewChatService(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel, subRepo, planRepo)
```

with:

```go
	// Chat gateway: LLM_PROVIDERS_JSON wins; the legacy DEEPSEEK_* triple
	// synthesizes a one-model catalog when the JSON is absent. Both empty →
	// chat disabled (/chat returns 404).
	llmCatalog, err := llm.ParseCatalog(cfg.LLMProvidersJSON)
	if err != nil {
		log.Fatalf("parse LLM_PROVIDERS_JSON: %v", err)
	}
	if llmCatalog == nil {
		llmCatalog = llm.LegacyCatalog(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	}
	if llmCatalog != nil {
		log.Printf("chat: %d models across %d providers (default %s)",
			len(llmCatalog.Models), len(llmCatalog.Providers), llmCatalog.DefaultModel)
	}
	llmUsageRepo := repo.NewLLMUsageRepo(db)
	chatSvc := service.NewChatService(llmCatalog, subRepo, planRepo, llmUsageRepo)
```

Check `err` shadowing: main.go already has `err` in scope from earlier setup — reuse `:=` only if legal, else `=`. The implementer must compile and fix accordingly.

Add import `"github.com/yunhou/users/internal/llm"`.

At the `router.Setup(...)` call site, add the new trailing argument `service.NewLLMUsageService(llmUsageRepo)` after `usageSvc` to match the Task 9 signature.

- [ ] **Step 4: Verify build + full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: everything compiles; all tests PASS (DB-backed tests may SKIP without Postgres).

Also run any e2e suite compile check: `go build ./tests/... 2>/dev/null || true` and inspect `tests/e2e` for references to `/chat` request shape (the `model` field is optional so old calls keep working; but if the e2e suite constructs `service.NewChatService` or `router.Setup` directly, update those call sites too — grep for `NewChatService|router.Setup` across the whole repo and fix every call site).

- [ ] **Step 5: Docs + env example**

`.env.example` — add after the DEEPSEEK_* block:

```bash
# Multi-model chat catalog (takes precedence over DEEPSEEK_* when set).
# JSON object: providers (protocol openai|anthropic, base_url, api_keys[],
# optional headers{}) + models (logical id → provider, upstream_model,
# display_name, prices in CNY per 1M tokens, optional max_tokens).
# LLM_PROVIDERS_JSON={"default_model":"deepseek-flash","providers":{"deepseek":{"protocol":"openai","base_url":"https://api.deepseek.com","api_keys":["sk-..."]},"kimi":{"protocol":"anthropic","base_url":"https://api.kimi.com/coding","api_keys":["sk-kimi-..."]}},"models":{"deepseek-flash":{"provider":"deepseek","upstream_model":"deepseek-chat","display_name":"DeepSeek Flash","input_price_per_mtok":2,"output_price_per_mtok":8},"kimi-k3":{"provider":"kimi","upstream_model":"kimi-k3-latest","display_name":"Kimi K3","max_tokens":16384}}}
```

`docs/api-integration-guide.md` chat section — document:
- `POST /chat` now accepts optional `model` (logical id; default when omitted).
- `GET /chat/models` (JWT) → `{"code":0,"data":{"models":[{"id","display_name","provider","default"}]}}`, plan-filtered.
- Error codes: unknown model → 400 `unknown chat model`; plan disallows → 403 `chat model is not allowed for the current plan`.
- Operator docs: `LLM_PROVIDERS_JSON` schema, key pooling behavior (round-robin + 60s cooldown on 429/5xx, one transparent retry when ≥2 keys), metering table `llm_usage_events` + `GET /admin/stats/llm-usage?from&to`, per-plan allowlist via SQL `UPDATE plans SET chat_models = '{deepseek-flash,kimi-k3}' WHERE id = 'monthly'`.

- [ ] **Step 6: Final verification + commit**

Run: `go build ./... && go vet ./... && go test ./... && go run ./cmd/migrate` (migrate needs DATABASE_URL; skip when no DB).
Expected: PASS / clean.

```bash
git add -A
git commit -m "feat(llm): wire multi-model catalog via LLM_PROVIDERS_JSON, docs, env example"
```

---

## Self-Review Notes (conclusions, already applied above)

- Spec coverage: catalog+routing (T1,T7), key pool (T1,T7), dual protocol (T2-T4,T7), plan gating (T6,T7), metering (T2,T4,T5,T7,T8), model picker endpoint (T7,T8), admin stats (T9), config/wiring/docs (T10). Legacy back-compat: `LegacyCatalog` (T1) + wiring (T10). No-quota-yet: per-plan token QUOTA is deliberately out of scope (metering lands first; quota enforcement is a follow-up reading `llm_usage_events`).
- Type consistency: `ChatRoute`, `ChatModelInfo`, `LLMUsageEvent`, `LLMUsageRow`, `LLMUsageRepo`, `ParseCatalog`, `LegacyCatalog`, `TranslateAnthropicStream`, `BuildOpenAIPayload`, `BuildAnthropicPayload`, `ExtractStreamUsage`, `NewKeyPool/Acquire/Cool/Len` are used with identical signatures across tasks.
- Known acceptable gap: Anthropic `content_block_start` text blocks emit nothing (OpenAI needs no block-start); tool_calls index uses the Anthropic block index (gaps possible, harmless). Providers ignoring `stream_options.include_usage` meter as 0 tokens — the row is still written.

---

## Post-Review Fixes (2026-09-07, senior review of feat/multi-model-gateway)

The plan body above describes the as-designed implementation; the following
review-driven fixes supersede two of its details:

- **Metering no longer reads the capped capture.** The relay feeds every
  upstream read into an incremental `llm.UsageTracker` (line-reassembling,
  `"usage"`-gated, last-wins) BEFORE the client write, and the handler meters
  from the tracker regardless of how the relay ended. The 256 KiB
  `chatRawLogCap` capture now bounds ONLY the audit-log output text;
  `ExtractStreamUsage` remains as the batch-shaped twin used by tests. This
  makes migration 022's header claim (disconnected/interrupted streams still
  record consumed tokens) actually true. Additionally, the Anthropic
  translator emits the terminal OpenAI usage chunk (+`[DONE]`) when the
  stream ends WITHOUT `message_stop` (EOF or upstream error) and usage
  counters are non-zero, so partial Anthropic consumption is metered too.
- **Anthropic translation merges consecutive same-role turns.** Handler
  validation deliberately allows them (legal for OpenAI-protocol models) but
  Anthropic 400s without strict user/assistant alternation. Text blocks
  append onto a previous same-role turn; tool_result blocks always lead a
  merged user turn (a tool result following plain user text inserts ahead of
  the text, plain user text following tool results appends after them).

Smaller hardening: the audit log truncates the `model` field at
`ChatMaxModelLen` (+ellipsis) inside `logAccess`; `LLMUsageHandler` answers
503 when its service is nil (route is registered unconditionally);
`KeyPool.Acquire` returns `(-1, "")` on an empty pool instead of panicking;
nameless tool definitions are dropped during Anthropic translation (the
`tools` key is omitted when nothing survives) instead of emitting
`"name":null` and failing the whole request upstream.
