// admin_entitlements.go — 存量权益增补 admin 面（entitlement-model-amendment
// 需求，deploy 侧 PR #345）。四个端点：
//
//	POST /admin/entitlements/:id/revise    R1 单权益修订（billing:adjust）
//	POST /admin/entitlements/amend-models  R2 批量增补（billing:adjust）
//	GET  /admin/entitlements/:id           R3 只读（usage:read）
//	GET  /admin/entitlements               R3 列表（usage:read）
//
// 业务语义全部在 management.EntitlementAdminService（幂等短路、行级事务、
// 审计同事务）；本层只做 HTTP↔服务转换与入参形状校验。权限挂载见
// AdminOps.Mount（写 billing:adjust / 读 usage:read，需求 R5 建议口径）。
package httpapi

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AdminEntitlementsHandler exposes the entitlement admin surface.
type AdminEntitlementsHandler struct {
	svc *management.EntitlementAdminService
}

func NewAdminEntitlementsHandler(svc *management.EntitlementAdminService) *AdminEntitlementsHandler {
	return &AdminEntitlementsHandler{svc: svc}
}

// RegisterWrite mounts R1/R2（调用方已挂 billing:adjust 中间件）。
func (h *AdminEntitlementsHandler) RegisterWrite(g *gin.RouterGroup) {
	g.POST("/entitlements/:id/revise", h.ReviseEntitlement)
	g.POST("/entitlements/amend-models", h.AmendModels)
}

// RegisterRead mounts R3（调用方已挂 usage:read 中间件）。
func (h *AdminEntitlementsHandler) RegisterRead(g *gin.RouterGroup) {
	g.GET("/entitlements", h.ListEntitlements)
	g.GET("/entitlements/:id", h.GetEntitlement)
}

// entitlementDTO 是权益的 wire 视图（R1/R3 共用）。
type entitlementDTO struct {
	ID               string     `json:"id"`
	BillingAccountID string     `json:"billing_account_id"`
	SourceType       string     `json:"source_type"`
	SourceID         string     `json:"source_id"`
	ModelIDs         []string   `json:"model_ids"`
	PolicyVersionID  string     `json:"policy_version_id"`
	AnchorAt         time.Time  `json:"anchor_at"`
	EffectiveFrom    time.Time  `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to"`
	Revision         int        `json:"revision"`
	Stackable        bool       `json:"stackable"`
	Status           string     `json:"status"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func toEntitlementDTO(e *domain.Entitlement) entitlementDTO {
	models := e.ModelIDs
	if models == nil {
		models = []string{} // 显式集合语义：null 不出现在 wire 上（红线 1）
	}
	return entitlementDTO{
		ID: e.ID, BillingAccountID: e.BillingAccountID,
		SourceType: string(e.SourceType), SourceID: e.SourceID,
		ModelIDs: models, PolicyVersionID: e.PolicyVersionID,
		AnchorAt: e.AnchorAt, EffectiveFrom: e.EffectiveFrom, EffectiveTo: e.EffectiveTo,
		Revision: e.Revision, Stackable: e.Stackable, Status: string(e.Status),
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// operatorAttr 转换授权中间件的双腿归因为服务层类型。
func operatorAttr(c *gin.Context) management.EntitlementOperator {
	op := OperatorOf(c)
	return management.EntitlementOperator{UserID: op.UserID, AppID: op.AppID}
}

// --- R3 只读 ---

// GetEntitlement GET /entitlements/:id
func (h *AdminEntitlementsHandler) GetEntitlement(c *gin.Context) {
	ent, err := h.svc.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"entitlement": toEntitlementDTO(ent)})
}

// ListEntitlements GET /entitlements?billing_account_id=&status=&limit=&offset=
func (h *AdminEntitlementsHandler) ListEntitlements(c *gin.Context) {
	filter := management.EntitlementListFilter{
		BillingAccountID: c.Query("billing_account_id"),
		Limit:            parseLimit(c, 50),
	}
	if s := c.Query("status"); s != "" {
		switch domain.EntitlementStatus(s) {
		case domain.EntitlementActive, domain.EntitlementExpired,
			domain.EntitlementRevoked, domain.EntitlementSuperseded:
			filter.Status = s
		default:
			fail(c, domain.NewError(domain.CodeInvalidInput,
				"status must be active, expired, revoked or superseded"))
			return
		}
	}
	if raw := c.Query("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			filter.Offset = n
		}
	}
	ents, err := h.svc.List(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]entitlementDTO, 0, len(ents))
	for i := range ents {
		out = append(out, toEntitlementDTO(&ents[i]))
	}
	ok(c, gin.H{"entitlements": out})
}

// --- R1 单权益修订 ---

type reviseEntitlementRequest struct {
	AddModelIDs    []string `json:"add_model_ids"`
	RemoveModelIDs []string `json:"remove_model_ids"`
	Reason         string   `json:"reason"`
}

// ReviseEntitlement POST /entitlements/:id/revise — 幂等：集合无变化 →
// 200 changed=false；非 active / 乐观锁冲突 → 409。
func (h *AdminEntitlementsHandler) ReviseEntitlement(c *gin.Context) {
	var req reviseEntitlementRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	res, err := h.svc.Revise(c.Request.Context(), operatorAttr(c), c.Param("id"), management.ReviseInput{
		AddModelIDs: req.AddModelIDs, RemoveModelIDs: req.RemoveModelIDs, Reason: req.Reason,
	})
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"changed": res.Changed, "entitlement": toEntitlementDTO(res.Entitlement)})
}

// --- R2 批量增补 ---

type amendModelsRequest struct {
	ModelID  string `json:"model_id"`
	Action   string `json:"action"`
	Selector string `json:"selector"`
	Reason   string `json:"reason"`
	// DryRun 默认 true（预演优先，需求 R2）：指针区分「未传」与显式 false。
	DryRun *bool `json:"dry_run"`
	Sample int   `json:"sample"`
}

// AmendModels POST /entitlements/amend-models — 行级失败不置整体失败，
// 明细在 report.errors[] / skipped_detail（N6 迁移工具先例）。
func (h *AdminEntitlementsHandler) AmendModels(c *gin.Context) {
	var req amendModelsRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	rep, err := h.svc.AmendModels(c.Request.Context(), operatorAttr(c), management.AmendRequest{
		ModelID: req.ModelID, Action: req.Action, Selector: req.Selector,
		Reason: req.Reason, DryRun: dryRun, SampleN: req.Sample,
	})
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, rep)
}
