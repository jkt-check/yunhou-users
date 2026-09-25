package management

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// price_versions_test.go — 售价版本服务的纯编排单测（fake store，无真实
// 库）：µs 截断的内容比较、唯一键竞态兜底的固定 409 文案（不泄露 PG
// 约束名）、审计 detail 中 extra_rates 的可递归脱敏。

// --- fake store / uow ---

type fakeUOW struct{ rolledBack bool }

func (u *fakeUOW) Commit(context.Context) error   { return nil }
func (u *fakeUOW) Rollback(context.Context) error { u.rolledBack = true; return nil }

type fakePVStore struct {
	existing  *PriceVersionInfo
	insertErr error
	// failFirstLookup 让幂等预检未命中（走插入路径）；failReread 让竞态
	// 兜底的二次重读失败。
	failFirstLookup bool
	failReread      bool
	lookupCalls     int
	uow             *fakeUOW
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
// 四舍五入到微秒——存储行 = round(候选, 1µs)。同内容重放（候选带亚微秒尾
// 数）必须判 duplicate；超过舍入边界的真实差异仍是 conflict；EffectiveTo
// nil 态不一致也是 conflict。
func TestSamePriceVersionContent_MicrosecondPrecision(t *testing.T) {
	// stored 是 PG 实存值：'...00.123456789' 入库后四舍五入为 .123457。
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
