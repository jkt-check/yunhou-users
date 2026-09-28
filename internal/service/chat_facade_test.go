package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

// chat_facade_test.go — 评审批次7 Minor-4/5 验收 + R7-N4a/N4b 契约对齐：
// Minor-4：CodeUnpricedCapability（运营定价配置错误）映射为 500 类而不是
// 403「无权限」；Minor-5：AllowedModels 非哨兵错误记录日志并透传。
// N4a：AllowedModels 的「无计费账户」哨兵 → ErrChatNoAccess（403 对齐
// legacy）；N4b：CodeNotFound/CodeModelNotAllowed/CodeInvalidKey 拆分
// 为未知模型 400 / 无权益 403 / 无访问 403。

// facadeKeyStore 是 access.KeyStore 的手写桩（CLAUDE.md：hand-rolled
// doubles）；只有 GetBillingAccountByUser 的返回对本测试有意义。
type facadeKeyStore struct {
	accountErr error
}

func (f *facadeKeyStore) EnsureBillingAccount(context.Context, string) (*domain.BillingAccount, error) {
	return nil, errors.New("not implemented")
}
func (f *facadeKeyStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	return nil, f.accountErr
}
func (f *facadeKeyStore) GetBillingAccountByID(context.Context, string) (*domain.BillingAccount, error) {
	return nil, errors.New("not implemented")
}
func (f *facadeKeyStore) ListActiveEntitlements(context.Context, string, time.Time) ([]domain.Entitlement, error) {
	return nil, nil
}
func (f *facadeKeyStore) InsertAPIKey(context.Context, *domain.APIKey, string) error {
	return errors.New("not implemented")
}
func (f *facadeKeyStore) GetAPIKeyByPrefix(context.Context, string) (*domain.APIKey, string, error) {
	return nil, "", errors.New("not implemented")
}
func (f *facadeKeyStore) GetAPIKeyByID(context.Context, string) (*domain.APIKey, error) {
	return nil, errors.New("not implemented")
}
func (f *facadeKeyStore) ListAPIKeysByAccount(context.Context, string, int, int) ([]domain.APIKey, int64, error) {
	return nil, 0, errors.New("not implemented")
}
func (f *facadeKeyStore) UpdateAPIKey(context.Context, *domain.APIKey) error {
	return errors.New("not implemented")
}
func (f *facadeKeyStore) RevokeAPIKey(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("not implemented")
}
func (f *facadeKeyStore) MarkAPIKeyExpired(context.Context, string) error {
	return errors.New("not implemented")
}
func (f *facadeKeyStore) TouchAPIKeyLastUsed(context.Context, string, time.Time) error {
	return errors.New("not implemented")
}

// stubSnapshotSource 是 gateway.SnapshotSource 的手写桩（同 facadeKeyStore
// 风格）：直接喂快照或错误，驱动就绪闸门单元测试。
type stubSnapshotSource struct {
	snap *catalog.Snapshot
	err  error
}

func (s stubSnapshotSource) Current(context.Context) (*catalog.Snapshot, error) {
	return s.snap, s.err
}

// facadeRevisionSource 是 SnapshotCache 的 revisionSource 手写桩（head 探针
// + 全量加载），用于经真实 cache 驱动冷启动路径——而非手注哨兵。
type facadeRevisionSource struct {
	headErr error
	rev     *domain.ConfigRevision
	revErr  error
}

func (s *facadeRevisionSource) ActiveRevisionHead(context.Context, domain.ConfigScope) (int64, int, error) {
	return 0, 0, s.headErr
}
func (s *facadeRevisionSource) ActiveRevision(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
	return s.rev, s.revErr
}


