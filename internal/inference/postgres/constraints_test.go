package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// 验收口径：请求/尝试/结算唯一键冲突、负数 CHECK、币种 CHECK、
// 窗口重叠 EXCLUDE、账本不对用户级联删除 — 全部在真实 PostgreSQL 上验证。

func TestUniqueKeys_RequestAttemptUsage(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	req := &domain.Request{
		ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
		EntitlementID: f.entID, ModelID: f.modelID,
		Protocol: domain.ProtocolOpenAIChat, PolicyVersionID: f.policyID,
	}
	if err := s.InsertRequest(ctx, req); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	// 请求唯一键：重复 request id → CodeConflict
	dup := *req
	if err := s.InsertRequest(ctx, &dup); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate request: err = %v, want CodeConflict", err)
	}

	att := &domain.Attempt{ID: uuid.NewString(), RequestID: req.ID, AttemptNo: 1}
	if err := s.InsertAttempt(ctx, att); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	// 尝试唯一键：UNIQUE(request_id, attempt_no)
	dupAtt := &domain.Attempt{ID: uuid.NewString(), RequestID: req.ID, AttemptNo: 1}
	if err := s.InsertAttempt(ctx, dupAtt); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate attempt_no: err = %v, want CodeConflict", err)
	}

	in, out := int64(100), int64(50)
	rec := &domain.UsageRecord{
		RequestID: req.ID, AttemptID: att.ID, Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
	}
	if err := s.InsertUsageRecord(ctx, rec); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
	// 计量唯一键：UNIQUE(attempt_id, revision)
	dupRec := &domain.UsageRecord{
		RequestID: req.ID, AttemptID: att.ID, Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
	}
	if err := s.InsertUsageRecord(ctx, dupRec); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate usage revision: err = %v, want CodeConflict", err)
	}
}

func TestNegativeChecksRejected(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 负数窗口限额 → CHECK → CodeInvalidInput
	w := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowFiveHour,
		Start: time.Now().UTC(), End: time.Now().UTC().Add(5 * time.Hour),
		Limit: -1,
	}
	if err := s.InsertQuotaWindow(ctx, w); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("negative limit: err = %v, want CodeInvalidInput (CHECK)", err)
	}

	// 窗口 end <= start → CHECK
	ts := time.Now().UTC()
	w2 := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowFiveHour,
		Start: ts, End: ts,
		Limit: 100,
	}
	if err := s.InsertQuotaWindow(ctx, w2); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("empty window range: err = %v, want CodeInvalidInput (CHECK)", err)
	}

	// 负数价格 → CHECK
	pv := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: -5, Revision: 1, EffectiveFrom: time.Now().UTC(),
	}
	if err := s.InsertPriceVersion(ctx, pv); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("negative price: err = %v, want CodeInvalidInput (CHECK)", err)
	}

	// 负数 Key 预算 → CHECK
	key := &domain.APIKey{
		BillingAccountID: f.accountID, Prefix: "yk-neg-" + uuid.NewString()[:8],
		BudgetLimit: micro(-1),
	}
	if err := s.InsertAPIKey(ctx, key, "sha256:x"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("negative key budget: err = %v, want CodeInvalidInput (CHECK)", err)
	}
}

func TestCurrencyAndUnitChecks(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	now := time.Now().UTC()

	// 小写币种 → CHECK (^[A-Z]{3}$)
	pv := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleMoney, Unit: "micromoney", Currency: "usd",
		Revision: 1, EffectiveFrom: now,
	}
	if err := s.InsertPriceVersion(ctx, pv); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("lowercase currency: err = %v, want CodeInvalidInput", err)
	}
	// micromoney 缺币种 → CHECK ((unit='microcredit') = (currency IS NULL))
	pv2 := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleMoney, Unit: "micromoney",
		Revision: 2, EffectiveFrom: now,
	}
	if err := s.InsertPriceVersion(ctx, pv2); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("micromoney without currency: err = %v, want CodeInvalidInput", err)
	}
	// microcredit 带币种 → 同 CHECK
	pv3 := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit", Currency: "CNY",
		Revision: 3, EffectiveFrom: now,
	}
	if err := s.InsertPriceVersion(ctx, pv3); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("microcredit with currency: err = %v, want CodeInvalidInput", err)
	}
	// 合法组合通过
	pv4 := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleMoney, Unit: "micromoney", Currency: "CNY",
		InputPerMtok: 800, OutputPerMtok: 3200, Revision: 4, EffectiveFrom: now,
	}
	if err := s.InsertPriceVersion(ctx, pv4); err != nil {
		t.Fatalf("valid price: %v", err)
	}
	// 价格版本唯一键：UNIQUE(model_id, kind, revision)
	dup := *pv4
	if err := s.InsertPriceVersion(ctx, &dup); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate price revision: err = %v, want CodeConflict", err)
	}
}

