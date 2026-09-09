// subscription_view.go — 客户模型套餐读模型（Task 11，设计 §9.2
// /user/model-subscriptions）。
//
// Coding Plan（product_code='coding-plan'）的订阅、当前权益与赠送来源的
// 独立视图 —— 与旧会员（kaya-membership）展示分离：旧会员只以一个
// 命名空间字段 kaya_membership 出现（用于解释捆绑赠送的来源），绝不混入
// coding-plan 列表。赠送来源记录到订阅/规则级（bundle:<订阅> /
// migration:<规则>），上游供应商账号永不进入客户视图（不暴露上游账号）。
//
// 跨域只读：subscriptions/plans 行的读取走 postgres 存储的只读面
// （Task 10 outbox_repo.go 的先例），inference_* 表仍只由 inference 模块写。

package management

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// ProductCodingPlan is the commercial product code of the standalone model
// API plan (migration 027). Duplicated here (instead of importing
// internal/model) to keep the inference module's boundary explicit: the
// value is a stable wire constant, not a shared struct dependency.
const ProductCodingPlan = "coding-plan"

// ProductKayaMembership is the legacy membership product code — used ONLY
// for the namespaced kaya_membership marker (与旧会员展示分离).
const ProductKayaMembership = "kaya-membership"

// Grant source kinds (access.BundleGiftSourceID / MigrationGiftSourceID
// 的客户可读分类；上游账号与此无关，永不出现).
const (
	GrantKindBundle    = "bundle_gift"    // bundle:<subscription_id>
	GrantKindMigration = "migration_gift" // migration:<rule_id>:<user_id>
	GrantKindOther     = "other"
)

// ProductSubscription is the customer-facing view of one legacy
// subscription row (any status) joined with its plan's display name.
type ProductSubscription struct {
	ID          string
	PlanID      string
	PlanName    *string // plan 已删除时为 null（行不删除历史订阅的展示）
	ProductCode string
	Status      string
	StartedAt   time.Time
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// SourceView describes where one entitlement came from （记录 grant 来源）.
// Reference is the raw source id — it is the customer's OWN subscription /
// order / gift-rule id, never an upstream account identifier.
type SourceView struct {
	Type domain.EntitlementSource
	// Kind classifies grants: bundle_gift | migration_gift | other;
	// empty for subscription/order sources.
	Kind string
	// Reference is the raw source_id.
	Reference string
	// SubscriptionID is set when the source resolves to one of the user's
	// own subscriptions (explicit plan, or the kaya membership behind a
	// bundle gift).
	SubscriptionID string
	PlanID         string
	PlanName       *string
}

// EntitlementItem is one entitlement in the subscriptions view (any
// status: 当前权益与已停用权益都如实列出).
type EntitlementItem struct {
	ID              string
	Status          domain.EntitlementStatus
	Revision        int
	ModelIDs        []string
	PolicyVersionID string
	AnchorAt        time.Time
	EffectiveFrom   time.Time
	EffectiveTo     *time.Time
	Stackable       bool
	CreatedAt       time.Time
	Source          SourceView
}

// ModelSubscriptionsView is the assembled /user/model-subscriptions read
// model.
type ModelSubscriptionsView struct {
	ServerTime time.Time
	AsOf       time.Time
	// Subscriptions lists the user's coding-plan subscriptions (all
	// statuses, newest first). NEVER contains kaya-membership rows.
	Subscriptions []ProductSubscription
	// Entitlements lists the billing account's entitlements (active first,
	// then newest retired), each with its grant source.
	Entitlements []EntitlementItem
	// KayaMembership is the user's ACTIVE legacy membership, namespaced and
	// separate — it explains bundle gifts; nil when no active membership.
	KayaMembership *ProductSubscription
}

// SubscriptionViewStore is the read surface the subscription view needs;
// satisfied by inference/postgres.Store. The subscription reads are
// cross-domain READ-ONLY (Task 10 先例).
type SubscriptionViewStore interface {
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	ListEntitlements(ctx context.Context, billingAccountID string) ([]domain.Entitlement, error)
	// ListProductSubscriptions returns the user's subscriptions of one
	// product (all statuses, newest first) with plan display names.
	ListProductSubscriptions(ctx context.Context, userID, productCode string) ([]ProductSubscription, error)
}

// SubscriptionViewService assembles the model-subscriptions view.
type SubscriptionViewService struct {
	store SubscriptionViewStore
	clock domain.Clock
}

// NewSubscriptionViewService builds the service; a nil clock uses the
// system clock (UTC).
func NewSubscriptionViewService(store SubscriptionViewStore, clock domain.Clock) *SubscriptionViewService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &SubscriptionViewService{store: store, clock: clock}
}