func TestMapGatewayError_UnpricedCapabilityIsUpstreamError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want error
	}{
		// Minor-4：运营定价配置错误 → 500 类（与真实权益拒绝区分开）。
		{"unpriced_capability", domain.NewError(domain.CodeUnpricedCapability, "no price"), ErrChatUpstreamError},
		// N4b 拆分（对齐 legacy）：
		// 目录查无此模型 id → ErrChatUnknownModel（400，picker 回退默认）；
		// 模型存在但无权益 → ErrChatModelNotAllowed（403 legacy checkAccess 文案）；
		// 账户停用 → ErrChatNoAccess（403）。
		{"model_not_allowed", domain.NewError(domain.CodeModelNotAllowed, "denied"), ErrChatModelNotAllowed},
		{"invalid_key", domain.NewError(domain.CodeInvalidKey, "denied"), ErrChatNoAccess},
		{"not_found", domain.NewError(domain.CodeNotFound, "no such model"), ErrChatUnknownModel},
		{"rate_limited", domain.NewError(domain.CodeRateLimited, "slow down"), ErrChatRateLimited},
		{"internal", domain.NewError(domain.CodeInternal, "boom"), ErrChatUpstreamError},
		// R7-N1：目录冷启动（无已验证快照）哨兵 → 503，不得伪装成 403/500。
		{"no_verified_snapshot", fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot), ErrChatNotReady},
		// 评审修复 C1 验证：gateway 把 Current 错误包成 CodeInternal
		// （gateway/service.go:310），domain.Error.Unwrap 暴露 cause，
		// 哨兵穿透 WrapError 后仍被 errors.Is 命中。
		{"wrapped_no_verified_snapshot", domain.WrapError(domain.CodeInternal, "gateway: catalog snapshot unavailable",
			fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot)), ErrChatNotReady},
		// R7-N4 契约差异修复：failover 包装（CodeUpstreamUnavailable +
		// DispatchError cause）按链上真实状态码分类，对齐 legacy：
		// 上游 429（候选全限流）→ 429；上游 4xx → ChatUpstreamRejection
		// （errors.Is 命中 ErrChatUpstreamRejected）；上游 5xx / 传输错误
		// 仍落 ErrChatUpstreamError（502）。
		{"upstream_429_exhausted", domain.WrapError(domain.CodeUpstreamUnavailable, "all compatible upstreams failed",
			&providers.DispatchError{StatusCode: 429, Body: `{"error":{"message":"slow down"}}`}), ErrChatRateLimited},
		{"upstream_4xx_rejected", domain.WrapError(domain.CodeUpstreamUnavailable, "upstream rejected the request",
			&providers.DispatchError{StatusCode: 400, Body: `{"error":{"message":"maximum context length exceeded","code":"context_length_exceeded"}}`}), ErrChatUpstreamRejected},
		{"upstream_500", domain.WrapError(domain.CodeUpstreamUnavailable, "all compatible upstreams failed",
			&providers.DispatchError{StatusCode: 500, Body: `{"error":{"message":"boom"}}`}), ErrChatUpstreamError},
		{"upstream_transport", domain.WrapError(domain.CodeUpstreamUnavailable, "all compatible upstreams failed",
			&providers.DispatchError{Err: errors.New("connection refused")}), ErrChatUpstreamError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapGatewayError(c.err); !errors.Is(got, c.want) {
				t.Errorf("mapGatewayError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// R7-N4：facade 的上游 4xx 拒绝必须与 legacy classifyUpstreamRejection
// 产生逐字段相同的结构化分类（status/code/message），kaya 依赖
// data.upstream_code 区分失败类别。
func TestMapGatewayError_UpstreamRejectionMatchesLegacyClassification(t *testing.T) {
	t.Parallel()
	body := `{"error":{"message":"This model's maximum context length is 8192 tokens","code":"context_length_exceeded"}}`
	legacy := classifyUpstreamRejection(400, []byte(body))
	got := mapGatewayError(domain.WrapError(domain.CodeUpstreamUnavailable,
		"upstream rejected the request", &providers.DispatchError{StatusCode: 400, Body: body}))
	var rej *ChatUpstreamRejection
	if !errors.As(got, &rej) {
		t.Fatalf("err = %v, want *ChatUpstreamRejection", got)
	}
	if rej.Status != legacy.Status || rej.Code != legacy.Code || rej.Message != legacy.Message {
		t.Errorf("facade rejection = %+v, want legacy classification %+v", rej, legacy)
	}
}

// N4a：无计费账户 = 无有效订阅 → ErrChatNoAccess（403），对齐 legacy
// accessPlan；不再是 200 空列表（picker 对 403 隐藏选择器，200 空数组会
// 让 picker 静默消失且与服务故障无法区分）。
func TestChatFacade_AllowedModels_NoAccountIsNoAccess(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	f := NewChatGatewayFacade(nil, resolver, nil, nil, "m")
	models, err := f.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if !errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("err = %v, want ErrChatNoAccess（无计费账户 → 403，对齐 legacy）", err)
	}
	if models != nil {
		t.Errorf("models = %+v, want nil（403 下不返回列表）", models)
	}
}

func TestChatFacade_AllowedModels_TransientErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("db connection reset")
	resolver := access.NewResolver(&facadeKeyStore{accountErr: boom}, nil)
	f := NewChatGatewayFacade(nil, resolver, nil, nil, "m")
	_, err := f.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 透传 %v（瞬时故障不得吞掉伪装成权限语义）", err, boom)
	}
}

