// user_api_keys.go — /user/api-keys 客户自管端点（Task 5；设计 §9.2）。
//
// 管理 envelope（{code,data,message}）。创建、分页查询、更新权限/预算、
// 撤销；新 Key 的明文只在创建响应中出现一次，列表/详情永不携带 Key
// 材料（只有展示前缀）。所有权一律来自 JWT 中间件的服务端用户身份；
// 跨客户访问与不存在无差别（404），不泄漏存在性。

package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// AccessOps bundles the Task 5 customer-access surface for router.Setup:
// the /user/api-keys management handler (mounted under JWT auth) and the
// /v1/* API-key auth chain. nil fields leave the corresponding surface
// unmounted (fail closed), mirroring AdminOps.
type AccessOps struct {
	UserAPIKeys *UserAPIKeysHandler
	// V1Auth is the customer API-key authentication middleware for the
	// /v1 group. Task 8 registers the standard-protocol routes into that
	// group; the auth chain is fixed at mount time so no /v1 route can
	// exist without it.
	V1Auth gin.HandlerFunc
	// RPMCounter backs V1Auth's per-key/account buckets; the router
	// starts its janitor goroutine when set.
	RPMCounter *access.RPMCounter

	// V1Models / V1ChatCompletions are the Task 8 standard-protocol
	// endpoints; both require V1Auth (mounted into the authenticated /v1
	// group). Nil leaves the route unmounted (404) even when V1Auth exists.
	V1Models          *ModelsHandler
	V1ChatCompletions *ChatCompletionsHandler
	// V1Messages / V1Responses are the Task 13 programming-tool surfaces
	// (Anthropic Messages / OpenAI Responses native shapes); same mount
	// rule — nil = unmounted.
	V1Messages  *MessagesHandler
	V1Responses *ResponsesHandler

	// Task 11 customer read views (设计 §9.2): /user/model-quotas,
	// /user/model-usage/{summary,requests}, /user/model-subscriptions.
	// Mounted under the JWT-authenticated /user group; ownership derives
	// from the JWT identity only. Nil fields leave the surface unmounted
	// (fail closed).
	UserQuotas        *UserQuotasHandler
	UserUsage         *UserUsageHandler
	UserSubscriptions *UserSubscriptionsHandler

	// KayaChat, when non-nil, replaces the legacy chat service for
	// POST /chat (迁移开关 INFERENCE_KAYA_CHAT_GATEWAY 的接线点).
	KayaChat service.ChatStreamer
	// KayaChatModels serves GET /chat/models (Kaya 模型选择契约); mounted
	// only together with the facade.
	KayaChatModels *KayaModelsHandler
}

// UserAPIKeysHandler serves the customer key-management endpoints.
type UserAPIKeysHandler struct {
	keys *access.KeyService
}

func NewUserAPIKeysHandler(keys *access.KeyService) *UserAPIKeysHandler {
	return &UserAPIKeysHandler{keys: keys}
}

// Register mounts the endpoints on a JWT-authenticated group.
func (h *UserAPIKeysHandler) Register(g *gin.RouterGroup) {
	g.POST("/api-keys", h.Create)
	g.GET("/api-keys", h.List)
	g.GET("/api-keys/:id", h.Get)
	g.PATCH("/api-keys/:id", h.Update)
	g.DELETE("/api-keys/:id", h.Revoke)
}

// --- DTOs -------------------------------------------------------------------

// keyJSON is the management view of a key. Micro-credit amounts are
// decimal integer STRINGS (设计 §9.2: 避免 JS 大整数精度丢失). No field
// ever carries key material — prefix is for display/lookup correlation
// only.
type keyJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Prefix       string   `json:"prefix"`
	ModelIDs     []string `json:"model_ids"`
	BudgetMicros *string  `json:"budget_micros"`
	BudgetUsed   string   `json:"budget_used_micros"`
	RPMLimit     *int     `json:"rpm_limit"`
	Status       string   `json:"status"`
	ExpiresAt    *string  `json:"expires_at"`
	RevokedAt    *string  `json:"revoked_at"`
	LastUsedAt   *string  `json:"last_used_at"`
	CreatedAt    string   `json:"created_at"`
}

