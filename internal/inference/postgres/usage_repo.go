package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// usage_repo.go — inference_usage_records/attempts 的读取面（Task 9）。
// 写入路径：insertUsageRecord（request_repo.go，结算同事务追加）与
// CorrectSettlement（reconciliation_repo.go，修正 revision 追加）。
// UNIQUE(attempt_id, revision) 是 usage revision 的幂等键（设计 §7.2）。

// ListAttempts loads every persisted attempt of a logical request, in
// attempt order — the recovery worker's evidence base for distinguishing
// 未发送 / 可能已发送 / 已知结果 / 未知费用 (设计 §7.2).
func (s *Store) ListAttempts(ctx context.Context, requestID string) ([]domain.Attempt, error) {
	var rows []struct {
		ID                string         `db:"id"`
		AttemptNo         int            `db:"attempt_no"`
		DeploymentID      sql.NullString `db:"deployment_id"`
		UpstreamAccountID sql.NullString `db:"upstream_account_id"`
		Status            string         `db:"status"`
		UpstreamRequestID string         `db:"upstream_request_id"`
		ErrorKind         string         `db:"error_kind"`
		CostMicros        sql.NullInt64  `db:"cost_micros"`
		CostCurrency      sql.NullString `db:"cost_currency"`
		CostBasis         sql.NullString `db:"cost_basis"`
		StartedAt         *time.Time     `db:"started_at"`
		FinishedAt        *time.Time     `db:"finished_at"`
		CreatedAt         time.Time      `db:"created_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id, attempt_no, deployment_id, upstream_account_id, status,
		        upstream_request_id, error_kind,
		        cost_micros, cost_currency, cost_basis, started_at, finished_at, created_at
		 FROM inference_attempts WHERE request_id = $1 ORDER BY attempt_no`, requestID); err != nil {
		return nil, mapError("list attempts", err)
	}
	out := make([]domain.Attempt, 0, len(rows))
	for _, r := range rows {
		a := domain.Attempt{
			ID: r.ID, RequestID: requestID, AttemptNo: r.AttemptNo,
			DeploymentID: strFromNull(r.DeploymentID), UpstreamAccountID: strFromNull(r.UpstreamAccountID),
			Status: r.Status, UpstreamRequestID: r.UpstreamRequestID,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt,
		}
		if r.CostMicros.Valid {
			m := domain.Money{Micros: r.CostMicros.Int64, Currency: r.CostCurrency.String}
			a.Cost = &m
		}
		if r.CostBasis.Valid {
			b := domain.CostBasis(r.CostBasis.String)
			a.Basis = &b
		}
		// error_kind 是恢复分类的依据之一（http_4xx=上游拒绝零消费；
		// stream_interrupted/client_gone=已产生流量），保留在 Attempt 外的
		// 行级读取中由 LoadAttemptErrorKinds 提供。
		out = append(out, a)
	}
	return out, nil
}

// AttemptErrorKinds returns attempt_id → error_kind for one request; the
// classification needs the kind (an HTTP rejection proves zero consumption,
// a mid-stream interruption does not) and domain.Attempt deliberately does
// not carry it.
func (s *Store) AttemptErrorKinds(ctx context.Context, requestID string) (map[string]string, error) {
	var rows []struct {
		ID        string `db:"id"`
		ErrorKind string `db:"error_kind"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id, error_kind FROM inference_attempts WHERE request_id = $1`, requestID); err != nil {
		return nil, mapError("attempt error kinds", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.ErrorKind
	}
	return out, nil
}

// LatestUsageRevision returns the highest persisted usage revision of an
// attempt (0 when none) — the next correction appends revision+1
// (UNIQUE(attempt_id, revision); 历史保留，不重写).
func (s *Store) LatestUsageRevision(ctx context.Context, attemptID string) (int, error) {
	var rev sql.NullInt64
	if err := s.db.QueryRowxContext(ctx,
		`SELECT MAX(revision) FROM inference_usage_records WHERE attempt_id = $1`,
		attemptID).Scan(&rev); err != nil {
		return 0, mapError("latest usage revision", err)
	}
	if !rev.Valid {
		return 0, nil
	}
	return int(rev.Int64), nil
}

// LoadUsageRecords loads every usage revision of a request, oldest first —
// the audit trail of estimates and their corrections.
func (s *Store) LoadUsageRecords(ctx context.Context, requestID string) ([]domain.UsageRecord, error) {
	var rows []struct {
		ID         string          `db:"id"`
		AttemptID  string          `db:"attempt_id"`
		Source     string          `db:"source"`
		Input      sql.NullInt64   `db:"input_tokens"`
		CacheRead  sql.NullInt64   `db:"cache_read_tokens"`
		CacheWrite sql.NullInt64   `db:"cache_write_tokens"`
		Output     sql.NullInt64   `db:"output_tokens"`
		Reasoning  sql.NullInt64   `db:"reasoning_tokens"`
		RawUsage   json.RawMessage `db:"raw_usage"`
		Revision   int             `db:"revision"`
		RecordedAt time.Time       `db:"recorded_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id, attempt_id, source, input_tokens, cache_read_tokens, cache_write_tokens,
		        output_tokens, reasoning_tokens, raw_usage, revision, recorded_at
		 FROM inference_usage_records WHERE request_id = $1
		 ORDER BY attempt_id, revision`, requestID); err != nil {
		return nil, mapError("load usage records", err)
	}
	out := make([]domain.UsageRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.UsageRecord{
			ID: r.ID, RequestID: requestID, AttemptID: r.AttemptID,
			Source: domain.UsageSource(r.Source),
			Buckets: domain.UsageBuckets{
				InputTokens:      nullInt64Ptr(r.Input),
				CacheReadTokens:  nullInt64Ptr(r.CacheRead),
				CacheWriteTokens: nullInt64Ptr(r.CacheWrite),
				OutputTokens:     nullInt64Ptr(r.Output),
				ReasoningTokens:  nullInt64Ptr(r.Reasoning),
			},
			RawUsage:   domain.ExtensionConfig{SchemaVersion: 1, Raw: r.RawUsage},
			Revision:   r.Revision,
			RecordedAt: r.RecordedAt,
		})
	}
	return out, nil
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}
