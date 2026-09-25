package management

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// price_versions_test.go — 售价版本服务的纯编排单测（fake store，无真实
// 库）：µs 舍入（half-to-even）的内容比较、唯一键竞态兜底的固定 409 文案
// （不泄露 PG 约束名）、审计 detail 中 extra_rates 的可递归脱敏、revision
// 上限与 max+1 强制。

// --- fake store / uow ---

type fakeUOW struct{ rolledBack bool }

func (u *fakeUOW) Commit(context.Context) error   { return nil }
func (u *fakeUOW) Rollback(context.Context) error { u.rolledBack = true; return nil }

// fakeClock 每次 Now 前进一分钟（缺省 effective_from 的重放语义测试）。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time {
	t := c.now
	c.now = c.now.Add(time.Minute)
	return t
}

type fakePVStore struct {
	existing  *PriceVersionInfo
	insertErr error
	// failFirstLookup 让幂等预检未命中（走插入路径）；failReread 让竞态
	// 兜底的二次重读失败。
	failFirstLookup bool
	failReread      bool
	lookupCalls     int
	// maxRevision 是 MaxPriceVersionRevisionTx 的返回（0 = 该 (model,kind)
	// 尚无版本）。
	maxRevision int
	uow         *fakeUOW
}

func (f *fakePVStore) GetModel(context.Context, string) (*domain.Model, error) {
	return &domain.Model{ID: "m-1", DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100}, nil
}
func (f *fakePVStore) GetPriceVersionByRevision(context.Context, string, string, int) (*PriceVersionInfo, error) {
	f.lookupCalls++
	if (f.lookupCalls == 1 && f.failFirstLookup) || (f.lookupCalls > 1 && f.failReread) {
		return nil, domain.NewError(domain.CodeNotFound, "get price version by revision: not found")
	}
	if f.existing == nil {
		return nil, domain.NewError(domain.CodeNotFound, "get price version by revision: not found")
	}
	return f.existing, nil
}
func (f *fakePVStore) ListPriceVersions(context.Context, PriceVersionFilter) ([]PriceVersionInfo, error) {
	return nil, nil
}
func (f *fakePVStore) Begin(context.Context) (domain.UnitOfWork, error) { return f.uow, nil }
func (f *fakePVStore) MaxPriceVersionRevisionTx(context.Context, domain.UnitOfWork, string, string) (int, error) {
	return f.maxRevision, nil
}
func (f *fakePVStore) InsertPriceVersionTx(_ context.Context, _ domain.UnitOfWork, p *PriceVersionInfo) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	p.ID = "pv-new"
	p.CreatedAt = time.Now().UTC()
	return nil
}

func validCreateInput() CreatePriceVersionInput {
	return CreatePriceVersionInput{
		ModelID: "m-1", Kind: "sale_credit", Revision: 1,
		EffectiveFrom: time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC),
		Reason:        "上架定价",
	}
}

// TestPriceVersionCreate_RaceConflictFixedMessage: 预检未命中 → 插入撞唯一
// 键（并发赢家）→ 兜底重读也失败时，返回固定文案的 409，不得把含 PG 约束
// 名的原始错误抛给调用方（评审轮1 finding）。
func TestPriceVersionCreate_RaceConflictFixedMessage(t *testing.T) {
	uow := &fakeUOW{}
	fs := &fakePVStore{
		failFirstLookup: true,
		failReread:      true,
		insertErr: domain.WrapError(domain.CodeConflict,
			"insert price version: duplicate key (inference_price_versions_model_id_kind_revision_key)",
			errors.New("pq: duplicate key value violates unique constraint")),
		uow: uow,
	}
	svc := NewPriceVersionService(fs, nil, nil)
	_, err := svc.Create(context.Background(), "user:u@app:a", validCreateInput())
	if err == nil {
		t.Fatal("want conflict error")
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %v, want conflict", domain.CodeOf(err))
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want domain error", err)
	}
	if de.Message != "price version conflict for (model_id, kind, revision)" {
		t.Fatalf("message = %q, want fixed wording", de.Message)
	}
	if strings.Contains(err.Error(), "inference_price_versions") || strings.Contains(err.Error(), "pq:") {
		t.Fatalf("message leaks PG internals: %q", err.Error())
	}
	var exists *PriceVersionExistsError
	if errors.As(err, &exists) {
		t.Fatal("fixed fallback must not masquerade as an existing-view 409")
	}
	if !uow.rolledBack {
		t.Fatal("failed insert must roll back the transaction")
	}
}

