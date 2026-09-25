package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// pricing_repo_test.go — inference_price_versions 真实库行为（Task 6）：
// 不可变版本、生效边界 [from,to)、钉住版本的请求不受新价影响、账本
// charge 携带价格版本归因。

func insertCreditPrice(t *testing.T, s *Store, modelID string, rev int, from time.Time, to *time.Time, inPerMtok, outPerMtok int64) *PriceVersion {
	t.Helper()
	p := &PriceVersion{
		ModelID: modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: inPerMtok, OutputPerMtok: outPerMtok,
		Revision: rev, EffectiveFrom: from, EffectiveTo: to,
	}
	if err := s.InsertPriceVersion(context.Background(), p); err != nil {
		t.Fatalf("insert price v%d: %v", rev, err)
	}
	return p
}

// TestPriceChangeDoesNotAffectSettledRequest 验收硬项：价格版本不可变，
// 旧请求仍按旧版本结算。
func TestPriceChangeDoesNotAffectSettledRequest(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// v1：输入 1、输出 2 microcredit/token（1_000_000/2_000_000 per Mtok）。
	from := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	v1 := insertCreditPrice(t, s, f.modelID, 1, from, nil, 1_000_000, 2_000_000)

	// 请求钉住 v1；结算额度由规则层 Quote 计算。
	pure1, err := v1.Pure()
	if err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	usage := domain.UsageRecord{
		AttemptID: uuid.NewString(), Source: domain.UsageReported,
		Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
	}
	charge, err := pure1.Quote(domain.UsageRecord{Source: usage.Source, Buckets: usage.Buckets}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if charge.Credit != 800+400 {
		t.Fatalf("charge = %d, want 1200", charge.Credit)
	}

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := reserveCmd(f, w5, ww, wm, 10_000)
	cmd.Request.PriceVersionID = &v1.ID // 入场钉住价格版本
	adm, err := s.Reserve(ctx, uow, cmd)
	if err != nil {
		t.Fatal(err)
	}
	att := &domain.Attempt{ID: usage.AttemptID, RequestID: adm.RequestID, AttemptNo: 1}
	if err := insertAttempt(ctx, mustTx(t, uow), att); err != nil {
		t.Fatal(err)
	}
	usage.RequestID = adm.RequestID
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID, Usage: usage,
		ChargeMicros: charge.Credit, SettledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 账本 charge 归因到钉住的 v1，且同一消费恰一条客户消费记录。
	var ledgerPrice string
	var charges int
	if err := s.db.QueryRow(
		`SELECT price_version_id::text, COUNT(*) OVER () FROM inference_ledger_entries
		 WHERE request_id = $1 AND entry_type = 'charge'`, adm.RequestID).Scan(&ledgerPrice, &charges); err != nil {
		t.Fatal(err)
	}
	if ledgerPrice != v1.ID || charges != 1 {
		t.Errorf("ledger: price %s charges %d, want pinned v1 exactly once", ledgerPrice, charges)
	}

	// 运营发布更贵的新价 v2（更晚生效区间之外，新版本立即覆盖最新解析）。
	v2 := insertCreditPrice(t, s, f.modelID, 2, from, nil, 9_000_000, 9_000_000)

	// 不可变：按 ID 读回的 v1 与钉住时的计价逐微一致。
	got1, err := s.GetPriceVersion(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.InputPerMtok != 1_000_000 || got1.OutputPerMtok != 2_000_000 {
		t.Errorf("v1 mutated: %+v", got1)
	}
	pure1again, err := got1.Pure()
	if err != nil {
		t.Fatal(err)
	}
	again, err := pure1again.Quote(domain.UsageRecord{Source: usage.Source, Buckets: usage.Buckets}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Credit != charge.Credit {
		t.Errorf("old request repriced: %d → %d (价格版本必须不可变)", charge.Credit, again.Credit)
	}

	// 新请求按新价：Latest 解析到 v2，同用量计价不同。
	latest, err := s.LatestPriceVersion(ctx, f.modelID, PriceSaleCredit, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != v2.ID {
		t.Errorf("latest = %s, want v2", latest.ID)
	}
	pure2, err := v2.Pure()
	if err != nil {
		t.Fatal(err)
	}
	newCharge, err := pure2.Quote(domain.UsageRecord{Source: usage.Source, Buckets: usage.Buckets}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newCharge.Credit != 7200+1800 {
		t.Errorf("v2 charge = %d, want 9000", newCharge.Credit)
	}

	// 未定价模型：repo 读不到 → NotFound；规则层拒绝未定价能力。
	if _, err := s.LatestPriceVersion(ctx, "unpriced-model", PriceSaleCredit, time.Now().UTC()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("unpriced model: %v, want not_found", err)
	}
	if _, err := accounting.ResolvePrice(nil, accounting.PriceSaleCredit, time.Now()); domain.CodeOf(err) != domain.CodeUnpricedCapability {
		t.Errorf("unpriced capability: %v, want unpriced_capability", err)
	}
}

func TestLatestPriceVersion_EffectiveBoundaries(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	from := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	v1 := insertCreditPrice(t, s, f.modelID, 1, from, &mid, 1, 1)
	v2 := insertCreditPrice(t, s, f.modelID, 2, mid, nil, 2, 2)

	// [from, to)：at==from 命中 v1；at==mid（v1 的 to、v2 的 from）命中 v2。
	got, err := s.LatestPriceVersion(ctx, f.modelID, PriceSaleCredit, from)
	if err != nil || got.ID != v1.ID {
		t.Errorf("at from: %v %v", got, err)
	}
	got, err = s.LatestPriceVersion(ctx, f.modelID, PriceSaleCredit, mid)
	if err != nil || got.ID != v2.ID {
		t.Errorf("at boundary must hit NEW revision: %v %v", got, err)
	}
	// 首个生效时刻之前：无版本。
	if _, err := s.LatestPriceVersion(ctx, f.modelID, PriceSaleCredit, from.Add(-time.Second)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("before first version: %v, want not_found", err)
	}
	// 未知版本 ID。
	if _, err := s.GetPriceVersion(ctx, uuid.NewString()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("unknown id: %v, want not_found", err)
	}
}

func TestPriceVersion_PureRoundTrip(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	p := &PriceVersion{
		ModelID: f.modelID, Kind: PriceSaleMoney, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 2_500_000, CacheReadPerMtok: 250_000,
		CacheWritePerMtok: 3_000_000, OutputPerMtok: 10_000_000,
		ExtraRates: domain.ExtensionConfig{SchemaVersion: 1,
			Raw: json.RawMessage(`{"schema_version":1,"rates":{"web_search":5000}}`)},
		Revision: 1, EffectiveFrom: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
	}
	if err := s.InsertPriceVersion(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPriceVersion(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	pure, err := got.Pure()
	if err != nil {
		t.Fatal(err)
	}
	if err := pure.Validate(); err != nil {
		t.Fatalf("pure validate: %v", err)
	}
	if pure.Kind != accounting.PriceSaleMoney || pure.Currency != "USD" {
		t.Errorf("pure = kind %s currency %q", pure.Kind, pure.Currency)
	}
	// 存储的 per-Mtok 微金额 → 每 token 精确有理率。
	if pure.Input.Num != 2_500_000 || pure.Input.Den != 1_000_000 {
		t.Errorf("input rate = %d/%d", pure.Input.Num, pure.Input.Den)
	}
	if r := pure.ExtraRates["web_search"]; r.Num != 5000 || r.Den != 1 {
		t.Errorf("extra rate = %v, want 5000/1", r)
	}

	// 金额计价走 Money（带币种），客户口径向上取整。
	in := int64(3)
	c, err := pure.Quote(domain.UsageRecord{
		Source: domain.UsageReported, Buckets: domain.UsageBuckets{InputTokens: &in},
	}, map[string]int64{"web_search": 2})
	if err != nil {
		t.Fatal(err)
	}
	// 3 tokens × 2.5 micro/token = 7.5 → 向上取整 8；2 × 5000 = 10000。
	if c.Money == nil || c.Money.Micros != 10008 || c.Money.Currency != "USD" {
		t.Errorf("money charge = %+v, want 10008 USD micros", c.Money)
	}
}

// TestGetPriceVersionByRevision: UNIQUE(model_id, kind, revision) 自然键读
// （管理端幂等预检）——命中返回完整视图，未命中 CodeNotFound。
func TestGetPriceVersionByRevision(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	from := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	v1 := insertCreditPrice(t, s, f.modelID, 1, from, nil, 1_000_000, 2_000_000)

	got, err := s.GetPriceVersionByRevision(ctx, f.modelID, PriceSaleCredit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != v1.ID || got.ModelID != f.modelID || got.Kind != PriceSaleCredit ||
		got.Unit != "microcredit" || got.Currency != "" ||
		got.InputPerMtok != 1_000_000 || got.OutputPerMtok != 2_000_000 ||
		got.Revision != 1 || !got.EffectiveFrom.Equal(from) || got.EffectiveTo != nil ||
		got.CreatedAt.IsZero() {
		t.Fatalf("by revision = %+v", got)
	}
	if string(got.ExtraRates) == "" {
		t.Fatalf("extra_rates must round-trip (default schema doc): %+v", got)
	}

	// 未命中（revision/kind/model 任一不同）→ not_found。
	if _, err := s.GetPriceVersionByRevision(ctx, f.modelID, PriceSaleCredit, 2); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing revision: %v, want not_found", err)
	}
	if _, err := s.GetPriceVersionByRevision(ctx, f.modelID, PriceSaleMoney, 1); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing kind: %v, want not_found", err)
	}
	if _, err := s.GetPriceVersionByRevision(ctx, "no-such-model", PriceSaleCredit, 1); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing model: %v, want not_found", err)
	}
}

// TestListPriceVersions: 过滤（model_id/kind）、固定排序
// (model_id, kind, revision DESC) 与 limit/offset 分页；空集返回空切片。
func TestListPriceVersions(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false) // model glm-4.6

	// 第二个模型（FK 要求真实存在）。
	if err := s.InsertModel(ctx, &domain.Model{
		ID: "deepseek-chat", DisplayName: "DeepSeek", ContextTokens: 1000, MaxOutputTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	insertCreditPrice(t, s, f.modelID, 1, from, nil, 100, 200)
	insertCreditPrice(t, s, f.modelID, 2, from, nil, 300, 400)
	// 金额口径：unit=micromoney + 币种（表 CHECK 口径）。
	if err := s.InsertPriceVersion(ctx, &PriceVersion{
		ModelID: f.modelID, Kind: PriceUpstreamCost, Unit: "micromoney", Currency: "USD",
		InputPerMtok: 50, OutputPerMtok: 100, Revision: 1, EffectiveFrom: from,
	}); err != nil {
		t.Fatal(err)
	}
	insertCreditPrice(t, s, "deepseek-chat", 1, from, nil, 1_000_000, 2_000_000)

	all, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("list = %d rows, want 4", len(all))
	}
	type key struct {
		model string
		kind  string
		rev   int
	}
	want := []key{
		{"deepseek-chat", PriceSaleCredit, 1},
		{f.modelID, PriceSaleCredit, 2},
		{f.modelID, PriceSaleCredit, 1},
		{f.modelID, PriceUpstreamCost, 1},
	}
	for i, w := range want {
		if all[i].ModelID != w.model || all[i].Kind != w.kind || all[i].Revision != w.rev {
			t.Fatalf("order[%d] = %s/%s/%d, want %v", i, all[i].ModelID, all[i].Kind, all[i].Revision, w)
		}
	}

	// model_id 过滤 + 币种行视图字段。
	byModel, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{ModelID: f.modelID, Limit: 100})
	if err != nil || len(byModel) != 3 {
		t.Fatalf("model filter = %d err=%v", len(byModel), err)
	}
	if byModel[2].Unit != "micromoney" || byModel[2].Currency != "USD" {
		t.Fatalf("money row = %+v", byModel[2])
	}

	// kind 过滤 + 组合过滤。
	byKind, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{Kind: PriceSaleCredit, Limit: 100})
	if err != nil || len(byKind) != 3 {
		t.Fatalf("kind filter = %d err=%v", len(byKind), err)
	}
	combo, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{ModelID: f.modelID, Kind: PriceSaleCredit, Limit: 100})
	if err != nil || len(combo) != 2 || combo[0].Revision != 2 || combo[1].Revision != 1 {
		t.Fatalf("combo filter = %+v err=%v", combo, err)
	}

	// 分页：limit=1 offset=1 → 全序第二项（glm sale_credit rev2）。
	page, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].Revision != 2 || page[0].ModelID != f.modelID {
		t.Fatalf("page = %+v err=%v", page, err)
	}

	// 空集：空切片（非 nil 语义由调用方/序列化保证，这里至少不报错）。
	none, err := s.ListPriceVersions(ctx, management.PriceVersionFilter{ModelID: "no-such-model", Limit: 100})
	if err != nil || len(none) != 0 {
		t.Fatalf("empty = %+v err=%v", none, err)
	}
}

// TestInsertPriceVersionTx: 管理端同事务写入路径——提交后自然键可读；
// 同 (model_id, kind, revision) 撞唯一键 → CodeConflict（409 竞态兜底）。
func TestInsertPriceVersionTx(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	from := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)

	info := &management.PriceVersionInfo{
		ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		ExtraRates: json.RawMessage(`{"schema_version":1}`),
		Revision:   1, EffectiveFrom: from,
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPriceVersionTx(ctx, uow, info); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.CreatedAt.IsZero() {
		t.Fatalf("tx insert must fill id/created_at: %+v", info)
	}
	got, err := s.GetPriceVersionByRevision(ctx, f.modelID, PriceSaleCredit, 1)
	if err != nil || got.ID != info.ID {
		t.Fatalf("readback = %+v err=%v", got, err)
	}

	// 唯一键竞态兜底：同 (model_id, kind, revision) 再插 → CodeConflict。
	dup := &management.PriceVersionInfo{
		ModelID: f.modelID, Kind: PriceSaleCredit, Unit: "microcredit",
		Revision: 1, EffectiveFrom: from,
	}
	uow2, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.InsertPriceVersionTx(ctx, uow2, dup)
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate insert = %v, want conflict", err)
	}
}
