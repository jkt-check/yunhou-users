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

// facadeCatalogStore 是 catalog.Store 的手写桩（同 facadeKeyStore 风格）：
// 只有 ActiveRevision 对本测试有意义（Service.LoadSnapshot = ActiveRevision
// + ParseSnapshot），其余方法一律 not implemented。
type facadeCatalogStore struct {
	rev *domain.ConfigRevision
	err error
}

var _ catalog.Store = (*facadeCatalogStore)(nil)

func (s *facadeCatalogStore) ActiveRevision(context.Context, domain.ConfigScope) (*domain.ConfigRevision, error) {
	return s.rev, s.err
}

func (s *facadeCatalogStore) InsertModel(context.Context, *domain.Model) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) GetModel(context.Context, string) (*domain.Model, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ListModels(context.Context, domain.ModelFilter) ([]domain.Model, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) UpdateModel(context.Context, *domain.Model) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) DeleteModel(context.Context, string) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) InsertProvider(context.Context, *domain.Provider) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) GetProvider(context.Context, string) (*domain.Provider, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) GetProviderByCode(context.Context, string) (*domain.Provider, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ListProviders(context.Context, string, int) ([]domain.Provider, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) UpdateProvider(context.Context, *domain.Provider) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) DeleteProvider(context.Context, string) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) InsertDeployment(context.Context, *domain.Deployment) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) GetDeployment(context.Context, string) (*domain.Deployment, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) FindDeployment(context.Context, string, string, string) (*domain.Deployment, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ListDeployments(context.Context, domain.DeploymentFilter) ([]domain.Deployment, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) UpdateDeployment(context.Context, *domain.Deployment) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) DeleteDeployment(context.Context, string) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) InsertRoute(context.Context, *domain.ModelRoute) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) GetRoute(context.Context, string) (*domain.ModelRoute, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ListRoutes(context.Context, string) ([]domain.ModelRoute, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) UpdateRoute(context.Context, *domain.ModelRoute) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) DeleteRoute(context.Context, string) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) InsertConfigRevision(context.Context, *domain.ConfigRevision) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) ActivateRevision(context.Context, domain.ConfigScope, int) error {
	return errors.New("not implemented")
}
func (s *facadeCatalogStore) ActiveRevisionMeta(context.Context, domain.ConfigScope) (*domain.RevisionMeta, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ActiveRevisionHead(context.Context, domain.ConfigScope) (int64, int, error) {
	return 0, 0, errors.New("not implemented")
}
func (s *facadeCatalogStore) GetRevision(context.Context, domain.ConfigScope, int) (*domain.ConfigRevision, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) ListRevisionMetas(context.Context, domain.ConfigScope, int, int) ([]domain.RevisionMeta, error) {
	return nil, errors.New("not implemented")
}
func (s *facadeCatalogStore) LatestRevision(context.Context, domain.ConfigScope) (int, error) {
	return 0, errors.New("not implemented")
}
func (s *facadeCatalogStore) BeginPublish(context.Context) (catalog.PublishTx, error) {
	return nil, errors.New("not implemented")
}

// emptyCatalogRevision 是能通过 ParseSnapshot 的最小合法修订（空目录合法：
// 修订体完整即可，模型集合可为空；仿 catalog 包 validCatalogRevision）。
func emptyCatalogRevision() *domain.ConfigRevision {
	return &domain.ConfigRevision{
		ID:       1,
		Scope:    domain.ScopeCatalog,
		Revision: 1,
		IsActive: true,
		Payload: domain.ExtensionConfig{
			SchemaVersion: 1,
			Raw:           json.RawMessage(`{"schema_version":1,"models":[],"providers":[],"deployments":[],"routes":[]}`),
		},
	}
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
	f := NewChatGatewayFacade(nil, resolver, nil, "m")
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
	f := NewChatGatewayFacade(nil, resolver, nil, "m")
	_, err := f.AllowedModels(context.Background(), "u-1", "yunhou-website")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 透传 %v（瞬时故障不得吞掉伪装成空列表）", err, boom)
	}
}

// --- R7-N1：facade 请求期就绪闸门（替代 boot 期 log.Fatalf） ---

// 默认模型不在已发布快照 → ErrChatNotReady（503），且不触碰 gateway。
// gw 传 nil：闸门若未短路，ChatCompletions 会 nil deref 让测试立刻失败。
func TestChatFacade_StreamChat_DefaultModelMissingIsNotReady(t *testing.T) {
	t.Parallel()
	resolver := access.NewResolver(&facadeKeyStore{
		accountErr: domain.NewError(domain.CodeNotFound, "billing account not found"),
	}, nil)
	cat := catalog.NewService(&facadeCatalogStore{rev: emptyCatalogRevision()})
	f := NewChatGatewayFacade(nil, resolver, cat, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（默认模型不在快照 → 503，gateway 不得被调用）", err)
	}
}

// 快照冷启动错误（携带 ErrNoVerifiedSnapshot 哨兵）→ ErrChatNotReady。
func TestChatFacade_StreamChat_ColdStartSentinelIsNotReady(t *testing.T) {
	t.Parallel()
	cold := fmt.Errorf("%w: active revision unavailable: db gone", catalog.ErrNoVerifiedSnapshot)
	cat := catalog.NewService(&facadeCatalogStore{err: cold})
	f := NewChatGatewayFacade(nil, nil, cat, "glm-4.6")
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
	cat := catalog.NewService(&facadeCatalogStore{err: cold})
	f := NewChatGatewayFacade(nil, resolver, cat, "glm-4.6")
	_, _, err := f.StreamChat(context.Background(), "u-1", "yunhou-website", "", nil, nil, nil)
	if !errors.Is(err, ErrChatNotReady) {
		t.Fatalf("err = %v, want ErrChatNotReady（服务未就绪优先于用户权限分层）", err)
	}
}
