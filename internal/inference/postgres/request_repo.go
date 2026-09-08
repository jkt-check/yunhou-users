package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// request_repo.go — 表组 inference_requests/attempts/usage_records
// (migration 026)。请求/尝试/计量的唯一键见 DDL；本文件只提供持久化
// 骨架（唯一键冲突 → CodeConflict）。

// InsertRequest persists a logical request. The ID is caller-generated
// (the gateway mints it after auth); a duplicate ID is a conflict — this
// is the request-level idempotency key (设计 §7.2).
func insertRequest(ctx context.Context, ex sqlxExecutor, r *domain.Request) error {
	status := string(r.Status)
	if status == "" {
		status = string(domain.ReqAuthenticated)
	}
	usageStatus := string(r.UsageStatus)
	if usageStatus == "" {
		usageStatus = string(domain.UsagePending)
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO inference_requests
		 (id, billing_account_id, api_key_id, entitlement_id, model_id,
		  protocol, stream, status, admitted_at, price_version_id, policy_version_id,
		  window_five_hour_id, window_weekly_id, window_monthly_id,
		  reserved_micros, usage_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		r.ID, r.BillingAccountID, strPtr(r.APIKeyID), r.EntitlementID, r.ModelID,
		string(r.Protocol), r.Stream, status, r.AdmittedAt, strPtr(r.PriceVersionID), r.PolicyVersionID,
		strPtr(r.WindowFiveHourID), strPtr(r.WindowWeeklyID), strPtr(r.WindowMonthlyID),
		microPtr(r.ReservedMicros), usageStatus)
	return mapError("insert request", err)
}

// InsertRequest inserts outside an open UnitOfWork (single-statement).
func (s *Store) InsertRequest(ctx context.Context, r *domain.Request) error {
	return insertRequest(ctx, s.db, r)
}

// requestStatusUpdate sets status (and optional error text) on a request.
func (s *Store) updateRequestStatus(ctx context.Context, ex sqlxExecutor, id string, status domain.RequestStatus, lastErr string) error {
	res, err := ex.ExecContext(ctx,
		`UPDATE inference_requests SET status = $2, last_error = $3, updated_at = now()
		 WHERE id = $1`, id, string(status), lastErr)
	if err != nil {
		return mapError("update request status", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return mapError("update request status", sql.ErrNoRows)
	}
	return nil
}

// GetRequest loads one logical request.
func (s *Store) GetRequest(ctx context.Context, id string) (*domain.Request, error) {
	var row struct {
		ID            string         `db:"id"`
		AccountID     string         `db:"billing_account_id"`
		APIKeyID      sql.NullString `db:"api_key_id"`
		EntitlementID string         `db:"entitlement_id"`
		ModelID       string         `db:"model_id"`
		Protocol      string         `db:"protocol"`
		Stream        bool           `db:"stream"`
		Status        string         `db:"status"`
		AdmittedAt    *time.Time     `db:"admitted_at"`
		PriceID       sql.NullString `db:"price_version_id"`
		PolicyID      string         `db:"policy_version_id"`
		Win5h         sql.NullString `db:"window_five_hour_id"`
		WinW          sql.NullString `db:"window_weekly_id"`
		WinM          sql.NullString `db:"window_monthly_id"`
		Reserved      sql.NullInt64  `db:"reserved_micros"`
		Settled       sql.NullInt64  `db:"settled_micros"`
		UsageStatus   string         `db:"usage_status"`
		LastError     string         `db:"last_error"`
		CreatedAt     time.Time      `db:"created_at"`
		UpdatedAt     time.Time      `db:"updated_at"`
		CompletedAt   *time.Time     `db:"completed_at"`
	}
	err := s.db.GetContext(ctx, &row, `SELECT * FROM inference_requests WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get request", err)
	}
	return &domain.Request{
		ID: row.ID, BillingAccountID: row.AccountID,
		APIKeyID: strFromNull(row.APIKeyID), EntitlementID: row.EntitlementID,
		ModelID: row.ModelID, Protocol: domain.Protocol(row.Protocol), Stream: row.Stream,
		Status: domain.RequestStatus(row.Status), AdmittedAt: row.AdmittedAt,
		PriceVersionID: strFromNull(row.PriceID), PolicyVersionID: row.PolicyID,
		WindowFiveHourID: strFromNull(row.Win5h), WindowWeeklyID: strFromNull(row.WinW),
		WindowMonthlyID: strFromNull(row.WinM),
		ReservedMicros:  microFromNull(row.Reserved), SettledMicros: microFromNull(row.Settled),
		UsageStatus: domain.UsageSource(row.UsageStatus), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt,
	}, nil
}

// InsertAttempt persists one upstream attempt. UNIQUE(request_id,
// attempt_no) is the attempt idempotency key.
func insertAttempt(ctx context.Context, ex sqlxExecutor, a *domain.Attempt) error {
	status := a.Status
	if status == "" {
		status = "dispatching"
	}
	var costMicros, costCurrency interface{}
	var costBasis interface{}
	if a.Cost != nil {
		costMicros = a.Cost.Micros
		costCurrency = a.Cost.Currency
	}
	if a.Basis != nil {
		costBasis = string(*a.Basis)
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO inference_attempts
		 (id, request_id, attempt_no, deployment_id, upstream_account_id, status,
		  upstream_request_id, cost_micros, cost_currency, cost_basis, started_at, finished_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.ID, a.RequestID, a.AttemptNo, strPtr(a.DeploymentID), strPtr(a.UpstreamAccountID), status,
		a.UpstreamRequestID, costMicros, costCurrency, costBasis, a.StartedAt, a.FinishedAt)
	return mapError("insert attempt", err)
}

// InsertAttempt inserts outside an open UnitOfWork.
func (s *Store) InsertAttempt(ctx context.Context, a *domain.Attempt) error {
	return insertAttempt(ctx, s.db, a)
}

// InsertAttemptTx is InsertAttempt inside an open UnitOfWork — the gateway
// persists the attempt intent in the SAME transaction as the upstream
// concurrency lease (设计 §7.2: 上游发送前持久化尝试意图与并发租约).
func (s *Store) InsertAttemptTx(ctx context.Context, w domain.UnitOfWork, a *domain.Attempt) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return insertAttempt(ctx, tx, a)
}

// UpdateRequestStatus moves a request through the §7.2 lifecycle outside an
// open UnitOfWork (single-statement). Terminal transitions that must commit
// with their effects (settled/released/reconciliation_required) have their
// own transactional paths — this is for the dispatch markers.
func (s *Store) UpdateRequestStatus(ctx context.Context, id string, status domain.RequestStatus, lastErr string) error {
	return s.updateRequestStatus(ctx, s.db, id, status, lastErr)
}

// FinishAttempt marks an attempt's terminal state (completed | failed |
// cancelled | unknown) with its error kind and the upstream request id when
// known. errorKind is a short classifier (e.g. "http_429", "transport",
// "payload") — never a raw upstream body.
func (s *Store) FinishAttempt(ctx context.Context, id, status, errorKind, upstreamRequestID string, finishedAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_attempts
		 SET status = $2, error_kind = $3, upstream_request_id = $4, finished_at = $5
		 WHERE id = $1`,
		id, status, errorKind, upstreamRequestID, finishedAt.UTC())
	if err != nil {
		return mapError("finish attempt", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("finish attempt", sql.ErrNoRows)
	}
	return nil
}

// InsertUsageRecord persists one normalized metering fact.
// UNIQUE(attempt_id, revision) is the usage-revision idempotency key.
// Unknown usage stores NULL buckets — never zero (设计 §7.1).
func insertUsageRecord(ctx context.Context, ex sqlxExecutor, u *domain.UsageRecord) error {
	raw := u.RawUsage.Raw
	if len(raw) == 0 {
		raw = json.RawMessage(`{"schema_version":1}`)
	}
	rev := u.Revision
	if rev == 0 {
		rev = 1
	}
	var id string
	err := ex.QueryRowxContext(ctx,
		`INSERT INTO inference_usage_records
		 (request_id, attempt_id, source,
		  input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
		  raw_usage, revision)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 RETURNING id`,
		u.RequestID, u.AttemptID, string(u.Source),
		int64PtrFromPtr(u.Buckets.InputTokens), int64PtrFromPtr(u.Buckets.CacheReadTokens),
		int64PtrFromPtr(u.Buckets.CacheWriteTokens), int64PtrFromPtr(u.Buckets.OutputTokens),
		int64PtrFromPtr(u.Buckets.ReasoningTokens), raw, rev).
		Scan(&id)
	if err != nil {
		return mapError("insert usage record", err)
	}
	u.ID = id
	return nil
}

// InsertUsageRecord inserts outside an open UnitOfWork.
func (s *Store) InsertUsageRecord(ctx context.Context, u *domain.UsageRecord) error {
	return insertUsageRecord(ctx, s.db, u)
}

func int64PtrFromPtr(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
