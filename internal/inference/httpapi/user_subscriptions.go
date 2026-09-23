// user_subscriptions.go — GET /user/model-subscriptions（Task 11；设计 §9.2：
// Coding Plan 套餐、当前权益、赠送来源；与旧会员展示分离）。
//
// 响应只覆盖 coding-plan 产品：subscriptions 列表 + 权益（含 grant 来源）
// + 命名空间化的 kaya_membership 活跃标记（解释捆绑赠送来源用，不与
// coding-plan 列表混排；旧会员详情仍由 /user/subscriptions 提供）。
// 赠送来源记录到订阅/规则级，上游供应商账号不出现在任何字段中。

package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/management"
)

// UserSubscriptionsHandler serves the model-subscriptions endpoint.
type UserSubscriptionsHandler struct {
	svc *management.SubscriptionViewService
}

func NewUserSubscriptionsHandler(svc *management.SubscriptionViewService) *UserSubscriptionsHandler {
	return &UserSubscriptionsHandler{svc: svc}
}

// Register mounts the endpoint on a JWT-authenticated group.
func (h *UserSubscriptionsHandler) Register(g *gin.RouterGroup) {
	g.GET("/model-subscriptions", h.Get)
}

// productSubscriptionJSON is one subscription row (any status).
type productSubscriptionJSON struct {
	ID          string  `json:"id"`
	PlanID      string  `json:"plan_id"`
	PlanName    *string `json:"plan_name"`
	ProductCode string  `json:"product_code"`
	Status      string  `json:"status"`
	StartedAt   string  `json:"started_at"`
	ExpiresAt   *string `json:"expires_at"`
	CreatedAt   string  `json:"created_at"`
}

func toProductSubscriptionJSON(s management.ProductSubscription) productSubscriptionJSON {
	return productSubscriptionJSON{
		ID: s.ID, PlanID: s.PlanID, PlanName: s.PlanName,
		ProductCode: s.ProductCode, Status: s.Status,
		StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
		ExpiresAt: rfc3339Ptr(s.ExpiresAt),
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// entitlementSourceJSON records the grant origin （订阅/订单/赠送规则）.
// reference is the customer's own source id — never an upstream account.
type entitlementSourceJSON struct {
	Type           string  `json:"type"` // subscription | order | grant
	Kind           string  `json:"kind,omitempty"`
	Reference      string  `json:"reference"`
	SubscriptionID string  `json:"subscription_id,omitempty"`
	PlanID         string  `json:"plan_id,omitempty"`
	PlanName       *string `json:"plan_name,omitempty"`
}

// entitlementItemJSON is one entitlement with its grant source.
type entitlementItemJSON struct {
	ID              string                 `json:"id"`
	Status          string                 `json:"status"`
	Revision        int                    `json:"revision"`
	ModelIDs        []string               `json:"model_ids"`
	PolicyVersionID string                 `json:"policy_version_id"`
	AnchorAt        string                 `json:"anchor_at"`
	EffectiveFrom   string                 `json:"effective_from"`
	EffectiveTo     *string                `json:"effective_to"`
	Stackable       bool                   `json:"stackable"`
	CreatedAt       string                 `json:"created_at"`
	Source          entitlementSourceJSON  `json:"source"`
}

func toEntitlementItemJSON(e management.EntitlementItem) entitlementItemJSON {
	return entitlementItemJSON{
		ID: e.ID, Status: string(e.Status), Revision: e.Revision,
		ModelIDs: e.ModelIDs, PolicyVersionID: e.PolicyVersionID,
		AnchorAt:      e.AnchorAt.UTC().Format(time.RFC3339),
		EffectiveFrom: e.EffectiveFrom.UTC().Format(time.RFC3339),
		EffectiveTo:   rfc3339Ptr(e.EffectiveTo),
		Stackable:     e.Stackable,
		CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339),
		Source: entitlementSourceJSON{
			Type: string(e.Source.Type), Kind: e.Source.Kind,
			Reference: e.Source.Reference, SubscriptionID: e.Source.SubscriptionID,
			PlanID: e.Source.PlanID, PlanName: e.Source.PlanName,
		},
	}
}

// Get handles GET /user/model-subscriptions. Ownership is the JWT identity;
// every row is scoped by the server-verified user id.
func (h *UserSubscriptionsHandler) Get(c *gin.Context) {
	view, err := h.svc.Get(c.Request.Context(), userIDOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	subs := make([]productSubscriptionJSON, 0, len(view.Subscriptions))
	for _, s := range view.Subscriptions {
		subs = append(subs, toProductSubscriptionJSON(s))
	}
	ents := make([]entitlementItemJSON, 0, len(view.Entitlements))
	for _, e := range view.Entitlements {
		ents = append(ents, toEntitlementItemJSON(e))
	}
	var kaya *productSubscriptionJSON
	if view.KayaMembership != nil {
		m := toProductSubscriptionJSON(*view.KayaMembership)
		kaya = &m
	}
	ok(c, gin.H{
		"server_time":     view.ServerTime.UTC().Format(time.RFC3339),
		"as_of":           view.AsOf.UTC().Format(time.RFC3339),
		"subscriptions":   subs,
		"entitlements":    ents,
		"kaya_membership": kaya,
	})
}
