package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
)

const ContextApp = "app"

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
			util.CheckSecret(util.DummyBcryptHash, appSecret)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app credentials",
			})
			return
		}

		if !util.CheckSecret(app.Secret, appSecret) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app credentials",
			})
			return
		}

		c.Set(ContextApp, app)
		c.Next()
	}
}
