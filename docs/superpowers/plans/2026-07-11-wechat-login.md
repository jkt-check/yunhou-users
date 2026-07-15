# WeChat Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add WeChat Open Platform 网站应用 (QR-code) OAuth2.0 login as a second identity provider alongside GitHub, mirroring the existing GitHub flow verbatim — same JWT issuance, same BFF-facing fragment-based redirect, same per-app credential storage.

**Architecture:** Concrete `*service.WeChatOAuthService` paralleling `*service.GitHubOAuthService` 1:1 (no shared interface). New `internal/handler/auth_wechat.go` mounts `/auth/wechat/redirect` + `/auth/wechat/callback` on the same `publicLimiter` as `/auth/github/*`. Per-app WeChat credentials land in `apps.config.oauth_providers.wechat` (no DB migration — `social_identities.provider` CHECK already permits `'wechat'`). `provider_uid = "wechat_" + unionid`; if `unionid` is missing from the userinfo response, the callback rejects with `reason=wechat_no_unionid`.

**Tech Stack:** Go, Gin, sqlx, standard library `net/http`, `util.IssueOAuthState`/`VerifyOAuthState` (already provider-agnostic, reused).

**Spec:** `docs/superpowers/specs/2026-07-11-wechat-login-design.md`

**Working directory:** `/Users/lili/Downloads/github/yunhou-users`

---

## Task 1: Add `WeChatOAuthConfig` to `OAuthProvidersConfig`

**Files:**
- Modify: `internal/model/app.go:65-94`

- [ ] **Step 1: Edit `internal/model/app.go`**

Replace the existing `OAuthProvidersConfig` struct and add `WeChatOAuthConfig` after `GitHubOAuthConfig`:

```go
// OAuthProvidersConfig groups all OAuth providers configured for an app.
// Today GitHub and WeChat are supported; the block is structured so future
// providers slot in alongside.
type OAuthProvidersConfig struct {
	GitHub *GitHubOAuthConfig `json:"github,omitempty"`
	WeChat *WeChatOAuthConfig `json:"wechat,omitempty"`
}
```

Append after the `GitHubOAuthConfig` struct (after line 94):

```go
// WeChatOAuthConfig stores the WeChat Open Platform 网站应用 credentials
// Yunhou uses to run the /auth/wechat/redirect + /auth/wechat/callback
// flow on behalf of a consumer app. Same boundary contract as the GitHub
// block:
//
//   - AppID is the 微信开放平台 网站应用 AppID (public — appears in the
//     authorize URL anyway).
//   - AppSecret is server-side only. Never returned in any response body;
//     the handler maps ErrWeChatNotConfigured without surfacing the secret.
//   - CallbackURLs is the whitelist the callback handler matches the
//     incoming redirect_uri against. Multiple entries allowed (web / iOS /
//     Android sharing one WeChat 网站应用 registration).
//
// All Yunhou consumer apps that want cross-app unionid unification MUST
// register their 网站应用 under the SAME 微信开放平台 account — this is a
// Tencent-side requirement, not enforced in code.
type WeChatOAuthConfig struct {
	AppID        string   `json:"app_id"`
	AppSecret    string   `json:"app_secret"`
	CallbackURLs []string `json:"callback_urls"`
}
```

- [ ] **Step 2: Verify build still compiles**

Run: `make build`
Expected: `bin/server` produced, exit 0. The `model` package compiles, no consumers of `OAuthProvidersConfig` broke (GitHub field unchanged).

- [ ] **Step 3: Commit**

```bash
git add internal/model/app.go
git commit -m "feat(model): add WeChatOAuthConfig alongside GitHubOAuthConfig"
```

---

## Task 2: Add WeChat sentinel errors

**Files:**
- Modify: `internal/service/github_oauth.go` (or new file `internal/service/wechat_oauth.go` if preferable — see note)

**Note:** Errors can live in either file. Putting them in the eventual `wechat_oauth.go` keeps each provider self-contained, mirroring how `ErrGitHubUpstream` sits next to `GitHubOAuthService`. We'll create `wechat_oauth.go` in Task 3 and add the sentinels there.

Skip the separate "create errors file" task. Sentinels go in `wechat_oauth.go` (Task 3).

---

## Task 3: Implement `WeChatOAuthService` — service struct + sentinels + `BuildAuthorizeURL` + `VerifyCallbackState`

**Files:**
- Create: `internal/service/wechat_oauth.go`
- Create: `internal/service/wechat_oauth_test.go`

- [ ] **Step 1: Write the failing test for `BuildAuthorizeURL`**

Create `internal/service/wechat_oauth_test.go`:

```go
package service

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

// newWeChatTestSecret returns a 32-byte state secret for tests. The real
// config layer enforces ≥32 chars; mirroring here keeps tests honest about
// the production contract.
func newWeChatTestSecret(t *testing.T) string {
	t.Helper()
	if len(newGitHubTestSecret()) < 32 {
		t.Fatalf("test secret too short")
	}
	return newGitHubTestSecret()
}

func TestWeChatOAuthService_BuildAuthorizeURL_HappyPath(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	fixed := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, fixed)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}

	if !strings.HasPrefix(u, "https://open.weixin.qq.com/connect/qrconnect?") {
		t.Fatalf("unexpected base URL: %s", u)
	}
	if !strings.HasSuffix(u, "#wechat_redirect") {
		t.Fatalf("missing #wechat_redirect fragment: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()
	if q.Get("appid") != "wx0123456789abcdef" {
		t.Errorf("appid = %q, want wx0123456789abcdef", q.Get("appid"))
	}
	if q.Get("redirect_uri") != "https://bff.example.com/auth/wechat-callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("scope") != "snsapi_login,snsapi_userinfo" {
		t.Errorf("scope = %q, want snsapi_login,snsapi_userinfo", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Errorf("state is empty")
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_NilConfig(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	_, err := svc.BuildAuthorizeURL("yundian", nil, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_MissingAppID(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{AppSecret: "x", CallbackURLs: []string{"https://b"}}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_EmptyCallbackURLs(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{AppID: "wx...", AppSecret: "x"}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 0, time.Now())
	if err != ErrWeChatNotConfigured {
		t.Fatalf("err = %v, want ErrWeChatNotConfigured", err)
	}
}

func TestWeChatOAuthService_BuildAuthorizeURL_CallbackIndexOutOfRange(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx...",
		AppSecret:    "x",
		CallbackURLs: []string{"https://b"},
	}
	_, err := svc.BuildAuthorizeURL("yundian", cfg, 5, time.Now())
	if !errors.Is(err, ErrWeChatCallbackURLMismatch) {
		t.Fatalf("err = %v, want ErrWeChatCallbackURLMismatch", err)
	}
}

func TestWeChatOAuthService_VerifyCallbackState_HappyPath(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx...",
		AppSecret:    "x",
		CallbackURLs: []string{"https://b"},
	}
	fixed := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	u, err := svc.BuildAuthorizeURL("yundian", cfg, 0, fixed)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	state := extractQueryValue(t, u, "state")

	idx, err := svc.VerifyCallbackState(state, "yundian", fixed.Add(time.Minute))
	if err != nil {
		t.Fatalf("VerifyCallbackState: %v", err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0", idx)
	}
}

func TestWeChatOAuthService_VerifyCallbackState_Expired(t *testing.T) {
	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	cfg := &model.WeChatOAuthConfig{
		AppID: "wx...", AppSecret: "x", CallbackURLs: []string{"https://b"},
	}
	issue := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	u, _ := svc.BuildAuthorizeURL("yundian", cfg, 0, issue)
	state := extractQueryValue(t, u, "state")

	_, err := svc.VerifyCallbackState(state, "yundian", issue.Add(10*time.Minute))
	if err == nil {
		t.Fatalf("expected error for expired state")
	}
}
```

Note: `extractQueryValue` is a helper. Add it at the bottom of the test file:

