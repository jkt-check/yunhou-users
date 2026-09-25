// admin_oauth.go — 上游 OAuth 授权管理端点（Task 12，设计 §8/§9.2）。
//
// 独立于社交登录（不经过 apps/oauth_providers/identities）；全部端点挂在
// credentials:manage 授权组下，写操作审计。响应只有脱敏视图：authorize
// URL + state 可以展示给发起运营人员（state 是一次性且绑定其身份），
// token 材料/密文/PKCE verifier 永不出现在响应中。
//
// 上游额度视图（GET /upstream-accounts）仅管理端可见；客户读面（Task 11
// /user/*）不经手任何上游账号数据。

package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
)

// AdminOAuthStore is the read surface the handler needs beyond the
// services; satisfied by inference/postgres.Store.
type AdminOAuthStore interface {
	ListUpstreamAccounts(ctx context.Context, providerID string, limit int) ([]domain.UpstreamAccount, error)
}

// AdminOAuthHandler exposes the upstream OAuth authorization lifecycle.
type AdminOAuthHandler struct {
	oauth     *credentials.OAuthService
	refresher *credentials.Refresher
	store     AdminOAuthStore
}

func NewAdminOAuthHandler(oauth *credentials.OAuthService, refresher *credentials.Refresher, store AdminOAuthStore) *AdminOAuthHandler {
	return &AdminOAuthHandler{oauth: oauth, refresher: refresher, store: store}
}

// Register mounts the OAuth endpoints. The caller wraps the group with the
// credentials:manage authorization middleware.
func (h *AdminOAuthHandler) Register(g *gin.RouterGroup) {
	g.POST("/oauth/authorizations", h.BeginAuthorization)
	g.POST("/oauth/callback", h.Callback)
	g.POST("/oauth/credentials/:id/revoke", h.Revoke)
	g.POST("/oauth/credentials/:id/refresh", h.Refresh)
	g.GET("/upstream-accounts", h.ListAccounts)
}

type beginAuthorizationRequest struct {
	ProviderID   string `json:"provider_id" binding:"required"`
	Connector    string `json:"connector" binding:"required"`
	AccountLabel string `json:"account_label"`
	Reason       string `json:"reason" binding:"required"`
}

// BeginAuthorization POST /oauth/authorizations — returns the vendor URL +
// state for the operator to complete in a browser.
func (h *AdminOAuthHandler) BeginAuthorization(c *gin.Context) {
	var req beginAuthorizationRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	start, err := h.oauth.BeginAuthorization(c.Request.Context(), OperatorOf(c),
		req.ProviderID, req.Connector, req.AccountLabel, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": start})
}

type oauthCallbackRequest struct {
	State  string `json:"state" binding:"required"`
	Code   string `json:"code" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// Callback POST /oauth/callback — one-time state consumption + code
// exchange + sealed storage + account creation. A replayed or expired state
// is a 409.
func (h *AdminOAuthHandler) Callback(c *gin.Context) {
	var req oauthCallbackRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	res, err := h.oauth.HandleCallback(c.Request.Context(), OperatorOf(c), req.State, req.Code, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}

type oauthReasonRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// Revoke POST /oauth/credentials/:id/revoke — 本地吊销为准（凭据 revoked +
// 账号 disabled 同事务），厂商侧吊销尽力而为。
func (h *AdminOAuthHandler) Revoke(c *gin.Context) {
	var req oauthReasonRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	view, err := h.oauth.Revoke(c.Request.Context(), OperatorOf(c), c.Param("id"), req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, view)
}

// Refresh POST /oauth/credentials/:id/refresh — operator-triggered rotation
// (same lock + CAS protocol as the worker).
func (h *AdminOAuthHandler) Refresh(c *gin.Context) {
	var req oauthReasonRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	outcome, err := h.refresher.RefreshCredential(c.Request.Context(), c.Param("id"), req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, outcome)
}

// accountView masks an upstream account for the operator read. The upstream
// quota cache is operator-only (never reaches customer read surfaces).
type accountView struct {
	ID                string `json:"id"`
	ProviderID        string `json:"provider_id"`
	CredentialID      string `json:"credential_id"`
	ExternalAccountID string `json:"external_account_id"`
	DisplayName       string `json:"display_name"`
	Status            string `json:"status"`
	ConcurrencyLimit  int    `json:"concurrency_limit"`
	// 上游额度缓存：字段可空 = 未知（设计 §8 未知保持未知）。
	QuotaLimitMicros     *int64  `json:"quota_limit_micros,omitempty"`
	QuotaRemainingMicros *int64  `json:"quota_remaining_micros,omitempty"`
	QuotaObservedAt      *string `json:"quota_observed_at,omitempty"`
	QuotaSource          *string `json:"quota_source,omitempty"`
	QuotaResetAt         *string `json:"quota_reset_at,omitempty"`
}

// ListAccounts GET /upstream-accounts?provider_id=&limit=
func (h *AdminOAuthHandler) ListAccounts(c *gin.Context) {
	providerID := c.Query("provider_id")
	if providerID == "" {
		fail(c, domain.NewError(domain.CodeInvalidInput, "provider_id is required"))
		return
	}
	accounts, err := h.store.ListUpstreamAccounts(c.Request.Context(), providerID, parseLimit(c, 100))
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]accountView, 0, len(accounts))
	for i := range accounts {
		views = append(views, toAccountView(&accounts[i]))
	}
	ok(c, gin.H{"accounts": views})
}
