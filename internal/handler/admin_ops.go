package handler

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// adminOpsService is the AdminOpsService surface the handler needs,
// declared locally so tests can inject a mock (usageService pattern).
type adminOpsService interface {
	Metrics(ctx context.Context, tz string) (*service.AdminOpsMetrics, error)
}

// AdminOpsHandler serves GET /admin/ops/metrics (dashboard-admin-api spec
// §2) on the adminGroup chain (RateLimit + InternalAppAuth).
type AdminOpsHandler struct {
	svc adminOpsService
}

func NewAdminOpsHandler(svc adminOpsService) *AdminOpsHandler {
	return &AdminOpsHandler{svc: svc}
}

// GetMetrics handles GET /admin/ops/metrics?tz=... (tz defaults to
// Asia/Shanghai in the service).
func (h *AdminOpsHandler) GetMetrics(c *gin.Context) {
	metrics, err := h.svc.Metrics(c.Request.Context(), c.Query("tz"))
	if err != nil {
		if errors.Is(err, service.ErrAdminInvalidParam) {
			writeAdminOpsError(c, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("admin ops metrics: %v", err)
		writeAdminOpsError(c, http.StatusInternalServerError, "internal error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": metrics})
}

// writeAdminOpsError emits the standard {"code","data","message"} shape
// (usage.go writeUsageError pattern).
func writeAdminOpsError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"code": status, "data": nil, "message": message})
}