```go
// extractQueryValue pulls a query parameter from a URL string for tests
// that build URLs via the service and need to read back the embedded
// state / code. Mirrors the helper at github_oauth_test.go:595.
func extractQueryValue(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return u.Query().Get(key)
}
```

Add `"errors"` to the imports if not already there.

- [ ] **Step 2: Run the failing test**

Run: `go test -race -run TestWeChatOAuth ./internal/service/`
Expected: FAIL — `wechat_oauth.go` doesn't exist.

- [ ] **Step 3: Create `internal/service/wechat_oauth.go`**

```go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

// WeChat OAuth redirect flow boundary (mirrors the GitHub boundary — see
// github_oauth.go for the full design contract):
//
//   - Yunhou is the only holder of the WeChat Open Platform 网站应用
//     AppSecret. The BFF never sees it.
//   - The state token is provider-agnostic — util.IssueOAuthState /
//     util.VerifyOAuthState bind (appID, callbackIndex) regardless of which
//     provider will echo the state back.
//   - Callback URLs are stored per app in
//     apps.config.oauth_providers.wechat.callback_urls and validated
//     against the incoming redirect_uri on every callback request.
//   - The WeChat access_token Yunhou receives during the code exchange is
//     used in-process only: one call to /sns/userinfo, then dropped. Never
//     written to DB, never returned to the BFF.
//
// Identity key: provider_uid = "wechat_" + unionid. We REJECT logins that
// lack a unionid — the shared-user-identity model across Yunhou consumer
// apps requires unionid, and falling back to openid would silently create
// per-app identity splits.

var (
	wechatAuthorizeURL   = "https://open.weixin.qq.com/connect/qrconnect"
	wechatAccessTokenURL = "https://api.weixin.qq.com/sns/oauth2/access_token"
	wechatUserInfoURL    = "https://api.weixin.qq.com/sns/userinfo"
	wechatOAuthHTTPClient = &http.Client{Timeout: 10 * time.Second}
)

// WeChatOAuthService is the entry point Yunhou's redirect handler uses to:
//  1. Build the upstream authorize URL the BFF redirects the user to.
//  2. Exchange the auth code WeChat returns at the callback endpoint.
//  3. Fetch the user's WeChat profile (nickname, headimgurl, unionid).
type WeChatOAuthService struct {
	stateSecret    []byte
	authorizeURL   string // override for tests; defaults to open.weixin.qq.com in prod
	accessTokenURL string // override for tests
	userInfoURL    string // override for tests
	httpClient     *http.Client
}

// NewWeChatOAuthService builds a service. stateSecret must be ≥32 chars;
// IssueRedirect will panic at request time if it's empty (matching the
// GitHub path's behaviour).
func NewWeChatOAuthService(stateSecret string) *WeChatOAuthService {
	return &WeChatOAuthService{
		stateSecret:    []byte(stateSecret),
		authorizeURL:   "https://open.weixin.qq.com/connect/qrconnect",
		accessTokenURL: "https://api.weixin.qq.com/sns/oauth2/access_token",
		userInfoURL:    "https://api.weixin.qq.com/sns/userinfo",
		httpClient:     &http.Client{Timeout: 10 * time.Second},
	}
}

// SetHTTPClient overrides the HTTP client used for upstream calls.
func (s *WeChatOAuthService) SetHTTPClient(c *http.Client) { s.httpClient = c }

// SetAccessTokenURL overrides the upstream access-token endpoint.
func (s *WeChatOAuthService) SetAccessTokenURL(u string) { s.accessTokenURL = u }

// SetAuthorizeURL overrides the upstream authorize endpoint.
func (s *WeChatOAuthService) SetAuthorizeURL(u string) { s.authorizeURL = u }

// SetUserInfoURL overrides the upstream /sns/userinfo endpoint.
func (s *WeChatOAuthService) SetUserInfoURL(u string) { s.userInfoURL = u }

// ErrWeChatNotConfigured signals that apps.config.oauth_providers.wechat
// is absent or empty for the requested app. Mapped to 404 by the handler.
var ErrWeChatNotConfigured = errors.New("wechat oauth not configured for app")

// ErrWeChatCallbackURLMismatch signals the redirect_uri submitted at
// callback time is not in the app's callback_urls whitelist. Mapped to 400.
var ErrWeChatCallbackURLMismatch = errors.New("redirect_uri not in wechat callback_urls whitelist")

// ErrWeChatUpstream signals a non-recoverable error from WeChat itself
// (network, 5xx, decode failure, errcode in body). Mapped to a BFF-facing
// #error=auth_failed&reason=wechat_upstream fragment.
var ErrWeChatUpstream = errors.New("wechat oauth upstream error")

// ErrWeChatNoUnionID signals that the /sns/userinfo response did not
// include unionid. This happens when the website app did not request
// snsapi_userinfo scope or the user did not grant it. We require unionid
// for cross-app identity unification, so we reject the login.
var ErrWeChatNoUnionID = errors.New("wechat userinfo missing unionid")

// BuildAuthorizeURL assembles the upstream WeChat authorize URL. The
// state token binds (appID, callbackIndex). The #wechat_redirect fragment
// is REQUIRED per WeChat docs — without it WeChat returns "该链接无法访问".
func (s *WeChatOAuthService) BuildAuthorizeURL(appID string, cfg *model.WeChatOAuthConfig, callbackIndex int, now time.Time) (string, error) {
	if cfg == nil {
		return "", ErrWeChatNotConfigured
	}
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return "", ErrWeChatNotConfigured
	}
	if len(cfg.CallbackURLs) == 0 {
		return "", ErrWeChatNotConfigured
	}
	if callbackIndex < 0 || callbackIndex >= len(cfg.CallbackURLs) {
		return "", fmt.Errorf("%w: callback_index %d out of range", ErrWeChatCallbackURLMismatch, callbackIndex)
	}

	state, err := util.IssueOAuthState(s.stateSecret, appID, callbackIndex, now)
	if err != nil {
		return "", fmt.Errorf("issue state: %w", err)
	}

	q := url.Values{}
	q.Set("appid", cfg.AppID)
	q.Set("redirect_uri", cfg.CallbackURLs[callbackIndex])
	q.Set("response_type", "code")
	q.Set("scope", "snsapi_login,snsapi_userinfo")
	q.Set("state", state)
	return s.authorizeURL + "?" + q.Encode() + "#wechat_redirect", nil
}

// VerifyCallbackState confirms the state token WeChat echoed back came
// from our /auth/wechat/redirect handler. Returns callbackIndex.
func (s *WeChatOAuthService) VerifyCallbackState(state, expectedAppID string, now time.Time) (int, error) {
	return util.VerifyOAuthState(s.stateSecret, state, expectedAppID, now)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -run TestWeChatOAuth ./internal/service/`
Expected: PASS for all 6 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/service/wechat_oauth.go internal/service/wechat_oauth_test.go
git commit -m "feat(service): WeChatOAuthService with BuildAuthorizeURL + VerifyCallbackState"
```

---

## Task 4: Implement `WeChatOAuthService.ExchangeCode`

**Files:**
- Modify: `internal/service/wechat_oauth.go`
- Modify: `internal/service/wechat_oauth_test.go`

- [ ] **Step 1: Append failing tests for `ExchangeCode`**

Add to `internal/service/wechat_oauth_test.go`:

```go
import (
	"net/http/httptest"
	// ... existing imports
)

func TestWeChatOAuthService_ExchangeCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		if q.Get("appid") != "wx..." || q.Get("secret") != "sec" || q.Get("code") != "auth-code" || q.Get("grant_type") != "authorization_code" {
			t.Errorf("unexpected query: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"AT","expires_in":7200,"refresh_token":"RT","openid":"oid","scope":"snsapi_login,snsapi_userinfo","unionid":"uid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)

	cfg := &model.WeChatOAuthConfig{AppID: "wx...", AppSecret: "sec", CallbackURLs: []string{"https://b"}}
	tok, err := svc.ExchangeCode(context.Background(), cfg, "auth-code", "https://b/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "AT" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if tok.OpenID != "oid" {
		t.Errorf("OpenID = %q", tok.OpenID)
	}
	if tok.RefreshToken != "RT" {
		t.Errorf("RefreshToken = %q", tok.RefreshToken)
	}
}

