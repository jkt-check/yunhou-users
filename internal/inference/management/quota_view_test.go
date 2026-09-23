package management

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

// quota_view_test.go — 客户配额视图的纯规则测试（注入时钟 + 内存 store，
// 不依赖 DB）：权益选择优先序、阻断原因（多窗口/权益过期）、禁用与未激活
// 窗口、remaining 计算。

func micro(v int64) *domain.Microcredit { m := domain.Microcredit(v); return &m }

var qvNow = time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)

type fakeQuotaStore struct {
	account    *domain.BillingAccount
	accountErr error
	ents       []domain.Entitlement
	windows    []domain.QuotaWindow
	policy     quota.Policy
	policyErr  error
}

func (f *fakeQuotaStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	return f.account, nil
}

func (f *fakeQuotaStore) ListEntitlements(context.Context, string) ([]domain.Entitlement, error) {
	return f.ents, nil
}

func (f *fakeQuotaStore) LoadWindows(context.Context, string, []domain.WindowKind) ([]domain.QuotaWindow, error) {
	return f.windows, nil
}

func (f *fakeQuotaStore) GetQuotaPolicy(context.Context, string) (quota.Policy, error) {
	if f.policyErr != nil {
		return quota.Policy{}, f.policyErr
	}
	return f.policy, nil
}

func notFoundErr() error { return domain.NewError(domain.CodeNotFound, "not found") }

func activeEntitlement(id string, createdAt time.Time) domain.Entitlement {
	return domain.Entitlement{
		ID: id, BillingAccountID: "acct-1",
		SourceType: domain.SourceSubscription, SourceID: "sub-1",
		ModelIDs: []string{"glm-4.6"}, PolicyVersionID: "pol-1",
		AnchorAt:      createdAt.Add(-24 * time.Hour),
		EffectiveFrom: createdAt.Add(-24 * time.Hour),
		Revision:      1, Status: domain.EntitlementActive, CreatedAt: createdAt,
	}
}

func TestSelectViewEntitlement_Precedence(t *testing.T) {
	older := activeEntitlement("ent-a", qvNow.Add(-48*time.Hour))
	newer := activeEntitlement("ent-b", qvNow.Add(-24*time.Hour))
	gift := activeEntitlement("ent-g", qvNow.Add(-12*time.Hour))
	gift.SourceType, gift.SourceID = domain.SourceGrant, "bundle:sub-kaya"

	// 显式套餐优先于赠送；同类取最早创建（与调用路径同一优先序）。
	got := SelectViewEntitlement([]domain.Entitlement{gift, newer, older}, qvNow)
	if got == nil || got.ID != "ent-a" {
		t.Fatalf("active precedence: got %+v", got)
	}

	// 全部停用时：取最近创建的显式停用行（过期展示最近套餐状态）。
	expA, expB := older, newer
	expToA, expToB := qvNow.Add(-time.Hour), qvNow.Add(-30*time.Minute)
	expA.EffectiveTo, expB.EffectiveTo = &expToA, &expToB
	// 更早的时刻两条都仍可用 → 最早创建的显式权益胜出（与调用路径一致）。
	got = SelectViewEntitlement([]domain.Entitlement{expA, expB, gift}, qvNow.Add(-2*time.Hour))
	if got == nil || got.ID != "ent-a" {
		t.Fatalf("usable at earlier time: got %+v", got)
	}
	// 显式全停用时赠送仍在 → 赠送参与（无显式时兜底展示）；赠送也停用后
	// → 最近创建的显式停用行。
	got = SelectViewEntitlement([]domain.Entitlement{expA, expB, gift}, qvNow)
	if got == nil || got.ID != "ent-g" {
		t.Fatalf("gift participates when no explicit usable: got %+v", got)
	}
	retiredGift := gift
	giftTo := qvNow.Add(-10 * time.Minute)
	retiredGift.EffectiveTo = &giftTo
	got = SelectViewEntitlement([]domain.Entitlement{expA, expB, retiredGift}, qvNow)
	if got == nil || got.ID != "ent-b" {
		t.Fatalf("retired precedence (newest explicit): got %+v", got)
	}

	// 无任何行 → nil。
	if got := SelectViewEntitlement(nil, qvNow); got != nil {
		t.Fatalf("empty: got %+v", got)
	}
}

