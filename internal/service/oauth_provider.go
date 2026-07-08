package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// providerHTTPClient is the HTTP client used to call provider userinfo APIs.
// It's a package var so tests can swap it for a stub. The default 10s timeout
// caps how long an /auth/login request can block on a slow OAuth provider.
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
func fetchGitHubUser(ctx context.Context, token string) (*ProviderUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubUserURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build github user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call github user api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: github returned %d", ErrInvalidProviderToken, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// Don't include raw response body — it may contain internal hints.
		return nil, fmt.Errorf("github user api: status %d", resp.StatusCode)
	}

	var ghUser struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&ghUser); err != nil {
		return nil, fmt.Errorf("decode github user: %w", err)
	}
	if ghUser.ID == 0 {
		return nil, fmt.Errorf("%w: github user payload missing id", ErrInvalidProviderToken)
	}

	email := ghUser.Email
	// GitHub's primary email returned by /user is *not* necessarily verified.
	// Only /user/emails tells us which addresses are confirmed, so always
	// fall back to that endpoint and reject any email that isn't verified.
	// Without this guard, an attacker can register a GitHub account with the
	// victim's email (unverified) and merge into the victim's user via the
	// email-merge path in AuthService.
	if email == "" || !isGitHubPrimaryEmailVerified(ctx, token) {
		verified := fetchGitHubVerifiedPrimaryEmail(ctx, token)
		if verified != "" {
			email = verified
		}
	}

	return &ProviderUserInfo{
		Provider:    "github",
		ProviderUID: "github_" + strconv.FormatInt(ghUser.ID, 10),
		Email:       normalizeEmail(email),
		Nickname:    firstNonEmpty(ghUser.Name, ghUser.Login),
		AvatarURL:   ghUser.AvatarURL,
	}, nil
}

// fetchGitHubPrimaryEmail is a backwards-compatible alias. It now refuses to
// return any unverified email so the caller can't be tricked into trusting a
// non-primary address for account-merge purposes.
func fetchGitHubPrimaryEmail(ctx context.Context, token string) string {
	return fetchGitHubVerifiedPrimaryEmail(ctx, token)
}

// isGitHubPrimaryEmailVerified reports whether the email returned by the
// /user endpoint is also the user's primary *and* verified email per
// /user/emails. Used to decide whether the inline email can be trusted for
// account-merge purposes.
func isGitHubPrimaryEmailVerified(ctx context.Context, token string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubEmailsURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&emails); err != nil {
		return false
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return true
		}
	}
	return false
}

// fetchGitHubVerifiedPrimaryEmail returns the user's primary verified email,
// falling back to any verified email, falling back to the first listed email.
// Returns "" if the endpoint fails or no email is available.
func fetchGitHubVerifiedPrimaryEmail(ctx context.Context, token string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubEmailsURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&emails); err != nil {
		return ""
	}
	// 1. Primary AND verified.
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email
		}
	}
	// 2. Any verified email.
	for _, e := range emails {
		if e.Verified {
			return e.Email
		}
	}
	// 3. No verified emails — refuse to return anything. Falling back to
	//    unverified addresses would re-introduce the email-merge takeover.
	return ""
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
