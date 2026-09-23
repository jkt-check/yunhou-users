package access

import (
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// benefit_grant_test.go — Task 10 纯规则：同步决策 / 收敛 / coding-plan 激活
// 期 / 迁移赠送幂等键。无 DB，注入固定时钟。

var benefitNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func activeSubState(planID string, expiresAt *time.Time) *SubscriptionState {
	return &SubscriptionState{
		ID: "sub-1", UserID: "u-1", PlanID: planID,
		ProductCode: model.ProductCodingPlan, Status: "active", ExpiresAt: expiresAt,
	}
}

func paidSnap(orderID, policyID, grantMode string) *OrderBenefitSnapshot {
	return &OrderBenefitSnapshot{
		OrderID: orderID, PolicyVersionID: policyID,
		ModelIDs: []string{"glm-4.6"}, GrantMode: grantMode,
	}
}

func TestDecideSync(t *testing.T) {
	exp := benefitNow.Add(30 * 24 * time.Hour)

	t.Run("nil subscription → retire revoked", func(t *testing.T) {
		d, err := DecideSync(nil, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target != nil || d.RetireAs != domain.EntitlementRevoked {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("cancelled subscription → retire revoked", func(t *testing.T) {
		sub := activeSubState("cp_basic", &exp)
		sub.Status = "cancelled"
		d, err := DecideSync(sub, paidSnap("o-1", "pol-1", model.BenefitGrantModeSubscription), benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target != nil || d.RetireAs != domain.EntitlementRevoked {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("lapsed subscription → retire expired", func(t *testing.T) {
		past := benefitNow.Add(-time.Hour)
		sub := activeSubState("cp_basic", &past)
		d, err := DecideSync(sub, paidSnap("o-1", "pol-1", model.BenefitGrantModeSubscription), benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target != nil || d.RetireAs != domain.EntitlementExpired {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("active subscription without paid order → backstop: no grant", func(t *testing.T) {
		d, err := DecideSync(activeSubState("cp_basic", &exp), nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target != nil || d.Reason != "no_paid_order_evidence" || d.RetireAs != domain.EntitlementRevoked {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("subscription mode → subscription source keyed on sub id", func(t *testing.T) {
		d, err := DecideSync(activeSubState("cp_basic", &exp), paidSnap("o-1", "pol-1", model.BenefitGrantModeSubscription), benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target == nil {
			t.Fatal("want target")
		}
		if d.Target.SourceType != domain.SourceSubscription || d.Target.SourceID != "sub-1" {
			t.Fatalf("source = %v/%v", d.Target.SourceType, d.Target.SourceID)
		}
		if d.Target.PolicyVersionID != "pol-1" || d.Target.Stackable {
			t.Fatalf("target = %+v", d.Target)
		}
		if d.Target.EffectiveTo == nil || !d.Target.EffectiveTo.Equal(exp) {
			t.Fatalf("effective_to = %v, want %v", d.Target.EffectiveTo, exp)
		}
	})

	t.Run("gift mode → grant source keyed bundle:<sub>", func(t *testing.T) {
		d, err := DecideSync(activeSubState("monthly", &exp), paidSnap("o-9", "pol-2", model.BenefitGrantModeGift), benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Target == nil || d.Target.SourceType != domain.SourceGrant || d.Target.SourceID != "bundle:sub-1" {
			t.Fatalf("target = %+v", d.Target)
		}
	})

	t.Run("missing policy version → error", func(t *testing.T) {
		snap := paidSnap("o-1", "", model.BenefitGrantModeSubscription)
		if _, err := DecideSync(activeSubState("cp_basic", &exp), snap, benefitNow); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("unknown grant mode → error", func(t *testing.T) {
		snap := paidSnap("o-1", "pol-1", "bogus")
		if _, err := DecideSync(activeSubState("cp_basic", &exp), snap, benefitNow); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestConverge(t *testing.T) {
	exp := benefitNow.Add(30 * 24 * time.Hour)
	target := &BenefitTarget{
		SourceType: domain.SourceSubscription, SourceID: "sub-1",
		PolicyVersionID: "pol-1", ModelIDs: []string{"glm-4.6"}, EffectiveTo: &exp,
	}
	decision := &SyncDecision{Reason: "active_subscription", Target: target}

	t.Run("no current → insert with anchor=now", func(t *testing.T) {
		a, err := Converge(nil, decision, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeInsert {
			t.Fatalf("op = %s", a.Op)
		}
		if !a.Anchor.Equal(benefitNow) || !a.EffectiveFrom.Equal(benefitNow) {
			t.Fatalf("anchor/from = %v/%v", a.Anchor, a.EffectiveFrom)
		}
		if a.InsertPlan.SourceType != domain.SourceSubscription || a.InsertPlan.SourceID != "sub-1" ||
			a.InsertPlan.PolicyVersionID != "pol-1" {
			t.Fatalf("insert plan = %+v", a.InsertPlan)
		}
	})

	current := func() *domain.Entitlement {
		return &domain.Entitlement{
			ID: "ent-1", BillingAccountID: "acct-1",
			SourceType: domain.SourceSubscription, SourceID: "sub-1",
			ModelIDs: []string{"glm-4.6"}, PolicyVersionID: "pol-1",
			AnchorAt: benefitNow.Add(-24 * time.Hour), EffectiveFrom: benefitNow.Add(-24 * time.Hour),
			EffectiveTo: &exp, Revision: 3, Status: domain.EntitlementActive,
		}
	}

	t.Run("identical state → noop (幂等重放)", func(t *testing.T) {
		a, err := Converge(current(), decision, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeNoop {
			t.Fatalf("op = %s", a.Op)
		}
	})

	t.Run("renewal → revise effective_to only, same revision guard", func(t *testing.T) {
		later := exp.Add(30 * 24 * time.Hour)
		d := &SyncDecision{Target: &BenefitTarget{
			SourceType: domain.SourceSubscription, SourceID: "sub-1",
			PolicyVersionID: "pol-1", ModelIDs: []string{"glm-4.6"}, EffectiveTo: &later,
		}}
		a, err := Converge(current(), d, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRevise || a.Patch.EffectiveTo == nil || !a.Patch.EffectiveTo.Equal(later) {
			t.Fatalf("action = %+v", a)
		}
		if a.Patch.ExpectedRevision != 3 || a.Patch.PolicyVersionID != nil || a.Patch.ModelIDs != nil {
			t.Fatalf("patch = %+v", a.Patch)
		}
	})

	t.Run("upgrade → revise policy+models in place", func(t *testing.T) {
		d := &SyncDecision{Target: &BenefitTarget{
			SourceType: domain.SourceSubscription, SourceID: "sub-1",
			PolicyVersionID: "pol-2", ModelIDs: []string{"glm-4.6", "kimi-k2"}, EffectiveTo: &exp,
		}}
		a, err := Converge(current(), d, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRevise || a.Patch.PolicyVersionID == nil || *a.Patch.PolicyVersionID != "pol-2" {
			t.Fatalf("action = %+v", a)
		}
		if len(a.Patch.ModelIDs) != 2 {
			t.Fatalf("models = %v", a.Patch.ModelIDs)
		}
	})

	t.Run("retired current → revive", func(t *testing.T) {
		cur := current()
		cur.Status = domain.EntitlementRevoked
		a, err := Converge(cur, decision, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRevive || a.Patch.ExpectedRevision != 3 {
			t.Fatalf("action = %+v", a)
		}
	})

	t.Run("nil target retires active current", func(t *testing.T) {
		d := &SyncDecision{Reason: "subscription_cancelled", RetireAs: domain.EntitlementRevoked}
		a, err := Converge(current(), d, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRetire || a.RetireAs != domain.EntitlementRevoked || a.ExpectedRevision != 3 {
			t.Fatalf("action = %+v", a)
		}
	})

	t.Run("nil target on already-retired → noop", func(t *testing.T) {
		cur := current()
		cur.Status = domain.EntitlementRevoked
		d := &SyncDecision{RetireAs: domain.EntitlementRevoked}
		a, err := Converge(cur, d, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeNoop {
			t.Fatalf("op = %s", a.Op)
		}
	})

	t.Run("effective_to must stay after effective_from", func(t *testing.T) {
		bad := benefitNow.Add(-48 * time.Hour)
		d := &SyncDecision{Target: &BenefitTarget{
			SourceType: domain.SourceSubscription, SourceID: "sub-1",
			PolicyVersionID: "pol-1", ModelIDs: []string{"glm-4.6"}, EffectiveTo: &bad,
		}}
		if _, err := Converge(current(), d, benefitNow); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestResolveCodingPlanActivation(t *testing.T) {
	planID := "cp_pro"
	otherPlan := "cp_basic"
	fromPlan := "cp_basic"
	existingExp := benefitNow.Add(10 * 24 * time.Hour)

	t.Run("new, no current → now + interval", func(t *testing.T) {
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 30, nil, planID, nil, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || act.ExpiresAt == nil || !act.ExpiresAt.Equal(benefitNow.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("new with existing SAME plan → renewal rollover", func(t *testing.T) {
		cur := activeSubState(planID, &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 30, nil, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || act.ExpiresAt == nil || !act.ExpiresAt.Equal(existingExp.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("new with existing OTHER plan → conflict blocked", func(t *testing.T) {
		cur := activeSubState(otherPlan, &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 30, nil, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked == "" || act.ExpiresAt != nil {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("renewal extends from current expiry", func(t *testing.T) {
		cur := activeSubState(planID, &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindRenewal, 30, nil, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || !act.ExpiresAt.Equal(existingExp.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("renewal with lapsed sub starts fresh", func(t *testing.T) {
		past := benefitNow.Add(-time.Hour)
		cur := activeSubState(planID, &past)
		act, err := ResolveCodingPlanActivation(model.OrderKindRenewal, 30, nil, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || !act.ExpiresAt.Equal(benefitNow.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("upgrade replacing pinned from-plan → fresh period from now", func(t *testing.T) {
		cur := activeSubState(fromPlan, &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindUpgrade, 30, &fromPlan, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || !act.ExpiresAt.Equal(benefitNow.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("upgrade landing on own plan (double order) → extend", func(t *testing.T) {
		cur := activeSubState(planID, &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindUpgrade, 30, &fromPlan, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked != "" || !act.ExpiresAt.Equal(existingExp.Add(30*24*time.Hour)) {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("upgrade with unrelated current plan → conflict blocked", func(t *testing.T) {
		cur := activeSubState("cp_team", &existingExp)
		act, err := ResolveCodingPlanActivation(model.OrderKindUpgrade, 30, &fromPlan, planID, cur, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Blocked == "" {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("hint clamped to now+interval", func(t *testing.T) {
		far := benefitNow.Add(365 * 24 * time.Hour)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 30, nil, planID, nil, &far, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if !act.ExpiresAt.Equal(benefitNow.Add(30 * 24 * time.Hour)) {
			t.Fatalf("expires = %v", act.ExpiresAt)
		}
	})

	t.Run("interval 0 → open-ended", func(t *testing.T) {
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 0, nil, planID, nil, nil, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.ExpiresAt != nil || act.Blocked != "" {
			t.Fatalf("act = %+v", act)
		}
	})

	t.Run("lifetime order + past hint → stays open-ended", func(t *testing.T) {
		past := benefitNow.Add(-24 * time.Hour)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 0, nil, planID, nil, &past, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.ExpiresAt != nil || act.Blocked != "" {
			t.Fatalf("past hint must not expire a lifetime grant, act = %+v", act)
		}
	})

	t.Run("lifetime order + future hint → stays open-ended", func(t *testing.T) {
		far := benefitNow.Add(30 * 24 * time.Hour)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 0, nil, planID, nil, &far, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.ExpiresAt != nil {
			t.Fatalf("hint must never bound an open-ended grant, expires = %v", act.ExpiresAt)
		}
	})

	t.Run("open-ended renewal + past hint → stays open-ended", func(t *testing.T) {
		past := benefitNow.Add(-24 * time.Hour)
		cur := activeSubState(planID, nil) // 开放期订阅续期：rollFrom(nil) = nil
		act, err := ResolveCodingPlanActivation(model.OrderKindRenewal, 30, nil, planID, cur, &past, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.ExpiresAt != nil || act.Blocked != "" {
			t.Fatalf("past hint must not expire an open-ended rollover, act = %+v", act)
		}
	})

	t.Run("past hint never shortens a paid interval", func(t *testing.T) {
		past := benefitNow.Add(-24 * time.Hour)
		act, err := ResolveCodingPlanActivation(model.OrderKindNew, 30, nil, planID, nil, &past, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.ExpiresAt == nil || !act.ExpiresAt.Equal(benefitNow.Add(30*24*time.Hour)) {
			t.Fatalf("expires = %v", act.ExpiresAt)
		}
	})

	t.Run("unknown kind → error", func(t *testing.T) {
		if _, err := ResolveCodingPlanActivation("bogus", 30, nil, planID, nil, nil, benefitNow); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestMigrationGift(t *testing.T) {
	ent, err := MigrationGift("rule-2026-09", "user-1", []string{"glm-4.6"}, "pol-1", benefitNow, benefitNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ent.SourceType != domain.SourceGrant || ent.SourceID != "migration:rule-2026-09:user-1" {
		t.Fatalf("source = %v/%v", ent.SourceType, ent.SourceID)
	}
	if ent.Stackable {
		t.Fatal("gift must be non-stackable")
	}
	// 独立幂等来源键：同一 (rule, user) 永远得到同一 source id。
	if MigrationGiftSourceID("rule-2026-09", "user-1") != "migration:rule-2026-09:user-1" {
		t.Fatal("source id unstable")
	}
	if _, err := MigrationGift("", "user-1", nil, "pol-1", benefitNow, benefitNow, nil); err == nil {
		t.Fatal("want error for empty rule")
	}
	if _, err := MigrationGift("r", "user-1", nil, "", benefitNow, benefitNow, nil); err == nil {
		t.Fatal("want error for empty policy")
	}
}

// TestConverge_NilTargetEffectiveTo pins the审查修复: a nil target
// EffectiveTo (open-ended subscription, expires_at NULL) against an
// existing FINITE entitlement must not panic — it converges via an explicit
// set-NULL patch (ClearEffectiveTo), on both the revise and revive paths.
func TestConverge_NilTargetEffectiveTo(t *testing.T) {
	fin := benefitNow.Add(30 * 24 * time.Hour)
	mkCurrent := func(status domain.EntitlementStatus) *domain.Entitlement {
		return &domain.Entitlement{
			ID: "ent-1", BillingAccountID: "acct-1",
			SourceType: domain.SourceSubscription, SourceID: "sub-1",
			ModelIDs: []string{"glm-4.6"}, PolicyVersionID: "pol-1",
			AnchorAt: benefitNow.Add(-24 * time.Hour), EffectiveFrom: benefitNow.Add(-24 * time.Hour),
			EffectiveTo: &fin, Revision: 3, Status: status,
		}
	}
	openTarget := &SyncDecision{Target: &BenefitTarget{
		SourceType: domain.SourceSubscription, SourceID: "sub-1",
		PolicyVersionID: "pol-1", ModelIDs: []string{"glm-4.6"},
		EffectiveTo: nil, // 开放型目标
	}}

	t.Run("revise path: finite current + open target → ClearEffectiveTo, no panic", func(t *testing.T) {
		var a ConvergeAction
		var err error
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Converge panicked on nil target EffectiveTo: %v", r)
			}
		}()
		a, err = Converge(mkCurrent(domain.EntitlementActive), openTarget, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRevise {
			t.Fatalf("op = %s, want revise", a.Op)
		}
		if !a.Patch.ClearEffectiveTo || a.Patch.EffectiveTo != nil {
			t.Fatalf("patch = %+v, want ClearEffectiveTo=true and EffectiveTo=nil", a.Patch)
		}
	})

	t.Run("revive path: retired current + open target → ClearEffectiveTo, no panic", func(t *testing.T) {
		a, err := Converge(mkCurrent(domain.EntitlementRevoked), openTarget, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeRevive || !a.Patch.ClearEffectiveTo {
			t.Fatalf("action = %+v, want revive with ClearEffectiveTo", a)
		}
	})

	t.Run("both nil → noop", func(t *testing.T) {
		cur := mkCurrent(domain.EntitlementActive)
		cur.EffectiveTo = nil
		a, err := Converge(cur, openTarget, benefitNow)
		if err != nil {
			t.Fatal(err)
		}
		if a.Op != ConvergeNoop {
			t.Fatalf("op = %s, want noop", a.Op)
		}
	})
}
