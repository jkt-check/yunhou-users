package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// LLMUsageHandler serves GET /admin/stats/llm-usage (per-model token/cost
// aggregates over llm_usage_events).
type LLMUsageHandler struct {
	svc *service.LLMUsageService
}

func NewLLMUsageHandler(svc *service.LLMUsageService) *LLMUsageHandler {
	return &LLMUsageHandler{svc: svc}
}

// GetByModel handles GET /admin/stats/llm-usage?from=YYYY-MM-DD&to=YYYY-MM-DD.
func (h *LLMUsageHandler) GetByModel(c *gin.Context) {
	// The route is registered unconditionally (router.Setup); a nil service
	// means the feature isn't wired, so degrade to 503 instead of panicking —
	// the same request-time-error convention as a disabled chat service.
	if h.svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": http.StatusServiceUnavailable, "data": nil, "message": "llm usage stats not enabled"})
		return
	}
	rows, err := h.svc.StatsByModel(c.Request.Context(), c.Query("from"), c.Query("to"))
	if err != nil {
		if errors.Is(err, service.ErrUsageInvalidParam) {
			c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "data": nil, "message": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": http.StatusInternalServerError, "data": nil, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rows})
}
