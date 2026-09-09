// service_auth.go — 自托管服务的独立接入适配器（设计 §8: 用独立接入适配
// 器支持自托管服务认证，不强迫 OSS 模型使用 OAuth）。
//
// 自托管部署（Provider.AccessType = self_hosted，凭据 auth_type='service'
// 或 'api_key'）不参与任何 OAuth 授权流：没有 authorize/callback/refresh
// 状态机，凭据静态（由 Task 4 的凭据生命周期管理轮换）。本文件只提供
// 健康探测，让健康 worker 对 OAuth 与自托管账号走同一观察口径，
// 而授权语义完全分离。

package connector

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// ServiceAuth probes self-hosted deployments with their static secret.
type ServiceAuth struct {
	HTTP HTTPDoer
}

// Check pings the deployment's base URL (health endpoint when configured)
// with the static service token. A 401/403 means the static credential was
// rotated away upstream — classified reauth_required so the health worker
// stops scheduling the account until an operator fixes the credential;
// transport/5xx stay retryable (account cooldown, not reauth).
func (s *ServiceAuth) Check(ctx context.Context, healthURL string, secret []byte) error {
	if healthURL == "" {
		return &Error{Kind: KindInvalid, Msg: "service-auth check needs a health url"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return &Error{Kind: KindInvalid, Msg: "build service-auth probe", Err: err}
	}
	if len(secret) > 0 {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	}
	h := s.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return &Error{Kind: KindRetryable, Msg: "service endpoint unreachable", Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, bodyCap))
	switch {
	case resp.StatusCode < 400:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &Error{Kind: KindReauthRequired, StatusCode: resp.StatusCode, Msg: "static service credential rejected"}
	default:
		return &Error{Kind: KindRetryable, StatusCode: resp.StatusCode, Msg: "service endpoint unhealthy"}
	}
}
