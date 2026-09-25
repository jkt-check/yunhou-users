package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// operations_repo_test.go — Task 15 运营读模型与批量导入的真实库验证。
// 核心验收：统计聚合能与账本抽样核对（每模型/每客户独立 SQL 重算对比）；
// 成本按 币种 × cost_basis 分片且 unknown 单列；批量导入部分错误预览不
// 半发布、重复提交不重复创建；补偿可定位到人员。

// --- seeding helpers (直接 SQL：读模型测试需要精确控制行形态) ---

func seedProvider(t *testing.T, s *Store, code string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`INSERT INTO inference_providers (code, display_name, access_type, status)
		 VALUES ($1, $1, 'official_api', 'active') RETURNING id::text`, code).Scan(&id); err != nil {
		t.Fatalf("insert provider %s: %v", code, err)
	}
	return id
}

func seedDeployment(t *testing.T, s *Store, providerID, upstreamModel string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`INSERT INTO inference_deployments (provider_id, upstream_model, base_url, protocol, status)
		 VALUES ($1, $2, 'https://api.example.com', 'openai_chat', 'active')
		 RETURNING id::text`, providerID, upstreamModel).Scan(&id); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	return id
}

func seedUpstreamAccount(t *testing.T, s *Store, providerID, status string) string {
	t.Helper()
	var credID, acctID string
	if err := s.db.QueryRow(
		`INSERT INTO inference_credentials (provider_id, auth_type, ciphertext, key_version)
		 VALUES ($1, 'api_key', $2, 1) RETURNING id::text`,
		providerID, []byte("ciphertext-"+uuid.NewString())).Scan(&credID); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	if err := s.db.QueryRow(
		`INSERT INTO inference_upstream_accounts (provider_id, credential_id, display_name, status)
		 VALUES ($1, $2, 'acct', $3) RETURNING id::text`, providerID, credID, status).Scan(&acctID); err != nil {
		t.Fatalf("insert upstream account: %v", err)
	}
	return acctID
}

// seedOpsRequest inserts one request row with the given status/usage_status
// (created_at = now，落在默认统计窗口内).
func seedOpsRequest(t *testing.T, s *Store, f fixture, modelID, status, usageStatus string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO inference_requests
		 (id, billing_account_id, api_key_id, entitlement_id, model_id, protocol,
		  status, usage_status, policy_version_id)
		 VALUES ($1,$2,$3,$4,$5,'openai_chat',$6,$7,$8)`,
		id, f.accountID, nil, f.entID, modelID, status, usageStatus, f.policyID); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	return id
}

// seedOpsAttempt inserts one attempt with cost + latency facts.
func seedOpsAttempt(t *testing.T, s *Store, requestID string, attemptNo int, deploymentID, accountID string,
	costMicros *int64, currency, basis string, latency time.Duration) string {
	t.Helper()
	id := uuid.NewString()
	started := time.Now().UTC()
	finished := started.Add(latency)
	var costCur interface{}
	var costBasis interface{}
	if costMicros != nil {
		costCur = currency
		costBasis = basis
	}
	if _, err := s.db.Exec(
		`INSERT INTO inference_attempts
		 (id, request_id, attempt_no, deployment_id, upstream_account_id, status,
		  cost_micros, cost_currency, cost_basis, started_at, finished_at)
		 VALUES ($1,$2,$3,$4,$5,'completed',$6,$7,$8,$9,$10)`,
		id, requestID, attemptNo, strToNil(deploymentID), strToNil(accountID),
		costMicros, costCur, costBasis, started, finished); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	return id
}

// seedOpsUsage inserts one usage record; unknown 来源的桶必须为 NULL
// （CHECK: source='unknown' → input_tokens IS NULL），nil 桶保持未知。
func seedOpsUsage(t *testing.T, s *Store, requestID, attemptID, source string, in, out *int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO inference_usage_records (request_id, attempt_id, source, input_tokens, output_tokens)
		 VALUES ($1,$2,$3,$4,$5)`, requestID, attemptID, source, in, out); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// seedOpsLedger inserts one microcredit ledger entry (charge/reversal), or
