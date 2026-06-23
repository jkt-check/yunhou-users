package model

import (
	"encoding/json"
	"time"

	"github.com/lib/pq"
)

// AuditLog is an ops event written by the service layer. Webhooks do NOT
// write here — they write to webhook_events. AuditLog is for events the
// service layer itself emits (sweeper, late-payment, defensive transitions).
//
// Retention is unbounded in v1 (design doc §AuditLog); revisit when
// storage cost matters.
//
// Convention for free-text fields:
//   - actor  = "sweeper" / "service" / "user:<user_id>" / "admin:<app_id>"
//   - action = short verb-noun, e.g. "late_payment_post_expiry", "cancel_order",
//              "unexpected_state_transition"
//   - target = resource reference, e.g. "order:<order_id>"
type AuditLog struct {
	ID         string          `db:"id" json:"id"`
	OccurredAt time.Time       `db:"occurred_at" json:"occurred_at"`
	Actor      string          `db:"actor" json:"actor"`
	Action     string          `db:"action" json:"action"`
	Target     *string         `db:"target" json:"target,omitempty"`
	Tags       pq.StringArray  `db:"tags" json:"tags"`
	Context    json.RawMessage `db:"context" json:"context"`
}