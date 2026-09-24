// admin_auth.go — 运营授权中间件与运营人员管理端点（设计 §9.2）。
//
// 运营身份 = 组合认定：用户 JWT（middleware.JWTAuth 服务端验证，ContextUserID；
// 其 app claim 落在 ContextAppID）+ 受验证的服务身份（middleware.InternalAppAuth，
// ContextApp）。三者缺一即拒，且 JWT 的 app claim 必须与受验证服务身份一致
// （跨 app 的 JWT+secret 组合 403）；角色只来自 operator_roles 表，请求体自报的
// role/actor 一律无效（凭据端点用严格 JSON 解码显式拒绝携带未知字段的请求）。

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

// OperatorAdminTxStore 是支持事务化授权变更的 operator store（评审轮1
// I-2：权限变更与审计行同事务提交或回滚，绝不落下无审计的权限变更——与
// 钱包面 RecordTx 先例同口径）。由 inference/postgres.Store 满足。
type OperatorAdminTxStore interface {
	OperatorAdminStore
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GrantRoleTx(ctx context.Context, w domain.UnitOfWork, userID, role string, grantedBy *string, reason string) (bool, error)
	RevokeRoleTx(ctx context.Context, w domain.UnitOfWork, userID, role string) (bool, error)
}

// loadOperator resolves the verified dual identity from the gin context
// (set by JWTAuth + InternalAppAuth before this middleware runs). Fail
// closed on every missing or mismatched leg: the user identity, the service
// identity, the BINDING between them (a JWT issued for app A must not be
// usable with app B's verified secret), and at least one operator role.
var (
	errNoUserIdentity    = errors.New("operator user identity missing")
	errNoServiceIdentity = errors.New("verified service identity missing")
	errAppMismatch       = errors.New("JWT audience app does not match the verified service identity")
	errNotAnOperator     = errors.New("user is not an operator")
)

func loadOperator(c *gin.Context, store OperatorStore) (userID, appID string, roles []string, err error) {
	userID = c.GetString(middleware.ContextUserID)
	jwtAppID := c.GetString(middleware.ContextAppID)
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
	// Bind the two legs: JWTAuth stores the token's app claim in
	// ContextAppID; InternalAppAuth stores the secret-verified *model.App.
	// A stolen operator JWT (issued for app A) combined with an attacker's
	// own app B secret must not authorize — and must not launder audit
	// attribution through the attacker's app.
	if jwtAppID == "" || jwtAppID != appID {
		return "", "", nil, errAppMismatch
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
// ignore (设计 §9.2: 不接受请求体伪造 actor/role). 单值后必须是 EOF
// （评审轮1 m4：{"user_id":"x"...} garbage 这类尾部脏数据同样拒绝）。
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
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing data after the JSON value")
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

// grantAuditSupport resolves the transactional store + audit recorder the
// mutation handlers need. 缺任一时 fail-closed（评审轮1 I-2，对齐
// bulk_import.go 先例：审计与权限变更不能同生共死就拒绝变更，绝不落下
// 无审计的授权/撤销）。
func (h *AdminAuthHandler) grantAuditSupport() (OperatorAdminTxStore, management.AuditTxRecorder, error) {
	txStore, ok := h.store.(OperatorAdminTxStore)
	if !ok {
		return nil, nil, domain.NewError(domain.CodeInternal, "operator store lacks transactional support")
	}
	txAudit, ok := h.audit.(management.AuditTxRecorder)
	if !ok {
		return nil, nil, domain.NewError(domain.CodeInternal, "audit recorder lacks transactional support")
	}
	return txStore, txAudit, nil
}

// GrantRole POST /operators — idempotent (re-grant reports granted=false).
// 授权与审计同事务提交（评审轮1 I-2）。
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
	txStore, txAudit, err := h.grantAuditSupport()
	if err != nil {
		fail(c, err)
		return
	}
	op := OperatorOf(c)
	ctx := c.Request.Context()
	uow, err := txStore.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	granted, err := txStore.GrantRoleTx(ctx, uow, req.UserID, req.Role, strPtrOrNil(op.UserID), req.Reason)
	if err != nil {
		_ = uow.Rollback(ctx)
		fail(c, err)
		return
	}
	if err := txAudit.RecordTx(ctx, uow, management.AuditEvent{
		Action: "permission.grant", ObjectType: "permission", ObjectID: req.UserID + ":" + req.Role,
		Reason: req.Reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(map[string]any{"role": req.Role, "granted": granted}),
	}); err != nil {
		_ = uow.Rollback(ctx)
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"granted": granted, "user_id": req.UserID, "role": req.Role})
}

// RevokeRole DELETE /operators/:user_id/roles/:role — idempotent.
// 撤销与审计同事务提交（评审轮1 I-2）。
func (h *AdminAuthHandler) RevokeRole(c *gin.Context) {
	userID, role := c.Param("user_id"), c.Param("role")
	if !management.ValidRole(role) {
		fail(c, domain.NewError(domain.CodeInvalidInput, "role must be admin, operator or auditor"))
		return
	}
	txStore, txAudit, err := h.grantAuditSupport()
	if err != nil {
		fail(c, err)
		return
	}
	op := OperatorOf(c)
	ctx := c.Request.Context()
	uow, err := txStore.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	revoked, err := txStore.RevokeRoleTx(ctx, uow, userID, role)
	if err != nil {
		_ = uow.Rollback(ctx)
		fail(c, err)
		return
	}
	if err := txAudit.RecordTx(ctx, uow, management.AuditEvent{
		Action: "permission.revoke", ObjectType: "permission", ObjectID: userID + ":" + role,
		Reason: "admin revoke", ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(map[string]any{"role": role, "revoked": revoked}),
	}); err != nil {
		_ = uow.Rollback(ctx)
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
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