func TestEntitlementBlock_States(t *testing.T) {
	active := activeEntitlement("e1", qvNow.Add(-24*time.Hour))
	if _, ok := EntitlementBlock(&active, qvNow); ok {
		t.Error("active entitlement must not block")
	}

	// 存储 status 尚未被卫生任务翻转，但 effective_to 已过 → 过期（授权判
	// 定只看 effective_to）。
	past := qvNow.Add(-time.Minute)
	expired := activeEntitlement("e2", qvNow.Add(-24*time.Hour))
	expired.EffectiveTo = &past
	b, ok := EntitlementBlock(&expired, qvNow)
	if !ok || b.Reason != BlockEntitlementExpired || b.ExpiredAt == nil || !b.ExpiredAt.Equal(past) {
		t.Errorf("expired-by-time: %+v ok=%v", b, ok)
	}
	// 卫生翻转后的行同样报过期。
	expired.Status = domain.EntitlementExpired
	if b, ok = EntitlementBlock(&expired, qvNow); !ok || b.Reason != BlockEntitlementExpired {
		t.Errorf("expired-by-status: %+v ok=%v", b, ok)
	}

	revoked := activeEntitlement("e3", qvNow.Add(-24*time.Hour))
	revoked.Status = domain.EntitlementRevoked
	if b, ok = EntitlementBlock(&revoked, qvNow); !ok || b.Reason != BlockEntitlementRevoked {
		t.Errorf("revoked: %+v ok=%v", b, ok)
	}

	future := activeEntitlement("e4", qvNow.Add(time.Hour))
	future.EffectiveFrom = qvNow.Add(time.Hour)
	if b, ok = EntitlementBlock(&future, qvNow); !ok || b.Reason != BlockEntitlementNotYetEffective {
		t.Errorf("not-yet-effective: %+v ok=%v", b, ok)
	}
}

func TestExhaustedWindowBlock(t *testing.T) {
	// 零限额（零额度 fixture）：remaining=0 → 阻断；未激活五小时窗口
	// resets_at 必须为 nil（未知恢复时刻不编造倒计时）。
	zero := quota.WindowView{Kind: domain.WindowFiveHour, Limit: micro(0),
		Activation: quota.ActivationOnFirstConsumption}
	b, ok := ExhaustedWindowBlock(zero)
	if !ok || b.Reason != BlockQuotaExhausted || b.Window == nil || b.Window.ResetsAt != nil {
		t.Fatalf("zero-limit unactivated: %+v ok=%v", b, ok)
	}

	// 耗尽的活动窗口：resets_at = 窗口末。
	end := qvNow.Add(time.Hour)
	ex := quota.WindowView{Kind: domain.WindowFiveHour, Limit: micro(100),
		Used: 100, ResetsAt: &end}
	b, ok = ExhaustedWindowBlock(ex)
	if !ok || b.Window.ResetsAt == nil || !b.Window.ResetsAt.Equal(end) {
		t.Fatalf("exhausted live: %+v ok=%v", b, ok)
	}

	// 有剩余/禁用窗口不阻断。
	ok = false
	if _, ok = ExhaustedWindowBlock(quota.WindowView{Kind: domain.WindowWeekly, Limit: micro(100), Used: 50}); ok {
		t.Error("partial window must not block")
	}
	if _, ok = ExhaustedWindowBlock(quota.WindowView{Kind: domain.WindowMonthly, Disabled: true}); ok {
		t.Error("disabled window must not block")
	}

	// Remaining：max(0, limit-used-reserved)；禁用为 nil。
	v := quota.WindowView{Kind: domain.WindowWeekly, Limit: micro(100), Used: 60, Reserved: 50}
	if r := Remaining(v); r == nil || *r != 0 {
		t.Errorf("remaining clamp: %v", r)
	}
	if r := Remaining(quota.WindowView{Kind: domain.WindowWeekly, Disabled: true}); r != nil {
		t.Errorf("disabled remaining must be nil, got %v", *r)
	}
}