// TestPriceVersionCreate_RaceConflictReturnsExistingView: 竞态兜底重读命中
// 赢家行 → 409 + 已存在视图（与预检路径同规则）。
func TestPriceVersionCreate_RaceConflictReturnsExistingView(t *testing.T) {
	uow := &fakeUOW{}
	existing := &PriceVersionInfo{
		ID: "pv-winner", ModelID: "m-1", Kind: "sale_credit", Unit: "microcredit",
		Revision:      1,
		EffectiveFrom: time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC),
		ExtraRates:    json.RawMessage(`{"schema_version":1}`),
	}
	fs := &fakePVStore{
		existing:        existing,
		failFirstLookup: true, // 预检未命中
		insertErr:       domain.NewError(domain.CodeConflict, "insert price version: duplicate key"),
		uow:             uow,
	}
	svc := NewPriceVersionService(fs, nil, nil)
	_, err := svc.Create(context.Background(), "user:u@app:a", validCreateInput())
	var exists *PriceVersionExistsError
	if !errors.As(err, &exists) || exists.Existing == nil || exists.Existing.ID != "pv-winner" {
		t.Fatalf("err = %v, want PriceVersionExistsError with winner view", err)
	}
	if !exists.Identical {
		t.Fatal("winner row has identical content — must classify as duplicate replay")
	}
}

// TestSamePriceVersionContent_MicrosecondPrecision: PG timestamptz 把分数秒
// 舍入（rint，四舍六入五成双/half-to-even）到微秒——存储行 = rint(候选,
// 1µs)。同内容重放（候选带亚微秒尾数，含恰好半微秒的 tie）必须判
// duplicate；超过舍入边界的真实差异仍是 conflict；EffectiveTo nil 态不
// 一致也是 conflict。
func TestSamePriceVersionContent_MicrosecondPrecision(t *testing.T) {
	// stored 是 PG 实存值：'...00.123456789' 入库后舍入为 .123457。
	stored := time.Date(2026, 9, 25, 8, 0, 0, 123457000, time.UTC)
	base := func() *PriceVersionInfo {
		return &PriceVersionInfo{
			ModelID: "m-1", Kind: "sale_credit", Unit: "microcredit",
			Revision: 1, EffectiveFrom: stored,
			ExtraRates: json.RawMessage(`{"schema_version":1}`),
		}
	}

	// 同内容重放：候选带纳秒尾数（入库时被舍入到 stored 的值）→ duplicate。
	candidate := base()
	candidate.EffectiveFrom = time.Date(2026, 9, 25, 8, 0, 0, 123456789, time.UTC)
	if !samePriceVersionContent(base(), candidate) {
		t.Fatal("sub-microsecond tail must not flip duplicate to conflict")
	}

	// 恰好半微秒的 tie：PG rint 舍到偶数微秒（'.1234565' → .123456，实测），
	// Go time.Round 是 half-away-from-zero（→ .123457）——必须与 PG 一致
	// （评审轮2 finding）。
	tie := base()
	tie.EffectiveFrom = time.Date(2026, 9, 25, 8, 0, 0, 123456500, time.UTC)
	storedTie := base()
	storedTie.EffectiveFrom = time.Date(2026, 9, 25, 8, 0, 0, 123456000, time.UTC)
	if !samePriceVersionContent(storedTie, tie) {
		t.Fatal("exact half-µs tie must round half-to-even like PG (rint)")
	}
	// 奇数微秒上的 tie 向上舍（'.1234575' → .123458）。
	tieOdd := base()
	tieOdd.EffectiveFrom = time.Date(2026, 9, 25, 8, 0, 0, 123457500, time.UTC)
	storedTieOdd := base()
	storedTieOdd.EffectiveFrom = time.Date(2026, 9, 25, 8, 0, 0, 123458000, time.UTC)
	if !samePriceVersionContent(storedTieOdd, tieOdd) {
		t.Fatal("half-µs tie on odd µs must round up (rint to even)")
	}

	// effective_to 同理（两侧都带，纳秒尾数不同 → duplicate）。
	storedTo := time.Date(2026, 10, 25, 8, 0, 0, 987654000, time.UTC)
	candidateTo := time.Date(2026, 10, 25, 8, 0, 0, 987654321, time.UTC)
	a, b := base(), base()
	a.EffectiveTo, b.EffectiveTo = &storedTo, &candidateTo
	if !samePriceVersionContent(a, b) {
		t.Fatal("effective_to sub-microsecond tail must not flip duplicate to conflict")
	}

	// 真实差异（超出舍入边界）→ conflict。
	c := base()
	c.EffectiveFrom = stored.Add(2 * time.Microsecond)
	if samePriceVersionContent(base(), c) {
		t.Fatal("real >1µs difference must stay conflict")
	}

	// effective_to 一侧 nil 一侧非 nil → conflict。
	d := base()
	d.EffectiveTo = &storedTo
	if samePriceVersionContent(base(), d) || samePriceVersionContent(d, base()) {
		t.Fatal("effective_to nil mismatch must stay conflict")
	}
}

