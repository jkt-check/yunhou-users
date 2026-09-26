// quota_policies_test.go — 配额策略服务的编排单测(fake store,无真实
// 库):create/patch/publish/retire 状态机、校验矩阵、幂等不产生第二条
// 审计、retire 引用保护(默认 409 / ?force 放行)、写+审计同事务。
// spec: docs/superpowers/specs/2026-09-26-admin-quota-policies-design.md。
package management

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// --- fake store ---

type fakeQPStore struct {
	rows     map[string]*QuotaPolicyInfo // by id
	models   map[string]bool
	refCount int
	maxRev   int
	uow      *fakeUOW
	inserted *QuotaPolicyInfo
	updated  *QuotaPolicyInfo
	publish  struct {
		calls int
		id    string
		name  string
	}
	retire struct {
		calls int
		id    string
	}
}

func newFakeQPStore() *fakeQPStore {
	return &fakeQPStore{
		rows:   map[string]*QuotaPolicyInfo{},
		models: map[string]bool{"deepseek-chat": true},
		uow:    &fakeUOW{},
	}
}

func (f *fakeQPStore) GetModel(_ context.Context, id string) (*domain.Model, error) {
	if !f.models[id] {
		return nil, domain.NewError(domain.CodeNotFound, "model not found")
	}
	return &domain.Model{ID: id, DisplayName: id, ContextTokens: 1, MaxOutputTokens: 1}, nil
}

func (f *fakeQPStore) GetQuotaPolicyVersion(_ context.Context, id string) (*QuotaPolicyInfo, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "policy not found")
	}
	return r, nil
}

func (f *fakeQPStore) ListQuotaPolicies(_ context.Context, filter QuotaPolicyFilter) ([]QuotaPolicyInfo, error) {
	return nil, nil
}

func (f *fakeQPStore) CountActiveEntitlementsByPolicy(_ context.Context, id string) (int, error) {
	return f.refCount, nil
}

func (f *fakeQPStore) Begin(context.Context) (domain.UnitOfWork, error) { return f.uow, nil }

func (f *fakeQPStore) MaxPolicyRevisionTx(context.Context, domain.UnitOfWork, string) (int, error) {
	return f.maxRev, nil
}

func (f *fakeQPStore) InsertQuotaPolicyTx(_ context.Context, _ domain.UnitOfWork, p *QuotaPolicyInfo) error {
	p.ID = "pol-new"
	f.inserted = p
	f.rows[p.ID] = p
	return nil
}

func (f *fakeQPStore) UpdateDraftQuotaPolicyTx(_ context.Context, _ domain.UnitOfWork, p *QuotaPolicyInfo) error {
	f.updated = p
	f.rows[p.ID] = p
	return nil
}

func (f *fakeQPStore) PublishQuotaPolicyTx(_ context.Context, _ domain.UnitOfWork, id, name string, at time.Time) error {
	f.publish.calls++
	f.publish.id, f.publish.name = id, name
	if r, ok := f.rows[id]; ok {
		r.Status = "published"
	}
	return nil
}

func (f *fakeQPStore) RetireQuotaPolicyTx(_ context.Context, _ domain.UnitOfWork, id string) error {
	f.retire.calls++
	f.retire.id = id
	if r, ok := f.rows[id]; ok {
		r.Status = "retired"
	}
	return nil
}

// --- fake audit ---

type fakeQPAudit struct {
	events []AuditEvent
}

func (a *fakeQPAudit) Record(context.Context, AuditEvent) error {
	return errors.New("non-tx record must not be used")
}
func (a *fakeQPAudit) RecordTx(_ context.Context, _ domain.UnitOfWork, ev AuditEvent) error {
	a.events = append(a.events, ev)
	return nil
}

func newQPService() (*QuotaPolicyService, *fakeQPStore, *fakeQPAudit) {
	fs := newFakeQPStore()
	audit := &fakeQPAudit{}
	return NewQuotaPolicyService(fs, audit, nil), fs, audit
}

func i64(v int64) *int64 { return &v }
func ip(v int) *int      { return &v }

func baseCreateInput() CreateQuotaPolicyInput {
	return CreateQuotaPolicyInput{
		Name: "kaya-gift", ModelIDs: []string{"deepseek-chat"},
		WeeklyLimit: i64(5_000_000), Reason: "出策略",
	}
}

