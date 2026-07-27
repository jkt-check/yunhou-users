package e2e

import (
	"net/http"
	"net/url"
	"os"
	"testing"
)

// TestEnvOr_CoversAllBranches exercises envOr's set / unset / empty paths.
func TestEnvOr_CoversAllBranches(t *testing.T) {
	// Set
	os.Setenv("TEST_E2E_ENVOR_SET", "value")
	defer os.Unsetenv("TEST_E2E_ENVOR_SET")
	if got := envOr("TEST_E2E_ENVOR_SET", "fallback"); got != "value" {
		t.Errorf("set: got %q, want value", got)
	}

	// Empty → fallback
	os.Setenv("TEST_E2E_ENVOR_EMPTY", "")
	defer os.Unsetenv("TEST_E2E_ENVOR_EMPTY")
	if got := envOr("TEST_E2E_ENVOR_EMPTY", "fb"); got != "fb" {
		t.Errorf("empty: got %q, want fb", got)
	}

	// Unset → fallback
	os.Unsetenv("TEST_E2E_ENVOR_UNSET")
	if got := envOr("TEST_E2E_ENVOR_UNSET", "fb2"); got != "fb2" {
		t.Errorf("unset: got %q, want fb2", got)
	}
}

// TestExtractQuery_Coverage exercises extractQuery against a URL.
func TestExtractQuery_Coverage(t *testing.T) {
	u, _ := url.Parse("https://example.com/path?foo=bar&baz=qux")
	if got := extractQuery(t, u.String(), "foo"); got != "bar" {
		t.Errorf("foo: got %q, want bar", got)
	}
	if got := extractQuery(t, u.String(), "baz"); got != "qux" {
		t.Errorf("baz: got %q, want qux", got)
	}
	if got := extractQuery(t, u.String(), "missing"); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}
}

// TestNewMockGitHubServer_Coverage starts the mock GitHub server and
// confirms its three endpoints serve the expected fixtures.
func TestNewMockGitHubServer_Coverage(t *testing.T) {
	srv := newMockGitHubServer(t)
	defer srv.Close()

	// /user
	resp, err := http.Get(srv.URL + "/user")
	if err != nil {
		t.Fatalf("/user: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("/user: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /user/emails
	resp, err = http.Get(srv.URL + "/user/emails")
	if err != nil {
		t.Fatalf("/user/emails: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("/user/emails: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /login/oauth/access_token — bad code path
	form := url.Values{}
	form.Set("code", "wrong-code")
	resp, err = http.PostForm(srv.URL+"/login/oauth/access_token", form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("token wrong-code: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// /login/oauth/access_token — good code path
	form.Set("code", mockGitHubCode)
	resp, err = http.PostForm(srv.URL+"/login/oauth/access_token", form)
	if err != nil {
		t.Fatalf("token ok: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("token ok: %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSetupE2EServer_BareSetup exercises setupE2EServer (no verifier).
func TestSetupE2EServer_BareSetup(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	if engine == nil {
		t.Fatal("nil engine")
	}
	// JWKS endpoint should still work.
	resp := doRequest(t, engine, http.MethodGet, "/.well-known/jwks.json", "", nil)
	if resp.StatusCode != 200 {
		t.Errorf("jwks: %d", resp.StatusCode)
	}
}
