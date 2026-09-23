// models.go — GET /v1/models（Task 8，设计 §9.1）。
//
// Returns the caller-visible published models in the standard model-list
// shape — the management {code,data,message} envelope never touches the
// /v1 surface. Visibility = lifecycle active AND an enabled route to an
// active deployment AND a bound sellable price AND the caller's effective
// entitlement set (catalog.Service.ListPublishedModels 的 AccessCheck 挂
// 钩接权益模型集合；Key 的 model_allow 只能收窄不能放宽).
package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
)

// ModelsHandler serves GET /v1/models.
type ModelsHandler struct {
	Catalog  *catalog.Service
	Resolver *access.Resolver
}

// NewModelsHandler builds the handler.
func NewModelsHandler(cat *catalog.Service, resolver *access.Resolver) *ModelsHandler {
	return &ModelsHandler{Catalog: cat, Resolver: resolver}
}

// modelObject is the standard (OpenAI-compatible) model list entry.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // always "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// List handles GET /v1/models.
func (h *ModelsHandler) List(c *gin.Context) {
	p := CallerPrincipalOf(c)
	if p == nil {
		v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"missing caller principal (route mounted without auth)")
		return
	}
	// The caller's effective model set: entitlement union narrowed by the
	// key's model_allow (设计 §4.2).
	allowed, err := h.Resolver.AuthorizedModelIDs(c.Request.Context(), p, CallerKeyOf(c))
	if err != nil {
		v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
		return
	}
	allowSet := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		allowSet[id] = true
	}
	models, err := h.Catalog.ListPublishedModels(c.Request.Context(),
		func(_ context.Context, modelID string) (bool, error) { return allowSet[modelID], nil })
	if err != nil {
		v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
		return
	}
	data := make([]modelObject, 0, len(models))
	for _, m := range models {
		data = append(data, modelObject{
			ID: m.ID, Object: "model", Created: m.CreatedAt.Unix(), OwnedBy: "yunhou",
		})
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}
