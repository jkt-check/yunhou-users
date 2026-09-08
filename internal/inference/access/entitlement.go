package access

import (
	"context"
	"sort"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

// entitlement.go — 套餐版本 → 权益映射、来源与修订规则（Task 6，设计 §4.2）。
//
// An entitlement is the authorization source of a model call: which plan
// revision conferred it (subscription / order / gift rule), which explicit
// model set and quota policy version it carries, its effective range, and
// its revision. This file owns the pure mapping and selection rules; the
// postgres repo owns persistence.

// PlanRevision maps ONE commercial plan revision (套餐版本) to the explicit
// model set and quota policy version it confers (设计 §4.2/§5).
type PlanRevision struct {
	SourceType      domain.EntitlementSource
	SourceID        string
	ModelIDs        []string
	PolicyVersionID string
}

// GrantFromPlan mints the entitlement for one purchase/gift event.
//
//   - AnchorAt is the ORIGINAL effective instant driving the weekly/monthly
//     windows; it never moves on upgrade/renewal (设计 §6). A zero anchor
//     defaults to effectiveFrom.
//   - ModelIDs is ALWAYS explicit: nil collapses to an empty set, which
//     grants NO models — there is no NULL-means-all semantics, so a legacy
//     member without an explicit grant never auto-receives every model
//     (设计 §4.2, 基线报告差距 3).
//   - Gift rules default to non-stackable: no merge rule is published in
//     the first phase, so Stackable is always false here (设计 §4.2: 后续
//     需要叠加时再发布明确合并规则).
func GrantFromPlan(plan PlanRevision, anchor, effectiveFrom time.Time, effectiveTo *time.Time) (*domain.Entitlement, error) {
	switch plan.SourceType {
	case domain.SourceSubscription, domain.SourceOrder, domain.SourceGrant:
	default:
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: unknown source type "+string(plan.SourceType))
	}
	if plan.SourceID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: source id required")
	}
	if plan.PolicyVersionID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: policy version required")
	}
	from := effectiveFrom.UTC()
	a := anchor.UTC()
	if anchor.IsZero() {
		a = from
	}
	if a.After(from) {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: anchor must not be after effective_from")
	}
	if effectiveTo != nil && !effectiveTo.UTC().After(from) {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: effective_to must be after effective_from")
	}
	var to *time.Time
	if effectiveTo != nil {
		t := effectiveTo.UTC()
		to = &t
	}
	return &domain.Entitlement{
		SourceType:      plan.SourceType,
		SourceID:        plan.SourceID,
		ModelIDs:        dedupeModels(plan.ModelIDs),
		PolicyVersionID: plan.PolicyVersionID,
		AnchorAt:        a,
		EffectiveFrom:   from,
		EffectiveTo:     to,
		Revision:        1,
		Stackable:       false,
		Status:          domain.EntitlementActive,
	}, nil
}

func dedupeModels(ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// Selection: 显式套餐优先、赠送默认不叠加/不自动兜底（设计 §4.2）
// ---------------------------------------------------------------------------

// EntitlementStore is the persistence surface the resolver needs;
// satisfied by inference/postgres.Store.
type EntitlementStore interface {
	ListActiveEntitlements(ctx context.Context, billingAccountID string, at time.Time) ([]domain.Entitlement, error)
}

// EntitlementResolver implements domain.EntitlementResolver: it picks THE
// ONE entitlement governing a call. It never stacks entitlements — no
// merge rule is published in the first phase.
type EntitlementResolver struct {
	store EntitlementStore
	clock domain.Clock
}

// NewEntitlementResolver builds the resolver; a nil clock uses the system
// clock (UTC).
func NewEntitlementResolver(store EntitlementStore, clock domain.Clock) *EntitlementResolver {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &EntitlementResolver{store: store, clock: clock}
}

// Resolve implements domain.EntitlementResolver. A zero `at` reads the
// injected clock (server-side metering time).
func (r *EntitlementResolver) Resolve(ctx context.Context, billingAccountID, modelID string, at time.Time) (*domain.Entitlement, error) {
	if billingAccountID == "" || modelID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "entitlement: account and model required")
	}
	if at.IsZero() {
		at = r.clock.Now()
	}
	ents, err := r.store.ListActiveEntitlements(ctx, billingAccountID, at)
	if err != nil {
		return nil, err
	}
	return SelectEntitlement(ents, modelID, at)
}

