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

	Models      *AdminModelsHandler
	Credentials *AdminCredentialsHandler
	Auth        *AdminAuthHandler
	// OAuth guards the upstream OAuth authorization lifecycle (Task 12);
	// mounted under the same credentials:manage permission.
	OAuth *AdminOAuthHandler
	// Adjustments is the operator wallet surface (Task 14: 运营调整/冲正/
	// PAYG 发布配置); mounted under billing:adjust.
	Adjustments *AdminAdjustmentsHandler
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
	if o.Auth != nil && o.RequireAdmin != nil {
		o.Auth.RegisterOperators(g.Group("", o.RequireAdmin))
	}
	if o.Adjustments != nil && o.RequireBilling != nil {
		o.Adjustments.Register(g.Group("", o.RequireBilling))
	}
}