// one adjustment + its ledger entry (entry_type='adjustment'). requestID ""
// = 账户级调整（无请求）.
func seedOpsLedger(t *testing.T, s *Store, accountID, requestID, entryType string, amount int64, adjDirection string) {
	t.Helper()
	var adjID interface{}
	if entryType == "adjustment" {
		var id string
		if err := s.db.QueryRow(
			`INSERT INTO inference_adjustments
			 (billing_account_id, request_id, reason, amount_micros, direction, unit,
			  operator_subject, service_subject, idempotency_key)
			 VALUES ($1,$2,'ops test',$3,$4,'microcredit','user:op@app:svc','httpapi/test',$5)
			 RETURNING id::text`,
			accountID, strToNil(requestID), amount, adjDirection, "test-adj-"+uuid.NewString()).Scan(&id); err != nil {
			t.Fatalf("insert adjustment: %v", err)
		}
		adjID = id
	}
	if _, err := s.db.Exec(
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, adjustment_id)
		 VALUES ($1,$2,$3,$4,$5)`, accountID, strToNil(requestID), entryType, amount, adjID); err != nil {
		t.Fatalf("insert ledger %s: %v", entryType, err)
	}
}

func strToNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ledgerSpotCheck recomputes the per-group ledger-derived amounts with an
// INDEPENDENT SQL (抽样对账断言：聚合必须与账本逐字一致).
func ledgerSpotCheck(t *testing.T, s *Store, groupCol string) map[string][3]int64 {
	t.Helper()
	rows, err := s.db.Query(
		fmt.Sprintf(
			`SELECT r.%s::text AS gkey,
			        COALESCE(SUM(l.amount_micros) FILTER (WHERE l.entry_type = 'charge'), 0),
			        COALESCE(SUM(l.amount_micros) FILTER (WHERE l.entry_type = 'reversal'), 0),
			        COALESCE(SUM(CASE WHEN a.direction = 'debit' THEN l.amount_micros
			                          WHEN a.direction = 'credit' THEN -l.amount_micros END)
			                 FILTER (WHERE l.entry_type = 'adjustment'), 0)
			   FROM inference_ledger_entries l
			   JOIN inference_requests r ON r.id = l.request_id
			   LEFT JOIN inference_adjustments a ON a.id = l.adjustment_id
			  WHERE l.unit = 'microcredit'
			  GROUP BY r.%s`, groupCol, groupCol))
	if err != nil {
		t.Fatalf("spot check: %v", err)
	}
	defer rows.Close()
	out := map[string][3]int64{}
	for rows.Next() {
		var k string
		var v [3]int64
		if err := rows.Scan(&k, &v[0], &v[1], &v[2]); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// TestOpsSummary_ModelGroups_LedgerSpotCheck: 每模型聚合的账本派生金额与
// 独立账本 SQL 抽样核对逐字一致；计数/计量完整性/token/延迟如实。
func TestOpsSummary_ModelGroups_LedgerSpotCheck(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f1 := seedFixture(t, s, true)
	f2 := seedFixtureModel(t, s, true, "glm-4.7")

	prov := seedProvider(t, s, "glm")
	dep := seedDeployment(t, s, prov, "glm-4.6-upstream")

	// f1: 2 settled（glm-4.6）+ 1 failed；f2: 1 settled（glm-4.6）+ 1 released（glm-4.7）。
	cost := int64(500)
	mk := func(f fixture, modelID, status, usageStatus string, withCost bool) string {
		rid := seedOpsRequest(t, s, f, modelID, status, usageStatus)
		var c *int64
		cur, ba := "", ""
		if withCost {
			c = &cost
			cur, ba = "USD", "reported"
		}
		aid := seedOpsAttempt(t, s, rid, 1, dep, "", c, cur, ba, 1200*time.Millisecond)
		switch usageStatus {
		case "reported", "estimated":
			in, out := int64(100), int64(50)
			seedOpsUsage(t, s, rid, aid, usageStatus, &in, &out)
		case "unknown":
			seedOpsUsage(t, s, rid, aid, "unknown", nil, nil) // 未知不落桶值
		case "pending":
			// 尚无计量记录
		}
		return rid
	}
	r1 := mk(f1, "glm-4.6", "settled", "reported", true)
	r2 := mk(f1, "glm-4.6", "settled", "estimated", true)
	mk(f1, "glm-4.6", "failed", "unknown", false)
	r4 := mk(f2, "glm-4.6", "settled", "reported", true)
	mk(f2, "glm-4.7", "released", "pending", false)

	// 账本：charge×3 + reversal×1 + adjustment(debit)×1 + adjustment(credit)×1。
	seedOpsLedger(t, s, f1.accountID, r1, "charge", 1000, "")
	seedOpsLedger(t, s, f1.accountID, r2, "charge", 700, "")
	seedOpsLedger(t, s, f1.accountID, r2, "reversal", 200, "")
	seedOpsLedger(t, s, f1.accountID, r2, "adjustment", 50, "debit")
	seedOpsLedger(t, s, f2.accountID, r4, "charge", 900, "")
	seedOpsLedger(t, s, f2.accountID, r4, "adjustment", 300, "credit")

	svc := management.NewOperationsService(s, nil)
	sum, err := svc.Summary(ctx, management.OpsUsageFilter{GroupBy: management.OpsGroupModel})
	if err != nil {
		t.Fatal(err)
	}
	if sum.GroupBy != management.OpsGroupModel || !sum.AsOf.Equal(sum.CompleteThrough) {
		t.Fatalf("summary meta = %+v", sum)
	}
	byModel := map[string]management.OpsGroup{}
	for _, g := range sum.Groups {
		byModel[g.ModelID] = g
	}
	g := byModel["glm-4.6"]
	if g.RequestsTotal != 4 || g.SettledRequests != 3 || g.FailedRequests != 1 {
		t.Fatalf("model group counts = %+v", g)
	}
	if g.Reported != 2 || g.Estimated != 1 || g.Unknown != 1 {
		t.Fatalf("usage integrity = %+v", g)
	}
	// token：4 个 settled/failed 请求各有 100+50（unknown 记录的桶为 NULL——
	// unknown 行不落桶值）。
	if g.Tokens.InputTokens == nil || *g.Tokens.InputTokens != 300 {
		t.Fatalf("tokens = %+v", g.Tokens)
	}
	if g.AvgLatencyMs == nil || *g.AvgLatencyMs != 1200 {
		t.Fatalf("latency = %+v", g.AvgLatencyMs)
	}
	// 成本分片：3 条 USD/reported × 500；unknown 成本尝试 = 2（r3/r5 不在本模型…
	// r3 在本模型无 cost → 1 个 unknown）。
	if g.CostUnknownAttempts != 1 {
		t.Fatalf("unknown cost attempts = %d, want 1", g.CostUnknownAttempts)
	}
	if len(g.CostSlices) != 1 || g.CostSlices[0].Currency != "USD" ||
		g.CostSlices[0].Basis != "reported" || g.CostSlices[0].Micros != 1500 {
		t.Fatalf("cost slices = %+v", g.CostSlices)
	}
	if g.SuccessRate == nil || *g.SuccessRate != 0.75 {
		t.Fatalf("success rate = %v", g.SuccessRate)
	}

	// 账本抽样核对（独立 SQL 重算）。
	spot := ledgerSpotCheck(t, s, "model_id")
	for id, grp := range byModel {
		want := spot[id]
		if grp.ChargeMicros != want[0] || grp.ReversedMicros != want[1] || grp.AdjustedMicros != want[2] {
			t.Fatalf("model %s ledger mismatch: group=(%d,%d,%d) ledger=(%d,%d,%d)",
				id, grp.ChargeMicros, grp.ReversedMicros, grp.AdjustedMicros, want[0], want[1], want[2])
		}
	}
	if got := byModel["glm-4.6"].NetMicros(); got != 1000+700-200+50+900-300 {
		t.Fatalf("net = %d", got)
	}
}

// TestOpsSummary_CustomerGroups: 每客户聚合 + 账本抽样核对 + user_id 关联。
func TestOpsSummary_CustomerGroups(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f1 := seedFixture(t, s, true)
	f2 := seedFixtureModel(t, s, true, "glm-4.7")

	prov := seedProvider(t, s, "glm")
	dep := seedDeployment(t, s, prov, "glm-4.6-upstream")

	r1 := seedOpsRequest(t, s, f1, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r1, 1, dep, "", nil, "", "", time.Second)
	seedOpsLedger(t, s, f1.accountID, r1, "charge", 500, "")
	r2 := seedOpsRequest(t, s, f2, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r2, 1, dep, "", nil, "", "", time.Second)
	seedOpsLedger(t, s, f2.accountID, r2, "charge", 800, "")
	seedOpsLedger(t, s, f2.accountID, r2, "adjustment", 100, "debit")

	svc := management.NewOperationsService(s, nil)
	sum, err := svc.Summary(ctx, management.OpsUsageFilter{GroupBy: management.OpsGroupCustomer})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Groups) != 2 {
		t.Fatalf("groups = %+v", sum.Groups)
	}
	byAcct := map[string]management.OpsGroup{}
	for _, g := range sum.Groups {
		byAcct[g.AccountID] = g
		if g.UserID == "" {
			t.Fatalf("customer group missing user_id: %+v", g)
		}
	}
	spot := ledgerSpotCheck(t, s, "billing_account_id")
	for id, grp := range byAcct {
		want := spot[id]
		if grp.ChargeMicros != want[0] || grp.ReversedMicros != want[1] || grp.AdjustedMicros != want[2] {
			t.Fatalf("account %s ledger mismatch", id)
		}
	}
	if byAcct[f2.accountID].NetMicros() != 800+100 {
		t.Fatalf("f2 net = %d", byAcct[f2.accountID].NetMicros())
	}
}

// TestOpsSummary_ProviderGroups: 供应商维度——请求级按最终尝试归属（重试
// 不双计），尝试级成本/延迟按每次尝试自己的部署归属。
func TestOpsSummary_ProviderGroups(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	p1 := seedProvider(t, s, "p-one")
	p2 := seedProvider(t, s, "p-two")
	d1 := seedDeployment(t, s, p1, "m-up")
	d2 := seedDeployment(t, s, p2, "m-up")

	// r1：p1 失败后重试到 p2 成功（最终尝试归 p2）；r2：p1 一次成功。
	r1 := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	c1 := int64(100)
	seedOpsAttempt(t, s, r1, 1, d1, "", &c1, "USD", "reported", 300*time.Millisecond)
	c2 := int64(200)
	seedOpsAttempt(t, s, r1, 2, d2, "", &c2, "CNY", "estimated", 900*time.Millisecond)
	r2 := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r2, 1, d1, "", nil, "", "", 500*time.Millisecond) // 成本未知

	svc := management.NewOperationsService(s, nil)
	sum, err := svc.Summary(ctx, management.OpsUsageFilter{GroupBy: management.OpsGroupProvider})
	if err != nil {
		t.Fatal(err)
	}
	byProv := map[string]management.OpsGroup{}
	for _, g := range sum.Groups {
		byProv[g.ProviderCode] = g
	}
	gp1, gp2 := byProv["p-one"], byProv["p-two"]
	// 请求级：r1 最终尝试在 p2 → p1 只计 r2。
	if gp1.RequestsTotal != 1 || gp2.RequestsTotal != 1 {
		t.Fatalf("provider request attribution: p1=%d p2=%d", gp1.RequestsTotal, gp2.RequestsTotal)
	}
	// 尝试级成本：p1 = 100 USD/reported + 1 unknown；p2 = 200 CNY/estimated。
	if len(gp1.CostSlices) != 1 || gp1.CostSlices[0].Micros != 100 ||
		gp1.CostSlices[0].Currency != "USD" || gp1.CostSlices[0].Basis != "reported" {
		t.Fatalf("p1 cost = %+v", gp1.CostSlices)
	}
	if gp1.CostUnknownAttempts != 1 {
		t.Fatalf("p1 unknown cost = %d, want 1", gp1.CostUnknownAttempts)
	}
	if len(gp2.CostSlices) != 1 || gp2.CostSlices[0].Micros != 200 ||
		gp2.CostSlices[0].Currency != "CNY" || gp2.CostSlices[0].Basis != "estimated" {
		t.Fatalf("p2 cost = %+v", gp2.CostSlices)
	}
	// 供应商维度不归属客户账本金额（跨供应商重试拆账是编造）。
	if gp1.ChargeMicros != 0 || gp2.ChargeMicros != 0 {
		t.Fatalf("provider groups must not attribute ledger amounts: %+v %+v", gp1, gp2)
	}
	if gp1.AvgLatencyMs == nil || gp2.AvgLatencyMs == nil {
		t.Fatalf("provider latency missing: %+v %+v", gp1.AvgLatencyMs, gp2.AvgLatencyMs)
	}
}

// TestOpsExceptions: 异常预占 / 授权失效 / 结算积压三个筛选。
func TestOpsExceptions(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	// 异常预占：held 且 created_at 距今 2 小时。
	rid := seedOpsRequest(t, s, f, "glm-4.6", "streaming", "pending")
	if _, err := s.db.Exec(
		`INSERT INTO inference_reservations (request_id, target_kind, api_key_id, amount_micros, state, created_at)
		 VALUES ($1, 'key_budget', $2, 1000, 'held', now() - interval '2 hours')`, rid, f.keyID); err != nil {
		t.Fatalf("insert stuck reservation: %v", err)
	}
	// 对照：新 held 预占（不异常）+ 已结算预占。
	ridFresh := seedOpsRequest(t, s, f, "glm-4.6", "streaming", "pending")
	if _, err := s.db.Exec(
		`INSERT INTO inference_reservations (request_id, target_kind, api_key_id, amount_micros, state)
		 VALUES ($1, 'key_budget', $2, 1000, 'held')`, ridFresh, f.keyID); err != nil {
		t.Fatal(err)
	}

	prov := seedProvider(t, s, "glm")
	seedUpstreamAccount(t, s, prov, "reauth_required")
	seedUpstreamAccount(t, s, prov, "active")

	// 结算积压：pending（未超期）+ escalated（已超期）+ resolved（不出现）。
	ridA := seedOpsRequest(t, s, f, "glm-4.6", "reconciliation_required", "unknown")
	ridB := seedOpsRequest(t, s, f, "glm-4.6", "reconciliation_required", "unknown")
	ridC := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	for _, job := range []struct{ rid, status string; overdue bool }{
		{ridA, "pending", false}, {ridB, "escalated", true}, {ridC, "resolved", false},
	} {
		deadline := time.Now().Add(time.Hour)
		if job.overdue {
			deadline = time.Now().Add(-time.Hour)
		}
		if _, err := s.db.Exec(
			`INSERT INTO inference_reconciliation_jobs (request_id, reason, status, deadline_at, resolved_at)
			 VALUES ($1, 'unknown_usage', $2, $3, CASE WHEN $2 = 'resolved' THEN now() END)`,
			job.rid, job.status, deadline); err != nil {
			t.Fatalf("insert job: %v", err)
		}
	}

	svc := management.NewOperationsService(s, nil)

	stuck, err := svc.Exceptions(ctx, management.ExceptionStuckReservations, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck.StuckReservations) != 1 || stuck.StuckReservations[0].RequestID != rid {
		t.Fatalf("stuck = %+v", stuck.StuckReservations)
	}
	if stuck.StuckReservations[0].AgeSeconds < 7000 {
		t.Fatalf("age = %d, want >= 2h", stuck.StuckReservations[0].AgeSeconds)
	}

	reauth, err := svc.Exceptions(ctx, management.ExceptionReauthAccounts, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(reauth.ReauthAccounts) != 1 || reauth.ReauthAccounts[0].ProviderCode != "glm" {
		t.Fatalf("reauth = %+v", reauth.ReauthAccounts)
	}

	backlog, err := svc.Exceptions(ctx, management.ExceptionSettlementBacklog, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog.SettlementBacklog) != 2 {
		t.Fatalf("backlog = %+v", backlog.SettlementBacklog)
	}
	overdues := 0
	for _, j := range backlog.SettlementBacklog {
		if j.Overdue {
			overdues++
		}
	}
	if overdues != 1 {
		t.Fatalf("overdue count = %d, want 1", overdues)
	}

	if _, err := svc.Exceptions(ctx, "bogus", 100); err == nil {
		t.Fatal("unknown exception kind accepted")
	}
}

// TestSharedDeploymentAccounts: "一账号一部署"检测视图（Task 13 移交项）。
func TestSharedDeploymentAccounts(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	prov := seedProvider(t, s, "glm")
	d1 := seedDeployment(t, s, prov, "m-a")
	d2 := seedDeployment(t, s, prov, "m-b")
	shared := seedUpstreamAccount(t, s, prov, "active")
	solo := seedUpstreamAccount(t, s, prov, "active")

	// shared 账号服务过两个部署；solo 只服务过一个。
	r1 := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r1, 1, d1, shared, nil, "", "", time.Second)
	r2 := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r2, 1, d2, shared, nil, "", "", time.Second)
	r3 := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, r3, 1, d1, solo, nil, "", "", time.Second)

	svc := management.NewOperationsService(s, nil)
	rows, err := svc.SharedDeploymentAccounts(ctx, time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AccountID != shared {
		t.Fatalf("shared accounts = %+v", rows)
	}
	if len(rows[0].Deployments) != 2 || rows[0].Attempts != 2 {
		t.Fatalf("shared row = %+v", rows[0])
	}
}

// TestListAdjustments_Attribution: 补偿列表可定位到人员（operator/service
// 双重归因 + 幂等键 + 原因）。
func TestListAdjustments_Attribution(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	seedOpsLedger(t, s, f.accountID, "", "adjustment", 500, "credit")

	svc := management.NewOperationsService(s, nil)
	views, err := svc.Adjustments(ctx, f.accountID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("adjustments = %+v", views)
	}
	v := views[0]
	if v.OperatorSubject != "user:op@app:svc" || v.ServiceSubject != "httpapi/test" ||
		v.IdempotencyKey == "" || v.Reason != "ops test" || v.AmountMicros != 500 || v.Direction != "credit" {
		t.Fatalf("adjustment attribution = %+v", v)
	}
	// 账户收敛过滤。
	other, err := svc.Adjustments(ctx, uuid.NewString(), 100)
	if err != nil || len(other) != 0 {
		t.Fatalf("scoped adjustments = %+v err=%v", other, err)
	}
}

// TestBulkImport_CommitReplayNoDuplicates: 批量导入 commit 原子落库 +
// task_id 幂等重放（同文档重复提交不重复创建；M-4：不同文档同 task_id
// → 409，不得静默返回旧任务的结果）。
func TestBulkImport_CommitReplayNoDuplicates(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	svc := management.NewBulkImportService(s, nil, func(context.Context, string) error { return nil })

	doc := &management.BulkCatalog{
		Providers: []management.BulkProvider{{Code: "glm", DisplayName: "GLM", AccessType: "official_api"}},
		Models: []management.BulkModel{{
			ID: "glm-4.7", DisplayName: "GLM 4.7", ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols: []string{"openai_chat"},
			Deployments: []management.BulkDeployment{{
				ProviderCode: "glm", UpstreamModel: "glm-4.7-up",
				BaseURL: "https://api.glm.example.com", Protocol: "openai_chat",
			}},
		}},
	}

	// dry-run：一行不写。
	pre, err := svc.Import(ctx, "user:op1@app:ops", "imp-1", doc, true)
	if err != nil || pre.HasErrors() {
		t.Fatalf("dry-run: %+v err=%v", pre, err)
	}
	assertRowCounts(t, s, 0, 0, 0, 0)

	// commit：4 项全 inserted，落库即草稿。
	res, err := svc.Import(ctx, "user:op1@app:ops", "imp-1", doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || res.Replayed || res.Inserted != 4 || res.Skipped != 0 {
		t.Fatalf("commit = %+v", res)
	}
	assertRowCounts(t, s, 1, 1, 1, 1)
	var lifecycle string
	if err := s.db.Get(&lifecycle, `SELECT lifecycle FROM inference_models WHERE id = 'glm-4.7'`); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "draft" {
		t.Fatalf("imported model lifecycle = %s, want draft (默认不可售)", lifecycle)
	}

	// 重放：同 task_id 同文档 → 返回已记录结果，一行不增。
	replay, err := svc.Import(ctx, "user:op2@app:ops", "imp-1", doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || !replay.Committed || replay.Inserted != 4 {
		t.Fatalf("replay = %+v", replay)
	}
	assertRowCounts(t, s, 1, 1, 1, 1)

	// M-4：同 task_id 不同文档 → 409（task_id 复用，不是良性重试），
	// 一行不增。
	doc2 := &management.BulkCatalog{
		Providers: []management.BulkProvider{{Code: "another", DisplayName: "X", AccessType: "official_api"}},
	}
	if _, err := svc.Import(ctx, "user:op2@app:ops", "imp-1", doc2, false); err == nil ||
		domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("different-doc replay err = %v, want CodeConflict", err)
	}
	assertRowCounts(t, s, 1, 1, 1, 1)

	// 新 task_id + 相同文档 → 自然键跳过（不重复创建）。
	again, err := svc.Import(ctx, "user:op1@app:ops", "imp-2", doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Committed || again.Inserted != 0 || again.Skipped != 4 {
		t.Fatalf("re-import = %+v", again)
	}
	assertRowCounts(t, s, 1, 1, 1, 1)
}

// TestBulkImport_CommitWithErrorsWritesNothing: 部分错误的 commit 一行不写
// （绝不半发布——服务层全有效闸门 + 任务表无记录）。
func TestBulkImport_CommitWithErrorsWritesNothing(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	svc := management.NewBulkImportService(s, nil, func(context.Context, string) error { return nil })

	doc := &management.BulkCatalog{
		Providers: []management.BulkProvider{{Code: "glm", DisplayName: "GLM", AccessType: "official_api"}},
		Models: []management.BulkModel{{
			ID: "ok-model", DisplayName: "ok", ContextTokens: 1000, MaxOutputTokens: 100,
			Deployments: []management.BulkDeployment{{
				ProviderCode: "ghost", UpstreamModel: "x", Protocol: "openai_chat", // 未知 provider
			}},
		}},
	}
	res, err := svc.Import(ctx, "user:op1@app:ops", "imp-err", doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasErrors() || res.Committed {
		t.Fatalf("commit-with-errors = %+v", res)
	}
	assertRowCounts(t, s, 0, 0, 0, 0)
	var tasks int
	if err := s.db.Get(&tasks, `SELECT COUNT(*) FROM inference_bulk_imports WHERE task_id = 'imp-err'`); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatal("failed import must not register a task row")
	}
}

func assertRowCounts(t *testing.T, s *Store, providers, models, deployments, routes int) {
	t.Helper()
	for tbl, want := range map[string]int{
		"inference_providers": providers, "inference_models": models,
		"inference_deployments": deployments, "inference_model_routes": routes,
	} {
		var n int
		if err := s.db.Get(&n, `SELECT COUNT(*) FROM `+tbl); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s rows = %d, want %d", tbl, n, want)
		}
	}
}

// TestPricingPreview_RealStore: 价格/策略变更预览的真实库验证——生效时刻
// 被替换版本、受影响套餐/账户、在途钉住、旧订阅保留版本。
func TestPricingPreview_RealStore(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	// 当前有效价格（sale_credit r1）。
	pv := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 100, OutputPerMtok: 300, Revision: 1,
		EffectiveFrom: time.Now().Add(-time.Hour).UTC(),
	}
	if err := s.InsertPriceVersion(ctx, pv); err != nil {
		t.Fatal(err)
	}
	// 订阅来源权益 → 受影响套餐。
	if _, err := s.db.Exec(
		`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code, is_active, accepting_new_subscriptions)
		 VALUES ('coding-pro', 'Coding Pro', 100, 30, '{}', 'CNY', 'coding-plan', true, true)
		 ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	subID := uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO subscriptions (id, user_id, plan_id, status, product_code)
		 VALUES ($1, $2, 'coding-pro', 'active', 'coding-plan')`, subID, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE inference_entitlements SET source_id = $1 WHERE id = $2`, subID, f.entID); err != nil {
		t.Fatal(err)
	}
	// 在途请求（钉住旧价）。
	seedOpsRequest(t, s, f, "glm-4.6", "streaming", "pending")
	// 策略预览的差分目标模型。
	if err := s.InsertModel(ctx, &domain.Model{
		ID: "glm-4.7", DisplayName: "GLM 4.7", ContextTokens: 100000, MaxOutputTokens: 4096,
	}); err != nil {
		t.Fatal(err)
	}

	svc := management.NewPricingPreviewService(s, nil)
	from := time.Now().UTC()
	res, err := svc.PreviewPriceChange(ctx, management.PriceChangePreview{
		ModelID: f.modelID, Kind: "sale_credit",
		InputPerMtok: 120, OutputPerMtok: 360, EffectiveFrom: from,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CurrentEffective == nil || res.CurrentEffective.Revision != 1 ||
		res.CurrentEffective.InputPerMtok != "100" {
		t.Fatalf("current effective = %+v", res.CurrentEffective)
	}
	if res.Proposed.InputPerMtok != "120" || !res.EffectiveFrom.Equal(from) {
		t.Fatalf("proposed = %+v", res.Proposed)
	}
	if res.AffectedAccounts != 1 || len(res.AffectedPlans) != 1 ||
		res.AffectedPlans[0].PlanID != "coding-pro" || res.AffectedPlans[0].Accounts != 1 {
		t.Fatalf("affected = %+v", res)
	}
	if res.InFlightPinnedRequests != 1 || !res.ExistingSubscriptionsKeepVersion {
		t.Fatalf("pinning = %+v", res)
	}

	// 策略预览：当前发布版 → 模型差分 + 钉住计数。
	if _, err := s.db.Exec(
		`UPDATE inference_policy_versions SET status = 'published', published_at = now() WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	pol, err := svc.PreviewPolicyChange(ctx, management.PolicyChangePreview{
		Name: "coding-plan-" + f.modelID, ModelIDs: []string{f.modelID, "glm-4.7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pol.CurrentPublished == nil || pol.CurrentPublished.Revision != 1 {
		t.Fatalf("current policy = %+v", pol.CurrentPublished)
	}
	if len(pol.AddedModels) != 1 || pol.AddedModels[0] != "glm-4.7" || len(pol.RemovedModels) != 0 {
		t.Fatalf("model diff = %+v", pol)
	}
	if pol.AffectedAccountsKeptOnOld != 1 || !pol.ExistingSubscriptionsKeepVersion {
		t.Fatalf("policy impact = %+v", pol)
	}
	// 未知模型的策略集合必须拒绝（授权不凭空指向模型）。
	if _, err := svc.PreviewPolicyChange(ctx, management.PolicyChangePreview{
		Name: "coding-plan-" + f.modelID, ModelIDs: []string{"no-such-model"},
	}); err == nil {
		t.Fatal("policy preview accepted unknown model")
	}
}

// TestDeploymentDisableKeepsHistory: 停用上游不删除历史路由/尝试记录
// （brief 验收）；有历史尝试/路由的部署不能被 DELETE（FK + 路由守卫双兜底）。
func TestDeploymentDisableKeepsHistory(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	prov := seedProvider(t, s, "glm")
	dep := seedDeployment(t, s, prov, "glm-up")
	if err := s.InsertRoute(ctx, &domain.ModelRoute{
		ModelID: f.modelID, DeploymentID: dep, Weight: 1, Enabled: true,
		PoolStrategy: domain.PoolRoundRobin,
	}); err != nil {
		t.Fatal(err)
	}
	rid := seedOpsRequest(t, s, f, "glm-4.6", "settled", "reported")
	seedOpsAttempt(t, s, rid, 1, dep, "", nil, "", "", time.Second)

	// 停用：状态翻转，路由与尝试记录原样保留。
	if _, err := s.db.Exec(
		`UPDATE inference_deployments SET status = 'disabled' WHERE id = $1`, dep); err != nil {
		t.Fatal(err)
	}
	routes, err := s.ListRoutes(ctx, f.modelID)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes after disable = %+v err=%v", routes, err)
	}
	atts, err := s.ListAttempts(ctx, rid)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts after disable = %+v err=%v", atts, err)
	}

	// DELETE 被路由守卫拒绝；先删路由则被 attempts FK 拒绝（历史不可抹）。
	if err := s.DeleteDeployment(ctx, dep); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delete with routes = %v, want conflict", err)
	}
	if _, err := s.db.Exec(`DELETE FROM inference_model_routes WHERE deployment_id = $1`, dep); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDeployment(ctx, dep); err == nil {
		t.Fatal("delete with historical attempts must fail (FK)")
	}
}
