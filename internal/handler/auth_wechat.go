package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// wechatOAuthClock is the wall clock used by the WeChat redirect +
// callback handlers for state expiry checks. Tests can swap it via
// installWeChatFixedClock.
var wechatOAuthClock = time.Now

// wechatOAuthMockCode is the deterministic code that /auth/wechat/redirect
// emits when mock mode is enabled. /auth/wechat/callback detects it and
// short-circuits the upstream exchange + userinfo fetch.
const wechatOAuthMockCode = "mock-code"

// wechatOAuthMockUnionID is the deterministic unionid embedded in the
// mock ProviderUserInfo. Same shape as the real unionid, prefixed with
// "wechat_" so the identity-key composition matches the production path.
const wechatOAuthMockUnionID = "wechat_mock-unionid-001"

// wechatOAuthDeps bundles the service-layer dependencies for the WeChat
// redirect flow. Same shape as githubOAuthDeps.
type wechatOAuthDeps struct {
	svc     *service.WeChatOAuthService
	appRepo appLoader
	authSvc service.AuthServiceInterface
	mock    bool
}

// RegisterWeChatOAuthRoutes attaches /redirect and /callback to the
// given router. Called from router.Setup() with an
// `engine.Group("/auth/wechat", ...)` so the routes resolve at
// /auth/wechat/redirect + /auth/wechat/callback.
//
// Both endpoints are public (no JWT) — same posture as /auth/github/* and
// /auth/refresh.
//
// The mock parameter enables the dev-only short-circuit
// (WECHAT_OAUTH_MOCK=1). When true, /redirect emits a BFF redirect
// with code=mock-code (no open.weixin.qq.com round-trip), and /callback
// constructs a fixed ProviderUserInfo with unionid=wechat_mock-unionid-001
// without calling /sns/oauth2/access_token or /sns/userinfo. State
// issuance + verification still runs unmodified — mock mode only
// bypasses the upstream WeChat HTTP calls, not the HMAC defence.
func RegisterWeChatOAuthRoutes(engine gin.IRouter, svc *service.WeChatOAuthService, appRepo appLoader, authSvc service.AuthServiceInterface, mock bool) {
	d := &wechatOAuthDeps{svc: svc, appRepo: appRepo, authSvc: authSvc, mock: mock}
	engine.GET("/redirect", d.Redirect)
	engine.GET("/callback", d.Callback)
}

// Redirect handles GET /auth/wechat/redirect?app_id=...&redirect_uri=...
func (d *wechatOAuthDeps) Redirect(c *gin.Context) {
	appID := c.Query("app_id")
	redirectURI := c.Query("redirect_uri")
	if appID == "" || redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "app_id and redirect_uri are required"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if !app.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "app is inactive"})
		return
	}

	cfg, callbackIdx, err := lookupWeChatConfig(app, redirectURI)
	if err != nil {
		if errors.Is(err, service.ErrWeChatNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "wechat oauth not configured for app"})
			return
		}
		if errors.Is(err, service.ErrWeChatCallbackURLMismatch) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "redirect_uri not in callback_urls whitelist"})
			return
		}
		// Malformed config — operators need to fix
		// apps.config.oauth_providers.wechat.
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "invalid app config"})
		return
	}

	if d.mock {
		// Mock-mode redirect: issue a real HMAC-signed state (so the
		// callback's VerifyCallbackState still runs unmodified) and
		// redirect straight back to the BFF with the mock code. No
		// upstream WeChat HTTP call.
		state, err := d.svc.IssueState(appID, callbackIdx, wechatOAuthClock())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "issue mock state"})
			return
		}
		c.Redirect(http.StatusFound, redirectWithFragment(redirectURI, url.Values{
			"code":  {wechatOAuthMockCode},
			"state": {state},
		}))
		return
	}

	authorizeURL, err := d.svc.BuildAuthorizeURL(appID, cfg, callbackIdx, wechatOAuthClock())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "build authorize url"})
		return
	}

	c.Redirect(http.StatusFound, authorizeURL)
}