// TestPriceVersionAuditDetail_SanitizesExtraRates: extra_rates 解码成 map
// 后入 detail，SanitizeDetail 才能递归剔除嵌套敏感键（不透明 RawMessage
// 会绕过脱敏）。
func TestPriceVersionAuditDetail_SanitizesExtraRates(t *testing.T) {
	p := &PriceVersionInfo{
		ModelID: "m-1", Kind: "sale_credit", Unit: "microcredit", Revision: 1,
		EffectiveFrom: time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC),
		ExtraRates:    json.RawMessage(`{"schema_version":1,"rates":{"api_key_price":5000,"web_search":100}}`),
	}
	d := SanitizeDetail(priceVersionAuditDetail(p))
	extra, ok := d["extra_rates"].(map[string]any)
	if !ok {
		t.Fatalf("extra_rates must be decoded to a map for recursive sanitize, got %T", d["extra_rates"])
	}
	rates, ok := extra["rates"].(map[string]any)
	if !ok {
		t.Fatalf("nested rates missing: %v", extra)
	}
	if _, leaked := rates["api_key_price"]; leaked {
		t.Fatalf("sensitive key survived sanitize: %v", rates)
	}
	if rates["web_search"] != float64(100) {
		t.Fatalf("benign rate dropped: %v", rates)
	}

	// 不可解码的 extra_rates 保底原样透传（不脱敏谎报）。
	p.ExtraRates = json.RawMessage(`{broken`)
	d = SanitizeDetail(priceVersionAuditDetail(p))
	if _, ok := d["extra_rates"].(json.RawMessage); !ok {
		t.Fatalf("unmarshal failure must keep the raw payload, got %T", d["extra_rates"])
	}
}

// TestPriceVersionCreate_RevisionOutOfRange: revision 列是 INT（int4）——
// 超上限必须在服务层 400，不得落库时报 22003 → 500（评审轮2 finding）。
func TestPriceVersionCreate_RevisionOutOfRange(t *testing.T) {
	fs := &fakePVStore{uow: &fakeUOW{}}
	svc := NewPriceVersionService(fs, nil, nil)
	in := validCreateInput()
	in.Revision = math.MaxInt32 + 1
	_, err := svc.Create(context.Background(), "user:u@app:a", in)
	if err == nil {
		t.Fatal("want invalid input error")
	}
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("code = %v, want invalid_input", domain.CodeOf(err))
	}
	var de *domain.Error
	if errors.As(err, &de) && de.Message != "revision out of range" {
		t.Fatalf("message = %q", de.Message)
	}
	if fs.lookupCalls != 0 {
		t.Fatal("out-of-range revision must be rejected before touching the store")
	}
}

// TestPriceVersionCreate_RevisionMonotonic: M-5 —— revision 必须 = 该
// (model_id, kind) 当前最大 revision + 1（创建事务内检查）；跳号 400 且
// 事务回滚，max+1 正常落库。
func TestPriceVersionCreate_RevisionMonotonic(t *testing.T) {
	uow := &fakeUOW{}
	fs := &fakePVStore{failFirstLookup: true, maxRevision: 0, uow: uow}
	svc := NewPriceVersionService(fs, nil, nil)

	// 跳号：首版即 revision 3 → 400。
	in := validCreateInput()
	in.Revision = 3
	_, err := svc.Create(context.Background(), "user:u@app:a", in)
	if err == nil {
		t.Fatal("want invalid input error for gap revision")
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Message != "revision must be max+1 for (model_id, kind)" {
		t.Fatalf("err = %v, want max+1 message", err)
	}
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("code = %v, want invalid_input", domain.CodeOf(err))
	}
	if !uow.rolledBack {
		t.Fatal("rejected create must roll back the transaction")
	}

	// max+1（首版 = 1）→ 落库。
	created, err := svc.Create(context.Background(), "user:u@app:a", validCreateInput())
	if err != nil || created.ID != "pv-new" {
		t.Fatalf("max+1 create = %+v err=%v", created, err)
	}

	// 已有 max=2 时只允许 3；2（不存在的旧号）也 400。
	uow2 := &fakeUOW{}
	fs2 := &fakePVStore{failFirstLookup: true, maxRevision: 2, uow: uow2}
	svc2 := NewPriceVersionService(fs2, nil, nil)
	in2 := validCreateInput()
	in2.Revision = 2
	if _, err := svc2.Create(context.Background(), "user:u@app:a", in2); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("stale-but-free revision: %v, want invalid_input", err)
	}
	in2.Revision = 3
	if _, err := svc2.Create(context.Background(), "user:u@app:a", in2); err != nil {
		t.Fatalf("max+1 after existing max=2: %v", err)
	}
}

