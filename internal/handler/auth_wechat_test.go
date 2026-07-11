package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// wechatAppWithOAuth builds a minimal *model.App with the WeChat block
// populated. Mirrors ghAppWithOAuth in auth_github_test.go.
func wechatAppWithOAuth(callbackURLs ...string) *model.App {
	cfg := model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			WeChat: &model.WeChatOAuthConfig{
				AppID:        "wx0123456789abcdef",
				AppSecret:    "0123456789abcdef0123456789abcdef",
				CallbackURLs: callbackURLs,
			},
		},
	}
	raw, _ := json.Marshal(cfg)
	return &model.App{AppID: "yundian", Name: "Yundian", Config: raw, IsActive: true}
}

// wechatTestClock is a fixed clock for state expiry control.
var wechatTestClock = func() time.Time { return time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC) }

func installWeChatFixedClock(t *testing.T) {
	t.Helper()
	prev := wechatOAuthClock
	wechatOAuthClock = wechatTestClock
	t.Cleanup(func() { wechatOAuthClock = prev })
}

// =========================================================================
// Redirect tests
// =========================================================================

func TestWeChatOAuth_Redirect_HappyPath(t *testing.T) {
	installWeChatFixedClock(t)
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fbff.example.com%2Fauth%2Fwechat-callback", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://open.weixin.qq.com/connect/qrconnect?") {
		t.Fatalf("location = %q, want prefix open.weixin.qq.com/connect/qrconnect", loc)
	}
	if !strings.HasSuffix(loc, "#wechat_redirect") {
		t.Fatalf("location = %q, missing #wechat_redirect fragment", loc)
	}
	parsed, _ := url.Parse(loc)
	q := parsed.Query()
	if q.Get("appid") != "wx0123456789abcdef" {
		t.Errorf("appid = %q", q.Get("appid"))
	}
}

func TestWeChatOAuth_Redirect_MissingAppID(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Redirect_MissingRedirectURI(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Redirect_AppNotFound(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{err: errors.New("not found")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=missing&redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWeChatOAuth_Redirect_InactiveApp(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	app := wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")
	app.IsActive = false
	appRepo := &stubAppLoader{app: app}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fbff.example.com%2Fauth%2Fwechat-callback", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestWeChatOAuth_Redirect_WeChatNotConfigured(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	// App exists but no WeChat block in config.
	appRepo := &stubAppLoader{app: &model.App{AppID: "yundian", IsActive: true, Config: []byte(`{}`)}}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWeChatOAuth_Redirect_CallbackURLMismatch(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://allowed.example.com/cb")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fattacker.example.com%2Fcb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// =========================================================================
// Callback tests
// =========================================================================

// wechatCallbackURIFor builds a callback URL with a valid state token.
// Returns the request path (not the full BFF URL) because httptest
// dispatches by path, not host.
func wechatCallbackURIFor(t *testing.T, svc *service.WeChatOAuthService, appID, callbackURL string) string {
	t.Helper()
	installWeChatFixedClock(t)
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "01234567890123456789012345678901",
		CallbackURLs: []string{callbackURL},
	}
	u, err := svc.BuildAuthorizeURL(appID, cfg, 0, wechatTestClock())
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	parsed, _ := url.Parse(u)
	return "/auth/wechat/callback?app_id=" + appID + "&code=AUTH_CODE&state=" + parsed.Query().Get("state")
}

// newWeChatStubService builds a WeChatOAuthService that points at two
// httptest servers (one for /access_token, one for /userinfo). Returns
// the service and a cleanup func.
func newWeChatStubService(t *testing.T, exchangeJSON, userInfoJSON string) (*service.WeChatOAuthService, func()) {
	t.Helper()
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, exchangeJSON)
	}))
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, userInfoJSON)
	}))
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	svc.SetAccessTokenURL(exchangeSrv.URL)
	svc.SetUserInfoURL(userInfoSrv.URL)
	return svc, func() {
		exchangeSrv.Close()
		userInfoSrv.Close()
	}
}

func TestWeChatOAuth_Callback_HappyPath(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"access_token":"AT","expires_in":7200,"openid":"oid","scope":"snsapi_login,snsapi_userinfo"}`,
		`{"openid":"oid","unionid":"uid","nickname":"nick","headimgurl":"http://img"}`,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{
		loginResp: &service.LoginResponse{
			AccessToken:  "yunhou-access",
			RefreshToken: "yunhou-refresh",
			User:         service.UserInfo{ID: "user-uuid"},
			Subscription: &service.SubscriptionInfo{HasAccess: true},
		},
	}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cbURL+"#") {
		t.Fatalf("location = %q, want prefix %s#", loc, cbURL)
	}
	if !strings.Contains(loc, "token=yunhou-access") {
		t.Errorf("location missing yunhou access token: %s", loc)
	}
	if !strings.Contains(loc, "refresh_token=yunhou-refresh") {
		t.Errorf("location missing refresh token: %s", loc)
	}
	if !strings.Contains(loc, "user_id=user-uuid") {
		t.Errorf("location missing user_id: %s", loc)
	}
	if !strings.Contains(loc, "has_access=true") {
		t.Errorf("location missing has_access=true: %s", loc)
	}
	if authSvc.calls != 1 {
		t.Errorf("LoginWithProfile called %d times, want 1", authSvc.calls)
	}
}

func TestWeChatOAuth_Callback_MissingUnionID(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"access_token":"AT","openid":"oid"}`,
		`{"openid":"oid","nickname":"nick"}`, // no unionid
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=auth_failed") || !strings.Contains(loc, "reason=wechat_no_unionid") {
		t.Fatalf("location = %q, want error=auth_failed&reason=wechat_no_unionid", loc)
	}
	if authSvc.calls != 0 {
		t.Errorf("LoginWithProfile should not be called when unionid missing")
	}
}

