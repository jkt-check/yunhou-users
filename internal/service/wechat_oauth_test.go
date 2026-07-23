package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

// newWeChatTestSecret returns the GitHub test secret as a string. The
// production config layer enforces >=32 chars; reusing the same secret
// across both providers' tests keeps the shared util.IssueOAuthState
// behaviour identical in both flows.
func newWeChatTestSecret(t *testing.T) string {
	t.Helper()
	return string(newGitHubTestSecret())
}

func TestWeChatOAuthService_BuildAuthorizeURL_HappyPath(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	fixed := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, fixed)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}

	if !startsWith(u, "https://open.weixin.qq.com/connect/qrconnect?") {
		t.Fatalf("unexpected base URL: %s", u)
	}
	if !endsWith(u, "#wechat_redirect") {
		t.Fatalf("missing #wechat_redirect fragment: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()
	if q.Get("appid") != "wx0123456789abcdef" {
		t.Errorf("appid = %q, want wx0123456789abcdef", q.Get("appid"))
	}
	if q.Get("redirect_uri") != "https://bff.example.com/auth/wechat-callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	// 2026-07-23: 网站应用 qrconnect must request snsapi_login ALONE.
	// Adding snsapi_userinfo (a 公众号-platform scope) makes WeChat show
	// an extra 昵称/头像 consent dialog; users clicking 拒绝 broke login
	// on cn-staging. /sns/userinfo still returns unionid without it.
	if q.Get("scope") != "snsapi_login" {
		t.Errorf("scope = %q, want snsapi_login", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Errorf("state is empty")
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_NilConfig(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	_, err := svc.BuildAuthorizeURL("yundian", nil, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_MissingAppID(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_EmptyCallbackURLs(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:     "wx0123456789abcdef",
		AppSecret: "0123456789abcdef0123456789abcdef",
	}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_CallbackIndexOutOfRange(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 5, time.Now())
	if !isWeChatCallbackURLMismatch(err) {
		t.Fatalf("err = %v, want ErrWeChatCallbackURLMismatch", err)
	}
}

func TestWeChatOAuthService_VerifyCallbackState_HappyPath(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	fixed := time.Unix(1_700_000_000, 0)
	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, fixed)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	state := extractWeChatState(t, u)

	idx, err := svc.VerifyCallbackState(state, "yundian", fixed.Add(time.Second))
	if err != nil {
		t.Fatalf("VerifyCallbackState: %v", err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0", idx)
	}
}

// extractWeChatState parses the state query parameter from a WeChat
// authorize URL. The shared extractQueryValue helper in
// github_oauth_test.go uses string splitting and chokes on the
// "#wechat_redirect" fragment; WeChat URLs always carry that fragment.
func extractWeChatState(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed.Query().Get("state")
}

// --- ExchangeCode tests ------------------------------------------------

func TestWeChatOAuthService_ExchangeCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		if q.Get("appid") != "wx0123456789abcdef" {
			t.Errorf("appid = %q", q.Get("appid"))
		}
		if q.Get("secret") != "0123456789abcdef0123456789abcdef" {
			t.Errorf("secret mismatch")
		}
		if q.Get("code") != "auth-code" {
			t.Errorf("code = %q", q.Get("code"))
		}
		if q.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", q.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"AT","expires_in":7200,"refresh_token":"RT","openid":"oid","scope":"snsapi_login,snsapi_userinfo"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)

	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	tok, err := svc.ExchangeCode(context.Background(), cfg, "auth-code", "https://bff.example.com/auth/wechat-callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "AT" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if tok.OpenID != "oid" {
		t.Errorf("OpenID = %q", tok.OpenID)
	}
	if tok.RefreshToken != "RT" {
		t.Errorf("RefreshToken = %q", tok.RefreshToken)
	}
}

func TestWeChatOAuthService_ExchangeCode_UpstreamErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"errcode":40029,"errmsg":"invalid code"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	_, err := svc.ExchangeCode(context.Background(), cfg, "bad", "https://bff.example.com/auth/wechat-callback")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_ExchangeCode_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	_, err := svc.ExchangeCode(context.Background(), cfg, "x", "https://bff.example.com/auth/wechat-callback")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_ExchangeCode_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"expires_in":7200,"openid":"oid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	_, err := svc.ExchangeCode(context.Background(), cfg, "x", "https://bff.example.com/auth/wechat-callback")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

// --- FetchWeChatProfile tests ----------------------------------------

func TestWeChatOAuthService_FetchWeChatProfile_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("access_token") != "AT" {
			t.Errorf("access_token = %q", q.Get("access_token"))
		}
		if q.Get("openid") != "oid" {
			t.Errorf("openid = %q", q.Get("openid"))
		}
		if q.Get("lang") != "zh_CN" {
			t.Errorf("lang = %q", q.Get("lang"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"openid":"oid","nickname":"nick","sex":1,"headimgurl":"http://img","unionid":"uid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	got, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if err != nil {
		t.Fatalf("FetchWeChatProfile: %v", err)
	}
	if got.Provider != "wechat" {
		t.Errorf("Provider = %q, want wechat", got.Provider)
	}
	if got.ProviderUID != "wechat_uid" {
		t.Errorf("ProviderUID = %q, want wechat_uid", got.ProviderUID)
	}
	if got.Email != "" {
		t.Errorf("Email = %q, want empty", got.Email)
	}
	if got.Nickname != "nick" {
		t.Errorf("Nickname = %q", got.Nickname)
	}
	if got.AvatarURL != "http://img" {
		t.Errorf("AvatarURL = %q", got.AvatarURL)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_MissingUnionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"openid":"oid","nickname":"nick","headimgurl":"http://img"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	_, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if !errors.Is(err, ErrWeChatNoUnionID) {
		t.Fatalf("err = %v, want ErrWeChatNoUnionID", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_UpstreamErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"errcode":40001,"errmsg":"invalid credential"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	_, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_EmptyAccessToken(t *testing.T) {
	// An empty access_token at the API boundary is an upstream failure
	// (the BFF's redirect chain fed us nothing usable). The handler maps
	// ErrWeChatUpstream to a BFF fragment, so wrapping this sentinel
	// avoids stranding the user on a 500 JSON page.
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	_, err := svc.FetchWeChatProfile(context.Background(), "", "oid")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_EmptyOpenID(t *testing.T) {
	// If ExchangeCode somehow returned a 200 body without an openid
	// field, FetchWeChatProfile must surface that as ErrWeChatUpstream
	// (not a bare errors.New) so the handler routes to the BFF
	// fragment rather than a 500 JSON page.
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	_, err := svc.FetchWeChatProfile(context.Background(), "AT", "")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_MissingOptionalFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"openid":"oid","unionid":"uid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	got, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if err != nil {
		t.Fatalf("FetchWeChatProfile: %v", err)
	}
	if got.Nickname != "" || got.AvatarURL != "" {
		t.Errorf("expected empty nickname/avatar, got %+v", got)
	}
	if got.ProviderUID != "wechat_uid" {
		t.Errorf("ProviderUID = %q, want wechat_uid", got.ProviderUID)
	}
}

func TestWeChatOAuthService_VerifyCallbackState_Expired(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	issue := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	u, _ := svc.BuildAuthorizeURL("yundian", cfg, 0, issue)
	state := extractWeChatState(t, u)

	_, err := svc.VerifyCallbackState(state, "yundian", issue.Add(10*time.Minute))
	if err == nil {
		t.Fatalf("expected error for expired state")
	}
}

// extractQueryValue is defined in github_oauth_test.go (shared helper).

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func isWeChatCallbackURLMismatch(err error) bool {
	return errors.Is(err, ErrWeChatCallbackURLMismatch)
}