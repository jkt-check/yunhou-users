package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

func TestPlanService_ListPlans(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("list all plans", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true}
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅", IsActive: true}
		planRepo.plans["inactive"] = &model.Plan{ID: "inactive", Name: "Inactive", IsActive: false}

		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		plans, err := svc.ListPlans(ctx)
		if err != nil {
			t.Fatalf("list plans: %v", err)
		}

		if len(plans) != 3 {
			t.Errorf("expected 3 plans, got %d", len(plans))
		}
	})

	t.Run("list returns error", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errTest

		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		_, err := svc.ListPlans(ctx)
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestPlanService_GetPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("get existing plan", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅", Price: 29.9}

		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		plan, err := svc.GetPlan(ctx, "monthly")
		if err != nil {
			t.Fatalf("get plan: %v", err)
		}
		if plan.ID != "monthly" {
			t.Errorf("expected plan ID monthly, got %s", plan.ID)
		}
		if plan.Price != 29.9 {
			t.Errorf("expected price 29.9, got %f", plan.Price)
		}
	})

	t.Run("get nonexistent plan", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		_, err := svc.GetPlan(ctx, "nonexistent")
		if err == nil {
			t.Error("expected error for nonexistent plan")
		}
	})
}

func TestPlanService_CreatePlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("create plan successfully", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		plan := &model.Plan{
			ID:           "test-plan",
			Name:         "Test Plan",
			Price:        9.99,
			IntervalDays: 30,
			IsActive:     true,
		}

		err := svc.CreatePlan(ctx, plan, "admin")
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}

		created := planRepo.plans["test-plan"]
		if created == nil {
			t.Fatal("plan was not stored")
		}
		if created.Currency != "CNY" {
			t.Errorf("Currency = %q, want CNY", created.Currency)
		}
	})

	t.Run("create plan with error", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errTest
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		plan := &model.Plan{ID: "test", Name: "Test"}
		err := svc.CreatePlan(ctx, plan, "admin")
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestPlanService_UpdatePlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("update existing plan", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "Old Name", Price: 19.9}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		plan := &model.Plan{ID: "monthly", Name: "New Name", Price: 29.9}
		err := svc.UpdatePlan(ctx, plan, "admin")
		if err != nil {
			t.Fatalf("update plan: %v", err)
		}

		if planRepo.plans["monthly"].Name != "New Name" {
			t.Errorf("expected name New Name, got %s", planRepo.plans["monthly"].Name)
		}
		if planRepo.plans["monthly"].Price != 29.9 {
			t.Errorf("expected price 29.9, got %f", planRepo.plans["monthly"].Price)
		}
	})

	t.Run("update plan with error", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errTest
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		plan := &model.Plan{ID: "test", Name: "Test"}
		err := svc.UpdatePlan(ctx, plan, "admin")
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestPlanService_DeletePlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("delete existing plan", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["to-delete"] = &model.Plan{ID: "to-delete", Name: "To Delete"}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		err := svc.DeletePlan(ctx, "to-delete", "admin")
		if err != nil {
			t.Fatalf("delete plan: %v", err)
		}

		if planRepo.plans["to-delete"] != nil {
			t.Error("plan was not deleted")
		}
	})

	t.Run("delete plan with error", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errTest
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		err := svc.DeletePlan(ctx, "test", "admin")
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestPlanService_CreatePlan_WritesAuditLog(t *testing.T) {
	ctx := context.Background()
	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	// B1: defense-in-depth ValidateApps runs at the top of CreatePlan
	// (before the tx). Seed the apps the plan references so the
	// pre-tx check passes and we exercise the in-tx audit-write path.
	appRepo.seedActive("yundian", "云店")
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)
	// Stub the tx-scoped validation so the test exercises the
	// audit-write path without standing up a tx-aware app repo.
	svc.validateAppsForShareFn = func(_ context.Context, _ *sqlx.Tx, _ []string) error {
		return nil
	}
	// The mock's WithTx (default) calls fn(nil); the service body
	// uses interface methods (CreateTx / InsertTx) that ignore the tx,
	// so fn runs end-to-end and InsertTx captures the audit row.
	plan := &model.Plan{ID: "pro", Name: "Pro", Price: 99, Apps: []string{"yundian"}}

	if err := svc.CreatePlan(ctx, plan, "admin"); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if len(changeLogRepo.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(changeLogRepo.calls))
	}
	call := changeLogRepo.calls[0]
	if call.planID != plan.ID || call.actorID != "admin" || call.changeType != "plan_create" {
		t.Errorf("audit identity = (%q, %q, %q), want (%q, admin, plan_create)", call.planID, call.actorID, call.changeType, plan.ID)
	}
	if call.before != nil {
		t.Errorf("before = %#v, want nil", call.before)
	}
	if call.after == nil || !reflect.DeepEqual(*call.after, *plan) {
		t.Errorf("after = %#v, want %#v", call.after, plan)
	}
}