func toKeyJSON(k *domain.APIKey) keyJSON {
	out := keyJSON{
		ID:         k.ID,
		Name:       k.Name,
		Prefix:     k.Prefix,
		ModelIDs:   k.ModelAllow,
		BudgetUsed: strconv.FormatInt(int64(k.BudgetUsed), 10),
		RPMLimit:   k.RPMLimit,
		Status:     string(k.Status),
		CreatedAt:  k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if k.BudgetLimit != nil {
		s := strconv.FormatInt(int64(*k.BudgetLimit), 10)
		out.BudgetMicros = &s
	}
	out.ExpiresAt = rfc3339Ptr(k.ExpiresAt)
	out.RevokedAt = rfc3339Ptr(k.RevokedAt)
	out.LastUsedAt = rfc3339Ptr(k.LastUsedAt)
	return out
}

func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// --- create -----------------------------------------------------------------

type createKeyRequest struct {
	Name         string   `json:"name"`
	ModelIDs     []string `json:"model_ids"`
	BudgetMicros *string  `json:"budget_micros"`
	RPMLimit     *int     `json:"rpm_limit"`
	ExpiresAt    *string  `json:"expires_at"`
}

// Create POST /user/api-keys — the plaintext is in THIS response only.
func (h *UserAPIKeysHandler) Create(c *gin.Context) {
	var req createKeyRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	params, err := req.toParams()
	if err != nil {
		fail(c, err)
		return
	}
	created, err := h.keys.CreateKey(c.Request.Context(), userIDOf(c), params)
	if err != nil {
		fail(c, err)
		return
	}
	body := toKeyJSON(created.Key)
	ok(c, gin.H{
		// Returned exactly once; never stored, never recoverable.
		"key":                created.Plaintext,
		"id":                 body.ID,
		"name":               body.Name,
		"prefix":             body.Prefix,
		"model_ids":          body.ModelIDs,
		"budget_micros":      body.BudgetMicros,
		"budget_used_micros": body.BudgetUsed,
		"rpm_limit":          body.RPMLimit,
		"status":             body.Status,
		"expires_at":         body.ExpiresAt,
		"created_at":         body.CreatedAt,
	})
}

func (r createKeyRequest) toParams() (access.CreateParams, error) {
	p := access.CreateParams{Name: r.Name, ModelAllow: r.ModelIDs, RPMLimit: r.RPMLimit}
	if r.BudgetMicros != nil {
		v, err := parseMicrosString(*r.BudgetMicros)
		if err != nil {
			return p, err
		}
		p.BudgetMicros = &v
	}
	if r.ExpiresAt != nil {
		t, err := parseRFC3339(*r.ExpiresAt)
		if err != nil {
			return p, err
		}
		p.ExpiresAt = &t
	}
	return p, nil
}

// --- list / get -------------------------------------------------------------

// List GET /user/api-keys?limit=&offset= — paginated; never carries
// plaintext (the second read cannot recover the key, by construction).
func (h *UserAPIKeysHandler) List(c *gin.Context) {
	limit, offset, err := pageParams(c)
	if err != nil {
		fail(c, err)
		return
	}
	page, err := h.keys.ListKeys(c.Request.Context(), userIDOf(c), limit, offset)
	if err != nil {
		fail(c, err)
		return
	}
	items := make([]keyJSON, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, toKeyJSON(&page.Items[i]))
	}
	ok(c, gin.H{
		"items":  items,
		"total":  page.Total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *UserAPIKeysHandler) Get(c *gin.Context) {
	key, err := h.keys.GetKey(c.Request.Context(), userIDOf(c), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toKeyJSON(key))
}

// --- update -----------------------------------------------------------------

// updateKeyRequest tracks field PRESENCE: an absent field keeps the
// current value, an explicit JSON null clears an optional constraint.
type updateKeyRequest struct {
	fields map[string]json.RawMessage
}

var updateKeyAllowed = map[string]bool{
	"name": true, "model_ids": true, "budget_micros": true,
	"rpm_limit": true, "expires_at": true,
}

func (u *updateKeyRequest) toPatch() (access.KeyPatch, error) {
	var patch access.KeyPatch
	for name := range u.fields {
		if !updateKeyAllowed[name] {
			return patch, domain.NewError(domain.CodeInvalidInput, "unknown field "+name)
		}
	}
	if raw, ok := u.fields["name"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return patch, domain.NewError(domain.CodeInvalidInput, "name must be a string")
		}
		patch.Name = &v
	}
	if raw, ok := u.fields["model_ids"]; ok {
		if isJSONNull(raw) {
			empty := []string{}
			patch.ModelAllow = &empty
		} else {
			var v []string
			if err := json.Unmarshal(raw, &v); err != nil {
				return patch, domain.NewError(domain.CodeInvalidInput, "model_ids must be an array of strings or null")
			}
			patch.ModelAllow = &v
		}
	}
	if raw, ok := u.fields["budget_micros"]; ok {
		if isJSONNull(raw) {
			patch.ClearBudget = true
		} else {
			v, err := parseMicrosJSON(raw)
			if err != nil {
				return patch, err
			}
			patch.BudgetMicros = &v
		}
	}
	if raw, ok := u.fields["rpm_limit"]; ok {
		if isJSONNull(raw) {
			patch.ClearRPM = true
		} else {
			var v int
			if err := json.Unmarshal(raw, &v); err != nil {
				return patch, domain.NewError(domain.CodeInvalidInput, "rpm_limit must be an integer or null")
			}
			patch.RPMLimit = &v
		}
	}
	if raw, ok := u.fields["expires_at"]; ok {
		if isJSONNull(raw) {
			patch.ClearExpires = true
		} else {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return patch, domain.NewError(domain.CodeInvalidInput, "expires_at must be an RFC3339 string or null")
			}
			t, err := parseRFC3339(s)
			if err != nil {
				return patch, err
			}
			patch.ExpiresAt = &t
		}
	}
	return patch, nil
}

