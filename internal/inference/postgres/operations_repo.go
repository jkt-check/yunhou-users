// operations_repo.go — Task 15 运营读模型的存储面（聚合统计 / 异常筛选 /
// 补偿追踪 / 变更预览的受影响面）。
//
// 口径与边界（与 management/operations.go 头部注释同一契约）：
//   - 数据源只有 inference_requests / inference_attempts /
//     inference_usage_records / inference_ledger_entries —— 绝不读
//     usage_events 心跳表。
//   - 账本派生金额与 Task 11 客户面逐字一致（charge 毛额 + reversal +
//     请求级 adjustment 签名合计）；供应商分组不归属请求级账本金额
//     （一次请求可能跨供应商重试，金额按供应商拆开是编造——金额权威
//     维度是模型/客户）。
//   - 供应商分组的请求级计数按"最终尝试"（attempt_no 最大者）的部署归
//     属——不重不漏；尝试级指标（延迟/成本/尝试数）按每次尝试自己的
//     部署归属。
//   - 全部只读：统计不触碰权威事务状态，不为统计引入共享锁（migration
//     034 的 created_at 索引服务这些范围扫描）。

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// Compile-time contract checks.
var (
	_ management.OperationsStore     = (*Store)(nil)
	_ management.PricingPreviewStore = (*Store)(nil)
	_ management.BulkImportStore     = (*Store)(nil)
)

// ---------------------------------------------------------------------------
// 运营聚合统计
// ---------------------------------------------------------------------------

// opsScopeClause renders the optional narrowing (model/provider/account).
func opsScopeClause(f management.OpsUsageFilter, args []interface{}, alias string) (string, []interface{}) {
	clause := ""
	if f.ModelID != "" {
		args = append(args, f.ModelID)
		clause += fmt.Sprintf(" AND %s.model_id = $%d", alias, len(args))
	}
	if f.AccountID != "" {
		args = append(args, f.AccountID)
		clause += fmt.Sprintf(" AND %s.billing_account_id = $%d", alias, len(args))
	}
	return clause, args
}

// opsGroupKey is the grouping SQL expression + join needed per dimension.
// The request-level counts attribute to the FINAL attempt's deployment for
// the provider dimension (见文件头注释).
func opsGroupBySQL(g management.OpsGroupBy) (keyExpr string, fromJoin string, err error) {
	switch g {
	case management.OpsGroupModel:
		return "r.model_id", `FROM inference_requests r`, nil
	case management.OpsGroupCustomer:
		return "r.billing_account_id::text", `FROM inference_requests r`, nil
	case management.OpsGroupProvider:
		return "pd.provider_id::text",
			`FROM inference_requests r
			 JOIN LATERAL (
			   SELECT a.deployment_id FROM inference_attempts a
			   WHERE a.request_id = r.id ORDER BY a.attempt_no DESC LIMIT 1
			 ) fa ON true
			 JOIN inference_deployments pd ON pd.id = fa.deployment_id`, nil
	}
	return "", "", domain.NewError(domain.CodeInvalidInput, "ops usage: unknown group_by "+string(g))
}

