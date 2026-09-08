package model

import (
	"time"

	"github.com/lib/pq"
)

// Grant modes for PlanBenefitConfig.GrantMode / Order.BenefitGrantMode
// (migration 029; design §4.1–§4.2).
const (
	// BenefitGrantModeSubscription: paying this plan confers an explicit
	// (subscription-sourced) model entitlement. Required on every
	// purchasable coding-plan plan; rejected on kaya-membership plans.
	BenefitGrantModeSubscription = "subscription"
	// BenefitGrantModeGift: paying this plan additionally issues a gifted
	// entitlement (bundle mapping, design §4.1: 捆绑商品通过明确的
	// benefit/grant 映射发放模型权益). Gifts never stack with an explicit
	// plan and never auto-fallback after exhaustion.
	BenefitGrantModeGift = "gift"
)

// Order kinds frozen onto coding-plan orders at creation time
// (orders.order_kind, migration 029). Kaya-membership orders keep the
// legacy empty kind — their repurchase/upgrade rules stay the
// interval-comparison ones.
const (
	OrderKindNew     = "new"
	OrderKindRenewal = "renewal"
	OrderKindUpgrade = "upgrade"
)

// PlanBenefitConfig is the published payment→benefit mapping for one plan
// (plan_benefit_configs, migration 029). It is the "payment configuration"
// of design §4.3: a coding-plan plan WITHOUT a row is not purchasable, and
// a kaya plan without a row grants no model benefits.
//
// The mapping pins an immutable policy version (the entitlement/quota
// version) plus an explicit model set; both are copied onto the order at
// creation time, so later config edits never retroactively change what an
// already-created order grants.
type PlanBenefitConfig struct {
	PlanID          string         `db:"plan_id" json:"plan_id"`
	PolicyVersionID string         `db:"policy_version_id" json:"policy_version_id"`
	ModelIDs        pq.StringArray `db:"model_ids" json:"model_ids"`
	GrantMode       string         `db:"grant_mode" json:"grant_mode"`
	CreatedAt       time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time      `db:"updated_at" json:"updated_at"`
}
