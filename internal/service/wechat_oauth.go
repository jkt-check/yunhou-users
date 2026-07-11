package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

// WeChat OAuth redirect flow boundary (mirrors the GitHub boundary — see
// github_oauth.go for the full design contract):
//
//   - Yunhou is the only holder of the WeChat Open Platform 网站应用
//     AppSecret. The BFF never sees it.
//   - The state token is provider-agnostic — util.IssueOAuthState /
//     util.VerifyOAuthState bind (appID, callbackIndex) regardless of
//     which provider echoes the state back.
//   - Callback URLs are stored per app in
//     apps.config.oauth_providers.wechat.callback_urls and validated
//     against the incoming redirect_uri on every callback request.
//   - The WeChat access_token Yunhou receives during the code exchange is
//     used in-process only: one call to /sns/userinfo, then dropped.
//     Never written to DB, never returned to the BFF.
//
// Identity key: provider_uid = "wechat_" + unionid. We REJECT logins that
// lack a unionid — the shared-user-identity model across Yunhou consumer
// apps requires unionid, and falling back to openid would silently create
// per-app identity splits.

var (
	wechatAuthorizeURL    = "https://open.weixin.qq.com/connect/qrconnect"
	wechatAccessTokenURL  = "https://api.weixin.qq.com/sns/oauth2/access_token"
	wechatUserInfoURL     = "https://api.weixin.qq.com/sns/userinfo"
	wechatOAuthHTTPClient = &http.Client{Timeout: 10 * time.Second}
)

// WeChatOAuthService is the entry point Yunhou's redirect handler uses
// to build the upstream authorize URL, exchange the auth code, and fetch
// the user's WeChat profile.
type WeChatOAuthService struct {
	stateSecret    []byte
	authorizeURL   string
	accessTokenURL string
	userInfoURL    string
	httpClient     *http.Client
}

// NewWeChatOAuthService builds a service. stateSecret is required at
// request time (IssueOAuthState panics on empty secret).
func NewWeChatOAuthService(stateSecret string) *WeChatOAuthService {
	return &WeChatOAuthService{
		stateSecret:    []byte(stateSecret),
		authorizeURL:   "https://open.weixin.qq.com/connect/qrconnect",
		accessTokenURL: "https://api.weixin.qq.com/sns/oauth2/access_token",
		userInfoURL:    "https://api.weixin.qq.com/sns/userinfo",
		httpClient:     &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient overrides the HTTP client used for upstream calls.
func (s *WeChatOAuthService) SetHTTPClient(c *http.Client) { s.httpClient = c }

// SetAccessTokenURL overrides the upstream access-token endpoint.
func (s *WeChatOAuthService) SetAccessTokenURL(u string) { s.accessTokenURL = u }

// SetAuthorizeURL overrides the upstream authorize endpoint.
func (s *WeChatOAuthService) SetAuthorizeURL(u string) { s.authorizeURL = u }

// SetUserInfoURL overrides the upstream /sns/userinfo endpoint.
func (s *WeChatOAuthService) SetUserInfoURL(u string) { s.userInfoURL = u }

// ErrWeChatNotConfigured signals that apps.config.oauth_providers.wechat
// is absent or empty for the requested app. Mapped to 404 by the handler.
var ErrWeChatNotConfigured = errors.New("wechat oauth not configured for app")

// ErrWeChatCallbackURLMismatch signals the redirect_uri submitted at
// callback time is not in the app's callback_urls whitelist. Mapped to 400.
var ErrWeChatCallbackURLMismatch = errors.New("redirect_uri not in wechat callback_urls whitelist")

// ErrWeChatUpstream signals a non-recoverable error from WeChat itself
// (network, 5xx, decode failure, errcode in body). Mapped to a
// BFF-facing #error=auth_failed&reason=wechat_upstream fragment.
var ErrWeChatUpstream = errors.New("wechat oauth upstream error")

// ErrWeChatNoUnionID signals that the /sns/userinfo response did not
// include unionid. This happens when the website app did not request
// snsapi_userinfo scope or the user did not grant it. We require
// unionid for cross-app identity unification, so we reject the login.
var ErrWeChatNoUnionID = errors.New("wechat userinfo missing unionid")

// BuildAuthorizeURL assembles the upstream WeChat authorize URL. The
// state token binds (appID, callbackIndex). The #wechat_redirect
// fragment is REQUIRED per WeChat docs — without it WeChat returns
// "该链接无法访问".
func (s *WeChatOAuthService) BuildAuthorizeURL(appID string, cfg *model.WeChatOAuthConfig, callbackIndex int, now time.Time) (string, error) {
	if cfg == nil {
		return "", ErrWeChatNotConfigured
	}
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return "", ErrWeChatNotConfigured
	}
	if len(cfg.CallbackURLs) == 0 {
		return "", ErrWeChatNotConfigured
	}
	if callbackIndex < 0 || callbackIndex >= len(cfg.CallbackURLs) {
		return "", fmt.Errorf("%w: callback_index %d out of range", ErrWeChatCallbackURLMismatch, callbackIndex)
	}

	state, err := util.IssueOAuthState(s.stateSecret, appID, callbackIndex, now)
	if err != nil {
		return "", fmt.Errorf("issue state: %w", err)
	}

	q := url.Values{}
	q.Set("appid", cfg.AppID)
	q.Set("redirect_uri", cfg.CallbackURLs[callbackIndex])
	q.Set("response_type", "code")
	q.Set("scope", "snsapi_login,snsapi_userinfo")
	q.Set("state", state)
	return s.authorizeURL + "?" + q.Encode() + "#wechat_redirect", nil
}

// VerifyCallbackState confirms the state token WeChat echoed back came
// from our /auth/wechat/redirect handler. Returns callbackIndex.
func (s *WeChatOAuthService) VerifyCallbackState(state, expectedAppID string, now time.Time) (int, error) {
	return util.VerifyOAuthState(s.stateSecret, state, expectedAppID, now)
}