// create 校验矩阵:name 格式 / model 存在性 / limit 至少一项 / limit >0 /
// overage 枚举 / reason 必填。
func TestQuotaPolicyCreate_Validation(t *testing.T) {
	svc, _, _ := newQPService()
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(*CreateQuotaPolicyInput)
	}{
		{"name empty", func(in *CreateQuotaPolicyInput) { in.Name = "" }},
		{"name bad char", func(in *CreateQuotaPolicyInput) { in.Name = "Kaya" }},
		{"name leading dash", func(in *CreateQuotaPolicyInput) { in.Name = "-x" }},
		{"model empty", func(in *CreateQuotaPolicyInput) { in.ModelIDs = nil }},
		{"model unknown", func(in *CreateQuotaPolicyInput) { in.ModelIDs = []string{"ghost"} }},
		{"no limits", func(in *CreateQuotaPolicyInput) { in.WeeklyLimit = nil }},
		{"limit zero", func(in *CreateQuotaPolicyInput) { z := int64(0); in.WeeklyLimit = &z }},
		{"limit negative", func(in *CreateQuotaPolicyInput) { n := int64(-1); in.WeeklyLimit = &n }},
		{"rpm negative", func(in *CreateQuotaPolicyInput) { in.RPMLimit = ip(-5) }},
		{"overage unknown", func(in *CreateQuotaPolicyInput) { in.OveragePolicy = "yolo" }},
		{"reason empty", func(in *CreateQuotaPolicyInput) { in.Reason = "" }},
	}
	for _, c := range cases {
		in := baseCreateInput()
		c.mut(&in)
		if _, err := svc.Create(ctx, "user:op@app:ops", in); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

// create 成功路径:revision = max+1,draft,overage 默认 reject,审计同事务。
func TestQuotaPolicyCreate_HappyPath(t *testing.T) {
	svc, fs, audit := newQPService()
	fs.maxRev = 1
	got, err := svc.Create(context.Background(), "user:op@app:ops", baseCreateInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Revision != 2 || got.Status != "draft" || got.OveragePolicy != "reject" {
		t.Fatalf("got revision=%d status=%s overage=%s", got.Revision, got.Status, got.OveragePolicy)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "quota_policy.create" {
		t.Fatalf("audit = %+v", audit.events)
	}
	if audit.events[0].ActorUser != "op" || audit.events[0].ActorApp != "ops" {
		t.Fatalf("actor = %s/%s", audit.events[0].ActorUser, audit.events[0].ActorApp)
	}
	if fs.uow.rolledBack {
		t.Fatal("uow rolled back on success")
	}
}

// patch:仅 draft 可改;空修改 400;presence 语义(缺席保留/显式 null 清空);
// 合并后再校验(清成零限制 → 400)。
func TestQuotaPolicyUpdate_StateMachine(t *testing.T) {
	svc, fs, audit := newQPService()
	draft := &QuotaPolicyInfo{ID: "p1", Name: "n", Revision: 1, Status: "draft",
		ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(5_000_000), ConcurrencyLimit: ip(4),
		OveragePolicy: "reject"}
	fs.rows["p1"] = draft

	// published 不可改。
	pub := &QuotaPolicyInfo{ID: "p2", Name: "n", Revision: 1, Status: "published", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	fs.rows["p2"] = pub
	if _, err := svc.Update(context.Background(), "user:op@app:ops", "p2",
		UpdateQuotaPolicyInput{Reason: "x", Set: map[string]bool{"rpm_limit": true}, RPMLimit: ip(9)}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("patch published: code = %v, want conflict", domain.CodeOf(err))
	}
	// 空修改 → 400。
	if _, err := svc.Update(context.Background(), "user:op@app:ops", "p1",
		UpdateQuotaPolicyInput{Reason: "x"}); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("empty patch: code = %v, want invalid_input", domain.CodeOf(err))
	}
	// 显式 null 清空 concurrency + 改 rpm;缺席的 weekly 保留。
	got, err := svc.Update(context.Background(), "user:op@app:ops", "p1",
		UpdateQuotaPolicyInput{Reason: "改", Set: map[string]bool{"rpm_limit": true, "concurrency_limit": true}, RPMLimit: ip(120)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.RPMLimit == nil || *got.RPMLimit != 120 {
		t.Fatalf("rpm = %v", got.RPMLimit)
	}
	if got.ConcurrencyLimit != nil {
		t.Fatalf("concurrency = %v, want nil after explicit clear", got.ConcurrencyLimit)
	}
	if got.WeeklyLimit == nil || *got.WeeklyLimit != 5_000_000 {
		t.Fatalf("weekly = %v, want preserved", got.WeeklyLimit)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "quota_policy.update" {
		t.Fatalf("audit = %+v", audit.events)
	}
	// 清成零限制 → 400,不落库(上一轮 update 后剩 weekly+rpm 两项,全清)。
	_, err = svc.Update(context.Background(), "user:op@app:ops", "p1",
		UpdateQuotaPolicyInput{Reason: "x", Set: map[string]bool{"weekly_limit_micros": true, "rpm_limit": true}})
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("clear all limits: code = %v, want invalid_input", domain.CodeOf(err))
	}
}

// publish:draft → published(审计);重复 → 幂等返回当前状态、零新增审计;
// superseded/retired → 409。
func TestQuotaPolicyPublish_Idempotent(t *testing.T) {
	svc, fs, audit := newQPService()
	fs.rows["p1"] = &QuotaPolicyInfo{ID: "p1", Name: "n", Revision: 1, Status: "draft", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	fs.rows["p2"] = &QuotaPolicyInfo{ID: "p2", Name: "n", Revision: 2, Status: "published", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	fs.rows["p3"] = &QuotaPolicyInfo{ID: "p3", Name: "n", Revision: 3, Status: "superseded", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	ctx := context.Background()

	if _, err := svc.Publish(ctx, "user:op@app:ops", "p1", "发布"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if fs.publish.calls != 1 || fs.publish.id != "p1" || fs.publish.name != "n" {
		t.Fatalf("publish tx = %+v, want one call for p1/n", fs.publish)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "quota_policy.publish" {
		t.Fatalf("audit = %+v", audit.events)
	}

	// 幂等重放(published 状态)→ 200,无新增审计、无第二次 tx 调用。
	got, err := svc.Publish(ctx, "user:op@app:ops", "p2", "重放")
	if err != nil || got.Status != "published" {
		t.Fatalf("idempotent publish: got=%+v err=%v", got, err)
	}
	if fs.publish.calls != 1 || len(audit.events) != 1 {
		t.Fatalf("replay must not re-publish/re-audit: calls=%d audits=%d", fs.publish.calls, len(audit.events))
	}

	// superseded → 409。
	if _, err := svc.Publish(ctx, "user:op@app:ops", "p3", "复活"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("publish superseded: code = %v, want conflict", domain.CodeOf(err))
	}
}

// retire:draft 直接;retired 幂等;published/superseded 默认 409(带
// referenced_by),force 放行;被拒的 409 不落审计。
func TestQuotaPolicyRetire_Guard(t *testing.T) {
	svc, fs, audit := newQPService()
	fs.rows["draft"] = &QuotaPolicyInfo{ID: "draft", Name: "n", Revision: 1, Status: "draft", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	fs.rows["pub"] = &QuotaPolicyInfo{ID: "pub", Name: "n", Revision: 1, Status: "published", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	fs.rows["ret"] = &QuotaPolicyInfo{ID: "ret", Name: "n", Revision: 2, Status: "retired", ModelIDs: []string{"deepseek-chat"}, WeeklyLimit: i64(1)}
	ctx := context.Background()

	// draft → retired 直接(无需 force,不查引用)。
	if _, err := svc.Retire(ctx, "user:op@app:ops", "draft", "废弃", false); err != nil {
		t.Fatalf("retire draft: %v", err)
	}
	// retired → 幂等。
	if _, err := svc.Retire(ctx, "user:op@app:ops", "ret", "重放", false); err != nil {
		t.Fatalf("retire replay: %v", err)
	}

	// published + 有引用 + 默认 → 409 带 referenced_by。
	fs.refCount = 3
	_, err := svc.Retire(ctx, "user:op@app:ops", "pub", "退役", false)
	var refErr *QuotaPolicyRetireConflictError
	if !errors.As(err, &refErr) || refErr.ReferencedBy != 3 {
		t.Fatalf("retire referenced: err = %v, want typed conflict with referenced_by=3", err)
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %v, want conflict", domain.CodeOf(err))
	}
	// force → 放行。
	if _, err := svc.Retire(ctx, "user:op@app:ops", "pub", "强制", true); err != nil {
		t.Fatalf("forced retire: %v", err)
	}

	// 审计:draft 1 + forced 1 = 2(被拒与幂等重放不落)。
	if len(audit.events) != 2 {
		t.Fatalf("audit events = %d, want 2 (draft retire + forced retire only)", len(audit.events))
	}
	if audit.events[0].Action != "quota_policy.retire" || audit.events[1].Action != "quota_policy.retire" {
		t.Fatalf("audit = %+v", audit.events)
	}
}
