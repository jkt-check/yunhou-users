package access

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// entitlement_test.go — 权益映射、来源选择与修订规则（内存 fake store，
// 不依赖 DB；真实库行为在 internal/inference/postgres 覆盖）。

func entUTC(y int, m time.Month, d, hh int) time.Time {
	return time.Date(y, m, d, hh, 0, 0, 0, time.UTC)
}

func mkEnt(src domain.EntitlementSource, srcID string, models []string, createdAt time.Time) domain.Entitlement {
	return domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: "acct-1",
		SourceType: src, SourceID: srcID, ModelIDs: models,
		PolicyVersionID: "pol-1", AnchorAt: createdAt, EffectiveFrom: createdAt,
		Revision: 1, Status: domain.EntitlementActive, CreatedAt: createdAt,
	}
}

func TestGrantFromPlan(t *testing.T) {
	from := entUTC(2026, 1, 31, 10)
	to := from.AddDate(1, 0, 0) // 年付

	ent, err := GrantFromPlan(PlanRevision{
		SourceType: domain.SourceSubscription, SourceID: "sub-1",
		ModelIDs: []string{"glm-4.6", "glm-4.6", "kimi-k2"}, PolicyVersionID: "pol-1",
	}, time.Time{}, from, &to)
	if err != nil {
		t.Fatal(err)
	}
	// 零锚点默认取生效时刻；修订从 1 起；赠送默认不叠加语义：stackable 恒 false。
	if !ent.AnchorAt.Equal(from) || ent.Revision != 1 || ent.Stackable {
		t.Errorf("grant = anchor %v rev %d stackable %v", ent.AnchorAt, ent.Revision, ent.Stackable)
	}
	if ent.Status != domain.EntitlementActive || !ent.EffectiveTo.Equal(to) {
		t.Errorf("grant = status %v to %v", ent.Status, ent.EffectiveTo)
	}
	// 模型集合去重保序。
	if len(ent.ModelIDs) != 2 || ent.ModelIDs[0] != "glm-4.6" || ent.ModelIDs[1] != "kimi-k2" {
		t.Errorf("models = %v, want deduped [glm-4.6 kimi-k2]", ent.ModelIDs)
	}

	// 显式空集合 = 不授权任何模型，永非"全部放行"。
	ent, err = GrantFromPlan(PlanRevision{
		SourceType: domain.SourceGrant, SourceID: "gift-1", PolicyVersionID: "pol-g",
	}, time.Time{}, from, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ent.ModelIDs == nil || len(ent.ModelIDs) != 0 || ent.AllowsModel("glm-4.6") {
		t.Errorf("empty model set must grant nothing: %+v", ent.ModelIDs)
	}

	// 校验矩阵。
	bad := []PlanRevision{
		{SourceType: "mystery", SourceID: "x", PolicyVersionID: "p"},
		{SourceType: domain.SourceOrder, SourceID: "", PolicyVersionID: "p"},
		{SourceType: domain.SourceOrder, SourceID: "x", PolicyVersionID: ""},
	}
	for i, b := range bad {
		if _, err := GrantFromPlan(b, time.Time{}, from, nil); domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("bad[%d]: err = %v, want invalid_input", i, err)
		}
	}
	// 锚点晚于生效时刻 / 结束不晚于开始均拒绝。
	if _, err := GrantFromPlan(PlanRevision{SourceType: domain.SourceOrder, SourceID: "x", PolicyVersionID: "p"},
		from.Add(time.Hour), from, nil); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("anchor after from: %v", err)
	}
	if _, err := GrantFromPlan(PlanRevision{SourceType: domain.SourceOrder, SourceID: "x", PolicyVersionID: "p"},
		time.Time{}, from, &from); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("to == from: %v", err)
	}
}

