package service

import (
	"errors"
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
	if q.Get("scope") != "snsapi_login,snsapi_userinfo" {
		t.Errorf("scope = %q, want snsapi_login,snsapi_userinfo", q.Get("scope"))
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