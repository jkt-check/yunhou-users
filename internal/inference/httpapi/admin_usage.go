// admin_usage.go — 运营统计 / 成本分析 / 异常筛选 / 变更预览端点
// （Task 15，设计 §9.2 /admin/model-usage、/admin/model-customers、
// /admin/model-prices、/admin/quota-policies、/admin/upstreams 族）。
//
//   GET  /admin/model-usage/summary?group_by=model|provider|customer&from&to
//        [&model_id&provider_id&billing_account_id]
//        请求量/成功率/延迟/客户消费(账本派生 microcredit)/采购成本(币种×
//        cost_basis 分片 + unknown 计数)（usage:read）。
//   GET  /admin/model-usage/exceptions?kind=stuck_reservations|
//        reauth_required_accounts|settlement_backlog[&limit]
//        异常预占 / 授权失效 / 结算积压（usage:read）。
//   GET  /admin/upstream-accounts/shared?from&to&limit
//        "一账号一部署"检测视图（Task 13 移交项；usage:read）。
//   POST /admin/model-prices/preview    价格变更影响预览（models:manage）。
//   POST /admin/quota-policies/preview  额度策略变更影响预览（models:manage）。
//
// 金额一律 DecimalInt64（十进制整数字符串）；成本分片按币种 × basis 呈现，
// 绝不跨币种合并；token 桶 null = 未知。统计是只读派生路径（as_of =
// complete_through = 读取时刻），额度闸门不受影响。

package httpapi

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AdminUsageHandler exposes the operator analytics surface.
type AdminUsageHandler struct {
	ops     *management.OperationsService
	preview *management.PricingPreviewService
}

// NewAdminUsageHandler builds the handler.
func NewAdminUsageHandler(ops *management.OperationsService, preview *management.PricingPreviewService) *AdminUsageHandler {
	return &AdminUsageHandler{ops: ops, preview: preview}
}

// RegisterRead mounts the usage:read endpoints.
func (h *AdminUsageHandler) RegisterRead(g *gin.RouterGroup) {
	g.GET("/model-usage/summary", h.Summary)
	g.GET("/model-usage/exceptions", h.Exceptions)
	g.GET("/upstream-accounts/shared", h.SharedAccounts)
	// 补偿追踪（设计 §9.2 /admin/model-adjustments）：与 billing:adjust 面
	// 的 GET /wallet/adjustments 同一服务同一口径；auditor 角色经此只读
	// 追踪补偿到人员。
	g.GET("/model-adjustments", h.Adjustments)
}

// Adjustments handles GET /model-adjustments?billing_account_id=&limit=.
func (h *AdminUsageHandler) Adjustments(c *gin.Context) {
	views, err := h.ops.Adjustments(c.Request.Context(), c.Query("billing_account_id"), parseLimit(c, 100))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"adjustments": adjustmentViewsJSON(views)})
}

// RegisterPreview mounts the change-preview endpoints (models:manage).
func (h *AdminUsageHandler) RegisterPreview(g *gin.RouterGroup) {
	g.POST("/model-prices/preview", h.PreviewPrice)
	g.POST("/quota-policies/preview", h.PreviewPolicy)
}

// --- summary ---

type costSliceJSON struct {
	Currency string `json:"currency"`
	Basis    string `json:"basis"`
	Micros   string `json:"micros"`
}

type opsGroupJSON struct {
	ModelID      string `json:"model_id,omitempty"`
	ProviderID   string `json:"provider_id,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
	AccountID    string `json:"billing_account_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`

	Requests              int64    `json:"requests"`
	Settled               int64    `json:"settled"`
	Released              int64    `json:"released"`
	InFlight              int64    `json:"in_flight"`
	Failed                int64    `json:"failed"`
	ReconciliationPending int64    `json:"reconciliation_pending"`
	Reported              int64    `json:"reported"`
	Estimated             int64    `json:"estimated"`
	Unknown               int64    `json:"unknown"`
	Pending               int64    `json:"pending"`
	SuccessRate           *float64 `json:"success_rate"`

	AvgLatencyMs *int64 `json:"avg_latency_ms"`
	P95LatencyMs *int64 `json:"p95_latency_ms"`

	ChargeMicros   string `json:"charge_micros"`
	ReversedMicros string `json:"reversed_micros"`
	AdjustedMicros string `json:"adjusted_micros"`
	NetMicros      string `json:"net_micros"`

	Cost                []costSliceJSON `json:"cost"`
	CostUnknownAttempts int64           `json:"cost_unknown_attempts"`

	Tokens usageTokensJSON `json:"tokens"`
}

