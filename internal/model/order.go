package model

import "time"

// Order is a pre-payment intent owned by yunhou-users.
// Created when a user picks a paid plan, BEFORE the frontend opens
// the channel SDK to actually collect money.
//
// See design doc §"Order" + webhook doc §3 for lifecycle.
type Order struct {
	ID             string    `db:"id" json:"id"`
	UserID         string    `db:"user_id" json:"user_id"`
	PlanID         string    `db:"plan_id" json:"plan_id"`
	Amount         float64   `db:"amount" json:"amount"`         // major currency units (e.g. 29.90 CNY)
	Currency       string    `db:"currency" json:"currency"`     // ISO 4217 (3 chars)
	Status         string    `db:"status" json:"status"`         // pending / paid / failed / refunded / cancelled / expired
	ExpiresAt      time.Time `db:"expires_at" json:"expires_at"` // 30 min default; sweeper flips pending→expired after this
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
	ProviderIntent []byte    `db:"provider_intent" json:"-"` // raw JSONB bytes; wechat_pay: {code_url, out_trade_no, mch_id}; never returned to clients
}