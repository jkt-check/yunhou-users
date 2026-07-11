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

// wechatOAuthDeps bundles the service-layer dependencies for the WeChat
// redirect flow. Same shape as githubOAuthDeps.
type wechatOAuthDeps struct {
	svc     *service.WeChatOAuthService
	appRepo appLoader
	authSvc service.AuthServiceInterface
}

// RegisterWeChatOAuthRoutes attaches /auth/wechat/redirect and
// /auth/wechat/callback to the engine. Both endpoints are public (no
// JWT) — same posture as /auth/github/* and /auth/refresh.
func RegisterWeChatOAuthRoutes(engine gin.IRouter, svc *service.WeChatOAuthService, appRepo appLoader, authSvc service.AuthServiceInterface) {
	d := &wechatOAuthDeps{svc: svc, appRepo: appRepo, authSvc: authSvc}
	engine.GET("/auth/wechat/redirect", d.Redirect)
	engine.GET("/auth/wechat/callback", d.Callback)
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
	if ghErr := c.Query("error"); ghErr != "" {
		ghErrDesc := c.Query("error_description")
		state := c.Query("state")
		appID := c.Query("app_id")
		if state != "" && appID != "" {
			if _, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock()); err == nil {
				if app, err := d.appRepo.FindByID(c.Request.Context(), appID); err == nil {
					cfg, _, lerr := lookupWeChatConfig(app, "")
					if lerr == nil && len(cfg.CallbackURLs) > 0 {
						redirect := cfg.CallbackURLs[0]
						frag := url.Values{}
						frag.Set("error", ghErr)
						if ghErrDesc != "" {
							frag.Set("error_description", ghErrDesc)
						}
						c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirect, nil)+"&"+frag.Encode())
						return
					}
				}
			}
		}
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": ghErr + ": " + ghErrDesc})
		return
	}

	appID := c.Query("app_id")
	code := c.Query("code")
	state := c.Query("state")
	if appID == "" || code == "" || state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "app_id, code, state are required"})
		return
	}

	// First verify: gate on signature + expiry.
	if _, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock()); err != nil {
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

	// Pick the verified callback URL from state so the BFF's
	// redirect_uri matches what WeChat registered.
	verifiedIdx, verr := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock())
	if verr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state index"})
		return
	}
	redirectURI := cfg.CallbackURLs[verifiedIdx]

	tok, err := d.svc.ExchangeCode(c.Request.Context(), cfg, code, redirectURI)
	if err != nil {
		if errors.Is(err, service.ErrWeChatUpstream) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_upstream")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "exchange code"})
		return
	}

	profile, err := d.svc.FetchWeChatProfile(c.Request.Context(), tok.AccessToken, tok.OpenID)
	if err != nil {
		if errors.Is(err, service.ErrWeChatNoUnionID) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_no_unionid")
			return
		}
		if errors.Is(err, service.ErrWeChatUpstream) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_upstream")
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
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason="+url.QueryEscape(authErrReason(err)))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login"})
		return
	}

	c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, loginResp))
}