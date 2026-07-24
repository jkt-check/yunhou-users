package service

import (
	"context"
	"errors"
	"testing"

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

		svc := NewPlanService(planRepo)
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

		svc := NewPlanService(planRepo)
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

		svc := NewPlanService(planRepo)
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
		svc := NewPlanService(planRepo)

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
		svc := NewPlanService(planRepo)

		plan := &model.Plan{
			ID:           "test-plan",
			Name:         "Test Plan",
			Price:        9.99,
			IntervalDays: 30,
			IsActive:     true,
			IsDefault:    true,
		}

		err := svc.CreatePlan(ctx, plan)
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
		if created.IsDefault {
			t.Error("IsDefault = true, want false")
		}
	})

	t.Run("create plan with error", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errTest
		svc := NewPlanService(planRepo)

		plan := &model.Plan{ID: "test", Name: "Test"}
		err := svc.CreatePlan(ctx, plan)
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
		svc := NewPlanService(planRepo)

		plan := &model.Plan{ID: "monthly", Name: "New Name", Price: 29.9}
		err := svc.UpdatePlan(ctx, plan)
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
		svc := NewPlanService(planRepo)

		plan := &model.Plan{ID: "test", Name: "Test"}
		err := svc.UpdatePlan(ctx, plan)
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
		svc := NewPlanService(planRepo)

		err := svc.DeletePlan(ctx, "to-delete")
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
		svc := NewPlanService(planRepo)

		err := svc.DeletePlan(ctx, "test")
		if err == nil {
			t.Error("expected error")
		}
	})
}

func TestPlanService_CheckAppAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("user with subscription can access included app", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["monthly"] = &model.Plan{ID: "monthly", Name: "按月订阅", IsActive: true, Apps: []string{"yundian", "yundash"}}
		svc := NewPlanService(planRepo)

		sub := &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "monthly"}
		canAccess := svc.CheckAppAccess(ctx, sub, "yundian")
		if !canAccess {
			t.Error("expected true for yundian on monthly plan")
		}
	})

	t.Run("user with subscription cannot access excluded app", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", Apps: []string{"yundian"}}
		svc := NewPlanService(planRepo)

		sub := &model.Subscription{ID: "sub-1", UserID: "user-1", PlanID: "free"}
		canAccess := svc.CheckAppAccess(ctx, sub, "yundash")
		if canAccess {
			t.Error("expected false for yundash on free plan")
		}
	})

	t.Run("user without subscription uses default plan", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.plans["free"] = &model.Plan{ID: "free", Name: "免费", IsActive: true, Apps: []string{"yundian"}}
		planRepo.defaultPlan = planRepo.plans["free"]
		svc := NewPlanService(planRepo)

		canAccess := svc.CheckAppAccess(ctx, nil, "yundian")
		if !canAccess {
			t.Error("expected true for yundian on default free plan")
		}

		canAccess = svc.CheckAppAccess(ctx, nil, "yundash")
		if canAccess {
			t.Error("expected false for yundash on default free plan")
		}
	})
}

// TestPlanService_CheckAppAccess_RarePaths covers the "find default
// plan error" and "find plan by id error" branches the table-driven
// test doesn't reach.
func TestPlanService_CheckAppAccess_RarePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("FindDefault error when no subscription", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errors.New("db down")
		svc := NewPlanService(planRepo)
		if svc.CheckAppAccess(ctx, nil, "yundian") {
			t.Error("expected false when FindDefault errors")
		}
	})

	t.Run("FindByID error when subscription exists", func(t *testing.T) {
		planRepo := newMockPlanRepo()
		planRepo.err = errors.New("db down")
		svc := NewPlanService(planRepo)
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
		svc := NewPlanService(planRepo)
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
		svc := NewPlanService(planRepo)
		_, err := svc.FindByApp(ctx, "yundian")
		if err == nil {
			t.Error("expected error, got nil")
		}
	})

	t.Run("empty result for unknown app", func(t *testing.T) {
		t.Parallel()
		planRepo := newMockPlanRepo()
		svc := NewPlanService(planRepo)
		plans, err := svc.FindByApp(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plans) != 0 {
			t.Errorf("expected 0 plans, got %d", len(plans))
		}
	})
}
