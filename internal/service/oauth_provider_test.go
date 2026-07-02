package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// withProviderHTTPClient swaps providerHTTPClient with a function returning
// the supplied *http.Client for the duration of the test, restoring on
// cleanup. This lets tests stub both GitHub and Google's userinfo API in one
// process without spinning up an internal registry.
func withProviderHTTPClient(t *testing.T, c *http.Client) {
	t.Helper()
	prev := providerHTTPClient
	providerHTTPClient = c
	t.Cleanup(func() { providerHTTPClient = prev })
}

// withProviderURLs swaps the package-level URL vars so fetches hit the
// supplied httptest server. Restored on cleanup.
func withProviderURLs(t *testing.T, base string) {
	t.Helper()
	prevUser, prevEmails, prevGoogle := githubUserURL, githubEmailsURL, googleUserURL
	githubUserURL = base + "/user"
	githubEmailsURL = base + "/user/emails"
	googleUserURL = base + "/userinfo"
	t.Cleanup(func() {
		githubUserURL = prevUser
		githubEmailsURL = prevEmails
		googleUserURL = prevGoogle
	})
}

// ============================================================================
// normalizeEmail
// ============================================================================

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"Foo@Bar.COM", "foo@bar.com"},
		{"  spaces@x.com  ", "spaces@x.com"},
		{"already@lower.com", "already@lower.com"},
		{"", ""},
		{"MiXeD@ExAmPlE.oRg", "mixed@example.org"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := normalizeEmail(c.in); got != c.want {
				t.Errorf("normalizeEmail(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ============================================================================
// firstNonEmpty
// ============================================================================

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()

	t.Run("returns first non-empty", func(t *testing.T) {
		got := firstNonEmpty("", "", "winner", "later")
		if got != "winner" {
			t.Errorf("got %q, want %q", got, "winner")
		}
	})
	t.Run("whitespace counts as non-empty (function does NOT trim)", func(t *testing.T) {
		got := firstNonEmpty("", "   ", "winner")
		if got != "   " {
			t.Errorf("got %q, want %q (whitespace passes through)", got, "   ")
		}
	})
	t.Run("all empty returns empty", func(t *testing.T) {
		got := firstNonEmpty("", "", "")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("single value", func(t *testing.T) {
		got := firstNonEmpty("only")
		if got != "only" {
			t.Errorf("got %q, want %q", got, "only")
		}
	})
	t.Run("no args returns empty", func(t *testing.T) {
		got := firstNonEmpty()
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// ============================================================================
// fetchGitHubUser — HTTP-level coverage
// ============================================================================

func TestFetchGitHubUser_Success(t *testing.T) {

	// Mock GitHub /user and /user/emails endpoints.
	userHits := int32(0)
	emailHits := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			atomic.AddInt32(&userHits, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 12345, "login": "octocat", "name": "Octo Cat", "email": "octo@example.com", "avatar_url": "https://avatars/octo.png"}`)
		case "/user/emails":
			atomic.AddInt32(&emailHits, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)

	withProviderHTTPClient(t, server.Client())

	got, err := fetchGitHubUser(context.Background(), "fake-token")
	if err != nil {
		t.Fatalf("fetchGitHubUser: %v", err)
	}
	if got.Provider != "github" {
		t.Errorf("Provider = %q, want github", got.Provider)
	}
	if got.ProviderUID != "github_12345" {
		t.Errorf("ProviderUID = %q, want github_12345", got.ProviderUID)
	}
	if got.Email != "octo@example.com" {
		t.Errorf("Email = %q, want octo@example.com", got.Email)
	}
	if got.Nickname != "Octo Cat" {
		t.Errorf("Nickname = %q, want Octo Cat", got.Nickname)
	}
	if got.AvatarURL != "https://avatars/octo.png" {
		t.Errorf("AvatarURL = %q", got.AvatarURL)
	}
	if atomic.LoadInt32(&userHits) != 1 || atomic.LoadInt32(&emailHits) != 1 {
		t.Errorf("expected one /user + one /user/emails hit, got user=%d emails=%d", userHits, emailHits)
	}
}

func TestFetchGitHubUser_FallbackNameToLogin(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			// No name; should fall back to login.
			fmt.Fprintf(w, `{"id": 99, "login": "octocat", "email": "octo@example.com"}`)
		case "/user/emails":
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		}
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	got, err := fetchGitHubUser(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Nickname != "octocat" {
		t.Errorf("Nickname = %q, want octocat (login fallback)", got.Nickname)
	}
}

func TestFetchGitHubUser_PicksFirstVerifiedEmail(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			// /user returns empty email — triggers fallback to /user/emails.
			fmt.Fprintf(w, `{"id": 99, "login": "octocat", "name": "Octo"}`)
		case "/user/emails":
			// Primary is unverified, second is verified. Expect second.
			fmt.Fprintf(w, `[{"email": "primary@example.com", "primary": true, "verified": false}, {"email": "fallback@example.com", "primary": false, "verified": true}]`)
		}
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	got, err := fetchGitHubUser(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Email != "fallback@example.com" {
		t.Errorf("Email = %q, want fallback@example.com", got.Email)
	}
}

func TestFetchGitHubUser_401(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGitHubUser(context.Background(), "bad")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("err = %v, want wraps ErrInvalidProviderToken", err)
	}
}

func TestFetchGitHubUser_403(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGitHubUser(context.Background(), "bad")
	if !errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("err = %v, want wraps ErrInvalidProviderToken (403 maps)", err)
	}
}

func TestFetchGitHubUser_500(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGitHubUser(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("500 should NOT map to ErrInvalidProviderToken")
	}
}

func TestFetchGitHubUser_MalformedJSON(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			fmt.Fprintf(w, "this is not JSON")
		}
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGitHubUser(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("decode error should NOT be ErrInvalidProviderToken")
	}
}

func TestFetchGitHubUser_MissingID(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			// id is 0 — the dedup would collapse all such users to one.
			fmt.Fprintf(w, `{"id": 0, "login": "no-id"}`)
		case "/user/emails":
			fmt.Fprintf(w, `[]`)
		}
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGitHubUser(context.Background(), "tok")
	if !errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("missing id should map to ErrInvalidProviderToken, got %v", err)
	}
}

// ============================================================================
// fetchGoogleUser
// ============================================================================

func TestFetchGoogleUser_Success(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing bearer")
		}
		fmt.Fprintf(w, `{"sub": "google-uid-1", "email": "alice@example.com", "email_verified": true, "name": "Alice", "picture": "https://example.com/a.png"}`)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	got, err := fetchGoogleUser(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Provider != "google" {
		t.Errorf("Provider = %q", got.Provider)
	}
	if got.ProviderUID != "google_google-uid-1" {
		t.Errorf("ProviderUID = %q", got.ProviderUID)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q", got.Email)
	}
	if got.Nickname != "Alice" {
		t.Errorf("Nickname = %q", got.Nickname)
	}
	if got.AvatarURL != "https://example.com/a.png" {
		t.Errorf("AvatarURL = %q", got.AvatarURL)
	}
}

func TestFetchGoogleUser_UnverifiedEmailDropped(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// email_verified=false — must be dropped to avoid email-merge takeover.
		fmt.Fprintf(w, `{"sub": "x", "email": "evil@example.com", "email_verified": false, "name": "Evil"}`)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	got, err := fetchGoogleUser(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Email != "" {
		t.Errorf("Email = %q, want empty (unverified dropped)", got.Email)
	}
}

func TestFetchGoogleUser_NicknameFallbackToEmail(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No name; should fall back to email.
		fmt.Fprintf(w, `{"sub": "x", "email": "x@example.com", "email_verified": true}`)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	got, err := fetchGoogleUser(context.Background(), "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Nickname != "x@example.com" {
		t.Errorf("Nickname = %q, want x@example.com (email fallback)", got.Nickname)
	}
}

func TestFetchGoogleUser_401(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGoogleUser(context.Background(), "tok")
	if !errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("err = %v, want ErrInvalidProviderToken", err)
	}
}

func TestFetchGoogleUser_500(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGoogleUser(context.Background(), "tok")
	if err == nil || errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("500 should not be ErrInvalidProviderToken, got %v", err)
	}
}

func TestFetchGoogleUser_MissingSub(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"email": "x@x.com", "email_verified": true}`)
	}))
	t.Cleanup(server.Close)
	withProviderURLs(t, server.URL)
	withProviderHTTPClient(t, server.Client())

	_, err := fetchGoogleUser(context.Background(), "tok")
	if !errorIs(err, ErrInvalidProviderToken) {
		t.Errorf("missing sub should be ErrInvalidProviderToken, got %v", err)
	}
}

// errorIs is a small alias for errors.Is to keep imports minimal in this file.
func errorIs(err, target error) bool {
	return err != nil && (err == target || (err != nil && strings.Contains(err.Error(), target.Error())))
}
