// Package quota holds the pure rules of the three quota windows and the
// quota policy (设计 §6, Task 6). Everything here is deterministic logic
// over injected time — no database, no wall clock. All metering time comes
// from the server; every computation normalizes to UTC, so a client
// switching timezones never moves a quota period (设计 §6: 数据库存 UTC；
// UI 转换本地显示，客户端切时区不改变额度).
package quota

import (
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// Window durations (设计 §6). The five-hour window is a fixed DURATION
// anchored at its activating consumption — explicitly NOT a "last 5 hours"
// sliding accumulation (明确不实现"最近 5 小时滑动累计").
const (
	FiveHourDuration = 5 * time.Hour
	WeeklyDuration   = 7 * 24 * time.Hour
)

// ActivationOnFirstConsumption is the read-model hint of a five-hour
// window that has not been activated yet (设计 §6: 未激活时
// window_start/resets_at 为 null，并提供 activation=on_first_consumption).
const ActivationOnFirstConsumption = "on_first_consumption"

// ErrBeforeAnchor rejects weekly/monthly resolution before the
// entitlement's ORIGINAL effective anchor: no period exists there.
var ErrBeforeAnchor = domain.NewError(domain.CodeInvalidInput, "quota: time precedes entitlement anchor")

// Interval is a half-open [Start, End) UTC window interval. A request
// landing exactly on End belongs to the NEXT window (设计 §6: 窗口区间均
// 为 [start, end)，到边界的请求属于新窗口).
type Interval struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether t ∈ [Start, End) — boundary-equal lands in the
// next window. t is normalized to UTC first.
func (iv Interval) Contains(t time.Time) bool {
	u := t.UTC()
	return !u.Before(iv.Start) && u.Before(iv.End)
}

// AddMonthsClamped shifts anchor by `months` calendar months, clamping the
// ORIGINAL anchor day into the target month when that month is shorter
// (设计 §6: 月末采用原始锚点日裁剪到目标月份最后一天：1 月 31 日 → 2 月末
// → 3 月 31 日，不因 2 月裁剪而永久漂移). The clamp always re-derives from
// the anchor itself, never from the previously clamped result, so the drift
// is impossible by construction. Result is UTC.
func AddMonthsClamped(anchor time.Time, months int) time.Time {
	a := anchor.UTC()
	total := int(a.Month()) - 1 + months
	year := a.Year() + floorDiv(total, 12)
	month := time.Month(floorMod(total, 12) + 1)
	day := min(a.Day(), daysIn(year, month))
	return time.Date(year, month, day, a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), time.UTC)
}

// daysIn returns the number of days of the given month (leap-aware).
func daysIn(year int, month time.Month) int {
	// Day 0 of the next month is the last day of this one.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int) int {
	return a - floorDiv(a, b)*b
}

// MonthlyInterval returns the k-th monthly period from the original anchor:
// [clamp(anchor, k), clamp(anchor, k+1)). Yearly-paid entitlements still
// earn quota monthly — one period per calendar month (设计 §6: 年付也按月
// 获得额度).
func MonthlyInterval(anchor time.Time, k int) (Interval, error) {
	if k < 0 {
		return Interval{}, domain.NewError(domain.CodeInvalidInput, "quota: negative month index")
	}
	return Interval{Start: AddMonthsClamped(anchor, k), End: AddMonthsClamped(anchor, k+1)}, nil
}

// MonthlyWindowAt finds the anchored monthly period containing `at`
// ([start, end) semantics) and its index. `at` before the anchor yields
// ErrBeforeAnchor.
func MonthlyWindowAt(anchor, at time.Time) (Interval, int, error) {
	a, t := anchor.UTC(), at.UTC()
	if t.Before(a) {
		return Interval{}, 0, ErrBeforeAnchor
	}
	k := (t.Year()-a.Year())*12 + int(t.Month()) - int(a.Month())
	// The clamped start of month k may sit AFTER t (anchor day beyond t's
	// day in a short month) — then t still lives in period k-1.
	if t.Before(AddMonthsClamped(a, k)) {
		k--
	}
	if k < 0 {
		return Interval{}, 0, ErrBeforeAnchor
	}
	iv, err := MonthlyInterval(a, k)
	if err != nil {
		return Interval{}, 0, err
	}
	return iv, k, nil
}

// WeeklyInterval returns the n-th 7×24h period from the original anchor.
func WeeklyInterval(anchor time.Time, n int) (Interval, error) {
	if n < 0 {
		return Interval{}, domain.NewError(domain.CodeInvalidInput, "quota: negative week index")
	}
	start := anchor.UTC().Add(time.Duration(n) * WeeklyDuration)
	return Interval{Start: start, End: start.Add(WeeklyDuration)}, nil
}

// WeeklyWindowAt finds the anchored 7×24h period containing `at`. `at`
// before the anchor yields ErrBeforeAnchor.
func WeeklyWindowAt(anchor, at time.Time) (Interval, int, error) {
	a, t := anchor.UTC(), at.UTC()
	if t.Before(a) {
		return Interval{}, 0, ErrBeforeAnchor
	}
	n := int(t.Sub(a) / WeeklyDuration)
	iv, err := WeeklyInterval(a, n)
	if err != nil {
		return Interval{}, 0, err
	}
	return iv, n, nil
}

