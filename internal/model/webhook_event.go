package model

import (
	"encoding/json"
	"time"
)

// WebhookEvent is one per-event audit row written by the webhook handler.
// INSERT here happens BEFORE any business action — this is the event-level
// idempotency key (webhook doc §5.1).
//
// processed_at semantics:
//   - NULL      = queued, handler hasn't finished (or crashed mid-process)
//   - NOT NULL  = handler finished. This DOES NOT mean a domain action was
//                 taken — many event types are intentionally no-op
//                 (Stripe `payment_method.attached` etc.).
type WebhookEvent struct {
	ID          string          `db:"id" json:"id"`
	Channel     string          `db:"channel" json:"channel"`
	EventID     string          `db:"event_id" json:"event_id"`
	EventType   string          `db:"event_type" json:"event_type"`
	ReceivedAt  time.Time       `db:"received_at" json:"received_at"`
	ProcessedAt *time.Time      `db:"processed_at" json:"processed_at,omitempty"`
	RawPayload  json.RawMessage `db:"raw_payload" json:"raw_payload"`
}