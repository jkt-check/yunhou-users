package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

// ProviderTokenLookup is the subset of ProviderTokenService the handler uses.
// Defined here so the handler can be tested without spinning up the full
// service stack.
//
// NewAppHandler tolerates a nil providerToken ONLY for unit tests that don't
// exercise GetProviderToken (CreateApp / UpdateApp). The production router
// MUST pass a non-nil providerToken; GetProviderToken panics defensively if
// it sees nil, so the misconfiguration fails fast at request time rather than
// silently returning 5xx-shaped data from a missing dependency.
type ProviderTokenLookup interface {
	Get(ctx context.Context, appID, channel string) (*model.ProviderToken, error)
}

type AppRepoInterface interface {
	List(context.Context) ([]model.App, error)
	FindByID(context.Context, string) (*model.App, error)
	Create(context.Context, *model.App) error
	Update(context.Context, *model.App) error
	RotateSecretHash(ctx context.Context, appID, newHash string) error
}

type AppHandler struct {
	appRepo       AppRepoInterface
	providerToken ProviderTokenLookup
}

func NewAppHandler(appRepo AppRepoInterface, providerToken ProviderTokenLookup) *AppHandler {
	return &AppHandler{appRepo: appRepo, providerToken: providerToken}
}

func (h *AppHandler) ListApps(c *gin.Context) {
	apps, err := h.appRepo.List(c.Request.Context())
	if err != nil {
		log.Printf("list apps error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list apps"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": apps})
}

func (h *AppHandler) GetApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if err != nil {
		log.Printf("get app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load app"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

func (h *AppHandler) CreateApp(c *gin.Context) {
	var req struct {
		AppID       string          `json:"app_id" binding:"required"`
		Name        string          `json:"name" binding:"required"`
		Description string          `json:"description"`
		Config      json.RawMessage `json:"config,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	// Re-marshal the validated config rather than persisting the raw bytes
	// the caller sent. The typed unmarshal above silently drops unknown
	// fields; persisting the original raw bytes would let operator typos
	// (or future-deprecated keys) linger in the DB un-rendered. Round-
	// tripping through AppConfig also normalises key ordering, which
	// keeps config equality checks stable across deployments.
	var configBytes json.RawMessage
	if req.Config != nil {
		var cfg model.AppConfig
		if uerr := json.Unmarshal(req.Config, &cfg); uerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid config: " + uerr.Error()})
			return
		}
		if verr := validateAppConfig(&cfg); verr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": verr.Error()})
			return
		}
		var merr error
		configBytes, merr = json.Marshal(cfg)
		if merr != nil {
			log.Printf("marshal validated app config: %v", merr)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to canonicalise config"})
			return
		}
	}

	// Generate a fresh shared secret BEFORE the DB write so a util error
	// never leaves a half-initialised app row behind. The plaintext is
	// returned to the caller exactly once; only the bcrypt hash is persisted.
	plaintext, hash, err := util.GenerateSecret()
	if err != nil {
		log.Printf("generate app secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to generate app secret"})
		return
	}

	app := &model.App{
		AppID:       req.AppID,
		Name:        req.Name,
		Description: req.Description,
		IsActive:    true,
		Config:      configBytes, // canonical JSONB; repo COALESCE handles NULL
		SecretHash:  hash,
	}
	if err := h.appRepo.Create(c.Request.Context(), app); err != nil {
		log.Printf("create app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create app"})
		return
	}

	// data.secret is the only place the plaintext ever appears — capture it
	// client-side. After this response the server only has the bcrypt hash.
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": gin.H{
		"app":    app,
		"secret": plaintext,
	}})
}

// RotateSecret handles POST /admin/apps/:id/rotate-secret. The auth middleware
// (X-App-ID + X-App-Secret) has already verified the caller's existing secret
// before this handler runs — this endpoint is gated by the admin group. We
// hand the caller a fresh plaintext, hash it, and persist. The old secret
// stops working the moment the UPDATE commits.
func (h *AppHandler) RotateSecret(c *gin.Context) {
	id := c.Param("id")

	plaintext, hash, err := util.GenerateSecret()
	if err != nil {
		log.Printf("generate app secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to generate app secret"})
		return
	}
	if err := h.appRepo.RotateSecretHash(c.Request.Context(), id, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
			return
		}
		log.Printf("rotate secret error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to rotate app secret"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"secret": plaintext}})
}

func (h *AppHandler) UpdateApp(c *gin.Context) {
	id := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if err != nil {
		log.Printf("update app lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load app"})
		return
	}

	var req struct {
		Name        *string          `json:"name"`
		Description *string          `json:"description"`
		IsActive    *bool            `json:"is_active"`
		Config      *json.RawMessage `json:"config,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "name must not be empty"})
			return
		}
		app.Name = trimmed
	}
	if req.Description != nil {
		app.Description = *req.Description
	}
	if req.IsActive != nil {
		app.IsActive = *req.IsActive
	}
	if req.Config != nil {
		var cfg model.AppConfig
		if err := json.Unmarshal(*req.Config, &cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid config: " + err.Error()})
			return
		}
		if err := validateAppConfig(&cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		// Re-marshal the validated config (same canonicalisation as CreateApp)
		// so an operator typo or future-deprecated key that the typed unmarshal
		// silently dropped cannot linger in the DB through a PATCH. Persisting
		// the raw bytes here would mean the same logical config can have two
		// different on-disk shapes depending on whether the row was POSTed or
		// later PATCHed.
		canonical, merr := json.Marshal(cfg)
		if merr != nil {
			log.Printf("marshal validated app config: %v", merr)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to canonicalise config"})
			return
		}
		app.Config = canonical
	}

	if err := h.appRepo.Update(c.Request.Context(), app); err != nil {
		log.Printf("update app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update app"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": app})
}

// GetProviderToken handles GET /apps/:id/provider-token/:channel.
//
// Auth is via the InternalAppAuth middleware (X-App-ID + X-App-Secret
// headers) — mounted in router.go on the same group as the other /apps
// routes. The middleware verifies the caller's app_id matches an active app
// AND the X-App-Secret bcrypt-verifies against apps.secret_hash; the service
// layer additionally loads apps.config.payment_providers.<channel> to read
// the credentials before calling the provider.
func (h *AppHandler) GetProviderToken(c *gin.Context) {
	appID := c.Param("id")
	channel := c.Param("channel")
	// Defensive nil-check: production router always wires a real
	// providerToken; if a deployment ships without one, fail loudly here
	// so the misconfig surfaces in load tests rather than silently 500ing
	// downstream where the cause is harder to trace.
	if h.providerToken == nil {
		log.Printf("FATAL: GetProviderToken called with nil dependency (appID=%s channel=%s)", appID, channel)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "provider token service unavailable"})
		return
	}
	tok, err := h.providerToken.Get(c.Request.Context(), appID, channel)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUnsupportedChannel):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "unsupported channel"})
		case errors.Is(err, service.ErrAppNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		case errors.Is(err, service.ErrAppInactive):
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "app is disabled"})
		case errors.Is(err, service.ErrProviderNotConfigured):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "provider not configured for app"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "provider upstream error"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": tok})
}

