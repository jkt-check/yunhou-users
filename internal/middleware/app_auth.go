package middleware

import (
	"log"
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
			// Don't differentiate "no such app" from "app disabled" —
			// the response code and message are the same as a wrong
			// secret, so an attacker can't enumerate X-App-ID values by
			// watching the response. Log the underlying reason for the
			// operator (it'd be visible in their dashboards).
			log.Printf("internal app auth: app %q lookup failed: %v", appID, err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app_secret",
			})
			return
		}

		if !app.IsActive {
			log.Printf("internal app auth: app %q disabled", appID)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid app_secret",
			})
			return
		}

		// secret_hash empty == app hasn't been backfilled yet (or row created
		// before migration 007). Refuse to authenticate rather than fall
		// through to the network-trust model — that is exactly the gap we
		// are closing here.
		//
		// Timing-oracle mitigation: an early-return on the empty-hash branch
		// would let an attacker measure response time to enumerate which
		// apps have been backfilled (no bcrypt call vs. real bcrypt call).
		// Always run bcrypt — against the real hash when present, against
		// util.DummyBcryptHash when not. The "missing X-App-Secret" branch
		// also runs the dummy comparison so the timing profile matches a
		// genuine secret mismatch. Both fail paths return the same
		// "invalid app_secret" message so the caller can't distinguish.
		hashToCheck := app.SecretHash
		secret := c.GetHeader("X-App-Secret")
		if hashToCheck == "" || secret == "" {
			hashToCheck = util.DummyBcryptHash
			secret = ""
		}
		if !util.CheckSecret(hashToCheck, secret) {
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
