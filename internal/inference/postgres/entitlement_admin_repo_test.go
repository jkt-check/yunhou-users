package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// entitlement_admin_repo_test.go — 权益 admin 面 repo 方法的真实库行为
// （entitlement-model-amendment 需求 R1–R3 的持久层）：tx 内读、过滤分页
// 列表、批量选择器（含非 active 行对 source_plan 可见，AC6 核对面）。

// seedAmendSubscription 造 plans + subscriptions 行（source_plan 选择器
// 的 join 目标），返回 subscription id。plans 幂等插入。
func seedAmendSubscription(t *testing.T, s *Store, userID, planID, subStatus string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO plans (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		planID, "plan "+planID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	subID := uuid.NewString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions (id, user_id, plan_id, status) VALUES ($1, $2, $3, $4)`,
		subID, userID, planID, subStatus); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	return subID
}

func retireEntitlement(t *testing.T, s *Store, id, to string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE inference_entitlements SET status = $2 WHERE id = $1`, id, to); err != nil {
		t.Fatalf("retire entitlement: %v", err)
	}
}

func TestGetEntitlementTx(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := s.GetEntitlementTx(ctx, uow, f.entID)
	if err != nil {
		t.Fatalf("get in tx: %v", err)
	}
	if ent.ID != f.entID || ent.BillingAccountID != f.accountID {
		t.Errorf("got %+v, want fixture entitlement", ent)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// 不存在 → CodeNotFound（handler 映射 404）。
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(ctx) //nolint:errcheck
	if _, err := s.GetEntitlementTx(ctx, uow, uuid.NewString()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing id: err = %v, want CodeNotFound", err)
	}
}

func TestListEntitlementsAdmin_Filters(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f1 := seedFixtureModel(t, s, false, "m-admin-a")
	f2 := seedFixtureModel(t, s, false, "m-admin-b")
	retireEntitlement(t, s, f2.entID, "revoked")

	// 无过滤：两行都可见（含 revoked）。
	ents, err := s.ListEntitlementsAdmin(ctx, management.EntitlementListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("no filter: %d rows, want 2 (revoked included)", len(ents))
	}

	// billing_account_id 过滤。
	ents, err = s.ListEntitlementsAdmin(ctx, management.EntitlementListFilter{BillingAccountID: f1.accountID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].ID != f1.entID {
		t.Fatalf("account filter: %+v, want only f1", ents)
	}

	// status 过滤。
	ents, err = s.ListEntitlementsAdmin(ctx, management.EntitlementListFilter{Status: "revoked"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].ID != f2.entID {
		t.Fatalf("status filter: %+v, want only the revoked row", ents)
	}

	// 分页：最新优先，limit=1 拿第一页，offset=1 拿第二页，两页不重叠。
	p1, err := s.ListEntitlementsAdmin(ctx, management.EntitlementListFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.ListEntitlementsAdmin(ctx, management.EntitlementListFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 1 || len(p2) != 1 || p1[0].ID == p2[0].ID {
		t.Fatalf("paging: p1=%v p2=%v, want 2 distinct rows", p1, p2)
	}
}

func TestListEntitlementsForAmend(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f1 := seedFixtureModel(t, s, false, "m-amend-a")
	f2 := seedFixtureModel(t, s, false, "m-amend-b")

	// plan-a：一条 active 订阅权益 + 一条 revoked 权益（过期订阅来源）。
	subA1 := seedAmendSubscription(t, s, f1.userID, "plan-amend-a", "active")
	entA1 := seedEntitlement(t, s, f1, domain.SourceSubscription, subA1, []string{f1.modelID},
		time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC), nil)
	subA2 := seedAmendSubscription(t, s, f2.userID, "plan-amend-a", "expired")
	entA2 := seedEntitlement(t, s, f2, domain.SourceSubscription, subA2, []string{f2.modelID},
		time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC), nil)
	retireEntitlement(t, s, entA2.ID, "revoked")

	// plan-b：一条 active 订阅权益。
	subB := seedAmendSubscription(t, s, f2.userID, "plan-amend-b", "active")
	entB := seedEntitlement(t, s, f2, domain.SourceSubscription, subB, []string{f2.modelID},
		time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC), nil)

	contains := func(list []domain.Entitlement, id string) bool {
		for _, e := range list {
			if e.ID == id {
				return true
			}
		}
		return false
	}

	// all_active：只有 active 行（fixture 自带的两条 + entA1 + entB），
	// revoked 的 entA2 不可见。
	ents, err := s.ListEntitlementsForAmend(ctx, management.SelectorAllActive, "")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ents, entA1.ID) || !contains(ents, entB.ID) {
		t.Errorf("all_active missing active rows: %v", ents)
	}
	if contains(ents, entA2.ID) {
		t.Errorf("all_active must not include revoked row %s", entA2.ID)
	}

	// source_plan:plan-amend-a：全部状态可见（AC6：命中的 revoked 行进
	// skipped 明细的前提是它必须被选择器命中）。
	ents, err = s.ListEntitlementsForAmend(ctx, management.SelectorSourcePlan, "plan-amend-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 || !contains(ents, entA1.ID) || !contains(ents, entA2.ID) {
		t.Fatalf("source_plan plan-a: %v, want entA1 + revoked entA2", ents)
	}
	if contains(ents, entB.ID) {
		t.Errorf("source_plan plan-a must not include plan-b row %s", entB.ID)
	}

	// 非法选择器键 → CodeInvalidInput（服务层已校验，repo 防御）。
	if _, err := s.ListEntitlementsForAmend(ctx, "bogus", ""); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("bogus selector: err = %v, want CodeInvalidInput", err)
	}
}

