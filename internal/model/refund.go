package model

import "time"

// Refund is one refund event. A payment can be refunded multiple times
// (partial refunds sum to ≤ original amount — enforced at the service
// layer, NOT a DB constraint, since cross-row sum checks aren't trivial
// in Postgres).
//
// Idempotency:
//   - idempotency_key (caller HTTP Idempotency-Key) blocks caller retry
//   - (channel, external_refund_id) blocks the same refund event being
//     recorded twice from different sources
//
// See design doc §"Refund" + POST /refunds contract.
type Refund struct {
	ID               string    `db:"id" json:"id"`
	PaymentID        string    `db:"payment_id" json:"payment_id"`
	Channel          string    `db:"channel" json:"channel"` // denormalized from payments.channel for UNIQUE
	Amount           float64   `db:"amount" json:"amount"`
	Reason           *string   `db:"reason" json:"reason,omitempty"`
	IdempotencyKey   string    `db:"idempotency_key" json:"idempotency_key"`
	ExternalRefundID *string   `db:"external_refund_id" json:"external_refund_id,omitempty"` // NULL until channel returns
	Status           string    `db:"status" json:"status"` // pending / paid / failed (reserved)
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at" json:"updated_at"`
}