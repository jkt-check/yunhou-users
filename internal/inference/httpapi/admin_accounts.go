// admin_accounts.go — 可调度账号（upstream account）管理端点。补静态 key
// 路径的管理端闭环：POST /admin/credentials 只建凭据，账号绑定经本面完
// 成；凭据吊销恢复后账号的唯一恢复入口也在本面（status 端点）。全部挂在
// credentials:manage 授权组（与 /admin/credentials、OAuth 面同组），写操
// 作 strictBindJSON + 必填 reason + 同事务审计。
//
// 幂等创建：表上 UNIQUE(provider_id, credential_id)（migration 025）；
// 重复创建返回 409 且 data 带已存在账号视图（调用方可安全重放）。
//
// 账号是运行时调度状态，不在 catalog 发布快照里：落库 active 即入路由
// 池，无需 publish。

package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
)

// AdminAccountsHandler exposes the upstream-account operator lifecycle.
type AdminAccountsHandler struct {
	svc *credentials.AccountService
}

func NewAdminAccountsHandler(svc *credentials.AccountService) *AdminAccountsHandler {
	return &AdminAccountsHandler{svc: svc}
}

// Register mounts the account endpoints. The caller wraps the group with
// the credentials:manage authorization middleware.
func (h *AdminAccountsHandler) Register(g *gin.RouterGroup) {
	g.POST("/upstream-accounts", h.Create)
	g.POST("/upstream-accounts/:id/status", h.SetStatus)
	g.PATCH("/upstream-accounts/:id", h.Update)
}

// toAccountView renders the operator view of one account — same shape as
// GET /upstream-accounts (admin_oauth.go). 上游额度缓存仅管理端可见。
func toAccountView(a *domain.UpstreamAccount) accountView {
	v := accountView{
		ID: a.ID, ProviderID: a.ProviderID, CredentialID: a.CredentialID,
		ExternalAccountID: a.ExternalAccountID, DisplayName: a.DisplayName,
		Status: string(a.Status), ConcurrencyLimit: a.ConcurrencyLimit,
		QuotaSource: a.Quota.Source,
	}
	if a.Quota.LimitMicros != nil {
		n := int64(*a.Quota.LimitMicros)
		v.QuotaLimitMicros = &n
	}
	if a.Quota.RemainingMicros != nil {
		n := int64(*a.Quota.RemainingMicros)
		v.QuotaRemainingMicros = &n
	}
	if a.Quota.ObservedAt != nil {
		s := a.Quota.ObservedAt.UTC().Format("2006-01-02T15:04:05.999999999Z")
		v.QuotaObservedAt = &s
	}
	if a.Quota.ResetAt != nil {
		s := a.Quota.ResetAt.UTC().Format("2006-01-02T15:04:05.999999999Z")
		v.QuotaResetAt = &s
	}
	return v
}

type createAccountRequest struct {
	ProviderID   string `json:"provider_id" binding:"required"`
	CredentialID string `json:"credential_id" binding:"required"`
	DisplayName  string `json:"display_name"`
	// nil = 服务端默认 1；显式 0 = 备而不用（合法）。
	ConcurrencyLimit *int `json:"concurrency_limit"`
	// 额度组：成组出现（limit+remaining 必填、≥0），给了即记
	// source=reported / observed_at=now。
	QuotaLimitMicros     *int64     `json:"quota_limit_micros"`
	QuotaRemainingMicros *int64     `json:"quota_remaining_micros"`
	QuotaResetAt         *time.Time `json:"quota_reset_at"`
	Reason               string     `json:"reason" binding:"required"`
}

// Create POST /upstream-accounts — 凭据→可调度账号绑定（新 provider 接入的
// 最后一步）。重复绑定同一对返回 409 + 已存在视图（幂等重放安全）。
func (h *AdminAccountsHandler) Create(c *gin.Context) {
	var req createAccountRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	acct, err := h.svc.Create(c.Request.Context(), OperatorOf(c), credentials.CreateAccountInput{
		ProviderID:           req.ProviderID,
		CredentialID:         req.CredentialID,
		DisplayName:          req.DisplayName,
		ConcurrencyLimit:     req.ConcurrencyLimit,
		QuotaLimitMicros:     req.QuotaLimitMicros,
		QuotaRemainingMicros: req.QuotaRemainingMicros,
		QuotaResetAt:         req.QuotaResetAt,
		Reason:               req.Reason,
	})
	if err != nil {
		var exists *credentials.AccountExistsError
		if errors.As(err, &exists) && exists.Existing != nil {
			c.JSON(http.StatusConflict, gin.H{
				"code":    http.StatusConflict,
				"message": "upstream account already exists for (provider_id, credential_id)",
				"data":    toAccountView(exists.Existing),
			})
			return
		}
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": toAccountView(acct)})
}

type setAccountStatusRequest struct {
	Status string `json:"status" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// SetStatus POST /upstream-accounts/:id/status — 运营启停（active/disabled
// 两种外部可写态；运行时内部态不可写）。禁用即退出调度，不反向动凭据；
// 激活是凭据吊销恢复后账号的唯一恢复入口（凭据仍 revoked 时 409）。
func (h *AdminAccountsHandler) SetStatus(c *gin.Context) {
	var req setAccountStatusRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	acct, err := h.svc.SetStatus(c.Request.Context(), OperatorOf(c), c.Param("id"), req.Status, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toAccountView(acct))
}

type updateAccountRequest struct {
	DisplayName      *string `json:"display_name"`
	ConcurrencyLimit *int    `json:"concurrency_limit"`
	Reason           string  `json:"reason" binding:"required"`
}

// Update PATCH /upstream-accounts/:id — 调整 display_name /
// concurrency_limit，不重建凭据绑定。
func (h *AdminAccountsHandler) Update(c *gin.Context) {
	var req updateAccountRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	acct, err := h.svc.Update(c.Request.Context(), OperatorOf(c), c.Param("id"),
		req.DisplayName, req.ConcurrencyLimit, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toAccountView(acct))
}
