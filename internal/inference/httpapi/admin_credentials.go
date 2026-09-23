// admin_credentials.go — 上游凭据管理端点（设计 §5 Credential/Account、
// §9.2 /admin/upstreams 凭据部分）。所有端点要求 credentials:manage
// 权限；响应只含脱敏状态视图，永不返回密文或明文。写操作审计。

package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
)

// AdminCredentialsHandler exposes the upstream credential lifecycle.
type AdminCredentialsHandler struct {
	svc *credentials.Service
}

func NewAdminCredentialsHandler(svc *credentials.Service) *AdminCredentialsHandler {
	return &AdminCredentialsHandler{svc: svc}
}

// Register mounts the credential endpoints. The caller wraps the group with
// the credentials:manage authorization middleware.
func (h *AdminCredentialsHandler) Register(g *gin.RouterGroup) {
	g.POST("/credentials", h.Create)
	g.GET("/credentials", h.List)
	g.GET("/credentials/:id", h.Get)
	g.POST("/credentials/:id/rotate", h.Rotate)
	g.POST("/credentials/:id/test", h.Test)
	g.POST("/credentials/:id/status", h.SetStatus)
}

type createCredentialRequest struct {
	ProviderID string     `json:"provider_id" binding:"required"`
	Label      string     `json:"label"`
	AuthType   string     `json:"auth_type" binding:"required"`
	Secret     string     `json:"secret" binding:"required"`
	Reason     string     `json:"reason" binding:"required"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// Create POST /credentials — the secret enters the vault exactly once here;
// only the masked view comes back.
func (h *AdminCredentialsHandler) Create(c *gin.Context) {
	var req createCredentialRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	view, err := h.svc.Create(c.Request.Context(), OperatorOf(c),
		req.ProviderID, req.Label, req.AuthType, req.Secret, req.Reason, req.ExpiresAt)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": view})
}

// List GET /credentials?provider_id=&limit=
func (h *AdminCredentialsHandler) List(c *gin.Context) {
	views, err := h.svc.List(c.Request.Context(), c.Query("provider_id"), parseLimit(c, 100))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"credentials": views})
}

// Get GET /credentials/:id
func (h *AdminCredentialsHandler) Get(c *gin.Context) {
	view, err := h.svc.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, view)
}

type rotateCredentialRequest struct {
	Secret string `json:"secret" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// Rotate POST /credentials/:id/rotate — replaces the secret material,
// re-encrypts under the current key version, bumps the generation CAS.
func (h *AdminCredentialsHandler) Rotate(c *gin.Context) {
	var req rotateCredentialRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	view, err := h.svc.Rotate(c.Request.Context(), OperatorOf(c), c.Param("id"), req.Secret, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, view)
}

type testCredentialRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// Test POST /credentials/:id/test — decrypt-check only; the response carries
// masked status, never secret bytes.
func (h *AdminCredentialsHandler) Test(c *gin.Context) {
	var req testCredentialRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	view, err := h.svc.Test(c.Request.Context(), OperatorOf(c), c.Param("id"), req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, view)
}

type setStatusRequest struct {
	Status string `json:"status" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// SetStatus POST /credentials/:id/status — emergency disable propagates to
// every upstream account bound to the credential in the same store call.
func (h *AdminCredentialsHandler) SetStatus(c *gin.Context) {
	var req setStatusRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	view, err := h.svc.SetStatus(c.Request.Context(), OperatorOf(c), c.Param("id"), req.Status, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, view)
}
