package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// Validation-branch unit tests for the M2 self-service methods: error
// injection on the repo layer drives the wrapped-error branches that DB
// tests can't reach (no tx is ever begun on these paths, so a nil *sqlx.DB
// service is safe).

type selfServiceSubRepo struct {
	stubSubRepo
	byIDSub *model.Subscription
	byIDErr error
}

func (s *selfServiceSubRepo) FindByID(_ context.Context, _ string) (*model.Subscription, error) {
	if s.byIDErr != nil {
		return nil, s.byIDErr
	}
	if s.byIDSub == nil {
		return nil, sql.ErrNoRows
	}
	return s.byIDSub, nil
}

type selfServicePlanRepo struct {
	stubPlanRepo
	byID   map[string]*model.Plan
	errFor map[string]error
}

func (s *selfServicePlanRepo) FindByID(_ context.Context, id string) (*model.Plan, error) {
	if err, ok := s.errFor[id]; ok {
		return nil, err
	}
	if p, ok := s.byID[id]; ok {
		return p, nil
	}
	return nil, sql.ErrNoRows
}

func newSelfServiceUnitSvc(subRepo *selfServiceSubRepo, planRepo *selfServicePlanRepo) *PaymentService {
	return NewPaymentService(
		nil,
		&stubOrderRepoLookup{},
		nil,
		nil,
		subRepo,
		planRepo,
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
}

func TestCancelSubscriptionByID_FindErrorWrapped(t *testing.T) {
	svc := newSelfServiceUnitSvc(
		&selfServiceSubRepo{byIDErr: errors.New("db down")},
		&selfServicePlanRepo{},
	)
	_, err := svc.CancelSubscriptionByID(context.Background(), "u1", "s1", "")
	if err == nil || !strings.Contains(err.Error(), "find subscription") {
		t.Fatalf("expected wrapped find error, got %v", err)
	}
}

func TestChangePlanByID_ValidationErrorBranches(t *testing.T) {
	paddle := "paddle"
	activeSub := &model.Subscription{
		ID: "s1", UserID: "u1", PlanID: "monthly", Status: "active",
		Channel: &paddle, AutoRenew: true, ProductCode: "kaya-membership",
		ExternalSubscriptionID: &paddleExt,
	}
	monthly := &model.Plan{ID: "monthly", IntervalDays: 30, IsActive: true, AcceptingNewSubscriptions: true, ProductCode: "kaya-membership"}
	yearly := &model.Plan{ID: "yearly", IntervalDays: 365, IsActive: true, AcceptingNewSubscriptions: true, ProductCode: "kaya-membership"}

	t.Run("find subscription error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDErr: errors.New("db down")},
			&selfServicePlanRepo{},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find subscription") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("find target plan error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{errFor: map[string]error{"yearly": errors.New("db down")}},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find target plan") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("current plan missing → ErrPlanNotFound", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			// yearly exists, monthly (the sub's plan) does not.
			&selfServicePlanRepo{byID: map[string]*model.Plan{"yearly": yearly}},
		)
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("find current plan error wrapped", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{
				byID:   map[string]*model.Plan{"yearly": yearly},
				errFor: map[string]error{"monthly": errors.New("db down")},
			},
		)
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find current plan") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no paddle client → ErrPaddleNotConfigured (unit)", func(t *testing.T) {
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{byID: map[string]*model.Plan{"monthly": monthly, "yearly": yearly}},
		)
		svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPaddleNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})

	// M1: retired target plan must be rejected BEFORE any Paddle call —
	// same retirement gate as UpgradeChannelSubscription (plan may be
	// active but no longer acquire new billing relationships).
	t.Run("retired target plan → ErrPlanNotAcceptingNew, paddle not called", func(t *testing.T) {
		retired := &model.Plan{ID: "yearly", IntervalDays: 365, IsActive: true, AcceptingNewSubscriptions: false}
		stub := &stubPaddle{}
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{byID: map[string]*model.Plan{"monthly": monthly, "yearly": retired}},
		)
		svc.SetPaddleClient(stub)
		svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanNotAcceptingNew) {
			t.Fatalf("got %v", err)
		}
		if stub.updateCalls != 0 {
			t.Errorf("paddle update calls = %d, want 0 (reject before any channel-side call)", stub.updateCalls)
		}
	})

	// M1: cross-product target plan must be rejected BEFORE any Paddle
	// call — same gate as UpgradeChannelSubscription (a plan change is not
	// an upgrade across product boundaries).
	t.Run("cross-product target plan → ErrPlanChangeNotUpgrade, paddle not called", func(t *testing.T) {
		cross := &model.Plan{ID: "yearly", IntervalDays: 365, IsActive: true,
			AcceptingNewSubscriptions: true, ProductCode: "coding-plan"}
		stub := &stubPaddle{}
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: activeSub},
			&selfServicePlanRepo{byID: map[string]*model.Plan{"monthly": monthly, "yearly": cross}},
		)
		svc.SetPaddleClient(stub)
		svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanChangeNotUpgrade) {
			t.Fatalf("got %v", err)
		}
		if stub.updateCalls != 0 {
			t.Errorf("paddle update calls = %d, want 0 (reject before any channel-side call)", stub.updateCalls)
		}
	})
}

