// customer_read_repo.go — Task 11 客户读模型的存储面（/user/model-quotas、
// /user/model-usage、/user/model-subscriptions）。
//
// 口径与边界：
//   - 用量只来自 inference_requests / inference_usage_records /
//     inference_ledger_entries；绝不读取 usage_events 心跳表（设计 §2/§9.2，
//     验收硬项）。
//   - subscriptions/plans 的读取是跨域只读（Task 10 outbox_repo.go 先例：
//     消费侧只读支付域事实，inference_* 表只由 inference 模块写）。
//   - 全部查询按 billing_account_id / user_id 限定 —— 只有本人可查自身
//     数据，不存在跨账户读取路径。

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/quota"
)

// Compile-time contract checks: the store satisfies the Task 11 customer
// read surfaces.
var (
	_ management.QuotaViewStore        = (*Store)(nil)
	_ management.UsageViewStore        = (*Store)(nil)
	_ management.SubscriptionViewStore = (*Store)(nil)
)

// ListEntitlements returns the account's entitlements in ANY status
// (retired included — the customer views render expired/revoked honestly).
// Ordering is the caller's business (management.SelectViewEntitlement /
// OrderViewEntitlements); here newest first for determinism.
func (s *Store) ListEntitlements(ctx context.Context, billingAccountID string) ([]domain.Entitlement, error) {
	var rows []entitlementRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_entitlements
		 WHERE billing_account_id = $1
		 ORDER BY created_at DESC, id DESC`, billingAccountID)
	if err != nil {
		return nil, mapError("list entitlements", err)
	}
	out := make([]domain.Entitlement, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}

// GetQuotaPolicy returns the pure policy shape of one immutable policy
// version (management 只依赖 quota.Policy 纯形状，不依赖存储行).
func (s *Store) GetQuotaPolicy(ctx context.Context, policyVersionID string) (quota.Policy, error) {
	p, err := s.GetPolicyVersion(ctx, policyVersionID)
	if err != nil {
		return quota.Policy{}, err
	}
	return p.Pure(), nil
}

// ---------------------------------------------------------------------------
// 用量汇总（分组 + 日序列）
// ---------------------------------------------------------------------------

// usageGroupColumn maps the grouping dimension to its SQL column. Internal
// constant only — never user input (no injection surface).
func usageGroupColumn(g management.UsageGroupBy) (string, error) {
	switch g {
	case management.GroupByModel:
		return "model_id", nil
	case management.GroupByKey:
		return "api_key_id", nil
	}
	return "", domain.NewError(domain.CodeInvalidInput, "usage: unknown group_by "+string(g))
}

// usageScopeClause renders the optional model/key narrowing with positional
// args appended to args (both are server-validated plain strings).
func usageScopeClause(f management.UsageSummaryFilter, args []interface{}) (string, []interface{}) {
	clause := ""
	if f.ModelID != "" {
		args = append(args, f.ModelID)
		clause += fmt.Sprintf(" AND r.model_id = $%d", len(args))
	}
	if f.APIKeyID != "" {
		args = append(args, f.APIKeyID)
		clause += fmt.Sprintf(" AND r.api_key_id = $%d", len(args))
	}
	return clause, args
}

// SummarizeUsageGroups implements management.UsageViewStore: per-group
// request counts, usage-integrity counts, charge sums (settled rows),
// reversal sums (ledger 冲正) and token sums (latest usage revision per
// attempt). NULL token sums stay NULL — unknown never collapses to zero.
func (s *Store) SummarizeUsageGroups(ctx context.Context, accountID string, f management.UsageSummaryFilter) ([]management.UsageGroup, error) {
	gcol, err := usageGroupColumn(f.GroupBy)
	if err != nil {
		return nil, err
	}
	scope, args := usageScopeClause(f, []interface{}{accountID, f.From, f.To})

	// 1) 请求计数 + 计量完整性 + charge 合计。
	keyCols := ""
	if f.GroupBy == management.GroupByKey {
		keyCols = ", k.name, k.key_prefix"
	}
	countsSQL := fmt.Sprintf(
		`SELECT r.%s AS gkey%s,
		        COUNT(*) AS requests_total,
		        COUNT(*) FILTER (WHERE r.status = 'settled') AS settled_requests,
		        COUNT(*) FILTER (WHERE r.status = 'released') AS released_requests,
		        COUNT(*) FILTER (WHERE r.status IN ('authenticated','reserved','dispatching','streaming','non_streaming','settling')) AS inflight_requests,
		        COUNT(*) FILTER (WHERE r.status = 'failed') AS failed_requests,
		        COUNT(*) FILTER (WHERE r.status = 'reconciliation_required') AS reconciliation_pending,
		        COUNT(*) FILTER (WHERE r.usage_status = 'reported') AS reported,
		        COUNT(*) FILTER (WHERE r.usage_status = 'estimated') AS estimated,
		        COUNT(*) FILTER (WHERE r.usage_status = 'unknown') AS unknown,
		        COUNT(*) FILTER (WHERE r.usage_status = 'pending') AS pending,
		        COALESCE(SUM(r.settled_micros) FILTER (WHERE r.status = 'settled'), 0) AS charge_micros
		   FROM inference_requests r
		   LEFT JOIN inference_api_keys k ON k.id = r.api_key_id
		  WHERE r.billing_account_id = $1 AND r.created_at >= $2 AND r.created_at < $3%s
		  GROUP BY r.%s%s
		  ORDER BY charge_micros DESC, gkey`, gcol, keyCols, scope, gcol, keyCols)
	type groupKey = string
	groups := make(map[groupKey]*management.UsageGroup)
	order := []groupKey{}
	var countRows []struct {
		Gkey         sql.NullString `db:"gkey"`
		KeyName      sql.NullString `db:"name"`
		KeyPrefix    sql.NullString `db:"key_prefix"`
		Total        int64          `db:"requests_total"`
		Settled      int64          `db:"settled_requests"`
		Released     int64          `db:"released_requests"`
		InFlight     int64          `db:"inflight_requests"`
		Failed       int64          `db:"failed_requests"`
		ReconPending int64          `db:"reconciliation_pending"`
		Reported     int64          `db:"reported"`
		Estimated    int64          `db:"estimated"`
		Unknown      int64          `db:"unknown"`
		Pending      int64          `db:"pending"`
		Charge       int64          `db:"charge_micros"`
	}
	if err := s.db.SelectContext(ctx, &countRows, countsSQL, args...); err != nil {
		return nil, mapError("usage groups: counts", err)
	}
	for _, r := range countRows {
		g := &management.UsageGroup{
			RequestsTotal: r.Total, SettledRequests: r.Settled, ReleasedRequests: r.Released,
			InFlightRequests: r.InFlight, FailedRequests: r.Failed, ReconciliationPending: r.ReconPending,
			Reported: r.Reported, Estimated: r.Estimated, Unknown: r.Unknown, Pending: r.Pending,
			ChargeMicros: r.Charge,
			KeyName:    stringFromNull(r.KeyName), KeyPrefix: stringFromNull(r.KeyPrefix),
		}
		if f.GroupBy == management.GroupByModel {
			id := r.Gkey.String
			g.ModelID = &id
		} else if r.Gkey.Valid {
			id := r.Gkey.String
			g.APIKeyID = &id
		}
		groups[r.Gkey.String] = g
		order = append(order, r.Gkey.String)
	}

	// 2) 账本冲正合计（按请求归属分组；账户级调整无 request_id，不进分组）。
	revSQL := fmt.Sprintf(
		`SELECT r.%s AS gkey, COALESCE(SUM(l.amount_micros), 0) AS reversed_micros
		   FROM inference_ledger_entries l
		   JOIN inference_requests r ON r.id = l.request_id
		  WHERE l.billing_account_id = $1 AND l.entry_type = 'reversal'
		    AND r.created_at >= $2 AND r.created_at < $3%s
		  GROUP BY r.%s`, gcol, scope, gcol)
	var revRows []struct {
		Gkey     sql.NullString `db:"gkey"`
		Reversed int64          `db:"reversed_micros"`
	}
	if err := s.db.SelectContext(ctx, &revRows, revSQL, args...); err != nil {
		return nil, mapError("usage groups: reversals", err)
	}
	for _, r := range revRows {
		if g, ok := groups[r.Gkey.String]; ok {
			g.ReversedMicros = r.Reversed
		}
	}

	// 3) token 合计：每个 attempt 取最新 revision（修正冲正历史保留，展示
	// 用最新事实），再按组求和；全 NULL → SUM 为 NULL（未知 ≠ 0）。
	tokSQL := fmt.Sprintf(
		`SELECT gkey,
		        SUM(input_tokens) AS input_tokens,
		        SUM(cache_read_tokens) AS cache_read_tokens,
		        SUM(cache_write_tokens) AS cache_write_tokens,
		        SUM(output_tokens) AS output_tokens,
		        SUM(reasoning_tokens) AS reasoning_tokens
		   FROM (
		     SELECT DISTINCT ON (u.attempt_id)
		            r.%s AS gkey,
		            u.input_tokens, u.cache_read_tokens, u.cache_write_tokens,
		            u.output_tokens, u.reasoning_tokens
		       FROM inference_usage_records u
		       JOIN inference_requests r ON r.id = u.request_id
		      WHERE r.billing_account_id = $1 AND r.created_at >= $2 AND r.created_at < $3%s
		      ORDER BY u.attempt_id, u.revision DESC
		   ) latest
		  GROUP BY gkey`, gcol, scope)
	var tokRows []struct {
		Gkey       sql.NullString `db:"gkey"`
		Input      sql.NullInt64  `db:"input_tokens"`
		CacheRead  sql.NullInt64  `db:"cache_read_tokens"`
		CacheWrite sql.NullInt64  `db:"cache_write_tokens"`
		Output     sql.NullInt64  `db:"output_tokens"`
		Reasoning  sql.NullInt64  `db:"reasoning_tokens"`
	}
	if err := s.db.SelectContext(ctx, &tokRows, tokSQL, args...); err != nil {
		return nil, mapError("usage groups: tokens", err)
	}
	for _, r := range tokRows {
		if g, ok := groups[r.Gkey.String]; ok {
			g.Tokens = domain.UsageBuckets{
				InputTokens:      nullInt64Ptr(r.Input),
				CacheReadTokens:  nullInt64Ptr(r.CacheRead),
				CacheWriteTokens: nullInt64Ptr(r.CacheWrite),
				OutputTokens:     nullInt64Ptr(r.Output),
				ReasoningTokens:  nullInt64Ptr(r.Reasoning),
			}
		}
	}

	out := make([]management.UsageGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out, nil
}

// SummarizeUsageSeries implements management.UsageViewStore: UTC calendar-day
// buckets over the in-range requests (date_trunc 在 UTC 壁钟上进行，与 DB
// 会话时区无关 —— 设计 §6 全部计量时间为服务端 UTC).
func (s *Store) SummarizeUsageSeries(ctx context.Context, accountID string, f management.UsageSummaryFilter) ([]management.UsageSeriesBucket, error) {
	scope, args := usageScopeClause(f, []interface{}{accountID, f.From, f.To})
	var rows []struct {
		BucketStart time.Time `db:"bucket_start"`
		Total       int64     `db:"requests_total"`
		Charge      int64     `db:"charge_micros"`
		Reported    int64     `db:"reported"`
		Estimated   int64     `db:"estimated"`
		Unknown     int64     `db:"unknown"`
	}
	err := s.db.SelectContext(ctx, &rows,
		`SELECT (date_trunc('day', r.created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS bucket_start,
		        COUNT(*) AS requests_total,
		        COALESCE(SUM(r.settled_micros) FILTER (WHERE r.status = 'settled'), 0) AS charge_micros,
		        COUNT(*) FILTER (WHERE r.usage_status = 'reported') AS reported,
		        COUNT(*) FILTER (WHERE r.usage_status = 'estimated') AS estimated,
		        COUNT(*) FILTER (WHERE r.usage_status = 'unknown') AS unknown
		   FROM inference_requests r
		  WHERE r.billing_account_id = $1 AND r.created_at >= $2 AND r.created_at < $3`+scope+`
		  GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, mapError("usage series", err)
	}
	out := make([]management.UsageSeriesBucket, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.UsageSeriesBucket{
			BucketStart: r.BucketStart.UTC(), RequestsTotal: r.Total, ChargeMicros: r.Charge,
			Reported: r.Reported, Estimated: r.Estimated, Unknown: r.Unknown,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 请求明细（keyset 分页）
// ---------------------------------------------------------------------------

// ListRequestRows implements management.UsageViewStore: at most f.Limit+1
// rows after the keyset cursor in (created_at DESC, id DESC) order. Each
// row is enriched with the latest usage revision per attempt (token sums)
// and the reversal sum from the ledger.
func (s *Store) ListRequestRows(ctx context.Context, accountID string, f management.RequestListFilter) ([]management.RequestRow, error) {
	args := []interface{}{accountID, f.From, f.To}
	query := `SELECT r.id, r.model_id, r.api_key_id, k.name AS key_name, k.key_prefix,
		        r.protocol, r.stream, r.status, r.usage_status,
		        r.reserved_micros, r.settled_micros,
		        r.created_at, r.admitted_at, r.completed_at
		   FROM inference_requests r
		   LEFT JOIN inference_api_keys k ON k.id = r.api_key_id
		  WHERE r.billing_account_id = $1 AND r.created_at >= $2 AND r.created_at < $3`
	if f.ModelID != "" {
		args = append(args, f.ModelID)
		query += fmt.Sprintf(" AND r.model_id = $%d", len(args))
	}
	if f.APIKeyID != "" {
		args = append(args, f.APIKeyID)
		query += fmt.Sprintf(" AND r.api_key_id = $%d", len(args))
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.CreatedAt, f.Cursor.ID)
		query += fmt.Sprintf(" AND (r.created_at < $%d OR (r.created_at = $%d AND r.id < $%d::uuid))",
			len(args)-1, len(args)-1, len(args))
	}
	args = append(args, f.Limit+1)
	query += fmt.Sprintf(" ORDER BY r.created_at DESC, r.id DESC LIMIT $%d", len(args))

	var rows []struct {
		ID          string         `db:"id"`
		ModelID     string         `db:"model_id"`
		APIKeyID    sql.NullString `db:"api_key_id"`
		KeyName     sql.NullString `db:"key_name"`
		KeyPrefix   sql.NullString `db:"key_prefix"`
		Protocol    string         `db:"protocol"`
		Stream      bool           `db:"stream"`
		Status      string         `db:"status"`
		UsageStatus string         `db:"usage_status"`
		Reserved    sql.NullInt64  `db:"reserved_micros"`
		Settled     sql.NullInt64  `db:"settled_micros"`
		CreatedAt   time.Time      `db:"created_at"`
		AdmittedAt  *time.Time     `db:"admitted_at"`
		CompletedAt *time.Time     `db:"completed_at"`
	}
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("usage requests", err)
	}
	out := make([]management.RequestRow, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.RequestRow{
			ID: r.ID, ModelID: r.ModelID, APIKeyID: strFromNull(r.APIKeyID),
			KeyName: stringFromNull(r.KeyName), KeyPrefix: stringFromNull(r.KeyPrefix),
			Protocol: r.Protocol, Stream: r.Stream,
			Status: domain.RequestStatus(r.Status), UsageStatus: domain.UsageSource(r.UsageStatus),
			ReservedMicros: microFromNull(r.Reserved), SettledMicros: microFromNull(r.Settled),
			CreatedAt: r.CreatedAt, AdmittedAt: r.AdmittedAt, CompletedAt: r.CompletedAt,
		})
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return out, nil
	}

	// Token enrichment: latest revision per attempt, summed per request.
	var tokRows []struct {
		RequestID  string         `db:"request_id"`
		Input      sql.NullInt64  `db:"input_tokens"`
		CacheRead  sql.NullInt64  `db:"cache_read_tokens"`
		CacheWrite sql.NullInt64  `db:"cache_write_tokens"`
		Output     sql.NullInt64  `db:"output_tokens"`
		Reasoning  sql.NullInt64  `db:"reasoning_tokens"`
	}
	if err := s.db.SelectContext(ctx, &tokRows,
		`SELECT request_id,
		        SUM(input_tokens) AS input_tokens,
		        SUM(cache_read_tokens) AS cache_read_tokens,
		        SUM(cache_write_tokens) AS cache_write_tokens,
		        SUM(output_tokens) AS output_tokens,
		        SUM(reasoning_tokens) AS reasoning_tokens
		   FROM (
		     SELECT DISTINCT ON (attempt_id) request_id,
		            input_tokens, cache_read_tokens, cache_write_tokens,
		            output_tokens, reasoning_tokens
		       FROM inference_usage_records
		      WHERE request_id = ANY($1)
		      ORDER BY attempt_id, revision DESC
		   ) latest
		  GROUP BY request_id`, pq.Array(ids)); err != nil {
		return nil, mapError("usage requests: tokens", err)
	}
	tokByReq := make(map[string]domain.UsageBuckets, len(tokRows))
	for _, r := range tokRows {
		tokByReq[r.RequestID] = domain.UsageBuckets{
			InputTokens:      nullInt64Ptr(r.Input),
			CacheReadTokens:  nullInt64Ptr(r.CacheRead),
			CacheWriteTokens: nullInt64Ptr(r.CacheWrite),
			OutputTokens:     nullInt64Ptr(r.Output),
			ReasoningTokens:  nullInt64Ptr(r.Reasoning),
		}
	}

	// Reversal enrichment (账本冲正合计，按请求归属).
	var revRows []struct {
		RequestID string `db:"request_id"`
		Reversed  int64  `db:"reversed_micros"`
	}
	if err := s.db.SelectContext(ctx, &revRows,
		`SELECT request_id, COALESCE(SUM(amount_micros), 0) AS reversed_micros
		   FROM inference_ledger_entries
		  WHERE entry_type = 'reversal' AND request_id = ANY($1)
		  GROUP BY request_id`, pq.Array(ids)); err != nil {
		return nil, mapError("usage requests: reversals", err)
	}
	revByReq := make(map[string]int64, len(revRows))
	for _, r := range revRows {
		revByReq[r.RequestID] = r.Reversed
	}

	for i := range out {
		if b, ok := tokByReq[out[i].ID]; ok {
			out[i].HasUsage = true
			out[i].Tokens = b
		}
		out[i].ReversedMicros = revByReq[out[i].ID]
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 订阅读取（跨域只读，Task 10 先例）
// ---------------------------------------------------------------------------

// ListProductSubscriptions returns the user's subscriptions of one product
// (all statuses, newest first) joined with the plan display name.
// Cross-domain READ-ONLY: subscriptions/plans rows are payment-domain
// facts; the inference module never writes them (设计 §3 模块边界).
func (s *Store) ListProductSubscriptions(ctx context.Context, userID, productCode string) ([]management.ProductSubscription, error) {
	var rows []struct {
		ID          string         `db:"id"`
		PlanID      string         `db:"plan_id"`
		PlanName    sql.NullString `db:"plan_name"`
		ProductCode string         `db:"product_code"`
		Status      string         `db:"status"`
		StartedAt   time.Time      `db:"started_at"`
		ExpiresAt   *time.Time     `db:"expires_at"`
		CreatedAt   time.Time      `db:"created_at"`
	}
	err := s.db.SelectContext(ctx, &rows,
		`SELECT s.id, s.plan_id, p.name AS plan_name, s.product_code, s.status,
		        s.started_at, s.expires_at, s.created_at
		   FROM subscriptions s
		   LEFT JOIN plans p ON p.id = s.plan_id
		  WHERE s.user_id = $1 AND s.product_code = $2
		  ORDER BY s.created_at DESC, s.id DESC`, userID, productCode)
	if err != nil {
		return nil, mapError("list product subscriptions", err)
	}
	out := make([]management.ProductSubscription, 0, len(rows))
	for _, r := range rows {
		sub := management.ProductSubscription{
			ID: r.ID, PlanID: r.PlanID, ProductCode: r.ProductCode, Status: r.Status,
			StartedAt: r.StartedAt, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
		}
		if r.PlanName.Valid {
			name := r.PlanName.String
			sub.PlanName = &name
		}
		out = append(out, sub)
	}
	return out, nil
}
