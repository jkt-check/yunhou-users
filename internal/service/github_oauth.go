package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

// GitHub OAuth redirect flow boundary:
//
//   - Yunhou is the only holder of the GitHub OAuth App's client_secret.
//     The BFF never sees it; Yunhou uses it only when exchanging an auth
//     code at GitHub's /login/oauth/access_token endpoint.
//   - The state parameter binding is enforced by util.IssueOAuthState /
//     util.VerifyOAuthState. Yunhou is the only party that knows the
//     signing secret.
//   - Callback URLs are stored per app (apps.config.oauth_providers.github
//     .callback_urls) and validated against the incoming redirect_uri on
//     every callback request — prevents open-redirect abuse even if state
//     somehow leaks.
//   - The GitHub access_token Yunhou receives during the code exchange is
//     used in-process only: one call to /user and one to /user/emails,
//     then dropped. It is never written to DB, never returned to the BFF.

// GitHubOAuthEndpoints is the set of URLs Yunhou talks to. They're package
// vars (instead of const) so tests can point them at an httptest server.
// Production callers should construct GitHubOAuthService via
// NewGitHubOAuthService (which already wires the production URLs) and
// override per-instance via SetAccessTokenURL / SetHTTPClient. These
// package vars are the legacy fallback used by the small handful of tests
// that exercise package-level helpers — production code does not read
// them.
var (
	githubAuthorizeURL   = "https://github.com/login/oauth/authorize"
	githubAccessTokenURL = "https://github.com/login/oauth/access_token"
)

// githubOAuthHTTPClient is the HTTP client used for GitHub OAuth calls.
// Same caveat as the URL vars — production code uses the per-instance
// client configured at construction.
var githubOAuthHTTPClient = &http.Client{Timeout: 10 * time.Second}

// GitHubOAuthService is the entry point Yunhou's redirect handler uses to:
//  1. Build the upstream authorize URL the BFF redirects the user to.
//  2. Exchange the auth code GitHub returns at the callback endpoint.
//  3. Fetch the user's GitHub profile + verified primary email.
//
// It depends on the OAuth config + state secret — both are injected at
// construction (not globals) so the service is testable.
type GitHubOAuthService struct {
	stateSecret    []byte
	authorizeURL   string // override for tests; defaults to github.com in prod
	accessTokenURL string // override for tests; defaults to github.com in prod
	userURL        string // override for tests; defaults to api.github.com/user in prod
	emailsURL      string // override for tests; defaults to api.github.com/user/emails in prod
	httpClient     *http.Client
}