// TestPriceVersionCreate_ReplayWinsOverMonotonic: 幂等重放优先于 max+1 闸
// 门——重放最新 revision 的同内容创建仍是 409 duplicate（带已存在视图），
// 不是 400；且根本不进事务。
func TestPriceVersionCreate_ReplayWinsOverMonotonic(t *testing.T) {
	existing := &PriceVersionInfo{
		ID: "pv-3", ModelID: "m-1", Kind: "sale_credit", Unit: "microcredit",
		Revision:      3,
		EffectiveFrom: time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC),
		ExtraRates:    json.RawMessage(`{"schema_version":1}`),
	}
	fs := &fakePVStore{existing: existing, maxRevision: 3, uow: nil} // uow 为 nil：触到 Begin 即炸
	svc := NewPriceVersionService(fs, nil, nil)
	in := validCreateInput()
	in.Revision = 3
	_, err := svc.Create(context.Background(), "user:u@app:a", in)
	var exists *PriceVersionExistsError
	if !errors.As(err, &exists) || !exists.Identical || exists.Existing == nil || exists.Existing.ID != "pv-3" {
		t.Fatalf("err = %v, want duplicate replay with existing view", err)
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %v, want conflict (409)", domain.CodeOf(err))
	}
}

// TestPriceVersionCreate_ReplayDefaultedEffectiveFromIsConflict: 钉住 spec
// §6 语义——缺省 effective_from 每次重放都取服务时钟新值，与已存行的生效
// 区间不同 ⇒ 同 revision 判 conflict（Identical=false），不是 duplicate。
func TestPriceVersionCreate_ReplayDefaultedEffectiveFromIsConflict(t *testing.T) {
	first := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	existing := &PriceVersionInfo{
		ID: "pv-1", ModelID: "m-1", Kind: "sale_credit", Unit: "microcredit",
		Revision:      1,
		EffectiveFrom: first, // 首次创建时 clock 给的时刻
		ExtraRates:    json.RawMessage(`{"schema_version":1}`),
	}
	fs := &fakePVStore{existing: existing, maxRevision: 1}
	// 重放时刻已晚一分钟：缺省 effective_from = 新的 now。
	clock := &fakeClock{now: first.Add(time.Minute)}
	svc := NewPriceVersionService(fs, nil, clock)
	in := validCreateInput()
	in.EffectiveFrom = time.Time{} // 缺省 → 服务时钟
	_, err := svc.Create(context.Background(), "user:u@app:a", in)
	var exists *PriceVersionExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("err = %v, want PriceVersionExistsError", err)
	}
	if exists.Identical {
		t.Fatal("defaulted effective_from differs per replay — must be conflict, not duplicate")
	}
}

// TestJSONDeepEqual_UseNumber: 大整数 extra_rates 不得经 float64 坍缩——
// 超过 2^53 的价目在 float64 下会相等（误判 duplicate），UseNumber 保住
// 精度（评审轮2 finding）。
func TestJSONDeepEqual_UseNumber(t *testing.T) {
	a := json.RawMessage(`{"schema_version":1,"rates":{"x":9007199254740993}}`)
	b := json.RawMessage(`{"schema_version":1,"rates":{"x":9007199254740992}}`)
	if jsonDeepEqual(a, b) {
		t.Fatal("2^53+1 vs 2^53 must differ (float64 would collapse them)")
	}
	if !jsonDeepEqual(a, a) {
		t.Fatal("identical documents must compare equal")
	}
	// key 序不敏感；数字与字符串仍严格区分。
	if !jsonDeepEqual(json.RawMessage(`{"b":1,"a":2}`), json.RawMessage(`{"a":2,"b":1}`)) {
		t.Fatal("key order must be insensitive")
	}
	if jsonDeepEqual(json.RawMessage(`{"x":1}`), json.RawMessage(`{"x":"1"}`)) {
		t.Fatal("number vs string must differ")
	}
}
