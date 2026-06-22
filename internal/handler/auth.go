package handler

import (
	"errors"
	"log"
	"net/http"

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

func (h *AuthHandler) Login(c *gin.Context) {
	var req service.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	resp, err := h.authSvc.Login(c.Request.Context(), req)
	if err != nil {
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