func TestWindowOverlapExcluded(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	base := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)

	mk := func(start, end time.Time) *domain.QuotaWindow {
		return &domain.QuotaWindow{
			EntitlementID: f.entID, Kind: domain.WindowFiveHour,
			Start: start, End: end, Limit: 1000,
		}
	}
	if err := s.InsertQuotaWindow(ctx, mk(base, base.Add(5*time.Hour))); err != nil {
		t.Fatalf("first window: %v", err)
	}
	// 同权益同 kind 重叠活跃窗口 → EXCLUDE → CodeConflict
	if err := s.InsertQuotaWindow(ctx, mk(base.Add(time.Hour), base.Add(6*time.Hour))); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("overlapping window: err = %v, want CodeConflict (EXCLUDE)", err)
	}
	// 半开区间：紧贴边界 [end, ...) 不重叠 → 允许
	if err := s.InsertQuotaWindow(ctx, mk(base.Add(5*time.Hour), base.Add(10*time.Hour))); err != nil {
		t.Fatalf("adjacent window [end,...) must be allowed: %v", err)
	}
	// 被撤销（voided）的窗口不参与排他，可重叠
	voided := mk(base.Add(30*time.Minute), base.Add(5*time.Hour+30*time.Minute))
	voided.Voided = true
	if err := s.InsertQuotaWindow(ctx, voided); err != nil {
		t.Fatalf("voided window overlapping active must be allowed: %v", err)
	}
	// 不同 kind 同区间 → 允许
	weekly := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowWeekly,
		Start: base, End: base.Add(7 * 24 * time.Hour), Limit: 1000,
	}
	if err := s.InsertQuotaWindow(ctx, weekly); err != nil {
		t.Fatalf("different kind same range must be allowed: %v", err)
	}
}

func TestBillingAccountNotCascadeDeleted(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 账本边界（设计 §7.3）：删除用户不得级联清掉计费账户 — FK 拒绝。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, f.userID); err == nil {
		t.Fatal("delete user with billing account must fail (no CASCADE)")
	} else if domain.CodeOf(mapError("delete user", err)) != domain.CodeInvalidInput {
		t.Fatalf("delete user: err = %v, want FK violation", err)
	}
	// 账户仍在。
	if _, err := s.GetBillingAccountByUser(ctx, f.userID); err != nil {
		t.Fatalf("billing account must survive failed user delete: %v", err)
	}
}

func TestIdempotencyKeys(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	now := time.Now().UTC()

	// 权益来源幂等键 UNIQUE(source_type, source_id, revision)
	ent := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceOrder,
		SourceID: "order-1", ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := s.InsertEntitlement(ctx, ent); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
	dup := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceOrder,
		SourceID: "order-1", ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := s.InsertEntitlement(ctx, dup); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate entitlement grant: err = %v, want CodeConflict", err)
	}

	// API Key 前缀唯一
	key := &domain.APIKey{
		BillingAccountID: f.accountID, Prefix: "yk-dup-" + uuid.NewString()[:8],
	}
	if err := s.InsertAPIKey(ctx, key, "sha256:a"); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	dupKey := &domain.APIKey{BillingAccountID: f.accountID, Prefix: key.Prefix}
	if err := s.InsertAPIKey(ctx, dupKey, "sha256:b"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate key prefix: err = %v, want CodeConflict", err)
	}

	// 每用户一个计费账户 UNIQUE(user_id)
	acct := &domain.BillingAccount{UserID: f.userID}
	if err := s.InsertBillingAccount(ctx, acct); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second billing account: err = %v, want CodeConflict", err)
	}

	// 调整幂等键 UNIQUE(idempotency_key)
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adj := Adjustment{
		BillingAccountID: f.accountID, Reason: "refund compensation",
		AmountMicros: 100, Direction: "credit",
		OperatorSubject: "op-1", IdempotencyKey: "adj-" + uuid.NewString(),
	}
	if _, err := s.AppendAdjustment(ctx, uow, adj); err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit adjustment: %v", err)
	}
	uow2, _ := s.Begin(ctx)
	if _, err := s.AppendAdjustment(ctx, uow2, adj); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate adjustment: err = %v, want CodeConflict", err)
	}
	_ = uow2.Rollback(ctx)
}

