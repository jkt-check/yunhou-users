package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/access"
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
