package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// adminUsersService is the AdminUsersService surface the handler needs,
// declared locally so tests can inject a mock (usageService pattern).
type adminUsersService interface {
	SearchUsers(ctx context.Context, q string) (*service.AdminUserSearchResult, error)
	GetUserDetail(ctx context.Context, userID string) (*service.AdminUserDetail, error)
	AddVipDays(ctx context.Context, appID, actor, userID string, days int, idemKey string) (*service.AdminVipResult, error)
}

// AdminUsersHandler serves the dashboard user-management endpoints:
// GET /admin/users/search, GET /admin/users/:id, POST /admin/users/:id/vip
// (dashboard-admin-api spec §3–§5). Auth is the adminGroup chain
// (RateLimit + InternalAppAuth), not the operator JWT opsGroup.
type AdminUsersHandler struct {
	svc adminUsersService
}

func NewAdminUsersHandler(svc adminUsersService) *AdminUsersHandler {
	return &AdminUsersHandler{svc: svc}
}

// adminUserUUIDPattern validates the :id path param (spec §4): lowercase
// or uppercase canonical UUID, anything else is a 400 rather than a DB
// error.
var adminUserUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// adminIdempotencyKeyMaxLen bounds the Idempotency-Key header (spec §5).
const adminIdempotencyKeyMaxLen = 128

// SearchUsers handles GET /admin/users/search?q=...
func (h *AdminUsersHandler) SearchUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		writeAdminUsersError(c, http.StatusBadRequest, "搜索关键词不能为空")
		return
	}
	res, err := h.svc.SearchUsers(c.Request.Context(), q)
	if err != nil {
		writeAdminUsersServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res})
}

// GetUser handles GET /admin/users/:id.
func (h *AdminUsersHandler) GetUser(c *gin.Context) {
	id := c.Param("id")
	if !adminUserUUIDPattern.MatchString(id) {
		writeAdminUsersError(c, http.StatusBadRequest, "非法用户 ID: "+id)
		return
	}
	detail, err := h.svc.GetUserDetail(c.Request.Context(), id)
	if err != nil {
		writeAdminUsersServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": detail})
}

// adminVipRequest is the POST /admin/users/:id/vip body. Decoding is
// strict (DisallowUnknownFields) so a typo'd field fails loudly.
type adminVipRequest struct {
	Days int `json:"days"`
}

// AddVip handles POST /admin/users/:id/vip. Attribution for the audit row
// and the idempotency-key scope come from the authenticated app
// (InternalAppAuth has already run on this group).
func (h *AdminUsersHandler) AddVip(c *gin.Context) {
	id := c.Param("id")
	if !adminUserUUIDPattern.MatchString(id) {
		writeAdminUsersError(c, http.StatusBadRequest, "非法用户 ID: "+id)
		return
	}

	var req adminVipRequest
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeAdminUsersError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Days < service.AdminVipMinDays || req.Days > service.AdminVipMaxDays {
		writeAdminUsersError(c, http.StatusBadRequest, "非法天数（应为 1-3650 的整数）")
		return
	}

	idemKey := c.GetHeader("Idempotency-Key")
	if len(idemKey) > adminIdempotencyKeyMaxLen {
		writeAdminUsersError(c, http.StatusBadRequest, "Idempotency-Key 过长（上限 128 字符）")
		return
	}

	res, err := h.svc.AddVipDays(c.Request.Context(), callerAppID(c), adminActorID(c), id, req.Days, idemKey)
	if err != nil {
		writeAdminUsersServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res})
}

// writeAdminUsersServiceError maps service sentinel errors to the spec
// §1.2 table; anything else is logged and surfaced as a generic 500.
func writeAdminUsersServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrAdminInvalidParam):
		writeAdminUsersError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrUserNotFound):
		writeAdminUsersError(c, http.StatusNotFound, "用户不存在")
	case errors.Is(err, service.ErrAdminVipRejected):
		writeAdminUsersError(c, http.StatusConflict, err.Error())
	default:
		log.Printf("admin users: %v", err)
		writeAdminUsersError(c, http.StatusInternalServerError, "internal error")
	}
}

// writeAdminUsersError emits the standard {"code","data","message"} shape
// (usage.go writeUsageError pattern).
func writeAdminUsersError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"code": status, "data": nil, "message": message})
}