func TestWeChatOAuthService_ExchangeCode_UpstreamErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"errcode":40029,"errmsg":"invalid code"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{AppID: "wx...", AppSecret: "sec", CallbackURLs: []string{"https://b"}}

	_, err := svc.ExchangeCode(context.Background(), cfg, "bad", "https://b/cb")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_ExchangeCode_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{AppID: "wx...", AppSecret: "sec", CallbackURLs: []string{"https://b"}}

	_, err := svc.ExchangeCode(context.Background(), cfg, "x", "https://b/cb")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_ExchangeCode_EmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"expires_in":7200,"openid":"oid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetAccessTokenURL(srv.URL)
	cfg := &model.WeChatOAuthConfig{AppID: "wx...", AppSecret: "sec", CallbackURLs: []string{"https://b"}}

	_, err := svc.ExchangeCode(context.Background(), cfg, "x", "https://b/cb")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}
```

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run TestWeChatOAuthService_ExchangeCode ./internal/service/`
Expected: FAIL — `ExchangeCode` doesn't exist.

- [ ] **Step 3: Add `ExchangeCode` to `wechat_oauth.go`**

Append after `VerifyCallbackState`:

```go
// wechatAccessToken is the parsed shape of /sns/oauth2/access_token's
// success body. unionid is intentionally NOT included here — the handler
// reads unionid from FetchWeChatProfile's response so there's a single
// source of truth and a single missing-unionid sentinel path.
type wechatAccessToken struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	OpenID       string `json:"openid"`
	Scope        string `json:"scope"`
}

// ExchangeCode trades the auth code WeChat returned for an access token
// + openid. WeChat's code-exchange endpoint is GET (not POST like
// GitHub's). Returns the parsed token struct — caller is responsible for
// using access_token immediately and not retaining it.
func (s *WeChatOAuthService) ExchangeCode(ctx context.Context, cfg *model.WeChatOAuthConfig, code, redirectURI string) (*wechatAccessToken, error) {
	if cfg == nil || cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, ErrWeChatNotConfigured
	}
	if code == "" {
		return nil, errors.New("empty code")
	}

	q := url.Values{}
	q.Set("appid", cfg.AppID)
	q.Set("secret", cfg.AppSecret)
	q.Set("code", code)
	q.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.accessTokenURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build access_token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: access_token endpoint returned %d", ErrWeChatUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrWeChatUpstream, err)
	}

	// Inspect for errcode BEFORE decoding as access_token — a 200 body
	// with errcode is a failure, not a token.
	var errResp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, errResp.ErrCode, errResp.ErrMsg)
	}

	var parsed wechatAccessToken
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("%w: empty access_token in response", ErrWeChatUpstream)
	}
	return &parsed, nil
}
```

