package handler

import (
	"strings"
	"testing"

	"github.com/yunhou/users/internal/service"
)

// buildCallbackRedirectURL tests — moved from auth_github_test.go when
// the function was lifted to auth_common.go so /auth/github/callback and
// /auth/wechat/callback share one canonical implementation.

func TestBuildCallbackRedirectURL_EmptyResponse(t *testing.T) {
	got := buildCallbackRedirectURL("https://yundian.com/cb", nil)
	if !strings.HasPrefix(got, "https://yundian.com/cb#") {
		t.Errorf("got = %q", got)
	}
}

func TestBuildCallbackRedirectURL_FullResponse(t *testing.T) {
	resp := &service.LoginResponse{
		AccessToken:  "a",
		RefreshToken: "r",
		User:         service.UserInfo{ID: "u-1"},
		Subscription: &service.SubscriptionInfo{HasAccess: true},
	}
	got := buildCallbackRedirectURL("https://yundian.com/cb", resp)
	if !strings.Contains(got, "token=a") || !strings.Contains(got, "refresh_token=r") ||
		!strings.Contains(got, "user_id=u-1") || !strings.Contains(got, "has_access=true") {
		t.Errorf("got = %q", got)
	}
}

func TestBuildCallbackRedirectURL_NoAccess(t *testing.T) {
	resp := &service.LoginResponse{
		Subscription: &service.SubscriptionInfo{HasAccess: false},
	}
	got := buildCallbackRedirectURL("https://yundian.com/cb", resp)
	if !strings.Contains(got, "has_access=false") {
		t.Errorf("got = %q", got)
	}
}

func TestBuildCallbackRedirectURL_PartialResponse(t *testing.T) {
	resp := &service.LoginResponse{AccessToken: "only-access"}
	got := buildCallbackRedirectURL("https://yundian.com/cb", resp)
	if !strings.Contains(got, "token=only-access") {
		t.Errorf("got = %q", got)
	}
	if strings.Contains(got, "refresh_token") {
		t.Errorf("got = %q, should not contain refresh_token", got)
	}
}

func TestBuildCallbackRedirectURL_BadURL(t *testing.T) {
	resp := &service.LoginResponse{AccessToken: "x"}
	got := buildCallbackRedirectURL("://bad-url", resp)
	if got != "://bad-url" {
		t.Errorf("bad URL should be returned as-is, got %q", got)
	}
}