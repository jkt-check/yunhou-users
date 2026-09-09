// Package connector implements the OAuth connector client for upstream
// account onboarding and lifecycle (设计 §8: 连接器接口覆盖授权开始/回调、
// 刷新、可用模型发现、限额读取和健康状态).
//
// 选型记录见 docs/runbooks/kaya-coding-plan-connector-evaluation.md：
// Sub2API/CLIProxyAPI 仅作协议与账号接入参考——本产品只保留 Yunhou 一个
// 客户权益/扣费权威，连接器只做"供应商侧 OAuth 授权与账号池信号"，
// 不同步任何第二套客户余额。
//
// The client is vendor-neutral: each vendor is described by a Spec from the
// deployment registry (INFERENCE_OAUTH_CONNECTORS_JSON) so onboarding a new
// OAuth vendor is configuration, not code. All endpoints are validated by
// the egress (SSRF) policy at config load and per call.
package connector

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
)

// Spec describes one OAuth vendor connector (one entry of the deployment
// registry). Endpoints are absolute https URLs; ClientSecret is deployment
// secret material and never crosses the API boundary.
type Spec struct {
	// Key is the registry id referenced by oauth grants ('kimi', 'codex'…).
	Key string `json:"key"`
	// AuthorizeURL / TokenURL are the RFC 6749 endpoints (required).
	AuthorizeURL string `json:"authorize_url"`
	TokenURL     string `json:"token_url"`
	// RevokeURL is the RFC 7009 revocation endpoint (optional: empty means
	// revocation is local-only — credential revoked + accounts disabled).
	RevokeURL string `json:"revoke_url,omitempty"`
	// ModelsURL is the vendor's model discovery endpoint (optional; empty =
	// discovery unsupported, the catalog stays operator-managed).
	ModelsURL string `json:"models_url,omitempty"`
	// QuotaURL is the vendor's account quota endpoint (optional; empty or
	// unparsable = upstream quota stays UNKNOWN — never estimated from
	// customer balances, 设计 §8).
	QuotaURL string `json:"quota_url,omitempty"`
	// HealthURL overrides the health probe target (optional; defaults to
	// ModelsURL, then TokenURL as a reachability check).
	HealthURL string `json:"health_url,omitempty"`

	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// RedirectURL is this product's registered callback.
	RedirectURL string `json:"redirect_url"`
	// PKCE, default true: S256 challenge on authorize, verifier on exchange.
	// A vendor that cannot do PKCE must set this false explicitly.
	PKCE *bool `json:"pkce,omitempty"`
}

// SupportsPKCE reports whether the authorize/exchange pair uses S256.
func (s Spec) SupportsPKCE() bool { return s.PKCE == nil || *s.PKCE }

// Validate rejects an unusable spec at config load (fail fast, no secret
// material in the error strings beyond presence/absence).
func (s Spec) Validate() error {
	if s.Key == "" {
		return errors.New("connector spec: key is required")
	}
	for _, u := range []struct{ name, v string }{
		{"authorize_url", s.AuthorizeURL}, {"token_url", s.TokenURL},
	} {
		if u.v == "" {
			return fmt.Errorf("connector %q: %s is required", s.Key, u.name)
		}
		if !strings.HasPrefix(u.v, "https://") && !strings.HasPrefix(u.v, "http://") {
			return fmt.Errorf("connector %q: %s must be an absolute http(s) URL", s.Key, u.name)
		}
	}
	if s.ClientID == "" {
		return fmt.Errorf("connector %q: client_id is required", s.Key)
	}
	if s.RedirectURL == "" {
		return fmt.Errorf("connector %q: redirect_url is required", s.Key)
	}
	return nil
}

// Registry is the configured vendor set keyed by Spec.Key.
type Registry map[string]Spec

// ParseRegistry decodes the INFERENCE_OAUTH_CONNECTORS_JSON deployment
// config: a JSON array of Spec. Duplicate keys and invalid specs are fatal
// at config load.
func ParseRegistry(raw string) (Registry, error) {
	if strings.TrimSpace(raw) == "" {
		return Registry{}, nil
	}
	var specs []Spec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, fmt.Errorf("oauth connectors registry: invalid JSON: %w", err)
	}
	reg := Registry{}
	for _, s := range specs {
		if err := s.Validate(); err != nil {
			return nil, err
		}
		if _, dup := reg[s.Key]; dup {
			return nil, fmt.Errorf("oauth connectors registry: duplicate key %q", s.Key)
		}
		reg[s.Key] = s
	}
	return reg, nil
}

