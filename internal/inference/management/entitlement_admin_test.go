package management

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// entitlement_admin_test.go — EntitlementAdminService 单测（fake store +
// fake audit）：R1 幂等短路/集合运算/审计归因/回滚，R2 dry_run 纯读、行级
// 事务、conflict 与行错误分流、skipped 明细（AC3/AC4/AC5/AC6 的服务层语义）。

// fakeEntUoW 把修订暂存为 pending，Commit 才应用 —— 与 PG 的 rollback 语义
// 对齐（审计失败回滚后库里必须无半更新）。
type fakeEntUoW struct {
	s       *fakeEntStore
	pending func()
}

func (u *fakeEntUoW) Commit(_ context.Context) error {
	u.s.commits++
	if u.pending != nil {
		u.pending()
	}
	return nil
}
func (u *fakeEntUoW) Rollback(_ context.Context) error { u.s.rollbacks++; return nil }

type fakeEntStore struct {
	ents      map[string]domain.Entitlement
	amendRows []domain.Entitlement
	begins    int
	commits   int
	rollbacks int
	revises   int
	reviseErr map[string]error // id → 强制错误（模拟乐观锁冲突/行故障）
}

func newFakeEntStore(ents ...domain.Entitlement) *fakeEntStore {
	s := &fakeEntStore{ents: map[string]domain.Entitlement{}, reviseErr: map[string]error{}}
	for _, e := range ents {
		s.ents[e.ID] = e
	}
	return s
}

func (s *fakeEntStore) Begin(context.Context) (domain.UnitOfWork, error) {
	s.begins++
	return &fakeEntUoW{s: s}, nil
}

func (s *fakeEntStore) GetEntitlement(_ context.Context, id string) (*domain.Entitlement, error) {
	e, ok := s.ents[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "get entitlement: not found")
	}
	cp := e
	return &cp, nil
}

func (s *fakeEntStore) GetEntitlementTx(_ context.Context, _ domain.UnitOfWork, id string) (*domain.Entitlement, error) {
	return s.GetEntitlement(context.Background(), id)
}

func (s *fakeEntStore) ReviseEntitlementTx(_ context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error) {
	s.revises++
	if err := s.reviseErr[id]; err != nil {
		return nil, err
	}
	cur, ok := s.ents[id]
	if !ok || cur.Revision != patch.ExpectedRevision || cur.Status != domain.EntitlementActive {
		return nil, domain.NewError(domain.CodeConflict, "revise entitlement: stale revision or entitlement not active")
	}
	revised := cur
	revised.Revision++
	if patch.PolicyVersionID != nil {
		revised.PolicyVersionID = *patch.PolicyVersionID
	}
	if patch.ModelIDs != nil {
		revised.ModelIDs = patch.ModelIDs
	}
	w.(*fakeEntUoW).pending = func() { s.ents[id] = revised }
	cp := revised
	return &cp, nil
}