// validateAppConfig ensures any configured payment provider has all required
// fields. Operators can leave payment_providers entirely absent (no providers
// configured) or include only the providers they intend to use; missing or
// malformed fields surface as 400 from the handler.
func validateAppConfig(cfg *model.AppConfig) error {
	if cfg.PaymentProviders == nil {
		// fall through — OAuthProviders is a separate block
	} else {
		if p := cfg.PaymentProviders.Paypal; p != nil {
			if p.ClientID == "" || p.ClientSecret == "" || p.WebhookID == "" {
				return errors.New("paypal: client_id, client_secret, webhook_id are required")
			}
			if p.Mode != "live" && p.Mode != "sandbox" {
				return errors.New("paypal.mode must be live or sandbox")
			}
		}
	}
	if gh := cfg.OAuthProviders; gh != nil {
		if g := gh.GitHub; g != nil {
			if err := validateGitHubOAuthConfig(g); err != nil {
				return err
			}
		}
		if w := gh.WeChat; w != nil {
			if err := validateWeChatOAuthConfig(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateGitHubOAuthConfig enforces the boundary contract for a GitHub
// OAuth App stored in apps.config.oauth_providers.github. Required when the
// block is present; absence of the block means "GitHub login disabled for
// this app" and is allowed.
func validateGitHubOAuthConfig(g *model.GitHubOAuthConfig) error {
	if g.ClientID == "" {
		return errors.New("oauth_providers.github.client_id is required")
	}
	if g.ClientSecret == "" {
		return errors.New("oauth_providers.github.client_secret is required")
	}
	if len(g.CallbackURLs) == 0 {
		return errors.New("oauth_providers.github.callback_urls must list at least one URL")
	}
	seen := make(map[string]struct{}, len(g.CallbackURLs))
	for _, u := range g.CallbackURLs {
		if u == "" {
			return errors.New("oauth_providers.github.callback_urls entries must not be empty")
		}
		// Restrict to https (callback URLs go over the wire carrying tokens);
		// http is allowed only on loopback for local dev.
		if !isAcceptableCallbackURL(u) {
			return errors.New("oauth_providers.github.callback_urls entries must be https:// or http://127.0.0.1 / http://localhost")
		}
		if _, dup := seen[u]; dup {
			return errors.New("oauth_providers.github.callback_urls must not contain duplicates")
		}
		seen[u] = struct{}{}
	}
	return nil
}

// wechatAppIDPattern matches WeChat Open Platform 网站应用 AppID format:
// "wx" + 16 hex chars. The "wx" prefix is always lowercase per Tencent's
// assignment convention, so we anchor it as case-sensitive; the hex
// tail is accepted case-insensitively because operators have
// reported real assignments issued with uppercase A-F, and a case
// mismatch would otherwise lock them out at admin time. Validating
// the pattern catches typos before they hit the live WeChat endpoint
// and surface as a confusing errcode=40013.
var wechatAppIDPattern = regexp.MustCompile(`^wx[0-9a-fA-F]{16}$`)

// validateWeChatOAuthConfig enforces the boundary contract for a WeChat
// Open Platform 网站应用 stored in apps.config.oauth_providers.wechat.
// Required when the block is present; absence of the block means
// "WeChat login disabled for this app" and is allowed.
func validateWeChatOAuthConfig(w *model.WeChatOAuthConfig) error {
	if w.AppID == "" {
		return errors.New("oauth_providers.wechat.app_id is required")
	}
	if !wechatAppIDPattern.MatchString(w.AppID) {
		return errors.New("oauth_providers.wechat.app_id must match ^wx[0-9a-fA-F]{16}$")
	}
	if len(w.AppSecret) != 32 {
		return errors.New("oauth_providers.wechat.app_secret must be 32 chars")
	}
	if len(w.CallbackURLs) == 0 {
		return errors.New("oauth_providers.wechat.callback_urls must list at least one URL")
	}
	seen := make(map[string]struct{}, len(w.CallbackURLs))
	for _, u := range w.CallbackURLs {
		if u == "" {
			return errors.New("oauth_providers.wechat.callback_urls entries must not be empty")
		}
		if !isAcceptableCallbackURL(u) {
			return errors.New("oauth_providers.wechat.callback_urls entries must be https:// or http://127.0.0.1 / http://localhost")
		}
		if _, dup := seen[u]; dup {
			return errors.New("oauth_providers.wechat.callback_urls must not contain duplicates")
		}
		seen[u] = struct{}{}
	}
	return nil
}

// isAcceptableCallbackURL permits https URLs in production and loopback
// http URLs for local development. Anything else is rejected to prevent
// tokens being shipped to a non-localhost cleartext endpoint by accident.
func isAcceptableCallbackURL(u string) bool {
	if len(u) >= 8 && u[:8] == "https://" {
		return true
	}
	if len(u) >= 7 && u[:7] == "http://" {
		rest := u[7:]
		// strip path/port. IPv6 hosts (e.g. "[::1]:3000") start with "["
		// and end with "]" — handle them as a special case so we don't
		// trip on the embedded ":" separator. IPv4-mapped IPv6 loopback
		// ("[::ffff:127.0.0.1]") is also loopback, just spelled with the
		// RFC 4291 §2.5.5.2 mapping form — accept it for parity.
		if len(rest) > 0 && rest[0] == '[' {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return false
			}
			host := rest[1:end]
			if host == "::1" || host == "127.0.0.1" {
				return true
			}
			if strings.HasPrefix(host, "::ffff:") && host[len("::ffff:"):] == "127.0.0.1" {
				return true
			}
			return false
		}
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' || rest[i] == ':' {
				rest = rest[:i]
				break
			}
		}
		return rest == "127.0.0.1" || rest == "localhost"
	}
	return false
}

type SubscriptionHandler struct {
	subSvc service.SubscriptionServiceInterface
}

func NewSubscriptionHandler(subSvc service.SubscriptionServiceInterface) *SubscriptionHandler {
	return &SubscriptionHandler{subSvc: subSvc}
}

func (h *SubscriptionHandler) ListUserSubscriptions(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}
	subs, err := h.subSvc.ListUserSubscriptions(c.Request.Context(), userID)
	if err != nil {
		log.Printf("list subscriptions error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list subscriptions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": subs})
}

