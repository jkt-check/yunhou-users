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

// fetchGitHubUser verifies a GitHub OAuth access token by calling api.github.com
// /user and /user/emails. It returns the canonical user info or an error wrapping
// the cause. Returns ErrInvalidProviderToken (via "invalid provider token" prefix)
// for 401/403 from GitHub so the caller can map it to a 401.
func fetchGitHubUser(ctx context.Context, token string) (*ProviderUserInfo, error) {
	const userURL = "https://api.github.com/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userURL, nil)
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
	if email == "" {
		// User's primary email may be hidden — query /user/emails to find a
		// verified primary one. Failures here are non-fatal; we still return
		// the user without an email if necessary.
		email = fetchGitHubPrimaryEmail(ctx, token)
	}

	return &ProviderUserInfo{
		Provider:    "github",
		ProviderUID: "github_" + strconv.FormatInt(ghUser.ID, 10),
		Email:       normalizeEmail(email),
		Nickname:    firstNonEmpty(ghUser.Name, ghUser.Login),
		AvatarURL:   ghUser.AvatarURL,
	}, nil
}

func fetchGitHubPrimaryEmail(ctx context.Context, token string) string {
	const emailsURL = "https://api.github.com/user/emails"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emailsURL, nil)
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
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email
		}
	}
	// Fall back to any verified email, else the first one.
	for _, e := range emails {
		if e.Verified {
			return e.Email
		}
	}
	if len(emails) > 0 {
		return emails[0].Email
	}
	return ""
}

// fetchGoogleUser verifies a Google OAuth access token by calling the v3
// userinfo endpoint. Same error-classification contract as fetchGitHubUser.
func fetchGoogleUser(ctx context.Context, token string) (*ProviderUserInfo, error) {
	const userURL = "https://www.googleapis.com/oauth2/v3/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build google user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call google userinfo api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: google returned %d", ErrInvalidProviderToken, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google userinfo api: status %d", resp.StatusCode)
	}

	var gUser struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&gUser); err != nil {
		return nil, fmt.Errorf("decode google user: %w", err)
	}
	if gUser.Sub == "" {
		return nil, fmt.Errorf("%w: google user payload missing sub", ErrInvalidProviderToken)
	}

	// Only trust email if Google verified it; otherwise leave empty so the
	// email-merge path in getOrCreateUser doesn't link unverified emails.
	email := ""
	if gUser.EmailVerified {
		email = gUser.Email
	}

	return &ProviderUserInfo{
		Provider:    "google",
		ProviderUID: "google_" + gUser.Sub,
		Email:       normalizeEmail(email),
		Nickname:    firstNonEmpty(gUser.Name, gUser.Email),
		AvatarURL:   gUser.Picture,
	}, nil
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
