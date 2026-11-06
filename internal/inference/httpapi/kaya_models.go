// kaya_models.go — GET /chat/models（Task 8，设计 §9.1：Kaya 模型选择契约）。
//
// 返回形状对齐候选分支的既有契约：{code:0, data:{models:[{id,
// display_name, provider, default, input_modalities}]}}（管理 envelope ——
// /chat 是 Kaya 面，不是 /v1 标准协议面）。内容 = 调用者权益可见且已发布的
// 模型集合（与 /v1/models 同一判定：catalog.ListPublishedModels + 权益
// AccessCheck）。input_modalities 供 picker 判定哪些模型允许发图；空值按
// ["text","image"] 透出（缺省放行，与视觉门控同口径——显式 ["text"] 才是
// 纯文本）。无计费账户 = 无有效订阅
// → 403 ErrChatNoAccess 形状（N4a，对齐 legacy GetModels/writeChatError），
// 不是 200 空列表。
//
// 该路由只在 /chat 网关迁移开关启用时挂载（见 router）。

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/middleware"
)

// KayaModelsHandler serves GET /chat/models for the Kaya model picker.
type KayaModelsHandler struct {
	Catalog      *catalog.Service
	Resolver     *access.Resolver
	Snapshots    gateway.SnapshotSource
	DefaultModel string
	// Capabilities answers "can this published model serve image input at
	// all?" — the routing package's capability decision, i.e. the SAME one
	// the dispatch path uses (T8). The listing must not advertise an image
	// the dispatcher refuses: a model whose only published deployment is an
	// openai_responses one cannot carry images, so the picker must not
	// offer them. Defaults to the production adapter registry; cmd/server
	// overrides it with the routing service so both use ONE adapter set.
	//
	// T9: the oracle also answers the tools/thinking questions, so the
	// listing can describe the model's real capability instead of
	// guessing from the declaration.
	Capabilities routing.CapabilityOracle
}

// NewKayaModelsHandler builds the handler; defaultModel marks the entry the
// client should preselect. snapshots is the readiness-gate/snapshot pin
// source — the shared SnapshotCache, so cold start carries
// ErrNoVerifiedSnapshot (评审裁定：不得用 Catalog.LoadSnapshot，它绕开 cache、
// 哨兵不可达且每请求全量 SELECT+ParseSnapshot；与 facade 同一来源)。
func NewKayaModelsHandler(cat *catalog.Service, resolver *access.Resolver, snapshots gateway.SnapshotSource, defaultModel string) *KayaModelsHandler {
	return &KayaModelsHandler{
		Catalog: cat, Resolver: resolver, Snapshots: snapshots, DefaultModel: defaultModel,
		Capabilities: routing.ProductionCapabilities(),
	}
}

// capabilityOracle returns the capability oracle to use (never nil).
func (h *KayaModelsHandler) capabilityOracle() routing.CapabilityOracle {
	if h.Capabilities != nil {
		return h.Capabilities
	}
	return routing.ProductionCapabilities()
}

