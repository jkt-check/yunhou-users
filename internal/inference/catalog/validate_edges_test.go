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

// 对抗评审 C1：RequestTimeout 不得超过生效 recovery grace——否则超 grace
// 的活请求会被恢复扫描按全额预占误结算，真实用量到达时撞终态守卫被吞。
func TestValidateDeploymentRecoveryWindow_Edges(t *testing.T) {
	dep := &domain.Deployment{RequestTimeout: 10 * time.Minute}
	if err := ValidateDeploymentRecoveryWindow(dep); err != nil {
		t.Fatalf("10m timeout under the default 15m grace rejected: %v", err)
	}
	// 边界：等于 grace 即拒绝（活请求可在 cutoff 之前完成全阶段）。
	dep.RequestTimeout = 15 * time.Minute
	if err := ValidateDeploymentRecoveryWindow(dep); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("timeout == grace: err = %v, want invalid_input", err)
	}
	dep.RequestTimeout = 16 * time.Minute
	if err := ValidateDeploymentRecoveryWindow(dep); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("timeout > grace: err = %v, want invalid_input", err)
	}
	// 调低 grace（config.Validate 地板 12m）后，原来能写的 13m 必须被拦。
	old := RecoveryGrace()
	t.Cleanup(func() { SetRecoveryGrace(old) })
	SetRecoveryGrace(12 * time.Minute)
	dep.RequestTimeout = 13 * time.Minute
	if err := ValidateDeploymentRecoveryWindow(dep); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("timeout > lowered grace: err = %v, want invalid_input", err)
	}
	dep.RequestTimeout = 10 * time.Minute
	if err := ValidateDeploymentRecoveryWindow(dep); err != nil {
		t.Errorf("10m timeout under the lowered 12m grace rejected: %v", err)
	}
	// 非正数布线不得破坏安全默认。
	SetRecoveryGrace(0)
	if RecoveryGrace() != 12*time.Minute {
		t.Errorf("SetRecoveryGrace(0) clobbered the effective grace: %s", RecoveryGrace())
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

// T7 — pool_strategy 支持集（写面）：常规池只支持 round_robin；
// least_loaded 与通用 session_sticky 明确拒绝 —— 不能保存后假装生效
// （保存即生效的假象比拒绝更坏：运营以为配了，实际跑的是 RR）。
//
// 注意：Responses 的 previous_response_id 专用绑定走的是会话绑定器
// （routing.SessionBinder），与 route 的 pool_strategy 无关 —— 拒绝
// 通用 session_sticky 不会影响那条链路。
func TestValidateRoute_PoolStrategySupportedSet(t *testing.T) {
	for _, ok := range []domain.PoolStrategy{"", domain.PoolRoundRobin} {
		r := &domain.ModelRoute{ModelID: "m1", DeploymentID: "d1", Weight: 1, PoolStrategy: ok}
		if err := ValidateRoute(r); err != nil {
			t.Errorf("write path rejected the supported strategy %q: %v", ok, err)
		}
	}
	for _, bad := range []domain.PoolStrategy{domain.PoolLeastLoaded, domain.PoolSessionSticky, "chaos"} {
		r := &domain.ModelRoute{ModelID: "m1", DeploymentID: "d1", Weight: 1, PoolStrategy: bad}
		err := ValidateRoute(r)
		if err == nil {
			t.Errorf("write path accepted %q (unsupported pool strategy must be refused, not stored as if it worked)", bad)
			continue
		}
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%q: code = %v, want invalid_input", bad, domain.CodeOf(err))
		}
		if !strings.Contains(err.Error(), string(domain.PoolRoundRobin)) {
			t.Errorf("%q: the refusal must name the supported replacement: %v", bad, err)
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

// 评审修复（组6-A）：跨 model alias 必须唯一。重复 alias 会让 Snapshot.Model
// 的 map 遍历解析变成随机路由（同一请求被打到不同 upstream、按不同价格版本
// 计费）；alias 撞 model ID 则被 exact-ID 查找静默遮蔽成死配置。发布闸与
// ParseSnapshot 双侧拒绝，歧义目录既发不出也载不入。
func TestValidateCatalogRejectsAmbiguousAliases(t *testing.T) {
	mk := func(id string, aliases ...string) domain.Model {
		return domain.Model{ID: id, DisplayName: "M", Aliases: aliases,
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"}}
	}
	prov := []domain.Provider{{ID: "p1", Code: "p", DisplayName: "P", AccessType: domain.AccessOfficialAPI, Status: "active"}}
	parse := func(models ...domain.Model) error {
		raw := BuildCatalogPayload(models, prov, nil, nil)
		_, err := ParseSnapshot(&domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1,
			Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: raw}})
		return err
	}

	// 各自 alias 互不冲突 → 双侧放行（同一 model 重复列出同一 alias 无害）。
	ok := []domain.Model{mk("m1", "latest", "latest"), mk("m2", "stable")}
	if err := validateCatalogForPublish(ok, prov, nil, nil); err != nil {
		t.Errorf("distinct aliases rejected by publish gate: %v", err)
	}
	if err := parse(ok...); err != nil {
		t.Errorf("distinct aliases rejected by ParseSnapshot: %v", err)
	}

	// 两个 model 配同一 alias → 双侧拒绝。
	dup := []domain.Model{mk("m1", "latest"), mk("m2", "latest")}
	if err := validateCatalogForPublish(dup, prov, nil, nil); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("duplicate alias, publish gate: err = %v, want invalid_input", err)
	}
	if err := parse(dup...); err == nil {
		t.Error("duplicate alias: ParseSnapshot must fail (non-deterministic dispatch resolution)")
	}

	// alias 等于另一个 model 的 ID → 双侧拒绝。
	shadow := []domain.Model{mk("m1"), mk("m2", "m1")}
	if err := validateCatalogForPublish(shadow, prov, nil, nil); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("alias shadowing a model id, publish gate: err = %v, want invalid_input", err)
	}
	if err := parse(shadow...); err == nil {
		t.Error("alias shadowing a model id: ParseSnapshot must fail (dead config)")
	}
}

