package management

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// subscription_view_test.go — 套餐视图的纯规则测试：grant 来源分类
// （bundle/migration/other）、权益展示排序、来源解析（订阅/捆绑赠送）、
// 与旧会员的命名空间隔离。

var svNow = time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)

type fakeSubStore struct {
	accountErr error
	ents       []domain.Entitlement
	subs       map[string][]ProductSubscription // productCode → rows
}

func (f *fakeSubStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	return &domain.BillingAccount{ID: "acct-1", Status: "active"}, nil
}

func (f *fakeSubStore) ListEntitlements(context.Context, string) ([]domain.Entitlement, error) {
	return f.ents, nil
}

func (f *fakeSubStore) ListProductSubscriptions(_ context.Context, _ string, productCode string) ([]ProductSubscription, error) {
	return f.subs[productCode], nil
}

func TestClassifyGrantSource(t *testing.T) {
	cases := []struct{ in, kind, ref string }{
		{"bundle:sub-42", GrantKindBundle, "sub-42"},
		{"bundle:", GrantKindOther, "bundle:"}, // 空引用不作 bundle
		{"migration:rule-7:user-1", GrantKindMigration, "rule-7"},
		{"migration:rule-only", GrantKindMigration, "rule-only"},
		{"gift-manual-9", GrantKindOther, "gift-manual-9"},
	}
	for _, c := range cases {
		kind, ref := ClassifyGrantSource(c.in)
		if kind != c.kind || ref != c.ref {
			t.Errorf("ClassifyGrantSource(%q) = %q,%q want %q,%q", c.in, kind, ref, c.kind, c.ref)
		}
	}
}

func TestOrderViewEntitlements(t *testing.T) {
	mk := func(id, status string, createdAt time.Time) domain.Entitlement {
		return domain.Entitlement{ID: id, Status: domain.EntitlementStatus(status),
			SourceType: domain.SourceSubscription, SourceID: "s-" + id,
			AnchorAt: createdAt, EffectiveFrom: createdAt, CreatedAt: createdAt}
	}
	items := OrderViewEntitlements([]domain.Entitlement{
		mk("e-old-expired", "expired", svNow.Add(-72*time.Hour)),
		mk("e-active", "active", svNow.Add(-48*time.Hour)),
		mk("e-new-expired", "expired", svNow.Add(-24*time.Hour)),
	})
	if len(items) != 3 || items[0].ID != "e-active" || items[1].ID != "e-new-expired" || items[2].ID != "e-old-expired" {
		t.Fatalf("order = %v", []string{items[0].ID, items[1].ID, items[2].ID})
	}
	// 排序携带来源字段（ResolveSourceView 依赖）。
	if items[0].Source.Type != domain.SourceSubscription || items[0].Source.Reference != "s-e-active" {
		t.Errorf("source carried = %+v", items[0].Source)
	}
}

