package handler

import (
	"net/url"

	"github.com/yunhou/users/internal/service"
)

// buildCallbackRedirectURL adds the standard post-login params to the
// BFF's callback URL via URL fragment (so the access_token doesn't end
// up in browser history, server logs, or referer headers).
//
//	https://bff.example.com/auth/callback#token=<access>&refresh_token=<refresh>&user_id=<uuid>&has_access=<bool>
//
// Shared between /auth/github/callback and /auth/wechat/callback (and any
// future OAuth callback) so the BFF's fragment contract has one
// canonical implementation.
func buildCallbackRedirectURL(base string, resp *service.LoginResponse) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	fragment := url.Values{}
	if resp == nil {
		// Empty response — still emit the # marker so the BFF's
		// client-side handler can route on "is there a token".
		s := u.String()
		return s + "#"
	}
	if resp.AccessToken != "" {
		fragment.Set("token", resp.AccessToken)
	}
	if resp.RefreshToken != "" {
		fragment.Set("refresh_token", resp.RefreshToken)
	}
	if resp.User.ID != "" {
		fragment.Set("user_id", resp.User.ID)
	}
	if resp.Subscription != nil {
		if resp.Subscription.HasAccess {
			fragment.Set("has_access", "true")
		} else {
			fragment.Set("has_access", "false")
		}
	}
	u.Fragment = fragment.Encode()
	return u.String()
}