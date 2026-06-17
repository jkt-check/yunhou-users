package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger is the minimal interface HealthHandler needs. *sqlx.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// HealthHandler exposes /healthz for liveness/readiness checks.
// Returns 200 when the process is alive and the dependency is reachable;
// 503 when the dependency is not. Intentionally bypasses rate limiting
// (route is registered before the public rate-limit middleware).
type HealthHandler struct {
	Pinger Pinger
}

func (h *HealthHandler) Handle(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
	defer cancel()
	if err := h.Pinger.PingContext(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    http.StatusServiceUnavailable,
			"message": "db unavailable",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{"status": "ok"},
	})
}

func NewHealthHandler(p Pinger) *HealthHandler {
	return &HealthHandler{Pinger: p}
}
