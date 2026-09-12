package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// validate_edges_test.go — Task 16 覆盖率补强：Validate* 纯校验器的边界
// 矩阵 + ParseSnapshot/ SnapshotCache 的失败路径（不得加载半个版本）。

func TestValidateModel_Edges(t *testing.T) {
	ok := &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM", ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}
	if err := ValidateModel(ok); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
	cases := map[string]*domain.Model{
		"empty id":      {DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols},
		"bad id shape":  {ID: "Bad Model!", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols},
		"no display":    {ID: "m1", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols},
		"zero context":  {ID: "m1", DisplayName: "x", MaxOutputTokens: 1, Protocols: ok.Protocols},
		"zero max out":  {ID: "m1", DisplayName: "x", ContextTokens: 1, Protocols: ok.Protocols},
		"out > context": {ID: "m1", DisplayName: "x", ContextTokens: 100, MaxOutputTokens: 200, Protocols: ok.Protocols},
		"no protocols":  {ID: "m1", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1},
		"bad protocol":  {ID: "m1", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: []domain.Protocol{"carrier-pigeon"}},
		"bad modality":  {ID: "m1", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols, InputModalities: []string{"smell"}},
		"bad lifecycle": {ID: "m1", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols, Lifecycle: "undead"},
		"huge context":  {ID: "m1", DisplayName: "x", ContextTokens: 1 << 40, MaxOutputTokens: 1, Protocols: ok.Protocols},
		"bad alias":     {ID: "m1", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1, Protocols: ok.Protocols, Aliases: []string{"bad alias!"}},
	}
	for name, m := range cases {
		if err := ValidateModel(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateProvider_Edges(t *testing.T) {
	ok := &domain.Provider{Code: "glm-x", DisplayName: "GLM", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := ValidateProvider(ok); err != nil {
		t.Fatalf("valid provider rejected: %v", err)
	}
	for name, p := range map[string]*domain.Provider{
		"empty code":    {DisplayName: "x", AccessType: ok.AccessType, Status: "active"},
		"bad code":      {Code: "GLM!", DisplayName: "x", AccessType: ok.AccessType, Status: "active"},
		"no display":    {Code: "glm", AccessType: ok.AccessType, Status: "active"},
		"bad access":    {Code: "glm", DisplayName: "x", AccessType: "telepathy", Status: "active"},
		"bad status":    {Code: "glm", DisplayName: "x", AccessType: ok.AccessType, Status: "zombie"},
		"code too long": {Code: strings.Repeat("a", 65), DisplayName: "x", AccessType: ok.AccessType, Status: "active"},
	} {
		if err := ValidateProvider(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateDeployment_Edges(t *testing.T) {
	ok := &domain.Deployment{
		ProviderID: "p1", UpstreamModel: "up", BaseURL: "https://api.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentDraft,
	}
	if err := ValidateDeployment(ok); err != nil {
		t.Fatalf("valid deployment rejected: %v", err)
	}
	mut := func(f func(d *domain.Deployment)) *domain.Deployment {
		d := *ok
		f(&d)
		return &d
	}
	for name, d := range map[string]*domain.Deployment{
		"no provider":   mut(func(d *domain.Deployment) { d.ProviderID = "" }),
		"no upstream":   mut(func(d *domain.Deployment) { d.UpstreamModel = "" }),
		"relative url":  mut(func(d *domain.Deployment) { d.BaseURL = "api.example.com" }),
		"non-http url":  mut(func(d *domain.Deployment) { d.BaseURL = "ftp://api.example.com" }),
		"no connect to": mut(func(d *domain.Deployment) { d.ConnectTimeout = 0 }),
		"no request to": mut(func(d *domain.Deployment) { d.RequestTimeout = 0 }),
		"bad protocol":  mut(func(d *domain.Deployment) { d.Protocol = "smoke-signal" }),
		"blacklist hdr": mut(func(d *domain.Deployment) {
			d.Config = domain.ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1,"headers":{"Authorization":"evil"}}`)}
		}),
		"bad hdr json": mut(func(d *domain.Deployment) {
			d.Config = domain.ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1,"headers":[1,2]}`)}
		}),
	} {
		if err := ValidateDeployment(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 良性 headers 放行。
	benign := mut(func(d *domain.Deployment) {
		d.Config = domain.ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1,"headers":{"X-Team":"ops"}}`)}
	})
	if err := ValidateDeployment(benign); err != nil {
		t.Errorf("benign headers rejected: %v", err)
	}
}

func TestValidateRoute_Edges(t *testing.T) {
	ok := &domain.ModelRoute{ModelID: "m1", DeploymentID: "d1", Weight: 1, Enabled: true}
	if err := ValidateRoute(ok); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	for name, r := range map[string]*domain.ModelRoute{
		"no model":      {DeploymentID: "d1", Weight: 1},
		"no deployment": {ModelID: "m1", Weight: 1},
		"zero weight":   {ModelID: "m1", DeploymentID: "d1", Weight: 0},
		"neg weight":    {ModelID: "m1", DeploymentID: "d1", Weight: -1},
		"bad strategy":  {ModelID: "m1", DeploymentID: "d1", Weight: 1, PoolStrategy: "chaos"},
	} {
		if err := ValidateRoute(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSnapshot_RejectsHalfVersions(t *testing.T) {
	// 非 JSON / 缺 schema_version / 半截 payload 全部显式失败（不得加载半
	// 个版本）。
	for name, raw := range map[string]string{
		"not json":     `nope`,
		"empty":        ``,
		"no schema":    `{"models":{}}`,
		"wrong schema": `{"schema_version":99}`,
		"half model":   `{"schema_version":1,"models":{"m1":{"id":"m1"}}}`,
	} {
		rev := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1,
			Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: []byte(raw)}}
		if _, err := ParseSnapshot(rev); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// 由 BuildCatalogPayload 产出的合法快照可以往返。
	payload := BuildCatalogPayload(
		[]domain.Model{{ID: "m1", DisplayName: "M", Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100, Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"}}},
		[]domain.Provider{{ID: "p1", Code: "p", DisplayName: "P", AccessType: domain.AccessOfficialAPI, Status: "active"}},
		[]domain.Deployment{{ID: "d1", ProviderID: "p1", UpstreamModel: "up", BaseURL: "https://api.example.com",
			Protocol: domain.ProtocolOpenAIChat, Status: domain.DeploymentActive,
			ConnectTimeout: time.Second, RequestTimeout: time.Second}},
		[]domain.ModelRoute{{ModelID: "m1", DeploymentID: "d1", Weight: 1, Enabled: true}},
	)
	rev := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: payload}, Status: domain.RevisionPublished}
	snap, err := ParseSnapshot(rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Models) != 1 || len(snap.ActiveDeployments("m1")) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
}