func TestPlanService_UpdatePlan_WritesAuditLog(t *testing.T) {
	ctx := context.Background()
	planRepo := newMockPlanRepo()
	before := &model.Plan{ID: "pro", Name: "Old", Price: 99, Apps: []string{"yundian"}}
	planRepo.plans[before.ID] = before
	appRepo := newMockAppRepo()
	// B1: defense-in-depth ValidateApps runs at the top of UpdatePlan
	// (after FindByID, before the tx). Seed both apps the update
	// references so the pre-tx check passes.
	appRepo.seedActive("yundian", "云店")
	appRepo.seedActive("yundash", "云盘")
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)
	// See TestPlanService_CreatePlan_WritesAuditLog — the tx-scoped app
	// validation must be stubbed for mock-based tests.
	svc.validateAppsForShareFn = func(_ context.Context, _ *sqlx.Tx, _ []string) error {
		return nil
	}
	after := &model.Plan{ID: "pro", Name: "New", Price: 129, Apps: []string{"yundian", "yundash"}}

	if err := svc.UpdatePlan(ctx, after, "admin"); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if len(changeLogRepo.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(changeLogRepo.calls))
	}
	call := changeLogRepo.calls[0]
	if call.planID != after.ID || call.actorID != "admin" || call.changeType != "plan_update" {
		t.Errorf("audit identity = (%q, %q, %q), want (%q, admin, plan_update)", call.planID, call.actorID, call.changeType, after.ID)
	}
	if call.before == nil || !reflect.DeepEqual(*call.before, *before) {
		t.Errorf("before = %#v, want %#v", call.before, before)
	}
	if call.after == nil || !reflect.DeepEqual(*call.after, *after) {
		t.Errorf("after = %#v, want %#v", call.after, after)
	}
}

// TestPlanService_DeletePlan_WritesArchiveLog covers the happy-path
// audit identity (plan_id / actor_id / change_type) and the
// before-snapshot capture. It does NOT verify call ordering
// (InsertTx must precede DeleteTx) — see
// TestPlanService_DeletePlan_AuditBeforeDelete — nor the
// no-leak-on-FK-failure contract — see
// TestPlanService_DeletePlan_NoAuditLeakOnDeleteFailure.
func TestPlanService_DeletePlan_WritesArchiveLog(t *testing.T) {
	ctx := context.Background()
	planRepo := newMockPlanRepo()
	snapshot := &model.Plan{ID: "legacy", Name: "Legacy", Price: 49, Apps: []string{"yundian"}}
	planRepo.plans[snapshot.ID] = snapshot
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, newMockAppRepo(), changeLogRepo)

	if err := svc.DeletePlan(ctx, snapshot.ID, "admin"); err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	if len(changeLogRepo.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(changeLogRepo.calls))
	}
	call := changeLogRepo.calls[0]
	if call.planID != snapshot.ID || call.actorID != "admin" || call.changeType != "plan_archive" {
		t.Errorf("audit identity = (%q, %q, %q), want (%q, admin, plan_archive)", call.planID, call.actorID, call.changeType, snapshot.ID)
	}
	if call.before == nil || !reflect.DeepEqual(*call.before, *snapshot) {
		t.Errorf("before = %#v, want %#v", call.before, snapshot)
	}
	if call.after != nil {
		t.Errorf("after = %#v, want nil", call.after)
	}
}

