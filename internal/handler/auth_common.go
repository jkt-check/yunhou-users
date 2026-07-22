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
	if resp == nil {
		// Empty response — still emit the # marker so the BFF's
		// client-side handler can route on "is there a token".
		s := u.String()
		return s + "#"
	}
	fragment := url.Values{}
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

// redirectWithFragment attaches a url.Values-encoded fragment to base.
// Lower-level primitive than buildCallbackRedirectURL — used by the
// OAuth error and denial paths (provider error_param, upstream
// failure, missing unionid, AuthService rejection) to surface a small
// set of error/reason params to the BFF without leaking anything into
// the query string. Goes through url.Parse so badly-formed bases are
// returned as-is instead of producing "<base>#key=value" by string
// concatenation (which would mis-separate an existing query string).
func redirectWithFragment(base string, fragment url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Fragment = fragment.Encode()
	return u.String()
}

// redirectWithQuery appends a url.Values-encoded query string to base,
// merging with any existing query. Used by mock-mode OAuth flows where
// the redirect must round-trip a `code` / `state` pair through the
// browser the same way a real WeChat authorization-endpoint 302 does
// (real WeChat 302s to redirect_uri with `?code&state` in the QUERY
// string, not the fragment — the Yunhou-orchestrated SPA
// AuthCallbackPage reads those exact two params from window.location
// before forwarding to /auth/wechat/callback, and a fragment-based
// mock would fall straight through to the un-auth branch).
func redirectWithQuery(base string, params url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// redirectWithErrorFragment is the convenience form used by the
// post-login error paths (exchange upstream failure, profile fetch
// failure, AuthService rejection): builds a fragment with
// `error=<errCode>&reason=<reason>` and returns the BFF redirect URL.
// Used by /auth/wechat/callback sites that surface a Yunhou-mapped
// reason to the BFF; the GitHub callback uses inline
// `target.Fragment = frag.Encode()` instead because its error vocabulary
// diverges.
func redirectWithErrorFragment(base, errCode, reason string) string {
	frag := url.Values{}
	frag.Set("error", errCode)
	frag.Set("reason", reason)
	return redirectWithFragment(base, frag)
}