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
	wechatAuthorizeURL   = "https://open.weixin.qq.com/connect/qrconnect"
	wechatAccessTokenURL = "https://api.weixin.qq.com/sns/oauth2/access_token"
	wechatUserInfoURL    = "https://api.weixin.qq.com/sns/userinfo"
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
		authorizeURL:   wechatAuthorizeURL,
		accessTokenURL: wechatAccessTokenURL,
		userInfoURL:    wechatUserInfoURL,
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
// include unionid. This happens when the website app is not bound to a
// WeChat Open Platform account (unionid only exists per Open Platform
// account) — NOT because of scope: snsapi_login is sufficient for
// /sns/userinfo on a 网站应用. We require unionid for cross-app identity
// unification, so we reject the login.
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
	// 2026-07-23: scope MUST be snsapi_login alone for a 网站应用
	// (qrconnect). snsapi_userinfo belongs to the 公众号 (mp) platform's
	// oauth2/authorize flow — appending it here makes WeChat render a
	// second, mobile-style "申请使用你的昵称、头像" consent dialog with
	// 拒绝/允许 buttons after the QR scan; clicking 拒绝 fails the login
	// (verified live on cn-staging). With snsapi_login alone,
	// /sns/userinfo still returns nickname/headimgurl/unionid for a
	// website app bound to an Open Platform account.
	q.Set("scope", "snsapi_login")
	q.Set("state", state)
	return s.authorizeURL + "?" + q.Encode() + "#wechat_redirect", nil
}

// VerifyCallbackState confirms the state token WeChat echoed back came
// from our /auth/wechat/redirect handler. Returns callbackIndex.
func (s *WeChatOAuthService) VerifyCallbackState(state, expectedAppID string, now time.Time) (int, error) {
	return util.VerifyOAuthState(s.stateSecret, state, expectedAppID, now)
}

// IssueState signs and returns the OAuth state token for an
// (appID, callbackIndex) pair. Used by the mock-mode redirect handler,
// which builds its own BFF redirect URL with code=mock-code instead of
// an upstream WeChat authorize URL. Returning a real HMAC-signed state
// keeps the callback's VerifyCallbackState working unchanged — mock
// mode doesn't bypass security checks, it only short-circuits the
// upstream WeChat round-trip.
func (s *WeChatOAuthService) IssueState(appID string, callbackIndex int, now time.Time) (string, error) {
	return util.IssueOAuthState(s.stateSecret, appID, callbackIndex, now)
}

// wechatAccessToken is the parsed shape of /sns/oauth2/access_token's
// body. Includes ErrCode/ErrMsg alongside the success fields so a
// single json.Unmarshal detects both success and the upstream error
// envelope. unionid is intentionally NOT included here — the handler
// reads unionid from FetchWeChatProfile's response so there's a single
// source of truth and a single missing-unionid sentinel path.
type wechatAccessToken struct {
	ErrCode      int    `json:"errcode"`
	ErrMsg       string `json:"errmsg"`
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

	// Single-struct decode — includes both success and errcode/errmsg
	// fields so we surface upstream errors without a second json pass.
	// Mirrors github_oauth.go ExchangeCode.
	var parsed wechatAccessToken
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}
	if parsed.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, parsed.ErrCode, parsed.ErrMsg)
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
		return nil, fmt.Errorf("%w: empty access token", ErrWeChatUpstream)
	}
	if openID == "" {
		// Caller passed an empty openid (e.g. ExchangeCode returned a
		// 200 body without an openid field). Wrap as upstream so the
		// handler routes to the BFF fragment (#error=auth_failed
		// &reason=wechat_upstream) instead of stranding the user on a
		// 500 JSON page.
		return nil, fmt.Errorf("%w: empty openid", ErrWeChatUpstream)
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

	// Single-struct decode — ErrCode/ErrMsg share the body with the
	// success fields so we surface upstream errors without a second
	// json pass.
	var parsed struct {
		ErrCode    int    `json:"errcode"`
		ErrMsg     string `json:"errmsg"`
		OpenID     string `json:"openid"`
		UnionID    string `json:"unionid"`
		Nickname   string `json:"nickname"`
		HeadImgURL string `json:"headimgurl"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}
	if parsed.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, parsed.ErrCode, parsed.ErrMsg)
	}

	if parsed.UnionID == "" {
		// Design decision: we REQUIRE unionid. A userinfo response
		// without it means the website app is not bound to a WeChat
		// Open Platform account (unionid is per-Open-Platform-account).
		// Reject the login rather than silently creating a per-app
		// identity.
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