Add to imports: `"net/http"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -run TestWeChatOAuthService_ExchangeCode ./internal/service/`
Expected: PASS for all 4 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/service/wechat_oauth.go internal/service/wechat_oauth_test.go
git commit -m "feat(service): WeChatOAuthService.ExchangeCode — GET /sns/oauth2/access_token"
```

---

## Task 5: Implement `WeChatOAuthService.FetchWeChatProfile`

**Files:**
- Modify: `internal/service/wechat_oauth.go`
- Modify: `internal/service/wechat_oauth_test.go`

- [ ] **Step 1: Append failing tests for `FetchWeChatProfile`**

Add to `internal/service/wechat_oauth_test.go`:

```go
func TestWeChatOAuthService_FetchWeChatProfile_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("access_token") != "AT" || q.Get("openid") != "oid" || q.Get("lang") != "zh_CN" {
			t.Errorf("unexpected query: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"openid":"oid","nickname":"nick","sex":1,"headimgurl":"http://img","unionid":"uid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	got, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if err != nil {
		t.Fatalf("FetchWeChatProfile: %v", err)
	}
	if got.Provider != "wechat" {
		t.Errorf("Provider = %q, want wechat", got.Provider)
	}
	if got.ProviderUID != "wechat_uid" {
		t.Errorf("ProviderUID = %q, want wechat_uid", got.ProviderUID)
	}
	if got.Email != "" {
		t.Errorf("Email = %q, want empty", got.Email)
	}
	if got.Nickname != "nick" {
		t.Errorf("Nickname = %q", got.Nickname)
	}
	if got.AvatarURL != "http://img" {
		t.Errorf("AvatarURL = %q", got.AvatarURL)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_MissingUnionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// userinfo without unionid — happens when snsapi_userinfo was not granted
		fmt.Fprint(w, `{"openid":"oid","nickname":"nick","headimgurl":"http://img"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	_, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if !errors.Is(err, ErrWeChatNoUnionID) {
		t.Fatalf("err = %v, want ErrWeChatNoUnionID", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_UpstreamErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"errcode":40001,"errmsg":"invalid credential"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	_, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if !errors.Is(err, ErrWeChatUpstream) {
		t.Fatalf("err = %v, want ErrWeChatUpstream", err)
	}
}

func TestWeChatOAuthService_FetchWeChatProfile_MissingOptionalFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// user has no nickname / no headimgurl — fields are optional
		fmt.Fprint(w, `{"openid":"oid","unionid":"uid"}`)
	}))
	defer srv.Close()

	svc := NewWeChatOAuthService(newWeChatTestSecret(t))
	svc.SetUserInfoURL(srv.URL)

	got, err := svc.FetchWeChatProfile(context.Background(), "AT", "oid")
	if err != nil {
		t.Fatalf("FetchWeChatProfile: %v", err)
	}
	if got.Nickname != "" || got.AvatarURL != "" {
		t.Errorf("expected empty nickname/avatar, got %+v", got)
	}
	if got.ProviderUID != "wechat_uid" {
		t.Errorf("ProviderUID = %q, want wechat_uid", got.ProviderUID)
	}
}
```

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run TestWeChatOAuthService_FetchWeChatProfile ./internal/service/`
Expected: FAIL — `FetchWeChatProfile` doesn't exist.

- [ ] **Step 3: Add `FetchWeChatProfile` to `wechat_oauth.go`**

Append after `ExchangeCode`:

```go
// FetchWeChatProfile calls /sns/userinfo using the access_token + openid
// returned by ExchangeCode. Returns a ProviderUserInfo with
// provider="wechat" and provider_uid="wechat_<unionid>". The access_token
// is used exactly once; the caller MUST drop it after this returns.
//
// Email is always "" — WeChat's /sns/userinfo does NOT expose email.
// This means WeChat identities can never trigger the cross-provider
// email-merge in AuthService.resolveOrCreateUser; a WeChat-only user
// always gets a fresh Yunhou account on first login. (Design doc §4.)
func (s *WeChatOAuthService) FetchWeChatProfile(ctx context.Context, accessToken, openID string) (*ProviderUserInfo, error) {
	if accessToken == "" {
		return nil, errors.New("empty access token")
	}
	if openID == "" {
		return nil, errors.New("empty openid")
	}

	q := url.Values{}
	q.Set("access_token", accessToken)
	q.Set("openid", openID)
	q.Set("lang", "zh_CN")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.userInfoURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build userinfo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: userinfo endpoint returned %d", ErrWeChatUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrWeChatUpstream, err)
	}

	// errcode path — 200 body with errcode is a failure
	var errResp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.ErrCode != 0 {
		return nil, fmt.Errorf("%w: errcode=%d errmsg=%s", ErrWeChatUpstream, errResp.ErrCode, errResp.ErrMsg)
	}

	var parsed struct {
		OpenID     string `json:"openid"`
		UnionID    string `json:"unionid"`
		Nickname   string `json:"nickname"`
		HeadImgURL string `json:"headimgurl"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrWeChatUpstream, err)
	}

	if parsed.UnionID == "" {
		// Design decision: we REQUIRE unionid. A userinfo response without
		// it means snsapi_userinfo was not granted (e.g. the operator
		// forgot to register the scope, or the user denied it). Reject
		// the login rather than silently creating a per-app identity.
		return nil, ErrWeChatNoUnionID
	}

	return &ProviderUserInfo{
		Provider:    "wechat",
		ProviderUID: "wechat_" + parsed.UnionID,
		Email:       "",
		Nickname:    parsed.Nickname,
		AvatarURL:   parsed.HeadImgURL,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -run TestWeChatOAuthService_FetchWeChatProfile ./internal/service/`
Expected: PASS for all 4 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/service/wechat_oauth.go internal/service/wechat_oauth_test.go
git commit -m "feat(service): WeChatOAuthService.FetchWeChatProfile — /sns/userinfo with unionid gate"
```

---

## Task 6: Extract `buildCallbackRedirectURL` to `internal/handler/auth_common.go`

**Files:**
- Create: `internal/handler/auth_common.go`
- Create: `internal/handler/auth_common_test.go`
- Modify: `internal/handler/auth_github.go` (delete `attachYunhouJWTToURL`, replace its call sites with `buildCallbackRedirectURL`)
- Modify: `internal/handler/auth_github_test.go` (update test imports if `TestAttachYunhouJWTToURL_*` lives there)

- [ ] **Step 1: Move `TestAttachYunhouJWTToURL_*` tests if they exist in `auth_github_test.go`**

Search: `grep -n "TestAttachYunhouJWTToURL" internal/handler/auth_github_test.go`

If tests exist (per CLAUDE.md reference at line 808), create `internal/handler/auth_common_test.go` with the same test bodies but renamed to `TestBuildCallbackRedirectURL_*` and pointing at the new function. Example shape:

```go
package handler

import (
	"testing"

	"github.com/yunhou/users/internal/service"
)

func TestBuildCallbackRedirectURL_HappyPath(t *testing.T) {
	resp := &service.LoginResponse{
		AccessToken:  "AT",
		RefreshToken: "RT",
		User:         service.UserInfo{ID: "u-1"},
		Subscription: &service.SubscriptionInfo{HasAccess: true},
	}
	got := buildCallbackRedirectURL("https://bff.example.com/auth/callback", resp)
	want := "https://bff.example.com/auth/callback#token=AT&refresh_token=RT&user_id=u-1&has_access=true"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCallbackRedirectURL_NoSubscription(t *testing.T) {
	resp := &service.LoginResponse{
		AccessToken:  "AT",
		RefreshToken: "RT",
		User:         service.UserInfo{ID: "u-1"},
		// Subscription nil — has_access must be omitted from fragment
	}
	got := buildCallbackRedirectURL("https://bff.example.com/auth/callback", resp)
	if strings.Contains(got, "has_access") {
		t.Errorf("has_access must be omitted when Subscription is nil, got %s", got)
	}
}

func TestBuildCallbackRedirectURL_HasAccessFalse(t *testing.T) {
	resp := &service.LoginResponse{
		AccessToken:  "AT",
		RefreshToken: "RT",
		User:         service.UserInfo{ID: "u-1"},
		Subscription: &service.SubscriptionInfo{HasAccess: false},
	}
	got := buildCallbackRedirectURL("https://bff.example.com/auth/callback", resp)
	if !strings.Contains(got, "has_access=false") {
		t.Errorf("expected has_access=false in fragment, got %s", got)
	}
}

func TestBuildCallbackRedirectURL_NilResponse(t *testing.T) {
	got := buildCallbackRedirectURL("https://bff.example.com/auth/callback", nil)
	if !strings.HasSuffix(got, "#") {
		t.Errorf("nil response should produce empty fragment, got %s", got)
	}
}
```

If the original GitHub tests have additional edge cases (URL parse errors, special chars, etc.), copy them verbatim, just renaming `attachYunhouJWTToURL` → `buildCallbackRedirectURL`.

- [ ] **Step 2: Create `internal/handler/auth_common.go`**

```go
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
	fragment := url.Values{}
	if resp == nil {
		// Empty response — still emit the # marker so the BFF's
		// client-side handler can route on "is there a token".
		s := u.String()
		return s + "#"
	}
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
```

- [ ] **Step 3: Edit `internal/handler/auth_github.go`**

Delete the `attachYunhouJWTToURL` function (lines 311–346 — adjust to actual line numbers if shifted). Update its only caller (in `Callback`, around line 254) from `attachYunhouJWTToURL(redirectURI, resp)` to `buildCallbackRedirectURL(redirectURI, resp)`.

- [ ] **Step 4: Edit `internal/handler/auth_github_test.go`**

If `TestAttachYunhouJWTToURL_*` tests were in this file, delete them (they moved to `auth_common_test.go` in step 1). Replace any remaining calls to `attachYunhouJWTToURL` with `buildCallbackRedirectURL`.

- [ ] **Step 5: Run all handler tests**

Run: `go test -race ./internal/handler/`
Expected: PASS. Both the moved `TestBuildCallbackRedirectURL_*` tests and the existing GitHub handler tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/auth_common.go internal/handler/auth_common_test.go internal/handler/auth_github.go internal/handler/auth_github_test.go
git commit -m "refactor(handler): extract buildCallbackRedirectURL to auth_common.go

Shared by /auth/github/callback and the upcoming /auth/wechat/callback.
Single canonical implementation of the BFF-facing fragment contract."
```

---

## Task 7: Add `validateWeChatOAuthConfig` and branch in `validateAppConfig`

**Files:**
- Modify: `internal/handler/app.go:288-308`
- Modify: `internal/handler/app_test.go` (or wherever `validateAppConfig` tests live)

- [ ] **Step 1: Find the existing `validateAppConfig` tests**

Run: `grep -n "validateAppConfig\|TestValidateAppConfig\|TestValidateGitHubOAuthConfig" internal/handler/*_test.go`

Add a parallel set of tests for `validateWeChatOAuthConfig` next to the GitHub ones.

- [ ] **Step 2: Write the failing tests**

In the same `_test.go` file as the GitHub validation tests, append:

```go
func TestValidateWeChatOAuthConfig_HappyPath(t *testing.T) {
	c := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://bff.example.com/auth/wechat-callback"},
	}
	if err := validateWeChatOAuthConfig(c); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestValidateWeChatOAuthConfig_MissingAppID(t *testing.T) {
	c := &model.WeChatOAuthConfig{
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(c); err == nil {
		t.Fatal("expected error for missing app_id")
	}
}

func TestValidateWeChatOAuthConfig_InvalidAppIDFormat(t *testing.T) {
	c := &model.WeChatOAuthConfig{
		AppID:        "not-a-wechat-appid",
		AppSecret:    "0123456789abcdef0123456789abcdef",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(c); err == nil {
		t.Fatal("expected error for invalid app_id format")
	}
}

func TestValidateWeChatOAuthConfig_InvalidAppSecretLength(t *testing.T) {
	c := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "tooshort",
		CallbackURLs: []string{"https://b"},
	}
	if err := validateWeChatOAuthConfig(c); err == nil {
		t.Fatal("expected error for invalid app_secret length")
	}
}

func TestValidateWeChatOAuthConfig_EmptyCallbackURLs(t *testing.T) {
	c := &model.WeChatOAuthConfig{
		AppID:     "wx0123456789abcdef",
		AppSecret: "0123456789abcdef0123456789abcdef",
	}
	if err := validateWeChatOAuthConfig(c); err == nil {
		t.Fatal("expected error for empty callback_urls")
	}
}

func TestValidateAppConfig_WeChatBranch(t *testing.T) {
	cfg := &model.AppConfig{
		OAuthProviders: &model.OAuthProvidersConfig{
			WeChat: &model.WeChatOAuthConfig{
				AppID:        "wx0123456789abcdef",
				AppSecret:    "0123456789abcdef0123456789abcdef",
				CallbackURLs: []string{"https://b"},
			},
		},
	}
	if err := validateAppConfig(cfg); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
```

- [ ] **Step 3: Run the failing tests**

Run: `go test -race -run "TestValidateWeChatOAuthConfig|TestValidateAppConfig_WeChatBranch" ./internal/handler/`
Expected: FAIL — `validateWeChatOAuthConfig` doesn't exist.

- [ ] **Step 4: Add `validateWeChatOAuthConfig` and branch in `internal/handler/app.go`**

After `validateGitHubOAuthConfig`, append:

```go
// validateWeChatOAuthConfig enforces the boundary contract for a WeChat
// Open Platform 网站应用 stored in apps.config.oauth_providers.wechat.
// Required when the block is present; absence of the block means "WeChat
// login disabled for this app" and is allowed.
func validateWeChatOAuthConfig(w *model.WeChatOAuthConfig) error {
	if w.AppID == "" {
		return errors.New("oauth_providers.wechat.app_id is required")
	}
	// WeChat 网站应用 AppID is always "wx" + 16 hex chars. Validating the
	// pattern catches typos before they hit the live WeChat endpoint and
	// surface as a confusing errcode=40013.
	if matched, _ := regexp.MatchString(`^wx[0-9a-f]{16}$`, w.AppID); !matched {
		return errors.New("oauth_providers.wechat.app_id must match ^wx[0-9a-f]{16}$")
	}
	if len(w.AppSecret) != 32 {
		return errors.New("oauth_providers.wechat.app_secret must be 32 chars")
	}
	if len(w.CallbackURLs) == 0 {
		return errors.New("oauth_providers.wechat.callback_urls must list at least one URL")
	}
	seen := make(map[string]struct{}, len(w.CallbackURLs))
	for _, u := range w.CallbackURLs {
		if u == "" {
			return errors.New("oauth_providers.wechat.callback_urls entries must not be empty")
		}
		if !isAcceptableCallbackURL(u) {
			return errors.New("oauth_providers.wechat.callback_urls entries must be https:// or http://127.0.0.1 / http://localhost")
		}
		if _, dup := seen[u]; dup {
			return errors.New("oauth_providers.wechat.callback_urls must not contain duplicates")
		}
		seen[u] = struct{}{}
	}
	return nil
}
```

Update `validateAppConfig` (around line 288) to add the WeChat branch. The current code is:

```go
if gh := cfg.OAuthProviders; gh != nil {
    if g := gh.GitHub; g != nil {
        if err := validateGitHubOAuthConfig(g); err != nil {
            return err
        }
    }
}
```

Change to:

```go
if gh := cfg.OAuthProviders; gh != nil {
    if g := gh.GitHub; g != nil {
        if err := validateGitHubOAuthConfig(g); err != nil {
            return err
        }
    }
    if w := gh.WeChat; w != nil {
        if err := validateWeChatOAuthConfig(w); err != nil {
            return err
        }
    }
}
```

Add to imports at top of `app.go`: `"regexp"` (if not already present).

- [ ] **Step 5: Run the tests**

Run: `go test -race -run "TestValidateWeChatOAuthConfig|TestValidateAppConfig_WeChatBranch" ./internal/handler/`
Expected: PASS for all 7 new tests. Existing GitHub validation tests still pass.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/app.go internal/handler/app_test.go
git commit -m "feat(handler): validateWeChatOAuthConfig — app_id format, secret length, callback_urls"
```

---

## Task 8: Extend `authErrReason` map

**Files:**
- Modify: `internal/handler/auth.go:52-67`

- [ ] **Step 1: Find any existing tests for `authErrReason`**

Run: `grep -rn "authErrReason\|TestAuthErrReason" internal/handler/`

If no tests exist, add tests to `internal/handler/auth_test.go` (or whichever file holds `auth.go`'s tests). Append:

```go
func TestAuthErrReason_WeChatUpstream(t *testing.T) {
	if got := authErrReason(service.ErrWeChatUpstream); got != "wechat_upstream" {
		t.Fatalf("got %q, want wechat_upstream", got)
	}
}

func TestAuthErrReason_WeChatNoUnionID(t *testing.T) {
	if got := authErrReason(service.ErrWeChatNoUnionID); got != "wechat_no_unionid" {
		t.Fatalf("got %q, want wechat_no_unionid", got)
	}
}
```

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run "TestAuthErrReason_WeChat" ./internal/handler/`
Expected: FAIL — current `authErrReason` doesn't handle these sentinels (returns `auth_failed`).

- [ ] **Step 3: Update `authErrReason` in `internal/handler/auth.go`**

Replace the existing switch (lines 52–67) with:

```go
func authErrReason(err error) string {
	switch {
	case errors.Is(err, service.ErrAppNotFound):
		return "app_not_found"
	case errors.Is(err, service.ErrAppInactive):
		return "app_disabled"
	case errors.Is(err, service.ErrUserNotFound), errors.Is(err, service.ErrUserDeleted):
		return "user_not_found"
	case errors.Is(err, service.ErrUserSuspended):
		return "user_suspended"
	case errors.Is(err, service.ErrSubscriptionExpired), errors.Is(err, service.ErrSubscriptionNotActive):
		return "subscription_expired"
	case errors.Is(err, service.ErrWeChatUpstream):
		return "wechat_upstream"
	case errors.Is(err, service.ErrWeChatNoUnionID):
		return "wechat_no_unionid"
	default:
		return "auth_failed"
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -run "TestAuthErrReason" ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/auth.go internal/handler/auth_test.go
git commit -m "feat(handler): authErrReason maps wechat_upstream + wechat_no_unionid"
```

---

## Task 9: Implement WeChat Redirect handler

**Files:**
- Create: `internal/handler/auth_wechat.go`
- Create: `internal/handler/auth_wechat_test.go`

- [ ] **Step 1: Write the failing tests for `Redirect`**

Create `internal/handler/auth_wechat_test.go`:

```go
package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// wechatAppWithOAuth builds a minimal *model.App with the WeChat block
// populated. Mirrors ghAppWithOAuth in auth_github_test.go:54.
func wechatAppWithOAuth(callbackURLs ...string) *model.App {
	return &model.App{
		AppID:    "yundian",
		Name:     "yundian",
		IsActive: true,
		Config:   []byte(`{"oauth_providers":{"wechat":{"app_id":"wx0123456789abcdef","app_secret":"0123456789abcdef0123456789abcdef","callback_urls":["` + strings.Join(callbackURLs, `","`) + `"]}}}`),
	}
}

// wechatTestClock is a fixed clock for state expiry control. Mirrors
// fixedTestClock in auth_github_test.go:235.
var wechatTestClock = func() time.Time { return time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC) }

func installWeChatFixedClock(t *testing.T) {
	t.Helper()
	prev := wechatOAuthClock
	wechatOAuthClock = wechatTestClock
	t.Cleanup(func() { wechatOAuthClock = prev })
}

func TestWeChatOAuth_Redirect_HappyPath(t *testing.T) {
	installWeChatFixedClock(t)
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fbff.example.com%2Fauth%2Fwechat-callback", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://open.weixin.qq.com/connect/qrconnect?") {
		t.Fatalf("location = %q, want prefix open.weixin.qq.com/connect/qrconnect", loc)
	}
	if !strings.HasSuffix(loc, "#wechat_redirect") {
		t.Fatalf("location = %q, missing #wechat_redirect fragment", loc)
	}
	parsed, _ := url.Parse(loc)
	q := parsed.Query()
	if q.Get("appid") != "wx0123456789abcdef" {
		t.Errorf("appid = %q", q.Get("appid"))
	}
}

func TestWeChatOAuth_Redirect_MissingAppID(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Redirect_MissingRedirectURI(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://b")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Redirect_AppNotFound(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{err: errors.New("not found")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=missing&redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWeChatOAuth_Redirect_InactiveApp(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	app := wechatAppWithOAuth("https://b")
	app.IsActive = false
	appRepo := &stubAppLoader{app: app}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestWeChatOAuth_Redirect_WeChatNotConfigured(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	// App exists but no WeChat block in config
	appRepo := &stubAppLoader{app: &model.App{AppID: "yundian", IsActive: true, Config: []byte(`{}`)}}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWeChatOAuth_Redirect_CallbackURLMismatch(t *testing.T) {
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://allowed.example.com/cb")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fattacker.example.com%2Fcb", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
```

Add `"errors"` to imports.

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run "TestWeChatOAuth_Redirect" ./internal/handler/`
Expected: FAIL — `auth_wechat.go` doesn't exist.

- [ ] **Step 3: Create `internal/handler/auth_wechat.go`**

```go
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// wechatOAuthClock is the wall clock used by the WeChat redirect +
// callback handlers for state expiry checks. Tests can swap it via
// installWeChatFixedClock.
var wechatOAuthClock = time.Now

// wechatOAuthDeps bundles the service-layer dependencies for the WeChat
// redirect flow. Same shape as githubOAuthDeps.
type wechatOAuthDeps struct {
	svc     *service.WeChatOAuthService
	appRepo appLoader
	authSvc service.AuthServiceInterface
}

// RegisterWeChatOAuthRoutes attaches /auth/wechat/redirect and
// /auth/wechat/callback to the engine. Both endpoints are public (no JWT)
// — same posture as /auth/github/* and /auth/refresh.
func RegisterWeChatOAuthRoutes(engine gin.IRouter, svc *service.WeChatOAuthService, appRepo appLoader, authSvc service.AuthServiceInterface) {
	d := &wechatOAuthDeps{svc: svc, appRepo: appRepo, authSvc: authSvc}
	engine.GET("/auth/wechat/redirect", d.Redirect)
	engine.GET("/auth/wechat/callback", d.Callback)
}

// Redirect handles GET /auth/wechat/redirect?app_id=...&redirect_uri=...
func (d *wechatOAuthDeps) Redirect(c *gin.Context) {
	appID := c.Query("app_id")
	redirectURI := c.Query("redirect_uri")
	if appID == "" || redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "app_id and redirect_uri are required"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if !app.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "app is inactive"})
		return
	}

	cfg, callbackIdx, err := lookupWeChatConfig(app, redirectURI)
	if err != nil {
		if errors.Is(err, service.ErrWeChatNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "wechat oauth not configured for app"})
			return
		}
		if errors.Is(err, service.ErrWeChatCallbackURLMismatch) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "redirect_uri not in callback_urls whitelist"})
			return
		}
		// Malformed config — treat as misconfigured app (500). Operators
		// need to fix apps.config.oauth_providers.wechat.
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "invalid app config"})
		return
	}

	authorizeURL, err := d.svc.BuildAuthorizeURL(appID, cfg, callbackIdx, wechatOAuthClock())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "build authorize url"})
		return
	}

	c.Redirect(http.StatusFound, authorizeURL)
}

