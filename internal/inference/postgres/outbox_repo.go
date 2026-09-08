package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// outbox_repo.go — 表组 inference_outbox/reconciliation_jobs
// (migration 026)。事务 outbox：Enqueue 必须在业务写入的同一
// UnitOfWork 内调用（设计 §3: 异步副作用使用事务 outbox）。

// EnqueueOutbox appends one outbox message inside the caller's
// transaction. dedupKey (optional) makes re-delivery idempotent.
func (s *Store) EnqueueOutbox(ctx context.Context, w domain.UnitOfWork, topic string, payload json.RawMessage, dedupKey *string) (int64, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_outbox (topic, payload, dedup_key)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (dedup_key) DO NOTHING
		 RETURNING id`, topic, payload, dedupKey).Scan(&id)
	if err != nil {
		// Duplicate dedup key: idempotent no-op (设计 §3 outbox 幂等).
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, mapError("outbox: enqueue", err)
	}
	return id, nil
}

// FetchPendingOutbox returns up to limit due messages oldest-first.
func (s *Store) FetchPendingOutbox(ctx context.Context, limit int) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []struct {
		ID        int64           `db:"id"`
		Topic     string          `db:"topic"`
		Payload   json.RawMessage `db:"payload"`
		Attempts  int             `db:"attempts"`
		CreatedAt time.Time       `db:"created_at"`
	}
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, topic, payload, attempts, created_at FROM inference_outbox
		 WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= now())
		 ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, mapError("outbox: fetch", err)
	}
	out := make([]OutboxMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, OutboxMessage{
			ID: r.ID, Topic: r.Topic, Payload: r.Payload, Attempts: r.Attempts, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// MarkOutboxDelivered finalizes a message.
func (s *Store) MarkOutboxDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inference_outbox SET status = 'delivered', delivered_at = now() WHERE id = $1`, id)
	return mapError("outbox: deliver", err)
}

// MarkOutboxFailed records a failed attempt and schedules the retry.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, nextRetry time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inference_outbox
		 SET attempts = attempts + 1, next_retry_at = $2 WHERE id = $1`, id, nextRetry)
	return mapError("outbox: fail", err)
}

// OutboxMessage is one pending outbox row.
type OutboxMessage struct {
	ID        int64
	Topic     string
	Payload   json.RawMessage
	Attempts  int
	CreatedAt time.Time
}
