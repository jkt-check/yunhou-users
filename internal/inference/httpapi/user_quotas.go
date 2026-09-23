// user_quotas.go — GET /user/model-quotas（Task 11；设计 §9.2 额度响应形状
// 逐字对齐：server_time/as_of/entitlement_id/unit/blocked_by/windows，金额
// 与额度一律十进制整数字符串）。
//
// 前端无需计算窗口：window_start/resets_at/remaining 由服务端给出；五小时
// 窗口未激活时 window_start/resets_at 为 null 且 activation=
// on_first_consumption（设计 §6）；禁用窗口 disabled=true + limit=null
// （缺失≠无限）。quota 读是当前权威状态（as_of 即读取时刻）。

package httpapi

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/quota"
)

// UserQuotasHandler serves the customer quota endpoint.
type UserQuotasHandler struct {
	svc *management.QuotaViewService
}

func NewUserQuotasHandler(svc *management.QuotaViewService) *UserQuotasHandler {
	return &UserQuotasHandler{svc: svc}
}

// Register mounts the endpoint on a JWT-authenticated group.
func (h *UserQuotasHandler) Register(g *gin.RouterGroup) {
	g.GET("/model-quotas", h.Get)
}

// windowMode is the §6 display semantics of each window kind:
// five_hour 固定时长锚定首次消费；weekly 锚点起 7×24h 周期；monthly 锚点
// 公历月推进（原始锚点日裁剪）。客户端切时区不改变额度（全部 UTC）。
func windowMode(kind domain.WindowKind) string {
	switch kind {
	case domain.WindowFiveHour:
		return "anchored_duration"
	case domain.WindowWeekly:
		return "anchored_period_7d"
	case domain.WindowMonthly:
		return "anchored_calendar_month"
	}
	return ""
}

// quotaWindowJSON is one window of the §9.2 response example. limit is null
// on a disabled window (显式标识 ≠ 无限额度); window_start/resets_at are
// null while a five-hour window is unactivated (activation carries the hint).
type quotaWindowJSON struct {
	Kind        string  `json:"kind"`
	Mode        string  `json:"mode"`
	Disabled    bool    `json:"disabled"`
	Limit       *string `json:"limit"`
	Used        string  `json:"used"`
	Reserved    string  `json:"reserved"`
	Remaining   *string `json:"remaining"`
	WindowStart *string `json:"window_start"`
	ResetsAt    *string `json:"resets_at"`
	Activation  string  `json:"activation,omitempty"`
}

func toQuotaWindowJSON(v quota.WindowView) quotaWindowJSON {
	out := quotaWindowJSON{
		Kind:        string(v.Kind),
		Mode:        windowMode(v.Kind),
		Disabled:    v.Disabled,
		Used:        strconv.FormatInt(int64(v.Used), 10),
		Reserved:    strconv.FormatInt(int64(v.Reserved), 10),
		WindowStart: rfc3339Ptr(v.WindowStart),
		ResetsAt:    rfc3339Ptr(v.ResetsAt),
		Activation:  v.Activation,
	}
	if v.Limit != nil {
		s := strconv.FormatInt(int64(*v.Limit), 10)
		out.Limit = &s
	}
	if r := management.Remaining(v); r != nil {
		s := strconv.FormatInt(int64(*r), 10)
		out.Remaining = &s
	}
	return out
}

// quotaBlockJSON is one current blocking constraint. All keys are always
// present; inapplicable values are null (typed-client friendly). Window
// blocks carry the counter snapshot; entitlement/account blocks carry
// reason (+expired_at for expiry).
type quotaBlockJSON struct {
	Kind      string  `json:"kind"`
	Reason    string  `json:"reason"`
	Limit     *string `json:"limit"`
	Used      *string `json:"used"`
	Reserved  *string `json:"reserved"`
	Remaining *string `json:"remaining"`
	ResetsAt  *string `json:"resets_at"`
	ExpiredAt *string `json:"expired_at"`
}

func toQuotaBlockJSON(b management.Block) quotaBlockJSON {
	out := quotaBlockJSON{Kind: b.Kind, Reason: b.Reason}
	if b.Window != nil {
		limit := strconv.FormatInt(int64(b.Window.Limit), 10)
		used := strconv.FormatInt(int64(b.Window.Used), 10)
		reserved := strconv.FormatInt(int64(b.Window.Reserved), 10)
		// Remaining 由 management.Remaining 计算（与阻断谓词同一实现），
		// 不在此处重复 max(0,...) 逻辑。
		remaining := strconv.FormatInt(int64(b.Window.Remaining), 10)
		out.Limit, out.Used, out.Reserved, out.Remaining = &limit, &used, &reserved, &remaining
		out.ResetsAt = rfc3339Ptr(b.Window.ResetsAt)
	}
	out.ExpiredAt = rfc3339Ptr(b.ExpiredAt)
	return out
}

// quotaEntitlementJSON is the effective-range detail of the entitlement the
// quota view keyed on (权益有效期).
type quotaEntitlementJSON struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	SourceType      string   `json:"source_type"`
	ModelIDs        []string `json:"model_ids"`
	PolicyVersionID string   `json:"policy_version_id"`
	AnchorAt        string   `json:"anchor_at"`
	EffectiveFrom   string   `json:"effective_from"`
	EffectiveTo     *string  `json:"effective_to"`
	Revision        int      `json:"revision"`
}

// Get handles GET /user/model-quotas. Ownership is the JWT identity
// (userIDOf); there is no id parameter, so cross-customer reads are
// impossible by construction.
func (h *UserQuotasHandler) Get(c *gin.Context) {
	view, err := h.svc.Get(c.Request.Context(), userIDOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	windows := make([]quotaWindowJSON, 0, len(view.Windows))
	for _, w := range view.Windows {
		windows = append(windows, toQuotaWindowJSON(w))
	}
	blocks := make([]quotaBlockJSON, 0, len(view.BlockedBy))
	for _, b := range view.BlockedBy {
		blocks = append(blocks, toQuotaBlockJSON(b))
	}
	var entitlementID *string
	var entitlement *quotaEntitlementJSON
	if view.Entitlement != nil {
		id := view.Entitlement.ID
		entitlementID = &id
		entitlement = &quotaEntitlementJSON{
			ID: view.Entitlement.ID, Status: string(view.Entitlement.Status),
			SourceType: string(view.Entitlement.SourceType),
			ModelIDs:   view.Entitlement.ModelIDs, PolicyVersionID: view.Entitlement.PolicyVersionID,
			AnchorAt: view.Entitlement.AnchorAt.UTC().Format(time.RFC3339),
			EffectiveFrom: view.Entitlement.EffectiveFrom.UTC().Format(time.RFC3339),
			EffectiveTo:   rfc3339Ptr(view.Entitlement.EffectiveTo),
			Revision:      view.Entitlement.Revision,
		}
	}
	ok(c, gin.H{
		"server_time":    view.ServerTime.UTC().Format(time.RFC3339),
		"as_of":          view.AsOf.UTC().Format(time.RFC3339),
		"unit":           view.Unit,
		"entitlement_id": entitlementID,
		"entitlement":    entitlement,
		"blocked_by":     blocks,
		"windows":        windows,
	})
}
