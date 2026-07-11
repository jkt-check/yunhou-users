package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// githubOAuthClock is the wall clock used by the redirect + callback
// handlers for state expiry checks. Production wires it to time.Now;
// tests can swap to a fixed value.
var githubOAuthClock = time.Now

// appLoader is the slice of repo.AppRepo the redirect handler actually
// needs. Defined here (not in internal/repo) so handler tests don't have
// to import the full repo package.
type appLoader interface {
	FindByID(ctx context.Context, appID string) (*model.App, error)
}

// githubOAuthDeps bundles the service-layer dependencies for the GitHub
// redirect flow. Wiring stays in router.go; tests construct this directly.
type githubOAuthDeps struct {
	svc     *service.GitHubOAuthService
	appRepo appLoader
	authSvc service.AuthServiceInterface
	tokenSvc service.TokenServiceInterface
}

// RegisterGitHubOAuthRoutes attaches /auth/github/redirect and
// /auth/github/callback to the engine. Called from router.Setup().
//
// Both endpoints are public (no JWT) — same posture as the existing
// /auth/refresh and /auth/logout. Rate-limited via the public limiter.
func RegisterGitHubOAuthRoutes(engine gin.IRouter, svc *service.GitHubOAuthService, appRepo appLoader, authSvc service.AuthServiceInterface, tokenSvc service.TokenServiceInterface) {
	d := &githubOAuthDeps{svc: svc, appRepo: appRepo, authSvc: authSvc, tokenSvc: tokenSvc}
	engine.GET("/auth/github/redirect", d.Redirect)
	engine.GET("/auth/github/callback", d.Callback)
}

// Redirect handles GET /auth/github/redirect?app_id=...&redirect_uri=...
//
// Yunhou's response is a 302 to GitHub's authorize URL with the bound
// state token. App ID + redirect_uri are validated against the app's
// stored callback_urls whitelist; mismatches return 400.
func (d *githubOAuthDeps) Redirect(c *gin.Context) {
	appID := c.Query("app_id")
	redirectURI := c.Query("redirect_uri")
	if appID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing app_id"})
		return
	}
	if redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing redirect_uri"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		// Don't differentiate app-not-found from app-inactive to the
		// caller — we don't want to confirm app_id existence pre-login.
		log.Printf("github oauth redirect: find app %q: %v", appID, err)
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	// Don't issue a real GitHub authorize URL for a disabled app — the user
	// would complete GitHub consent only to be rejected at /callback with
	// ErrAppInactive, dropping a real OAuth grant on the floor.
	if !app.IsActive {
		log.Printf("github oauth redirect: app %q inactive", appID)
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "app is disabled"})
		return
	}

	cfg, callbackIdx, err := lookupGitHubConfig(app, redirectURI)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrGitHubNotConfigured):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "github login not configured"})
		case errors.Is(err, service.ErrCallbackURLMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "redirect_uri not in callback_urls whitelist"})
		default:
			log.Printf("github oauth redirect: lookup config: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to read app config"})
		}
		return
	}

	authorizeURL, err := d.svc.BuildAuthorizeURL(appID, cfg, callbackIdx, githubOAuthClock())
	if err != nil {
		log.Printf("github oauth redirect: build authorize url: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to build authorize url"})
		return
	}
	c.Redirect(http.StatusFound, authorizeURL)
}