// kayaModelEntry is the candidate-branch return shape (对齐基准).
type kayaModelEntry struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	Provider        string   `json:"provider"`
	Default         bool     `json:"default"`
	InputModalities []string `json:"input_modalities"`
	// --- T9 追加：当前入口（已发布条目 + 路由）与 adapter 的真实能力 ---
	// SupportsImages 与 input_modalities 一致（模型声明 ∧ 派发能力）。
	SupportsImages bool `json:"supports_images"`
	// SupportsTools / SupportsThinking 是**派发判定**：有一条已发布路由
	// 的能力清单覆盖它、且该协议的适配层承载它。绝不声明超过入口能力。
	SupportsTools    bool `json:"supports_tools"`
	SupportsThinking bool `json:"supports_thinking"`
	// ContextTokens / MaxOutputTokens 是目录条目上的硬上限（准入时强制
	// 生效的上界），不是估计值。
	ContextTokens   int `json:"context_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
	// CapabilityDescription 是一句话人话描述（给人看；机器读上面的字段）。
	CapabilityDescription string `json:"capability_description,omitempty"`
}

// List handles GET /chat/models.
func (h *KayaModelsHandler) List(c *gin.Context) {
	// N2 就绪闸门：目录冷启动无已验证快照 → 503 + Retry-After（服务未就绪），
	// 先于 ResolveUserSession——与「已发布但无权限」（N4a 403）严格分层；
	// 新库无 revision 时无账户用户也拿 503（验收 §7.2）。非哨兵错误走 fail()
	// 口径（500），不得伪装成冷启动。快照 pin 一次复用到下方条目组装。
	snap, err := h.Snapshots.Current(c.Request.Context())
	if err != nil {
		if errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
			c.Header("Retry-After", "5")
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "data": nil, "message": "chat catalog is not ready"})
			return
		}
		fail(c, err)
		return
	}
	userID := c.GetString(middleware.ContextUserID)
	p, err := h.Resolver.ResolveUserSession(c.Request.Context(), userID)
	if err != nil {
		// N4a（E6 决策，推翻原 200 空数组设计）：无计费账户 = 无有效订阅，
		// 对齐 legacy 403 ErrChatNoAccess —— picker 对 403 已有处理（隐藏
		// 选择器），而 200 空数组会让 picker 静默消失且与服务故障无法区分。
		if domain.CodeOf(err) == domain.CodeNotFound {
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "data": nil, "message": "active subscription with access to this app is required"})
			return
		}
		// 其余错误（DB 故障、账户停用等）必须响亮报错（评审轮1 m5）：
		// 渲染成成功空列表会把故障伪装成「用户无模型」。统一走包内 fail()
		// 口径（评审轮2 C-M2）：CodeInvalidKey（账户停用）→ 401、其余→500，
		// 响应带全局 envelope 的 "data":null 键，形状与 /admin、/user 面一致。
		log.Printf("chat/models: resolve user session: %v", err)
		fail(c, err)
		return
	}
	allowed, err := h.Resolver.AuthorizedModelIDs(c.Request.Context(), p, nil)
	if err != nil {
		fail(c, err)
		return
	}
	allowSet := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		allowSet[id] = true
	}
	models, err := h.Catalog.ListPublishedModels(c.Request.Context(),
		func(_ context.Context, modelID string) (bool, error) { return allowSet[modelID], nil })
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]kayaModelEntry, 0, len(models))
	capability := h.capabilityOracle() // 一次解析（每请求一次），不逐模型取
	for _, m := range models {
		provider := ""
		if deps := snap.ActiveDeployments(m.ID); len(deps) > 0 {
			if pv, ok := snap.Providers[deps[0].ProviderID]; ok {
				provider = pv.Code
			}
		}
		// T8: 透出的 input_modalities 必须与**派发**能力一致 —— 声明了
		// image 但已发布部署没有能承载图片的协议时，不再 advertise image
		// （否则 picker 让客户发图，网关在准入前当场 400）。判据来自
		// routing 的同一个能力判定，不另立规则。
		canServeImages := capability.SupportsImageInput(snap, m.ID)
		modalities := domain.AdvertisedModalities(m.InputModalities, canServeImages)
		// T9: 追加的能力描述同样是**派发判定**（工具/思考），绝不声明
		// 超过当前入口与 adapter 的能力；上下文与输出上限取目录条目上的
		// 硬上限（准入时强制生效的那两个数）。
		supportsImages := false
		for _, mod := range modalities {
			if mod == "image" {
				supportsImages = true
			}
		}
		supportsTools := capability.SupportsTools(snap, m.ID)
		supportsThinking := capability.SupportsThinking(snap, m.ID)
		out = append(out, kayaModelEntry{
			ID: m.ID, DisplayName: m.DisplayName, Provider: provider,
			Default:          m.ID == h.DefaultModel,
			InputModalities:  modalities,
			SupportsImages:   supportsImages,
			SupportsTools:    supportsTools,
			SupportsThinking: supportsThinking,
			ContextTokens:    m.ContextTokens,
			MaxOutputTokens:  m.MaxOutputTokens,
			CapabilityDescription: capabilityDescription(supportsImages, supportsTools, supportsThinking,
				m.ContextTokens, m.MaxOutputTokens),
		})
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"models": out}})
}

// capabilityDescription renders the one-line capability description of one
// published entry (human-facing; the machine-readable fields are the flat
// booleans and the two hard limits).
func capabilityDescription(images, tools, thinking bool, contextTokens, maxOutputTokens int) string {
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprintf("images: %s; tools: %s; thinking: %s; context: %d tokens; max output: %d tokens",
		yes(images), yes(tools), yes(thinking), contextTokens, maxOutputTokens)
}
