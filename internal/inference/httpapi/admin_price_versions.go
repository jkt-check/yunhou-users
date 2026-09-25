// admin_price_versions.go — 售价版本（inference_price_versions）运营端点
// （spec: docs/superpowers/specs/2026-09-25-admin-price-versions-design.md）。
//
//   POST /admin/price-versions — 创建价格版本（只追加；unit 服务端按 kind
//     派生，不入请求；revision 自然键幂等：同内容重放 / 同 revision 冲突
//     内容均 409 且 data 带已存在视图，message 区分 duplicate/conflict）；
//     写 + 审计同事务（action price_version.create）。
//   GET  /admin/price-versions — 列表（model_id/kind 过滤；limit 默认 100
//     钳 500，offset <0 → 400；排序 (model_id, kind, revision DESC)）。
//
// 全部挂在 models:manage 授权组（目录/定价规则写面，与 bulk-import /
// model-prices/preview 同组）。micros 字段一律 string 渲染（int64 JSON
// 精度安全，与 preview 的 PriceVersionView 同约定）。

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AdminPriceVersionsHandler exposes the price-version operator surface.
type AdminPriceVersionsHandler struct {
	svc *management.PriceVersionService
}

func NewAdminPriceVersionsHandler(svc *management.PriceVersionService) *AdminPriceVersionsHandler {
	return &AdminPriceVersionsHandler{svc: svc}
}

// Register mounts the endpoints; the caller wraps the group with the
// models:manage authorization middleware.
func (h *AdminPriceVersionsHandler) Register(g *gin.RouterGroup) {
	g.POST("/price-versions", h.Create)
	g.GET("/price-versions", h.List)
}

type createPriceVersionRequest struct {
	ModelID           string          `json:"model_id"`
	Kind              string          `json:"kind"`
	Currency          string          `json:"currency"`
	InputPerMtok      int64           `json:"input_micros_per_mtok"`
	CacheReadPerMtok  int64           `json:"cache_read_micros_per_mtok"`
	CacheWritePerMtok int64           `json:"cache_write_micros_per_mtok"`
	OutputPerMtok     int64           `json:"output_micros_per_mtok"`
	ExtraRates        json.RawMessage `json:"extra_rates"`
	Revision          int             `json:"revision"`
	EffectiveFrom     *time.Time      `json:"effective_from"`
	EffectiveTo       *time.Time      `json:"effective_to"`
	Reason            string          `json:"reason"`
}

// priceVersionView is the wire shape (spec §2.1): micros as strings;
// currency omitted for sale_credit; effective_to omitted when open-ended.
type priceVersionView struct {
	PriceVersionID    string          `json:"price_version_id"`
	ModelID           string          `json:"model_id"`
	Kind              string          `json:"kind"`
	Unit              string          `json:"unit"`
	Currency          string          `json:"currency,omitempty"`
	InputPerMtok      string          `json:"input_micros_per_mtok"`
	CacheReadPerMtok  string          `json:"cache_read_micros_per_mtok"`
	CacheWritePerMtok string          `json:"cache_write_micros_per_mtok"`
	OutputPerMtok     string          `json:"output_micros_per_mtok"`
	ExtraRates        json.RawMessage `json:"extra_rates"`
	Revision          int             `json:"revision"`
	EffectiveFrom     time.Time       `json:"effective_from"`
	EffectiveTo       *time.Time      `json:"effective_to,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

func toPriceVersionView(p *management.PriceVersionInfo) priceVersionView {
	extra := p.ExtraRates
	if len(extra) == 0 {
		extra = json.RawMessage(`{"schema_version":1}`)
	}
	var to *time.Time
	if p.EffectiveTo != nil {
		t := p.EffectiveTo.UTC()
		to = &t
	}
	return priceVersionView{
		PriceVersionID: p.ID, ModelID: p.ModelID, Kind: p.Kind,
		Unit: p.Unit, Currency: p.Currency,
		InputPerMtok:      strconv.FormatInt(p.InputPerMtok, 10),
		CacheReadPerMtok:  strconv.FormatInt(p.CacheReadPerMtok, 10),
		CacheWritePerMtok: strconv.FormatInt(p.CacheWritePerMtok, 10),
		OutputPerMtok:     strconv.FormatInt(p.OutputPerMtok, 10),
		ExtraRates:        extra, Revision: p.Revision,
		EffectiveFrom: p.EffectiveFrom.UTC(), EffectiveTo: to,
		CreatedAt: p.CreatedAt.UTC(),
	}
}

// Create POST /price-versions — 追加一个不可变价格版本。重放/冲突均 409 +
// data=已存在版本视图（调用方可安全重试；内容不同则是 revision 取错的
// 操作错误信号）。
func (h *AdminPriceVersionsHandler) Create(c *gin.Context) {
	var req createPriceVersionRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	var from time.Time
	if req.EffectiveFrom != nil {
		from = *req.EffectiveFrom
	}
	created, err := h.svc.Create(c.Request.Context(), actorOf(c), management.CreatePriceVersionInput{
		ModelID: req.ModelID, Kind: req.Kind, Currency: req.Currency,
		InputPerMtok: req.InputPerMtok, CacheReadPerMtok: req.CacheReadPerMtok,
		CacheWritePerMtok: req.CacheWritePerMtok, OutputPerMtok: req.OutputPerMtok,
		ExtraRates: req.ExtraRates, Revision: req.Revision,
		EffectiveFrom: from, EffectiveTo: req.EffectiveTo, Reason: req.Reason,
	})
	if err != nil {
		var exists *management.PriceVersionExistsError
		if errors.As(err, &exists) && exists.Existing != nil {
			c.JSON(http.StatusConflict, gin.H{
				"code":    http.StatusConflict,
				"message": exists.Error(),
				"data":    toPriceVersionView(exists.Existing),
			})
			return
		}
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": toPriceVersionView(created)})
}

// List GET /price-versions?model_id=&kind=&limit=&offset= — items 空时为
// [] 而非 null；limit 默认 100 钳 500（parseLimit 既有约定）；offset <0
// 或非法 kind → 400。
func (h *AdminPriceVersionsHandler) List(c *gin.Context) {
	filter := management.PriceVersionFilter{
		ModelID: c.Query("model_id"),
		Limit:   parseLimit(c, 100),
	}
	if kind := c.Query("kind"); kind != "" {
		switch kind {
		case "sale_credit", "sale_money", "upstream_cost":
			filter.Kind = kind
		default:
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid query: unknown kind "+kind))
			return
		}
	}
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid query: offset must be >= 0"))
			return
		}
		filter.Offset = n
	}
	items, err := h.svc.List(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]priceVersionView, 0, len(items))
	for i := range items {
		out = append(out, toPriceVersionView(&items[i]))
	}
	ok(c, gin.H{"items": out, "limit": filter.Limit, "offset": filter.Offset})
}