func toOpsGroupJSON(g management.OpsGroup) opsGroupJSON {
	cost := make([]costSliceJSON, 0, len(g.CostSlices))
	for _, s := range g.CostSlices {
		cost = append(cost, costSliceJSON{Currency: s.Currency, Basis: s.Basis, Micros: strconv.FormatInt(s.Micros, 10)})
	}
	return opsGroupJSON{
		ModelID: g.ModelID, ProviderID: g.ProviderID, ProviderCode: g.ProviderCode,
		AccountID: g.AccountID, UserID: g.UserID,
		Requests: g.RequestsTotal, Settled: g.SettledRequests, Released: g.ReleasedRequests,
		InFlight: g.InFlightRequests, Failed: g.FailedRequests, ReconciliationPending: g.ReconciliationPending,
		Reported: g.Reported, Estimated: g.Estimated, Unknown: g.Unknown, Pending: g.Pending,
		SuccessRate:  g.SuccessRate,
		AvgLatencyMs: g.AvgLatencyMs, P95LatencyMs: g.P95LatencyMs,
		ChargeMicros:   strconv.FormatInt(g.ChargeMicros, 10),
		ReversedMicros: strconv.FormatInt(g.ReversedMicros, 10),
		AdjustedMicros: strconv.FormatInt(g.AdjustedMicros, 10),
		NetMicros:      strconv.FormatInt(g.NetMicros(), 10),
		Cost:           cost,
		CostUnknownAttempts: g.CostUnknownAttempts,
		Tokens:              toUsageTokensJSON(g.Tokens),
	}
}

// Summary handles GET /model-usage/summary.
func (h *AdminUsageHandler) Summary(c *gin.Context) {
	var f management.OpsUsageFilter
	var err error
	if f.From, err = parseOptionalRFC3339(c.Query("from"), "from"); err != nil {
		fail(c, err)
		return
	}
	if f.To, err = parseOptionalRFC3339(c.Query("to"), "to"); err != nil {
		fail(c, err)
		return
	}
	f.GroupBy = management.OpsGroupBy(c.Query("group_by"))
	f.ModelID = c.Query("model_id")
	f.ProviderID = c.Query("provider_id")
	f.AccountID = c.Query("billing_account_id")
	sum, err := h.ops.Summary(c.Request.Context(), f)
	if err != nil {
		fail(c, err)
		return
	}
	groups := make([]opsGroupJSON, 0, len(sum.Groups))
	for _, g := range sum.Groups {
		groups = append(groups, toOpsGroupJSON(g))
	}
	ok(c, gin.H{
		"server_time":      sum.ServerTime.UTC().Format(time.RFC3339),
		"as_of":            sum.AsOf.UTC().Format(time.RFC3339),
		"complete_through": sum.CompleteThrough.UTC().Format(time.RFC3339),
		"unit":             management.UnitMicrocredit,
		"range":            gin.H{"from": sum.From.Format(time.RFC3339), "to": sum.To.Format(time.RFC3339)},
		"group_by":         string(sum.GroupBy),
		"groups":           groups,
	})
}

// --- exceptions ---