// --- R7-N1：facade 请求期就绪闸门（替代 boot 期 log.Fatalf） ---

// 评审修复 C1 主测试：真实冷启动形态——store 对 head 探针返回裸
// CodeNotFound（postgres mapError 对「无 active revision」的真实产出，
// 不带哨兵），经**真实 SnapshotCache.Current** 后才携带
// ErrNoVerifiedSnapshot；facade 走 cache 而非直连 store，故映射 503。
func TestChatFacade_StreamChat_RealColdStartThroughSnapshotCache(t *testing.T) {
	t.Parallel()
	src := &facadeRevisionSource{
		headErr: domain.NewError(domain.CodeNotFound, "no active catalog revision"),
	}
	cache := catalog.NewSnapshotCache(src, func(error) {})
	if _, err := cache.Current(context.Background()); !errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
		t.Fatalf("cache.Current err = %v, want 携带 ErrNoVerifiedSnapshot 哨兵", err)
	}
	f := NewChatGatewayFacade(nil, nil, nil, cache, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（真实冷启动形态 → 503，不得 403/500）", err)
	}
	if errors.Is(err, ErrChatNoAccess) {
		t.Fatal("冷启动失败不得伪装成 403 无权限")
	}
}

// 默认模型不在已发布快照 → ErrChatNotReady（503），且不触碰 gateway。
// gw 传 nil：闸门若未短路，ChatCompletions 会 nil deref 让测试立刻失败。
func TestChatFacade_StreamChat_DefaultModelMissingIsNotReady(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	snaps := stubSnapshotSource{snap: &catalog.Snapshot{Models: map[string]domain.Model{}}}
	f := NewChatGatewayFacade(nil, resolver, nil, snaps, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（默认模型不在快照 → 503，gateway 不得被调用）", err)
	}
}

// 快照冷启动错误（携带 ErrNoVerifiedSnapshot 哨兵）→ ErrChatNotReady。
func TestChatFacade_StreamChat_ColdStartSentinelIsNotReady(t *testing.T) {
	t.Parallel()
	cold := fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot)
	f := NewChatGatewayFacade(nil, nil, nil, stubSnapshotSource{err: cold}, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（冷启动哨兵 → 503）", err)
	}
	if errors.Is(err, ErrChatNoAccess) {
		t.Fatal("冷启动失败不得伪装成 403 无权限")
	}
}

// 就绪检查在 ResolveUserSession 之前：无计费账户的用户（旧语义 403）在
// 目录冷启动失败时也必须拿到 503 —— 验收 §7.1/7.2 与用户状态无关。
func TestChatFacade_StreamChat_ReadinessBeforeResolveUserSession(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	cold := fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot)
	f := NewChatGatewayFacade(nil, resolver, nil, stubSnapshotSource{err: cold}, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（服务未就绪优先于用户权限分层）", err)
	}
}

// N4b：resolver CodeNotFound（无计费账户）必须在 resolver 分支显式映射为
// ErrChatNoAccess（403）——mapGatewayError 拆分后 CodeNotFound 已改指
// ErrChatUnknownModel（400 未知模型），若无显式分支，无账户用户会被误报
// 成「未知模型」。gw 传 nil：解析失败时不会触达。
func TestChatFacade_StreamChat_NoAccountResolverIsNoAccess(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	snaps := stubSnapshotSource{snap: &catalog.Snapshot{
		Models: map[string]domain.Model{"glm-4.6": {ID: "glm-4.6"}},
	}}
	f := NewChatGatewayFacade(nil, resolver, nil, snaps, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("err = %v, want ErrChatNoAccess（无计费账户 → 403，不得误报未知模型）", err)
	}
	if errors.Is(err, ErrChatUnknownModel) {
		t.Fatal("resolver CodeNotFound 不得落入 ErrChatUnknownModel（那是 gateway 目录查无此 id 的语义）")
	}
}

