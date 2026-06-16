package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/config"
)

func TestBuildAuthorizeURL(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:    "test-github-client-id",
		GitHubClientSecret: "test-github-client-secret",
	}

	provider := NewOAuthProvider(cfg, nil)

	tests := []struct {
		name         string
		providerName string
		appID        string
		redirectURI  string
		state        string
		wantErr      bool
		errContains  string
		validate     func(t *testing.T, authURL string)
	}{
		{
			name:         "github provider",
			providerName: "github",
			appID:        "app-1",
			redirectURI:  "https://example.com/callback",
			state:        "random-state-123",
			wantErr:      false,
			validate: func(t *testing.T, authURL string) {
				u, err := url.Parse(authURL)
				if err != nil {
					t.Fatalf("failed to parse authorize URL: %v", err)
				}
				if u.Host != "github.com" {
					t.Errorf("expected host github.com, got %s", u.Host)
				}
				if u.Path != "/login/oauth/authorize" {
					t.Errorf("expected path /login/oauth/authorize, got %s", u.Path)
				}
				q := u.Query()
				if q.Get("client_id") != "test-github-client-id" {
					t.Errorf("expected client_id test-github-client-id, got %s", q.Get("client_id"))
				}
				if q.Get("state") != "random-state-123" {
					t.Errorf("expected state random-state-123, got %s", q.Get("state"))
				}
				if q.Get("scope") != "read:user,user:email" {
					t.Errorf("expected scope read:user,user:email, got %s", q.Get("scope"))
				}
			},
		},
		{
			name:         "github localhost redirect",
			providerName: "github",
			appID:        "app-1",
			redirectURI:  "http://localhost:3000/callback",
			state:        "state-local",
			wantErr:      false,
			validate: func(t *testing.T, authURL string) {
				u, err := url.Parse(authURL)
				if err != nil {
					t.Fatalf("failed to parse authorize URL: %v", err)
				}
				q := u.Query()
				redirectURI := q.Get("redirect_uri")
				if !strings.HasPrefix(redirectURI, "http://localhost:") {
					t.Errorf("expected localhost redirect_uri, got %s", redirectURI)
				}
			},
		},
		{
			name:         "unsupported provider",
			providerName: "twitter",
			appID:        "app-1",
			redirectURI:  "https://example.com/callback",
			state:        "state-123",
			wantErr:      true,
			errContains:  "unsupported provider: twitter",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authURL, err := provider.BuildAuthorizeURL(tc.providerName, tc.appID, tc.redirectURI, tc.state)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errContains)
				} else if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if authURL == "" {
				t.Error("expected non-empty authorize URL")
			}

			if tc.validate != nil {
				tc.validate(t, authURL)
			}
		})
	}
}

// Tests that modify http.DefaultTransport must NOT use t.Parallel() to avoid data races.
// They are serialized by running sequentially within this test function.

func TestFetchUser_GitHubWithProfileEmail(t *testing.T) {
	// Do NOT use t.Parallel() — this test modifies http.DefaultTransport

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/oauth/access_token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"access_token": "mock-github-token",
				"token_type":   "bearer",
			})
			return
		}
	}))
	defer tokenServer.Close()

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":         12345,
				"login":      "testuser",
				"avatar_url": "https://github.com/avatar.png",
				"email":      "testuser@example.com",
			})
			return
		}
	}))
	defer userServer.Close()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:    "test-client-id",
		GitHubClientSecret: "test-client-secret",
	}
	p := NewOAuthProvider(cfg, nil)

	transport := &multiTestTransport{
		routes: map[string]http.Handler{
			"https://github.com/login/oauth/access_token": tokenServer.Config.Handler,
			"https://api.github.com/user":                userServer.Config.Handler,
		},
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	info, err := p.FetchUser(context.Background(), "github", "test-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Provider != "github" {
		t.Errorf("expected Provider github, got %s", info.Provider)
	}
	if info.ProviderUID != "12345" {
		t.Errorf("expected ProviderUID 12345, got %s", info.ProviderUID)
	}
	if info.Email != "testuser@example.com" {
		t.Errorf("expected Email testuser@example.com, got %s", info.Email)
	}
	if info.Nickname != "testuser" {
		t.Errorf("expected Nickname testuser, got %s", info.Nickname)
	}
	if info.AvatarURL != "https://github.com/avatar.png" {
		t.Errorf("expected AvatarURL https://github.com/avatar.png, got %s", info.AvatarURL)
	}
}

