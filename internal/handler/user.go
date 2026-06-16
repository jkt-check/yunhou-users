package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
)

type UserHandler struct {
	userRepo    repo.UserRepo
	identityRepo repo.SocialIdentityRepo
	subRepo     repo.SubscriptionRepo
}

func NewUserHandler(userRepo repo.UserRepo, identityRepo repo.SocialIdentityRepo, subRepo repo.SubscriptionRepo) *UserHandler {
	return &UserHandler{userRepo: userRepo, identityRepo: identityRepo, subRepo: subRepo}
}

func (h *UserHandler) GetProfile(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	user, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": user})
}

func (h *UserHandler) UpdateProfile(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	user, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "user not found"})
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

	if req.Nickname != nil {
		user.Nickname = req.Nickname
	}
	if req.AvatarURL != nil {
		user.AvatarURL = req.AvatarURL
	}

	if err := h.userRepo.Update(c.Request.Context(), user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update profile"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": user})
}

func (h *UserHandler) ListIdentities(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	list, err := h.identityRepo.ListByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list identities"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

func (h *UserHandler) UnbindIdentity(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	identityID := c.Param("id")

	count, err := h.identityRepo.CountByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to check identities"})
		return
	}
	if count <= 1 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "must keep at least one social account"})
		return
	}

	if err := h.identityRepo.Delete(c.Request.Context(), identityID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to unbind identity"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "unbound"})
}

func (h *UserHandler) ListApps(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	list, err := h.subRepo.ListByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list apps"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}
