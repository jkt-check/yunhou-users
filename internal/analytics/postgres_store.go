package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
)

// PostgresStore is the analytics_outbox implementation of OutboxStore
// (migration 046). SaveFailed is INSERT ... ON CONFLICT (uuid) DO NOTHING:
// the event UUID is the idempotency key, so a repeated failure of the
// same event never produces a second row.
type PostgresStore struct {
	db *sqlx.DB
}

// NewPostgresStore builds the store on an existing pool.
func NewPostgresStore(db *sqlx.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// SaveFailed persists one failed delivery. Payload is the full Event JSON
// so a redelivery carries the same event (same UUID — PostHog dedupes on
// uuid, not bytes; JSONB normalizes key order/format, which is harmless).
func (s *PostgresStore) SaveFailed(ctx context.Context, evt Event) error {
	if evt.UUID == "" {
		return errors.New("analytics outbox: event UUID is empty — refusing to persist (the UUID is the idempotency key; an empty one would swallow every later empty-UUID failure)")
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO analytics_outbox (uuid, event_name, distinct_id, payload)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (uuid) DO NOTHING
	`, evt.UUID, evt.Name, evt.DistinctID, payload)
	return err
}

// ListDue returns up to limit unsent events whose next attempt is due,
// oldest first. The row lock (FOR UPDATE SKIP LOCKED) is held only for
// the scan: the tx commits immediately, BEFORE the caller sends, so it
// does NOT serialize redeliveries — two overlapping workers can claim the
// same row. That is fine: MarkSent/Reschedule guard with
// WHERE sent_at IS NULL, and the real double-send protection is
// PostHog's own UUID dedup — a redelivery carries the same event UUID as
// the first attempt, so even a double-send is deduplicated upstream.
// A row whose payload no longer decodes (e.g. a schema drift) is
// dead-lettered in place and skipped, so one bad row can never wedge the
// whole queue.
func (s *PostgresStore) ListDue(ctx context.Context, limit int) ([]OutboxEvent, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin list-due tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT uuid, event_name, distinct_id, payload, attempts
		FROM analytics_outbox
		WHERE sent_at IS NULL AND next_attempt_at <= now()
		ORDER BY next_attempt_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("select due: %w", err)
	}
	var out []OutboxEvent
	var undecodable []string
	for rows.Next() {
		var (
			oe      OutboxEvent
			payload []byte
		)
		if err := rows.Scan(&oe.UUID, &oe.Name, &oe.DistinctID, &payload, &oe.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan due row: %w", err)
		}
		if err := json.Unmarshal(payload, &oe.Event); err != nil {
			// Payload written by SaveFailed always decodes; a failure here
			// means schema drift or manual tampering. Dead-letter the row
			// (parked past every real timestamp) so it can never wedge the
			// queue — the other rows in this batch still go out.
			log.Printf("analytics: outbox payload %s undecodable (%v) — dead-lettering", oe.UUID, err)
			undecodable = append(undecodable, oe.UUID)
			continue
		}
		out = append(out, oe)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate due rows: %w", err)
	}
	rows.Close()

	// Dead-letter undecodable rows while their locks are still held by
	// this tx.
	for _, uuid := range undecodable {
		if _, err := tx.ExecContext(ctx, `
			UPDATE analytics_outbox SET next_attempt_at = $2
			WHERE uuid = $1 AND sent_at IS NULL
		`, uuid, retryInfinity); err != nil {
			return nil, fmt.Errorf("dead-letter undecodable row %s: %w", uuid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list-due tx: %w", err)
	}
	return out, nil
}

// MarkSent records a successful redelivery. Conditional on sent_at IS
// NULL so a late duplicate claim is a no-op.
func (s *PostgresStore) MarkSent(ctx context.Context, eventUUID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE analytics_outbox SET sent_at = now()
		WHERE uuid = $1 AND sent_at IS NULL
	`, eventUUID)
	return err
}

// Reschedule records a failed attempt and the next due time. Dead-lettered
// rows carry the far-future retryInfinity so they leave the due index.
func (s *PostgresStore) Reschedule(ctx context.Context, eventUUID string, attempts int, nextAttemptAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE analytics_outbox
		SET attempts = $2, next_attempt_at = $3
		WHERE uuid = $1 AND sent_at IS NULL
	`, eventUUID, attempts, nextAttemptAt)
	return err
}
