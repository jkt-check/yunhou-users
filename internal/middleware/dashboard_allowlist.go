package middleware

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
)

// DashboardAllowlist gates the dashboard 运营 surface (/admin/ops/metrics,
// /admin/users/*) to the app IDs in the DASHBOARD_APP_IDS allowlist
// (audit I-2). The ancestor adminGroup chain (InternalAppAuth) proves the
// caller holds *a* valid app secret, but every app secret is equally
// powerful — without this check any leaked internal secret could search
// user emails (PII) and mint VIP time.
//
// This middleware runs AFTER InternalAppAuth, so ContextApp is always set
// here; a missing app is treated as "not allowed" (fail closed) rather
// than panicking. An empty allowlist denies every app — the same fail-
// closed default as the cmd/server startup refusal, so a test or
// alternate main that mounts the router without configuring the list can
// never open the surface by accident.
//
// Authenticated-but-not-allowed is a 403, not a 404: hiding the route
// would suggest "not mounted" to operators while the audit question is
// "which app tried", and the route's existence is not a secret.
func DashboardAllowlist(allowed []string) gin.HandlerFunc {
	set := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	return func(c *gin.Context) {
		var appID string
		if v, ok := c.Get(ContextApp); ok {
			if app, ok := v.(*model.App); ok && app != nil {
				appID = app.AppID
			}
		}
		if appID == "" || !set[appID] {
			log.Printf("dashboard allowlist: app %q denied access to dashboard ops surface", appID)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"message": "app is not permitted to use the dashboard ops surface",
			})
			return
		}
		c.Next()
	}
}