// NewGitHubOAuthService builds a service. The state secret is required —
// pass an empty slice and you'll get an error from IssueRedirect later, but
// we don't error at construction so the call site can defer the failure
// to a per-request boundary.
func NewGitHubOAuthService(stateSecret string) *GitHubOAuthService {
	return &GitHubOAuthService{
		stateSecret:    []byte(stateSecret),
		authorizeURL:   "https://github.com/login/oauth/authorize",
		accessTokenURL: "https://github.com/login/oauth/access_token",
		userURL:        "https://api.github.com/user",
		emailsURL:      "https://api.github.com/user/emails",
		httpClient:     &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient overrides the HTTP client used for upstream calls. Tests
// use this to inject httptest servers (or to set a short timeout).
func (s *GitHubOAuthService) SetHTTPClient(c *http.Client) {
	s.httpClient = c
}

// SetAccessTokenURL overrides the upstream access-token endpoint. Tests
// use this to point at an httptest stub.
func (s *GitHubOAuthService) SetAccessTokenURL(u string) {
	s.accessTokenURL = u
}

// SetAuthorizeURL overrides the upstream authorize endpoint. Tests use
// this to point at an httptest stub.
func (s *GitHubOAuthService) SetAuthorizeURL(u string) {
	s.authorizeURL = u
}

// SetUserURL overrides the GitHub /user endpoint used by FetchGitHubProfile.
// Tests point this at an httptest stub.
func (s *GitHubOAuthService) SetUserURL(u string) {
	s.userURL = u
}

// SetEmailsURL overrides the GitHub /user/emails endpoint used by
// FetchGitHubProfile. Tests point this at an httptest stub.
func (s *GitHubOAuthService) SetEmailsURL(u string) {
	s.emailsURL = u
}

// ErrGitHubNotConfigured signals that apps.config.oauth_providers.github is
// absent or empty for the requested app. Mapped to 404 by the handler —
// "GitHub login is disabled for this app".
var ErrGitHubNotConfigured = errors.New("github oauth not configured for app")

// ErrCallbackURLMismatch signals the redirect_uri submitted at callback
// time is not in the app's callback_urls whitelist. Mapped to 400.
var ErrCallbackURLMismatch = errors.New("redirect_uri not in callback_urls whitelist")

// ErrGitHubUpstream signals a non-recoverable error from GitHub itself
// (network, 5xx, decode failure). Mapped to 502.
var ErrGitHubUpstream = errors.New("github oauth upstream error")

// BuildAuthorizeURL assembles the upstream GitHub authorize URL Yunhou
// redirects the user to. The state token binds (appID, callbackIndex) so
// the callback handler can verify the round-trip.
//
// appID is the Yunhou app identifier (e.g. "yundian").
// cfg is the GitHub OAuth block for the app.
// callbackIndex is the index of the chosen callback URL inside cfg.CallbackURLs.
// now is injectable for tests.
func (s *GitHubOAuthService) BuildAuthorizeURL(appID string, cfg *model.GitHubOAuthConfig, callbackIndex int, now time.Time) (string, error) {
	if cfg == nil {
		return "", ErrGitHubNotConfigured
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return "", ErrGitHubNotConfigured
	}
	if callbackIndex < 0 || callbackIndex >= len(cfg.CallbackURLs) {
		return "", fmt.Errorf("%w: callback_index %d out of range", ErrCallbackURLMismatch, callbackIndex)
	}
	redirectURI := cfg.CallbackURLs[callbackIndex]

	state, err := util.IssueOAuthState(s.stateSecret, appID, callbackIndex, now)
	if err != nil {
		return "", fmt.Errorf("issue state: %w", err)
	}

	q := url.Values{}
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	q.Set("allow_signup", "true")
	return s.authorizeURL + "?" + q.Encode(), nil
}

// VerifyCallbackState confirms the state token GitHub echoed back to us
// really came from our /auth/github/redirect handler. Returns the
// callbackIndex that was bound at issue time so the handler can fetch the
// matching URL from cfg.CallbackURLs and confirm it equals redirect_uri.
func (s *GitHubOAuthService) VerifyCallbackState(state, expectedAppID string, now time.Time) (callbackIndex int, err error) {
	return util.VerifyOAuthState(s.stateSecret, state, expectedAppID, now)
}

// ExchangeCode trades the auth code GitHub returned for an access token.
// Returns the raw access_token string — caller is responsible for using
// it immediately and not retaining it.
func (s *GitHubOAuthService) ExchangeCode(ctx context.Context, cfg *model.GitHubOAuthConfig, code, redirectURI string) (string, error) {
	if cfg == nil || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return "", ErrGitHubNotConfigured
	}
	if code == "" {
		return "", errors.New("empty code")
	}

	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.accessTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build access_token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGitHubUpstream, err)
	}
	defer resp.Body.Close()

	// Inspect status code BEFORE decoding — a 5xx with an HTML body
	// would otherwise fail at json.Unmarshal and report a misleading
	// "decode" error to operators triaging a GitHub incident.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: access_token endpoint returned %d", ErrGitHubUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("%w: read body: %v", ErrGitHubUpstream, err)
	}

	// GitHub returns 200 with {"error":"bad_verification_code", ...} on a
	// bad code (not a non-2xx status), so we always inspect the body.
	var parsed struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("%w: decode: %v", ErrGitHubUpstream, err)
	}
	if parsed.Error != "" {
		return "", fmt.Errorf("%w: %s: %s", ErrGitHubUpstream, parsed.Error, parsed.ErrorDesc)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("%w: empty access_token in response", ErrGitHubUpstream)
	}
	return parsed.AccessToken, nil
}