// TestReviseEntitlement_ConcurrentConflict（AC4 实证）：两个事务以同一
// revision 并发修订同一权益——恰一方成功，另一方 CodeConflict，无半更新
// （最终 model_ids 是胜者的完整结果，revision 只 +1）。
func TestReviseEntitlement_ConcurrentConflict(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	ent, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	patchOf := func(models []string) domain.EntitlementPatch {
		return domain.EntitlementPatch{ExpectedRevision: ent.Revision, ModelIDs: models}
	}

	type result struct {
		rev *domain.Entitlement
		err error
	}
	revise := func(models []string) result {
		uow, err := s.Begin(ctx)
		if err != nil {
			return result{err: err}
		}
		defer uow.Rollback(ctx) //nolint:errcheck
		rev, err := s.ReviseEntitlementTx(ctx, uow, f.entID, patchOf(models))
		if err != nil {
			return result{err: err}
		}
		if err := uow.Commit(ctx); err != nil {
			return result{err: err}
		}
		return result{rev: rev}
	}

	// 两个 goroutine 同时以 revision=N 开写：PG 行锁让后到者阻塞至前者
	// 提交，随后 WHERE revision=N 重评估失败 → CodeConflict。
	chA, chB := make(chan result, 1), make(chan result, 1)
	go func() { chA <- revise([]string{f.modelID, "m-winner-a"}) }()
	go func() { chB <- revise([]string{f.modelID, "m-winner-b"}) }()
	ra, rb := <-chA, <-chB

	var wins, conflicts int
	var winner *domain.Entitlement
	for _, r := range []result{ra, rb} {
		switch {
		case r.err == nil:
			wins++
			winner = r.rev
		case domain.CodeOf(r.err) == domain.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly 1/1", wins, conflicts)
	}
	if winner.Revision != ent.Revision+1 {
		t.Errorf("winner revision = %d, want %d", winner.Revision, ent.Revision+1)
	}
	// 最终行是胜者的完整结果（无交错半更新）。
	final, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Revision != winner.Revision || len(final.ModelIDs) != 2 {
		t.Errorf("final row = rev %d models %v, want winner's complete result",
			final.Revision, final.ModelIDs)
	}
}

// TestReviseEntitlement_EmptySetIsExplicitEmpty（红线 1 落库面）：remove 至
// 空集合落库为 '{}'（显式空集 = 无任何模型），不是 NULL。
func TestReviseEntitlement_EmptySetIsExplicitEmpty(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	ent, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.ReviseEntitlement(ctx, f.entID, domain.EntitlementPatch{
		ExpectedRevision: ent.Revision, ModelIDs: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev.ModelIDs == nil || len(rev.ModelIDs) != 0 {
		t.Errorf("revised set = %#v, want explicit empty", rev.ModelIDs)
	}
	var isEmpty, isNull bool
	if err := s.db.Get(&isEmpty,
		`SELECT model_ids = '{}'::text[] FROM inference_entitlements WHERE id = $1`, f.entID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Get(&isNull,
		`SELECT model_ids IS NULL FROM inference_entitlements WHERE id = $1`, f.entID); err != nil {
		t.Fatal(err)
	}
	if !isEmpty || isNull {
		t.Errorf("stored model_ids: empty=%v null=%v, want '{}'非 NULL", isEmpty, isNull)
	}
}
