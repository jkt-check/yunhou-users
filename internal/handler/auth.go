package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
)

type AuthHandler struct {
	authSvc  *service.AuthService
	tokenSvc *service.TokenService
	oauth    *service.OAuthProvider
}

func NewAuthHandler(authSvc *service.AuthService, tokenSvc *service.TokenService, oauth *service.OAuthProvider) *AuthHandler {
	return &AuthHandler{authSvc: authSvc, tokenSvc: tokenSvc, oauth: oauth}
}

// statePayload is encoded into the OAuth state parameter to survive the redirect.
type statePayload struct {
	AppID      string `json:"a"`
	RedirectURI string `json:"r"`
	State      string `json:"s"`
}

func encodeState(appID, redirectURI, state string) string {
	p := statePayload{AppID: appID, RedirectURI: redirectURI, State: state}
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeState(encoded string) (*statePayload, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var p statePayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
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

	// Encode app_id + redirect_uri + original state into the OAuth state parameter
	oauthState := encodeState(appID, redirectURI, state)
	oauthURL, err := h.oauth.BuildAuthorizeURL(provider, appID, redirectURI, oauthState)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
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

	p, err := decodeState(rawState)
	if err != nil || p.AppID == "" || p.RedirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid state parameter"})
		return
	}

	userInfo, err := h.oauth.FetchUser(c.Request.Context(), provider, code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to authenticate with provider"})
		return
	}

	authCode, err := h.authSvc.AuthorizeOrCreate(c.Request.Context(), userInfo, p.AppID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}

	redirectURI := p.RedirectURI
	c.Redirect(http.StatusTemporaryRedirect, redirectURI+"?code="+authCode+"&state="+p.State)
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
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
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
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	accessToken, refreshToken, err := h.tokenSvc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
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
