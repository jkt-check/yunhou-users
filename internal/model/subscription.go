package model

import "time"

type Subscription struct {
	ID                      string     `db:"id" json:"id"`
	UserID                  string     `db:"user_id" json:"user_id"`
	PlanID                  string     `db:"plan_id" json:"plan_id"`
	Status                  string     `db:"status" json:"status"` // active/expired/cancelled
	StartedAt               time.Time  `db:"started_at" json:"started_at"`
	ExpiresAt               *time.Time `db:"expires_at" json:"expires_at"`
	ExternalSubscriptionID  string     `db:"external_subscription_id" json:"external_subscription_id,omitempty"` // PayPal subscription ID (`I-...`)
	CreatedAt               time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt               time.Time  `db:"updated_at" json:"updated_at"`
}