// TestPlanService_DeletePlan_AuditBeforeDelete pins the A2 contract
// that the plan_change_log audit row is written BEFORE the hard DELETE
// on plans. The plan_change_log.plan_id FK was relaxed to ON DELETE
// SET NULL in migration 013 (so a post-delete audit row with
// plan_id=NULL survives), but at the moment of the INSERT the plan
// row must still exist — otherwise the INSERT itself fails with a FK
// violation and the operator loses the audit trail. The test asserts
// the call order by reading the per-mock `at` timestamps on
// changeLogRepo.calls[0] (the audit InsertTx) and planRepo.txCalls[0]
// (the plan DeleteTx). time.Now() is monotonic, so consecutive calls
// produce strictly increasing timestamps.
func TestPlanService_DeletePlan_AuditBeforeDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	planRepo.plans["legacy"] = &model.Plan{ID: "legacy", Name: "Legacy", Apps: []string{"yundian"}}
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, newMockAppRepo(), changeLogRepo)

	if err := svc.DeletePlan(ctx, "legacy", "admin"); err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}

	if len(changeLogRepo.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(changeLogRepo.calls))
	}
	if len(planRepo.txCalls) != 1 {
		t.Fatalf("planRepo txCalls = %d, want 1", len(planRepo.txCalls))
	}

	auditCall := changeLogRepo.calls[0]
	planCall := planRepo.txCalls[0]
	if planCall.op != "DeleteTx" {
		t.Errorf("planRepo txCall op = %q, want DeleteTx", planCall.op)
	}
	if planCall.planID != "legacy" {
		t.Errorf("planRepo txCall planID = %q, want legacy", planCall.planID)
	}
	if auditCall.changeType != "plan_archive" {
		t.Errorf("audit changeType = %q, want plan_archive", auditCall.changeType)
	}

	if !auditCall.at.Before(planCall.at) {
		t.Errorf("audit (%v) must be strictly before DeleteTx (%v) — otherwise the plan_change_log.plan_id FK fails at INSERT time",
			auditCall.at, planCall.at)
	}
}

// fkViolationErr is a stand-in for *pq.Error{Code: "23503"} (foreign
// key violation). Used by TestPlanService_DeletePlan_NoAuditLeakOnDeleteFailure
// to drive the FK-rollback path without standing up a real Postgres.
// errors.Is(err, fkViolationErr{}) returns true so the test can
// pin the error type the operator would see in production.
type fkViolationErr struct{}

func (fkViolationErr) Error() string { return "mock FK violation (SQLSTATE 23503)" }

