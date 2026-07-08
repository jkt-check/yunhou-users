package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
)

const ContextApp = "app"

// InternalAppAuth validates X-App-ID + X-App-Secret headers for internal
// service-to-service calls. v1 dropped the secret in favour of network-layer
// isolation (apps.secret was removed in 002_simplify_plans), but v2's public
// deploy has no VPC / IP whitelist — see deploy/nginx.conf — so we bring
// secret auth back. See migrations/007_app_secret.sql + the integration guide
// §"App 接口" / §"频率限制" for context.
func InternalAppAuth(appRepo repo.AppRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		appID := c.GetHeader("X-App-ID")
		if appID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "missing X-App-ID header",
			})
			return
		}

		app, err := appRepo.FindByID(c.Request.Context(), appID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app_id",
			})
			return
		}

		if !app.IsActive {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"message": "app is disabled",
			})
			return
		}

		// secret_hash empty == app hasn't been backfilled yet (or row created
		// before migration 007). Refuse to authenticate rather than fall
		// through to the network-trust model — that is exactly the gap we
		// are closing here.
		if app.SecretHash == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "app secret not initialized",
			})
			return
		}

		secret := c.GetHeader("X-App-Secret")
		if secret == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "missing X-App-Secret header",
			})
			return
		}
		// CheckSecret uses bcrypt.CompareHashAndPassword, which is
		// constant-time on the hash and the user-supplied plaintext.
		if !util.CheckSecret(app.SecretHash, secret) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app_secret",
			})
			return
		}

		c.Set(ContextApp, app)
		c.Next()
	}
}