// TokenSet is one OAuth token endpoint response.
type TokenSet struct {
	AccessToken  string
	RefreshToken string // may be empty when the vendor does not rotate it
	TokenType    string
	// Expiry is the absolute access-token expiry (zero = unknown).
	Expiry time.Time
	// ExternalAccountID is the vendor-side account id when the token
	// response (or its id_token) carries one; empty stays empty.
	ExternalAccountID string
}

// ErrorKind classifies a connector failure for the refresh/health paths.
type ErrorKind string

const (
	// KindReauthRequired: the vendor definitively rejected the grant
	// (invalid_grant / 401 / 403) — the account must stop scheduling until
	// an operator re-authorizes (设计 §8 reauth_required).
	KindReauthRequired ErrorKind = "reauth_required"
	// KindRetryable: 429/5xx/transport — retry with backoff, account state
	// unchanged by the refresh path.
	KindRetryable ErrorKind = "retryable"
	// KindInvalid: the vendor answered but the payload is unusable.
	KindInvalid ErrorKind = "invalid_response"
)

// Error is a classified connector failure.
type Error struct {
	Kind       ErrorKind
	StatusCode int
	Msg        string
	Err        error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("connector %s (status %d): %s", e.Kind, e.StatusCode, e.Msg)
	}
	return fmt.Sprintf("connector %s: %s", e.Kind, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf extracts the ErrorKind (unknown errors are retryable — a transport
// failure must never flip an account to reauth_required).
func KindOf(err error) ErrorKind {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return KindRetryable
}

// HTTPDoer is the connector's outbound client (production: the
// egress-checked providers.NewHTTPClient; tests: httptest's client).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client talks to vendor OAuth endpoints. It holds no secrets beyond the
// per-call arguments; token material stays inside the credentials boundary.
type Client struct {
	HTTP HTTPDoer
}

// bodyCap bounds vendor response bodies kept in memory.
const bodyCap = 1 << 20

// BuildAuthorizeURL renders the authorization redirect (授权开始). state and
// challenge come from the credentials boundary; the URL never contains the
// PKCE verifier.
func (c *Client) BuildAuthorizeURL(spec Spec, state, challenge string) (string, error) {
	u, err := url.Parse(spec.AuthorizeURL)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Msg: "authorize url unparsable", Err: err}
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", spec.ClientID)
	q.Set("redirect_uri", spec.RedirectURL)
	q.Set("state", state)
	if len(spec.Scopes) > 0 {
		q.Set("scope", strings.Join(spec.Scopes, " "))
	}
	if spec.SupportsPKCE() && challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode trades the one-time authorization code for tokens (回调).
func (c *Client) ExchangeCode(ctx context.Context, spec Spec, code, verifier string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {spec.RedirectURL},
	}
	if spec.SupportsPKCE() && verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return c.tokenCall(ctx, spec, form)
}

// RefreshToken rotates an access token (刷新). The vendor may rotate the
// refresh token itself — the returned TokenSet carries whatever the vendor
// returned, and the caller persists it whole (旧 token 被新值整体替换).
func (c *Client) RefreshToken(ctx context.Context, spec Spec, refreshToken string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return c.tokenCall(ctx, spec, form)
}

func (c *Client) tokenCall(ctx context.Context, spec Spec, form url.Values) (*TokenSet, error) {
	if spec.ClientSecret != "" {
		form.Set("client_id", spec.ClientID)
		form.Set("client_secret", spec.ClientSecret)
	} else {
		// Public-client (PKCE-only) vendors authenticate by client_id alone.
		form.Set("client_id", spec.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Msg: "build token request", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, &Error{Kind: KindRetryable, Msg: "token endpoint unreachable", Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))

	if resp.StatusCode != http.StatusOK {
		kind := KindRetryable
		// RFC 6749 §5.2: invalid_grant = the grant is dead (expired/revoked)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			kind = KindReauthRequired
		} else if resp.StatusCode == http.StatusBadRequest {
			var oe struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &oe) == nil && oe.Error == "invalid_grant" {
				kind = KindReauthRequired
			}
		}
		return nil, &Error{Kind: kind, StatusCode: resp.StatusCode, Msg: truncate(string(body), 256)}
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		AccountID    string `json:"account_id"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, &Error{Kind: KindInvalid, Msg: "token response unparsable", Err: err}
	}
	if tok.AccessToken == "" {
		return nil, &Error{Kind: KindInvalid, Msg: "token response missing access_token"}
	}
	ts := &TokenSet{
		AccessToken:       tok.AccessToken,
		RefreshToken:      tok.RefreshToken,
		TokenType:         tok.TokenType,
		ExternalAccountID: tok.AccountID,
	}
	if tok.ExpiresIn > 0 {
		ts.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return ts, nil
}

// Revoke asks the vendor to drop the grant (RFC 7009). A vendor without a
// revocation endpoint returns nil — local revocation (credential revoked +
// accounts disabled) is the authoritative part. Vendor-side errors are
// logged by the caller; they never block the local revocation.
func (c *Client) Revoke(ctx context.Context, spec Spec, token string) error {
	if spec.RevokeURL == "" || token == "" {
		return nil
	}
	form := url.Values{"token": {token}}
	if spec.ClientSecret != "" {
		form.Set("client_id", spec.ClientID)
		form.Set("client_secret", spec.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.RevokeURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return &Error{Kind: KindInvalid, Msg: "build revoke request", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return &Error{Kind: KindRetryable, Msg: "revoke endpoint unreachable", Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, bodyCap))
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return &Error{Kind: KindRetryable, StatusCode: resp.StatusCode, Msg: "vendor revoke rejected"}
	}
	return nil
}

// Models lists the vendor's available model ids (可用模型发现). A vendor
// without a discovery endpoint reports CodeUnimplemented-style unsupported:
// the catalog stays operator-managed (design §8: 技术接入与商业可售独立配置).
func (c *Client) Models(ctx context.Context, spec Spec, accessToken string) ([]string, error) {
	if spec.ModelsURL == "" {
		return nil, &Error{Kind: KindInvalid, Msg: "connector has no models endpoint"}
	}
	body, err := c.get(ctx, spec.ModelsURL, accessToken)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		// Gemini-shaped discovery responses carry "models" instead.
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &Error{Kind: KindInvalid, Msg: "models response unparsable", Err: err}
	}
	out := make([]string, 0, len(payload.Data)+len(payload.Models))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	for _, m := range payload.Models {
		if m.Name != "" {
			out = append(out, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	return out, nil
}

// QuotaSnapshot is one observed view of the upstream account's own quota.
// Any field may be nil: unknown stays unknown (设计 §8: 上游额度缓存包含
// observed_at/source/reset_at，未知保持未知；不得由客户余额反推).
type QuotaSnapshot struct {
	LimitMicros     *int64
	RemainingMicros *int64
	ResetAt         *time.Time
	ObservedAt      time.Time
}

// Quota reads the vendor's account quota endpoint (限额读取). Vendors
// without one — or with an unparsable payload — yield (nil, nil): the
// account's quota stays UNKNOWN rather than fabricated.
func (c *Client) Quota(ctx context.Context, spec Spec, accessToken string, now time.Time) (*QuotaSnapshot, error) {
	if spec.QuotaURL == "" {
		return nil, nil
	}
	body, err := c.get(ctx, spec.QuotaURL, accessToken)
	if err != nil {
		return nil, err
	}
	var payload struct {
		LimitMicros     *int64  `json:"limit_micros"`
		RemainingMicros *int64  `json:"remaining_micros"`
		ResetAt         *string `json:"reset_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Unparsable shape: not an error — the vendor's quota surface is
		// unknown to us, record nothing.
		return nil, nil
	}
	snap := &QuotaSnapshot{
		LimitMicros:     payload.LimitMicros,
		RemainingMicros: payload.RemainingMicros,
		ObservedAt:      now.UTC(),
	}
	if payload.ResetAt != nil {
		if t, perr := time.Parse(time.RFC3339, *payload.ResetAt); perr == nil {
			snap.ResetAt = &t
		}
	}
	return snap, nil
}

// Health probes the vendor with the account's access token (健康状态).
// 401/403 classify as KindReauthRequired; everything else retryable.
func (c *Client) Health(ctx context.Context, spec Spec, accessToken string) error {
	target := spec.HealthURL
	if target == "" {
		target = spec.ModelsURL
	}
	if target == "" {
		target = spec.TokenURL
	}
	_, err := c.get(ctx, target, accessToken)
	return err
}

func (c *Client) get(ctx context.Context, url, accessToken string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Msg: "build get request", Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, &Error{Kind: KindRetryable, Msg: "endpoint unreachable", Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &Error{Kind: KindReauthRequired, StatusCode: resp.StatusCode, Msg: truncate(string(body), 256)}
	default:
		return nil, &Error{Kind: KindRetryable, StatusCode: resp.StatusCode, Msg: truncate(string(body), 256)}
	}
}

func (c *Client) http() HTTPDoer {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
