package model

import "time"

// Commercial product ownership for plans and subscriptions (design §4.1).
// product_code is deliberately separate from app_id (the login surface):
// one product can span several apps, and a single user may hold one active
// subscription per product at the same time.
const (
	// ProductKayaMembership is the compatibility product every pre-027 plan
	// and subscription belongs to. Legacy API paths that don't name a
	// product operate on this product only.
	ProductKayaMembership = "kaya-membership"
	// ProductCodingPlan is the standalone model-API plan. Not on sale in
	// this phase: code supports dual-product rows, but no client-facing
	// sale path may create coding-plan subscriptions yet.
	ProductCodingPlan = "coding-plan"
	// ProductWalletTopup is the prepaid pay-as-you-go balance top-up
	// product (Task 14): paying a wallet-topup order credits the
	// customer's inference wallet (cash source) instead of activating any
	// subscription — 充值金额不作为订阅有效期（设计 §4.3, 裁决 1).
	ProductWalletTopup = "wallet-topup"
)

type Subscription struct {
	ID                     string     `db:"id" json:"id"`
	UserID                 string     `db:"user_id" json:"user_id"`
	PlanID                 string     `db:"plan_id" json:"plan_id"`
	ProductCode            string     `db:"product_code" json:"product_code"`
	Status                 string     `db:"status" json:"status"` // active/expired/cancelled
	StartedAt              time.Time  `db:"started_at" json:"started_at"`
	ExpiresAt              *time.Time `db:"expires_at" json:"expires_at"`
	ExternalSubscriptionID *string    `db:"external_subscription_id" json:"external_subscription_id,omitempty"` // PayPal subscription ID (`I-...`); NULL for non-PayPal subs
	CreatedAt              time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt              time.Time  `db:"updated_at" json:"updated_at"`
}
