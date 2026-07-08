package model

import (
	"encoding/json"
	"time"
)

// Payment is a channel-side transaction bound to one Order.
// One Order can have at most one paid Payment (enforced by partial
// unique index idx_payments_one_paid_per_order at the DB layer).
//
// See design doc §"Payment" + webhook doc §5.2 for business-level
// idempotency on (channel, external_txn_id).
type Payment struct {
	ID            string          `db:"id" json:"id"`
	OrderID       string          `db:"order_id" json:"order_id"`
	Channel       string          `db:"channel" json:"channel"` // stripe / wechat_pay / alipay / paypal
	ExternalTxnID string          `db:"external_txn_id" json:"external_txn_id"`
	Amount        float64         `db:"amount" json:"amount"`     // major currency units
	Currency      string          `db:"currency" json:"currency"` // ISO 4217
	Status        string          `db:"status" json:"status"`     // pending / paid / failed / refunded
	PaidAt        *time.Time      `db:"paid_at" json:"paid_at,omitempty"`
	FailedReason  *string         `db:"failed_reason" json:"failed_reason,omitempty"`
	Disputed      bool            `db:"disputed" json:"disputed"`
	DisputedAt    *time.Time      `db:"disputed_at" json:"disputed_at,omitempty"`
	RawPayload    json.RawMessage `db:"raw_payload" json:"raw_payload"`
	CreatedAt     time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time       `db:"updated_at" json:"updated_at"`
}