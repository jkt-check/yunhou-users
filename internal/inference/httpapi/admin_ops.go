// admin_ops.go — Task 4 挂载结构：把授权中间件与三个管理 handler 组合成
// 运营写面，router.Setup 在授权就绪后一次性挂载（Task 3 的写路由 404 是
// 被测试固定的安全属性，本文件是它们获得授权后的唯一挂载路径）。

package httpapi

import "github.com/gin-gonic/gin"

// AdminOps bundles the operator write surface: one authorization middleware
// per permission level and the handlers they protect. Built in cmd/server;
// nil in tests that don't exercise the operator surface.
type AdminOps struct {
	// RequireModels guards catalog writes (models:manage).
	RequireModels gin.HandlerFunc
	// RequireCredentials guards the credential lifecycle (credentials:manage).
	RequireCredentials gin.HandlerFunc
	// RequireAdmin guards operator administration (admin role).
	RequireAdmin gin.HandlerFunc
	// RequireBilling guards wallet adjustments / PAYG publishing
	// (billing:adjust, Task 14).
	RequireBilling gin.HandlerFunc
	// RequireUsage guards the operator analytics surface (usage:read,
	// Task 15) — auditor 角色也持有该权限（只读统计/异常/补偿追踪）。
	RequireUsage gin.HandlerFunc

	Models      *AdminModelsHandler
	Credentials *AdminCredentialsHandler
	Auth        *AdminAuthHandler
	// OAuth guards the upstream OAuth authorization lifecycle (Task 12);
	// mounted under the same credentials:manage permission.
	OAuth *AdminOAuthHandler
	// Accounts is the schedulable-account operator surface (create binding /
	// status / tune concurrency); same credentials:manage permission.
	Accounts *AdminAccountsHandler
	// Adjustments is the operator wallet surface (Task 14: 运营调整/冲正/
	// PAYG 发布配置; Task 15 扩展: 补偿列表 + 追加审计); mounted under
	// billing:adjust.
	Adjustments *AdminAdjustmentsHandler
	// Bulk is the catalog bulk-import surface (Task 15); models:manage.
	Bulk *AdminBulkHandler
	// Usage is the operator analytics surface (Task 15): summary/exceptions/
	// shared-account detection under usage:read; price/policy change
	// previews under models:manage.
	Usage *AdminUsageHandler
}

// Mount wires the write surface onto g. The caller must already have
// mounted JWTAuth + InternalAppAuth on g (or an ancestor group): every
// sub-group here adds its own permission gate on top.
func (o *AdminOps) Mount(g *gin.RouterGroup) {
	if o.Models != nil && o.RequireModels != nil {
		o.Models.RegisterWrite(g.Group("", o.RequireModels))
	}
	if o.Credentials != nil && o.RequireCredentials != nil {
		o.Credentials.Register(g.Group("", o.RequireCredentials))
	}
	if o.OAuth != nil && o.RequireCredentials != nil {
		o.OAuth.Register(g.Group("", o.RequireCredentials))
	}
	if o.Accounts != nil && o.RequireCredentials != nil {
		o.Accounts.Register(g.Group("", o.RequireCredentials))
	}
	if o.Auth != nil && o.RequireAdmin != nil {
		o.Auth.RegisterOperators(g.Group("", o.RequireAdmin))
	}
	if o.Adjustments != nil && o.RequireBilling != nil {
		o.Adjustments.Register(g.Group("", o.RequireBilling))
	}
	// Task 15: 批量导入与变更预览走 models:manage（目录/价格规则写面）；
	// 统计/异常/共享账号检测/补偿追踪走 usage:read（只读，auditor 可达）。
	if o.Bulk != nil && o.RequireModels != nil {
		o.Bulk.Register(g.Group("", o.RequireModels))
	}
	if o.Usage != nil && o.RequireUsage != nil {
		o.Usage.RegisterRead(g.Group("", o.RequireUsage))
	}
	if o.Usage != nil && o.RequireModels != nil {
		o.Usage.RegisterPreview(g.Group("", o.RequireModels))
	}
}