func (s *fakeEntStore) ListEntitlementsAdmin(_ context.Context, f EntitlementListFilter) ([]domain.Entitlement, error) {
	var out []domain.Entitlement
	for _, e := range s.ents {
		if f.BillingAccountID != "" && e.BillingAccountID != f.BillingAccountID {
			continue
		}
		if f.Status != "" && string(e.Status) != f.Status {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *fakeEntStore) ListEntitlementsForAmend(_ context.Context, key, _ string) ([]domain.Entitlement, error) {
	if key != SelectorAllActive && key != SelectorSourcePlan {
		return nil, domain.NewError(domain.CodeInvalidInput, "unknown amend selector: "+key)
	}
	// 回当前状态副本（amendRows 只是候选名单；重跑场景要看到上一次
	// commit 后的最新集合，与真实库一致）。
	out := make([]domain.Entitlement, 0, len(s.amendRows))
	for _, e := range s.amendRows {
		out = append(out, s.ents[e.ID])
	}
	return out, nil
}

type fakeAudit struct {
	events []AuditEvent
	txErr  error
}

func (a *fakeAudit) Record(_ context.Context, ev AuditEvent) error {
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeAudit) RecordTx(_ context.Context, _ domain.UnitOfWork, ev AuditEvent) error {
	if a.txErr != nil {
		return a.txErr
	}
	a.events = append(a.events, ev)
	return nil
}

// plainAudit 只有 Record，没有 RecordTx —— 缺 tx 能力必须 fail-closed。
type plainAudit struct{}

func (plainAudit) Record(context.Context, AuditEvent) error { return nil }

var testOp = EntitlementOperator{UserID: "op-1", AppID: "ops-console"}

func activeEnt(id, account string, models ...string) domain.Entitlement {
	return domain.Entitlement{
		ID: id, BillingAccountID: account, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + id, ModelIDs: models, PolicyVersionID: "pv-1",
		Revision: 3, Status: domain.EntitlementActive,
	}
}

func TestRevise_IdempotentNoChange(t *testing.T) {
	st := newFakeEntStore(activeEnt("e1", "acct-1", "m-a", "m-b"))
	au := &fakeAudit{}
	svc := NewEntitlementAdminService(st, au)

	res, err := svc.Revise(context.Background(), testOp, "e1", ReviseInput{
		AddModelIDs: []string{"m-b", " m-b "}, Reason: "dup add",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("already-present add must be Changed=false (幂等短路)")
	}
	if res.Entitlement.Revision != 3 {
		t.Errorf("revision bumped on no-op: %d", res.Entitlement.Revision)
	}
	if st.revises != 0 || st.commits != 0 || len(au.events) != 0 {
		t.Errorf("no-op must not write/audit: revises=%d commits=%d audits=%d", st.revises, st.commits, len(au.events))
	}
	if st.rollbacks != 1 {
		t.Errorf("short-circuit tx must roll back: rollbacks=%d", st.rollbacks)
	}
}

func TestRevise_AddRemoveAndAudit(t *testing.T) {
	st := newFakeEntStore(activeEnt("e1", "acct-1", "m-a", "m-b"))
	au := &fakeAudit{}
	svc := NewEntitlementAdminService(st, au)

	res, err := svc.Revise(context.Background(), testOp, "e1", ReviseInput{
		AddModelIDs: []string{"m-c"}, RemoveModelIDs: []string{"m-a"}, Reason: " 发布 m-c ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("want Changed=true")
	}
	if got := res.Entitlement.ModelIDs; !reflect.DeepEqual(got, []string{"m-b", "m-c"}) {
		t.Errorf("model set = %v, want [m-b m-c]（保序：既有优先、add 追加）", got)
	}
	if res.Entitlement.Revision != 4 {
		t.Errorf("revision = %d, want 4", res.Entitlement.Revision)
	}
	if st.commits != 1 {
		t.Fatalf("commits = %d, want 1", st.commits)
	}
	// 库里已应用（fake 在 Commit 时应用 pending）。
	if got := st.ents["e1"].ModelIDs; !reflect.DeepEqual(got, []string{"m-b", "m-c"}) {
		t.Errorf("stored set = %v", got)
	}
	if len(au.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(au.events))
	}
	ev := au.events[0]
	if ev.Action != "entitlement.revise" || ev.ObjectType != "entitlement" || ev.ObjectID != "e1" {
		t.Errorf("audit identity: %+v", ev)
	}
	if ev.Reason != "发布 m-c" || ev.ActorUser != "op-1" || ev.ActorApp != "ops-console" {
		t.Errorf("audit attribution: %+v", ev)
	}
	d := ev.Detail
	if !reflect.DeepEqual(d["before_model_ids"], []string{"m-a", "m-b"}) ||
		!reflect.DeepEqual(d["after_model_ids"], []string{"m-b", "m-c"}) {
		t.Errorf("audit model detail: %v → %v", d["before_model_ids"], d["after_model_ids"])
	}
	if d["before_policy_version_id"] != "pv-1" || d["after_policy_version_id"] != "pv-1" ||
		d["before_revision"] != 3 || d["after_revision"] != 4 {
		t.Errorf("audit policy/revision detail: %+v", d)
	}
}

func TestRevise_Validation(t *testing.T) {
	st := newFakeEntStore(activeEnt("e1", "acct-1", "m-a"))
	svc := NewEntitlementAdminService(st, &fakeAudit{})
	ctx := context.Background()

	cases := []struct {
		name string
		in   ReviseInput
	}{
		{"空 reason", ReviseInput{AddModelIDs: []string{"m-b"}, Reason: "  "}},
		{"add/remove 全空", ReviseInput{Reason: "r"}},
		{"add/remove 交集", ReviseInput{AddModelIDs: []string{"m-x"}, RemoveModelIDs: []string{"m-x"}, Reason: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Revise(ctx, testOp, "e1", tc.in); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Fatalf("err = %v, want CodeInvalidInput", err)
			}
			if st.begins != 0 {
				t.Errorf("validation failure must not open a tx: begins=%d", st.begins)
			}
		})
	}
}

func TestRevise_NotActiveAndStale(t *testing.T) {
	ctx := context.Background()

	// 非 active：集合有变化才会走到 Upgrade 的 active 守卫 → CodeConflict。
	revoked := activeEnt("e1", "acct-1", "m-a")
	revoked.Status = domain.EntitlementRevoked
	st := newFakeEntStore(revoked)
	svc := NewEntitlementAdminService(st, &fakeAudit{})
	if _, err := svc.Revise(ctx, testOp, "e1", ReviseInput{AddModelIDs: []string{"m-b"}, Reason: "r"}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("revoked: err = %v, want CodeConflict", err)
	}

	// 乐观锁 stale：repo 层 CodeConflict 透传，回滚无审计。
	st2 := newFakeEntStore(activeEnt("e1", "acct-1", "m-a"))
	st2.reviseErr["e1"] = domain.NewError(domain.CodeConflict, "revise entitlement: stale revision or entitlement not active")
	au2 := &fakeAudit{}
	svc2 := NewEntitlementAdminService(st2, au2)
	if _, err := svc2.Revise(ctx, testOp, "e1", ReviseInput{AddModelIDs: []string{"m-b"}, Reason: "r"}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale: err = %v, want CodeConflict", err)
	}
	if st2.commits != 0 || len(au2.events) != 0 {
		t.Errorf("conflict must not commit/audit: commits=%d audits=%d", st2.commits, len(au2.events))
	}
}

func TestRevise_AuditFailureRollsBack(t *testing.T) {
	st := newFakeEntStore(activeEnt("e1", "acct-1", "m-a"))
	au := &fakeAudit{txErr: errors.New("audit sink down")}
	svc := NewEntitlementAdminService(st, au)

	_, err := svc.Revise(context.Background(), testOp, "e1", ReviseInput{AddModelIDs: []string{"m-b"}, Reason: "r"})
	if domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("err = %v, want CodeInternal", err)
	}
	if st.commits != 0 {
		t.Errorf("audit failure must roll back: commits=%d", st.commits)
	}
	if got := st.ents["e1"].ModelIDs; !reflect.DeepEqual(got, []string{"m-a"}) {
		t.Errorf("store mutated despite rollback: %v", got)
	}

	// 审计器缺 tx 能力 → fail-closed（不变更）。
	svcNoTx := NewEntitlementAdminService(st, plainAudit{})
	if _, err := svcNoTx.Revise(context.Background(), testOp, "e1", ReviseInput{AddModelIDs: []string{"m-b"}, Reason: "r"}); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("no-tx audit: err = %v, want CodeInternal", err)
	}
}

