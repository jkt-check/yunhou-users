package handler

import (
	"net/url"
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

func TestRedirectWithFragment_EncodesValues(t *testing.T) {
	frag := url.Values{}
	frag.Set("error", "auth_failed")
	frag.Set("reason", "wechat_upstream")
	got := redirectWithFragment("https://bff.example.com/cb", frag)
	want := "https://bff.example.com/cb#error=auth_failed&reason=wechat_upstream"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}

func TestRedirectWithFragment_BadURL(t *testing.T) {
	frag := url.Values{}
	frag.Set("error", "x")
	got := redirectWithFragment("://bad-url", frag)
	if got != "://bad-url" {
		t.Errorf("bad URL should be returned as-is, got %q", got)
	}
}

// TestRedirectWithFragment_NoLeadingAmpersand pins the contract that
// the post-login error redirect produces the canonical fragment shape
// (`<base>#error=...&reason=...`). The pre-fix code assembled
// "buildCallbackRedirectURL(base, nil) + \"&\" + encoded", which
// produced `<base>#&error=...` — the leading `&` is a parser trap.
func TestRedirectWithFragment_NoLeadingAmpersand(t *testing.T) {
	frag := url.Values{}
	frag.Set("error", "auth_failed")
	frag.Set("reason", "wechat_no_unionid")
	got := redirectWithFragment("https://bff.example.com/cb", frag)
	if strings.Contains(got, "#&") {
		t.Errorf("got %q contains leading-`&` (the bug we're guarding against)", got)
	}
}

func TestRedirectWithErrorFragment_BuildsAuthFailedShape(t *testing.T) {
	got := redirectWithErrorFragment("https://bff.example.com/cb", "auth_failed", "user_suspended")
	want := "https://bff.example.com/cb#error=auth_failed&reason=user_suspended"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}