package service

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// providerHTTPClient is the HTTP client used to call provider userinfo APIs.
// It's a package var so tests can swap it for a stub. The default 10s timeout
// caps how long a /auth/github/callback can block on a slow OAuth provider.
var providerHTTPClient = &http.Client{Timeout: 10 * time.Second}

// githubUserURL and githubEmailsURL are package vars so tests can point the
// fetcher at an httptest server. Production leaves them at the canonical
// endpoints.
var (
	githubUserURL   = "https://api.github.com/user"
	githubEmailsURL = "https://api.github.com/user/emails"
)

// fetchGitHubUser verifies a GitHub OAuth access token by calling api.github.com
// /user and /user/emails. It returns the canonical user info or an error wrapping
// the cause. Returns ErrInvalidProviderToken (via "invalid provider token" prefix)
// for 401/403 from GitHub so the caller can map it to a 401.
//
// Thin wrapper over fetchGitHubUserWithURLs (github_oauth.go), which is the
// URL-injectable version used by the GitHubOAuthService. Both share the same
// /user + /user/emails + email-merge-safety logic; the split exists because
// the package-global URL/Client vars here are how tests stub GitHub for the
// legacy fetchGitHubUser callers, while the GitHubOAuthService injects URLs
// per-instance via SetUserURL/SetEmailsURL.
func fetchGitHubUser(ctx context.Context, token string) (*ProviderUserInfo, error) {
	return fetchGitHubUserWithURLs(ctx, providerHTTPClient, githubUserURL, githubEmailsURL, token)
}

// fetchGitHubPrimaryEmail is a backwards-compatible alias. It now refuses to
// return any unverified email so the caller can't be tricked into trusting a
// non-primary address for account-merge purposes.
func fetchGitHubPrimaryEmail(ctx context.Context, token string) string {
	return fetchGitHubVerifiedPrimaryEmail(ctx, token)
}

// isGitHubPrimaryEmailVerified is a package-var wrapper over
// fetchGitHubEmails (github_oauth.go). Kept for the existing tests that
// stub githubEmailsURL + providerHTTPClient directly; production callers
// use fetchGitHubUser / fetchGitHubUserWithURLs which fetch /user/emails
// once and branch on both return values.
func isGitHubPrimaryEmailVerified(ctx context.Context, token string) bool {
	primary, _ := fetchGitHubEmails(ctx, providerHTTPClient, githubEmailsURL, token)
	return primary
}

// fetchGitHubVerifiedPrimaryEmail is a package-var wrapper over
// fetchGitHubEmails (github_oauth.go). Kept for the existing tests; see
// isGitHubPrimaryEmailVerified for the production-code path.
func fetchGitHubVerifiedPrimaryEmail(ctx context.Context, token string) string {
	_, email := fetchGitHubEmails(ctx, providerHTTPClient, githubEmailsURL, token)
	return email
}

// normalizeEmail trims and lowercases an email so case differences across
// providers don't fragment a single user into multiple accounts.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