// Exceptions handles GET /model-usage/exceptions?kind=...
func (h *AdminUsageHandler) Exceptions(c *gin.Context) {
	kind := c.Query("kind")
	limit := parseLimit(c, 100)
	view, err := h.ops.Exceptions(c.Request.Context(), kind, limit)
	if err != nil {
		fail(c, err)
		return
	}
	stuck := make([]gin.H, 0, len(view.StuckReservations))
	for _, r := range view.StuckReservations {
		stuck = append(stuck, gin.H{
			"reservation_id": r.ReservationID, "request_id": r.RequestID,
			"request_status": r.RequestStatus, "billing_account_id": r.AccountID,
			"model_id": r.ModelID, "target_kind": r.TargetKind,
			"amount_micros": strconv.FormatInt(r.AmountMicros, 10),
			"age_seconds":   r.AgeSeconds,
			"created_at":    r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	reauth := make([]gin.H, 0, len(view.ReauthAccounts))
	for _, a := range view.ReauthAccounts {
		reauth = append(reauth, gin.H{
			"account_id": a.AccountID, "provider_id": a.ProviderID,
			"provider_code": a.ProviderCode, "display_name": a.DisplayName,
			"status":     a.Status,
			"updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	backlog := make([]gin.H, 0, len(view.SettlementBacklog))
	for _, j := range view.SettlementBacklog {
		backlog = append(backlog, gin.H{
			"id": j.ID, "request_id": j.RequestID, "reason": j.Reason,
			"status": j.Status, "deadline_at": j.DeadlineAt.UTC().Format(time.RFC3339),
			"overdue":    j.Overdue,
			"created_at": j.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	ok(c, gin.H{
		"server_time": view.ServerTime.UTC().Format(time.RFC3339),
		"kind":        view.Kind,
		"stuck_reservations": stuck,
		"reauth_required_accounts": reauth,
		"settlement_backlog":       backlog,
	})
}

// SharedAccounts handles GET /upstream-accounts/shared — the
// "一账号一部署"检测视图（两部署共享同一上游账号会在故障切换时撞租约唯一
// 键；调度语义不改，运营面显式检测）。
func (h *AdminUsageHandler) SharedAccounts(c *gin.Context) {
	from, err := parseOptionalRFC3339(c.Query("from"), "from")
	if err != nil {
		fail(c, err)
		return
	}
	to, err := parseOptionalRFC3339(c.Query("to"), "to")
	if err != nil {
		fail(c, err)
		return
	}
	rows, err := h.ops.SharedDeploymentAccounts(c.Request.Context(), from, to, parseLimit(c, 100))
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"account_id": r.AccountID, "provider_id": r.ProviderID,
			"provider_code": r.ProviderCode, "display_name": r.DisplayName,
			"deployment_ids": r.Deployments, "attempts": r.Attempts,
			"latest_attempt_at": r.LatestAt.UTC().Format(time.RFC3339),
		})
	}
	ok(c, gin.H{"accounts": out})
}

// --- change previews ---

type pricePreviewRequest struct {
	ModelID           string     `json:"model_id" binding:"required"`
	Kind              string     `json:"kind" binding:"required"`
	Currency          string     `json:"currency"`
	InputPerMtok      int64      `json:"input_micros_per_mtok"`
	CacheReadPerMtok  int64      `json:"cache_read_micros_per_mtok"`
	CacheWritePerMtok int64      `json:"cache_write_micros_per_mtok"`
	OutputPerMtok     int64      `json:"output_micros_per_mtok"`
	EffectiveFrom     *time.Time `json:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to"`
}

// PreviewPrice handles POST /model-prices/preview — 只读影响预览（不落库）。
func (h *AdminUsageHandler) PreviewPrice(c *gin.Context) {
	var req pricePreviewRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	cmd := management.PriceChangePreview{
		ModelID: req.ModelID, Kind: req.Kind, Currency: req.Currency,
		InputPerMtok: req.InputPerMtok, CacheReadPerMtok: req.CacheReadPerMtok,
		CacheWritePerMtok: req.CacheWritePerMtok, OutputPerMtok: req.OutputPerMtok,
		EffectiveTo: req.EffectiveTo,
	}
	if req.EffectiveFrom != nil {
		cmd.EffectiveFrom = *req.EffectiveFrom
	}
	res, err := h.preview.PreviewPriceChange(c.Request.Context(), cmd)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}

type policyPreviewRequest struct {
	Name             string   `json:"name" binding:"required"`
	ModelIDs         []string `json:"model_ids"`
	FiveHourLimit    *int64   `json:"five_hour_limit_micros"`
	WeeklyLimit      *int64   `json:"weekly_limit_micros"`
	MonthlyLimit     *int64   `json:"monthly_limit_micros"`
	RPMLimit         *int     `json:"rpm_limit"`
	TPMLimit         *int64   `json:"tpm_limit"`
	ConcurrencyLimit *int     `json:"concurrency_limit"`
	OveragePolicy    string   `json:"overage_policy"`
}

// PreviewPolicy handles POST /quota-policies/preview — 只读影响预览（不落库）。
func (h *AdminUsageHandler) PreviewPolicy(c *gin.Context) {
	var req policyPreviewRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	res, err := h.preview.PreviewPolicyChange(c.Request.Context(), management.PolicyChangePreview{
		Name: req.Name, ModelIDs: req.ModelIDs,
		FiveHourLimit: req.FiveHourLimit, WeeklyLimit: req.WeeklyLimit, MonthlyLimit: req.MonthlyLimit,
		RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit, ConcurrencyLimit: req.ConcurrencyLimit,
		OveragePolicy: req.OveragePolicy,
	})
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, res)
}