func TestSubscriptionView_Get(t *testing.T) {
	cpPlanName := "Coding Plan Basic"
	kayaPlanName := "Kaya Monthly"
	cpSub := ProductSubscription{
		ID: "sub-cp-1", PlanID: "cp_basic", PlanName: &cpPlanName,
		ProductCode: ProductCodingPlan, Status: "active",
		StartedAt: svNow.Add(-72 * time.Hour), CreatedAt: svNow.Add(-72 * time.Hour),
	}
	kayaSub := ProductSubscription{
		ID: "sub-kaya-1", PlanID: "monthly", PlanName: &kayaPlanName,
		ProductCode: ProductKayaMembership, Status: "active",
		StartedAt: svNow.Add(-96 * time.Hour), CreatedAt: svNow.Add(-96 * time.Hour),
	}
	expired := svNow.Add(-24 * time.Hour)
	ents := []domain.Entitlement{
		{ID: "e-cp", Status: domain.EntitlementActive, SourceType: domain.SourceSubscription,
			SourceID: "sub-cp-1", ModelIDs: []string{"glm-4.6"}, PolicyVersionID: "pol-1",
			AnchorAt: svNow.Add(-72 * time.Hour), EffectiveFrom: svNow.Add(-72 * time.Hour),
			Revision: 2, CreatedAt: svNow.Add(-72 * time.Hour)},
		{ID: "e-gift", Status: domain.EntitlementExpired, SourceType: domain.SourceGrant,
			SourceID: "bundle:sub-kaya-1", ModelIDs: []string{"glm-4.6"}, PolicyVersionID: "pol-0",
			AnchorAt: svNow.Add(-96 * time.Hour), EffectiveFrom: svNow.Add(-96 * time.Hour),
			EffectiveTo: &expired, Revision: 1, CreatedAt: svNow.Add(-96 * time.Hour)},
		{ID: "e-mig", Status: domain.EntitlementRevoked, SourceType: domain.SourceGrant,
			SourceID: "migration:legacy-v1:user-1", PolicyVersionID: "pol-m",
			AnchorAt: svNow.Add(-200 * time.Hour), EffectiveFrom: svNow.Add(-200 * time.Hour),
			Revision: 1, CreatedAt: svNow.Add(-200 * time.Hour)},
	}
	store := &fakeSubStore{
		ents: ents,
		subs: map[string][]ProductSubscription{
			ProductCodingPlan:     {cpSub},
			ProductKayaMembership: {kayaSub},
		},
	}
	svc := NewSubscriptionViewService(store, domain.FixedClock{T: svNow})
	v, err := svc.Get(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}

	// coding-plan 订阅隔离：列表里只有 coding-plan；kaya 只在命名字段。
	if len(v.Subscriptions) != 1 || v.Subscriptions[0].ID != "sub-cp-1" {
		t.Fatalf("subscriptions = %+v", v.Subscriptions)
	}
	if v.KayaMembership == nil || v.KayaMembership.ID != "sub-kaya-1" {
		t.Fatalf("kaya_membership = %+v", v.KayaMembership)
	}
	for _, s := range v.Subscriptions {
		if s.ProductCode != ProductCodingPlan {
			t.Errorf("非 coding-plan 行混入列表: %+v", s)
		}
	}

	// 权益排序 + 来源解析。
	if len(v.Entitlements) != 3 || v.Entitlements[0].ID != "e-cp" {
		t.Fatalf("entitlements = %+v", v.Entitlements)
	}
	cp := v.Entitlements[0]
	if cp.Source.Type != domain.SourceSubscription || cp.Source.SubscriptionID != "sub-cp-1" ||
		cp.Source.PlanID != "cp_basic" || cp.Source.PlanName == nil || *cp.Source.PlanName != cpPlanName {
		t.Errorf("subscription source = %+v", cp.Source)
	}
	gift := v.Entitlements[1]
	if gift.ID != "e-gift" || gift.Source.Kind != GrantKindBundle ||
		gift.Source.SubscriptionID != "sub-kaya-1" || gift.Source.PlanID != "monthly" {
		t.Errorf("bundle gift source = %+v", gift.Source)
	}
	mig := v.Entitlements[2]
	if mig.ID != "e-mig" || mig.Source.Kind != GrantKindMigration || mig.Source.Reference != "migration:legacy-v1:user-1" {
		t.Errorf("migration gift source = %+v", mig.Source)
	}
	// 任何字段不得携带上游账号标识（结构性保证：视图无上游字段）。
	// —— 由 DTO 形状钉死（entitlementItemJSON 无 upstream 字段）。

	t.Run("user without billing account still sees subscriptions", func(t *testing.T) {
		store2 := &fakeSubStore{accountErr: notFoundErr(), subs: store.subs}
		svc2 := NewSubscriptionViewService(store2, domain.FixedClock{T: svNow})
		v2, err := svc2.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(v2.Subscriptions) != 1 || len(v2.Entitlements) != 0 {
			t.Errorf("view = %+v", v2)
		}
	})

	t.Run("no active kaya membership → null marker", func(t *testing.T) {
		expiredKaya := kayaSub
		expiredKaya.Status = "expired"
		store3 := &fakeSubStore{
			ents: ents,
			subs: map[string][]ProductSubscription{
				ProductCodingPlan:     {cpSub},
				ProductKayaMembership: {expiredKaya},
			},
		}
		svc3 := NewSubscriptionViewService(store3, domain.FixedClock{T: svNow})
		v3, err := svc3.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if v3.KayaMembership != nil {
			t.Errorf("expired kaya membership must not show as active: %+v", v3.KayaMembership)
		}
	})
}