// T7 — 发布闸的路由拒绝必须点名**真正**的原因：pool_strategy 不受支持时
// 给出替代配置；其它不变量（weight / model_id）失败时保留它自己的错误，
// 绝不把运营指到错误的旋钮上（一个自相矛盾的「round_robin 不受支持，
// 请设成 round_robin」既无用又误导）。
func TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause(t *testing.T) {
	prov := []domain.Provider{{ID: "p1", Code: "p", DisplayName: "P",
		AccessType: domain.AccessOfficialAPI, Status: "active"}}
	deps := []domain.Deployment{{ID: "d1", ProviderID: "p1", UpstreamModel: "up",
		BaseURL: "https://api.example.com", Protocol: domain.ProtocolOpenAIChat,
		Status: domain.DeploymentActive, ConnectTimeout: time.Second, RequestTimeout: time.Second}}

	cases := []struct {
		name    string
		route   domain.ModelRoute
		wantSub string
		mustNot string
	}{
		{
			name:    "weight zero keeps its own cause",
			route:   domain.ModelRoute{ID: "route-1", ModelID: "m1", DeploymentID: "d1", Weight: 0, Enabled: true},
			wantSub: "weight",
			mustNot: "round_robin is not supported",
		},
		{
			name:    "empty model keeps its own cause",
			route:   domain.ModelRoute{ID: "route-2", DeploymentID: "d1", Weight: 1, Enabled: true},
			wantSub: "model_id",
			mustNot: "round_robin is not supported",
		},
		{
			name:    "unsupported strategy names the replacement",
			route:   domain.ModelRoute{ID: "route-3", ModelID: "m1", DeploymentID: "d1", Weight: 1, Enabled: true, PoolStrategy: domain.PoolLeastLoaded},
			wantSub: "round_robin",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateCatalogForPublish(nil, prov, deps, []domain.ModelRoute{c.route})
			if err == nil {
				t.Fatalf("publish must refuse the route")
			}
			if domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("code = %v, want invalid_input", domain.CodeOf(err))
			}
			if !strings.Contains(err.Error(), c.route.ID) {
				t.Errorf("error = %v, want it to name the route id %q", err, c.route.ID)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error = %v, want it to name %q", err, c.wantSub)
			}
			if c.mustNot != "" && strings.Contains(err.Error(), c.mustNot) {
				t.Errorf("error = %v, must not point the operator at the pool_strategy knob", err)
			}
		})
	}

	// 写面同样保留各自的原因（与发布闸同一口径）。
	if err := ValidateRoute(&domain.ModelRoute{ModelID: "m1", DeploymentID: "d1", Weight: 0}); err == nil ||
		!strings.Contains(err.Error(), "weight") {
		t.Errorf("write path: err = %v, want the weight cause", err)
	}
	if err := ValidateRoute(&domain.ModelRoute{ModelID: "m1", DeploymentID: "d1", Weight: 1, PoolStrategy: domain.PoolLeastLoaded}); err == nil ||
		!strings.Contains(err.Error(), "round_robin") {
		t.Errorf("write path: err = %v, want the pool_strategy replacement", err)
	}
}
