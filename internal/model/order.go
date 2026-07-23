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
	// LastReconciledAt is stamped whenever GetOrder drives the active
	// channel-query reconcile path (wechat_pay, non-mock). Used as a
	// rate-limit guard so 500ms FE polls collapse to ~1 outbound call
	// per reconcileMinInterval per order. Not surfaced in JSON — it's
	// an internal reconcile timestamp, not a user-visible field.
	LastReconciledAt time.Time `db:"last_reconciled_at" json:"-"`
	// ProviderIntent holds per-channel metadata written after a
	// channel-specific pre-auth (wechat_pay → {appid, mchid, code_url,
	// out_trade_no}; paypal/alipay populate their own keys). Exposed via
	// json with omitempty so orders without a pre-auth payload don't
	// carry an empty field in the response; the BFF reads `code_url`
	// from here for the WeChat QR render.
	//
	// Pointer so a SQL NULL column (set after migration
	// 010_provider_intent_nullable) scans into a nil *json.RawMessage,
	// and omitempty on a nil pointer fires — without the pointer, sqlx
	// can't scan NULL into a []byte (would error with "unsupported
	// Scan, storing driver.Value type <nil> into type *json.RawMessage").
	ProviderIntent *json.RawMessage `db:"provider_intent" json:"provider_intent,omitempty"`
}
