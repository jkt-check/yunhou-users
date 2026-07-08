package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

func newGitHubTestSecret() []byte { return []byte("test-oauth-state-secret-padding-bytes") }

func TestGitHubOAuthService_BuildAuthorizeURL_HappyPath(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.test",
		ClientSecret: "secret",
		CallbackURLs: []string{"https://yundian.com/auth/callback"},
	}
	now := time.Unix(1_700_000_000, 0)
	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(u, "https://github.com/login/oauth/authorize?") {
		t.Errorf("URL prefix wrong: %s", u)
	}
	for _, want := range []string{
		"client_id=Iv1.test",
		"redirect_uri=https%3A%2F%2Fyundian.com%2Fauth%2Fcallback",
		"scope=read%3Auser+user%3Aemail",
		"state=",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("URL missing %q in %s", want, u)
		}
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_NilConfig(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	if _, err := svc.BuildAuthorizeURL("yundian", nil, 0, time.Unix(1_700_000_000, 0)); err != ErrGitHubNotConfigured {
		t.Errorf("err = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_EmptyClientID(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	if _, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Unix(1_700_000_000, 0)); err != ErrGitHubNotConfigured {
		t.Errorf("err = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_EmptyClientSecret(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", CallbackURLs: []string{"https://x"}}
	if _, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Unix(1_700_000_000, 0)); err != ErrGitHubNotConfigured {
		t.Errorf("err = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_NoCallbackURLs(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret"}
	if _, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for missing callback_urls")
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_CallbackIndexOutOfRange(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.x",
		ClientSecret: "secret",
		CallbackURLs: []string{"https://a", "https://b"},
	}
	if _, err := svc.BuildAuthorizeURL("yundian", cfg, 5, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for out-of-range index")
	}
	if _, err := svc.BuildAuthorizeURL("yundian", cfg, -1, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for negative index")
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_DistinctStatesPerCall(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.x",
		ClientSecret: "secret",
		CallbackURLs: []string{"https://x"},
	}
	now := time.Unix(1_700_000_000, 0)
	seen := make(map[string]struct{}, 8)
	for i := 0; i < 8; i++ {
		u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, now)
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		state := extractQueryValue(t, u, "state")
		if _, dup := seen[state]; dup {
			t.Fatalf("state collision at i=%d: %s", i, state)
		}
		seen[state] = struct{}{}
	}
}

func TestGitHubOAuthService_BuildAuthorizeURL_UsesOverrideAuthorizeURL(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAuthorizeURL("https://stub.test/authorize")
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(u, "https://stub.test/authorize?") {
		t.Errorf("override URL not used: %s", u)
	}
}

func TestGitHubOAuthService_VerifyCallbackState_HappyPath(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.x",
		ClientSecret: "secret",
		CallbackURLs: []string{"https://a", "https://b", "https://c"},
	}
	now := time.Unix(1_700_000_000, 0)
	u, err := svc.BuildAuthorizeURL("yundian", cfg, 2, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	state := extractQueryValue(t, u, "state")
	idx, err := svc.VerifyCallbackState(state, "yundian", now.Add(time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if idx != 2 {
		t.Errorf("idx = %d, want 2", idx)
	}
}

func TestGitHubOAuthService_VerifyCallbackState_WrongAppID(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	now := time.Unix(1_700_000_000, 0)
	u, _ := svc.BuildAuthorizeURL("yundian", cfg, 0, now)
	state := extractQueryValue(t, u, "state")
	if _, err := svc.VerifyCallbackState(state, "yundash", now.Add(time.Second)); err == nil {
		t.Error("expected error for wrong appID")
	}
}

func TestGitHubOAuthService_VerifyCallbackState_Expired(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	now := time.Unix(1_700_000_000, 0)
	u, _ := svc.BuildAuthorizeURL("yundian", cfg, 0, now)
	state := extractQueryValue(t, u, "state")
	if _, err := svc.VerifyCallbackState(state, "yundian", now.Add(10*time.Minute)); err == nil {
		t.Error("expected error for expired state")
	}
}

func TestGitHubOAuthService_VerifyCallbackState_Tampered(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	now := time.Unix(1_700_000_000, 0)
	u, _ := svc.BuildAuthorizeURL("yundian", cfg, 0, now)
	state := extractQueryValue(t, u, "state")
	tampered := flipChar(state)
	if _, err := svc.VerifyCallbackState(tampered, "yundian", now.Add(time.Second)); err == nil {
		t.Error("expected error for tampered state")
	}
}

func TestGitHubOAuthService_VerifyCallbackState_WrongSecret(t *testing.T) {
	svcA := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svcB := NewGitHubOAuthService("a-different-secret-padding-bytes-here")
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	now := time.Unix(1_700_000_000, 0)
	u, _ := svcA.BuildAuthorizeURL("yundian", cfg, 0, now)
	state := extractQueryValue(t, u, "state")
	if _, err := svcB.VerifyCallbackState(state, "yundian", now.Add(time.Second)); err == nil {
		t.Error("expected error for wrong-secret verify")
	}
}

func TestGitHubOAuthService_VerifyCallbackState_EmptyToken(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	if _, err := svc.VerifyCallbackState("", "yundian", time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("expected error for empty token")
	}
}

// --- ExchangeCode ---

func TestGitHubOAuthService_ExchangeCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("client_id") == "" || r.PostForm.Get("client_secret") == "" || r.PostForm.Get("code") == "" {
			http.Error(w, "missing field", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_at_test",
			"token_type":   "bearer",
			"scope":        "read:user,user:email",
		})
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAccessTokenURL(srv.URL + "/login/oauth/access_token")
	cfg := &model.GitHubOAuthConfig{
		ClientID:     "Iv1.test",
		ClientSecret: "secret",
		CallbackURLs: []string{"https://yundian.com/cb"},
	}
	tok, err := svc.ExchangeCode(context.Background(), cfg, "the-code", "https://yundian.com/cb")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "gho_at_test" {
		t.Errorf("token = %q, want gho_at_test", tok)
	}
}

func TestGitHubOAuthService_ExchangeCode_GitHubError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "bad_verification_code",
			"error_description": "The code passed is incorrect or expired.",
		})
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAccessTokenURL(srv.URL + "/login/oauth/access_token")
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	if _, err := svc.ExchangeCode(context.Background(), cfg, "bad", "https://x"); err == nil {
		t.Error("expected error for bad_verification_code")
	}
}