func TestSelectEntitlement_ExplicitWinsGiftFallbackOnly(t *testing.T) {
	at := entUTC(2026, 9, 8, 12)
	early := entUTC(2026, 9, 1, 0)
	later := entUTC(2026, 9, 5, 0)

	explicit := mkEnt(domain.SourceSubscription, "sub-1", []string{"glm-4.6"}, later)
	gift := mkEnt(domain.SourceGrant, "gift-1", []string{"glm-4.6", "kimi-k2"}, early)

	// 显式套餐优先：即使赠送更早建、模型更多，也选显式套餐。
	got, err := SelectEntitlement([]domain.Entitlement{gift, explicit}, "glm-4.6", at)
	if err != nil || got.SourceID != "sub-1" {
		t.Errorf("explicit must win: %v %v", got, err)
	}

	// 账户存在显式套餐时赠送被整体抑制（默认不叠加/不自动兜底）：
	// 请求只有赠送覆盖的 kimi-k2 → 拒绝，不回退到赠送。
	if _, err := SelectEntitlement([]domain.Entitlement{gift, explicit}, "kimi-k2", at); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("gift must not auto-fallback beside an explicit plan: %v", err)
	}

	// 无显式套餐时赠送生效。
	got, err = SelectEntitlement([]domain.Entitlement{gift}, "kimi-k2", at)
	if err != nil || got.SourceID != "gift-1" {
		t.Errorf("gift applies when no explicit plan: %v %v", got, err)
	}

	// 多份赠送不叠加：确定性地取最早创建的一份。
	gift2 := mkEnt(domain.SourceGrant, "gift-2", []string{"kimi-k2"}, later)
	got, err = SelectEntitlement([]domain.Entitlement{gift2, gift}, "kimi-k2", at)
	if err != nil || got.SourceID != "gift-1" {
		t.Errorf("gifts never stack; earliest wins: %v %v", got, err)
	}

	// 既有会员无任何显式 grant → 不自动获得全部模型。
	if _, err := SelectEntitlement(nil, "glm-4.6", at); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("legacy member without grant: %v, want model_not_allowed", err)
	}

	// 有权益但模型不在显式集合内 → 拒绝。
	if _, err := SelectEntitlement([]domain.Entitlement{explicit}, "other-model", at); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("model outside set: %v", err)
	}
}

func TestSelectEntitlement_ActiveWindowSemantics(t *testing.T) {
	from := entUTC(2026, 9, 1, 0)
	to := entUTC(2026, 10, 1, 0)
	e := mkEnt(domain.SourceSubscription, "sub-1", []string{"glm-4.6"}, from)
	e.EffectiveTo = &to

	// [from, to)：from 活跃，to 已到边界 → 不活跃（设计 §6 区间语义一致）。
	if _, err := SelectEntitlement([]domain.Entitlement{e}, "glm-4.6", from); err != nil {
		t.Errorf("at effective_from must be active: %v", err)
	}
	if _, err := SelectEntitlement([]domain.Entitlement{e}, "glm-4.6", to); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("at effective_to must be inactive: %v", err)
	}
	if _, err := SelectEntitlement([]domain.Entitlement{e}, "glm-4.6", from.Add(-time.Second)); domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("before effective_from must be inactive: %v", err)
	}

	// 状态过滤：superseded/revoked/expired 不参与。
	for _, st := range []domain.EntitlementStatus{domain.EntitlementSuperseded, domain.EntitlementRevoked, domain.EntitlementExpired} {
		e.Status = st
		if _, err := SelectEntitlement([]domain.Entitlement{e}, "glm-4.6", entUTC(2026, 9, 8, 0)); domain.CodeOf(err) != domain.CodeModelNotAllowed {
			t.Errorf("status %s must not participate: %v", st, err)
		}
	}
}

func TestEntitlementResolver_ViaStore(t *testing.T) {
	fs := newFakeStore()
	account, err := fs.EnsureBillingAccount(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	now := entUTC(2026, 9, 8, 12)
	gift := mkEnt(domain.SourceGrant, "gift-1", []string{"kimi-k2"}, entUTC(2026, 9, 1, 0))
	gift.BillingAccountID = account.ID
	fs.ents = append(fs.ents, gift)

	r := NewEntitlementResolver(fs, domain.FixedClock{T: now})
	// 零时刻走注入时钟（服务端计量时间）。
	got, err := r.Resolve(context.Background(), account.ID, "kimi-k2", time.Time{})
	if err != nil || got.SourceID != "gift-1" {
		t.Errorf("resolve: %v %v", got, err)
	}
	// 缺参校验。
	if _, err := r.Resolve(context.Background(), "", "kimi-k2", now); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("empty account: %v", err)
	}
	// 接口契约：实现 domain.EntitlementResolver。
	var _ domain.EntitlementResolver = r
}

