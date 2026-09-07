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
		{"model references unknown provider", func(c *Catalog) {
			m := c.Models["deepseek-flash"]
			m.Provider = "ghost"
			c.Models["deepseek-flash"] = m
		}, "unknown provider"},
		{"model without upstream_model", func(c *Catalog) {
			m := c.Models["deepseek-flash"]
			m.UpstreamModel = ""
			c.Models["deepseek-flash"] = m
		}, "upstream_model"},
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