func TestGitHubOAuthService_ExchangeCode_EmptyCode(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	if _, err := svc.ExchangeCode(context.Background(), cfg, "", "https://x"); err == nil {
		t.Error("expected error for empty code")
	}
}

func TestGitHubOAuthService_ExchangeCode_NilConfig(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	if _, err := svc.ExchangeCode(context.Background(), nil, "code", "https://x"); err != ErrGitHubNotConfigured {
		t.Errorf("err = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestGitHubOAuthService_ExchangeCode_NetworkError(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAccessTokenURL("http://127.0.0.1:1/login/oauth/access_token")
	svc.SetHTTPClient(&http.Client{Timeout: 100 * time.Millisecond})
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	_, err := svc.ExchangeCode(context.Background(), cfg, "code", "https://x")
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "upstream") {
		t.Errorf("err = %v, want ErrGitHubUpstream wrap", err)
	}
}

func TestGitHubOAuthService_ExchangeCode_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAccessTokenURL(srv.URL + "/login/oauth/access_token")
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	_, err := svc.ExchangeCode(context.Background(), cfg, "code", "https://x")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want decode error", err)
	}
}

func TestGitHubOAuthService_ExchangeCode_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"token_type": "bearer"})
	}))
	defer srv.Close()
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetAccessTokenURL(srv.URL + "/login/oauth/access_token")
	cfg := &model.GitHubOAuthConfig{ClientID: "Iv1.x", ClientSecret: "secret", CallbackURLs: []string{"https://x"}}
	if _, err := svc.ExchangeCode(context.Background(), cfg, "code", "https://x"); err == nil {
		t.Error("expected error for empty access_token in response")
	}
}

// --- FetchGitHubProfile ---

