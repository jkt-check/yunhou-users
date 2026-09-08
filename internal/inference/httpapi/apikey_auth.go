// apikey_auth.go — 客户 API Key 鉴权中间件与 Kaya JWT facade（Task 5）。
//
// /v1/* 标准协议入口由 APIKeyAuth 保护：Authorization: Bearer <key> →
// access.Resolver.Authenticate → 统一内部 principal（kind=api_key）放入
// gin context，并施加按 Key/账户的 RPM 限速。客户 Key 鉴权独立于运营
// 链（X-App-Secret 不对客户开放，设计 §9.2、Task 5 控制者决定）。
//
// /v1/* 使用协议原生错误形状（error.type/code），不套管理 envelope
// （设计 §9.1）。

package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/middleware"
)

// Caller context keys, set ONLY by the customer-facing auth middleware.
const (
	// ContextCallerPrincipal holds the unified internal principal
	// (*domain.Principal) — api_key kind for /v1/*, kaya_jwt for the
	// facade path. The Task 8 gateway consumes this single shape.
	ContextCallerPrincipal = "inference_caller_principal"
	// ContextCallerKey holds the authenticated *domain.APIKey (nil for
	// session principals) so the quota gate can apply the Key sub-budget.
	ContextCallerKey = "inference_caller_api_key"
)

// CallerPrincipalOf extracts the principal set by the auth middleware.
// A nil result means the route was mounted without authentication —
// handlers must fail closed on it.
func CallerPrincipalOf(c *gin.Context) *domain.Principal {
	if v, ok := c.Get(ContextCallerPrincipal); ok {
		if p, ok2 := v.(*domain.Principal); ok2 {
			return p
		}
	}
	return nil
}

// CallerKeyOf extracts the authenticated API key (nil on the facade path).
func CallerKeyOf(c *gin.Context) *domain.APIKey {
	if v, ok := c.Get(ContextCallerKey); ok {
		if k, ok2 := v.(*domain.APIKey); ok2 {
			return k
		}
	}
	return nil
}

// v1Error writes the /v1 native error shape — never the management
// envelope (设计 §9.1).
func v1Error(c *gin.Context, status int, typ, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{"message": message, "type": typ, "code": code},
	})
}

// APIKeyAuth authenticates /v1/* requests by customer API key and applies
// per-Key (when the key carries rpm_limit) and per-account (accountRPM,
// when > 0) sliding-window rate limits. Rate limiting runs AFTER
// authentication so buckets key on verified identities, not client input.
func APIKeyAuth(resolver *access.Resolver, counter *access.RPMCounter, accountRPM int) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
				"missing bearer API key")
			return
		}
		res, err := resolver.Authenticate(c.Request.Context(), strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			if domain.CodeOf(err) == domain.CodeInternal {
				v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
				return
			}
			v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
				"invalid, revoked or expired API key")
			return
		}
		if counter != nil {
			limited := false
			var scope string
			if res.Key.RPMLimit != nil {
				scope = "key:" + res.Key.ID
				limited = !counter.Allow(scope, *res.Key.RPMLimit)
			}
			if !limited && accountRPM > 0 {
				scope = "acct:" + res.Account.ID
				limited = !counter.Allow(scope, accountRPM)
			}
			if limited {
				if ra := counter.RetryAfter(scope); ra > 0 {
					c.Header("Retry-After", strconv.Itoa(int(ra.Seconds())+1))
				}
				v1Error(c, http.StatusTooManyRequests, "rate_limit_error", "rate_limited",
					"rate limit exceeded")
				return
			}
		}
		c.Set(ContextCallerPrincipal, res.Principal)
		c.Set(ContextCallerKey, res.Key)
		c.Next()
	}
}

// UserSessionPrincipal is the facade adapter middleware: it converts the
// server-verified Kaya JWT identity (middleware.ContextUserID, set by
// JWTAuth earlier in the chain) into the SAME internal principal shape
// the API-key path produces. The Task 8 gateway consumes ContextCallerPrincipal
// uniformly; this task owns the mapping itself.
func UserSessionPrincipal(resolver *access.Resolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.ContextUserID)
		p, err := resolver.ResolveUserSession(c.Request.Context(), userID)
		if err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound:
				// The user never established a billing account (no model
				// API purchase/grant yet) — fail closed, never create one
				// on a read path.
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"code": 403, "message": "no model billing account for this user",
				})
			case domain.CodeInvalidKey:
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"code": 403, "message": "billing account is not active",
				})
			default:
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code": 500, "message": "internal error",
				})
			}
			return
		}
		c.Set(ContextCallerPrincipal, p)
		c.Next()
	}
}
