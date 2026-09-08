// operators.go — 运营角色 → 权限映射（设计 §9.2 权限矩阵）。角色只来自
// operator_roles 表（迁移 028），由受验证的用户 JWT + 服务身份在中间件中
// 解析；请求体自报 role/actor 一律无效。
package management

import "time"

// Role names stored in operator_roles.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleAuditor  = "auditor"
)

// Permissions of the design §9.2 matrix.
const (
	PermModelsManage      = "models:manage"
	PermCredentialsManage = "credentials:manage"
	PermBillingAdjust     = "billing:adjust"
	PermUsageRead         = "usage:read"
)

// rolePermissions maps each role to its permission set. 后台读权限
// (usage:read) 与秘密写权限 (credentials:manage) 显式分开：auditor 只读
// 统计，不能触碰凭据。
var rolePermissions = map[string][]string{
	RoleAdmin:    {PermModelsManage, PermCredentialsManage, PermBillingAdjust, PermUsageRead},
	RoleOperator: {PermModelsManage, PermCredentialsManage, PermUsageRead},
	RoleAuditor:  {PermUsageRead},
}

// ValidRole reports whether role is a known role name.
func ValidRole(role string) bool {
	_, ok := rolePermissions[role]
	return ok
}

// PermissionsOf unions the permission sets of the given roles. Unknown roles
// contribute nothing (fail closed).
func PermissionsOf(roles []string) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		for _, p := range rolePermissions[r] {
			out[p] = true
		}
	}
	return out
}

// HasPermission reports whether perms grants permission.
func HasPermission(perms map[string]bool, permission string) bool {
	return perms[permission]
}

// OperatorGrant is one operator_roles row — the shared DTO of the postgres
// store and the httpapi handlers.
type OperatorGrant struct {
	UserID    string    `json:"user_id"`
	Role      string    `json:"role"`
	GrantedBy *string   `json:"granted_by,omitempty"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