// TestPlanService_DeletePlan_NoAuditLeakOnDeleteFailure pins the A2
// contract that when the hard DELETE fails with SQLSTATE 23503
// (subscriptions / orders still reference the plan), the just-inserted
// audit row is rolled back together with the failed DELETE — the
// operator never sees a ghost "plan archived" row for a plan that
// still exists.
//
// The mock can't model real tx semantics (it has no rollback state),
// so the test verifies the *proxy* conditions:
//   - DeleteTx returns the FK error (set via planRepo.deleteErr)
//   - The mock's InsertTx was called (the service *attempted* the
//     audit insert — production would commit it inside the same tx)
//   - A custom withTxFn observes the tx body returning an error and
//     "rolls back" (captures rolledBack=true), proving WithTx would
//     abort the transaction instead of committing
//   - The plan row still exists in the mock state (Delete returned
//     the error before mutating m.plans)
//   - The service surfaces the FK error to the caller
//
// A real-DB verification (plan_change_log row count == 0 after
// rollback) lives in internal/repo/repo_test.go's TestPlanRepo_* suite
// if/when it covers that path.
func TestPlanService_DeletePlan_NoAuditLeakOnDeleteFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	planRepo.plans["in-use"] = &model.Plan{ID: "in-use", Name: "In Use", Apps: []string{"yundian"}}
	// Force DeleteTx → Delete to surface an FK violation. m.err is left
	// nil so FindByID at the top of DeletePlan still succeeds (it
	// reads m.plans["in-use"] and returns the seeded plan).
	planRepo.deleteErr = fkViolationErr{}

	// Track whether the tx body returned an error — if it did, the
	// production WithTx would call tx.Rollback() instead of
	// tx.Commit(). The mock's default WithTx just returns fn's error,
	// so we wrap it to observe the rollback decision.
	rolledBack := false
	committed := false
	planRepo.withTxFn = func(_ context.Context, fn func(*sqlx.Tx) error) error {
		err := fn(nil)
		if err != nil {
			rolledBack = true
			return err
		}
		committed = true
		return nil
	}

	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, newMockAppRepo(), changeLogRepo)

	err := svc.DeletePlan(ctx, "in-use", "admin")
	if err == nil {
		t.Fatal("DeletePlan: expected FK error, got nil")
	}
	if !errors.Is(err, fkViolationErr{}) {
		t.Errorf("DeletePlan err = %v, want errors.Is fkViolationErr", err)
	}

	// 1. The service attempted the audit insert (the mock's InsertTx
	//    captured the call). In production the audit row lives inside
	//    the same tx as the failed DELETE — the rollback below takes
	//    it out.
	if len(changeLogRepo.calls) != 1 {
		t.Errorf("changeLogRepo.calls = %d, want 1 — service must attempt audit insert before DELETE", len(changeLogRepo.calls))
	}
	if changeLogRepo.calls[0].changeType != "plan_archive" {
		t.Errorf("audit changeType = %q, want plan_archive", changeLogRepo.calls[0].changeType)
	}

	// 2. WithTx simulated a rollback (not a commit). This is the proxy
	//    for the production tx.Rollback() that would discard the
	//    audit row.
	if !rolledBack {
		t.Error("tx was committed despite FK error — production would leave a ghost plan_archive audit row")
	}
	if committed {
		t.Error("tx body returned an error but WithTx reported a commit — race between rollback signal and commit")
	}

	// 3. The plan row is still present in mock state — Delete
	//    returned the FK error before mutating m.plans.
	if _, ok := planRepo.plans["in-use"]; !ok {
		t.Error("plan was removed from mock state — Delete must not mutate state when it returns an error")
	}

	// 4. The mock recorded exactly one DeleteTx call (proves the
	//    service reached the DELETE step inside the tx body).
	if len(planRepo.txCalls) != 1 || planRepo.txCalls[0].op != "DeleteTx" {
		t.Errorf("planRepo.txCalls = %+v, want one DeleteTx entry", planRepo.txCalls)
	}
}

func TestPlanService_CheckAppAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("user with subscription can access included app", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅", IsActive: true, Apps: []string{"yundian", "yundash"}}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		sub := &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "monthly"}
		canAccess := svc.CheckAppAccess(ctx, sub, "yundian")
		if !canAccess {
			t.Error("expected true for yundian on monthly plan")
		}
	})

	t.Run("user with subscription cannot access excluded app", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		sub := &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "free"}
		canAccess := svc.CheckAppAccess(ctx, sub, "yundash")
		if canAccess {
			t.Error("expected false for yundash on free plan")
		}
	})

	t.Run("user without subscription has no access", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true, Apps: []string{"yundian"}}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())

		canAccess := svc.CheckAppAccess(ctx, nil, "yundian")
		if canAccess {
			t.Error("expected false without subscription")
		}

		canAccess = svc.CheckAppAccess(ctx, nil, "yundash")

	})
}

// TestPlanService_CheckAppAccess_RarePaths covers the "find default
// plan error" and "find plan by id error" branches the table-driven
// test doesn't reach.
func TestPlanService_CheckAppAccess_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no subscription has no access", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errors.New("db down")
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		if svc.CheckAppAccess(ctx, nil, "yundian") {
			t.Error("expected false without subscription")
		}
	})

	t.Run("FindByID error when subscription exists", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errors.New("db down")
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		sub := &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "missing"}
		if svc.CheckAppAccess(ctx, sub, "yundian") {
			t.Error("expected false when FindByID errors")
		}
	})
}

