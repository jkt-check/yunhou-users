package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/repo"
)

const ContextApp = "app"

// InternalAppAuth validates app_id header for internal service-to-service calls.
// No secret required since it's used within the internal network.
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

		c.Set(ContextApp, app)
		c.Next()
	}
}
