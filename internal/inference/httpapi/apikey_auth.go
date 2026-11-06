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

// isAnthropicV1Path reports whether the request speaks the Anthropic
// protocol (POST /v1/messages). The Anthropic error taxonomy differs from
// the OpenAI one, so every renderer that has to pick a shape asks here.
func isAnthropicV1Path(path string) bool {
	return strings.HasSuffix(path, "/v1/messages")
}

// v1AuthError writes the auth-chain error in the shape of the protocol the
// route speaks: Anthropic-native for /v1/messages (设计 §9.1: 各自协议的
// 原生错误), the OpenAI /v1 shape elsewhere.
func v1AuthError(c *gin.Context, status int, typ, code, message string) {
	if isAnthropicV1Path(c.Request.URL.Path) {
		anthropicError(c, status, typ, message)
		return
	}
	v1Error(c, status, typ, code, message)
}

// V1RateLimitResponder renders the /v1 perimeter rejection (the per-IP abuse
// guard mounted in front of the /v1 group) in the shape of the protocol the
// route speaks: Anthropic-native for /v1/messages, the OpenAI /v1 error
// object elsewhere.
//
// It exists because the generic limiter middleware renders the MANAGEMENT
// envelope — which must never be forced onto /v1 (设计 §9.1: /v1/* 使用
// 各自协议的原生 body 与错误). The rejection happens before authentication
// and before any upstream call, so the response is the client's only signal.
func V1RateLimitResponder(c *gin.Context, retryAfter string) {
	if retryAfter != "" {
		c.Header("Retry-After", retryAfter)
	}
	if isAnthropicV1Path(c.Request.URL.Path) {
		anthropicError(c, http.StatusTooManyRequests, "rate_limit_error", "too many requests")
		return
	}
	v1Error(c, http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "too many requests")
}

// APIKeyAuth authenticates /v1/* requests by customer API key.
//
// Credentials are accepted as `Authorization: Bearer <key>` (OpenAI style)
// or `X-Api-Key: <key>` (Anthropic style — Task 13: Claude Code 等
// Anthropic SDK 客户端以 x-api-key 为默认凭据头). Both headers present:
// Bearer wins. The key VALUE space is identical.
//
// Task 4: the business rate limits (全局账户 RPM / 选中权益策略 RPM /
// Key RPM + 账户级策略 TPM) are NOT enforced here any more — they are
// enforced ONCE in the business admission (quota.Service.Admit), where the
// counters are shared through PostgreSQL across instances, across Keys and
// across /chat and /v1. The old process-local buckets were a duplicate,
// per-instance approximation of that limit: they under-counted globally
// (N instances ⇒ N× the limit) and they rejected requests that never
// reached the business admission (model not allowed, invalid shape).
//
// What stays here: **authentication** (the key must be valid) and the
// per-IP abuse guard mounted in front of the group (router.Setup). A
// request that fails authentication never enters the business admission,
// so it consumes neither RPM nor TPM.
func APIKeyAuth(resolver *access.Resolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		var rawKey string
		if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			rawKey = strings.TrimPrefix(auth, "Bearer ")
		} else if xkey := c.GetHeader("X-Api-Key"); xkey != "" {
			rawKey = xkey
		}
		if rawKey == "" {
			v1AuthError(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
				"missing bearer API key")
			return
		}
		res, err := resolver.Authenticate(c.Request.Context(), rawKey)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeInternal {
				v1AuthError(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
				return
			}
			v1AuthError(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
				"invalid, revoked or expired API key")
			return
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
