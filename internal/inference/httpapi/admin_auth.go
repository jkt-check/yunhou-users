// admin_auth.go — 运营授权中间件与运营人员管理端点（设计 §9.2）。
//
// 运营身份 = 组合认定：用户 JWT（middleware.JWTAuth 服务端验证，ContextUserID）
// + 受验证的服务身份（middleware.InternalAppAuth，ContextApp）。二者缺一即拒；
// 角色只来自 operator_roles 表，请求体自报的 role/actor 一律无效（凭据端点用
// 严格 JSON 解码显式拒绝携带未知字段的请求）。

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// Operator context keys, set ONLY by the authorization middleware.
const (
	ctxOperatorUserID = "operator_user_id"
	ctxOperatorAppID  = "operator_app_id"
	ctxOperatorRoles  = "operator_roles"
)

// OperatorSubjectKey is the gin context key the catalog handlers read via
// actorOf (admin_models.go). Value format: "user:<uid>@app:<appid>".
const OperatorSubjectKey = "operator_subject"

// OperatorStore loads operator roles. Satisfied by inference/postgres.Store.
type OperatorStore interface {
	RolesForUser(ctx context.Context, userID string) ([]string, error)
}

// OperatorAdminStore extends OperatorStore with the grant/revoke/list
// operations the AdminAuthHandler needs.
type OperatorAdminStore interface {
	OperatorStore
	GrantRole(ctx context.Context, userID, role string, grantedBy *string, reason string) (bool, error)
	RevokeRole(ctx context.Context, userID, role string) (bool, error)
	ListOperators(ctx context.Context) ([]management.OperatorGrant, error)
}

// loadOperator resolves the verified dual identity from the gin context
// (set by JWTAuth + InternalAppAuth before this middleware runs). Fail
// closed on every missing leg: the user identity, the service identity,
// and at least one operator role.
var (
	errNoUserIdentity    = errors.New("operator user identity missing")
	errNoServiceIdentity = errors.New("verified service identity missing")
	errNotAnOperator     = errors.New("user is not an operator")
)

func loadOperator(c *gin.Context, store OperatorStore) (userID, appID string, roles []string, err error) {
	userID = c.GetString(middleware.ContextUserID)
	if v, present := c.Get(middleware.ContextApp); present {
		if app, ok := v.(*model.App); ok && app != nil {
			appID = app.AppID
		}
	}
	if userID == "" {
		return "", "", nil, errNoUserIdentity
	}
	if appID == "" {
		return "", "", nil, errNoServiceIdentity
	}
	roles, err = store.RolesForUser(c.Request.Context(), userID)
	if err != nil {
		return "", "", nil, err
	}
	if len(roles) == 0 {
		return "", "", nil, errNotAnOperator
	}
	return userID, appID, roles, nil
}

// authzAbort maps a loadOperator failure to the response: a missing user
// identity is 401 (unauthenticated), everything else is 403.
func authzAbort(c *gin.Context, err error) {
	status := http.StatusForbidden
	if errors.Is(err, errNoUserIdentity) {
		status = http.StatusUnauthorized
	}
	c.AbortWithStatusJSON(status, gin.H{"code": status, "message": "operator authorization failed"})
}

func setOperatorContext(c *gin.Context, userID, appID string, roles []string) {
	c.Set(ctxOperatorUserID, userID)
	c.Set(ctxOperatorAppID, appID)
	c.Set(ctxOperatorRoles, roles)
	c.Set(OperatorSubjectKey, "user:"+userID+"@app:"+appID)
}

// OperatorAuthz requires the verified user JWT + service identity combo and
// a role carrying permission (management.PermissionsOf). Mount AFTER
// JWTAuth and InternalAppAuth.
func OperatorAuthz(store OperatorStore, permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, appID, roles, err := loadOperator(c, store)
		if err != nil {
			authzAbort(c, err)
			return
		}
		if !management.HasPermission(management.PermissionsOf(roles), permission) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "missing permission " + permission})
			return
		}
		setOperatorContext(c, userID, appID, roles)
		c.Next()
	}
}