// Update PATCH /user/api-keys/:id — permission/budget lifecycle. Scope
// widening beyond the account entitlement is rejected by the service.
func (h *UserAPIKeysHandler) Update(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "unreadable body"))
		return
	}
	var req updateKeyRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req.fields); err != nil || req.fields == nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "body must be a JSON object"))
		return
	}
	patch, err := req.toPatch()
	if err != nil {
		fail(c, err)
		return
	}
	key, err := h.keys.UpdateKey(c.Request.Context(), userIDOf(c), c.Param("id"), patch)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toKeyJSON(key))
}

// --- revoke -----------------------------------------------------------------

// Revoke DELETE /user/api-keys/:id — idempotent; effective on the next
// /v1 call because resolution reads status from the store every time.
func (h *UserAPIKeysHandler) Revoke(c *gin.Context) {
	changed, err := h.keys.RevokeKey(c.Request.Context(), userIDOf(c), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"id": c.Param("id"), "revoked": changed})
}

// --- helpers ----------------------------------------------------------------

func userIDOf(c *gin.Context) string {
	return c.GetString(middleware.ContextUserID)
}

func pageParams(c *gin.Context) (limit, offset int, err error) {
	limit, offset = 50, 0
	if s := c.Query("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, domain.NewError(domain.CodeInvalidInput, "limit must be 1..100")
		}
	}
	if s := c.Query("offset"); s != "" {
		offset, err = strconv.Atoi(s)
		if err != nil || offset < 0 {
			return 0, 0, domain.NewError(domain.CodeInvalidInput, "offset must be >= 0")
		}
	}
	return limit, offset, nil
}

// parseMicrosString parses the decimal-integer-string credit contract
// (设计 §9.2).
func parseMicrosString(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, domain.NewError(domain.CodeInvalidInput, "budget_micros must be a decimal integer string")
	}
	return v, nil
}

// parseMicrosJSON accepts the string contract and, for client
// convenience, a bare JSON integer.
func parseMicrosJSON(raw json.RawMessage) (int64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseMicrosString(s)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, domain.NewError(domain.CodeInvalidInput, "budget_micros must be a decimal integer string")
	}
	return v, nil
}

func parseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, domain.NewError(domain.CodeInvalidInput, "expires_at must be RFC3339")
	}
	return t, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