// ClassifyGrantSource parses a grant source id into its customer-facing
// kind and the referenced object (access.BundleGiftSourceID /
// MigrationGiftSourceID formats). Unknown shapes stay "other" with the
// raw id as reference — the source is still recorded, never dropped.
func ClassifyGrantSource(sourceID string) (kind, reference string) {
	if rest, ok := strings.CutPrefix(sourceID, "bundle:"); ok && rest != "" {
		return GrantKindBundle, rest
	}
	if rest, ok := strings.CutPrefix(sourceID, "migration:"); ok {
		if rule, _, ok2 := strings.Cut(rest, ":"); ok2 && rule != "" {
			return GrantKindMigration, rule
		}
		return GrantKindMigration, rest
	}
	return GrantKindOther, sourceID
}

// Get builds the view for the caller's own account. A user without a
// billing account still sees their coding-plan subscriptions (the
// subscription rows exist independently of the inference account).
func (s *SubscriptionViewService) Get(ctx context.Context, userID string) (*ModelSubscriptionsView, error) {
	now := s.clock.Now().UTC()
	out := &ModelSubscriptionsView{
		ServerTime: now, AsOf: now,
		Subscriptions: []ProductSubscription{}, Entitlements: []EntitlementItem{},
	}
	subs, err := s.store.ListProductSubscriptions(ctx, userID, ProductCodingPlan)
	if err != nil {
		return nil, err
	}
	out.Subscriptions = subs
	kayaSubs, err := s.store.ListProductSubscriptions(ctx, userID, ProductKayaMembership)
	if err != nil {
		return nil, err
	}
	for i := range kayaSubs {
		if kayaSubs[i].Status == "active" {
			m := kayaSubs[i]
			out.KayaMembership = &m
			break
		}
	}

	// Source resolution map: the user's own subscriptions by id (both
	// products — bundle gifts reference the kaya membership row).
	subByID := make(map[string]ProductSubscription, len(subs)+len(kayaSubs))
	for _, sub := range subs {
		subByID[sub.ID] = sub
	}
	for _, sub := range kayaSubs {
		subByID[sub.ID] = sub
	}

	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return out, nil
		}
		return nil, err
	}
	ents, err := s.store.ListEntitlements(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	out.Entitlements = OrderViewEntitlements(ents)
	for i := range out.Entitlements {
		out.Entitlements[i].Source = ResolveSourceView(out.Entitlements[i], subByID)
	}
	return out, nil
}

// OrderViewEntitlements orders entitlements for display: active first,
// then retired rows newest created; stable by id. The source type/id are
// carried into SourceView (subscription resolution happens afterwards in
// ResolveSourceView).
func OrderViewEntitlements(ents []domain.Entitlement) []EntitlementItem {
	out := make([]EntitlementItem, 0, len(ents))
	for _, e := range ents {
		out = append(out, EntitlementItem{
			ID: e.ID, Status: e.Status, Revision: e.Revision,
			ModelIDs: e.ModelIDs, PolicyVersionID: e.PolicyVersionID,
			AnchorAt: e.AnchorAt.UTC(), EffectiveFrom: e.EffectiveFrom.UTC(),
			EffectiveTo: e.EffectiveTo, Stackable: e.Stackable, CreatedAt: e.CreatedAt.UTC(),
			Source: SourceView{Type: e.SourceType, Reference: e.SourceID},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if (a.Status == domain.EntitlementActive) != (b.Status == domain.EntitlementActive) {
			return a.Status == domain.EntitlementActive
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out
}

// ResolveSourceView classifies the entitlement's grant source and resolves
// the linked subscription/plan when it is one of the user's own rows
// (上游账号永不参与). Pure: subByID carries the user's own subscriptions.
func ResolveSourceView(item EntitlementItem, subByID map[string]ProductSubscription) SourceView {
	sv := SourceView{Type: item.Source.Type, Reference: item.Source.Reference}
	switch item.Source.Type {
	case domain.SourceGrant:
		kind, ref := ClassifyGrantSource(item.Source.Reference)
		sv.Kind = kind
		if kind == GrantKindBundle {
			sv.SubscriptionID = ref
			if sub, ok := subByID[ref]; ok {
				sv.PlanID = sub.PlanID
				sv.PlanName = sub.PlanName
			}
		}
	default:
		// subscription / order 显式来源：subscription 源直接解析到套餐。
		if item.Source.Type == domain.SourceSubscription {
			sv.SubscriptionID = item.Source.Reference
			if sub, ok := subByID[item.Source.Reference]; ok {
				sv.PlanID = sub.PlanID
				sv.PlanName = sub.PlanName
			}
		}
	}
	return sv
}
