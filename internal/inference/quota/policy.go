package quota

import (
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// policy.go — 配额策略（PolicyVersion）的纯规则（Task 6，设计 §5/§6）。

// OveragePolicy mirrors the DB CHECK on inference_policy_versions.
type OveragePolicy string

const (
	// OverageReject: quota exhaustion stops the call (设计 §4.3: 额度不足
	// 默认停止).
	OverageReject OveragePolicy = "reject"
	// OverageClampIfDeclared: only when the client explicitly declared it
	// may shrink the output cap, the reservation follows the remaining
	// quota (设计 §7.2; 不静默钳制).
	OverageClampIfDeclared OveragePolicy = "clamp_if_declared"
	// OverageAllowOverage: reserved for the explicit pay-as-you-go phase
	// (Task 14); 首期不启用.
	OverageAllowOverage OveragePolicy = "allow_overage"
)

// Policy is the pure shape of one immutable quota policy revision (设计 §5
// PolicyVersion: 模型集合、三个窗口限额、RPM/TPM/并发及超额策略).
type Policy struct {
	Name     string
	Revision int
	// ModelIDs is the explicit granted model set; empty grants NOTHING
	// (不存在 NULL 全放行语义, 设计 §4.2).
	ModelIDs []string
	// A nil window limit means the window is DISABLED — explicitly marked,
	// never read as unlimited (设计 §9.2: 不能把缺失解释为无限额度).
	FiveHourLimit *domain.Microcredit
	WeeklyLimit   *domain.Microcredit
	MonthlyLimit  *domain.Microcredit
	RPMLimit      *int
	TPMLimit      *int64
	// ConcurrencyLimit caps in-flight requests of the consumption subject.
	ConcurrencyLimit *int
	Overage          OveragePolicy
}

// AllowsModel reports membership in the explicit model set.
func (p Policy) AllowsModel(modelID string) bool {
	for _, id := range p.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

// LimitFor returns the window limit and whether the window is enabled.
// enabled=false means DISABLED, not unlimited.
func (p Policy) LimitFor(kind domain.WindowKind) (*domain.Microcredit, bool) {
	var l *domain.Microcredit
	switch kind {
	case domain.WindowFiveHour:
		l = p.FiveHourLimit
	case domain.WindowWeekly:
		l = p.WeeklyLimit
	case domain.WindowMonthly:
		l = p.MonthlyLimit
	}
	return l, l != nil
}

// EnabledWindows lists the enabled window kinds in the fixed lock order
// (domain.WindowOrder, 设计 §7.2: 原子检查并锁定适用窗口，顺序固定).
func (p Policy) EnabledWindows() []domain.WindowKind {
	out := make([]domain.WindowKind, 0, len(domain.WindowOrder))
	for _, k := range domain.WindowOrder {
		if _, ok := p.LimitFor(k); ok {
			out = append(out, k)
		}
	}
	return out
}

// EvaluateAdmission checks ONE consumption hold against every applicable
// window — a call must satisfy ALL of them, never just one (设计 §6:
// 一次调用必须满足所有适用窗口). windows carries the caller-resolved
// window per enabled kind: an existing active row, or a not-yet-persisted
// window built from ConsumptionWindow (zero ID is fine — this function is
// pure). An enabled kind without a resolved window is a caller bug, not
// infinite quota: it fails closed with invalid_input.
//
// Every blocking window is reported together so the API layer can answer
// 429 with full detail (设计 §9.1: 多个窗口共同阻断时计算全部约束).
func EvaluateAdmission(p Policy, windows []domain.QuotaWindow, hold domain.Microcredit, at time.Time) error {
	if hold <= 0 {
		return domain.WrapError(domain.CodeInvalidInput, "quota: hold must be > 0", domain.ErrNegativeValue)
	}
	var blocked []domain.WindowBlock
	for _, kind := range p.EnabledWindows() {
		limit, _ := p.LimitFor(kind)
		w, ok := findWindow(windows, kind, at)
		if !ok {
			return domain.NewError(domain.CodeInvalidInput,
				"quota: enabled window "+string(kind)+" not resolved (resolve via ConsumptionWindow before admission)")
		}
		if w.Available() < hold {
			end := w.End.UTC()
			blocked = append(blocked, domain.WindowBlock{
				Kind:           kind,
				LimitMicros:    *limit,
				UsedMicros:     w.Used,
				ReservedMicros: w.Reserved,
				ResetsAt:       &end,
			})
		}
	}
	if len(blocked) > 0 {
		return domain.NewQuotaExceeded(blocked, false)
	}
	return nil
}

// findWindow locates the active window of `kind` whose interval contains
// `at` ([start, end) — a boundary-equal instant belongs to the NEXT window
// and therefore matches no expired row).
func findWindow(windows []domain.QuotaWindow, kind domain.WindowKind, at time.Time) (domain.QuotaWindow, bool) {
	t := at.UTC()
	for _, w := range windows {
		if w.Kind != kind || w.Voided {
			continue
		}
		if (Interval{Start: w.Start.UTC(), End: w.End.UTC()}).Contains(t) {
			return w, true
		}
	}
	return domain.QuotaWindow{}, false
}

// SettlementPlan maps ONE customer consumption to the used-increment of
// every applicable window: the SAME charge mirrors into each window — the
// customer is charged exactly ONCE, never len(kinds)× (设计 §6: 一次消费
// 同时增加三个适用窗口的 used，客户只结算一次). Task 7/9 own the
// transaction; this is the rule they must implement.
func SettlementPlan(charge domain.Microcredit, kinds []domain.WindowKind) map[domain.WindowKind]domain.Microcredit {
	out := make(map[domain.WindowKind]domain.Microcredit, len(kinds))
	for _, k := range kinds {
		out[k] = charge
	}
	return out
}

// Views builds the three §9.2 window views in the fixed order without
// activating anything (读取不激活).
func Views(p Policy, anchor time.Time, active []domain.QuotaWindow, at time.Time) ([]WindowView, error) {
	out := make([]WindowView, 0, len(domain.WindowOrder))
	for _, kind := range domain.WindowOrder {
		limit, _ := p.LimitFor(kind)
		v, err := ReadWindow(kind, anchor, limit, active, at)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