// lookupWeChatConfig mirrors lookupGitHubConfig (auth_github.go). Returns
// (cfg, callbackIdx, nil) when redirect_uri matches a whitelisted entry;
// (nil, -1, ErrWeChatNotConfigured) when no WeChat block exists or it has
// no callback URLs; (nil, 0, ErrWeChatCallbackURLMismatch) when
// redirect_uri is not in the whitelist.
func lookupWeChatConfig(app *model.App, redirectURI string) (cfg *model.WeChatOAuthConfig, callbackIdx int, err error) {
	if app == nil || len(app.Config) == 0 {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	var ac model.AppConfig
	if err := json.Unmarshal(app.Config, &ac); err != nil {
		return nil, 0, err
	}
	if ac.OAuthProviders == nil || ac.OAuthProviders.WeChat == nil {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	cfg = ac.OAuthProviders.WeChat
	if len(cfg.CallbackURLs) == 0 {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	if redirectURI == "" {
		return cfg, 0, nil
	}
	want := normalizeCallbackURLForCompare(redirectURI)
	for i, u := range cfg.CallbackURLs {
		if normalizeCallbackURLForCompare(u) == want {
			return cfg, i, nil
		}
	}
	return nil, 0, service.ErrWeChatCallbackURLMismatch
}

// Callback handles GET /auth/wechat/callback?app_id=...&code=...&state=...
// WeChat calls this URL after the user scans the QR code and confirms
// (or denies) authorization. On success we exchange the code, fetch the
// user profile, and redirect to the BFF with Yunhou's JWT in the
// fragment.
func (d *wechatOAuthDeps) Callback(c *gin.Context) {
	// WeChat-side denial: ?error=access_denied&error_description=...
	// Mirror the GitHub error-param pattern — redirect to the BFF with
	// the error echoed in the fragment so the BFF can show a localized
	// message.
	if upstreamErr := c.Query("error"); upstreamErr != "" {
		upstreamErrDesc := c.Query("error_description")
		state := c.Query("state")
		appID := c.Query("app_id")
		if state != "" && appID != "" {
			if idx, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock()); err == nil {
				if app, err := d.appRepo.FindByID(c.Request.Context(), appID); err == nil {
					cfg, _, lerr := lookupWeChatConfig(app, "")
					if lerr == nil && idx >= 0 && idx < len(cfg.CallbackURLs) {
						frag := url.Values{}
						frag.Set("error", upstreamErr)
						if upstreamErrDesc != "" {
							frag.Set("error_description", upstreamErrDesc)
						}
						c.Redirect(http.StatusFound, redirectWithFragment(cfg.CallbackURLs[idx], frag))
						return
					}
				}
			}
		}
		// Fallback when state verify / app lookup fails — surface the
		// upstream error verbatim. Drop the trailing ": " that would
		// otherwise appear when error_description is empty.
		msg := upstreamErr
		if upstreamErrDesc != "" {
			msg = upstreamErr + ": " + upstreamErrDesc
		}
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": msg})
		return
	}

	appID := c.Query("app_id")
	code := c.Query("code")
	state := c.Query("state")
	if appID == "" || code == "" || state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "app_id, code, state are required"})
		return
	}

	// Verify state once and reuse the verified index below. A second
	// verify call would re-run the HMAC + nonce lookup for nothing
	// (and can disagree across the expiry boundary because the two
	// reads of wechatOAuthClock() see slightly different times).
	verifiedIdx, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}

	cfg, _, err := lookupWeChatConfig(app, "")
	if err != nil {
		if errors.Is(err, service.ErrWeChatNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "wechat oauth not configured for app"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "invalid app config"})
		return
	}

	// Bounds-check before indexing. An operator can shorten the
	// callback_urls list between state-issue and callback; the state
	// token's index would then be out of range, and a bare indexing
	// would panic the request. Mirrors auth_github.go:204-206.
	if verifiedIdx < 0 || verifiedIdx >= len(cfg.CallbackURLs) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid callback index"})
		return
	}
	redirectURI := cfg.CallbackURLs[verifiedIdx]

	if d.mock && code == wechatOAuthMockCode {
		// Mock-mode callback: skip /sns/oauth2/access_token and
		// /sns/userinfo. Build a deterministic ProviderUserInfo that
		// reuses the production identity-key shape (wechat_<unionid>)
		// so the rest of the login pipeline doesn't see a special case.
		profile := &service.ProviderUserInfo{
			Provider:    "wechat",
			ProviderUID: wechatOAuthMockUnionID,
			Email:       "",
			Nickname:    "mock-user",
			AvatarURL:   "",
		}
		loginResp, err := d.authSvc.LoginWithProfile(c.Request.Context(), service.LoginWithProfileRequest{
			Profile: profile,
			AppID:   appID,
		})
		if err != nil {
			if isExpectedAuthErr(err) {
				c.Redirect(http.StatusFound, redirectWithErrorFragment(redirectURI, "auth_failed", authErrReason(err)))
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login"})
			return
		}
		c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, loginResp))
		return
	}

	tok, err := d.svc.ExchangeCode(c.Request.Context(), cfg, code, redirectURI)
	if err != nil {
		if errors.Is(err, service.ErrWeChatUpstream) {
			c.Redirect(http.StatusFound, redirectWithErrorFragment(redirectURI, "auth_failed", "wechat_upstream"))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "exchange code"})
		return
	}

	profile, err := d.svc.FetchWeChatProfile(c.Request.Context(), tok.AccessToken, tok.OpenID)
	if err != nil {
		if errors.Is(err, service.ErrWeChatNoUnionID) {
			c.Redirect(http.StatusFound, redirectWithErrorFragment(redirectURI, "auth_failed", "wechat_no_unionid"))
			return
		}
		if errors.Is(err, service.ErrWeChatUpstream) {
			c.Redirect(http.StatusFound, redirectWithErrorFragment(redirectURI, "auth_failed", "wechat_upstream"))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "fetch profile"})
		return
	}

	loginResp, err := d.authSvc.LoginWithProfile(c.Request.Context(), service.LoginWithProfileRequest{
		Profile: profile,
		AppID:   appID,
	})
	if err != nil {
		if isExpectedAuthErr(err) {
			c.Redirect(http.StatusFound, redirectWithErrorFragment(redirectURI, "auth_failed", authErrReason(err)))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login"})
		return
	}

	c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, loginResp))
}