func TestQuotaView_Get(t *testing.T) {
	anchor := qvNow.Add(-48 * time.Hour)
	policy := quota.Policy{
		FiveHourLimit: micro(1_000_000), WeeklyLimit: micro(10_000_000),
		MonthlyLimit: micro(100_000_000),
	}

	t.Run("no account → empty view with no_active_entitlement", func(t *testing.T) {
		svc := NewQuotaViewService(&fakeQuotaStore{accountErr: notFoundErr()}, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-x")
		if err != nil {
			t.Fatal(err)
		}
		if v.Entitlement != nil || len(v.Windows) != 0 {
			t.Errorf("view = %+v", v)
		}
		if len(v.BlockedBy) != 1 || v.BlockedBy[0].Reason != BlockNoActiveEntitlement {
			t.Errorf("blocked_by = %+v", v.BlockedBy)
		}
		if !v.AsOf.Equal(qvNow) || v.Unit != UnitMicrocredit {
			t.Errorf("as_of/unit = %v %v", v.AsOf, v.Unit)
		}
	})

	t.Run("active entitlement: windows + exhausted block", func(t *testing.T) {
		ent := activeEntitlement("ent-1", anchor.Add(24*time.Hour))
		// weekly 窗口耗尽（used=limit）；five_hour 未激活；monthly 有剩余。
		weekIv, _, err := quota.WeeklyWindowAt(ent.AnchorAt, qvNow)
		if err != nil {
			t.Fatal(err)
		}
		store := &fakeQuotaStore{
			account: &domain.BillingAccount{ID: "acct-1", UserID: "user-1", Status: "active"},
			ents:    []domain.Entitlement{ent},
			windows: []domain.QuotaWindow{{
				ID: "w-week", EntitlementID: ent.ID, Kind: domain.WindowWeekly,
				Start: weekIv.Start, End: weekIv.End,
				Limit: 10_000_000, Used: 10_000_000,
			}},
			policy: policy,
		}
		svc := NewQuotaViewService(store, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if v.Entitlement == nil || v.Entitlement.ID != "ent-1" {
			t.Fatalf("entitlement = %+v", v.Entitlement)
		}
		if len(v.Windows) != 3 {
			t.Fatalf("windows = %d, want 3", len(v.Windows))
		}
		// five_hour 未激活：无窗口边界 + activation 提示（读取不激活）。
		fh := v.Windows[0]
		if fh.Kind != domain.WindowFiveHour || fh.WindowStart != nil || fh.ResetsAt != nil ||
			fh.Activation != quota.ActivationOnFirstConsumption {
			t.Errorf("five_hour view = %+v", fh)
		}
		if r := Remaining(fh); r == nil || *r != 1_000_000 {
			t.Errorf("five_hour remaining = %v", r)
		}
		// 阻断只来自 weekly（多窗口阻断时全部返回 —— 此处恰一个）。
		if len(v.BlockedBy) != 1 || v.BlockedBy[0].Kind != "weekly" || v.BlockedBy[0].Reason != BlockQuotaExhausted {
			t.Fatalf("blocked_by = %+v", v.BlockedBy)
		}
		if v.BlockedBy[0].Window.ResetsAt == nil || !v.BlockedBy[0].Window.ResetsAt.Equal(weekIv.End) {
			t.Errorf("weekly block resets_at = %v, want %v", v.BlockedBy[0].Window.ResetsAt, weekIv.End)
		}
	})

	t.Run("multi-window block returns every reason", func(t *testing.T) {
		ent := activeEntitlement("ent-1", anchor.Add(24*time.Hour))
		weekIv, _, _ := quota.WeeklyWindowAt(ent.AnchorAt, qvNow)
		monthIv, _, _ := quota.MonthlyWindowAt(ent.AnchorAt, qvNow)
		fhStart := qvNow.Add(-time.Hour)
		store := &fakeQuotaStore{
			account: &domain.BillingAccount{ID: "acct-1", UserID: "user-1", Status: "active"},
			ents:    []domain.Entitlement{ent},
			windows: []domain.QuotaWindow{
				{ID: "w-5h", EntitlementID: ent.ID, Kind: domain.WindowFiveHour,
					Start: fhStart, End: fhStart.Add(5 * time.Hour), Limit: 1_000_000, Used: 1_000_000},
				{ID: "w-week", EntitlementID: ent.ID, Kind: domain.WindowWeekly,
					Start: weekIv.Start, End: weekIv.End, Limit: 10_000_000, Used: 10_000_000},
				{ID: "w-month", EntitlementID: ent.ID, Kind: domain.WindowMonthly,
					Start: monthIv.Start, End: monthIv.End, Limit: 100_000_000, Used: 500},
			},
			policy: policy,
		}
		svc := NewQuotaViewService(store, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(v.BlockedBy) != 2 {
			t.Fatalf("blocked_by = %+v, want 2 window blocks", v.BlockedBy)
		}
		if v.BlockedBy[0].Kind != "five_hour" || v.BlockedBy[1].Kind != "weekly" {
			t.Errorf("block kinds = %s,%s", v.BlockedBy[0].Kind, v.BlockedBy[1].Kind)
		}
		for _, b := range v.BlockedBy {
			if b.Window == nil || b.Window.ResetsAt == nil {
				t.Errorf("block missing window detail: %+v", b)
			}
		}
	})

	t.Run("expired entitlement: entitlement block + factual windows", func(t *testing.T) {
		ent := activeEntitlement("ent-1", anchor.Add(24*time.Hour))
		past := qvNow.Add(-time.Hour)
		ent.EffectiveTo = &past
		ent.Status = domain.EntitlementExpired
		weekIv, _, _ := quota.WeeklyWindowAt(ent.AnchorAt, qvNow)
		store := &fakeQuotaStore{
			account: &domain.BillingAccount{ID: "acct-1", UserID: "user-1", Status: "active"},
			ents:    []domain.Entitlement{ent},
			windows: []domain.QuotaWindow{{
				ID: "w-week", EntitlementID: ent.ID, Kind: domain.WindowWeekly,
				Start: weekIv.Start, End: weekIv.End, Limit: 10_000_000, Used: 42,
			}},
			policy: policy,
		}
		svc := NewQuotaViewService(store, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(v.BlockedBy) != 1 || v.BlockedBy[0].Kind != BlockKindEntitlement ||
			v.BlockedBy[0].Reason != BlockEntitlementExpired {
			t.Fatalf("blocked_by = %+v", v.BlockedBy)
		}
		// 权益过期但窗口如实展示存储状态（used=42 保留可见）。
		if v.Windows[1].Used != 42 {
			t.Errorf("weekly used = %v, want 42 (已用额度不因过期隐藏)", v.Windows[1].Used)
		}
	})

	t.Run("suspended account adds account block", func(t *testing.T) {
		ent := activeEntitlement("ent-1", anchor.Add(24*time.Hour))
		store := &fakeQuotaStore{
			account: &domain.BillingAccount{ID: "acct-1", UserID: "user-1", Status: "suspended"},
			ents:    []domain.Entitlement{ent},
			policy:  policy,
		}
		svc := NewQuotaViewService(store, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, b := range v.BlockedBy {
			if b.Kind == BlockKindAccount && b.Reason == BlockAccountNotActive {
				found = true
			}
		}
		if !found {
			t.Errorf("blocked_by = %+v, want account_not_active", v.BlockedBy)
		}
	})

	t.Run("disabled window is explicit, never unlimited", func(t *testing.T) {
		ent := activeEntitlement("ent-1", anchor.Add(24*time.Hour))
		store := &fakeQuotaStore{
			account: &domain.BillingAccount{ID: "acct-1", UserID: "user-1", Status: "active"},
			ents:    []domain.Entitlement{ent},
			policy: quota.Policy{
				FiveHourLimit: micro(1_000_000), WeeklyLimit: nil, // 禁用
				MonthlyLimit: micro(100_000_000),
			},
		}
		svc := NewQuotaViewService(store, domain.FixedClock{T: qvNow})
		v, err := svc.Get(context.Background(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		w := v.Windows[1]
		if w.Kind != domain.WindowWeekly || !w.Disabled || w.Limit != nil {
			t.Errorf("disabled weekly view = %+v", w)
		}
		if Remaining(w) != nil {
			t.Error("disabled window must have null remaining")
		}
		if len(v.BlockedBy) != 0 {
			t.Errorf("disabled window must not block: %+v", v.BlockedBy)
		}
	})
}