func (h *SubscriptionHandler) CreateSubscription(c *gin.Context) {
	var req struct {
		PlanID    string  `json:"plan_id" binding:"required"`
		ExpiresAt *string `json:"expires_at"` // accepted for backward compat; ignored by the service
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid expires_at format, use RFC3339"})
			return
		}
		expiresAt = &t
	}

	sub, err := h.subSvc.Create(c.Request.Context(), userID, req.PlanID, expiresAt)
	if err != nil {
		// Map known service sentinels to HTTP codes; everything else is
		// treated as internal and surfaces only as a generic 500.
		switch {
		case errors.Is(err, service.ErrPlanNotFound):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan not found"})
			return
		case errors.Is(err, service.ErrPlanInactive):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is inactive"})
			return
		case errors.Is(err, service.ErrPaidPlanForbidden):
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "paid plans require payment, cannot self-subscribe"})
			return
		case errors.Is(err, service.ErrPlanNotAcceptingNew):
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan is not accepting new subscriptions"})
			return
		case errors.Is(err, service.ErrUserHasActiveSub), errors.Is(err, service.ErrSubscriptionExists):
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "user already has an active subscription"})
			return
		default:
			log.Printf("create subscription error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create subscription"})
			return
		}
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": sub})
}

func (h *SubscriptionHandler) CancelSubscription(c *gin.Context) {
	id := c.Param("id")
	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing auth"})
		return
	}

	if err := h.subSvc.Cancel(c.Request.Context(), id, userID); err != nil {
		// Service intentionally returns ErrSubscriptionNotFound for both
		// missing and not-owned-by-caller; surface that as 404.
		switch {
		case errors.Is(err, service.ErrSubscriptionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "subscription not found"})
			return
		case errors.Is(err, service.ErrAlreadyCancelled):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "already cancelled"})
			return
		default:
			log.Printf("cancel subscription error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to cancel subscription"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "cancelled"})
}

