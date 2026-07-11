package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// wechatAccessToken is the parsed shape of /sns/oauth2/access_token's
// success body. unionid is intentionally NOT included here — the handler
// reads unionid from FetchWeChatProfile's response so there's a single
// source of truth and a single missing-unionid sentinel path.
type wechatAccessToken struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	OpenID       string `json:"openid"`
	Scope        string `json:"scope"`
}

// ExchangeCode trades the auth code WeChat returned for an access token
// + openid. WeChat's code-exchange endpoint is GET (not POST like
// GitHub's). Returns the parsed token struct — caller is responsible for
// using access_token immediately and not retaining it.
func (s *WeChatOAuthService) ExchangeCode(ctx context.Context, cfg *model.WeChatOAuthConfig, code, redirectURI string) (*wechatAccessToken, error) {
	if cfg == nil || cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, ErrWeChatNotConfigured
	}
	if code == "" {
		return nil, errors.New("empty code")
	}

	q := url.Values{}
	q.Set("appid", cfg.AppID)
	q.Set("secret", cfg.AppSecret)
	q.Set("code", code)
	q.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.accessTokenURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build access_token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: access_token endpoint returned %d", ErrWeChatUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrWeChatUpstream, err)
	}

	// Inspect for errcode BEFORE decoding as access_token — a 200 body
	// with errcode is a failure, not a token.
	var errResp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, errResp.ErrCode, errResp.ErrMsg)
	}

	var parsed wechatAccessToken
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("%w: empty access_token in response", ErrWeChatUpstream)
	}
	return &parsed, nil
}

// FetchWeChatProfile calls /sns/userinfo using the access_token + openid
// returned by ExchangeCode. Returns a ProviderUserInfo with
// provider="wechat" and provider_uid="wechat_<unionid>". The
// access_token is used exactly once; the caller MUST drop it after this
// returns.
//
// Email is always "" — WeChat's /sns/userinfo does NOT expose email.
// This means WeChat identities can never trigger the cross-provider
// email-merge in AuthService.resolveOrCreateUser; a WeChat-only user
// always gets a fresh Yunhou account on first login. (Design doc §4.)
func (s *WeChatOAuthService) FetchWeChatProfile(ctx context.Context, accessToken, openID string) (*ProviderUserInfo, error) {
	if accessToken == "" {
		return nil, errors.New("empty access token")
	}
	if openID == "" {
		return nil, errors.New("empty openid")
	}

	q := url.Values{}
	q.Set("access_token", accessToken)
	q.Set("openid", openID)
	q.Set("lang", "zh_CN")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.userInfoURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build userinfo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: userinfo endpoint returned %d", ErrWeChatUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrWeChatUpstream, err)
	}

	var errResp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, errResp.ErrCode, errResp.ErrMsg)
	}

	var parsed struct {
		OpenID     string `json:"openid"`
		UnionID    string `json:"unionid"`
		Nickname   string `json:"nickname"`
		HeadImgURL string `json:"headimgurl"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}

	if parsed.UnionID == "" {
		// Design decision: we REQUIRE unionid. A userinfo response
		// without it means snsapi_userinfo was not granted (e.g. the
		// operator forgot to register the scope, or the user denied
		// it). Reject the login rather than silently creating a
		// per-app identity.
		return nil, ErrWeChatNoUnionID
	}

	return &ProviderUserInfo{
		Provider:    "wechat",
		ProviderUID: "wechat_" + parsed.UnionID,
		Email:       "",
		Nickname:    parsed.Nickname,
		AvatarURL:   parsed.HeadImgURL,
	}, nil
}