func TestGitHubOAuthService_FetchGitHubProfile_EmptyToken(t *testing.T) {
	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	if _, err := svc.FetchGitHubProfile(context.Background(), ""); err == nil {
		t.Error("expected error for empty token")
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 42, "login": "octocat", "name": "Octo Cat", "avatar_url": "https://avatars/x"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "octo@example.com", "primary": true, "verified": true}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	info, err := svc.FetchGitHubProfile(context.Background(), "any-token")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.Provider != "github" {
		t.Errorf("Provider = %q", info.Provider)
	}
	if info.ProviderUID != "github_42" {
		t.Errorf("ProviderUID = %q", info.ProviderUID)
	}
	if info.Nickname != "Octo Cat" {
		t.Errorf("Nickname = %q", info.Nickname)
	}
	if info.Email != "octo@example.com" {
		t.Errorf("Email = %q, want octo@example.com", info.Email)
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_NoEmailInUser(t *testing.T) {
	// /user returns no email — we should fall back to /user/emails and
	// pick the verified primary. This exercises the verifiedPrimaryEmail
	// branch (which has its own test below for the negative case).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 50, "login": "noemail"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "fallback@example.com", "primary": true, "verified": true}]`)
		}
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	info, err := svc.FetchGitHubProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.Email != "fallback@example.com" {
		t.Errorf("Email = %q, want fallback@example.com", info.Email)
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_UnverifiedEmailDropped(t *testing.T) {
	// /user returns an email that is NOT marked as verified primary
	// in /user/emails — for safety we drop it (otherwise an attacker
	// could register a GitHub account with the victim's email,
	// unverified, and merge into the victim via email-merge).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 60, "login": "attacker", "email": "victim@example.com"}`)
		case "/user/emails":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"email": "victim@example.com", "primary": false, "verified": false}]`)
		}
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	info, err := svc.FetchGitHubProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.Email != "" {
		t.Errorf("Email = %q, want empty (unverified email should be dropped)", info.Email)
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_EmailsEndpointErrors(t *testing.T) {
	// /user/emails returns 500 — verifiedPrimaryEmail should return
	// empty rather than crashing, leaving Email empty.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id": 70, "login": "lone", "name": "Lone"}`)
		case "/user/emails":
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	info, err := svc.FetchGitHubProfile(context.Background(), "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.Email != "" {
		t.Errorf("Email = %q, want empty (emails endpoint failed)", info.Email)
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_UserEndpoint401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	if _, err := svc.FetchGitHubProfile(context.Background(), "tok"); err == nil {
		t.Error("expected error for 401")
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_UserEndpoint500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	if _, err := svc.FetchGitHubProfile(context.Background(), "tok"); err == nil {
		t.Error("expected error for 500")
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_UserMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	if _, err := svc.FetchGitHubProfile(context.Background(), "tok"); err == nil {
		t.Error("expected decode error")
	}
}

func TestGitHubOAuthService_FetchGitHubProfile_UserMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login": "no-id"}`))
	}))
	defer srv.Close()

	svc := NewGitHubOAuthService(string(newGitHubTestSecret()))
	svc.SetUserURL(srv.URL + "/user")
	svc.SetEmailsURL(srv.URL + "/user/emails")
	_, err := svc.FetchGitHubProfile(context.Background(), "tok")
	if err == nil {
		t.Error("expected error for missing id")
	}
}

func TestIsPrimaryEmailVerified_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"email":"a@x","primary":true,"verified":true}]`))
	}))
	defer srv.Close()
	if !isPrimaryEmailVerified(context.Background(), &http.Client{}, srv.URL, "tok") {
		t.Error("expected true")
	}
}

func TestIsPrimaryEmailVerified_NoPrimaryVerified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"email":"a@x","primary":true,"verified":false}]`))
	}))
	defer srv.Close()
	if isPrimaryEmailVerified(context.Background(), &http.Client{}, srv.URL, "tok") {
		t.Error("expected false (not verified)")
	}
}

func TestIsPrimaryEmailVerified_EmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if isPrimaryEmailVerified(context.Background(), &http.Client{}, srv.URL, "tok") {
		t.Error("expected false for empty list")
	}
}

func TestIsPrimaryEmailVerified_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if isPrimaryEmailVerified(context.Background(), &http.Client{}, srv.URL, "tok") {
		t.Error("expected false for 500")
	}
}

func TestIsPrimaryEmailVerified_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if isPrimaryEmailVerified(context.Background(), &http.Client{}, srv.URL, "tok") {
		t.Error("expected false for malformed JSON")
	}
}

func TestIsPrimaryEmailVerified_NetworkError(t *testing.T) {
	if isPrimaryEmailVerified(context.Background(), &http.Client{Timeout: 50 * time.Millisecond}, "http://127.0.0.1:1/nope", "tok") {
		t.Error("expected false for connection refused")
	}
}

func TestVerifiedPrimaryEmail_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if got := verifiedPrimaryEmail(context.Background(), &http.Client{}, srv.URL, "tok"); got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}

func TestVerifiedPrimaryEmail_NetworkError(t *testing.T) {
	if got := verifiedPrimaryEmail(context.Background(), &http.Client{Timeout: 50 * time.Millisecond}, "http://127.0.0.1:1/nope", "tok"); got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}

func TestVerifiedPrimaryEmail_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if got := verifiedPrimaryEmail(context.Background(), &http.Client{}, srv.URL, "tok"); got != "" {
		t.Errorf("got = %q, want empty", got)
	}
}

// extractQueryValue pulls a single query parameter out of a URL string for
// tests. Returns the empty string + t.Fatal on parse errors.
func extractQueryValue(t *testing.T, raw, key string) string {
	t.Helper()
	for _, part := range strings.Split(splitOffQuery(raw), "&") {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if part[:eq] == key {
			return part[eq+1:]
		}
	}
	t.Fatalf("query param %q not found in %q", key, raw)
	return ""
}

func splitOffQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

func flipChar(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}