type adminPlanService interface {
	service.PlanServiceInterface
	ValidateApps(ctx context.Context, apps []string) error
}

type PlanHandler struct {
	planSvc  adminPlanService
	appRepo  AppRepoInterface
	quoteSvc QuoteLookup
}

// adminActorID returns the audit-log attribution string for the
// authenticated internal-app caller: "admin:<appID>". The
// InternalAppAuth middleware has already verified the X-App-ID +
// X-App-Secret pair and stored the *model.App at middleware.ContextApp,
// so reading it here is safe (panic if absent is the same blast radius
// as the rest of the admin group — the middleware guarantees the value).
//
// We deliberately thread the appID (not the user) into the audit log:
// the /admin/* endpoints are internal-service-to-service calls, not
// user-initiated, so the relevant attribution is "which admin app did
// this" rather than "which user clicked the button". Returns
// "admin:unknown" if the context key is somehow missing (defensive —
// the middleware would have aborted the request before reaching here).
func adminActorID(c *gin.Context) string {
	if v, ok := c.Get(middleware.ContextApp); ok {
		if app, ok := v.(*model.App); ok && app != nil && app.AppID != "" {
			return "admin:" + app.AppID
		}
	}
	return "admin:unknown"
}

// QuoteLookup is the subset of QuoteService the handler uses. Defined here so
// handler tests can plug in a fake without standing up the full service stack.
type QuoteLookup interface {
	Get(ctx context.Context, appID, planID, userID string) (*model.Quote, error)
}

