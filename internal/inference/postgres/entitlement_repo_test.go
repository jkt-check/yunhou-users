package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// entitlement_repo_test.go — inference_entitlements 真实库行为（Task 6）：
// 显式套餐优先的排序、有效期 [from,to)、in-place 修订（升级不清空
// used/reserved；续费不提前重置窗口）、乐观锁修订。

func seedEntitlement(t *testing.T, s *Store, f fixture, src domain.EntitlementSource, srcID string, models []string, anchor time.Time, to *time.Time) domain.Entitlement {
	t.Helper()
	ent := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: src, SourceID: srcID,
		ModelIDs: models, PolicyVersionID: f.policyID,
		AnchorAt: anchor, EffectiveFrom: anchor, EffectiveTo: to,
	}
	if err := s.InsertEntitlement(context.Background(), ent); err != nil {
		t.Fatalf("insert entitlement %s: %v", srcID, err)
	}
	return *ent
}

func TestListActiveEntitlements_OrderingAndRange(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false) // f.entID：订阅来源，2026-09-08 04:00 起

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	gift := seedEntitlement(t, s, f, domain.SourceGrant, "gift-"+uuid.NewString(), []string{f.modelID}, base, nil)
	seedEntitlement(t, s, f, domain.SourceSubscription, "sub-ended-"+uuid.NewString(), []string{f.modelID}, base, &to) // 查询时刻已结束

	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	ents, err := s.ListActiveEntitlements(ctx, f.accountID, at)
	if err != nil {
		t.Fatal(err)
	}
	// 过期权益不出现；显式订阅（fixture 的 f.entID）排在赠送之前。
	if len(ents) != 2 {
		t.Fatalf("active ents = %d, want 2 (expired excluded)", len(ents))
	}
	if ents[0].ID != f.entID || ents[1].ID != gift.ID {
		t.Errorf("order = %s,%s, want explicit subscription first, gift second", ents[0].ID, ents[1].ID)
	}

	// [from, to)：恰在 effective_to 边界 → 该权益不活跃（此刻 fixture
	// 尚未生效，只剩无结束时间的赠送）。
	ents, err = s.ListActiveEntitlements(ctx, f.accountID, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].ID != gift.ID {
		t.Fatalf("at boundary: active = %v, want only the open-ended gift", ents)
	}
}

// TestReviseEntitlement_UpgradeKeepsConsumption 验收硬项（DB 侧）：同一消费
// 主体升级只升 revision、换策略版本，锚点不变，已有窗口的 used/reserved
// 原样保留 —— 不存在"新 policy ID → 全新空窗口"的逃生门。
func TestReviseEntitlement_UpgradeKeepsConsumption(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 新策略版本（升级目标）。
	bigger := &PolicyVersion{
		Name: "coding-plan-pro", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(5_000_000), WeeklyLimit: micro(50_000_000),
		MonthlyLimit: micro(500_000_000),
	}
	if err := s.InsertPolicyVersion(ctx, bigger); err != nil {
		t.Fatal(err)
	}

	// 已有窗口带累计用量。
	start := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	w := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowFiveHour,
		Start: start, End: start.Add(5 * time.Hour),
		Limit: 1_000_000, Used: 123_456, Reserved: 7_890,
	}
	if err := s.InsertQuotaWindow(ctx, w); err != nil {
		t.Fatal(err)
	}

	ent, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	newPolicy := bigger.ID
	rev, err := s.ReviseEntitlement(ctx, f.entID, domain.EntitlementPatch{
		ExpectedRevision: ent.Revision,
		PolicyVersionID:  &newPolicy,
		ModelIDs:         []string{f.modelID},
	})
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if rev.Revision != ent.Revision+1 || rev.PolicyVersionID != bigger.ID {
		t.Errorf("revised = rev %d policy %s, want rev+1/new policy", rev.Revision, rev.PolicyVersionID)
	}
	// 锚点与消费主体不动。
	if rev.ID != f.entID || !rev.AnchorAt.Equal(ent.AnchorAt) || !rev.EffectiveFrom.Equal(ent.EffectiveFrom) {
		t.Errorf("consumption subject moved: id %s anchor %v from %v", rev.ID, rev.AnchorAt, rev.EffectiveFrom)
	}

	// 窗口行的 used/reserved 原样保留。
	if used, reserved := windowState(t, s, w.ID); used != 123_456 || reserved != 7_890 {
		t.Errorf("window after upgrade: used=%d reserved=%d, want 123456/7890 (升级不清空)", used, reserved)
	}
	var winEnt string
	if err := s.db.Get(&winEnt, `SELECT entitlement_id FROM inference_quota_windows WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if winEnt != f.entID {
		t.Errorf("window owner moved to %s, want same consumption subject %s", winEnt, f.entID)
	}
}

// TestReviseEntitlement_RenewalNoEarlyReset：续费只延长 effective_to，
// 窗口与锚点不动（续费不提前刷新额度）。
func TestReviseEntitlement_RenewalNoEarlyReset(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)
	w5, _, _ := makeWindows(t, s, f.entID)

	ent, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	newEnd := time.Date(2027, 9, 8, 4, 0, 0, 0, time.UTC) // 续一年
	rev, err := s.ReviseEntitlement(ctx, f.entID, domain.EntitlementPatch{
		ExpectedRevision: ent.Revision,
		EffectiveTo:      &newEnd,
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if rev.EffectiveTo == nil || !rev.EffectiveTo.Equal(newEnd) {
		t.Errorf("effective_to = %v, want extended", rev.EffectiveTo)
	}
	if rev.PolicyVersionID != f.policyID || !rev.AnchorAt.Equal(ent.AnchorAt) {
		t.Errorf("renewal must not touch policy/anchor: %+v", rev)
	}
	// 窗口行不变（同 ID、无重置）。
	if used, reserved := windowState(t, s, w5); used != 0 || reserved != 0 {
		t.Errorf("window after renewal: used=%d reserved=%d, want untouched 0/0", used, reserved)
	}
}

func TestReviseEntitlement_ConflictAndNotFound(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// 过期修订 → 乐观锁冲突。
	newPolicy := f.policyID
	_, err := s.ReviseEntitlement(ctx, f.entID, domain.EntitlementPatch{
		ExpectedRevision: 99, PolicyVersionID: &newPolicy,
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale revision: %v, want conflict", err)
	}

	// 未知 ID → NotFound。
	_, err = s.ReviseEntitlement(ctx, uuid.NewString(), domain.EntitlementPatch{
		ExpectedRevision: 1, PolicyVersionID: &newPolicy,
	})
	if domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("unknown id: %v, want not_found", err)
	}

	// 非活跃权益不可修订。
	if _, err := s.db.Exec(`UPDATE inference_entitlements SET status = 'revoked' WHERE id = $1`, f.entID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReviseEntitlement(ctx, f.entID, domain.EntitlementPatch{
		ExpectedRevision: 1, PolicyVersionID: &newPolicy,
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("revoked revise: %v, want conflict", err)
	}
}