// --- R7-N3：目录刷新失败继续服务旧快照（facade 层钉） ---
// SnapshotCache 层的 absorb 语义已由 snapshot_cache_edges_test.go /
// service_test.go 覆盖，这里只钉 facade 侧的可观察行为：刷新失败窗口内
// 就绪闸门照常放行（请求推进到用户解析 = 业务继续，绝不退化成 503），且
// OnRefreshError 告警被调用。

// flakyFacadeRevisionSource 手搓桩：healthy 翻 false 后 head 探针与全量
// 加载都失败——模拟「已发布后 DB 故障」的刷新失败窗口（对比
// facadeRevisionSource 的固定冷启动形态）。
type flakyFacadeRevisionSource struct {
	healthy bool
	rev     *domain.ConfigRevision
	err     error
}

func (s *flakyFacadeRevisionSource) ActiveRevisionHead(context.Context, domain.ConfigScope) (int64, int, error) {
	if !s.healthy {
		return 0, 0, s.err
	}
	return s.rev.ID, s.rev.Revision, nil
}

func (s *flakyFacadeRevisionSource) ActiveRevision(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
	if !s.healthy {
		return nil, s.err
	}
	return s.rev, nil
}

// facadeReadyRevision 是含默认模型 glm-4.6 的最小合法修订（就绪闸门第二段
// ——默认模型必须在快照内——也要能过）。
func facadeReadyRevision() *domain.ConfigRevision {
	return &domain.ConfigRevision{
		ID: 7, Scope: domain.ScopeCatalog, Revision: 3, IsActive: true,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: json.RawMessage(
			`{"schema_version":1,` +
				`"models":[{"id":"glm-4.6","display_name":"GLM","lifecycle":"active","model_version":"",` +
				`"aliases":[],"input_modalities":["text"],"output_modalities":["text"],` +
				`"context_tokens":1000,"max_output_tokens":100,"protocols":["kaya_chat"],` +
				`"supports_tools":false,"supports_reasoning":false}],` +
				`"providers":[],"deployments":[],"routes":[]}`)},
	}
}

func TestChatFacade_RefreshFailureServesLastVerifiedSnapshot(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("db gone")
	src := &flakyFacadeRevisionSource{healthy: true, rev: facadeReadyRevision(), err: errBoom}
	absorbed := 0
	cache := catalog.NewSnapshotCache(src, func(error) { absorbed++ })
	// 无计费账户的 resolver：闸门放行后的下一站是 ResolveUserSession →
	// ErrChatNoAccess（403 语义）。gw 传 nil：若闸门误判 503 之前推进到
	// gateway 会 nil deref 立刻暴露；正常路径根本不触达 gw。
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	f := NewChatGatewayFacade(nil, resolver, nil, cache, "glm-4.6")

	// 首次：快照加载成功，闸门放行 → 推进到用户解析（403 语义证明过了闸门）。
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("warm err = %v, want ErrChatNoAccess（就绪闸门放行后推进到用户解析）", err)
	}
	// 刷新失败窗口：cache 兜底旧快照，闸门继续放行——可观察行为是不退化
	// 成 503（ErrChatNotReady），业务继续；告警必须触发（静默兜底会藏住
	// 卡死的目录）。
	src.healthy = false
	_, _, err = f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if errors.Is(err, ErrChatNotReady) {
		t.Fatal("refresh failure with a verified snapshot must NOT degrade to 503（继续服务旧快照）")
	}
	if !errors.Is(err, ErrChatNoAccess) {
		t.Fatalf("post-failure err = %v, want ErrChatNoAccess（请求照常推进到用户解析）", err)
	}
	if absorbed == 0 {
		t.Error("OnRefreshError must fire on the absorbed refresh failure")
	}
}