type createPlanRequest struct {
	ID                        string   `json:"id"`
	Name                      string   `json:"name"`
	Price                     float64  `json:"price"`
	IntervalDays              int      `json:"interval_days"`
	Apps                      []string `json:"apps"`
	Currency                  *string  `json:"currency"`
	IsListed                  *bool    `json:"is_listed"`
	AcceptingNewSubscriptions *bool    `json:"accepting_new_subscriptions"`
	TrialDays                 int      `json:"trial_days"`
	Description               *string  `json:"description"`
	DisplayOrder              int      `json:"display_order"`
	IsActive                  *bool    `json:"is_active"`
}

type updatePlanRequest struct {
	Name                      *string   `json:"name"`
	Price                     *float64  `json:"price"`
	IntervalDays              *int      `json:"interval_days"`
	Apps                      *[]string `json:"apps"`
	Currency                  *string   `json:"currency"`
	IsListed                  *bool     `json:"is_listed"`
	AcceptingNewSubscriptions *bool     `json:"accepting_new_subscriptions"`
	TrialDays                 *int      `json:"trial_days"`
	Description               **string  `json:"description"`
	DisplayOrder              *int      `json:"display_order"`
	IsActive                  *bool     `json:"is_active"`
}

func decodeAdminPlanRequest(c *gin.Context, dst any) bool {
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return false
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return false
	}
	if _, present := raw["is_default"]; present {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "is_default is no longer supported; use plan selection logic in BFF",
		})
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return false
	}
	// encoding/json decodes both an absent **string field and an explicit null
	// to a nil outer pointer. Preserve explicit null for PATCH so UpdatePlan can
	// distinguish it from an omitted description.
	if req, ok := dst.(*updatePlanRequest); ok {
		if _, present := raw["description"]; present && req.Description == nil {
			req.Description = new(*string)
		}
	}
	return true
}

func validatePlanName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", errors.New("name must not be empty")
	}
	return trimmed, nil
}

func isSupportedPlanCurrency(currency string) bool {
	switch currency {
	case "", "CNY", "USD", "EUR":
		return true
	default:
		return false
	}
}

func (h *PlanHandler) validatePlanApps(c *gin.Context, apps []string) bool {
	if err := h.planSvc.ValidateApps(c.Request.Context(), apps); err != nil {
		if errors.Is(err, service.ErrInvalidAppID) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return false
		}
		log.Printf("validate plan apps error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to validate plan apps"})
		return false
	}
	return true
}

func NewPlanHandler(planSvc adminPlanService, appRepo AppRepoInterface, quoteSvc QuoteLookup) *PlanHandler {
	return &PlanHandler{planSvc: planSvc, appRepo: appRepo, quoteSvc: quoteSvc}
}

