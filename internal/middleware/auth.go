package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
)

const (
	ContextUserID = "user_id"
	ContextAppID  = "app_id"
	ContextScope  = "scope"
)

func JWTAuth(tokenSvc *service.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "missing or invalid authorization header",
			})
			return
		}

		tokenStr := strings.TrimPrefix(auth, "Bearer ")
		claims, err := tokenSvc.VerifyAccessToken(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid or expired token",
			})
			return
		}

		c.Set(ContextUserID, claims.Subject)
		c.Set(ContextAppID, claims.AppID)
		c.Set(ContextScope, claims.Scope)
		c.Next()
	}
}