// errTest is a test error
var errTest = errTestType{}

type errTestType struct{}

func (e errTestType) Error() string { return "test error" }

func TestPlanService_FindByApp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("returns plans from repo", func(t *testing.T) {
		t.Parallel()
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true, Apps: []string{"yundian"}}
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅", IsActive: true, Apps: []string{"yundian", "yundash"}}
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		plans, err := svc.FindByApp(ctx, "yundian")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plans) != 2 {
			t.Errorf("expected 2 plans, got %d", len(plans))
		}
	})

	t.Run("propagates repo error", func(t *testing.T) {
		t.Parallel()
		planRepo := newMockPlanRepo()
		planRepo.err = errTest
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		_, err := svc.FindByApp(ctx, "yundian")
		if err == nil {
			t.Error("expected error, got nil")
		}
	})

	t.Run("empty result for unknown app", func(t *testing.T) {
		t.Parallel()
		planRepo := newMockPlanRepo()
		svc := NewPlanService(planRepo, newMockAppRepo(), newMockPlanChangeLogRepo())
		plans, err := svc.FindByApp(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plans) != 0 {
			t.Errorf("expected 0 plans, got %d", len(plans))
		}
	})
}

func TestPlanService_ValidateApps_UnknownApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	appRepo.seedActive("yundian", "云店")

	svc := NewPlanService(planRepo, appRepo, newMockPlanChangeLogRepo())

	err := svc.ValidateApps(ctx, []string{"yundian", "missing"})
	if err == nil {
		t.Fatal("expected error for unknown app, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("err = %v, want wraps ErrInvalidAppID", err)
	}
	// Verify "missing" appears in the error message so operators can
	// tell which ID was rejected (the spec calls this out explicitly).
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want message to mention 'missing'", err)
	}
}

func TestPlanService_ValidateApps_InactiveApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	appRepo.seedActive("yundian", "云店")
	appRepo.seedInactive("yundash", "云盘")

	svc := NewPlanService(planRepo, appRepo, newMockPlanChangeLogRepo())

	err := svc.ValidateApps(ctx, []string{"yundian", "yundash"})
	if err == nil {
		t.Fatal("expected error for inactive app, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("err = %v, want wraps ErrInvalidAppID", err)
	}
	if !strings.Contains(err.Error(), "yundash") {
		t.Errorf("err = %v, want message to mention 'yundash'", err)
	}
}