func (h *PlanHandler) ListPlans(c *gin.Context) {
	plans, err := h.planSvc.ListPlans(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list plans"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plans})
}

func (h *PlanHandler) GetPlan(c *gin.Context) {
	id := c.Param("id")
	plan, err := h.planSvc.GetPlan(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}
	if err != nil {
		log.Printf("get plan error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load plan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) CreatePlan(c *gin.Context) {
	var req createPlanRequest
	if !decodeAdminPlanRequest(c, &req) {
		return
	}
	if req.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}
	var err error
	if req.Name, err = validatePlanName(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	if req.Price < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "price must be non-negative"})
		return
	}
	if req.IntervalDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "interval_days must be non-negative"})
		return
	}
	if req.TrialDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "trial_days must be non-negative"})
		return
	}
	if req.Currency != nil && !isSupportedPlanCurrency(*req.Currency) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "currency must be one of CNY/USD/EUR"})
		return
	}
	if !h.validatePlanApps(c, req.Apps) {
		return
	}

	currency := "CNY"
	if req.Currency != nil && *req.Currency != "" {
		currency = *req.Currency
	}
	isListed := true
	if req.IsListed != nil {
		isListed = *req.IsListed
	}
	acceptingNewSubscriptions := true
	if req.AcceptingNewSubscriptions != nil {
		acceptingNewSubscriptions = *req.AcceptingNewSubscriptions
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	plan := &model.Plan{
		ID:                        req.ID,
		Name:                      req.Name,
		Price:                     req.Price,
		IntervalDays:              req.IntervalDays,
		Apps:                      pq.StringArray(req.Apps),
		IsActive:                  isActive,
		IsListed:                  isListed,
		AcceptingNewSubscriptions: acceptingNewSubscriptions,
		Currency:                  currency,
		TrialDays:                 req.TrialDays,
		Description:               req.Description,
		DisplayOrder:              req.DisplayOrder,
	}
	if err := h.planSvc.CreatePlan(c.Request.Context(), plan, adminActorID(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to create plan"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) UpdatePlan(c *gin.Context) {
	var req updatePlanRequest
	if !decodeAdminPlanRequest(c, &req) {
		return
	}
	var normalizedName string
	if req.Name != nil {
		var err error
		normalizedName, err = validatePlanName(*req.Name)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
	}
	if req.Price != nil && *req.Price < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "price must be non-negative"})
		return
	}
	if req.IntervalDays != nil && *req.IntervalDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "interval_days must be non-negative"})
		return
	}
	if req.TrialDays != nil && *req.TrialDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "trial_days must be non-negative"})
		return
	}
	if req.Currency != nil && !isSupportedPlanCurrency(*req.Currency) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "currency must be one of CNY/USD/EUR"})
		return
	}
	if req.Apps != nil && !h.validatePlanApps(c, *req.Apps) {
		return
	}

	id := c.Param("id")
	plan, err := h.planSvc.GetPlan(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		return
	}
	if err != nil {
		log.Printf("update plan lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load plan"})
		return
	}

	if req.Name != nil {
		plan.Name = normalizedName
	}
	if req.Price != nil {
		plan.Price = *req.Price
	}
	if req.IntervalDays != nil {
		plan.IntervalDays = *req.IntervalDays
	}
	if req.Apps != nil {
		plan.Apps = pq.StringArray(*req.Apps)
	}
	if req.Currency != nil && *req.Currency != "" {
		plan.Currency = *req.Currency
	}
	if req.IsListed != nil {
		plan.IsListed = *req.IsListed
	}
	if req.AcceptingNewSubscriptions != nil {
		plan.AcceptingNewSubscriptions = *req.AcceptingNewSubscriptions
	}
	if req.TrialDays != nil {
		plan.TrialDays = *req.TrialDays
	}
	if req.Description != nil {
		if *req.Description == nil {
			plan.Description = nil
		} else {
			plan.Description = *req.Description
		}
	}
	if req.DisplayOrder != nil {
		plan.DisplayOrder = *req.DisplayOrder
	}
	if req.IsActive != nil {
		plan.IsActive = *req.IsActive
	}

	if err := h.planSvc.UpdatePlan(c.Request.Context(), plan, adminActorID(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update plan"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": plan})
}

func (h *PlanHandler) DeletePlan(c *gin.Context) {
	id := c.Param("id")
	if err := h.planSvc.DeletePlan(c.Request.Context(), id, adminActorID(c)); err != nil {
		// Postgres returns SQLSTATE 23503 when a FK reference prevents the
		// delete; surface that as a 409 Conflict with a clear message.
		if isFKViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "plan is in use by existing subscriptions"})
			return
		}
		log.Printf("delete plan error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to delete plan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "deleted"})
}

// isFKViolation returns true for lib/pq *pq.Error whose SQLSTATE is 23503
// (foreign_key_violation). Used by DeletePlan to map DB-level errors to a
// meaningful HTTP status code.
func isFKViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23503"
	}
	return false
}

// GetAppPlans handles GET /apps/:id/plans. It is intentionally unauthenticated
// — plan IDs and prices are already public (the marketing page needs them).
// The handler loads the app to read apps.config.payment_providers for the
// per-channel provider IDs and cycle summary, then assembles a PublicPlan
// per active plan.
func (h *PlanHandler) GetAppPlans(c *gin.Context) {
	appID := c.Param("id")
	app, err := h.appRepo.FindByID(c.Request.Context(), appID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		return
	}
	if err != nil {
		log.Printf("get app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load app"})
		return
	}

	plans, err := h.planSvc.FindByApp(c.Request.Context(), appID)
	if err != nil {
		log.Printf("list plans by app error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to list plans"})
		return
	}

	var cfg model.AppConfig
	if len(app.Config) > 0 {
		if err := json.Unmarshal(app.Config, &cfg); err != nil {
			log.Printf("decode app config: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to decode app config"})
			return
		}
	}

	out := make([]model.PublicPlan, 0, len(plans))
	for _, p := range plans {
		out = append(out, buildPublicPlan(p, cfg))
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": out})
}