// amendFixture：三行候选——缺模型的 active（将变更）、已含模型的 active
// （幂等跳过）、revoked（AC6 跳过明细）。
func amendFixture() *fakeEntStore {
	missing := activeEnt("e-missing", "acct-1", "m-a")
	present := activeEnt("e-present", "acct-2", "m-a", "m-new")
	revoked := activeEnt("e-revoked", "acct-3", "m-a")
	revoked.Status = domain.EntitlementRevoked
	st := newFakeEntStore(missing, present, revoked)
	st.amendRows = []domain.Entitlement{missing, present, revoked}
	return st
}

func TestAmend_DryRunIsReadOnly(t *testing.T) {
	st := amendFixture()
	au := &fakeAudit{}
	svc := NewEntitlementAdminService(st, au)

	rep, err := svc.AmendModels(context.Background(), testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "发布 m-new",
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 || rep.Amended != 1 || rep.Skipped != 2 || rep.Conflicts != 0 {
		t.Errorf("counts = %+v, want scanned 3 / amended 1 / skipped 2", rep)
	}
	if st.begins != 0 || st.commits != 0 || len(au.events) != 0 {
		t.Errorf("dry-run must be read-only: begins=%d commits=%d audits=%d", st.begins, st.commits, len(au.events))
	}
	// 抽样含三行，note 分流正确。
	if len(rep.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(rep.Samples))
	}
	notes := map[string]string{}
	for _, sm := range rep.Samples {
		notes[sm.EntitlementID] = sm.Note
	}
	if notes["e-missing"] != "would amend" || notes["e-present"] != "already contains model" ||
		notes["e-revoked"] != "not active: revoked" {
		t.Errorf("sample notes = %v", notes)
	}
	// would-amend 行带 after 集合，跳过行不带。
	for _, sm := range rep.Samples {
		if sm.EntitlementID == "e-missing" && !reflect.DeepEqual(sm.After, []string{"m-a", "m-new"}) {
			t.Errorf("sample after = %v", sm.After)
		}
		if sm.EntitlementID != "e-missing" && sm.After != nil {
			t.Errorf("skipped sample must not carry after: %+v", sm)
		}
	}
	// skipped 明细两行（AC6 核对面）。
	if len(rep.SkippedDetail) != 2 {
		t.Fatalf("skipped_detail = %v", rep.SkippedDetail)
	}
}

