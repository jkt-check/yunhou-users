package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// chat_facade_test.go — 评审批次7 Minor-4/5 验收：
// Minor-4：CodeUnpricedCapability（运营定价配置错误）映射为 500 类而不是
// 403「无权限」；Minor-5：AllowedModels 只吞「无计费账户」哨兵，其余错误
// 记录日志并透传。

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
		// 真实权益拒绝保持 403 语义。
		{"model_not_allowed", domain.NewError(domain.CodeModelNotAllowed, "denied"), ErrChatNoAccess},
		{"invalid_key", domain.NewError(domain.CodeInvalidKey, "denied"), ErrChatNoAccess},
		{"not_found", domain.NewError(domain.CodeNotFound, "no account"), ErrChatNoAccess},
		{"rate_limited", domain.NewError(domain.CodeRateLimited, "slow down"), ErrChatRateLimited},
		{"internal", domain.NewError(domain.CodeInternal, "boom"), ErrChatUpstreamError},
		// R7-N1：目录冷启动（无已验证快照）哨兵 → 503，不得伪装成 403/500。
		{"no_verified_snapshot", fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot), ErrChatNotReady},
		// 评审修复 C1 验证：gateway 把 Current 错误包成 CodeInternal
		// （gateway/service.go:310），domain.Error.Unwrap 暴露 cause，
		// 哨兵穿透 WrapError 后仍被 errors.Is 命中。
		{"wrapped_no_verified_snapshot", domain.WrapError(domain.CodeInternal, "gateway: catalog snapshot unavailable",
			fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot)), ErrChatNotReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapGatewayError(c.err); !errors.Is(got, c.want) {
				t.Errorf("mapGatewayError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestChatFacade_AllowedModels_NoAccountIsEmptyList(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	f := NewChatGatewayFacade(nil, resolver, nil, nil, "m")
	models, err := f.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if err != nil {
		t.Fatalf("AllowedModels: %v（无计费账户是正常态，不得报错）", err)
	}
	if len(models) != 0 {
		t.Errorf("models = %+v, want empty (picker UX)", models)
	}
}

func TestChatFacade_AllowedModels_TransientErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("db connection reset")
	resolver := access.NewResolver(&facadeKeyStore{accountErr: boom}, nil)
	f := NewChatGatewayFacade(nil, resolver, nil, nil, "m")
	_, err := f.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 透传 %v（瞬时故障不得吞掉伪装成空列表）", err, boom)
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