// lookupWeChatConfig mirrors lookupGitHubConfig (auth_github.go:262).
// Returns (cfg, callbackIdx, nil) when redirect_uri matches a whitelisted
// entry; (nil, -1, ErrWeChatNotConfigured) when no WeChat block exists or
// it has no callback URLs; (nil, 0, ErrWeChatCallbackURLMismatch) when the
// redirect_uri is not in the whitelist.
func lookupWeChatConfig(app *model.App, redirectURI string) (cfg *model.WeChatOAuthConfig, callbackIdx int, err error) {
	if app == nil || len(app.Config) == 0 {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	var ac model.AppConfig
	if err := json.Unmarshal(app.Config, &ac); err != nil {
		return nil, 0, err
	}
	if ac.OAuthProviders == nil || ac.OAuthProviders.WeChat == nil {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	cfg = ac.OAuthProviders.WeChat
	if len(cfg.CallbackURLs) == 0 {
		return nil, 0, service.ErrWeChatNotConfigured
	}
	if redirectURI == "" {
		return cfg, 0, nil
	}
	want := normalizeCallbackURLForCompare(redirectURI)
	for i, u := range cfg.CallbackURLs {
		if normalizeCallbackURLForCompare(u) == want {
			return cfg, i, nil
		}
	}
	return nil, 0, service.ErrWeChatCallbackURLMismatch
}
```

Add to imports: `"time"` (for `time.Now` in `wechatOAuthClock` declaration).

- [ ] **Step 4: Run the Redirect tests**

Run: `go test -race -run "TestWeChatOAuth_Redirect" ./internal/handler/`
Expected: PASS for all 7 redirect tests.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/auth_wechat.go internal/handler/auth_wechat_test.go
git commit -m "feat(handler): /auth/wechat/redirect — lookupWeChatConfig + 302 to WeChat"
```

---

## Task 10: Implement WeChat Callback handler

**Files:**
- Modify: `internal/handler/auth_wechat.go`
- Modify: `internal/handler/auth_wechat_test.go`

- [ ] **Step 1: Write the failing tests for `Callback`**

Append to `internal/handler/auth_wechat_test.go`:

```go
// wechatCallbackURIFor builds a callback URL with a valid state token
// pointing at svc — used to forge a "WeChat is calling us back" URL in
// tests. Mirrors callbackURIFor in auth_github_test.go:244.
func wechatCallbackURIFor(t *testing.T, svc *service.WeChatOAuthService, appID, callbackURL string) string {
	t.Helper()
	installWeChatFixedClock(t)
	cfg := &model.WeChatOAuthConfig{
		AppID:        "wx0123456789abcdef",
		AppSecret:    "01234567890123456789012345678901",
		CallbackURLs: []string{callbackURL},
	}
	u, err := svc.BuildAuthorizeURL(appID, cfg, 0, wechatTestClock())
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	parsed, _ := url.Parse(u)
	return callbackURL + "?app_id=" + appID + "&code=AUTH_CODE&state=" + parsed.Query().Get("state")
}

type stubWeChatOAuthService struct {
	*service.WeChatOAuthService
	exchangeResp *service.wechatAccessToken
	exchangeErr  error
	profileResp  *service.ProviderUserInfo
	profileErr   error
}

// Override ExchangeCode + FetchWeChatProfile on a clone. Since the
// service struct is concrete, the simplest stub is to construct a
// service that points SetAccessTokenURL/SetUserInfoURL at httptest
// servers returning canned bodies.
func newWeChatStubService(t *testing.T, exchangeJSON string, userInfoJSON string, exchangeStatus, userInfoStatus int) (*service.WeChatOAuthService, func()) {
	t.Helper()
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if exchangeStatus != 0 {
			w.WriteHeader(exchangeStatus)
		}
		fmt.Fprint(w, exchangeJSON)
	}))
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if userInfoStatus != 0 {
			w.WriteHeader(userInfoStatus)
		}
		fmt.Fprint(w, userInfoJSON)
	}))
	svc := service.NewWeChatOAuthService("01234567890123456789012345678901")
	svc.SetAccessTokenURL(exchangeSrv.URL)
	svc.SetUserInfoURL(userInfoSrv.URL)
	return svc, func() {
		exchangeSrv.Close()
		userInfoSrv.Close()
	}
}

func TestWeChatOAuth_Callback_HappyPath(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"access_token":"AT","expires_in":7200,"openid":"oid","scope":"snsapi_login,snsapi_userinfo"}`,
		`{"openid":"oid","unionid":"uid","nickname":"nick","headimgurl":"http://img"}`,
		0, 0,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{
		loginResp: &service.LoginResponse{
			AccessToken:  "yunhou-access",
			RefreshToken: "yunhou-refresh",
			User:         service.UserInfo{ID: "user-uuid"},
			Subscription: &service.SubscriptionInfo{HasAccess: true},
		},
	}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, cbURL+"#") {
		t.Fatalf("location = %q, want prefix %s#", loc, cbURL)
	}
	if !strings.Contains(loc, "token=yunhou-access") {
		t.Errorf("location missing yunhou access token: %s", loc)
	}
	if !strings.Contains(loc, "refresh_token=yunhou-refresh") {
		t.Errorf("location missing refresh token: %s", loc)
	}
	if !strings.Contains(loc, "user_id=user-uuid") {
		t.Errorf("location missing user_id: %s", loc)
	}
	if !strings.Contains(loc, "has_access=true") {
		t.Errorf("location missing has_access=true: %s", loc)
	}
}

