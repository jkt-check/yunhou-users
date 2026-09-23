package quota

import (
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// policy_test.go — 配额策略纯规则：禁用窗口 ≠ 无限、全窗口同时满足、
// 一次消费镜像三窗口但客户只结算一次。

func microP(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

func testPolicy() Policy {
	return Policy{
		Name: "coding-plan", Revision: 1,
		ModelIDs:      []string{"glm-4.6"},
		FiveHourLimit: microP(1_000_000),
		WeeklyLimit:   microP(10_000_000),
		MonthlyLimit:  microP(100_000_000),
		Overage:       OverageReject,
	}
}

func TestPolicy_ExplicitModelSetOnly(t *testing.T) {
	p := testPolicy()
	if !p.AllowsModel("glm-4.6") {
		t.Error("explicit model must be allowed")
	}
	if p.AllowsModel("other") {
		t.Error("unlisted model must be denied")
	}
	empty := Policy{Name: "gift"}
	if empty.AllowsModel("glm-4.6") {
		t.Error("empty model set must grant NOTHING (no NULL-means-all semantics)")
	}
}

func TestPolicy_DisabledWindowIsNotUnlimited(t *testing.T) {
	p := testPolicy()
	p.FiveHourLimit = nil // 禁用五小时窗口

	if _, ok := p.LimitFor(domain.WindowFiveHour); ok {
		t.Error("nil limit must read as DISABLED, not enabled")
	}
	enabled := p.EnabledWindows()
	if len(enabled) != 2 || enabled[0] != domain.WindowWeekly || enabled[1] != domain.WindowMonthly {
		t.Errorf("enabled windows = %v, want [weekly monthly] in fixed order", enabled)
	}
	// 固定锁序：五小时 → 周 → 月。
	full := testPolicy().EnabledWindows()
	if len(full) != 3 || full[0] != domain.WindowFiveHour || full[1] != domain.WindowWeekly || full[2] != domain.WindowMonthly {
		t.Errorf("enabled order = %v, want five_hour/weekly/monthly", full)
	}
}

func TestEvaluateAdmission(t *testing.T) {
	at := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	mkWindow := func(kind domain.WindowKind, limit, used, reserved int64) domain.QuotaWindow {
		return domain.QuotaWindow{
			Kind: kind, Start: at.Add(-time.Hour), End: at.Add(time.Hour),
			Limit: domain.Microcredit(limit), Used: domain.Microcredit(used),
			Reserved: domain.Microcredit(reserved),
		}
	}
	p := testPolicy()

	// 全部充足 → 放行。
	windows := []domain.QuotaWindow{
		mkWindow(domain.WindowFiveHour, 1_000_000, 100_000, 50_000),
		mkWindow(domain.WindowWeekly, 10_000_000, 0, 0),
		mkWindow(domain.WindowMonthly, 100_000_000, 0, 0),
	}
	if err := EvaluateAdmission(p, windows, 500_000, at); err != nil {
		t.Errorf("admission: %v", err)
	}

	// 可用 = max(0, limit-used-reserved)：月窗口余 40 万 < 50 万 → 阻断，
	// 且阻断详情带可计算的恢复时刻（窗口 End）。
	windows[2].Used = 99_600_000
	err := EvaluateAdmission(p, windows, 500_000, at)
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if len(qe.BlockedBy) != 1 || qe.BlockedBy[0].Kind != domain.WindowMonthly {
		t.Errorf("blocked by %+v, want monthly only", qe.BlockedBy)
	}
	if qe.BlockedBy[0].ResetsAt == nil || !qe.BlockedBy[0].ResetsAt.Equal(at.Add(time.Hour)) {
		t.Errorf("resets_at = %v, want window end", qe.BlockedBy[0].ResetsAt)
	}

	// 多窗口同时不足 → 一次报全（设计 §9.1）。
	windows[0].Used = 999_999 // 五小时余 1
	err = EvaluateAdmission(p, windows, 500_000, at)
	if !errors.As(err, &qe) || len(qe.BlockedBy) != 2 {
		t.Fatalf("err = %v, want 2 blocks", err)
	}
	if qe.BlockedBy[0].Kind != domain.WindowFiveHour || qe.BlockedBy[1].Kind != domain.WindowMonthly {
		t.Errorf("blocks order = %+v, want fixed order five_hour,monthly", qe.BlockedBy)
	}

	// 超限到负：可用钳到 0 而不是负数。
	windows[1].Used = 20_000_000 // 超出 limit
	err = EvaluateAdmission(p, windows, 1, at)
	if !errors.As(err, &qe) {
		t.Errorf("over-used window must block any hold: %v", err)
	}
}

func TestEvaluateAdmission_FailsClosedOnUnresolvedWindow(t *testing.T) {
	at := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	p := testPolicy()

	// 启用窗口缺行 = 调用方 bug，fail closed（不能当无限额度）。
	err := EvaluateAdmission(p, nil, 1, at)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("err = %v, want invalid_input", err)
	}

	// 已过期窗口（边界相等属新窗口）不匹配 → 同样缺行。
	expired := domain.QuotaWindow{
		Kind: domain.WindowFiveHour, Start: at.Add(-5 * time.Hour), End: at,
		Limit: 1_000_000,
	}
	err = EvaluateAdmission(p, []domain.QuotaWindow{expired}, 1, at)
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("boundary-expired window: err = %v, want invalid_input", err)
	}

	// 非法 hold。
	full := []domain.QuotaWindow{
		{Kind: domain.WindowFiveHour, Start: at.Add(-time.Hour), End: at.Add(time.Hour), Limit: 1_000_000},
		{Kind: domain.WindowWeekly, Start: at.Add(-time.Hour), End: at.Add(time.Hour), Limit: 10_000_000},
		{Kind: domain.WindowMonthly, Start: at.Add(-time.Hour), End: at.Add(time.Hour), Limit: 100_000_000},
	}
	if err := EvaluateAdmission(p, full, 0, at); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("zero hold: err = %v, want invalid_input", err)
	}

	// 禁用窗口不要求行。
	p.FiveHourLimit = nil
	if err := EvaluateAdmission(p, full[1:], 1, at); err != nil {
		t.Errorf("disabled five_hour must not require a window row: %v", err)
	}
}

