package quota

import (
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// window_test.go — 三窗口纯规则测试：注入时间，不依赖 DB（任务书约定）。
// 时间语义是本模块的最高优先级：闰年、月末裁剪与恢复原始锚点、边界相等
// （[start,end)）、跨月/跨年、UTC 展示转换全部在此钉牢。

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func TestAddMonthsClamped(t *testing.T) {
	cases := []struct {
		name   string
		anchor time.Time
		months int
		want   time.Time
	}{
		{"identity", utc(2026, 1, 31, 10, 30), 0, utc(2026, 1, 31, 10, 30)},
		// 验收硬项：1 月 31 日锚点 → 2 月裁剪 → 3 月恢复 31 日。
		{"jan31 to feb (non-leap)", utc(2026, 1, 31, 10, 30), 1, utc(2026, 2, 28, 10, 30)},
		{"jan31 to mar restores 31", utc(2026, 1, 31, 10, 30), 2, utc(2026, 3, 31, 10, 30)},
		{"jan31 to feb (leap)", utc(2024, 1, 31, 10, 30), 1, utc(2024, 2, 29, 10, 30)},
		{"jan31 leap to mar", utc(2024, 1, 31, 10, 30), 2, utc(2024, 3, 31, 10, 30)},
		{"mar31 to apr clips", utc(2026, 3, 31, 0, 0), 1, utc(2026, 4, 30, 0, 0)},
		{"mar31 to may restores", utc(2026, 3, 31, 0, 0), 2, utc(2026, 5, 31, 0, 0)},
		// 闰日锚点：平年裁剪、下个闰年恢复 29 日。
		{"feb29 to non-leap feb", utc(2024, 2, 29, 8, 0), 12, utc(2025, 2, 28, 8, 0)},
		{"feb29 to next leap feb", utc(2024, 2, 29, 8, 0), 48, utc(2028, 2, 29, 8, 0)},
		// 跨年。
		{"dec31 cross-year", utc(2025, 12, 31, 23, 59), 1, utc(2026, 1, 31, 23, 59)},
		{"oct31 to feb cross-year", utc(2025, 10, 31, 0, 0), 4, utc(2026, 2, 28, 0, 0)},
		// 负月（回望）。
		{"mar31 back one month", utc(2026, 3, 31, 0, 0), -1, utc(2026, 2, 28, 0, 0)},
		{"jan31 back one cross-year", utc(2026, 1, 31, 0, 0), -1, utc(2025, 12, 31, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AddMonthsClamped(c.anchor, c.months); !got.Equal(c.want) {
				t.Errorf("AddMonthsClamped(%v, %d) = %v, want %v", c.anchor, c.months, got, c.want)
			}
		})
	}
}

// TestMonthlyWindowAt_Jan31AnchorRestoresMar31 是验收硬项：锚点 1 月 31 日，
// 2 月窗口裁剪到月末，3 月恢复 31 日 —— 锚点本身永不被改写。
func TestMonthlyWindowAt_Jan31AnchorRestoresMar31(t *testing.T) {
	anchor := utc(2026, 1, 31, 10, 0)

	// 2 月中的时刻落在第 0 期 [Jan 31, Feb 28)。
	iv, k, err := MonthlyWindowAt(anchor, utc(2026, 2, 15, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 0 || !iv.Start.Equal(utc(2026, 1, 31, 10, 0)) || !iv.End.Equal(utc(2026, 2, 28, 10, 0)) {
		t.Errorf("feb: k=%d iv=[%v,%v), want k=0 [Jan31,Feb28)", k, iv.Start, iv.End)
	}

	// 3 月 1 日落在第 1 期 [Feb 28, Mar 31) —— 期末恢复原始锚点日 31。
	iv, k, err = MonthlyWindowAt(anchor, utc(2026, 3, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 1 || !iv.Start.Equal(utc(2026, 2, 28, 10, 0)) || !iv.End.Equal(utc(2026, 3, 31, 10, 0)) {
		t.Errorf("mar: k=%d iv=[%v,%v), want k=1 [Feb28,Mar31)", k, iv.Start, iv.End)
	}

	// 边界相等：at == 窗口 End 属于下一个窗口 [start, end)。
	iv, k, err = MonthlyWindowAt(anchor, utc(2026, 3, 31, 10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 2 || !iv.Start.Equal(utc(2026, 3, 31, 10, 0)) || !iv.End.Equal(utc(2026, 4, 30, 10, 0)) {
		t.Errorf("boundary: k=%d iv=[%v,%v), want k=2 [Mar31,Apr30)", k, iv.Start, iv.End)
	}

	// 裁剪不永久漂移：4 月后再回 31 日。
	iv, k, err = MonthlyWindowAt(anchor, utc(2026, 5, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 3 || !iv.End.Equal(utc(2026, 5, 31, 10, 0)) {
		t.Errorf("may: k=%d end=%v, want k=3 end May 31 (no permanent drift)", k, iv.End)
	}
}

func TestMonthlyWindowAt_LeapYear(t *testing.T) {
	anchor := utc(2024, 1, 31, 0, 0)
	// 闰年 2 月裁剪到 29 日。
	iv, k, err := MonthlyWindowAt(anchor, utc(2024, 2, 10, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 0 || !iv.End.Equal(utc(2024, 2, 29, 0, 0)) {
		t.Errorf("leap feb: k=%d end=%v, want [Jan31,Feb29)", k, iv.End)
	}
	// 闰日锚点在平年裁剪到 28 日。
	leapAnchor := utc(2024, 2, 29, 0, 0)
	iv, k, err = MonthlyWindowAt(leapAnchor, utc(2025, 3, 5, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 12 || !iv.Start.Equal(utc(2025, 2, 28, 0, 0)) || !iv.End.Equal(utc(2025, 3, 29, 0, 0)) {
		t.Errorf("leap anchor +12: k=%d iv=[%v,%v), want k=12 [Feb28 2025,Mar29 2025)", k, iv.Start, iv.End)
	}
}

func TestMonthlyWindowAt_CrossYearAndBoundary(t *testing.T) {
	anchor := utc(2025, 11, 30, 5, 0)
	cases := []struct {
		at       time.Time
		k        int
		start, e time.Time
	}{
		{utc(2025, 11, 30, 5, 0), 0, utc(2025, 11, 30, 5, 0), utc(2025, 12, 30, 5, 0)}, // 恰在锚点
		{utc(2025, 12, 30, 5, 0), 1, utc(2025, 12, 30, 5, 0), utc(2026, 1, 30, 5, 0)},  // 边界 → 新窗口
		{utc(2026, 2, 10, 0, 0), 2, utc(2026, 1, 30, 5, 0), utc(2026, 2, 28, 5, 0)},    // 跨年 + 2 月裁剪
		{utc(2026, 3, 1, 0, 0), 3, utc(2026, 2, 28, 5, 0), utc(2026, 3, 30, 5, 0)},     // 恢复 30 日
	}
	for _, c := range cases {
		iv, k, err := MonthlyWindowAt(anchor, c.at)
		if err != nil {
			t.Fatalf("at %v: %v", c.at, err)
		}
		if k != c.k || !iv.Start.Equal(c.start) || !iv.End.Equal(c.e) {
			t.Errorf("at %v: k=%d iv=[%v,%v), want k=%d [%v,%v)", c.at, k, iv.Start, iv.End, c.k, c.start, c.e)
		}
	}

	// 锚点之前没有周期。
	if _, _, err := MonthlyWindowAt(anchor, utc(2025, 11, 30, 4, 59)); err == nil {
		t.Error("before anchor: want ErrBeforeAnchor")
	}
}

// TestMonthlyWindow_AnnualPaymentEarnsMonthly 验收硬项：年付也按月获得额度
// —— 12 个公历月窗口逐月推进，锚点日裁剪规则全程一致。
func TestMonthlyWindow_AnnualPaymentEarnsMonthly(t *testing.T) {
	anchor := utc(2026, 1, 31, 10, 0)
	wantStarts := []time.Time{
		utc(2026, 1, 31, 10, 0), utc(2026, 2, 28, 10, 0), utc(2026, 3, 31, 10, 0),
		utc(2026, 4, 30, 10, 0), utc(2026, 5, 31, 10, 0), utc(2026, 6, 30, 10, 0),
		utc(2026, 7, 31, 10, 0), utc(2026, 8, 31, 10, 0), utc(2026, 9, 30, 10, 0),
		utc(2026, 10, 31, 10, 0), utc(2026, 11, 30, 10, 0), utc(2026, 12, 31, 10, 0),
	}
	for k, want := range wantStarts {
		iv, err := MonthlyInterval(anchor, k)
		if err != nil {
			t.Fatal(err)
		}
		if !iv.Start.Equal(want) {
			t.Errorf("period %d: start=%v, want %v", k, iv.Start, want)
		}
		next, err := MonthlyInterval(anchor, k+1)
		if err != nil {
			t.Fatal(err)
		}
		if !iv.End.Equal(next.Start) {
			t.Errorf("period %d: end=%v does not meet next start=%v (无缝推进)", k, iv.End, next.Start)
		}
	}
	// 年末时刻仍在按月推进的窗口内（第 10 期，0 基）。
	iv, k, err := MonthlyWindowAt(anchor, utc(2026, 12, 15, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if k != 10 || !iv.Start.Equal(utc(2026, 11, 30, 10, 0)) || !iv.End.Equal(utc(2026, 12, 31, 10, 0)) {
		t.Errorf("dec: k=%d iv=[%v,%v), want k=10 [Nov30,Dec31)", k, iv.Start, iv.End)
	}
}

// TestWindowMath_IsTimezoneInvariant：同一时刻无论客户端/调用方时区如何
// 表达都落同一窗口（设计 §6: UTC 锚点，客户端切时区不改变额度；UI 只在
// 展示层做本地转换）。
func TestWindowMath_IsTimezoneInvariant(t *testing.T) {
	plus8 := time.FixedZone("UTC+8", 8*3600)
	minus5 := time.FixedZone("UTC-5", -5*3600)

	anchorUTC := utc(2026, 1, 31, 10, 0)
	anchorLocal := time.Date(2026, 1, 31, 18, 0, 0, 0, plus8) // 同一时刻
	if !anchorLocal.Equal(anchorUTC) {
		t.Fatal("test premise broken: anchors differ")
	}
	atUTC := utc(2026, 3, 1, 0, 0)
	atLocal := time.Date(2026, 2, 28, 19, 0, 0, 0, minus5) // 同一时刻

	iv1, k1, err1 := MonthlyWindowAt(anchorUTC, atUTC)
	iv2, k2, err2 := MonthlyWindowAt(anchorLocal, atLocal)
	if err1 != nil || err2 != nil {
		t.Fatalf("errs: %v %v", err1, err2)
	}
	if k1 != k2 || !iv1.Start.Equal(iv2.Start) || !iv1.End.Equal(iv2.End) {
		t.Errorf("timezone changed the window: [%v,%v)#%d vs [%v,%v)#%d",
			iv1.Start, iv1.End, k1, iv2.Start, iv2.End, k2)
	}
	if iv1.Start.Location() != time.UTC {
		t.Errorf("window start not UTC: %v", iv1.Start.Location())
	}
}

func TestWeeklyWindowAt(t *testing.T) {
	anchor := utc(2026, 9, 1, 0, 0)

	// 第 0 期 [Sep 1, Sep 8)。
	iv, n, err := WeeklyWindowAt(anchor, utc(2026, 9, 7, 23, 59))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || !iv.Start.Equal(anchor) || !iv.End.Equal(utc(2026, 9, 8, 0, 0)) {
		t.Errorf("n=%d iv=[%v,%v), want n=0 [Sep1,Sep8)", n, iv.Start, iv.End)
	}

	// 边界相等：恰好 +7×24h 属第 1 期。
	iv, n, err = WeeklyWindowAt(anchor, utc(2026, 9, 8, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !iv.Start.Equal(utc(2026, 9, 8, 0, 0)) || !iv.End.Equal(utc(2026, 9, 15, 0, 0)) {
		t.Errorf("boundary: n=%d iv=[%v,%v), want n=1 [Sep8,Sep15)", n, iv.Start, iv.End)
	}

	// 跨年长程：365 天后是第 52 期（7×24h 固定时长，不经公历）。
	iv, n, err = WeeklyWindowAt(anchor, anchor.Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 52 {
		t.Errorf("n=%d, want 52", n)
	}
	if got := iv.End.Sub(iv.Start); got != WeeklyDuration {
		t.Errorf("weekly span = %v, want %v", got, WeeklyDuration)
	}

	if _, _, err := WeeklyWindowAt(anchor, utc(2026, 8, 31, 23, 59)); err == nil {
		t.Error("before anchor: want ErrBeforeAnchor")
	}
}

// --- five_hour：首次有效消费开启、到期后下一次消费开新窗、读取不激活 ---

func TestFiveHour_ConsumptionActivatesAndReuses(t *testing.T) {
	anchor := utc(2026, 9, 8, 4, 0) // five_hour 不用锚点，传入无害

	// 首次消费开启 [t, t+5h)。
	t0 := utc(2026, 9, 8, 4, 0)
	iv, activate, err := ConsumptionWindow(domain.WindowFiveHour, anchor, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !activate || !iv.Start.Equal(t0) || !iv.End.Equal(t0.Add(FiveHourDuration)) {
		t.Errorf("first consumption: iv=[%v,%v) activate=%v, want new [%v,%v)", iv.Start, iv.End, activate, t0, t0.Add(FiveHourDuration))
	}

	// 有效窗口内的消费复用同一窗口，不开新窗。
	live := domain.QuotaWindow{Kind: domain.WindowFiveHour, Start: iv.Start, End: iv.End}
	iv2, activate2, err := ConsumptionWindow(domain.WindowFiveHour, anchor, []domain.QuotaWindow{live}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if activate2 || !iv2.Start.Equal(iv.Start) || !iv2.End.Equal(iv.End) {
		t.Errorf("reuse: iv=[%v,%v) activate=%v, want reuse original", iv2.Start, iv2.End, activate2)
	}

	// 到期后下一次有效消费开启新窗口。
	iv3, activate3, err := ConsumptionWindow(domain.WindowFiveHour, anchor, []domain.QuotaWindow{live}, t0.Add(6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !activate3 || !iv3.Start.Equal(t0.Add(6*time.Hour)) {
		t.Errorf("after expiry: iv=[%v,%v) activate=%v, want new window at consumption time", iv3.Start, iv3.End, activate3)
	}

	// 边界相等：at == End 属新窗口。
	iv4, activate4, err := ConsumptionWindow(domain.WindowFiveHour, anchor, []domain.QuotaWindow{live}, iv.End)
	if err != nil {
		t.Fatal(err)
	}
	if !activate4 || !iv4.Start.Equal(iv.End) {
		t.Errorf("boundary: iv=[%v,%v) activate=%v, want new window starting at old End", iv4.Start, iv4.End, activate4)
	}

	// 已撤销（voided）窗口不算活跃：消费开新窗。
	voided := domain.QuotaWindow{Kind: domain.WindowFiveHour, Start: iv.Start, End: iv.End, Voided: true}
	_, activate5, err := ConsumptionWindow(domain.WindowFiveHour, anchor, []domain.QuotaWindow{voided}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !activate5 {
		t.Error("voided window must not be reused")
	}
}

func TestFiveHour_ReadNeverActivates(t *testing.T) {
	anchor := utc(2026, 9, 8, 4, 0)
	limit := domain.Microcredit(1_000_000)

	// 未激活：window_start/resets_at 为 null，携带 activation 提示。
	v, err := ReadWindow(domain.WindowFiveHour, anchor, &limit, nil, utc(2026, 9, 8, 4, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.WindowStart != nil || v.ResetsAt != nil {
		t.Errorf("unactivated view: start=%v resets=%v, want null/null", v.WindowStart, v.ResetsAt)
	}
	if v.Activation != ActivationOnFirstConsumption {
		t.Errorf("activation = %q, want %q", v.Activation, ActivationOnFirstConsumption)
	}

	// 只有已过期窗口：读取同样不激活新窗口（读路径无状态）。
	expired := domain.QuotaWindow{
		Kind:  domain.WindowFiveHour,
		Start: utc(2026, 9, 8, 0, 0), End: utc(2026, 9, 8, 5, 0),
		Used: 100,
	}
	v, err = ReadWindow(domain.WindowFiveHour, anchor, &limit, []domain.QuotaWindow{expired}, utc(2026, 9, 8, 6, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.WindowStart != nil || v.Activation != ActivationOnFirstConsumption {
		t.Errorf("read after expiry must not activate: %+v", v)
	}

	// 活跃窗口读出区间与用量。
	live := domain.QuotaWindow{
		Kind:  domain.WindowFiveHour,
		Start: utc(2026, 9, 8, 5, 30), End: utc(2026, 9, 8, 10, 30),
		Used: 200, Reserved: 50,
	}
	v, err = ReadWindow(domain.WindowFiveHour, anchor, &limit, []domain.QuotaWindow{live}, utc(2026, 9, 8, 6, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.WindowStart == nil || !v.WindowStart.Equal(live.Start) || !v.ResetsAt.Equal(live.End) {
		t.Errorf("live view: %+v", v)
	}
	if v.Used != 200 || v.Reserved != 50 || v.Activation != "" {
		t.Errorf("live view counters: %+v", v)
	}
}

func TestReadWindow_AnchoredKinds(t *testing.T) {
	anchor := utc(2026, 1, 31, 10, 0)
	limit := domain.Microcredit(10_000_000)
	at := utc(2026, 3, 15, 0, 0)

	// weekly：无行也读出当前锚定周期（确定性派生，不创建状态）。
	v, err := ReadWindow(domain.WindowWeekly, anchor, &limit, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if v.WindowStart == nil || v.ResetsAt == nil || v.Used != 0 {
		t.Errorf("weekly view without rows: %+v", v)
	}
	// 周窗口期长恒为 7×24h。
	if got := v.ResetsAt.Sub(*v.WindowStart); got != WeeklyDuration {
		t.Errorf("weekly span = %v, want %v", got, WeeklyDuration)
	}

	// monthly：3 月 15 日 → 第 1 期 [Feb 28, Mar 31)，行存在时带计数。
	row := domain.QuotaWindow{
		Kind:  domain.WindowMonthly,
		Start: utc(2026, 2, 28, 10, 0), End: utc(2026, 3, 31, 10, 0),
		Used: 42, Reserved: 7,
	}
	v, err = ReadWindow(domain.WindowMonthly, anchor, &limit, []domain.QuotaWindow{row}, at)
	if err != nil {
		t.Fatal(err)
	}
	if !v.WindowStart.Equal(utc(2026, 2, 28, 10, 0)) || !v.ResetsAt.Equal(utc(2026, 3, 31, 10, 0)) {
		t.Errorf("monthly view interval: [%v,%v)", v.WindowStart, v.ResetsAt)
	}
	if v.Used != 42 || v.Reserved != 7 {
		t.Errorf("monthly view counters: %+v", v)
	}

	// 禁用窗口：显式标识，不给区间（≠ 无限额度）。
	v, err = ReadWindow(domain.WindowMonthly, anchor, nil, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Disabled || v.WindowStart != nil || v.Limit != nil {
		t.Errorf("disabled view: %+v", v)
	}

	// 锚点之前：锚定窗口无周期。
	if _, err := ReadWindow(domain.WindowWeekly, anchor, &limit, nil, utc(2026, 1, 1, 0, 0)); err == nil {
		t.Error("weekly read before anchor: want ErrBeforeAnchor")
	}
}

func TestConsumptionWindow_AnchoredActivation(t *testing.T) {
	anchor := utc(2026, 1, 31, 10, 0)
	at := utc(2026, 3, 15, 0, 0)

	// 首个该周期消费：activate=true，区间为锚定周期。
	iv, activate, err := ConsumptionWindow(domain.WindowMonthly, anchor, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if !activate || !iv.Start.Equal(utc(2026, 2, 28, 10, 0)) || !iv.End.Equal(utc(2026, 3, 31, 10, 0)) {
		t.Errorf("monthly first consumption: iv=[%v,%v) activate=%v", iv.Start, iv.End, activate)
	}

	// 周期行已存在：复用，不重复建窗。
	row := domain.QuotaWindow{Kind: domain.WindowMonthly, Start: iv.Start, End: iv.End}
	_, activate, err = ConsumptionWindow(domain.WindowMonthly, anchor, []domain.QuotaWindow{row}, at)
	if err != nil {
		t.Fatal(err)
	}
	if activate {
		t.Error("existing period row must not re-activate")
	}

	// 下个周期（边界相等后）开新行。
	iv2, activate2, err := ConsumptionWindow(domain.WindowMonthly, anchor, []domain.QuotaWindow{row}, iv.End)
	if err != nil {
		t.Fatal(err)
	}
	if !activate2 || !iv2.Start.Equal(iv.End) {
		t.Errorf("next period: iv=[%v,%v) activate=%v", iv2.Start, iv2.End, activate2)
	}

	// weekly 同理：行存在复用，新周期建行。
	wiv, wact, err := ConsumptionWindow(domain.WindowWeekly, anchor, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if !wact {
		t.Error("weekly first consumption must activate")
	}
	wrow := domain.QuotaWindow{Kind: domain.WindowWeekly, Start: wiv.Start, End: wiv.End}
	if _, act, _ := ConsumptionWindow(domain.WindowWeekly, anchor, []domain.QuotaWindow{wrow}, at); act {
		t.Error("weekly existing row must not re-activate")
	}
}
