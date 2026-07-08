package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// withProviderHTTPClient swaps providerHTTPClient with a function returning
// the supplied *http.Client for the duration of the test, restoring on
// cleanup. This lets tests stub GitHub's userinfo + emails API in one
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
	prevUser, prevEmails := githubUserURL, githubEmailsURL
	githubUserURL = base + "/user"
	githubEmailsURL = base + "/user/emails"
	t.Cleanup(func() {
		githubUserURL = prevUser
		githubEmailsURL = prevEmails
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

// errorIs is a thin alias kept for the test-table call sites below.
// Implementation walks the wrap chain via errors.Is — do NOT regress to a
// substring match on Error() strings (would falsely match unrelated errors
// that happen to contain the sentinel's text).
func errorIs(err, target error) bool {
	return errors.Is(err, target)
}

// ============================================================================
// Wrapper coverage — fetchGitHubPrimaryEmail / isGitHubPrimaryEmailVerified
// / fetchGitHubVerifiedPrimaryEmail each call fetchGitHubEmails under the
// hood; ensure every wrapper path is exercised.
// ============================================================================

func TestFetchGitHubPrimaryEmail(t *testing.T) {
	t.Run("returns verified primary email", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user/emails" {
				fmt.Fprintf(w, `[{"email": "primary@example.com", "primary": true, "verified": true}]`)
			}
		}))
		t.Cleanup(server.Close)
		withProviderURLs(t, server.URL)
		withProviderHTTPClient(t, server.Client())

		got := fetchGitHubPrimaryEmail(context.Background(), "tok")
		if got != "primary@example.com" {
			t.Errorf("got %q, want primary@example.com", got)
		}
	})

	t.Run("returns empty for unverified primary", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user/emails" {
				fmt.Fprintf(w, `[{"email": "unverified@example.com", "primary": true, "verified": false}]`)
			}
		}))
		t.Cleanup(server.Close)
		withProviderURLs(t, server.URL)
		withProviderHTTPClient(t, server.Client())

		got := fetchGitHubPrimaryEmail(context.Background(), "tok")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestIsGitHubPrimaryEmailVerified(t *testing.T) {
	t.Run("verified primary → true", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user/emails" {
				fmt.Fprintf(w, `[{"email": "p@x.com", "primary": true, "verified": true}]`)
			}
		}))
		t.Cleanup(server.Close)
		withProviderURLs(t, server.URL)
		withProviderHTTPClient(t, server.Client())

		if !isGitHubPrimaryEmailVerified(context.Background(), "tok") {
			t.Error("expected true for verified primary")
		}
	})

	t.Run("unverified primary → false", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user/emails" {
				fmt.Fprintf(w, `[{"email": "p@x.com", "primary": true, "verified": false}]`)
			}
		}))
		t.Cleanup(server.Close)
		withProviderURLs(t, server.URL)
		withProviderHTTPClient(t, server.Client())

		if isGitHubPrimaryEmailVerified(context.Background(), "tok") {
			t.Error("expected false for unverified primary")
		}
	})
}

func TestFetchGitHubVerifiedPrimaryEmail(t *testing.T) {
	t.Run("returns email when verified", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user/emails" {
				fmt.Fprintf(w, `[{"email": "p@x.com", "primary": true, "verified": true}]`)
			}
		}))
		t.Cleanup(server.Close)
		withProviderURLs(t, server.URL)
		withProviderHTTPClient(t, server.Client())

		got := fetchGitHubVerifiedPrimaryEmail(context.Background(), "tok")
		if got != "p@x.com" {
			t.Errorf("got %q, want p@x.com", got)
		}
	})
}