func TestAmend_CommitRowLevelTx(t *testing.T) {
	st := amendFixture()
	au := &fakeAudit{}
	svc := NewEntitlementAdminService(st, au)

	rep, err := svc.AmendModels(context.Background(), testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "发布 m-new",
		DryRun: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Amended != 1 || rep.Skipped != 2 || rep.Conflicts != 0 || len(rep.Errors) != 0 {
		t.Errorf("counts = %+v", rep)
	}
	// 行级事务：只有变更行开事务。
	if st.begins != 1 || st.commits != 1 {
		t.Errorf("tx count = begins %d / commits %d, want 1/1", st.begins, st.commits)
	}
	if got := st.ents["e-missing"].ModelIDs; !reflect.DeepEqual(got, []string{"m-a", "m-new"}) {
		t.Errorf("amended row = %v", got)
	}
	// 跳过行未被触碰。
	if got := st.ents["e-revoked"]; got.Status != domain.EntitlementRevoked || len(got.ModelIDs) != 1 {
		t.Errorf("revoked row must be untouched: %+v", got)
	}
	// 审计：每 amended 行一条，action=entitlement.amend，detail 带批次上下文。
	if len(au.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(au.events))
	}
	ev := au.events[0]
	if ev.Action != "entitlement.amend" || ev.ObjectID != "e-missing" || ev.Reason != "发布 m-new" {
		t.Errorf("audit = %+v", ev)
	}
	if ev.Detail["batch_selector"] != "all_active" || ev.Detail["batch_action"] != "add" || ev.Detail["batch_model_id"] != "m-new" {
		t.Errorf("audit batch detail = %+v", ev.Detail)
	}
}