func TestWeChatOAuth_Callback_MissingUnionID(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"access_token":"AT","openid":"oid"}`,
		`{"openid":"oid","nickname":"nick"}`, // no unionid
		0, 0,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=auth_failed") || !strings.Contains(loc, "reason=wechat_no_unionid") {
		t.Fatalf("location = %q, want error=auth_failed&reason=wechat_no_unionid", loc)
	}
}

func TestWeChatOAuth_Callback_UpstreamErrcode(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t,
		`{"errcode":40029,"errmsg":"invalid code"}`,
		``,
		0, 0,
	)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	uri := wechatCallbackURIFor(t, svc, "yundian", cbURL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uri, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "reason=wechat_upstream") {
		t.Fatalf("location = %q, want reason=wechat_upstream", loc)
	}
}

func TestWeChatOAuth_Callback_InvalidState(t *testing.T) {
	svc, cleanup := newWeChatStubService(t, `{}`, `{}`, 0, 0)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian&code=x&state=garbage", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Callback_MissingCodeOrState(t *testing.T) {
	svc, _ := newWeChatStubService(t, `{}`, `{}`, 0, 0)
	appRepo := &stubAppLoader{app: wechatAppWithOAuth("https://bff.example.com/auth/wechat-callback")}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWeChatOAuth_Callback_WeChatErrorParam(t *testing.T) {
	installWeChatFixedClock(t)
	cbURL := "https://bff.example.com/auth/wechat-callback"
	svc, cleanup := newWeChatStubService(t, `{}`, `{}`, 0, 0)
	defer cleanup()

	appRepo := &stubAppLoader{app: wechatAppWithOAuth(cbURL)}
	authSvc := &stubAuthSvc{}

	r := gin.New()
	RegisterWeChatOAuthRoutes(r, svc, appRepo, authSvc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/wechat/callback?app_id=yundian&error=access_denied&error_description=user+denied", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=access_denied") {
		t.Errorf("location = %q, want error=access_denied", loc)
	}
}
```