// SelectEntitlement is the pure selection rule:
//
//  1. only entitlements active at `at` participate (defensive re-check of
//     the store's filter: status active, from <= at < to);
//  2. an explicit purchase (subscription/order) wins; gift (grant) rows
//     participate ONLY when the account has no active explicit entitlement
//     at all — gifts never stack with an explicit plan and never
//     auto-fallback after the plan is exhausted (设计 §4.2);
//  3. within the winning class the earliest-created row that grants the
//     model wins (deterministic; exactly one entitlement ever applies);
//  4. nothing grants the model → CodeModelNotAllowed. A legacy Kaya member
//     without an explicit grant therefore gets NO models automatically.
func SelectEntitlement(ents []domain.Entitlement, modelID string, at time.Time) (*domain.Entitlement, error) {
	t := at.UTC()
	var explicit, gifts []domain.Entitlement
	for _, e := range ents {
		if e.Status != domain.EntitlementActive {
			continue
		}
		if t.Before(e.EffectiveFrom.UTC()) {
			continue
		}
		if e.EffectiveTo != nil && !t.Before(e.EffectiveTo.UTC()) {
			continue
		}
		if e.SourceType == domain.SourceGrant {
			gifts = append(gifts, e)
		} else {
			explicit = append(explicit, e)
		}
	}
	pool := explicit
	if len(pool) == 0 {
		pool = gifts
	}
	if len(pool) == 0 {
		return nil, domain.NewError(domain.CodeModelNotAllowed,
			"no active entitlement for this account (无显式权益/赠送，不自动获得模型)")
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if !pool[i].CreatedAt.Equal(pool[j].CreatedAt) {
			return pool[i].CreatedAt.Before(pool[j].CreatedAt)
		}
		return pool[i].ID < pool[j].ID
	})
	for i := range pool {
		if pool[i].AllowsModel(modelID) {
			return &pool[i], nil
		}
	}
	return nil, domain.NewError(domain.CodeModelNotAllowed,
		"model "+modelID+" is not covered by the effective entitlement")
}

// ---------------------------------------------------------------------------
// 权益修订：升级 / 续费 / 降级（设计 §4.2/§6）
// ---------------------------------------------------------------------------

// Upgrade computes the in-place patch moving an active entitlement to a new
// policy version / model set. The consumption subject (ID), the ORIGINAL
// anchor and the effective range are untouched; because quota windows key
// on the entitlement ID, accumulated used/reserved carry over (升级沿用同
// 一消费主体与已有窗口，更新限额不清空 used/reserved). A no-op upgrade
// (same policy and same model set) is rejected as an operator error.
func Upgrade(ent *domain.Entitlement, newPolicyVersionID string, newModelIDs []string) (domain.EntitlementPatch, error) {
	if ent == nil {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput, "entitlement: nil")
	}
	if ent.Status != domain.EntitlementActive {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeConflict,
			"entitlement not active: "+string(ent.Status))
	}
	if newPolicyVersionID == "" {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput, "entitlement: policy version required")
	}
	models := dedupeModels(newModelIDs)
	if newPolicyVersionID == ent.PolicyVersionID && equalModelSets(models, ent.ModelIDs) {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput,
			"entitlement: upgrade without change is a no-op")
	}
	return domain.EntitlementPatch{
		ExpectedRevision: ent.Revision,
		PolicyVersionID:  &newPolicyVersionID,
		ModelIDs:         models,
	}, nil
}

// Renew extends the effective range (续费延长有效期，不提前重置窗口). The
// anchor and the windows are untouched — no early reset; the new end must
// be strictly after the current one. An open-ended entitlement has nothing
// to extend.
func Renew(ent *domain.Entitlement, newEffectiveTo time.Time) (domain.EntitlementPatch, error) {
	if ent == nil {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput, "entitlement: nil")
	}
	if ent.Status != domain.EntitlementActive {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeConflict,
			"entitlement not active: "+string(ent.Status))
	}
	if ent.EffectiveTo == nil {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput,
			"entitlement: open-ended entitlement has nothing to renew")
	}
	to := newEffectiveTo.UTC()
	if !to.After(ent.EffectiveTo.UTC()) {
		return domain.EntitlementPatch{}, domain.NewError(domain.CodeInvalidInput,
			"entitlement: renewal must extend the effective range")
	}
	return domain.EntitlementPatch{
		ExpectedRevision: ent.Revision,
		EffectiveTo:      &to,
	}, nil
}

// DowngradeEffectiveAt computes when a downgrade takes effect: the end of
// the current monthly (agreed) period derived from the ORIGINAL anchor —
// never mid-period (设计 §4.2: 降级在下个约定周期生效). The caller applies
// the change through the ordinary upgrade patch when that instant arrives.
func DowngradeEffectiveAt(ent *domain.Entitlement, at time.Time) (time.Time, error) {
	if ent == nil {
		return time.Time{}, domain.NewError(domain.CodeInvalidInput, "entitlement: nil")
	}
	iv, _, err := quota.MonthlyWindowAt(ent.AnchorAt, at)
	if err != nil {
		return time.Time{}, err
	}
	return iv.End, nil
}

func equalModelSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, id := range a {
		seen[id]++
	}
	for _, id := range b {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	return true
}
