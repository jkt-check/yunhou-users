package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// usageService is the UsageService surface the handler needs, declared
// locally so tests can inject a hand-rolled mock (same pattern as
// chatStreamer in chat.go).
type usageService interface {
	RecordBeats(ctx context.Context, userID, appID string, beats []model.UsageBeat) (int, error)
	ActiveStats(ctx context.Context, date, granularity, groupBy string) (*model.ActiveStats, error)
	DurationStats(ctx context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error)
	NewUsersStats(ctx context.Context, from, to string) ([]model.NewUsersRow, error)
}

// UsageHandler serves POST /user/usage/heartbeat (JWT user route) and the
// three /admin/stats/* reads (internal app auth). See
// 2026-09-04-usage-analytics-design.md §3.3.
type UsageHandler struct {
	svc usageService
}

func NewUsageHandler(svc usageService) *UsageHandler {
	return &UsageHandler{svc: svc}
}

// usageUUIDPattern validates client_event_id — the DB column is UUID, so
// rejecting malformed values here turns a would-be 500 into a 400.
var usageUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// PostHeartbeat handles POST /user/usage/heartbeat. Identity comes from the
// JWT (middleware.ContextUserID / ContextAppID) — the body carries only
// non-PII beat data, so there is nothing to cross-check against.
func (h *UsageHandler) PostHeartbeat(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	appID := c.GetString(middleware.ContextAppID)

	var req model.UsageHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUsageError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg := validateUsageBeats(req.Beats); msg != "" {
		writeUsageError(c, http.StatusBadRequest, msg)
		return
	}

	inserted, err := h.svc.RecordBeats(c.Request.Context(), userID, appID, req.Beats)
	if err != nil {
		log.Printf("usage: record beats: %v", err)
		writeUsageError(c, http.StatusInternalServerError, "internal error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"accepted": len(req.Beats),
		"inserted": inserted,
	}})
}

// validateUsageBeats enforces the per-beat bounds from the design (§3.3):
// batch ≤100, platform enum, active_seconds 0–3600, version length cap.
// Returns "" when valid, otherwise a client-safe reason.
func validateUsageBeats(beats []model.UsageBeat) string {
	if len(beats) == 0 {
		return "beats is required"
	}
	if len(beats) > model.UsageMaxBeatsPerRequest {
		return "too many beats"
	}
	for _, b := range beats {
		if !usageUUIDPattern.MatchString(b.ClientEventID) {
			return "invalid client_event_id"
		}
		if b.OccurredAt.IsZero() {
			return "occurred_at is required"
		}
		if _, err := time.Parse("2006-01-02", b.LocalDate); err != nil {
			return "invalid local_date"
		}
		if b.ActiveSeconds < 0 || b.ActiveSeconds > model.UsageMaxActiveSeconds {
			return "active_seconds out of range"
		}
		validPlatform := false
		for _, p := range model.UsagePlatforms {
			if b.Platform == p {
				validPlatform = true
				break
			}
		}
		if !validPlatform {
			return "invalid platform"
		}
		if len(b.AppVersion) == 0 || len(b.AppVersion) > model.UsageMaxAppVersionLen {
			return "invalid app_version"
		}
	}
	return ""
}

// GetActiveStats handles GET /admin/stats/active. date defaults to today
// (server timezone), granularity to "day"; group_by is optional.
func (h *UsageHandler) GetActiveStats(c *gin.Context) {
	date := c.DefaultQuery("date", time.Now().Format("2006-01-02"))
	granularity := c.DefaultQuery("granularity", "day")
	groupBy := c.Query("group_by")

	stats, err := h.svc.ActiveStats(c.Request.Context(), date, granularity, groupBy)
	if err != nil {
		writeUsageStatsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": stats})
}

// GetUsageDuration handles GET /admin/stats/usage-duration. from/to are
// required; granularity defaults to "day"; group_by is optional.
func (h *UsageHandler) GetUsageDuration(c *gin.Context) {
	from, to := c.Query("from"), c.Query("to")
	if from == "" || to == "" {
		writeUsageError(c, http.StatusBadRequest, "from and to are required")
		return
	}
	granularity := c.DefaultQuery("granularity", "day")
	groupBy := c.Query("group_by")

	rows, err := h.svc.DurationStats(c.Request.Context(), from, to, granularity, groupBy)
	if err != nil {
		writeUsageStatsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rows})
}

// GetNewUsers handles GET /admin/stats/new-users. from/to are required.
func (h *UsageHandler) GetNewUsers(c *gin.Context) {
	from, to := c.Query("from"), c.Query("to")
	if from == "" || to == "" {
		writeUsageError(c, http.StatusBadRequest, "from and to are required")
		return
	}
	rows, err := h.svc.NewUsersStats(c.Request.Context(), from, to)
	if err != nil {
		writeUsageStatsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rows})
}

// writeUsageStatsError maps service errors: parameter validation → 400
// (message carries the detail), anything else → generic 500.
func writeUsageStatsError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrUsageInvalidParam) {
		writeUsageError(c, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("usage: stats query: %v", err)
	writeUsageError(c, http.StatusInternalServerError, "internal error")
}

// writeUsageError emits the standard {"code","data","message"} error shape.
func writeUsageError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"code": status, "data": nil, "message": message})
}
