package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
)

func AppAuth(appRepo repo.AppRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		appID := c.GetHeader("X-App-ID")
		appSecret := c.GetHeader("X-App-Secret")
		if appID == "" || appSecret == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "missing app_id or app_secret",
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

		if !util.CheckSecret(app.Secret, appSecret) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app_secret",
			})
			return
		}

		c.Set("app", app)
		c.Next()
	}
}
