package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

type UserRepoInterface interface {
	FindByID(ctx context.Context, id string) (*model.User, error)
	Update(ctx context.Context, u *model.User) error
}

type IdentityRepoInterface interface {
	ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error)
	DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error)
}

type UserHandler struct {
	userRepo     UserRepoInterface
	identityRepo IdentityRepoInterface
}

func NewUserHandler(userRepo UserRepoInterface, identityRepo IdentityRepoInterface) *UserHandler {
	return &UserHandler{userRepo: userRepo, identityRepo: identityRepo}
}

func (h *UserHandler) GetProfile(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	user, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "user not found"})
		return
	}
	if err != nil {
		log.Printf("get profile error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load profile"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": user})
}

func (h *UserHandler) UpdateProfile(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	user, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "user not found"})
		return
	}
	if err != nil {
		log.Printf("update profile lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load profile"})
		return
	}

	var req struct {
		Nickname  *string `json:"nickname"`
		AvatarURL *string `json:"avatar_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	updated := *user
	if req.Nickname != nil {
		trimmed := strings.TrimSpace(*req.Nickname)
		if trimmed == "" || len(trimmed) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "nickname must be 1-100 characters"})
			return
		}
		updated.Nickname = &trimmed
	}
	if req.AvatarURL != nil {
		u, err := url.Parse(*req.AvatarURL)
		// Reject anything that isn't a clean HTTPS URL. userinfo (`https://x:y@evil.com`)
		// would otherwise sneak a different host into the displayed link.
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Fragment != "" || u.User != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "avatar_url must be a valid HTTPS URL without userinfo or fragment"})
			return
		}
		normalized := u.String()
		updated.AvatarURL = &normalized
	}

	if err := h.userRepo.Update(c.Request.Context(), &updated); err != nil {
		log.Printf("update profile error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update profile"})
		return
	}

	now := time.Now()
	updated.UpdatedAt = now
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": updated})
}

func (h *UserHandler) ListIdentities(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	list, err := h.identityRepo.ListByUserID(c.Request.Context(), userID)
	if err != nil {
		log.Printf("list identities error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list identities"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

func (h *UserHandler) UnbindIdentity(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	identityID := c.Param("id")

	deleted, err := h.identityRepo.DeleteIfNotLast(c.Request.Context(), identityID, userID)
	if err != nil {
		log.Printf("unbind identity error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to unbind identity"})
		return
	}
	if !deleted {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "must keep at least one social account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "unbound"})
}
