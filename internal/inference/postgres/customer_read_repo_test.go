package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// customer_read_repo_test.go — Task 11 客户读模型的真实库验证：
// 分组聚合（模型/Key）、账本冲正归属、token 桶最新 revision、UTC 日序列、
// keyset 分页稳定性、订阅读取（跨域只读）、心跳表隔离（usage 绝不来自
// usage_events）。

// settleOne drives the real reserve → settle pipeline for one request
// against the graph's SHARED windows (one window row per kind per
// entitlement — the EXCLUDE no-overlap constraint forbids a second row).
func settleOne(t *testing.T, s *Store, f fixture, wins [3]string, keyID *string, modelID string, charge domain.Microcredit, src domain.UsageSource, buckets domain.UsageBuckets) string {
	t.Helper()
	ctx := context.Background()
	w5, ww, wm := wins[0], wins[1], wins[2]
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, charge+10_000) // hold > charge（预占上界）
	cmd.Request.APIKeyID = keyID
	cmd.Request.ModelID = modelID
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	attemptID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attemptID, Source: src, Buckets: buckets,
		},
		ChargeMicros: charge,
		AttemptID:    &attemptID,
		SettledAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return adm.RequestID
}

// releaseOne drives reserve → release (confirmed zero consumption).
func releaseOne(t *testing.T, s *Store, f fixture, wins [3]string) string {
	t.Helper()
	ctx := context.Background()
	w5, ww, wm := wins[0], wins[1], wins[2]
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 5_000)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Release(ctx, uow, adm.RequestID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

// reserveOnly leaves an in-flight (held) reservation — the 预占中 state.
func reserveOnly(t *testing.T, s *Store, f fixture, wins [3]string) string {
	t.Helper()
	ctx := context.Background()
	w5, ww, wm := wins[0], wins[1], wins[2]
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 7_500)
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

// insertParkedRequest inserts a reconciliation_required/unknown request
// （待核对：已产生流量但计量事实缺失；预占保留）. api_key_id is NULL —
// facade-originated rows have no key.
func insertParkedRequest(t *testing.T, s *Store, f fixture) string {
	t.Helper()
	ctx := context.Background()
	req := &domain.Request{
		ID: uuid.NewString(), BillingAccountID: f.accountID,
		EntitlementID: f.entID, ModelID: f.modelID,
		Protocol: domain.ProtocolOpenAIChat, Status: domain.ReqReconciliationRequired,
		PolicyVersionID: f.policyID,
		ReservedMicros:  micro(9_999),
		UsageStatus:     domain.UsageUnknown,
		LastError:       "upstream stream interrupted without final usage",
	}
	if err := s.InsertRequest(ctx, req); err != nil {
		t.Fatalf("insert parked request: %v", err)
	}
	return req.ID
}

func usageRange() (from, to time.Time) {
	now := time.Now().UTC()
	return now.Add(-time.Hour), now.Add(time.Hour)
}

func groupByKey(groups []management.UsageGroup, key string) *management.UsageGroup {
	for i := range groups {
		g := &groups[i]
		if g.ModelID != nil && *g.ModelID == key {
			return g
		}
		if g.APIKeyID != nil && *g.APIKeyID == key {
			return g
		}
		if g.ModelID == nil && g.APIKeyID == nil && key == "" {
			return g
		}
	}
	return nil
}

// seedUsageGraph builds the shared request graph used by the summary/series
// tests: 2 settled (reported + estimated) on glm-4.6, 1 released, 1
// in-flight, 1 parked (unknown, no key), 1 settled on deepseek-v4, one
// reversal against the first request, one out-of-range settled request.
type usageGraph struct {
	req1, req2, req3, req4, req5, req6, reqOut string
	key2ID                                     string
}

func seedUsageGraph(t *testing.T, s *Store, f fixture) usageGraph {
	t.Helper()
	ctx := context.Background()
	var g usageGraph
	wins := [3]string{}
	wins[0], wins[1], wins[2] = makeWindows(t, s, f.entID)

	if err := s.InsertModel(ctx, &domain.Model{
		ID: "deepseek-v4", DisplayName: "DSv4", ContextTokens: 128000, MaxOutputTokens: 4096,
	}); err != nil {
		t.Fatalf("insert model2: %v", err)
	}
	key2 := &domain.APIKey{
		BillingAccountID: f.accountID, Name: "web", Prefix: "yk-test-" + uuid.NewString()[:8],
	}
	if err := s.InsertAPIKey(ctx, key2, "sha256:"+uuid.NewString()); err != nil {
		t.Fatalf("insert key2: %v", err)
	}
	g.key2ID = key2.ID

	in1, out1 := int64(800), int64(200)
	g.req1 = settleOne(t, s, f, wins, &f.keyID, f.modelID, 60_000, domain.UsageReported,
		domain.UsageBuckets{InputTokens: &in1, OutputTokens: &out1})
	// 修正 revision 2：输出改为 250（历史保留，展示取最新）。
	att1 := ""
	if err := s.db.GetContext(ctx, &att1,
		`SELECT id FROM inference_attempts WHERE request_id = $1`, g.req1); err != nil {
		t.Fatal(err)
	}
	out1b := int64(250)
	if err := s.InsertUsageRecord(ctx, &domain.UsageRecord{
		RequestID: g.req1, AttemptID: att1, Source: domain.UsageReported, Revision: 2,
		Buckets: domain.UsageBuckets{InputTokens: &in1, OutputTokens: &out1b},
	}); err != nil {
		t.Fatalf("usage correction: %v", err)
	}

	in2, out2 := int64(100), int64(50)
	g.req2 = settleOne(t, s, f, wins, &g.key2ID, f.modelID, 20_000, domain.UsageEstimated,
		domain.UsageBuckets{InputTokens: &in2, OutputTokens: &out2})
	g.req3 = insertParkedRequest(t, s, f)
	g.req4 = releaseOne(t, s, f, wins)
	g.req5 = reserveOnly(t, s, f, wins)
	g.req6 = settleOne(t, s, f, wins, &f.keyID, "deepseek-v4", 5_000, domain.UsageReported, domain.UsageBuckets{})

	// 账本冲正：req1 的 10_000 被退回（追加 reversal，不重写 charge）。
	var chargeID int64
	if err := s.db.GetContext(ctx, &chargeID,
		`SELECT id FROM inference_ledger_entries WHERE request_id = $1 AND entry_type = 'charge'`, g.req1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO inference_ledger_entries
		 (billing_account_id, request_id, entry_type, amount_micros, unit, reverses_entry_id)
		 VALUES ($1, $2, 'reversal', 10000, 'microcredit', $3)`,
		f.accountID, g.req1, chargeID); err != nil {
		t.Fatalf("reversal: %v", err)
	}

	// 范围外请求（10 天前）——任何查询都不得计入。
	g.reqOut = settleOne(t, s, f, wins, &f.keyID, f.modelID, 40_000, domain.UsageReported, domain.UsageBuckets{})
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inference_requests SET created_at = now() - interval '10 days' WHERE id = $1`, g.reqOut); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCustomerRead_SummarizeByModel(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	seedUsageGraph(t, s, f)
	from, to := usageRange()

	groups, err := s.SummarizeUsageGroups(context.Background(), f.accountID,
		management.UsageSummaryFilter{From: from, To: to, GroupBy: management.GroupByModel})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	g := groupByKey(groups, f.modelID)
	if g == nil {
		t.Fatalf("model group missing: %+v", groups)
	}
	// req1 settled + req2 settled + req3 parked + req4 released + req5 in-flight。
	if g.RequestsTotal != 5 || g.SettledRequests != 2 || g.ReleasedRequests != 1 ||
		g.InFlightRequests != 1 || g.ReconciliationPending != 1 {
		t.Errorf("counts = %+v", g)
	}
	if g.Reported != 1 || g.Estimated != 1 || g.Unknown != 1 || g.Pending != 2 {
		t.Errorf("integrity = reported %d estimated %d unknown %d pending %d, want 1/1/1/2",
			g.Reported, g.Estimated, g.Unknown, g.Pending)
	}
	if g.ChargeMicros != 80_000 || g.ReversedMicros != 10_000 || g.NetMicros() != 70_000 {
		t.Errorf("amounts = charge %d reversed %d", g.ChargeMicros, g.ReversedMicros)
	}
	// token：req1 最新 revision（output 250）+ req2（100/50）；未报告桶保持
	// NULL（未知 ≠ 0）。
	if g.Tokens.InputTokens == nil || *g.Tokens.InputTokens != 900 {
		t.Errorf("input = %v, want 900 (800+100)", g.Tokens.InputTokens)
	}
	if g.Tokens.OutputTokens == nil || *g.Tokens.OutputTokens != 300 {
		t.Errorf("output = %v, want 300 (250 修正后 +50)", g.Tokens.OutputTokens)
	}
	if g.Tokens.CacheReadTokens != nil || g.Tokens.ReasoningTokens != nil {
		t.Errorf("unreported buckets must stay null: %+v", g.Tokens)
	}

	g2 := groupByKey(groups, "deepseek-v4")
	if g2 == nil || g2.RequestsTotal != 1 || g2.ChargeMicros != 5_000 {
		t.Errorf("deepseek group = %+v", g2)
	}
	// 范围外请求未计入任何组（glm-4.6 组恰为 5 个范围内请求；若 reqOut
	// 漏入则 total=6、charge=120_000）。
	if g.RequestsTotal != 5 || g.ChargeMicros != 80_000 {
		t.Errorf("out-of-range request leaked: %+v", g)
	}
}

func TestCustomerRead_SummarizeByKey(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	g := seedUsageGraph(t, s, f)
	from, to := usageRange()

	groups, err := s.SummarizeUsageGroups(context.Background(), f.accountID,
		management.UsageSummaryFilter{From: from, To: to, GroupBy: management.GroupByKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("key groups = %+v, want key1/key2/null", groups)
	}
	k1 := groupByKey(groups, f.keyID)
	if k1 == nil || k1.RequestsTotal != 4 || k1.ChargeMicros != 65_000 {
		t.Errorf("key1 group = %+v (req1+req4+req5+req6)", k1)
	}
	if k1 == nil || k1.KeyName != "cli" || k1.KeyPrefix == "" {
		t.Errorf("key1 display = %+v", k1)
	}
	k2 := groupByKey(groups, g.key2ID)
	if k2 == nil || k2.RequestsTotal != 1 || k2.KeyName != "web" {
		t.Errorf("key2 group = %+v", k2)
	}
	nullGroup := groupByKey(groups, "")
	if nullGroup == nil || nullGroup.RequestsTotal != 1 || nullGroup.ReconciliationPending != 1 {
		t.Errorf("null-key (facade) group = %+v", nullGroup)
	}
}

func TestCustomerRead_SummarizeSeriesUTCDays(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	g := seedUsageGraph(t, s, f)
	ctx := context.Background()

	// reqOut 已在 10 天前；再把 req6 拨到 3 天前 → 范围内两天两个桶。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inference_requests SET created_at = now() - interval '3 days' WHERE id = $1`, g.req6); err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().UTC().Add(-4*24*time.Hour), time.Now().UTC().Add(time.Hour)
	series, err := s.SummarizeUsageSeries(ctx, f.accountID,
		management.UsageSummaryFilter{From: from, To: to, GroupBy: management.GroupByModel})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("series = %+v, want 2 day buckets", series)
	}
	for _, b := range series {
		if b.BucketStart.Hour() != 0 || b.BucketStart.Minute() != 0 || b.BucketStart.Location() != time.UTC {
			t.Errorf("bucket_start not UTC midnight: %v", b.BucketStart)
		}
	}
	if !series[0].BucketStart.Before(series[1].BucketStart) {
		t.Errorf("series order = %v then %v", series[0].BucketStart, series[1].BucketStart)
	}
	// 当日桶：req1..req5（req6 已拨走）。
	today := series[1]
	if today.RequestsTotal != 5 || today.ChargeMicros != 80_000 ||
		today.Reported != 1 || today.Estimated != 1 || today.Unknown != 1 {
		t.Errorf("today bucket = %+v", today)
	}
	old := series[0]
	if old.RequestsTotal != 1 || old.ChargeMicros != 5_000 {
		t.Errorf("old bucket = %+v", old)
	}

	// model_id 过滤收窄序列。
	series2, err := s.SummarizeUsageSeries(ctx, f.accountID,
		management.UsageSummaryFilter{From: from, To: to, GroupBy: management.GroupByModel, ModelID: "deepseek-v4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(series2) != 1 || series2[0].RequestsTotal != 1 {
		t.Errorf("filtered series = %+v", series2)
	}
}

func TestCustomerRead_ListRequestsKeyset(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	g := seedUsageGraph(t, s, f)
	ctx := context.Background()

	// 钉死 created_at（含一组同刻并列，验证 id tiebreak）：范围 1h 内。
	base := time.Now().UTC().Add(-time.Minute)
	times := map[string]time.Time{
		g.req1: base, g.req2: base.Add(-time.Minute), g.req3: base.Add(-2 * time.Minute),
		g.req4: base.Add(-2 * time.Minute), // 与 req3 同刻
		g.req5: base.Add(-3 * time.Minute), g.req6: base.Add(-4 * time.Minute),
	}
	for id, ts := range times {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE inference_requests SET created_at = $2 WHERE id = $1`, id, ts); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{g.req1, g.req2}
	// 同刻并列：id DESC 决定 req3/req4 顺序。
	if g.req3 > g.req4 {
		want = append(want, g.req3, g.req4)
	} else {
		want = append(want, g.req4, g.req3)
	}
	want = append(want, g.req5, g.req6)

	from, to := usageRange()
	var got []string
	var cursor *management.RequestCursor
	for page := 0; page < 6; page++ {
		fq := management.RequestListFilter{From: from, To: to, Limit: 2, Cursor: cursor}
		rows, err := s.ListRequestRows(ctx, f.accountID, fq)
		if err != nil {
			t.Fatal(err)
		}
		// store 返回 limit+1 探测行；服务层负责截断 —— 这里模拟同一约定。
		if len(rows) > 2 {
			rows = rows[:2]
			last := rows[len(rows)-1]
			cursor = &management.RequestCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		} else {
			cursor = nil
		}
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if cursor == nil {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("paginated %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page order %v, want %v (keyset: created_at DESC, id DESC)", got, want)
		}
	}

	// 明细增强：req1 有 usage + 冲正；req3 待核对（无计量事实）。
	rows, err := s.ListRequestRows(ctx, f.accountID, management.RequestListFilter{
		From: from, To: to, Limit: 10, ModelID: f.modelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var r1, r3 *management.RequestRow
	for i := range rows {
		switch rows[i].ID {
		case g.req1:
			r1 = &rows[i]
		case g.req3:
			r3 = &rows[i]
		}
	}
	if r1 == nil || r3 == nil {
		t.Fatalf("rows missing: %+v", rows)
	}
	if !r1.HasUsage || r1.Tokens.OutputTokens == nil || *r1.Tokens.OutputTokens != 250 {
		t.Errorf("req1 tokens = %+v (latest revision wins)", r1.Tokens)
	}
	if r1.ReversedMicros != 10_000 || r1.NetMicrosPtr() == nil || *r1.NetMicrosPtr() != 50_000 {
		t.Errorf("req1 net = %v reversed %d", r1.NetMicrosPtr(), r1.ReversedMicros)
	}
	if r1.SettledMicros == nil || *r1.SettledMicros != 60_000 {
		t.Errorf("req1 charge = %v", r1.SettledMicros)
	}
	if r3.HasUsage {
		t.Errorf("parked request must have no usage record: %+v", r3.Tokens)
	}
	if r3.Status != domain.ReqReconciliationRequired || r3.UsageStatus != domain.UsageUnknown {
		t.Errorf("req3 status = %s/%s", r3.Status, r3.UsageStatus)
	}
	if r3.ReservedMicros == nil || *r3.ReservedMicros != 9_999 {
		t.Errorf("req3 reserved = %v (预占保留可见)", r3.ReservedMicros)
	}
	if r3.APIKeyID != nil {
		t.Errorf("facade request key = %v, want null", r3.APIKeyID)
	}

	// api_key_id 过滤。
	rows, err = s.ListRequestRows(ctx, f.accountID, management.RequestListFilter{
		From: from, To: to, Limit: 10, APIKeyID: g.key2ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != g.req2 {
		t.Errorf("key filter rows = %+v", rows)
	}
}

func TestCustomerRead_HeartbeatTableIsNotAUsageSource(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	g := seedUsageGraph(t, s, f)
	ctx := context.Background()

	// 塞入 usage_events 心跳行（客户端活跃信号），汇总结果必须分毫不动
	// （验收硬项：模型用量来自 inference_usage_records/ledger，不是心跳表）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO apps (app_id, name, is_active) VALUES ('kaya', 'Kaya', true)
		 ON CONFLICT (app_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO usage_events (user_id, app_id, client_event_id, occurred_at, local_date, active_seconds, platform, app_version)
			 VALUES ($1, 'kaya', $2, now(), CURRENT_DATE, 300, 'macos', '1.0.0')`,
			f.userID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	from, to := usageRange()
	groups, err := s.SummarizeUsageGroups(ctx, f.accountID,
		management.UsageSummaryFilter{From: from, To: to, GroupBy: management.GroupByModel})
	if err != nil {
		t.Fatal(err)
	}
	gr := groupByKey(groups, f.modelID)
	if gr == nil || gr.RequestsTotal != 5 || gr.ChargeMicros != 80_000 {
		t.Errorf("heartbeat rows changed the summary: %+v", gr)
	}
	rows, err := s.ListRequestRows(ctx, f.accountID, management.RequestListFilter{From: from, To: to, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 { // req1..req6（reqOut 在范围外）
		t.Errorf("rows = %d, want 6 (心跳行不得成为用量明细)", len(rows))
	}
	_ = g
}

func TestCustomerRead_ListEntitlementsAllStatuses(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour).UTC()
	expired := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceOrder,
		SourceID: "order-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: past.Add(-24 * time.Hour),
		EffectiveFrom: past.Add(-24 * time.Hour), EffectiveTo: &past,
		Status:        domain.EntitlementExpired,
	}
	if err := s.InsertEntitlement(ctx, expired); err != nil {
		t.Fatal(err)
	}
	revoked := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceGrant,
		SourceID: "bundle:sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: past.Add(-48 * time.Hour),
		EffectiveFrom: past.Add(-48 * time.Hour), Status: domain.EntitlementRevoked,
	}
	if err := s.InsertEntitlement(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	ents, err := s.ListEntitlements(ctx, f.accountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 3 {
		t.Fatalf("entitlements = %d, want 3 (active+expired+revoked)", len(ents))
	}
	seen := map[domain.EntitlementStatus]bool{}
	for _, e := range ents {
		seen[e.Status] = true
	}
	if !seen[domain.EntitlementActive] || !seen[domain.EntitlementExpired] || !seen[domain.EntitlementRevoked] {
		t.Errorf("statuses = %v", seen)
	}
}

func TestCustomerRead_ListProductSubscriptions(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()

	userID := uuid.NewString()
	otherUser := uuid.NewString()
	for _, u := range []string{userID, otherUser} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO apps (app_id, name, is_active) VALUES ('kaya', 'Kaya', true)
		 ON CONFLICT (app_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		 VALUES ('cp_basic', 'Coding Plan Basic', 29.9, 30, '{}', 'CNY', 'coding-plan'),
		        ('cp_pro', 'Coding Plan Pro', 99.9, 30, '{}', 'CNY', 'coding-plan'),
		        ('monthly', 'Kaya Monthly', 9.9, 30, '{}', 'CNY', 'kaya-membership')
		 ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	mkSub := func(user, plan, product, status string, createdAgo time.Duration) {
		id := uuid.NewString()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO subscriptions (id, user_id, plan_id, product_code, status, started_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, now() - $6::interval, now() - $6::interval)`,
			id, user, plan, product, status, createdAgo.String()); err != nil {
			t.Fatalf("insert sub %s/%s: %v", plan, status, err)
		}
	}
	mkSub(userID, "cp_basic", "coding-plan", "cancelled", 72*time.Hour)
	mkSub(userID, "cp_pro", "coding-plan", "active", 24*time.Hour)
	mkSub(userID, "monthly", "kaya-membership", "active", 96*time.Hour)
	mkSub(otherUser, "cp_basic", "coding-plan", "active", time.Hour)

	subs, err := s.ListProductSubscriptions(ctx, userID, "coding-plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("subs = %+v, want 2 coding-plan rows", subs)
	}
	// 最新在前；plan_name 经 LEFT JOIN 带出；产品隔离（无 kaya、无他人）。
	if subs[0].PlanID != "cp_pro" || subs[0].PlanName == nil || *subs[0].PlanName != "Coding Plan Pro" {
		t.Errorf("subs[0] = %+v", subs[0])
	}
	if subs[1].Status != "cancelled" {
		t.Errorf("subs[1] = %+v (cancelled 历史行也列出)", subs[1])
	}
	for _, sub := range subs {
		if sub.ProductCode != "coding-plan" {
			t.Errorf("非 coding-plan 行混入: %+v", sub)
		}
	}

	kaya, err := s.ListProductSubscriptions(ctx, userID, "kaya-membership")
	if err != nil {
		t.Fatal(err)
	}
	if len(kaya) != 1 || kaya[0].PlanID != "monthly" {
		t.Errorf("kaya subs = %+v", kaya)
	}
}
