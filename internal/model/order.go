package model

import (
	"encoding/json"
	"time"
)

// Order is a pre-payment intent owned by yunhou-users.
// Created when a user picks a paid plan, BEFORE the frontend opens
// the channel SDK to actually collect money.
//
// See design doc §"Order" + webhook doc §3 for lifecycle.
type Order struct {
	ID        string    `db:"id" json:"id"`
	UserID    string    `db:"user_id" json:"user_id"`
	PlanID    string    `db:"plan_id" json:"plan_id"`
	Amount    float64   `db:"amount" json:"amount"`         // major currency units (e.g. 29.90 CNY)
	Currency  string    `db:"currency" json:"currency"`     // ISO 4217 (3 chars)
	Status    string    `db:"status" json:"status"`         // pending / paid / failed / refunded / cancelled / expired
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"` // 30 min default; sweeper flips pending→expired after this
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
	// ProviderIntent holds per-channel metadata written after a
	// channel-specific pre-auth (wechat_pay → {appid, mchid, code_url,
	// out_trade_no}; paypal/alipay populate their own keys). Exposed via
	// json with omitempty so orders without a pre-auth payload don't
	// carry an empty field in the response; the BFF reads `code_url`
	// from here for the WeChat QR render. json.RawMessage is a []byte
	// under the hood, so sqlx scans the JSONB column directly into it
	// without an extra Unmarshal round-trip.
	ProviderIntent json.RawMessage `db:"provider_intent" json:"provider_intent,omitempty"`
}