func TestFetchUser_GitHubWithEmailEndpoint(t *testing.T) {
	// Do NOT use t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "mock-github-token-2",
			"token_type":   "bearer",
		})
	}))
	defer tokenServer.Close()

	emailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"email": "secondary@example.com", "primary": false},
			{"email": "primary@example.com", "primary": true},
		})
	}))
	defer emailServer.Close()

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         67890,
			"login":      "noemailuser",
			"avatar_url": "https://github.com/noemailavatar.png",
			"email":      "",
		})
	}))
	defer userServer.Close()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:    "test-client-id",
		GitHubClientSecret: "test-client-secret",
	}
	p := NewOAuthProvider(cfg, nil)

	transport := &multiTestTransport{
		routes: map[string]http.Handler{
			"https://github.com/login/oauth/access_token": tokenServer.Config.Handler,
			"https://api.github.com/user":                userServer.Config.Handler,
			"https://api.github.com/user/emails":         emailServer.Config.Handler,
		},
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	info, err := p.FetchUser(context.Background(), "github", "test-code-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Email != "primary@example.com" {
		t.Errorf("expected primary email primary@example.com, got %s", info.Email)
	}
}

func TestFetchUser_UnsupportedProvider(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
	}
	p := NewOAuthProvider(cfg, nil)

	_, err := p.FetchUser(context.Background(), "twitter", "some-code")
	if err == nil {
		t.Error("expected error for unsupported provider, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported provider") {
		t.Errorf("expected error about unsupported provider, got %q", err.Error())
	}
}

func TestFetchUser_TokenExchangeFailure(t *testing.T) {
	// Do NOT use t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			// no access_token — simulates bad code
		})
	}))
	defer tokenServer.Close()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:    "test-client-id",
		GitHubClientSecret: "test-client-secret",
	}
	p := NewOAuthProvider(cfg, nil)

	transport := &multiTestTransport{
		routes: map[string]http.Handler{
			"https://github.com/login/oauth/access_token": tokenServer.Config.Handler,
		},
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.FetchUser(context.Background(), "github", "bad-code")
	if err == nil {
		t.Error("expected error for bad code, got nil")
	}
	if !strings.Contains(err.Error(), "exchange github code") {
		t.Errorf("expected error about exchange, got %q", err.Error())
	}
}

func TestFetchUser_InvalidUserResponse(t *testing.T) {
	// Do NOT use t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "mock-token",
			"token_type":   "bearer",
		})
	}))
	defer tokenServer.Close()

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid json`))
	}))
	defer userServer.Close()

	cfg := &config.Config{
		Port:               "8080",
		GitHubClientID:    "test-client-id",
		GitHubClientSecret: "test-client-secret",
	}
	p := NewOAuthProvider(cfg, nil)

	transport := &multiTestTransport{
		routes: map[string]http.Handler{
			"https://github.com/login/oauth/access_token": tokenServer.Config.Handler,
			"https://api.github.com/user":                userServer.Config.Handler,
		},
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = origTransport }()

	_, err := p.FetchUser(context.Background(), "github", "test-code")
	if err == nil {
		t.Error("expected error for invalid user response, got nil")
	}
	if !strings.Contains(err.Error(), "get github user") {
		t.Errorf("expected error about get github user, got %q", err.Error())
	}
}

// multiTestTransport routes HTTP requests to different test servers based on URL
type multiTestTransport struct {
	routes map[string]http.Handler
}

func (m *multiTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	handler, ok := m.routes[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("no test route for %s", req.URL.String())
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}
