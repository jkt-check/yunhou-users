package handler

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
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

func validateRedirectURIs(uris []string) (string, bool) {
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Fragment != "" {
			return "redirect_uri must be a valid URL without fragment: " + u, false
		}
		if parsed.Scheme != "https" && !isLocalhost(parsed) {
			return "redirect_uri must use HTTPS (http://localhost allowed for dev): " + u, false
		}
	}
	return "", true
}

func isLocalhost(u *url.URL) bool {
	return u.Scheme == "http" && (u.Host == "localhost" || strings.HasPrefix(u.Host, "localhost:") || u.Host == "127.0.0.1" || strings.HasPrefix(u.Host, "127.0.0.1:"))
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

	if msg, ok := validateRedirectURIs(req.RedirectURIs); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": msg})
		return
	}

	if len(req.Providers) == 0 {
		req.Providers = []string{"github", "google", "wechat"}
	}
	if req.DefaultPlan == "" {
		req.DefaultPlan = "free"
	}
	if req.DefaultPlan != "free" && req.DefaultPlan != "trial" && req.DefaultPlan != "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "default_plan must be one of: free, trial, paid"})
		return
	}

	plainSecret, err := service.GenerateRefreshToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to generate app secret"})
		return
	}
	plainSecret = plainSecret[:24]
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

func getAuthedApp(c *gin.Context) (*model.App, bool) {
	authedApp, exists := c.Get(middleware.ContextApp)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "app authentication required"})
		return nil, false
	}
	app, ok := authedApp.(*model.App)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "invalid app context"})
		return nil, false
	}
	return app, true
}

func (h *AppHandler) GetApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	authed, ok := getAuthedApp(c)
	if !ok {
		return
	}
	if authed.ID != app.ID {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "not authorized to access this app"})
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

	authed, ok := getAuthedApp(c)
	if !ok {
		return
	}
	if authed.ID != app.ID {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "not authorized to modify this app"})
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

	if req.RedirectURIs != nil {
		if msg, ok := validateRedirectURIs(req.RedirectURIs); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": msg})
			return
		}
	}

	updated := *app
	if req.Name != nil {
		updated.Name = *req.Name
	}
	if req.RedirectURIs != nil {
		updated.RedirectURIs = req.RedirectURIs
	}
	if req.Providers != nil {
		updated.Providers = req.Providers
	}
	if req.DefaultPlan != nil {
		if *req.DefaultPlan != "free" && *req.DefaultPlan != "trial" && *req.DefaultPlan != "paid" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "default_plan must be one of: free, trial, paid"})
			return
		}
		updated.DefaultPlan = *req.DefaultPlan
	}

	if err := h.appRepo.Update(c.Request.Context(), &updated); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": updated})
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

	authed, ok := getAuthedApp(c)
	if !ok {
		return
	}
	if authed.ID != req.AppID {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "not authorized to create subscriptions for this app"})
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
		msg := err.Error()
		if strings.Contains(msg, "already exists") {
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": msg})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create subscription"})
		}
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
	authed, ok := getAuthedApp(c)
	if !ok {
		return
	}
	if authed.ID != sub.AppID {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "not authorized to access this subscription"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": sub})
}

func (h *AppHandler) CancelSubscription(c *gin.Context) {
	id := c.Param("id")
	sub, err := h.subRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "subscription not found"})
		return
	}
	authed, ok := getAuthedApp(c)
	if !ok {
		return
	}
	if authed.ID != sub.AppID {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "not authorized to cancel this subscription"})
		return
	}
	if err := h.subSvc.Cancel(c.Request.Context(), id); err != nil {
		msg := err.Error()
		if msg == "subscription not found" || msg == "already cancelled" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": msg})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to cancel subscription"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "cancelled"})
}
