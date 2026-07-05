package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// ProviderTokenLookup is the subset of ProviderTokenService the handler uses.
// Defined here so the handler can be tested without spinning up the full
// service stack. NewAppHandler accepts nil for callers that don't exercise
// GetProviderToken (e.g. CreateApp / UpdateApp tests).
type ProviderTokenLookup interface {
	Get(ctx context.Context, appID, channel string) (*model.ProviderToken, error)
}

type AppRepoInterface interface {
	List(context.Context) ([]model.App, error)
	FindByID(context.Context, string) (*model.App, error)
	Create(context.Context, *model.App) error
	Update(context.Context, *model.App) error
}

type AppHandler struct {
	appRepo       AppRepoInterface
	providerToken ProviderTokenLookup
}

func NewAppHandler(appRepo AppRepoInterface, providerToken ProviderTokenLookup) *AppHandler {
	return &AppHandler{appRepo: appRepo, providerToken: providerToken}
}

func (h *AppHandler) ListApps(c *gin.Context) {
	apps, err := h.appRepo.List(c.Request.Context())
	if err != nil {
		log.Printf("list apps error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list apps"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": apps})
}

func (h *AppHandler) GetApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if err != nil {
		log.Printf("get app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load app"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) CreateApp(c *gin.Context) {
	var req struct {
		AppID       string          `json:"app_id" binding:"required"`
		Name        string          `json:"name" binding:"required"`
		Description string          `json:"description"`
		Config      json.RawMessage `json:"config,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Config != nil {
		var cfg model.AppConfig
		if err := json.Unmarshal(req.Config, &cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid config: " + err.Error()})
			return
		}
		if err := validateAppConfig(&cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
	}

	app := &model.App{
		AppID:       req.AppID,
		Name:        req.Name,
		Description: req.Description,
		IsActive:    true,
		Config:      req.Config, // empty RawMessage when absent — repo COALESCE handles NULL
	}
	if err := h.appRepo.Create(c.Request.Context(), app); err != nil {
		log.Printf("create app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create app"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) UpdateApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if err != nil {
		log.Printf("update app lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load app"})
		return
	}

	var req struct {
		Name        *string          `json:"name"`
		Description *string          `json:"description"`
		IsActive    *bool            `json:"is_active"`
		Config      *json.RawMessage `json:"config,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "name must not be empty"})
			return
		}
		app.Name = trimmed
	}
	if req.Description != nil {
		app.Description = *req.Description
	}
	if req.IsActive != nil {
		app.IsActive = *req.IsActive
	}
	if req.Config != nil {
		var cfg model.AppConfig
		if err := json.Unmarshal(*req.Config, &cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid config: " + err.Error()})
			return
		}
		if err := validateAppConfig(&cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		app.Config = *req.Config
	}

	if err := h.appRepo.Update(c.Request.Context(), app); err != nil {
		log.Printf("update app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

// validateAppConfig ensures any configured payment provider has all required
// fields. Operators can leave payment_providers entirely absent (no providers
// configured) or include only the providers they intend to use; missing or
// malformed fields surface as 400 from the handler.
func validateAppConfig(cfg *model.AppConfig) error {
	if cfg.PaymentProviders == nil {
		return nil
	}
	if p := cfg.PaymentProviders.Paypal; p != nil {
		if p.ClientID == "" || p.ClientSecret == "" || p.WebhookID == "" {
			return errors.New("paypal: client_id, client_secret, webhook_id are required")
		}
		if p.Mode != "live" && p.Mode != "sandbox" {
			return errors.New("paypal.mode must be live or sandbox")
		}
	}
	if l := cfg.PaymentProviders.Lemonsqueezy; l != nil && (l.APIKey == "" || l.StoreID == "") {
		return errors.New("lemonsqueezy: api_key and store_id are required")
	}
	return nil
}

type SubscriptionHandler struct {
	subSvc service.SubscriptionServiceInterface
}

func NewSubscriptionHandler(subSvc service.SubscriptionServiceInterface) *SubscriptionHandler {
	return &SubscriptionHandler{subSvc: subSvc}
}

func (h *SubscriptionHandler) ListUserSubscriptions(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	subs, err := h.subSvc.ListUserSubscriptions(c.Request.Context(), userID)
	if err != nil {
		log.Printf("list subscriptions error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list subscriptions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": subs})
}

func (h *SubscriptionHandler) CreateSubscription(c *gin.Context) {
	var req struct {
		PlanID    string  `json:"plan_id" binding:"required"`
		ExpiresAt *string `json:"expires_at"` // accepted for backward compat; ignored by the service
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
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

	sub, err := h.subSvc.Create(c.Request.Context(), userID, req.PlanID, expiresAt)
	if err != nil {
		// Map known service sentinels to HTTP codes; everything else is
		// treated as internal and surfaces only as a generic 500.
		switch {
		case errors.Is(err, service.ErrPlanNotFound):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan not found"})
			return
		case errors.Is(err, service.ErrPlanInactive):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is inactive"})
			return
		case errors.Is(err, service.ErrPaidPlanForbidden):
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "paid plans require payment, cannot self-subscribe"})
			return
		case errors.Is(err, service.ErrUserHasActiveSub), errors.Is(err, service.ErrSubscriptionExists):
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "user already has an active subscription"})
			return
		default:
			log.Printf("create subscription error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create subscription"})
			return
		}
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": sub})
}

func (h *SubscriptionHandler) CancelSubscription(c *gin.Context) {
	id := c.Param("id")
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}

	if err := h.subSvc.Cancel(c.Request.Context(), id, userID); err != nil {
		// Service intentionally returns ErrSubscriptionNotFound for both
		// missing and not-owned-by-caller; surface that as 404.
		switch {
		case errors.Is(err, service.ErrSubscriptionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "subscription not found"})
			return
		case errors.Is(err, service.ErrAlreadyCancelled):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "already cancelled"})
			return
		default:
			log.Printf("cancel subscription error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to cancel subscription"})
			return
		}
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
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}
	if err != nil {
		log.Printf("get plan error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load plan"})
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
	if req.Price < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "price must be >= 0"})
		return
	}
	if req.IntervalDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "interval_days must be >= 0"})
		return
	}

	plan := &model.Plan{
		ID:           req.ID,
		Name:         req.Name,
		Price:        req.Price,
		IntervalDays: req.IntervalDays,
		Apps:         pq.StringArray(req.Apps),
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
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}
	if err != nil {
		log.Printf("update plan lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load plan"})
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
		if *req.Price < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "price must be >= 0"})
			return
		}
		plan.Price = *req.Price
	}
	if req.IntervalDays != nil {
		if *req.IntervalDays < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "interval_days must be >= 0"})
			return
		}
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
		// Postgres returns SQLSTATE 23503 when a FK reference prevents the
		// delete; surface that as a 409 Conflict with a clear message.
		if isFKViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan is in use by existing subscriptions"})
			return
		}
		log.Printf("delete plan error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to delete plan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "deleted"})
}

// isFKViolation returns true for lib/pq *pq.Error whose SQLSTATE is 23503
// (foreign_key_violation). Used by DeletePlan to map DB-level errors to a
// meaningful HTTP status code.
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23503"
	}
	return false
}