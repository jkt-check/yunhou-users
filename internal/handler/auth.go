package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

type AuthHandler struct {
	authSvc   *service.AuthService
	tokenSvc  *service.TokenService
	oauth     *service.OAuthProvider
	hmacKey   string
}

func NewAuthHandler(authSvc *service.AuthService, tokenSvc *service.TokenService, oauth *service.OAuthProvider, hmacKey string) *AuthHandler {
	return &AuthHandler{authSvc: authSvc, tokenSvc: tokenSvc, oauth: oauth, hmacKey: hmacKey}
}

type statePayload struct {
	AppID       string `json:"a"`
	RedirectURI string `json:"r"`
	State       string `json:"s"`
}

func (h *AuthHandler) encodeState(appID, redirectURI, state string) (string, error) {
	p := statePayload{AppID: appID, RedirectURI: redirectURI, State: state}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode state: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(h.hmacKey))
	mac.Write(b)
	sig := mac.Sum(nil)
	payloadEnc := base64.RawURLEncoding.EncodeToString(b)
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return payloadEnc + "." + sigEnc, nil
}

func (h *AuthHandler) decodeState(encoded string) (*statePayload, error) {
	parts := splitState(encoded)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid state format")
	}
	payloadB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid state payload: %w", err)
	}
	sigB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid state signature: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(h.hmacKey))
	mac.Write(payloadB)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sigB, expectedSig) {
		return nil, fmt.Errorf("invalid state signature")
	}
	var p statePayload
	if err := json.Unmarshal(payloadB, &p); err != nil {
		return nil, fmt.Errorf("invalid state payload: %w", err)
	}
	return &p, nil
}

func splitState(encoded string) []string {
	for i, c := range encoded {
		if c == '.' {
			return []string{encoded[:i], encoded[i+1:]}
		}
	}
	return nil
}

func (h *AuthHandler) Authorize(c *gin.Context) {
	appID := c.Query("app_id")
	provider := c.Query("provider")
	redirectURI := c.Query("redirect_uri")
	state := c.Query("state")

	if appID == "" || provider == "" || redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing required parameters: app_id, provider, redirect_uri"})
		return
	}

	// Validate redirect_uri against app's registered URIs
	app, err := h.oauth.FindApp(c.Request.Context(), appID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid app_id"})
		return
	}
	if !containsURI(app.RedirectURIs, redirectURI) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "redirect_uri not registered"})
		return
	}
	if !containsString(app.Providers, provider) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "provider not allowed for this app"})
		return
	}

	oauthState, err := h.encodeState(appID, redirectURI, state)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to encode state"})
		return
	}
	oauthURL, err := h.oauth.BuildAuthorizeURL(provider, appID, redirectURI, oauthState)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to build authorization URL"})
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, oauthURL)
}

func (h *AuthHandler) Callback(c *gin.Context) {
	provider := c.Param("provider")
	code := c.Query("code")
	rawState := c.Query("state")

	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing authorization code"})
		return
	}

	p, err := h.decodeState(rawState)
	if err != nil || p.AppID == "" || p.RedirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state parameter"})
		return
	}

	// Re-validate redirect_uri against app's registered URIs to prevent open redirect
	app, err := h.oauth.FindApp(c.Request.Context(), p.AppID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid app_id"})
		return
	}
	if !containsURI(app.RedirectURIs, p.RedirectURI) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "redirect_uri not registered"})
		return
	}

	userInfo, err := h.oauth.FetchUser(c.Request.Context(), provider, code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to authenticate with provider"})
		return
	}

	authCode, err := h.authSvc.AuthorizeOrCreate(c.Request.Context(), userInfo, p.AppID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal server error"})
		return
	}

	redirectURL := p.RedirectURI + "?code=" + url.QueryEscape(authCode) + "&state=" + url.QueryEscape(p.State)
	c.Redirect(http.StatusTemporaryRedirect, redirectURL)
}

func (h *AuthHandler) ExchangeToken(c *gin.Context) {
	var req struct {
		Code      string `json:"code" binding:"required"`
		AppID     string `json:"app_id" binding:"required"`
		AppSecret  string `json:"app_secret" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	accessToken, refreshToken, err := h.authSvc.ExchangeCode(c.Request.Context(), req.Code, req.AppID, req.AppSecret)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid credentials or authorization code"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"token_type":    "Bearer",
		},
	})
}

func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
		AppID        string `json:"app_id" binding:"required"`
		AppSecret    string `json:"app_secret" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	app, err := h.oauth.FindApp(c.Request.Context(), req.AppID)
	if err != nil {
		util.CheckSecret(util.DummyBcryptHash, req.AppSecret)
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid app credentials"})
		return
	}
	if !util.CheckSecret(app.Secret, req.AppSecret) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid app credentials"})
		return
	}

	accessToken, refreshToken, err := h.tokenSvc.Refresh(c.Request.Context(), req.RefreshToken, req.AppID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid or expired refresh token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"token_type":    "Bearer",
		},
	})
}

func (h *AuthHandler) JWKS(c *gin.Context) {
	c.JSON(http.StatusOK, h.tokenSvc.JWKS())
}

func containsURI(registered []string, candidate string) bool {
	for _, u := range registered {
		if u == candidate {
			return true
		}
	}
	return false
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