// Minor 2 (review): the plan comparison (downgrade判定) is re-verified
// INSIDE the row lock — the unlocked fromPlan read is stale when a
// concurrent change-plan committed a different plan_id on this row between
// the pre-read and the FOR UPDATE lock. These unit cases drive the in-lock
// branches with a fake tx (no DB needed: the pre-lock reads come from stub
// repos and every case rejects before the Paddle call).
func TestChangePlanByID_InLockPlanRecheck(t *testing.T) {
	paddle := "paddle"
	preReadSub := &model.Subscription{
		ID: "s1", UserID: "u1", PlanID: "monthly", Status: "active",
		Channel: &paddle, AutoRenew: true, ProductCode: "kaya-membership",
		ExternalSubscriptionID: &paddleExt,
	}
	monthly := &model.Plan{ID: "monthly", IntervalDays: 30, IsActive: true, AcceptingNewSubscriptions: true, ProductCode: "kaya-membership"}
	quarterly := &model.Plan{ID: "quarterly", IntervalDays: 90, IsActive: true, AcceptingNewSubscriptions: true, ProductCode: "kaya-membership"}
	yearly := &model.Plan{ID: "yearly", IntervalDays: 365, IsActive: true, AcceptingNewSubscriptions: true, ProductCode: "kaya-membership"}
	codingMonthly := &model.Plan{ID: "coding-monthly", IntervalDays: 30, IsActive: true,
		AcceptingNewSubscriptions: true, ProductCode: "coding-plan"}

	plans := map[string]*model.Plan{
		"monthly": monthly, "quarterly": quarterly, "yearly": yearly, "coding-monthly": codingMonthly,
	}
	planRepo := &selfServicePlanRepo{byID: plans}

	// newSvc returns a unit service whose tx lock always returns lockedPlan
	// as the row under the FOR UPDATE lock.
	newSvc := func(lockedPlanID string) (*PaymentService, *stubPaddle) {
		locked := *preReadSub
		locked.PlanID = lockedPlanID
		stub := &stubPaddle{}
		svc := newSelfServiceUnitSvc(
			&selfServiceSubRepo{byIDSub: preReadSub},
			planRepo,
		)
		svc.SetPaddleClient(stub)
		svc.SetPaddlePrices(map[string]string{"quarterly": "pri_q", "yearly": "pri_y"})
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return &subFillingTx{sub: &locked}, nil
		}
		return svc, stub
	}

	t.Run("concurrent change-plan committed longer cycle → downgrade判定 in-lock rejects", func(t *testing.T) {
		// Unlocked read: monthly → quarterly (90d) looks like an upgrade.
		// Locked row: a concurrent change-plan already moved the sub to
		// yearly — quarterly is now a downgrade and must be rejected.
		svc, stub := newSvc("yearly")
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "quarterly"); !errors.Is(err, ErrPlanDowngradeNotSupported) {
			t.Fatalf("got %v", err)
		}
		if stub.updateCalls != 0 {
			t.Errorf("paddle update calls = %d, want 0 (reject before any channel-side call)", stub.updateCalls)
		}
	})

	t.Run("locked row already on target plan → ErrSamePlanChange", func(t *testing.T) {
		svc, stub := newSvc("yearly")
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrSamePlanChange) {
			t.Fatalf("got %v", err)
		}
		if stub.updateCalls != 0 {
			t.Errorf("paddle update calls = %d, want 0", stub.updateCalls)
		}
	})

	t.Run("locked row's plan is another product → ErrPlanChangeNotUpgrade in-lock", func(t *testing.T) {
		svc, stub := newSvc("coding-monthly")
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanChangeNotUpgrade) {
			t.Fatalf("got %v", err)
		}
		if stub.updateCalls != 0 {
			t.Errorf("paddle update calls = %d, want 0", stub.updateCalls)
		}
	})

	t.Run("locked row's plan vanished → ErrPlanNotFound", func(t *testing.T) {
		svc, _ := newSvc("ghost-plan")
		planRepo.errFor = map[string]error{"ghost-plan": sql.ErrNoRows}
		defer func() { planRepo.errFor = nil }()
		if _, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly"); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("locked row's plan read error → wrapped", func(t *testing.T) {
		svc, _ := newSvc("ghost-plan")
		planRepo.errFor = map[string]error{"ghost-plan": errors.New("db down")}
		defer func() { planRepo.errFor = nil }()
		_, err := svc.ChangePlanByID(context.Background(), "u1", "s1", "yearly")
		if err == nil || !strings.Contains(err.Error(), "find locked current plan") {
			t.Fatalf("got %v", err)
		}
	})
}

var paddleExt = "sub_unit_test"