Add `"context"` and `"fmt"` to imports if not already present.

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run "TestWeChatOAuth_Callback" ./internal/handler/`
Expected: FAIL — `Callback` method doesn't exist.

- [ ] **Step 3: Add `Callback` to `internal/handler/auth_wechat.go`**

Append after `Redirect`:

```go
// Callback handles GET /auth/wechat/callback?app_id=...&code=...&state=...
// WeChat calls this URL after the user scans the QR code and confirms
// (or denies) authorization. On success we exchange the code, fetch the
// user profile, and redirect to the BFF with Yunhou's JWT in the fragment.
func (d *wechatOAuthDeps) Callback(c *gin.Context) {
	// WeChat-side denial: ?error=access_denied&error_description=...
	// Mirror GitHub's behavior — redirect to the BFF with the error
	// echoed in the fragment so the BFF can show a localized message.
	if ghErr := c.Query("error"); ghErr != "" {
		ghErrDesc := c.Query("error_description")
		state := c.Query("state")
		appID := c.Query("app_id")
		if state != "" && appID != "" {
			if _, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock()); err == nil {
				if app, err := d.appRepo.FindByID(c.Request.Context(), appID); err == nil {
					cfg, _, lerr := lookupWeChatConfig(app, "")
					if lerr == nil && len(cfg.CallbackURLs) > 0 {
						redirect := cfg.CallbackURLs[0]
						c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirect, nil)+"&error="+url.QueryEscape(ghErr)+"&error_description="+url.QueryEscape(ghErrDesc))
						return
					}
				}
			}
		}
		// Couldn't resolve redirect target — fall back to JSON 400.
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": ghErr + ": " + ghErrDesc})
		return
	}

	appID := c.Query("app_id")
	code := c.Query("code")
	state := c.Query("state")
	if appID == "" || code == "" || state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "app_id, code, state are required"})
		return
	}

	if _, err := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state"})
		return
	}

	app, err := d.appRepo.FindByID(c.Request.Context(), appID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}

	cfg, _, err := lookupWeChatConfig(app, "")
	if err != nil {
		if errors.Is(err, service.ErrWeChatNotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "wechat oauth not configured for app"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "invalid app config"})
		return
	}

	// Pick the verified callback URL from state so the BFF's
	// redirect_uri matches what WeChat registered. Mirrors the GitHub
	// pattern at auth_github.go (verifiedIdx from state).
	verifiedIdx, verr := d.svc.VerifyCallbackState(state, appID, wechatOAuthClock())
	if verr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state index"})
		return
	}
	redirectURI := cfg.CallbackURLs[verifiedIdx]

	tok, err := d.svc.ExchangeCode(c.Request.Context(), cfg, code, redirectURI)
	if err != nil {
		if errors.Is(err, service.ErrWeChatUpstream) {
			// We need a BFF target to redirect to — re-resolve via state.
			// State is already verified above, so we can extract callback
			// from cfg.
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_upstream")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "exchange code"})
		return
	}

	profile, err := d.svc.FetchWeChatProfile(c.Request.Context(), tok.AccessToken, tok.OpenID)
	if err != nil {
		if errors.Is(err, service.ErrWeChatNoUnionID) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_no_unionid")
			return
		}
		if errors.Is(err, service.ErrWeChatUpstream) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason=wechat_upstream")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "fetch profile"})
		return
	}

	loginResp, err := d.authSvc.LoginWithProfile(c.Request.Context(), service.LoginWithProfileRequest{
		Profile: profile,
		AppID:   appID,
	})
	if err != nil {
		if isExpectedAuthErr(err) {
			c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, nil)+"&error=auth_failed&reason="+url.QueryEscape(authErrReason(err)))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login"})
		return
	}

	c.Redirect(http.StatusFound, buildCallbackRedirectURL(redirectURI, loginResp))
}
```

Note: The above uses `cfg.CallbackURLs[0]` as the redirect URI for the post-callback redirect. We could verify it matches the `redirect_uri` from state, but since `state` is already verified, this is sufficient. Add a comment explaining.

- [ ] **Step 4: Run the Callback tests**

Run: `go test -race -run "TestWeChatOAuth_Callback" ./internal/handler/`
Expected: PASS for all 6 callback tests.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/auth_wechat.go internal/handler/auth_wechat_test.go
git commit -m "feat(handler): /auth/wechat/callback — code exchange + profile fetch + JWT redirect"
```

---

## Task 11: Wire `/auth/wechat/*` in router

**Files:**
- Modify: `internal/router/router.go:13-33, 56-57`
- Modify: `internal/router/router_test.go` (pass nil for new param)

- [ ] **Step 1: Update `router.Setup` signature**

In `internal/router/router.go`, add `wechatOAuthSvc *service.WeChatOAuthService` as the new last parameter to `Setup`:

```go
func Setup(
	ctx context.Context,
	engine *gin.Engine,
	healthPinger handler.Pinger,
	appRepo repo.AppRepo,
	userRepo repo.UserRepo,
	identityRepo repo.SocialIdentityRepo,
	planRepo repo.PlanRepo,
	subRepo repo.SubscriptionRepo,
	sessionRepo repo.SessionRepo,
	tokenSvc *service.TokenService,
	authSvc *service.AuthService,
	subSvc *service.SubscriptionService,
	planSvc *service.PlanService,
	paymentSvc *service.PaymentService,
	webhookVerifier middleware.ChannelSignatureVerifier,
	wechatAPIv3Key []byte,
	providerTokenSvc *service.ProviderTokenService,
	quoteSvc *service.QuoteService,
	githubOAuthSvc *service.GitHubOAuthService,
	wechatOAuthSvc *service.WeChatOAuthService, // NEW
) {
```

