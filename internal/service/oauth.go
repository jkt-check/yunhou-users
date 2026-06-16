package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

type OAuthProvider struct {
	cfg     *config.Config
	AppRepo repo.AppRepo
	Client  *http.Client // defaults to http.DefaultClient if nil
}

func NewOAuthProvider(cfg *config.Config, appRepo repo.AppRepo) *OAuthProvider {
	return &OAuthProvider{cfg: cfg, AppRepo: appRepo}
}

func (p *OAuthProvider) FindApp(ctx context.Context, appID string) (*model.App, error) {
	if p.AppRepo == nil {
		return nil, fmt.Errorf("app repository not configured")
	}
	return p.AppRepo.FindByID(ctx, appID)
}

func (p *OAuthProvider) httpClient() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

type githubTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error,omitempty"`
}

type githubUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

type githubEmailResponse struct {
	Email   string `json:"email"`
	Primary bool   `json:"primary"`
}

func (p *OAuthProvider) BuildAuthorizeURL(provider, appID, redirectURI, state string) (string, error) {
	switch provider {
	case "github":
		return p.githubAuthorizeURL(appID, redirectURI, state), nil
	default:
		return "", fmt.Errorf("unsupported provider: %s", provider)
	}
}

func (p *OAuthProvider) FetchUser(ctx context.Context, provider, code string) (ProviderUserInfo, error) {
	switch provider {
	case "github":
		return p.fetchGitHubUser(ctx, code)
	default:
		return ProviderUserInfo{}, fmt.Errorf("unsupported provider: %s", provider)
	}
}

func (p *OAuthProvider) githubAuthorizeURL(appID, redirectURI, state string) string {
	callbackURL := p.cfg.GitHubCallbackURL
	if callbackURL == "" {
		callbackURL = "http://localhost:" + p.cfg.Port + "/callback/github"
	}
	q := url.Values{
		"client_id":    {p.cfg.GitHubClientID},
		"redirect_uri": {callbackURL},
		"scope":        {"read:user,user:email"},
		"state":        {state},
	}
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

func (p *OAuthProvider) fetchGitHubUser(ctx context.Context, code string) (ProviderUserInfo, error) {
	token, err := p.exchangeGitHubCode(ctx, code)
	if err != nil {
		return ProviderUserInfo{}, fmt.Errorf("exchange github code: %w", err)
	}

	userResp, err := p.getGitHubUser(ctx, token)
	if err != nil {
		return ProviderUserInfo{}, fmt.Errorf("get github user: %w", err)
	}

	email := userResp.Email
	if email == "" {
		email, _ = p.getGitHubPrimaryEmail(ctx, token)
	}

	return ProviderUserInfo{
		Provider:    "github",
		ProviderUID: fmt.Sprintf("%d", userResp.ID),
		Email:       email,
		Nickname:    userResp.Login,
		AvatarURL:   userResp.AvatarURL,
	}, nil
}

func (p *OAuthProvider) exchangeGitHubCode(ctx context.Context, code string) (string, error) {
	data := url.Values{
		"client_id":     {p.cfg.GitHubClientID},
		"client_secret": {p.cfg.GitHubClientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var tokenResp githubTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("invalid token response: %s", string(body))
	}
	if tokenResp.Error != "" {
		return "", fmt.Errorf("github token error: %s", tokenResp.Error)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("no access token in response")
	}
	return tokenResp.AccessToken, nil
}

func (p *OAuthProvider) getGitHubUser(ctx context.Context, token string) (*githubUserResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var user githubUserResponse
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("invalid user response: %s", string(body))
	}
	return &user, nil
}

func (p *OAuthProvider) getGitHubPrimaryEmail(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var emails []githubEmailResponse
	if err := json.Unmarshal(body, &emails); err != nil {
		return "", fmt.Errorf("invalid email response: %s", string(body))
	}

	for _, e := range emails {
		if e.Primary {
			return e.Email, nil
		}
	}
	if len(emails) > 0 {
		return emails[0].Email, nil
	}
	return "", fmt.Errorf("no email found")
}
