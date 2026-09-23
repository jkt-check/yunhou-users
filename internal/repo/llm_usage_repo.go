package repo

import (
	"context"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// LLMUsageRepo is the storage surface for LLM token metering (migration
// 022): one insert per completed /chat upstream call, plus the per-model
// aggregate behind GET /admin/stats/llm-usage.
type LLMUsageRepo interface {
	// InsertEvent writes one metered chat call. No idempotency key: every
	// real upstream call is a real spend and must be counted, including
	// client retries (which produce a second real upstream call).
	InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error
	// SumByModel aggregates per logical model over the calendar range
	// [from, to] (YYYY-MM-DD, server timezone via created_at::date).
	SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error)
}

type llmUsageRepo struct{ db *sqlx.DB }

func NewLLMUsageRepo(db *sqlx.DB) *llmUsageRepo { return &llmUsageRepo{db: db} }

var _ LLMUsageRepo = (*llmUsageRepo)(nil)

func (r *llmUsageRepo) InsertEvent(ctx context.Context, ev model.LLMUsageEvent) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO llm_usage_events
		(user_id, app_id, model, provider, upstream_model, status, input_tokens, output_tokens, cost_micros)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ev.UserID, ev.AppID, ev.Model, ev.Provider, ev.UpstreamModel, ev.Status,
		ev.InputTokens, ev.OutputTokens, ev.CostMicros)
	return err
}

func (r *llmUsageRepo) SumByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	// Half-open range on raw TIMESTAMPTZ (sargable), same pattern as
	// usageRepo.CountNewUsers.
	const query = `SELECT model,
		COUNT(*) AS requests,
		COALESCE(SUM(input_tokens), 0)  AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cost_micros), 0)   AS cost_micros
		FROM llm_usage_events
		WHERE created_at >= $1::date AND created_at < $2::date + 1
		GROUP BY model ORDER BY cost_micros DESC`
	var rows []model.LLMUsageRow
	if err := r.db.SelectContext(ctx, &rows, query, from, to); err != nil {
		return nil, err
	}
	return rows, nil
}