func TestCatalogBasics(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()

	// 模型 ID 格式 CHECK
	bad := &domain.Model{ID: "Bad Model!", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}
	if err := s.InsertModel(ctx, bad); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("bad model id: err = %v, want CodeInvalidInput", err)
	}

	m := &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6",
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
	}
	if err := s.InsertModel(ctx, m); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	got, err := s.GetModel(ctx, "glm-4.6")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if got.Lifecycle != domain.LifecycleDraft {
		t.Errorf("new model lifecycle = %q, want draft (新模型默认不可售)", got.Lifecycle)
	}

	p := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu", AccessType: domain.AccessOfficialAPI}
	if err := s.InsertProvider(ctx, p); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	d := &domain.Deployment{
		ProviderID: p.ID, UpstreamModel: "glm-4.6-2026", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: 5 * time.Second, RequestTimeout: 10 * time.Minute,
	}
	if err := s.InsertDeployment(ctx, d); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	r := &domain.ModelRoute{ModelID: m.ID, DeploymentID: d.ID, Priority: 0, Weight: 1, Enabled: true}
	if err := s.InsertRoute(ctx, r); err != nil {
		t.Fatalf("insert route: %v", err)
	}
	// 路由唯一键 UNIQUE(model_id, deployment_id)
	dupR := &domain.ModelRoute{ModelID: m.ID, DeploymentID: d.ID, Weight: 1}
	if err := s.InsertRoute(ctx, dupR); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate route: err = %v, want CodeConflict", err)
	}
	routes, err := s.RoutesForModel(ctx, m.ID)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes = %v, %v; want 1", routes, err)
	}

	// 配置发布：原子切换 active revision（部分唯一索引保证每 scope 一个 active）
	rev1 := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 1, CreatedBy: "test"}
	if err := s.InsertConfigRevision(ctx, rev1); err != nil {
		t.Fatalf("rev1: %v", err)
	}
	rev2 := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 2, CreatedBy: "test"}
	if err := s.InsertConfigRevision(ctx, rev2); err != nil {
		t.Fatalf("rev2: %v", err)
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 1); err != nil {
		t.Fatalf("activate 1: %v", err)
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 2); err != nil {
		t.Fatalf("activate 2: %v", err)
	}
	active, err := s.ActiveRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		t.Fatalf("active revision: %v", err)
	}
	if active.Revision != 2 || !active.IsActive {
		t.Errorf("active = %+v, want revision 2 active", active)
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 99); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("activate missing revision: err = %v, want CodeNotFound", err)
	}
}

func TestStoreRejectsForeignUnitOfWork(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	// 预占/结算不得隐藏使用另一个连接（任务书）：外来 UnitOfWork 必须被拒绝。
	foreign := fakeUoW{}
	_, err := s.Reserve(ctx, foreign, domain.ReserveCommand{})
	if err == nil {
		t.Fatal("Reserve with foreign UnitOfWork must fail")
	}
	if err := s.Settle(ctx, foreign, domain.SettleCommand{}); err == nil {
		t.Fatal("Settle with foreign UnitOfWork must fail")
	}
}

type fakeUoW struct{}

func (fakeUoW) Commit(ctx context.Context) error   { return nil }
func (fakeUoW) Rollback(ctx context.Context) error { return nil }
