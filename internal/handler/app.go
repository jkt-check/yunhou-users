package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

type AppHandler struct {
	appRepo repo.AppRepo
	subRepo repo.SubscriptionRepo
	subSvc  *service.SubscriptionService
}

func NewAppHandler(appRepo repo.AppRepo, subRepo repo.SubscriptionRepo, subSvc *service.SubscriptionService) *AppHandler {
	return &AppHandler{appRepo: appRepo, subRepo: subRepo, subSvc: subSvc}
}

func (h *AppHandler) CreateApp(c *gin.Context) {
	var req struct {
		Name         string   `json:"name" binding:"required"`
		RedirectURIs []string `json:"redirect_uris" binding:"required"`
		Providers    []string `json:"providers"`
		DefaultPlan  string   `json:"default_plan"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if len(req.Providers) == 0 {
		req.Providers = []string{"github", "google", "wechat"}
	}
	if req.DefaultPlan == "" {
		req.DefaultPlan = "free"
	}

	plainSecret := service.GenerateRefreshToken()[:24]
	hashedSecret, err := util.HashSecret(plainSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to generate app secret"})
		return
	}

	app := &model.App{
		ID:           service.GenerateUUID(),
		Secret:       hashedSecret,
		Name:         req.Name,
		RedirectURIs: req.RedirectURIs,
		Providers:    req.Providers,
		DefaultPlan:  req.DefaultPlan,
	}
	if err := h.appRepo.Create(c.Request.Context(), app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create app"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code": 0,
		"data": gin.H{
			"app_id":     app.ID,
			"app_secret": plainSecret,
			"name":       app.Name,
		},
	})
}

func (h *AppHandler) GetApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) UpdateApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}

	var req struct {
		Name         *string  `json:"name"`
		RedirectURIs []string `json:"redirect_uris"`
		Providers    []string `json:"providers"`
		DefaultPlan  *string  `json:"default_plan"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Name != nil {
		app.Name = *req.Name
	}
	if req.RedirectURIs != nil {
		app.RedirectURIs = req.RedirectURIs
	}
	if req.Providers != nil {
		app.Providers = req.Providers
	}
	if req.DefaultPlan != nil {
		app.DefaultPlan = *req.DefaultPlan
	}

	if err := h.appRepo.Update(c.Request.Context(), app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) CreateSubscription(c *gin.Context) {
	var req struct {
		UserID    string  `json:"user_id" binding:"required"`
		AppID     string  `json:"app_id" binding:"required"`
		Plan      string  `json:"plan" binding:"required"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid expires_at format, use RFC3339"})
			return
		}
		expiresAt = &t
	}

	sub, err := h.subSvc.Create(c.Request.Context(), req.UserID, req.AppID, req.Plan, expiresAt)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": sub})
}

func (h *AppHandler) GetSubscription(c *gin.Context) {
	id := c.Param("id")
	sub, err := h.subRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "subscription not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": sub})
}

func (h *AppHandler) CancelSubscription(c *gin.Context) {
	id := c.Param("id")
	if err := h.subSvc.Cancel(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "cancelled"})
}
