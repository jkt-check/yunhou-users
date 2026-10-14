package llm

import (
	"slices"
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
		{"unsupported input modality", func(c *Catalog) {
			m := c.Models["deepseek-flash"]
			m.InputModalities = []string{"text", "audio"}
			c.Models["deepseek-flash"] = m
		}, "unsupported input_modality"},
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

// TestModel_AcceptsImages: 缺省/未配置 InputModalities 一律放行图片
// （default-on——未声明 = ["text","image"]）；只有显式声明不含 image 的
// 列表（如 ["text"]）保持纯文本拒绝。
func TestModel_AcceptsImages(t *testing.T) {
	cases := []struct {
		name       string
		modalities []string
		want       bool
	}{
		{"missing (nil)", nil, true},
		{"explicit empty", []string{}, true},
		{"text only", []string{"text"}, false},
		{"text + image", []string{"text", "image"}, true},
		{"image only", []string{"image"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Model{InputModalities: tc.modalities}).AcceptsImages(); got != tc.want {
				t.Errorf("AcceptsImages(%v) = %v, want %v", tc.modalities, got, tc.want)
			}
		})
	}
}

// TestParseCatalog_InputModalities: input_modalities 从 LLM_PROVIDERS_JSON
// 解析进 Model；缺省（未声明）的模型按 default-on 放行图片，显式
// ["text"] 的模型保持纯文本。
func TestParseCatalog_InputModalities(t *testing.T) {
	raw := `{"default_model":"vision",
		"providers":{"p":{"protocol":"openai","base_url":"https://x.example","api_keys":["k"]}},
		"models":{
			"vision":{"provider":"p","upstream_model":"u-v","input_modalities":["text","image"]},
			"plain":{"provider":"p","upstream_model":"u-p"},
			"textonly":{"provider":"p","upstream_model":"u-t","input_modalities":["text"]}
		}}`
	c, err := ParseCatalog(raw)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if !c.Models["vision"].AcceptsImages() {
		t.Error("vision model must accept images")
	}
	if !c.Models["plain"].AcceptsImages() {
		t.Error("model without input_modalities must default-on (accept images)")
	}
	if got := c.Models["plain"].NormalizedInputModalities(); !slices.Equal(got, []string{"text", "image"}) {
		t.Errorf("undeclared NormalizedInputModalities = %v, want [text image]", got)
	}
	if c.Models["textonly"].AcceptsImages() {
		t.Error("explicit [text] model must stay text-only")
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
	// DEEPSEEK_* 兜底目录钉死 text-only：DeepSeek 上游无视觉模型，不得
	// 跟随 default-on（否则带图请求只会在上游 400→502）。
	_, m, _ := c.Resolve("")
	if got := m.InputModalities; !slices.Equal(got, []string{"text"}) {
		t.Errorf("legacy InputModalities = %v, want explicit [text]", got)
	}
	if m.AcceptsImages() {
		t.Error("legacy DEEPSEEK_* catalog must stay text-only (pinned, not default-on)")
	}
}
