package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

type AppRepoInterface interface {
	List(context.Context) ([]model.App, error)
	FindByID(context.Context, string) (*model.App, error)
	Create(context.Context, *model.App) error
	Update(context.Context, *model.App) error
}

type AppHandler struct {
	appRepo AppRepoInterface
}

func NewAppHandler(appRepo AppRepoInterface) *AppHandler {
	return &AppHandler{appRepo: appRepo}
}

func (h *AppHandler) ListApps(c *gin.Context) {
	apps, err := h.appRepo.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list apps"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": apps})
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

func (h *AppHandler) CreateApp(c *gin.Context) {
	var req struct {
		AppID       string `json:"app_id" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	app := &model.App{
		AppID:       req.AppID,
		Name:        req.Name,
		Description: req.Description,
		IsActive:    true,
	}
	if err := h.appRepo.Create(c.Request.Context(), app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create app"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) UpdateApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		IsActive    *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Name != nil {
		app.Name = *req.Name
	}
	if req.Description != nil {
		app.Description = *req.Description
	}
	if req.IsActive != nil {
		app.IsActive = *req.IsActive
	}

	if err := h.appRepo.Update(c.Request.Context(), app); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

type SubscriptionHandler struct {
	subSvc service.SubscriptionServiceInterface
}

func NewSubscriptionHandler(subSvc service.SubscriptionServiceInterface) *SubscriptionHandler {
	return &SubscriptionHandler{subSvc: subSvc}
}

func (h *SubscriptionHandler) ListUserSubscriptions(c *gin.Context) {
	userID := c.GetString("user_id")
	subs, err := h.subSvc.ListUserSubscriptions(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list subscriptions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": subs})
}

func (h *SubscriptionHandler) CreateSubscription(c *gin.Context) {
	var req struct {
		PlanID    string  `json:"plan_id" binding:"required"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	userID := c.GetString("user_id")

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid expires_at format, use RFC3339"})
			return
		}
		expiresAt = &t
	}

	sub, err := h.subSvc.Create(c.Request.Context(), userID, req.PlanID, expiresAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": sub})
}

func (h *SubscriptionHandler) CancelSubscription(c *gin.Context) {
	id := c.Param("id")

	if err := h.subSvc.Cancel(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "cancelled"})
}

type PlanHandler struct {
	planSvc service.PlanServiceInterface
}

func NewPlanHandler(planSvc service.PlanServiceInterface) *PlanHandler {
	return &PlanHandler{planSvc: planSvc}
}

func (h *PlanHandler) ListPlans(c *gin.Context) {
	plans, err := h.planSvc.ListPlans(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list plans"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plans})
}

func (h *PlanHandler) GetPlan(c *gin.Context) {
	id := c.Param("id")
	plan, err := h.planSvc.GetPlan(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) CreatePlan(c *gin.Context) {
	var req struct {
		ID           string   `json:"id" binding:"required"`
		Name         string   `json:"name" binding:"required"`
		Price        float64  `json:"price"`
		IntervalDays int      `json:"interval_days"`
		Apps         []string `json:"apps"`
		IsDefault    bool     `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	plan := &model.Plan{
		ID:           req.ID,
		Name:         req.Name,
		Price:        req.Price,
		IntervalDays: req.IntervalDays,
		Apps:         req.Apps,
		IsActive:     true,
		IsDefault:    req.IsDefault,
	}
	if err := h.planSvc.CreatePlan(c.Request.Context(), plan); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create plan"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) UpdatePlan(c *gin.Context) {
	id := c.Param("id")
	plan, err := h.planSvc.GetPlan(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}

	var req struct {
		Name         *string  `json:"name"`
		Price        *float64 `json:"price"`
		IntervalDays *int     `json:"interval_days"`
		Apps         []string `json:"apps"`
		IsActive     *bool    `json:"is_active"`
		IsDefault    *bool    `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Name != nil {
		plan.Name = *req.Name
	}
	if req.Price != nil {
		plan.Price = *req.Price
	}
	if req.IntervalDays != nil {
		plan.IntervalDays = *req.IntervalDays
	}
	if req.Apps != nil {
		plan.Apps = req.Apps
	}
	if req.IsActive != nil {
		plan.IsActive = *req.IsActive
	}
	if req.IsDefault != nil {
		plan.IsDefault = *req.IsDefault
	}

	if err := h.planSvc.UpdatePlan(c.Request.Context(), plan); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update plan"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) DeletePlan(c *gin.Context) {
	id := c.Param("id")
	if err := h.planSvc.DeletePlan(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to delete plan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "deleted"})
}