func TestWeChatOAuth_Callback_UpstreamErrcode(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"errcode":40029,"errmsg":"invalid code"}`,
		``,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "reason=wechat_upstream") {
		t.Fatalf("location = %q, want reason=wechat_upstream", loc)
	}
}

func TestWeChatOAuth_Callback_InvalidState(t *testing.T) {
	svc, cleanup := newWeChatStubService(t, `{}`, `{}`)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian&code=x&state=garbage", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Callback_MissingCodeOrState(t *testing.T) {
	svc, _ := newWeChatStubService(t, `{}`, `{}`)
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Callback_WeChatErrorParam(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t, `{}`, `{}`)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	// Build a valid state via the service so the error-param branch
	// can resolve the BFF's redirect_uri and produce a 302.
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "01234567890123456789012345678901",
		CallbackURLs: []string{cbURL},
	}
	authURL, err := svc.BuildAuthorizeURL("yundian", cfg, 0, wechatTestClock())
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	state := mustExtractQueryValue(t, authURL, "state")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian&error=access_denied&error_description=user+denied&state="+state, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=access_denied") {
		t.Errorf("location = %q, want error=access_denied", loc)
	}
	if !strings.Contains(loc, "error_description=user+denied") {
		t.Errorf("location = %q, want error_description=user+denied", loc)
	}
}

// mustExtractQueryValue pulls a query param out of a URL, fataling on
// parse errors or missing values. Mirrors the GitHub helper but uses
// url.Parse (handles the #wechat_redirect fragment correctly).
func mustExtractQueryValue(t *testing.T, raw, key string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := parsed.Query().Get(key)
	if v == "" {
		t.Fatalf("query key %q missing in %s", key, raw)
	}
	return v
}

// TestWeChatOAuth_Callback_WeChatErrorParam_MultiBFF asserts that when
// an app registers multiple callback URLs (web + iOS + Android sharing
// one WeChat 网站应用), the provider's `?error=...` branch routes the
// error fragment to the BFF the user STARTED FROM, not to the first
// entry. State was issued for callback index 1 (mobile); the error
// branch must 302 to cfg.CallbackURLs[1], not [0]. Catches the
// regression where the ghErr branch discarded the verified idx and
// hard-coded [0].
func TestWeChatOAuth_Callback_WeChatErrorParam_MultiBFF(t *testing.T) {
	installWeChatFixedClock(t)
	webCB := "https://bff.example.com/auth/wechat-callback"
	mobileCB := "https://mobile.example.com/auth/wechat-callback"
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")

	// Issue the state for the MOBILE callback (index 1) so the test
	// requires the handler to honour the verified idx rather than
	// falling back to index 0.
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "01234567890123456789012345678901",
		CallbackURLs: []string{webCB, mobileCB},
	}
	authURL, err := svc.BuildAuthorizeURL("yundian", cfg, 1, wechatTestClock())
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	state := mustExtractQueryValue(t, authURL, "state")

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(webCB, mobileCB)}
	authSvc := &stubAuthSvc{}
	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/wechat/callback?app_id=yundian&error=access_denied&error_description=user+denied&state="+state,
		nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, mobileCB+"#") {
		t.Fatalf("location = %q, want prefix %s# (mobile BFF, NOT web)", loc, mobileCB)
	}
	if strings.HasPrefix(loc, webCB+"#") {
		t.Errorf("location = %q routed to web BFF; must use cfg.CallbackURLs[verified idx]", loc)
	}
	if !strings.Contains(loc, "error=access_denied") {
		t.Errorf("location missing error=access_denied fragment: %s", loc)
	}
}

// TestWeChatOAuth_Callback_UpstreamErrcode_FragmentFormat locks in
// the canonical fragment shape (no leading `&`) for the post-login
// error redirect. The earlier code assembled
//
//	buildCallbackRedirectURL(base, nil) + "&" + "error=..."
//
// which produced "<base>#&error=..." — the leading `&` is a parser
// trap and was the same shape across all four call sites. The fix
// introduced redirectWithErrorFragment / redirectWithFragment
// helpers; this test asserts the redirect URL starts with "<base>#"
// rather than "<base>#&".
func TestWeChatOAuth_Callback_UpstreamErrcode_FragmentFormat(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"errcode":40029,"errmsg":"invalid code"}`,
		``,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}
	r := gin.New()
	RegisterWeChatOAuthRoutes(r.Group("/auth/wechat"), svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	// Canonical fragment starts immediately with the first param,
	// not a leading `&` that breaks BFF-side parsing.
	if !strings.HasPrefix(loc, cbURL+"#error=") {
		t.Fatalf("location = %q, want prefix %q (no leading &)", loc, cbURL+"#error=")
	}
	if strings.Contains(loc, "#&") {
		t.Errorf("location = %q contains leading-`&` fragment bug", loc)
	}
}