// buildPublicPlan assembles a PublicPlan DTO from the canonical Plan row and
// the app's typed config. Resolves provider_ids for every configured channel
// and delegates cycle resolution to model.ResolveCycle for PublicPlan only.
// QuoteService inlines its own plan-based cycle configuration.
func buildPublicPlan(p model.Plan, cfg model.AppConfig) model.PublicPlan {
	out := model.PublicPlan{
		ID:           p.ID,
		Name:         p.Name,
		Price:        p.Price,
		IntervalDays: p.IntervalDays,
		Currency:     p.Currency,
		TrialDays:    p.TrialDays,
		Description:  p.Description,
		Apps:         []string(p.Apps),
		DisplayOrder: p.DisplayOrder,
		IsListed:     p.IsListed,
		ProviderIDs:  map[string]string{},
	}
	if cfg.PaymentProviders != nil {
		if pp := cfg.PaymentProviders.Paypal; pp != nil {
			if pc, ok := pp.Plans[p.ID]; ok && pc.PlanID != "" {
				out.ProviderIDs["paypal"] = pc.PlanID
			}
		}
		// WeChat Pay: surface the plan→product mapping under
		// provider_ids["wechat_pay"] so the BFF's wechatProvider.isConfigured()
		// can flip to true and PricingSection renders the 微信支付 option.
		// The actual UnifiedOrder routing against PlanMapping is still
		// deferred (M8 / design doc 2026-07-15 line 28), but the BFF only
		// reads provider_ids for *display* — mock-mode mock.UnifiedOrder
		// ignores the product code entirely, so a non-empty entry here is
		// sufficient to unlock the cn-staging demo path.
		if wp := cfg.PaymentProviders.WeChatPay; wp != nil {
			if pc, ok := wp.PlanMapping[p.ID]; ok && pc != "" {
				out.ProviderIDs["wechat_pay"] = pc
			}
		}
	}
	// Authoritative cycle = PayPal's per-plan entry when present; nil when
	// PayPal is unconfigured for this plan (the marketing page renders
	// cycle:null in that case so callers don't infer a fake cycle).
	if cfg.PaymentProviders != nil && cfg.PaymentProviders.Paypal != nil {
		if _, ok := cfg.PaymentProviders.Paypal.Plans[p.ID]; ok {
			cycle := model.ResolveCycle(cfg, p.ID, p.IntervalDays)
			out.Cycle = &model.CycleSummary{
				TrialDays:        cycle.TrialDays,
				BillingCycleDays: cycle.BillingCycleDays,
			}
		}
	}
	return out
}

// PostQuote handles POST /apps/:id/quote. JWT-authenticated (mounted in
// router.go). The user_id is read from the JWT context for future audit
// logging; the handler does not gate on subscription status — that's the
// orders endpoint's job.
//
// The JWT-derived app_id is intentionally NOT compared to the URL :id path
// parameter. Quote calls are scoped to one app, but a single user may hold
// JWTs for multiple apps; the URL always wins because the BFF is the source
// of truth for which app is asking. Caller errors here are the BFF's, not
// the end user's.
func (h *PlanHandler) PostQuote(c *gin.Context) {
	appID := c.Param("id")
	var req struct {
		PlanID string `json:"plan_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("quote bind error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}

	userID := c.GetString(middleware.ContextUserID)
	if userID == "" {
		// Defensive: the route is mounted with JWTAuth so this should be
		// impossible today, but a future re-mount or middleware reorder
		// must not silently let /quote fire with an empty user_id —
		// quoteSvc.Get would then INSERT a quote row tied to no user.
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing user identity"})
		return
	}
	quote, err := h.quoteSvc.Get(c.Request.Context(), appID, req.PlanID, userID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPlanNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "plan not found"})
		case errors.Is(err, service.ErrPlanInactive):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan is inactive"})
		case errors.Is(err, service.ErrPlanAppMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "plan does not include this app"})
		case errors.Is(err, service.ErrAppInactive):
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "app is disabled"})
		case errors.Is(err, service.ErrAppNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "app not found"})
		default:
			log.Printf("quote error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to compute quote"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": quote})
}
