// kaya_models.go — GET /chat/models（Task 8，设计 §9.1：Kaya 模型选择契约）。
//
// 返回形状对齐候选分支的既有契约：{code:0, data:{models:[{id,
// display_name, provider, default}]}}（管理 envelope —— /chat 是 Kaya 面，
// 不是 /v1 标准协议面）。内容 = 调用者权益可见且已发布的模型集合（与
// /v1/models 同一判定：catalog.ListPublishedModels + 权益 AccessCheck）。
//
// 该路由只在 /chat 网关迁移开关启用时挂载（见 router）。

package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/middleware"
)

// KayaModelsHandler serves GET /chat/models for the Kaya model picker.
type KayaModelsHandler struct {
	Catalog      *catalog.Service
	Resolver     *access.Resolver
	DefaultModel string
}

// NewKayaModelsHandler builds the handler; defaultModel marks the entry the
// client should preselect.
func NewKayaModelsHandler(cat *catalog.Service, resolver *access.Resolver, defaultModel string) *KayaModelsHandler {
	return &KayaModelsHandler{Catalog: cat, Resolver: resolver, DefaultModel: defaultModel}
}

// kayaModelEntry is the candidate-branch return shape (对齐基准).
type kayaModelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	Default     bool   `json:"default"`
}

// List handles GET /chat/models.
func (h *KayaModelsHandler) List(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	p, err := h.Resolver.ResolveUserSession(c.Request.Context(), userID)
	if err != nil {
		// No model billing account → the picker legitimately shows an empty
		// list (the user never purchased/received a model grant), not an
		// error — same UX as "no models available".
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"models": []kayaModelEntry{}}})
		return
	}
	allowed, err := h.Resolver.AuthorizedModelIDs(c.Request.Context(), p, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal error"})
		return
	}
	allowSet := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		allowSet[id] = true
	}
	models, err := h.Catalog.ListPublishedModels(c.Request.Context(),
		func(_ context.Context, modelID string) (bool, error) { return allowSet[modelID], nil })
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal error"})
		return
	}
	snap, err := h.Catalog.LoadSnapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "internal error"})
		return
	}
	out := make([]kayaModelEntry, 0, len(models))
	for _, m := range models {
		provider := ""
		if deps := snap.ActiveDeployments(m.ID); len(deps) > 0 {
			if pv, ok := snap.Providers[deps[0].ProviderID]; ok {
				provider = pv.Code
			}
		}
		out = append(out, kayaModelEntry{
			ID: m.ID, DisplayName: m.DisplayName, Provider: provider,
			Default: m.ID == h.DefaultModel,
		})
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"models": out}})
}