// OperatorRequireRole requires the verified combo plus a specific role
// (permission administration: grant/revoke need the admin role).
func OperatorRequireRole(store OperatorStore, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, appID, roles, err := loadOperator(c, store)
		if err != nil {
			authzAbort(c, err)
			return
		}
		has := false
		for _, r := range roles {
			if r == role {
				has = true
				break
			}
		}
		if !has {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "requires role " + role})
			return
		}
		setOperatorContext(c, userID, appID, roles)
		c.Next()
	}
}

// OperatorOf returns the verified operator attribution for handlers. Only
// meaningful after an authz middleware ran.
func OperatorOf(c *gin.Context) credentials.Operator {
	return credentials.Operator{
		UserID: c.GetString(ctxOperatorUserID),
		AppID:  c.GetString(ctxOperatorAppID),
		Roles:  c.GetStringSlice(ctxOperatorRoles),
	}
}

// strictBindJSON decodes the body rejecting unknown fields, so a client
// smuggling actor/role fields gets an explicit 400 instead of a silent
// ignore (设计 §9.2: 不接受请求体伪造 actor/role).
func strictBindJSON(c *gin.Context, dst any) error {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// --- operator administration handlers ---

// AdminAuthHandler exposes operator role administration (grant/revoke/list).
// Every mutation is audited with dual attribution.
type AdminAuthHandler struct {
	store OperatorAdminStore
	audit management.AuditRecorder
}

func NewAdminAuthHandler(store OperatorAdminStore, audit management.AuditRecorder) *AdminAuthHandler {
	return &AdminAuthHandler{store: store, audit: audit}
}

// RegisterOperators mounts the operator administration endpoints. All
// require the admin role (wired by the caller via OperatorRequireRole).
func (h *AdminAuthHandler) RegisterOperators(g *gin.RouterGroup) {
	g.GET("/operators", h.ListOperators)
	g.POST("/operators", h.GrantRole)
	g.DELETE("/operators/:user_id/roles/:role", h.RevokeRole)
}

type grantRoleRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Role   string `json:"role" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// GrantRole POST /operators — idempotent (re-grant reports granted=false).
func (h *AdminAuthHandler) GrantRole(c *gin.Context) {
	var req grantRoleRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	if !management.ValidRole(req.Role) {
		fail(c, domain.NewError(domain.CodeInvalidInput, "role must be admin, operator or auditor"))
		return
	}
	op := OperatorOf(c)
	granted, err := h.store.GrantRole(c.Request.Context(), req.UserID, req.Role, strPtrOrNil(op.UserID), req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	if err := h.audit.Record(c.Request.Context(), management.AuditEvent{
		Action: "permission.grant", ObjectType: "permission", ObjectID: req.UserID + ":" + req.Role,
		Reason: req.Reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(map[string]any{"role": req.Role, "granted": granted}),
	}); err != nil {
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	ok(c, gin.H{"granted": granted, "user_id": req.UserID, "role": req.Role})
}

// RevokeRole DELETE /operators/:user_id/roles/:role — idempotent.
func (h *AdminAuthHandler) RevokeRole(c *gin.Context) {
	userID, role := c.Param("user_id"), c.Param("role")
	if !management.ValidRole(role) {
		fail(c, domain.NewError(domain.CodeInvalidInput, "role must be admin, operator or auditor"))
		return
	}
	op := OperatorOf(c)
	revoked, err := h.store.RevokeRole(c.Request.Context(), userID, role)
	if err != nil {
		fail(c, err)
		return
	}
	if err := h.audit.Record(c.Request.Context(), management.AuditEvent{
		Action: "permission.revoke", ObjectType: "permission", ObjectID: userID + ":" + role,
		Reason: "admin revoke", ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(map[string]any{"role": role, "revoked": revoked}),
	}); err != nil {
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	ok(c, gin.H{"revoked": revoked, "user_id": userID, "role": role})
}

func (h *AdminAuthHandler) ListOperators(c *gin.Context) {
	grants, err := h.store.ListOperators(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"operators": grants})
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