func TestAmend_ConflictAndRowError(t *testing.T) {
	ctx := context.Background()

	// 乐观锁冲突行进 Conflicts + errors[]（reason 前缀 "conflict:"），批次不失败。
	st := amendFixture()
	st.reviseErr["e-missing"] = domain.NewError(domain.CodeConflict, "revise entitlement: stale revision or entitlement not active")
	svc := NewEntitlementAdminService(st, &fakeAudit{})
	rep, err := svc.AmendModels(ctx, testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts != 1 || rep.Amended != 0 || len(rep.Errors) != 1 {
		t.Fatalf("conflict split = %+v", rep)
	}
	if got := rep.Errors[0].Reason; got[:9] != "conflict:" {
		t.Errorf("error reason = %q, want conflict: prefix", got)
	}

	// 其他行错误进 errors[]，不计 Conflicts。
	st2 := amendFixture()
	st2.reviseErr["e-missing"] = errors.New("db gone")
	svc2 := NewEntitlementAdminService(st2, &fakeAudit{})
	rep2, err := svc2.AmendModels(ctx, testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Conflicts != 0 || len(rep2.Errors) != 1 || rep2.Errors[0].Reason[:6] != "error:" {
		t.Fatalf("row error split = %+v", rep2)
	}
}

func TestAmend_RemoveAndIdempotentRerun(t *testing.T) {
	ctx := context.Background()
	st := amendFixture()
	svc := NewEntitlementAdminService(st, &fakeAudit{})

	// remove：已含的行变更，不含的行跳过。
	rep, err := svc.AmendModels(ctx, testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionRemove, Selector: "all_active", Reason: "下架 m-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Amended != 1 || rep.Skipped != 2 {
		t.Fatalf("remove counts = %+v, want amended 1 / skipped 2", rep)
	}
	if got := st.ents["e-present"].ModelIDs; !reflect.DeepEqual(got, []string{"m-a"}) {
		t.Errorf("after remove = %v, want [m-a]", got)
	}

	// AC3：重跑同一增补 → amended=0、skipped 全量、无报错。
	st2 := amendFixture()
	svc2 := NewEntitlementAdminService(st2, &fakeAudit{})
	if _, err := svc2.AmendModels(ctx, testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "r",
	}); err != nil {
		t.Fatal(err)
	}
	rep2, err := svc2.AmendModels(ctx, testOp, AmendRequest{
		ModelID: "m-new", Action: AmendActionAdd, Selector: "all_active", Reason: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Amended != 0 || rep2.Skipped != 3 {
		t.Errorf("rerun = amended %d / skipped %d, want 0/3", rep2.Amended, rep2.Skipped)
	}
}

func TestAmend_Validation(t *testing.T) {
	svc := NewEntitlementAdminService(amendFixture(), &fakeAudit{})
	ctx := context.Background()
	cases := []struct {
		name string
		in   AmendRequest
	}{
		{"空 model_id", AmendRequest{Action: "add", Selector: "all_active", Reason: "r"}},
		{"非法 action", AmendRequest{ModelID: "m", Action: "set", Selector: "all_active", Reason: "r"}},
		{"空 reason", AmendRequest{ModelID: "m", Action: "add", Selector: "all_active"}},
		{"非法 selector", AmendRequest{ModelID: "m", Action: "add", Selector: "everything", Reason: "r"}},
		{"source_plan 缺 plan id", AmendRequest{ModelID: "m", Action: "add", Selector: "source_plan:", Reason: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.AmendModels(ctx, testOp, tc.in); domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Fatalf("err = %v, want CodeInvalidInput", err)
			}
		})
	}
}

func TestAmend_SourcePlanSelectorPassesThrough(t *testing.T) {
	st := amendFixture()
	svc := NewEntitlementAdminService(st, &fakeAudit{})
	// fake 不真正按 plan 过滤，但 selector 解析必须接受该形状并把 val 传给
	// store（repo DB 测试钉真实过滤行为）。
	rep, err := svc.AmendModels(context.Background(), testOp, AmendRequest{
		ModelID: "m-new", Action: "add", Selector: "source_plan:monthly", Reason: "r", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 {
		t.Errorf("scanned = %d, want fake's 3 rows", rep.Scanned)
	}
}