// FetchGitHubProfile calls /user and /user/emails using the access_token,
// returning a ProviderUserInfo. The token is used exactly twice; after this
// function returns, the caller MUST drop it.
//
// The /user and /user/emails URLs come from the per-service-instance
// fields (s.userURL / s.emailsURL) — production wiring uses GitHub's
// canonical endpoints; tests swap them via SetUserURL / SetEmailsURL.
func (s *GitHubOAuthService) FetchGitHubProfile(ctx context.Context, accessToken string) (*ProviderUserInfo, error) {
	if accessToken == "" {
		return nil, errors.New("empty access token")
	}
	user, err := fetchGitHubUserWithURLs(ctx, s.httpClient, s.userURL, s.emailsURL, accessToken)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// fetchGitHubUserWithURLs is the URL-injectable version of fetchGitHubUser.
// It exists so the GitHubOAuthService can swap upstream endpoints for
// tests; production callers go through FetchGitHubProfile (which uses the
// canonical GitHub URLs).
//
// Email-merge safety: GitHub's /user returns an email field that may not
// be verified. We always cross-check with /user/emails before trusting
// the email for identity binding. See oauth_provider.go fetchGitHubUser
// for the threat model (unverified-email takeover via email-merge).
func fetchGitHubUserWithURLs(ctx context.Context, httpClient *http.Client, userURL, emailsURL, token string) (*ProviderUserInfo, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	ghUser, err := fetchUserJSON(ctx, httpClient, userURL, token)
	if err != nil {
		return nil, err
	}
	email := ghUser.Email
	// Email-merge safety: /user's email is not necessarily verified.
	// Cross-check with /user/emails ONCE per login (was 2x in the prior
	// implementation). If /user returned no email, use the verified
	// primary. If /user/emails says /user's email isn't primary+verified,
	// drop it and use the verified primary.
	//
	// Without this guard, an attacker can register a GitHub account
	// with the victim's email (unverified) and merge into the victim
	// via the email-merge path in AuthService.
	primaryIsVerified, verifiedPrimary := fetchGitHubEmails(ctx, httpClient, emailsURL, token)
	if email == "" || !primaryIsVerified {
		email = verifiedPrimary
	}
	return &ProviderUserInfo{
		Provider:    "github",
		ProviderUID: "github_" + strconv.FormatInt(ghUser.ID, 10),
		Email:       normalizeEmail(email),
		Nickname:    firstNonEmpty(ghUser.Name, ghUser.Login),
		AvatarURL:   ghUser.AvatarURL,
	}, nil
}

// isPrimaryEmailVerified is a one-line wrapper over fetchGitHubEmails
// kept for the existing tests that target it directly. Production code
// path uses fetchGitHubEmails once and branches on both return values
// (see fetchGitHubUserWithURLs above).
func isPrimaryEmailVerified(ctx context.Context, httpClient *http.Client, emailsURL, token string) bool {
	primary, _ := fetchGitHubEmails(ctx, httpClient, emailsURL, token)
	return primary
}

// verifiedPrimaryEmail is a one-line wrapper over fetchGitHubEmails kept
// for the existing tests that target it directly. Production code path
// uses fetchGitHubEmails once and branches on both return values.
func verifiedPrimaryEmail(ctx context.Context, httpClient *http.Client, emailsURL, token string) string {
	_, email := fetchGitHubEmails(ctx, httpClient, emailsURL, token)
	return email
}

// fetchGitHubEmails decodes /user/emails once and returns both the
// (primaryIsVerified) flag and the (verifiedPrimary) email string.
// Single fetch — the previous implementation called /user/emails twice
// per login (once for the bool, once for the string), doubling GitHub's
// RTT cost on every /auth/github/callback. URL-injectable so tests
// stub it.
func fetchGitHubEmails(ctx context.Context, httpClient *http.Client, emailsURL, token string) (primaryIsVerified bool, verifiedPrimary string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emailsURL, nil)
	if err != nil {
		return false, ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, ""
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&emails); err != nil {
		return false, ""
	}
	// First pass: primary + verified (the strongest signal).
	for _, e := range emails {
		if e.Primary && e.Verified {
			return true, e.Email
		}
	}
	// Second pass: any verified (legitimate non-primary case).
	for _, e := range emails {
		if e.Verified {
			return false, e.Email
		}
	}
	// No verified emails — refuse to return anything. Falling back to
	// unverified addresses would re-introduce the email-merge takeover.
	return false, ""
}

// fetchUserJSON performs the /user call against an injectable URL and HTTP
// client. Mirrors the body of fetchGitHubUser so the URL is no longer
// package-global.
func fetchUserJSON(ctx context.Context, httpClient *http.Client, userURL, token string) (struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}, error) {
	var empty struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userURL, nil)
	if err != nil {
		return empty, fmt.Errorf("build github user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return empty, fmt.Errorf("call github user api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return empty, fmt.Errorf("%w: github returned %d", ErrInvalidProviderToken, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("github user api: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&empty); err != nil {
		return empty, fmt.Errorf("decode github user: %w", err)
	}
	if empty.ID == 0 {
		return empty, fmt.Errorf("%w: github user payload missing id", ErrInvalidProviderToken)
	}
	return empty, nil
}