// TestSettlementPlan_OneConsumptionMirrorsAllWindows 钉牢规则侧语义：
// 同一笔客户消费镜像进全部适用窗口的 used，但客户只结算一次 —— 增量
// 相等，绝不是按窗口数翻倍。（事务语义由 Task 7/9 实现并在真实库断言
// ledger 恰一条 charge。）
func TestSettlementPlan_OneConsumptionMirrorsAllWindows(t *testing.T) {
	charge := domain.Microcredit(60_000)
	plan := SettlementPlan(charge, domain.WindowOrder)
	if len(plan) != 3 {
		t.Fatalf("plan covers %d windows, want 3", len(plan))
	}
	for _, kind := range domain.WindowOrder {
		if plan[kind] != charge {
			t.Errorf("window %s delta = %d, want mirrored %d (not multiplied)", kind, plan[kind], charge)
		}
	}
	// 客户侧只结算一份：镜像总额 / 窗口数 == 单次消费额。
	var mirrored domain.Microcredit
	for _, v := range plan {
		mirrored += v
	}
	if mirrored/domain.Microcredit(len(plan)) != charge {
		t.Errorf("customer settlement would be %d per window, want exactly one charge of %d",
			mirrored/domain.Microcredit(len(plan)), charge)
	}
}

func TestViews_FixedOrderAndDisabledMarking(t *testing.T) {
	anchor := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	at := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	p := testPolicy()
	p.WeeklyLimit = nil // 禁用周窗口

	views, err := Views(p, anchor, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 {
		t.Fatalf("views = %d, want 3 (fixed order)", len(views))
	}
	if views[0].Kind != domain.WindowFiveHour || views[1].Kind != domain.WindowWeekly || views[2].Kind != domain.WindowMonthly {
		t.Errorf("order = %v/%v/%v", views[0].Kind, views[1].Kind, views[2].Kind)
	}
	// 五小时未激活：nulls + 提示；周窗口禁用：显式标识；月窗口：锚定周期。
	if views[0].Activation != ActivationOnFirstConsumption || views[0].WindowStart != nil {
		t.Errorf("five_hour view: %+v", views[0])
	}
	if !views[1].Disabled || views[1].WindowStart != nil {
		t.Errorf("disabled weekly view must be marked, got %+v", views[1])
	}
	if views[2].WindowStart == nil || !views[2].ResetsAt.Equal(time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("monthly view: %+v", views[2])
	}
}