After the existing GitHub OAuth group registration (around line 56-57), add:

```go
	// WeChat OAuth redirect flow — same shape as GitHub. Both endpoints
	// are public, rate-limited via the same public limiter, no JWT.
	wechatOAuthGroup := engine.Group("/auth/wechat", publicLimiter)
	handler.RegisterWeChatOAuthRoutes(wechatOAuthGroup, wechatOAuthSvc, appRepo, authSvc)
```

- [ ] **Step 2: Update `internal/router/router_test.go`**

Find the `router.Setup` call (around line 228) and add a trailing `nil` for the new parameter:

```go
nil, nil, // githubOAuthSvc, wechatOAuthSvc (test-only; Setup only stores pointers)
```

- [ ] **Step 3: Build to confirm wiring compiles**

Run: `make build`
Expected: FAIL — `cmd/server/main.go` hasn't been updated to pass `wechatOAuthSvc` yet.

- [ ] **Step 4: Commit**

```bash
git add internal/router/router.go internal/router/router_test.go
git commit -m "feat(router): wire /auth/wechat/* on publicLimiter"
```

---

## Task 12: Construct WeChatOAuthService in main and pass to router

**Files:**
- Modify: `cmd/server/main.go:148-154`

- [ ] **Step 1: Update `cmd/server/main.go`**

After line 148 (`githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)`), add:

```go
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
```

Update the `router.Setup` call (line 150-154) to add `wechatOAuthSvc` as the new last arg:

```go
	router.Setup(rootCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, webhookVerifier, []byte(cfg.WeChatAPIv3Key),
		providerTokenSvc, quoteSvc, githubOAuthSvc, wechatOAuthSvc)
```

- [ ] **Step 2: Build and run tests**

Run: `make build && make test`
Expected: `bin/server` produced, exit 0. All tests pass.

- [ ] **Step 3: Smoke-test that the binary starts**

Run: `OAUTH_STATE_SECRET=$(openssl rand -hex 32) ./bin/server &`
Wait 2 seconds, then: `curl -sS http://localhost:8080/healthz`
Expected: `{"code":0,"data":{"status":"ok"}}`
Then: `curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:8080/auth/wechat/redirect`
Expected: `400` (missing `app_id` and `redirect_uri`).
Kill the server with `kill %1`.

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat(server): construct WeChatOAuthService + pass to router.Setup"
```

---

## Task 13: Update CLAUDE.md

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update the GitHub OAuth Boundary section**

In the "GitHub OAuth Boundary" table, the first row says "GitHub OAuth App `client_id`" — generalize or add a parallel paragraph for WeChat. Find the table and add a sibling paragraph:

After the GitHub OAuth Boundary table, add:

```markdown
The same boundary applies to WeChat Open Platform 网站应用:

| Key information | Held by | Reachable by |
|---|---|---|
| WeChat Open Platform AppID | yunhou-users | Echoed in the upstream `open.weixin.qq.com/connect/qrconnect` URL. Public by design. |
| WeChat Open Platform AppSecret | yunhou-users only | **Never** sent to the BFF. Used only inside `/auth/wechat/callback` to exchange the auth code for an access token. |
| `callback_urls` whitelist | yunhou-users | Compared against the BFF-supplied `redirect_uri` on every callback. Stored as plaintext array in `apps.config.oauth_providers.wechat.callback_urls`. |
| WeChat `access_token` (after code exchange) | yunhou-users (transient) | Used exactly once — `/sns/userinfo` — then dropped. Never written to DB. Never returned to the BFF. |
| WeChat `refresh_token` (after code exchange) | yunhou-users (transient) | Discarded. Yunhou has its own refresh-token model; the WeChat refresh_token has no use beyond refreshing the WeChat access_token, which Yunhou does not need post-login. |
| `state` token (CSRF + open-redirect defence) | yunhou-users | Same HMAC mechanism as GitHub. Reuses `OAUTH_STATE_SECRET`. |

**Cross-app unionid unification** requires all Yunhou consumer apps to register their WeChat 网站应用 under the SAME 微信开放平台 account — this is a Tencent-side requirement, not enforced in code. Documented in the consumer-app onboarding runbook.
```

Also update the "Endpoints" section to add `/auth/wechat/redirect` and `/auth/wechat/callback` under the **Public** group.

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs(CLAUDE.md): document WeChat login boundary + endpoints"
```

---

## Task 14: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Run full test suite**

Run: `make test`
Expected: All tests pass with `ok` for each package. New tests in `internal/service/wechat_oauth_test.go` and `internal/handler/auth_wechat_test.go` are included.

- [ ] **Step 2: Run lint**

Run: `make lint`
Expected: exit 0.

- [ ] **Step 3: Run full build**

Run: `make build`
Expected: `bin/server` produced.

- [ ] **Step 4: Manual smoke test (developer environment)**

1. Start the server: `OAUTH_STATE_SECRET=$(openssl rand -hex 32) ./bin/server &`
2. `curl -i 'http://localhost:8080/auth/wechat/redirect?app_id=yundian&redirect_uri=https%3A%2F%2Fbff.example.com%2Fcb'` → expect 404 (no app `yundian` in DB) or 404 (`ErrWeChatNotConfigured` if app exists but no WeChat block).
3. `curl -i 'http://localhost:8080/auth/wechat/callback?app_id=x&code=y&state=z'` → expect 400 (invalid state).
4. `curl -sS http://localhost:8080/healthz` → expect `{"code":0,"data":{"status":"ok"}}`.

- [ ] **Step 5: Verify acceptance criteria from spec §13**

| Criterion | Status |
|---|---|
| 1. `make build` succeeds, server starts, `/auth/wechat/redirect` + `/auth/wechat/callback` registered | ✓ from Step 3, Step 4 |
| 2. `make test` passes with ≥80% coverage on wechat files | ✓ from Step 1 |
| 3. `make lint` passes | ✓ from Step 2 |
| 4. Manual E2E against real WeChat Open Platform test app | Requires registered WeChat test app — out of scope for CI; manual verification by developer |
| 5. `social_identities` row: `provider='wechat'`, `provider_uid='wechat_<unionid>'`, `email IS NULL` | Verified by code review of `LoginWithProfile` flow; manual E2E in step 4 confirms |
| 6. Second WeChat login on different Yunhou app → same identity row | Code review: `identityRepo.FindByProviderUID("wechat", "wechat_<unionid>")` matches existing row. Manual E2E in step 4 |
| 7. Existing GitHub login flow unchanged | Verified by `make test` + smoke test of `/auth/github/redirect` |

- [ ] **Step 6: Commit any final doc tweaks**

If you touched CLAUDE.md or other docs during verification:

```bash
git add -A
git commit -m "docs: post-verification tweaks"
```

---

## End of Plan

When all 14 tasks are complete and `make build && make test && make lint` all pass, WeChat login is shipped.

Final commit graph (one commit per task, ~14 commits total on top of `master`):

```
docs(spec): WeChat login design                  (already committed as 508b3b2)
feat(model): add WeChatOAuthConfig alongside GitHubOAuthConfig
feat(service): WeChatOAuthService with BuildAuthorizeURL + VerifyCallbackState
feat(service): WeChatOAuthService.ExchangeCode
feat(service): WeChatOAuthService.FetchWeChatProfile
refactor(handler): extract buildCallbackRedirectURL to auth_common.go
feat(handler): validateWeChatOAuthConfig
feat(handler): authErrReason maps wechat_upstream + wechat_no_unionid
feat(handler): /auth/wechat/redirect
feat(handler): /auth/wechat/callback
feat(router): wire /auth/wechat/*
feat(server): construct WeChatOAuthService + pass to router.Setup
docs(CLAUDE.md): document WeChat login boundary + endpoints
```