// Callback handles GET /auth/github/callback?code=...&state=...&app_id=...
//
// Flow:
//  1. Surface GitHub's own error parameters (access_denied, etc.) as 400.
//  2. Verify the state token — confirms appID matches what Yunhou
//     issued, that the token is unexpired, and returns the bound
//     callback_index.
//  3. Exchange the code for a GitHub access_token (server-side only).
//  4. Fetch /user + /user/emails using that token, then drop it.
//  5. Call AuthService.Login with provider="github" + the access_token
//     — same path the GitHub-only flow always used, so identity binding
//     stays in one place.
//  6. Redirect to the BFF's callback URL with the yunhou JWT in the
//     URL fragment (not query — fragments don't leak via referer /
//     server logs).
func (d *githubOAuthDeps) Callback(c *gin.Context) {
	if ghErr := c.Query("error"); ghErr != "" {
		// GitHub redirected back with an authorization error (e.g.
		// access_denied). The BFF expected a redirect — sending JSON
		// 400 here strands the browser on Yunhou's page with no way
		// back. Use the same state-verification path as the happy
		// branch so the redirect goes to the same callback URL the
		// user started from (web vs mobile vs dev), then 302 back with
		// the error in the fragment.
		desc := c.Query("error_description")
		appID := c.Query("app_id")
		state := c.Query("state")
		if appID != "" && state != "" {
			if idx, err := d.svc.VerifyCallbackState(state, appID, githubOAuthClock()); err == nil {
				if app, err := d.appRepo.FindByID(c.Request.Context(), appID); err == nil && app != nil {
					cfg, _, lerr := lookupGitHubConfig(app, "")
					if lerr == nil && idx >= 0 && idx < len(cfg.CallbackURLs) {
						frag := url.Values{}
						frag.Set("error", ghErr)
						if desc != "" {
							frag.Set("error_description", desc)
						}
						target, perr := url.Parse(cfg.CallbackURLs[idx])
						if perr == nil {
							target.Fragment = frag.Encode()
							c.Redirect(http.StatusFound, target.String())
							return
						}
					}
				}
			}
		}
		// Fallback (no app_id, no state, lookup failed, etc.): surface
		// the error as JSON so the caller at least sees what went
		// wrong.
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "github authorization failed: " + ghErr,
			"data":    gin.H{"error_description": desc},
		})
		return
	}

	appID := c.Query("app_id")
	code := c.Query("code")
	state := c.Query("state")
	if appID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing app_id"})
		return
	}
	if code == "" || state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing code or state"})
		return
	}

	// Verify state BEFORE we touch the database or upstream. State
	// validation is cheap and tells us whether this is even a
	// legitimate callback.
	verifiedIdx, err := d.svc.VerifyCallbackState(state, appID, githubOAuthClock())
	if err != nil {
		log.Printf("github oauth callback: state verify: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		log.Printf("github oauth callback: find app %q: %v", appID, err)
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}

	cfg, _, err := lookupGitHubConfig(app, "")
	if err != nil {
		if errors.Is(err, service.ErrGitHubNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "github login not configured"})
			return
		}
		log.Printf("github oauth callback: lookup config: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "github login not configured"})
		return
	}
	if verifiedIdx < 0 || verifiedIdx >= len(cfg.CallbackURLs) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid callback index"})
		return
	}
	redirectURI := cfg.CallbackURLs[verifiedIdx]

	accessToken, err := d.svc.ExchangeCode(c.Request.Context(), cfg, code, redirectURI)
	if err != nil {
		log.Printf("github oauth callback: exchange code: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "github upstream error"})
		return
	}

	profile, err := d.svc.FetchGitHubProfile(c.Request.Context(), accessToken)
	if err != nil {
		log.Printf("github oauth callback: fetch profile: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "github upstream error"})
		return
	}

	// Skip the second provider round-trip — AuthService.Login would
	// otherwise re-call fetchGitHubUser on the same token. LoginWithProfile
	// takes the already-fetched profile and runs only the identity-binding
	// + token-issuance portion.
	resp, err := d.authSvc.LoginWithProfile(c.Request.Context(), service.LoginWithProfileRequest{
		Profile: profile,
		AppID:   appID,
	})
	if err != nil {
		log.Printf("github oauth callback: auth login: %v", err)
		// AppNotFound / AppInactive / UserSuspended / etc.: the user
		// already completed GitHub consent — stranding them on a JSON
		// error page is bad UX. Redirect to the BFF callback URL with
		// the error code in the fragment so the BFF can show a
		// meaningful screen.
		if isExpectedAuthErr(err) {
			frag := url.Values{}
			frag.Set("error", "auth_failed")
			frag.Set("reason", authErrReason(err))
			if target, perr := url.Parse(redirectURI); perr == nil {
				target.Fragment = frag.Encode()
				c.Redirect(http.StatusFound, target.String())
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login failed"})
		return
	}

	c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, resp))
}

// lookupGitHubConfig extracts the GitHub OAuth config from app.Config.
// When redirectURI is non-empty (the redirect path), the returned index
// points at the whitelist entry that matches it. When empty (the
// callback path), the returned index is 0; callers should overwrite it
// from the verified state.
func lookupGitHubConfig(app *model.App, redirectURI string) (cfg *model.GitHubOAuthConfig, callbackIdx int, err error) {
	if app == nil || len(app.Config) == 0 {
		return nil, 0, service.ErrGitHubNotConfigured
	}
	var ac model.AppConfig
	if err := json.Unmarshal(app.Config, &ac); err != nil {
		return nil, 0, err
	}
	if ac.OAuthProviders == nil || ac.OAuthProviders.GitHub == nil {
		return nil, 0, service.ErrGitHubNotConfigured
	}
	cfg = ac.OAuthProviders.GitHub
	if len(cfg.CallbackURLs) == 0 {
		return nil, 0, service.ErrGitHubNotConfigured
	}
	if redirectURI == "" {
		return cfg, 0, nil
	}
	// Compare after percent-decoding both sides. c.Query has already
	// decoded the request's redirect_uri, but the operator-supplied
	// cfg entries may carry their own percent-encoding (e.g. "%2F" vs
	// "/") that needs normalising before string-equality works. Without
	// this, a BFF that percent-encodes its callback URL would 400 here
	// even though both URLs are semantically the same.
	want := normalizeCallbackURLForCompare(redirectURI)
	for i, u := range cfg.CallbackURLs {
		if normalizeCallbackURLForCompare(u) == want {
			return cfg, i, nil
		}
	}
	return nil, 0, service.ErrCallbackURLMismatch
}

// normalizeCallbackURLForCompare returns a canonical string for whitelist
// comparison. We intentionally only decode — not re-encode with a fixed
// scheme/host case — because the goal is "do these two strings mean the
// same URL", not "is this URL well-formed". A malformed entry still
// surfaces as ErrCallbackURLMismatch downstream because its decoded form
// will not match the BFF's request.
func normalizeCallbackURLForCompare(s string) string {
	// url.PathUnescape handles "%XX" sequences in the path; for full URL
	// decoding we'd want url.QueryUnescape, but the redirect_uri is a
	// single value (no query string) so PathUnescape is sufficient.
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}