package handler

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
)

type AuthHandler struct {
	authSvc  service.AuthServiceInterface
	tokenSvc service.TokenServiceInterface
}

func NewAuthHandler(authSvc service.AuthServiceInterface, tokenSvc service.TokenServiceInterface) *AuthHandler {
	return &AuthHandler{authSvc: authSvc, tokenSvc: tokenSvc}
}

// expectedAuthErrors lists the service-level sentinel errors that map to a
// user-facing 401. Anything else is treated as internal and surfaced only as
// a generic 500 (the underlying detail goes to the log, not the client).
var expectedAuthErrors = []error{
	service.ErrInvalidProviderToken,
	service.ErrUnsupportedProvider,
	service.ErrInvalidRefreshToken,
	service.ErrUserNotFound,
	service.ErrUserSuspended,
	service.ErrUserDeleted,
	service.ErrSubscriptionNotActive,
	service.ErrSubscriptionExpired,
	service.ErrAppNotFound,
	service.ErrAppInactive,
}

func isExpectedAuthErr(err error) bool {
	for _, target := range expectedAuthErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// authErrReason maps the service-layer auth sentinels to a short token
// safe to put in a URL fragment. Used by the GitHub OAuth callback to
// tell the BFF which class of failure occurred without stranding the
// browser on a JSON error page. Returns "auth_failed" for anything we
// don't recognise.
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
	default:
		return "auth_failed"
	}
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req service.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	// GitHub login must use the OAuth redirect flow (/auth/github/redirect
	// → /auth/github/callback) — the boundary contract is that Yunhou
	// holds the OAuth App's client_secret and runs the code exchange
	// server-side. Accepting a BFF-supplied access_token here would let
	// the BFF bypass that contract. Google keeps the direct-token path
	// for backward compatibility; that path will be deprecated in a
	// follow-up so it lines up with the GitHub boundary.
	if req.Provider == "github" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "github login requires /auth/github/redirect + /auth/github/callback; direct provider_token is not accepted",
		})
		return
	}

	resp, err := h.authSvc.Login(c.Request.Context(), req)
	if err != nil {
		// ErrUnsupportedProvider is a malformed request (caller sent an
		// unknown provider name) — surface as 400, not 401.
		if errors.Is(err, service.ErrUnsupportedProvider) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		if isExpectedAuthErr(err) {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
			return
		}
		log.Printf("login internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "login failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}

func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
		AppID       string `json:"app_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	resp, err := h.authSvc.RefreshToken(c.Request.Context(), req.RefreshToken, req.AppID)
	if err != nil {
		if isExpectedAuthErr(err) {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
			return
		}
		log.Printf("refresh internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "refresh failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}

func (h *AuthHandler) Logout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if err := h.authSvc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		log.Printf("logout internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "logout failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "logged out"})
}

func (h *AuthHandler) JWKS(c *gin.Context) {
	c.JSON(http.StatusOK, h.tokenSvc.JWKS())
}

// TestLogin is a dev-only login endpoint that returns a real JWT
// without going through any OAuth provider. It exists so the L3
// PayPal integration suite (tests/e2e-ui) can issue authenticated
// requests against a locally-running backend whose GitHub OAuth
// verifier is wired to the real api.github.com (which 401s on
// forged tokens).
//
// Gated by PAYPAL_L3_E2E_MODE=1: if unset, every call returns 404.
// This keeps the endpoint off in production. The route is also
// never registered in router.Setup when the env is missing.
//
// POST /test/login  body: {"email":"...","app_id":"yundian"}
// 200:        {access_token, refresh_token, user: {id, ...}}
//
// Side-effects: if the email has no matching user, creates one with
// the default 'free' plan (so the L3 tests have a user + sub to
// operate on). Refresh token row inserted via the same path as
// production /auth/login.
func (h *AuthHandler) TestLogin(c *gin.Context) {
	if os.Getenv("PAYPAL_L3_E2E_MODE") != "1" {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
		return
	}

	var req struct {
		Email string `json:"email" binding:"required"`
		AppID string `json:"app_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	resp, err := h.authSvc.TestLogin(c.Request.Context(), service.TestLoginRequest{
		Email: req.Email,
		AppID: req.AppID,
	})
	if err != nil {
		log.Printf("test login internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "test login failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}
