package paypal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// paypalOAuthMaxResponseBytes caps the OAuth token endpoint body. PayPal's
// successful token response is <1KB; a misbehaving (or compromised)
// upstream returning multi-GB would otherwise stream to EOF here, holding
// memory + connection until Go's GC catches up.
const paypalOAuthMaxResponseBytes = 64 * 1024

// paypalOAuthTimeout is the default per-call timeout used when the
// supplied *http.Client has no Timeout set. Production wires a
// timeout-configured client at startup; this fallback keeps test
// callers and unconfigured deployments from hanging.
const paypalOAuthTimeout = 5 * time.Second

// OAuthClient fetches PayPal OAuth access tokens via the client_credentials
// grant. One instance can be shared across goroutines; it is stateless
// beyond the supplied *http.Client.
type OAuthClient struct {
	httpClient *http.Client
	tokenURL   string
}

func NewOAuthClient(httpClient *http.Client, baseURL string) *OAuthClient {
	return &OAuthClient{
		httpClient: httpClient,
		tokenURL:   strings.TrimRight(baseURL, "/") + "/v1/oauth2/token",
	}
}

// FetchToken exchanges client_id/client_secret for an access token. Times out
// via the supplied *http.Client.Timeout (or paypalOAuthTimeout if the
// caller forgot to set one).
func (c *OAuthClient) FetchToken(ctx context.Context, clientID, clientSecret string) (*Token, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en_US")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cred := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req.Header.Set("Authorization", "Basic "+cred)

	// Use a per-call timeout that respects the supplied client's Timeout
	// when set, otherwise falls back to paypalOAuthTimeout so an
	// unconfigured client doesn't hit a hanging upstream.
	callCtx := ctx
	if c.httpClient.Timeout == 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, paypalOAuthTimeout)
		defer cancel()
	}
	req = req.WithContext(callCtx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call paypal oauth: %w", err)
	}
	defer resp.Body.Close()
	// Cap the body — a successful token response is <1KB. Without the
	// cap a misbehaving upstream returning GB of JSON would stream
	// until EOF, exhausting memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, paypalOAuthMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read paypal oauth: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Surface PayPal's error body in the returned error so operators
		// see "invalid_client" / "invalid_request" without having to
		// capture a separate log. The body is JSON-encoded, already
		// capped at paypalOAuthMaxResponseBytes above.
		return nil, fmt.Errorf("paypal oauth returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode paypal oauth: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("paypal oauth returned empty access_token")
	}
	return &Token{AccessToken: parsed.AccessToken, ExpiresIn: parsed.ExpiresIn}, nil
}