func TestPlanService_ValidateApps_AllValid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	appRepo.seedActive("yundian", "云店")
	appRepo.seedActive("yundash", "云盘")

	svc := NewPlanService(planRepo, appRepo, newMockPlanChangeLogRepo())

	if err := svc.ValidateApps(ctx, []string{"yundian", "yundash"}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestPlanService_ValidateApps_RepoError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	appRepo.findErr = errors.New("db down")

	svc := NewPlanService(planRepo, appRepo, newMockPlanChangeLogRepo())

	// A non-ErrNoRows error from the repo must surface as-is (not be
	// remapped to ErrInvalidAppID) so callers can distinguish a DB
	// outage from a real validation failure.
	err := svc.ValidateApps(ctx, []string{"yundian"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrInvalidAppID) {
		t.Errorf("err = %v, want does NOT wrap ErrInvalidAppID", err)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Errorf("err = %v, want underlying 'db down' to surface", err)
	}
}

// TestPlanService_CreatePlan_AppDeactivatedDuringTx pins the D8
// TOCTOU guarantee for CreatePlan: the FOR SHARE app lookup inside
// validateAppsForShareTx MUST see the post-deactivation truth, so a
// concurrent app deactivation (admin PATCH /apps/:id with
// is_active=false) between the prior ValidateApps call and the lock
// inside the eligibility tx surfaces as ErrInvalidAppID ("... is
// inactive"). The plan INSERT must NOT run.
//
// The fixture installs validateAppsForShareFn on the plan service to
// force the lock-step error (mimicking what a real DB would return
// when the app row's is_active flipped between calls).
func TestPlanService_CreatePlan_AppDeactivatedDuringTx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	appRepo.seedActive("yundian", "云店")

	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)
	// Simulate the "app-active at the initial ValidateApps call but
	// deactivated by the time the FOR SHARE lock fires" race.
	svc.validateAppsForShareFn = func(_ context.Context, _ *sqlx.Tx, apps []string) error {
		return fmt.Errorf("%w: %s is inactive", ErrInvalidAppID, apps[0])
	}

	plan := &model.Plan{
		ID:   "monthly",
		Name: "Monthly",
		Apps: []string{"yundian"},
	}
	err := svc.CreatePlan(ctx, plan, "admin")
	if err == nil {
		t.Fatal("CreatePlan: expected ErrInvalidAppID-wrap from deactivation race, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("CreatePlan err = %v, want wraps ErrInvalidAppID", err)
	}
	// The plan mutation must NOT have run (otherwise the row points
	// at a now-inactive app).
	if _, ok := planRepo.plans["monthly"]; ok {
		t.Errorf("plans[monthly] = %+v, want nil — INSERT must roll back when lock fails", planRepo.plans["monthly"])
	}
	// The audit insert (which lives inside the same tx) must NOT have
	// run either.
	if len(changeLogRepo.calls) != 0 {
		t.Errorf("changeLogRepo.calls = %d, want 0 — audit insert must roll back when lock fails",
			len(changeLogRepo.calls))
	}
}

// TestPlanService_CreatePlan_RejectsUnknownApp covers B1 (defense-in-depth):
// the pre-tx ValidateApps call at the top of CreatePlan must reject an
// unknown app_id BEFORE the transaction starts, so planRepo.CreateTx and
// changeLogRepo.InsertTx are never called. This guards against a future
// internal caller (batch job, second handler) inserting a plan that
// points at a non-existent app and silently bypassing the FK-less
// TEXT[] apps column.
func TestPlanService_CreatePlan_RejectsUnknownApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	appRepo := newMockAppRepo()
	// No apps seeded — every app lookup should fail and
	// ValidateApps remaps sql.ErrNoRows to ErrInvalidAppID.
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)

	plan := &model.Plan{
		ID:   "new-plan",
		Name: "New Plan",
		Apps: []string{"yundian"}, // unknown — appRepo lookup fails
	}

	err := svc.CreatePlan(ctx, plan, "admin")
	if err == nil {
		t.Fatal("CreatePlan: expected ErrInvalidAppID-wrap for unknown app, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("CreatePlan err = %v, want wraps ErrInvalidAppID", err)
	}
	if !strings.Contains(err.Error(), "yundian") {
		t.Errorf("CreatePlan err = %v, want message to mention 'yundian'", err)
	}
	// planRepo.CreateTx must NOT have run — the pre-tx check failed,
	// so the tx wrapper never executes the function body.
	if _, ok := planRepo.plans["new-plan"]; ok {
		t.Errorf("planRepo.plans[new-plan] = %+v, want nil — INSERT must not run when pre-tx ValidateApps fails", planRepo.plans["new-plan"])
	}
	// changeLogRepo.InsertTx must NOT have run either.
	if len(changeLogRepo.calls) != 0 {
		t.Errorf("changeLogRepo.calls = %d, want 0 — audit insert must not run when pre-tx ValidateApps fails", len(changeLogRepo.calls))
	}
}

