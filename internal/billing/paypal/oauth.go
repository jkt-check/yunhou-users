package paypal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

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
// via the supplied *http.Client.Timeout.
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call paypal oauth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("paypal oauth returned %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode paypal oauth: %w", err)
	}
	if body.AccessToken == "" {
		return nil, fmt.Errorf("paypal oauth returned empty access_token")
	}
	return &Token{AccessToken: body.AccessToken, ExpiresIn: body.ExpiresIn}, nil
}