// FiveHourInterval is the fixed-duration window a consumption at t opens:
// [t, t+5h).
func FiveHourInterval(t time.Time) Interval {
	s := t.UTC()
	return Interval{Start: s, End: s.Add(FiveHourDuration)}
}

// ConsumptionWindow computes the interval a consumption at `at` binds to
// (设计 §6). active holds the entitlement's current ACTIVE (non-voided)
// window rows of the given kind; voided rows are ignored.
//
// activate=true tells the caller to persist a NEW window row inside the
// reservation transaction (Task 7); false means bind to the existing row:
//
//   - five_hour: a live row (at ∈ [start, end)) is reused; after expiry the
//     next valid consumption opens a NEW [at, at+5h) window. Reads must use
//     ReadWindow instead — they never activate.
//   - weekly/monthly: the anchored period containing at; activate=true when
//     no row with this period's start exists yet (first consumption of the
//     period persists it in the reservation transaction).
func ConsumptionWindow(kind domain.WindowKind, anchor time.Time, active []domain.QuotaWindow, at time.Time) (Interval, bool, error) {
	t := at.UTC()
	switch kind {
	case domain.WindowFiveHour:
		for _, w := range active {
			if w.Kind != kind || w.Voided {
				continue
			}
			iv := Interval{Start: w.Start.UTC(), End: w.End.UTC()}
			if iv.Contains(t) {
				return iv, false, nil
			}
		}
		return FiveHourInterval(t), true, nil
	case domain.WindowWeekly:
		iv, _, err := WeeklyWindowAt(anchor, t)
		if err != nil {
			return Interval{}, false, err
		}
		return iv, !hasWindowStartingAt(active, kind, iv.Start), nil
	case domain.WindowMonthly:
		iv, _, err := MonthlyWindowAt(anchor, t)
		if err != nil {
			return Interval{}, false, err
		}
		return iv, !hasWindowStartingAt(active, kind, iv.Start), nil
	}
	return Interval{}, false, domain.NewError(domain.CodeInvalidInput, "quota: unknown window kind "+string(kind))
}

func hasWindowStartingAt(active []domain.QuotaWindow, kind domain.WindowKind, start time.Time) bool {
	for _, w := range active {
		if w.Kind == kind && !w.Voided && w.Start.UTC().Equal(start) {
			return true
		}
	}
	return false
}

// WindowView is the read shape of one quota window (设计 §9.2 额度响应).
// It is pure display state — building it NEVER activates a window.
type WindowView struct {
	Kind domain.WindowKind
	// Disabled marks a window the policy explicitly turned off; the absence
	// of limits must never render as unlimited (设计 §9.2).
	Disabled bool
	// Limit is the policy limit; nil when disabled.
	Limit    *domain.Microcredit
	Used     domain.Microcredit
	Reserved domain.Microcredit
	// WindowStart/ResetsAt are nil while a five-hour window is unactivated.
	WindowStart *time.Time
	ResetsAt    *time.Time
	// Activation is ActivationOnFirstConsumption for an unactivated
	// five-hour window, else "".
	Activation string
}

// ReadWindow builds the display view of one window at `at` WITHOUT
// activating anything (设计 §6: 读取额度接口不激活窗口). limit=nil marks
// the window explicitly disabled.
//
//   - five_hour: a live row yields its interval and counters; no live row
//     (never consumed, or the last window expired/voided) yields null
//     start/resets plus Activation=on_first_consumption.
//   - weekly/monthly: the anchored period containing `at` (deterministic,
//     no state created); counters come from the matching row when present.
//     `at` before the anchor yields ErrBeforeAnchor.
func ReadWindow(kind domain.WindowKind, anchor time.Time, limit *domain.Microcredit, active []domain.QuotaWindow, at time.Time) (WindowView, error) {
	v := WindowView{Kind: kind, Limit: limit, Disabled: limit == nil}
	t := at.UTC()
	if v.Disabled {
		// A disabled window carries no interval semantics at all.
		return v, nil
	}
	switch kind {
	case domain.WindowFiveHour:
		for _, w := range active {
			if w.Kind != kind || w.Voided {
				continue
			}
			iv := Interval{Start: w.Start.UTC(), End: w.End.UTC()}
			if iv.Contains(t) {
				start, end := iv.Start, iv.End
				v.WindowStart, v.ResetsAt = &start, &end
				v.Used, v.Reserved = w.Used, w.Reserved
				return v, nil
			}
		}
		v.Activation = ActivationOnFirstConsumption
		return v, nil
	case domain.WindowWeekly, domain.WindowMonthly:
		var iv Interval
		var err error
		if kind == domain.WindowWeekly {
			iv, _, err = WeeklyWindowAt(anchor, t)
		} else {
			iv, _, err = MonthlyWindowAt(anchor, t)
		}
		if err != nil {
			return WindowView{}, err
		}
		start, end := iv.Start, iv.End
		v.WindowStart, v.ResetsAt = &start, &end
		for _, w := range active {
			if w.Kind == kind && !w.Voided && w.Start.UTC().Equal(iv.Start) {
				v.Used, v.Reserved = w.Used, w.Reserved
				break
			}
		}
		return v, nil
	}
	return WindowView{}, domain.NewError(domain.CodeInvalidInput, "quota: unknown window kind "+string(kind))
}
