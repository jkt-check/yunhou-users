package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/access"
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
	return enqueueOutboxSQLTx(ctx, tx, topic, payload, dedupKey)
}

// EnqueueOutboxSQLTx is the enqueue variant for callers that hold a bare
// *sqlx.Tx instead of a domain.UnitOfWork — the payment service's webhook /
// Confirm transactions (service/payment.go) use it to make the
// entitlement-sync message commit atomically with the payment state
// transition it announces (Task 10: 同事务 outbox).
func (s *Store) EnqueueOutboxSQLTx(ctx context.Context, tx *sqlx.Tx, topic string, payload json.RawMessage, dedupKey *string) (int64, error) {
	if tx == nil {
		return 0, fmt.Errorf("postgres: outbox: nil *sqlx.Tx")
	}
	return enqueueOutboxSQLTx(ctx, tx, topic, payload, dedupKey)
}

func enqueueOutboxSQLTx(ctx context.Context, tx *sqlx.Tx, topic string, payload json.RawMessage, dedupKey *string) (int64, error) {
	var id int64
	err := tx.QueryRowxContext(ctx,
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
	return s.fetchPendingOutbox(ctx, "", limit)
}

// FetchPendingOutboxByTopic is FetchPendingOutbox restricted to one topic,
// so the entitlement-sync worker (Task 10) never interleaves with future
// topics on the same table.
func (s *Store) FetchPendingOutboxByTopic(ctx context.Context, topic string, limit int) ([]OutboxMessage, error) {
	return s.fetchPendingOutbox(ctx, topic, limit)
}

func (s *Store) fetchPendingOutbox(ctx context.Context, topic string, limit int) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, topic, payload, attempts, created_at FROM inference_outbox
		 WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= now())`
	args := []interface{}{}
	if topic != "" {
		query += ` AND topic = $1`
		args = append(args, topic)
		query += ` ORDER BY id LIMIT $2`
		args = append(args, limit)
	} else {
		query += ` ORDER BY id LIMIT $1`
		args = append(args, limit)
	}
	var rows []struct {
		ID        int64           `db:"id"`
		Topic     string          `db:"topic"`
		Payload   json.RawMessage `db:"payload"`
		Attempts  int             `db:"attempts"`
		CreatedAt time.Time       `db:"created_at"`
	}
	err := s.db.SelectContext(ctx, &rows, query, args...)
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

// MarkOutboxDeliveredTx finalizes a message inside the caller's
// UnitOfWork. The entitlement-sync worker uses it so the entitlement
// mutation and the delivery mark commit atomically — a crash between them
// redelivers the message, and convergence makes that replay a no-op.
func (s *Store) MarkOutboxDeliveredTx(ctx context.Context, w domain.UnitOfWork, id int64) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
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

// ---------------------------------------------------------------------------
// Task 10：支付状态读取面 —— entitlement_sync worker 的收敛输入
// ---------------------------------------------------------------------------
// 消费侧只读支付域的当前事实（subscriptions / orders），读取快照由
// access.DecideSync 纯规则收敛为目标权益；乱序/重复消息因此无害。
// 跨域只读不写：inference_* 表仍只由 inference 模块写（设计 §3 模块边界）。

// GetSyncSubscription returns the user's CURRENT subscription row for one
// product — the same row activateSubscriptionOnTx maintains (active first,
// else most recently created; the 027 partial unique index guarantees at
// most one active). sql.ErrNoRows when the user never had one.
func (s *Store) GetSyncSubscription(ctx context.Context, userID, productCode string) (*access.SubscriptionState, error) {
	var row struct {
		ID          string     `db:"id"`
		UserID      string     `db:"user_id"`
		PlanID      string     `db:"plan_id"`
		ProductCode string     `db:"product_code"`
		Status      string     `db:"status"`
		ExpiresAt   *time.Time `db:"expires_at"`
	}
	err := s.db.GetContext(ctx, &row, `
		SELECT id, user_id, plan_id, product_code, status, expires_at
		  FROM subscriptions
		 WHERE user_id = $1 AND product_code = $2
		 ORDER BY CASE WHEN status = 'active' THEN 0 ELSE 1 END, created_at DESC
		 LIMIT 1
	`, userID, productCode)
	if err != nil {
		return nil, mapError("outbox: sync subscription", err)
	}
	return &access.SubscriptionState{
		ID: row.ID, UserID: row.UserID, PlanID: row.PlanID,
		ProductCode: row.ProductCode, Status: row.Status, ExpiresAt: row.ExpiresAt,
	}, nil
}

// GetLatestPaidBenefitOrder returns the benefit snapshot of the user's
// LATEST paid order for one plan that carries one. It is the payment
// evidence + spec source for the entitlement: only orders frozen with a
// benefit snapshot (migration 029) qualify, a fully-refunded order
// (status='refunded') drops out, and a stale blocked order for a different
// plan never matches the subscription's current plan_id. sql.ErrNoRows
// when no qualifying order exists — the caller treats that as "no payment
// evidence, grant nothing".
func (s *Store) GetLatestPaidBenefitOrder(ctx context.Context, userID, planID string) (*access.OrderBenefitSnapshot, error) {
	var row struct {
		OrderID         string         `db:"id"`
		PolicyVersionID string         `db:"benefit_policy_version_id"`
		ModelIDs        pq.StringArray `db:"benefit_model_ids"`
		GrantMode       string         `db:"benefit_grant_mode"`
	}
	err := s.db.GetContext(ctx, &row, `
		SELECT id, benefit_policy_version_id, benefit_model_ids, benefit_grant_mode
		  FROM orders
		 WHERE user_id = $1 AND plan_id = $2 AND status = 'paid'
		   AND benefit_policy_version_id IS NOT NULL
		 ORDER BY updated_at DESC, created_at DESC
		 LIMIT 1
	`, userID, planID)
	if err != nil {
		return nil, mapError("outbox: latest paid benefit order", err)
	}
	return &access.OrderBenefitSnapshot{
		OrderID:         row.OrderID,
		PolicyVersionID: row.PolicyVersionID,
		ModelIDs:        []string(row.ModelIDs),
		GrantMode:       row.GrantMode,
	}, nil
}