// SummarizeOpsUsage implements management.OperationsStore: four query legs
// (counts / ledger / tokens / attempts) merged in Go — the same shape as
// the customer read repo, plus latency and cost shards.
func (s *Store) SummarizeOpsUsage(ctx context.Context, f management.OpsUsageFilter) ([]management.OpsGroup, error) {
	keyExpr, fromJoin, err := opsGroupBySQL(f.GroupBy)
	if err != nil {
		return nil, err
	}
	scope, args := opsScopeClause(f, []interface{}{f.From, f.To}, "r")
	// provider 维度的 provider_id 收敛（部署维度的过滤只能打在 join 上）。
	if f.ProviderID != "" && f.GroupBy == management.OpsGroupProvider {
		args = append(args, f.ProviderID)
		scope += fmt.Sprintf(" AND pd.provider_id = $%d", len(args))
	}

	groups := map[string]*management.OpsGroup{}
	order := []string{}
	ensure := func(key string) *management.OpsGroup {
		if g, ok := groups[key]; ok {
			return g
		}
		g := &management.OpsGroup{CostSlices: []management.CostSlice{}}
		switch f.GroupBy {
		case management.OpsGroupModel:
			g.ModelID = key
		case management.OpsGroupCustomer:
			g.AccountID = key
		case management.OpsGroupProvider:
			g.ProviderID = key
		}
		groups[key] = g
		order = append(order, key)
		return g
	}

	// 1) 请求计数 + 计量完整性（供应商维度 = 最终尝试归属）。
	countsSQL := fmt.Sprintf(
		`SELECT %s AS gkey,
		        COUNT(*) AS requests_total,
		        COUNT(*) FILTER (WHERE r.status = 'settled') AS settled_requests,
		        COUNT(*) FILTER (WHERE r.status = 'released') AS released_requests,
		        COUNT(*) FILTER (WHERE r.status IN ('authenticated','reserved','dispatching','streaming','non_streaming','settling')) AS inflight_requests,
		        COUNT(*) FILTER (WHERE r.status = 'failed') AS failed_requests,
		        COUNT(*) FILTER (WHERE r.status = 'reconciliation_required') AS reconciliation_pending,
		        COUNT(*) FILTER (WHERE r.usage_status = 'reported') AS reported,
		        COUNT(*) FILTER (WHERE r.usage_status = 'estimated') AS estimated,
		        COUNT(*) FILTER (WHERE r.usage_status = 'unknown') AS unknown,
		        COUNT(*) FILTER (WHERE r.usage_status = 'pending') AS pending
		   %s
		  WHERE r.created_at >= $1 AND r.created_at < $2%s
		  GROUP BY %s ORDER BY requests_total DESC, gkey`, keyExpr, fromJoin, scope, keyExpr)
	var countRows []struct {
		Gkey         string `db:"gkey"`
		Total        int64  `db:"requests_total"`
		Settled      int64  `db:"settled_requests"`
		Released     int64  `db:"released_requests"`
		InFlight     int64  `db:"inflight_requests"`
		Failed       int64  `db:"failed_requests"`
		ReconPending int64  `db:"reconciliation_pending"`
		Reported     int64  `db:"reported"`
		Estimated    int64  `db:"estimated"`
		Unknown      int64  `db:"unknown"`
		Pending      int64  `db:"pending"`
	}
	if err := s.db.SelectContext(ctx, &countRows, countsSQL, args...); err != nil {
		return nil, mapError("ops usage: counts", err)
	}
	for _, r := range countRows {
		g := ensure(r.Gkey)
		g.RequestsTotal, g.SettledRequests, g.ReleasedRequests = r.Total, r.Settled, r.Released
		g.InFlightRequests, g.FailedRequests, g.ReconciliationPending = r.InFlight, r.Failed, r.ReconPending
		g.Reported, g.Estimated, g.Unknown, g.Pending = r.Reported, r.Estimated, r.Unknown, r.Pending
		if terminal := r.Settled + r.Released + r.Failed; terminal > 0 {
			rate := float64(r.Settled+r.Released) / float64(terminal)
			rate = float64(int64(rate*10000+0.5)) / 10000
			g.SuccessRate = &rate
		}
	}

	// 2) 账本派生金额（microcredit；供应商维度不归属——见文件头注释）。
	if f.GroupBy != management.OpsGroupProvider {
		ledSQL := fmt.Sprintf(
			`SELECT %s AS gkey,
			        COALESCE(SUM(l.amount_micros) FILTER (WHERE l.entry_type = 'charge'), 0) AS charge_micros,
			        COALESCE(SUM(l.amount_micros) FILTER (WHERE l.entry_type = 'reversal'), 0) AS reversed_micros,
			        COALESCE(SUM(CASE WHEN a.direction = 'debit' THEN l.amount_micros
			                          WHEN a.direction = 'credit' THEN -l.amount_micros END)
			                 FILTER (WHERE l.entry_type = 'adjustment'), 0) AS adjusted_micros
			   FROM inference_ledger_entries l
			   JOIN inference_requests r ON r.id = l.request_id
			   LEFT JOIN inference_adjustments a ON a.id = l.adjustment_id
			  WHERE l.unit = 'microcredit' AND r.created_at >= $1 AND r.created_at < $2%s
			  GROUP BY %s`, keyExpr, scope, keyExpr)
		var ledRows []struct {
			Gkey     string `db:"gkey"`
			Charge   int64  `db:"charge_micros"`
			Reversed int64  `db:"reversed_micros"`
			Adjusted int64  `db:"adjusted_micros"`
		}
		if err := s.db.SelectContext(ctx, &ledRows, ledSQL, args...); err != nil {
			return nil, mapError("ops usage: ledger", err)
		}
		for _, r := range ledRows {
			g := ensure(r.Gkey)
			g.ChargeMicros, g.ReversedMicros, g.AdjustedMicros = r.Charge, r.Reversed, r.Adjusted
		}
	}

	// 3) token 合计（每 attempt 最新 revision；供应商维度按尝试的部署归属）。
	tokKey := keyExpr
	tokFrom := `
		   FROM (
		     SELECT DISTINCT ON (u.attempt_id)
		            %s AS gkey,
		            u.input_tokens, u.cache_read_tokens, u.cache_write_tokens,
		            u.output_tokens, u.reasoning_tokens
		       FROM inference_usage_records u
		       JOIN inference_requests r ON r.id = u.request_id
		       %s
		      WHERE r.created_at >= $1 AND r.created_at < $2%s
		      ORDER BY u.attempt_id, u.revision DESC
		   ) latest`
	tokJoin := ""
	if f.GroupBy == management.OpsGroupProvider {
		// 尝试级归属：token 属于产生它的尝试的部署。
		tokKey = "td.provider_id::text"
		tokJoin = `JOIN inference_attempts ta ON ta.id = u.attempt_id
		       JOIN inference_deployments td ON td.id = ta.deployment_id`
		if f.ProviderID != "" {
			// scope 里 pd 别名在 token 子查询中不存在 —— 用 td 重写。
			scope, args = opsScopeClause(f, []interface{}{f.From, f.To}, "r")
			args = append(args, f.ProviderID)
			scope += fmt.Sprintf(" AND td.provider_id = $%d", len(args))
		}
	}
	tokSQL := fmt.Sprintf(
		`SELECT gkey,
		        SUM(input_tokens) AS input_tokens,
		        SUM(cache_read_tokens) AS cache_read_tokens,
		        SUM(cache_write_tokens) AS cache_write_tokens,
		        SUM(output_tokens) AS output_tokens,
		        SUM(reasoning_tokens) AS reasoning_tokens`+tokFrom+`
		  GROUP BY gkey`, tokKey, tokJoin, scope)
	var tokRows []struct {
		Gkey       string        `db:"gkey"`
		Input      sql.NullInt64 `db:"input_tokens"`
		CacheRead  sql.NullInt64 `db:"cache_read_tokens"`
		CacheWrite sql.NullInt64 `db:"cache_write_tokens"`
		Output     sql.NullInt64 `db:"output_tokens"`
		Reasoning  sql.NullInt64 `db:"reasoning_tokens"`
	}
	if err := s.db.SelectContext(ctx, &tokRows, tokSQL, args...); err != nil {
		return nil, mapError("ops usage: tokens", err)
	}
	for _, r := range tokRows {
		ensure(r.Gkey).Tokens = domain.UsageBuckets{
			InputTokens:      nullInt64Ptr(r.Input),
			CacheReadTokens:  nullInt64Ptr(r.CacheRead),
			CacheWriteTokens: nullInt64Ptr(r.CacheWrite),
			OutputTokens:     nullInt64Ptr(r.Output),
			ReasoningTokens:  nullInt64Ptr(r.Reasoning),
		}
	}

	// 4) 尝试级：延迟（avg/p95，毫秒）+ 采购成本分片 + 未知成本计数。
	attKey := keyExpr
	attJoin := `JOIN inference_deployments ad ON ad.id = a.deployment_id`
	attScope := scope
	if f.GroupBy == management.OpsGroupProvider {
		attKey = "ad.provider_id::text"
		if f.ProviderID != "" {
			// 与 token 子查询同理：用 ad 别名重写 provider 收敛。
			attScope, args = opsScopeClause(f, []interface{}{f.From, f.To}, "r")
			args = append(args, f.ProviderID)
			attScope += fmt.Sprintf(" AND ad.provider_id = $%d", len(args))
		}
	}
	attSQL := fmt.Sprintf(
		`SELECT %s AS gkey,
		        ROUND(AVG(EXTRACT(EPOCH FROM (a.finished_at - a.started_at)) * 1000)
		              FILTER (WHERE a.finished_at IS NOT NULL AND a.started_at IS NOT NULL))::bigint AS avg_latency_ms,
		        ROUND(percentile_cont(0.95) WITHIN GROUP (
		              ORDER BY EXTRACT(EPOCH FROM (a.finished_at - a.started_at)) * 1000)
		              FILTER (WHERE a.finished_at IS NOT NULL AND a.started_at IS NOT NULL))::bigint AS p95_latency_ms,
		        COUNT(*) FILTER (WHERE a.cost_micros IS NULL) AS cost_unknown
		   FROM inference_attempts a
		   JOIN inference_requests r ON r.id = a.request_id
		   %s
		  WHERE r.created_at >= $1 AND r.created_at < $2%s
		  GROUP BY %s`, attKey, attJoin, attScope, attKey)
	var attRows []struct {
		Gkey    string        `db:"gkey"`
		AvgMs   sql.NullInt64 `db:"avg_latency_ms"`
		P95Ms   sql.NullInt64 `db:"p95_latency_ms"`
		Unknown int64         `db:"cost_unknown"`
	}
	if err := s.db.SelectContext(ctx, &attRows, attSQL, args...); err != nil {
		return nil, mapError("ops usage: attempts", err)
	}
	for _, r := range attRows {
		g := ensure(r.Gkey)
		g.AvgLatencyMs = nullInt64Ptr(r.AvgMs)
		g.P95LatencyMs = nullInt64Ptr(r.P95Ms)
		g.CostUnknownAttempts = r.Unknown
	}

	// 4b) 成本分片：币种 × cost_basis（reported/estimated/allocated），绝不
	// 跨币种合并。
	costSQL := fmt.Sprintf(
		`SELECT %s AS gkey, a.cost_currency, a.cost_basis, SUM(a.cost_micros) AS micros
		   FROM inference_attempts a
		   JOIN inference_requests r ON r.id = a.request_id
		   %s
		  WHERE r.created_at >= $1 AND r.created_at < $2%s
		    AND a.cost_micros IS NOT NULL
		  GROUP BY %s, a.cost_currency, a.cost_basis
		  ORDER BY gkey, a.cost_currency, a.cost_basis`, attKey, attJoin, attScope, attKey)
	var costRows []struct {
		Gkey     string `db:"gkey"`
		Currency string `db:"cost_currency"`
		Basis    string `db:"cost_basis"`
		Micros   int64  `db:"micros"`
	}
	if err := s.db.SelectContext(ctx, &costRows, costSQL, args...); err != nil {
		return nil, mapError("ops usage: cost", err)
	}
	for _, r := range costRows {
		g := ensure(r.Gkey)
		g.CostSlices = append(g.CostSlices, management.CostSlice{
			Currency: r.Currency, Basis: r.Basis, Micros: r.Micros,
		})
	}

	// 5) 分组键展示名：provider code / customer user_id。
	if err := s.decorateOpsGroups(ctx, f.GroupBy, order, groups); err != nil {
		return nil, err
	}

	out := make([]management.OpsGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out, nil
}

// decorateOpsGroups fills the display correlation of group keys (provider
// code; customer user_id). Best-effort per group; a dangling reference is a
// data-integrity issue and surfaces as an error rather than a blank.
func (s *Store) decorateOpsGroups(ctx context.Context, by management.OpsGroupBy, keys []string, groups map[string]*management.OpsGroup) error {
	if len(keys) == 0 {
		return nil
	}
	switch by {
	case management.OpsGroupProvider:
		var rows []struct {
			ID   string `db:"id"`
			Code string `db:"code"`
		}
		if err := s.db.SelectContext(ctx, &rows,
			`SELECT id::text, code FROM inference_providers WHERE id = ANY($1::uuid[])`, pq.Array(keys)); err != nil {
			return mapError("ops usage: provider codes", err)
		}
		for _, r := range rows {
			if g, ok := groups[r.ID]; ok {
				g.ProviderCode = r.Code
			}
		}
	case management.OpsGroupCustomer:
		var rows []struct {
			ID     string `db:"id"`
			UserID string `db:"user_id"`
		}
		if err := s.db.SelectContext(ctx, &rows,
			`SELECT id::text, user_id::text FROM inference_billing_accounts WHERE id = ANY($1::uuid[])`, pq.Array(keys)); err != nil {
			return mapError("ops usage: account users", err)
		}
		for _, r := range rows {
			if g, ok := groups[r.ID]; ok {
				g.UserID = r.UserID
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 异常筛选
// ---------------------------------------------------------------------------

// ListStuckReservations returns held reservations older than the threshold
// （异常预占：恢复 worker 正在/应当处理的滞留预占）.
func (s *Store) ListStuckReservations(ctx context.Context, olderThan time.Time, limit int) ([]management.StuckReservation, error) {
	var rows []struct {
		ID         string    `db:"id"`
		RequestID  string    `db:"request_id"`
		Status     string    `db:"status"`
		AccountID  string    `db:"billing_account_id"`
		ModelID    string    `db:"model_id"`
		TargetKind string    `db:"target_kind"`
		Amount     int64     `db:"amount_micros"`
		AgeSec     int64     `db:"age_seconds"`
		CreatedAt  time.Time `db:"created_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT res.id::text, res.request_id::text, r.status, r.billing_account_id::text,
		        r.model_id, res.target_kind, res.amount_micros,
		        EXTRACT(EPOCH FROM (now() - res.created_at))::bigint AS age_seconds,
		        res.created_at
		   FROM inference_reservations res
		   JOIN inference_requests r ON r.id = res.request_id
		  WHERE res.state = 'held' AND res.created_at < $1
		  ORDER BY res.created_at LIMIT $2`, olderThan.UTC(), limit); err != nil {
		return nil, mapError("ops: stuck reservations", err)
	}
	out := make([]management.StuckReservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.StuckReservation{
			ReservationID: r.ID, RequestID: r.RequestID, RequestStatus: r.Status,
			AccountID: r.AccountID, ModelID: r.ModelID, TargetKind: r.TargetKind,
			AmountMicros: r.Amount, AgeSeconds: r.AgeSec, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// ListReauthAccounts returns upstream accounts whose authorization lapsed
// (reauth_required — invalid_grant 传播后停止调度；Task 12 语义).
func (s *Store) ListReauthAccounts(ctx context.Context, limit int) ([]management.ReauthAccount, error) {
	var rows []struct {
		ID           string    `db:"id"`
		ProviderID   string    `db:"provider_id"`
		ProviderCode string    `db:"code"`
		DisplayName  string    `db:"display_name"`
		Status       string    `db:"status"`
		UpdatedAt    time.Time `db:"updated_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT a.id::text, a.provider_id::text, p.code, a.display_name, a.status, a.updated_at
		   FROM inference_upstream_accounts a
		   JOIN inference_providers p ON p.id = a.provider_id
		  WHERE a.status = 'reauth_required'
		  ORDER BY a.updated_at DESC LIMIT $1`, limit); err != nil {
		return nil, mapError("ops: reauth accounts", err)
	}
	out := make([]management.ReauthAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.ReauthAccount{
			AccountID: r.ID, ProviderID: r.ProviderID, ProviderCode: r.ProviderCode,
			DisplayName: r.DisplayName, Status: r.Status, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// ListSettlementBacklog returns open reconciliation jobs (pending/running/
// escalated), oldest first, with the overdue marker computed against now.
func (s *Store) ListSettlementBacklog(ctx context.Context, now time.Time, limit int) ([]management.BacklogJob, error) {
	var rows []struct {
		ID         string         `db:"id"`
		RequestID  sql.NullString `db:"request_id"`
		Reason     string         `db:"reason"`
		Status     string         `db:"status"`
		DeadlineAt time.Time      `db:"deadline_at"`
		CreatedAt  time.Time      `db:"created_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id::text, request_id::text, reason, status, deadline_at, created_at
		   FROM inference_reconciliation_jobs
		  WHERE status IN ('pending','running','escalated')
		  ORDER BY created_at LIMIT $1`, limit); err != nil {
		return nil, mapError("ops: settlement backlog", err)
	}
	out := make([]management.BacklogJob, 0, len(rows))
	for _, r := range rows {
		job := management.BacklogJob{
			ID: r.ID, Reason: r.Reason, Status: r.Status,
			DeadlineAt: r.DeadlineAt, CreatedAt: r.CreatedAt,
			Overdue: r.DeadlineAt.Before(now.UTC()),
		}
		if r.RequestID.Valid {
			id := r.RequestID.String
			job.RequestID = &id
		}
		out = append(out, job)
	}
	return out, nil
}

// ListSharedDeploymentAccounts runs the "一账号一部署"检测视图：观测窗口内
// 同一上游账号服务过 >1 个部署（attempts 落库事实）。
func (s *Store) ListSharedDeploymentAccounts(ctx context.Context, from, to time.Time, limit int) ([]management.SharedDeploymentAccount, error) {
	var rows []struct {
		AccountID    string         `db:"account_id"`
		ProviderID   string         `db:"provider_id"`
		ProviderCode string         `db:"code"`
		DisplayName  string         `db:"display_name"`
		Deployments  pq.StringArray `db:"deployment_ids"`
		Attempts     int64          `db:"attempts"`
		LatestAt     time.Time      `db:"latest_attempt_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT a.upstream_account_id::text AS account_id,
		        ua.provider_id::text, p.code, ua.display_name,
		        array_agg(DISTINCT a.deployment_id::text) AS deployment_ids,
		        COUNT(*) AS attempts, MAX(a.created_at) AS latest_attempt_at
		   FROM inference_attempts a
		   JOIN inference_upstream_accounts ua ON ua.id = a.upstream_account_id
		   JOIN inference_providers p ON p.id = ua.provider_id
		  WHERE a.upstream_account_id IS NOT NULL AND a.deployment_id IS NOT NULL
		    AND a.created_at >= $1 AND a.created_at < $2
		  GROUP BY a.upstream_account_id, ua.provider_id, p.code, ua.display_name
		 HAVING COUNT(DISTINCT a.deployment_id) > 1
		  ORDER BY attempts DESC LIMIT $3`, from.UTC(), to.UTC(), limit); err != nil {
		return nil, mapError("ops: shared deployment accounts", err)
	}
	out := make([]management.SharedDeploymentAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.SharedDeploymentAccount{
			AccountID: r.AccountID, ProviderID: r.ProviderID, ProviderCode: r.ProviderCode,
			DisplayName: r.DisplayName, Deployments: []string(r.Deployments),
			Attempts: r.Attempts, LatestAt: r.LatestAt,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 补偿追踪（有原因、对象、金额、操作者与幂等键）
// ---------------------------------------------------------------------------

// ListAdjustments returns compensation records newest first, optionally
// scoped to one billing account.
func (s *Store) ListAdjustments(ctx context.Context, accountID string, limit int) ([]management.AdjustmentView, error) {
	query := `SELECT id::text, billing_account_id::text, request_id::text, reason,
	        amount_micros, direction, unit, currency, operator_subject,
	        service_subject, idempotency_key, created_at
	   FROM inference_adjustments`
	var args []interface{}
	if accountID != "" {
		args = append(args, accountID)
		query += ` WHERE billing_account_id = $1`
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	var rows []struct {
		ID        string         `db:"id"`
		AccountID string         `db:"billing_account_id"`
		RequestID sql.NullString `db:"request_id"`
		Reason    string         `db:"reason"`
		Amount    int64          `db:"amount_micros"`
		Direction string         `db:"direction"`
		Unit      string         `db:"unit"`
		Currency  sql.NullString `db:"currency"`
		Operator  string         `db:"operator_subject"`
		Service   string         `db:"service_subject"`
		Key       string         `db:"idempotency_key"`
		CreatedAt time.Time      `db:"created_at"`
	}
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("ops: list adjustments", err)
	}
	out := make([]management.AdjustmentView, 0, len(rows))
	for _, r := range rows {
		v := management.AdjustmentView{
			ID: r.ID, BillingAccountID: r.AccountID, Reason: r.Reason,
			AmountMicros: r.Amount, Direction: r.Direction, Unit: r.Unit,
			Currency: stringFromNull(r.Currency), OperatorSubject: r.Operator,
			ServiceSubject: r.Service, IdempotencyKey: r.Key, CreatedAt: r.CreatedAt,
		}
		if r.RequestID.Valid {
			id := r.RequestID.String
			v.RequestID = &id
		}
		out = append(out, v)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 变更预览的受影响面（只读）
// ---------------------------------------------------------------------------

// LatestPriceVersionInfo adapts LatestPriceVersion to the management DTO
// (management 不依赖 postgres —— Task 11 customer_read_repo 先例).
func (s *Store) LatestPriceVersionInfo(ctx context.Context, modelID, kind string, at time.Time) (*management.PriceVersionInfo, error) {
	p, err := s.LatestPriceVersion(ctx, modelID, kind, at)
	if err != nil {
		return nil, err
	}
	return priceVersionInfo(p), nil
}

// LatestPolicyVersionByName returns the latest PUBLISHED revision of a
// policy name (CodeNotFound when none is published — 首次定义).
func (s *Store) LatestPolicyVersionByName(ctx context.Context, name string) (*management.PolicyVersionInfo, error) {
	var id string
	if err := s.db.GetContext(ctx, &id,
		`SELECT id::text FROM inference_policy_versions
		  WHERE name = $1 AND status = 'published'
		  ORDER BY revision DESC LIMIT 1`, name); err != nil {
		return nil, mapError("latest policy version by name", err)
	}
	p, err := s.GetPolicyVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	return &management.PolicyVersionInfo{
		ID: p.ID, Name: p.Name, Revision: p.Revision, ModelIDs: p.ModelIDs,
		FiveHourLimit: microToInt64(p.FiveHourLimit), WeeklyLimit: microToInt64(p.WeeklyLimit),
		MonthlyLimit: microToInt64(p.MonthlyLimit),
		RPMLimit:     p.RPMLimit, TPMLimit: p.TPMLimit, ConcurrencyLimit: p.ConcurrencyLimit,
		OveragePolicy: p.OveragePolicy, Status: p.Status,
	}, nil
}

func microToInt64(m *domain.Microcredit) *int64 {
	if m == nil {
		return nil
	}
	v := int64(*m)
	return &v
}

// CountActiveEntitlementsCoveringModel counts active entitlements whose
// explicit model set contains the model.
func (s *Store) CountActiveEntitlementsCoveringModel(ctx context.Context, modelID string) (int64, error) {
	var n int64
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM inference_entitlements
		  WHERE status = 'active' AND model_ids @> ARRAY[$1]::text[]`, modelID); err != nil {
		return 0, mapError("preview: entitlements covering model", err)
	}
	return n, nil
}

// AffectedPlansForModel lists plans reachable from active subscription-
// sourced entitlements covering the model (跨域只读：subscriptions/plans 是
// 支付域事实，Task 11 customer_read_repo 先例).
func (s *Store) AffectedPlansForModel(ctx context.Context, modelID string) ([]management.AffectedPlan, error) {
	var rows []struct {
		PlanID   string         `db:"plan_id"`
		PlanName sql.NullString `db:"plan_name"`
		Accounts int64          `db:"accounts"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT sub.plan_id, p.name AS plan_name, COUNT(*) AS accounts
		   FROM inference_entitlements e
		   JOIN subscriptions sub ON sub.id::text = e.source_id
		   LEFT JOIN plans p ON p.id = sub.plan_id
		  WHERE e.source_type = 'subscription' AND e.status = 'active'
		    AND e.model_ids @> ARRAY[$1]::text[]
		  GROUP BY sub.plan_id, p.name
		  ORDER BY accounts DESC, sub.plan_id`, modelID); err != nil {
		return nil, mapError("preview: affected plans", err)
	}
	out := make([]management.AffectedPlan, 0, len(rows))
	for _, r := range rows {
		out = append(out, management.AffectedPlan{
			PlanID: r.PlanID, PlanName: stringFromNull(r.PlanName), Accounts: r.Accounts,
		})
	}
	return out, nil
}

// CountActiveEntitlementsUsingPolicy counts active entitlements pinned to
// one policy version id.
func (s *Store) CountActiveEntitlementsUsingPolicy(ctx context.Context, policyVersionID string) (int64, error) {
	var n int64
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM inference_entitlements
		  WHERE status = 'active' AND policy_version_id = $1`, policyVersionID); err != nil {
		return 0, mapError("preview: entitlements using policy", err)
	}
	return n, nil
}

// CountInFlightRequestsForModel counts non-terminal requests of the model —
// 它们钉住了入场时的价格/策略版本（031 持久化准入绑定）。
func (s *Store) CountInFlightRequestsForModel(ctx context.Context, modelID string) (int64, error) {
	var n int64
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM inference_requests
		  WHERE model_id = $1 AND status IN
		    ('authenticated','reserved','dispatching','streaming','non_streaming','settling')`, modelID); err != nil {
		return 0, mapError("preview: inflight requests", err)
	}
	return n, nil
}