func TestUpgrade_KeepsConsumptionSubject(t *testing.T) {
	anchor := entUTC(2026, 1, 31, 10)
	to := entUTC(2027, 1, 31, 10)
	ent := mkEnt(domain.SourceSubscription, "sub-1", []string{"glm-4.6"}, anchor)
	ent.AnchorAt = anchor
	ent.EffectiveTo = &to
	ent.Revision = 3

	patch, err := Upgrade(&ent, "pol-2", []string{"glm-4.6", "kimi-k2"})
	if err != nil {
		t.Fatal(err)
	}
	// 补丁只携带新策略/模型集 + 乐观锁修订；ID/锚点/有效期不在补丁里
	// （同一消费主体，升级不清空 used/reserved —— 窗口键于 entitlement ID）。
	if patch.ExpectedRevision != 3 || patch.PolicyVersionID == nil || *patch.PolicyVersionID != "pol-2" {
		t.Errorf("patch = %+v", patch)
	}
	if len(patch.ModelIDs) != 2 || patch.EffectiveTo != nil {
		t.Errorf("patch = %+v, want models replaced / effective range untouched", patch)
	}

	// 无变化升级是运营错误。
	if _, err := Upgrade(&ent, "pol-1", []string{"glm-4.6"}); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("no-op upgrade: %v, want invalid_input", err)
	}
	// 非活跃权益不可升级。
	ent.Status = domain.EntitlementRevoked
	if _, err := Upgrade(&ent, "pol-2", nil); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("revoked upgrade: %v, want conflict", err)
	}
}

func TestRenew_ExtendsWithoutReset(t *testing.T) {
	anchor := entUTC(2026, 1, 31, 10)
	to := entUTC(2026, 9, 30, 10)
	ent := mkEnt(domain.SourceSubscription, "sub-1", []string{"glm-4.6"}, anchor)
	ent.AnchorAt = anchor
	ent.EffectiveTo = &to
	ent.Revision = 2

	// 续费延长有效期：补丁只动 effective_to；锚点与窗口（键于 ID）不动，
	// 不提前重置窗口。
	patch, err := Renew(&ent, entUTC(2027, 9, 30, 10))
	if err != nil {
		t.Fatal(err)
	}
	if patch.EffectiveTo == nil || !patch.EffectiveTo.Equal(entUTC(2027, 9, 30, 10)) {
		t.Errorf("patch = %+v", patch)
	}
	if patch.PolicyVersionID != nil || patch.ModelIDs != nil || patch.ExpectedRevision != 2 {
		t.Errorf("renewal must not touch policy/models: %+v", patch)
	}

	// 不延长（相等/提前）拒绝；无固定结束的权益无从续费。
	if _, err := Renew(&ent, to); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("same end: %v", err)
	}
	if _, err := Renew(&ent, to.Add(-time.Hour)); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("shorter end: %v", err)
	}
	ent.EffectiveTo = nil
	if _, err := Renew(&ent, entUTC(2027, 1, 1, 0)); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("open-ended renew: %v", err)
	}
}

// TestDowngradeEffectiveAt 钉牢"降级在下个约定周期生效"：取当前公历月配额
// 周期的结束时刻（原始锚点推进），月中降级绝不立即生效。
func TestDowngradeEffectiveAt(t *testing.T) {
	anchor := entUTC(2026, 1, 31, 10)
	ent := mkEnt(domain.SourceSubscription, "sub-1", []string{"glm-4.6"}, anchor)
	ent.AnchorAt = anchor

	// 2 月 15 日申请降级 → 当前月周期 [Jan31, Feb28) 结束即 Feb 28 10:00。
	got, err := DowngradeEffectiveAt(&ent, entUTC(2026, 2, 15, 12))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(entUTC(2026, 2, 28, 10)) {
		t.Errorf("effective at %v, want Feb 28 10:00 (2 月裁剪边界)", got)
	}

	// 3 月 1 日 → 当前周期 [Feb28, Mar31) → Mar 31（恢复原始锚点日）。
	got, err = DowngradeEffectiveAt(&ent, entUTC(2026, 3, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(entUTC(2026, 3, 31, 10)) {
		t.Errorf("effective at %v, want Mar 31 10:00", got)
	}

	// 恰在周期边界 → 已是新周期，生效于下个周期末。
	got, err = DowngradeEffectiveAt(&ent, entUTC(2026, 3, 31, 10))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(entUTC(2026, 4, 30, 10)) {
		t.Errorf("boundary: effective at %v, want Apr 30 10:00", got)
	}
}
