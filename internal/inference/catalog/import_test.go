package catalog_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

const validEnvCatalog = `{
  "providers": [
    {"code": "openai", "display_name": "OpenAI", "access_type": "official_api"}
  ],
  "models": [
    {
      "id": "glm-4.6",
      "display_name": "GLM 4.6",
      "context_tokens": 200000,
      "max_output_tokens": 8192,
      "protocols": ["openai_chat"],
      "capabilities": {"tools": true, "reasoning": true},
      "deployments": [
        {
          "provider": "openai",
          "upstream_model": "glm-4.6",
          "base_url": "https://api.example.com/v1",
          "protocol": "openai_chat",
          "region": "us"
        }
      ]
    },
    {
      "id": "deepseek-v4",
      "display_name": "DeepSeek V4",
      "context_tokens": 128000,
      "max_output_tokens": 4096,
      "protocols": ["openai_chat"],
      "deployments": [
        {
          "provider": "openai",
          "upstream_model": "deepseek-chat",
          "base_url": "https://api.example.com/v1",
          "protocol": "openai_chat"
        },
        {
          "provider": "openai",
          "upstream_model": "deepseek-chat",
          "base_url": "https://backup.example.com/v1",
          "protocol": "openai_chat"
        }
      ]
    }
  ]
}`

func TestImportIsExplicitIdempotentAndNonDestructive(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	res, err := svc.ImportEnvCatalog(ctx, validEnvCatalog)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if res.ProvidersInserted != 1 || res.ModelsInserted != 2 || res.DeploymentsInserted != 3 || res.RoutesInserted != 3 {
		t.Errorf("first import = %+v", res)
	}

	// Everything imported is DRAFT — env import alone never makes a model
	// sellable (新模型默认不可售).
	m, err := svc.GetModel(ctx, "glm-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if m.Lifecycle != domain.LifecycleDraft {
		t.Errorf("imported model lifecycle = %s, want draft", m.Lifecycle)
	}

	// Second run: pure no-op — full idempotency, nothing overwritten.
	res2, err := svc.ImportEnvCatalog(ctx, validEnvCatalog)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res2.ProvidersInserted+res2.ModelsInserted+res2.DeploymentsInserted+res2.RoutesInserted != 0 {
		t.Errorf("second import inserted rows: %+v", res2)
	}
	if res2.Skipped == 0 {
		t.Error("second import must report skipped existing rows")
	}

	// Operator edits in the DB are never clobbered by a re-import.
	m, _ = svc.GetModel(ctx, "glm-4.6")
	m.DisplayName = "Operator Renamed"
	if err := svc.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEnvCatalog(ctx, validEnvCatalog); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.GetModel(ctx, "glm-4.6")
	if m.DisplayName != "Operator Renamed" {
		t.Errorf("env import overwrote operator edit: %q", m.DisplayName)
	}

	// Multi-deployment mapping survived.
	deps, err := svc.ListDeployments(ctx, domain.DeploymentFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 3 {
		t.Errorf("deployments = %d, want 3 (model can have multiple deployments)", len(deps))
	}
}

func TestImportValidationFailures(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	mutate := func(old, new string) string {
		return strings.Replace(validEnvCatalog, old, new, 1)
	}
	inputs := []struct {
		name string
		json string
	}{
		{"unknown top-level field", mutate(`"models"`, `"surprise": true, "models"`)},
		{"unknown model field", mutate(`"context_tokens": 200000`, `"context_tokens": 200000, "surprise": true`)},
		{"bad model protocol", mutate(`"protocols": ["openai_chat"]`, `"protocols": ["carrier_pigeon"]`)},
		{"unknown provider reference", mutate(`"provider": "openai"`, `"provider": "does-not-exist"`)},
		{"metadata address", mutate(`"base_url": "https://api.example.com/v1"`, `"base_url": "https://169.254.169.254/latest"`)},
		{"zero context tokens", mutate(`"context_tokens": 200000`, `"context_tokens": 0`)},
		{"bad json", `{"providers": [`},
	}
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.ImportEnvCatalog(ctx, tc.json); err == nil {
				t.Errorf("want error, got nil")
			}
		})
	}
}

func TestParseEnvCatalogRejectsKayaChatUpstream(t *testing.T) {
	raw := `{"providers":[{"code":"p","display_name":"P","access_type":"official_api"}],
		"models":[{"id":"m-1","display_name":"M","context_tokens":1,"max_output_tokens":1,
		"protocols":["kaya_chat"],
		"deployments":[{"provider":"p","upstream_model":"u","base_url":"https://x.example.com","protocol":"kaya_chat"}]}]}`
	if _, err := catalog.ParseEnvCatalog(raw); err == nil {
		t.Error("kaya_chat must never appear on an upstream deployment")
	}
}
