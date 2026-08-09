package handler

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
)

type AuthHandler struct {
	authSvc  service.AuthServiceInterface
	tokenSvc service.TokenServiceInterface
}

func NewAuthHandler(authSvc service.AuthServiceInterface, tokenSvc service.TokenServiceInterface) *AuthHandler {
	return &AuthHandler{authSvc: authSvc, tokenSvc: tokenSvc}
}

// expectedAuthErrors lists the service-level sentinel errors that map to a
// user-facing 401. Anything else is treated as internal and surfaced only as
// a generic 500 (the underlying detail goes to the log, not the client).
//
// Subscription-state sentinels used to live here — historically
// ErrSubscriptionExpired and ErrSubscriptionNotActive came back from
// findUsableSubscription inside LoginWithProfile and were translated into
// a URL `reason=subscription_expired` that the BFF rendered as a banner.
// That coupling was identified as the cn-staging 2026-07-23 incident's
// root cause: a user with an expired-but-past `expires_at` row was
// bounced off the login page and could not renew. Login is now an
// identity-layer concern; subscription state is reported in the
// /auth/me response as Subscription.HasAccess=false and surfaced via
// console banners instead. See service.AuthService.peekSubscription
// and docs/superpowers/specs/2026-07-23-login-subscription-decouple-design.md.
var expectedAuthErrors = []error{
	service.ErrInvalidProviderToken,
	service.ErrUnsupportedProvider,
	service.ErrInvalidRefreshToken,
	service.ErrUserNotFound,
	service.ErrUserSuspended,
	service.ErrUserDeleted,
	service.ErrAppNotFound,
	service.ErrAppInactive,
}

func isExpectedAuthErr(err error) bool {
	for _, target := range expectedAuthErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// authErrReason maps the service-layer auth sentinels to a short token
// safe to put in a URL fragment. Used by the GitHub and WeChat OAuth
// callbacks to tell the BFF which class of failure occurred without
// stranding the browser on a JSON error page. Returns "auth_failed"
// for anything we don't recognise.
func authErrReason(err error) string {
	switch {
	case errors.Is(err, service.ErrAppNotFound):
		return "app_not_found"
	case errors.Is(err, service.ErrAppInactive):
		return "app_disabled"
	case errors.Is(err, service.ErrUserNotFound), errors.Is(err, service.ErrUserDeleted):
		return "user_not_found"
	case errors.Is(err, service.ErrUserSuspended):
		return "user_suspended"
	case errors.Is(err, service.ErrWeChatUpstream):
		return "wechat_upstream"
	case errors.Is(err, service.ErrWeChatNoUnionID):
		return "wechat_no_unionid"
	default:
		return "auth_failed"
	}
}

func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
		AppID        string `json:"app_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	resp, err := h.authSvc.RefreshToken(c.Request.Context(), req.RefreshToken, req.AppID)
	if err != nil {
		if isExpectedAuthErr(err) {
			// Generic client message on purpose: the distinct sentinel
			// texts ("invalid refresh token", "user is suspended",
			// "user is deleted", ...) would disclose account state to
			// anyone holding a token. Details go to the server log.
			log.Printf("refresh rejected: %v", err)
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "refresh failed"})
			return
		}
		log.Printf("refresh internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "refresh failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}

func (h *AuthHandler) Logout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if err := h.authSvc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		// Logout is idempotent at the service layer (missing/expired
		// session returns nil). Any error that reaches us is either an
		// expected sentinel (e.g. ErrInvalidRefreshToken) — treat as
		// already-logged-out, return 200 — or an unexpected DB failure
		// worth a 500. Mirrors RefreshToken's mapping so the two
		// sibling endpoints don't disagree on the same input shape.
		if isExpectedAuthErr(err) {
			c.JSON(http.StatusOK, gin.H{"code": 0, "message": "logged out"})
			return
		}
		log.Printf("logout internal error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "logout failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "logged out"})
}

func (h *AuthHandler) JWKS(c *gin.Context) {
	c.JSON(http.StatusOK, h.tokenSvc.JWKS())
}

// TestLogin is a dev-only login endpoint that returns a real JWT
// without going through any OAuth provider. It exists so the L3
// PayPal integration suite (tests/e2e-ui) can issue authenticated
// requests against a locally-running backend whose GitHub OAuth
// verifier is wired to the real api.github.com (which 401s on
// forged tokens).
//
// Gated by PAYPAL_L3_E2E_MODE=1: if unset, every call returns 404.
// This keeps the endpoint off in production. The route is also
// never registered in router.Setup when the env is missing.
//
// POST /test/login?plan_id=monthly  body: {"email":"...","app_id":"yundian"}
// 200:        {access_token, refresh_token, user: {id, ...}}
//
// The requested plan must be active and accepting new subscriptions. It is
// used for the minted token's scope; user creation and refresh-session storage
// continue through AuthService.TestLogin.
func (h *AuthHandler) TestLogin(c *gin.Context) {
	if os.Getenv("PAYPAL_L3_E2E_MODE") != "1" {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
		return
	}

	planID := c.Query("plan_id")
	if planID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan_id is required"})
		return
	}

	var req struct {
		Email string `json:"email" binding:"required"`
		AppID string `json:"app_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	resp, err := h.authSvc.TestLogin(c.Request.Context(), service.TestLoginRequest{
		Email:  req.Email,
		AppID:  req.AppID,
		PlanID: planID,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPlanNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		case errors.Is(err, service.ErrPlanInactive):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is not available for test login"})
		case errors.Is(err, service.ErrPlanNotAcceptingNew):
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan is not accepting new subscriptions"})
		default:
			log.Printf("test login internal error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "test login failed"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}