// TestPlanService_UpdatePlan_RejectsUnknownApp covers B1 (defense-in-depth):
// the pre-tx ValidateApps call at the top of UpdatePlan must reject an
// unknown app_id BEFORE the transaction starts, so planRepo.UpdateTx and
// changeLogRepo.InsertTx are never called. The rejected apps list is the
// `p.Apps` (the new list) — UpdatePlan already loaded `before` to
// compute the fallback when `p.Apps` is empty, but the rejection here
// is on the post-update apps to match the in-tx FOR SHARE check.
func TestPlanService_UpdatePlan_RejectsUnknownApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	existing := &model.Plan{ID: "monthly", Name: "Old Name", Price: 19.9, Apps: []string{"yundian"}}
	planRepo.plans[existing.ID] = existing
	appRepo := newMockAppRepo()
	appRepo.seedActive("yundian", "云店")
	// "yundash" is NOT seeded — the update's new apps list contains
	// an unknown id, so pre-tx ValidateApps must fail.
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)

	plan := &model.Plan{ID: "monthly", Name: "New Name", Price: 29.9, Apps: []string{"yundash"}}

	err := svc.UpdatePlan(ctx, plan, "admin")
	if err == nil {
		t.Fatal("UpdatePlan: expected ErrInvalidAppID-wrap for unknown app, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("UpdatePlan err = %v, want wraps ErrInvalidAppID", err)
	}
	if !strings.Contains(err.Error(), "yundash") {
		t.Errorf("UpdatePlan err = %v, want message to mention 'yundash'", err)
	}
	// planRepo.UpdateTx must NOT have run — the pre-tx check failed,
	// so the row's existing values must be untouched.
	got := planRepo.plans["monthly"]
	if got.Name != "Old Name" {
		t.Errorf("plan.Name = %q, want %q — UPDATE must not run when pre-tx ValidateApps fails", got.Name, "Old Name")
	}
	if got.Price != 19.9 {
		t.Errorf("plan.Price = %v, want 19.9 — UPDATE must not run when pre-tx ValidateApps fails", got.Price)
	}
	// changeLogRepo.InsertTx must NOT have run either.
	if len(changeLogRepo.calls) != 0 {
		t.Errorf("changeLogRepo.calls = %d, want 0 — audit insert must not run when pre-tx ValidateApps fails", len(changeLogRepo.calls))
	}
}

// TestPlanService_UpdatePlan_RejectsUnknownAppInFallback covers B1
// (defense-in-depth) for the partial-update case: when the request
// omits `apps` (p.Apps is empty), UpdatePlan validates the inherited
// `before.Apps` list. If that pre-existing list points at an app that
// has since been deleted, validation must fail and the UPDATE must NOT
// run — without this, an admin who deletes an app row but leaves
// behind plans.apps references would still see the UPDATE succeed.
func TestPlanService_UpdatePlan_RejectsUnknownAppInFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	planRepo := newMockPlanRepo()
	existing := &model.Plan{ID: "monthly", Name: "Old Name", Price: 19.9, Apps: []string{"yundian"}}
	planRepo.plans[existing.ID] = existing
	appRepo := newMockAppRepo()
	// "yundian" is NOT seeded — the partial update will inherit
	// before.Apps=["yundian"] and validate it; the lookup fails.
	changeLogRepo := newMockPlanChangeLogRepo()
	svc := NewPlanService(planRepo, appRepo, changeLogRepo)

	// Partial update: only price changes, no apps in the request.
	plan := &model.Plan{ID: "monthly", Name: "Old Name", Price: 29.9}

	err := svc.UpdatePlan(ctx, plan, "admin")
	if err == nil {
		t.Fatal("UpdatePlan: expected ErrInvalidAppID-wrap for inherited unknown app, got nil")
	}
	if !errors.Is(err, ErrInvalidAppID) {
		t.Errorf("UpdatePlan err = %v, want wraps ErrInvalidAppID", err)
	}
	if !strings.Contains(err.Error(), "yundian") {
		t.Errorf("UpdatePlan err = %v, want message to mention 'yundian'", err)
	}
	// UPDATE must NOT have run.
	got := planRepo.plans["monthly"]
	if got.Price != 19.9 {
		t.Errorf("plan.Price = %v, want 19.9 — UPDATE must not run when pre-tx ValidateApps fails on inherited apps", got.Price)
	}
	if len(changeLogRepo.calls) != 0 {
		t.Errorf("changeLogRepo.calls = %d, want 0 — audit insert must not run when pre-tx ValidateApps fails", len(changeLogRepo.calls))
	}
}
