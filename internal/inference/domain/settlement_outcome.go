package domain

import (
	"errors"
	"fmt"
)

// settlement_outcome.go — 结算投递的**可识别**终态结果（设计 §7.2：唯一键
// 防重复结算，但"已终态"不是一件事：重复结算、已释放后晚到用量、缺预占、
// 金额不一致、账本漂移各有各的处置。gateway/恢复 worker 必须能分辨它们，
// 绝不把所有 CodeConflict 一律当成功吞掉）。
//
// 判定口径：
//   - 只有"请求已 settled 且账本上存在**唯一且一致**的 charge/usage"才算
//     重复成功（幂等重投，无告警）；
//   - 已 settled 但本次投递的金额/用量与已入账的不一致 = 不一致的重复结算
//     （双扣风险）：网关必须告警并进入专门核对；恢复 worker 的**保守估算**
//     在这里让位（线上已入账的金额是权威的，估算可被后续真实证据冲正）——
//     只有这一种"已结算竞态"允许恢复 worker 静默跳过；
//   - 已 settled 但账本上没有 charge 行 = 账本漂移；held 预占数与准入不符 =
//     预占漂移：两者都是真正的账务异常，任何调用方都必须告警并进入专门
//     核对，不得当竞态吞掉；
//   - released 终态之后才到的**非零**用量是账务异常：告警 + 专门核对，绝不
//     因为 released 是终态就吞掉；
//   - 缺预占（准入时应当存在的金额预占不见了）是账务异常：不能结算，也不
//     能当重复。

// SettleOutcomeKind classifies a settlement delivery that did NOT create a
// new terminal state.
type SettleOutcomeKind string

const (
	// SettleOutcomeDuplicate: the request is already settled and the
	// recorded charge/usage is consistent with this delivery — an
	// idempotent replay (重复成功：唯一的 charge/usage 与投递一致).
	SettleOutcomeDuplicate SettleOutcomeKind = "duplicate"
	// SettleOutcomeReleased: the request was released (zero consumption
	// confirmed) and this delivery arrives afterwards. A nonzero delivery
	// is an accounting anomaly: the consumption happened but the request
	// was already finalized as free.
	SettleOutcomeReleased SettleOutcomeKind = "released"
	// SettleOutcomeChargeMismatch: the request is settled but the recorded
	// charge (or the recorded metering fact of a re-delivered attempt)
	// differs from this delivery — a double-billing risk. The gateway
	// alarms on it; the recovery's conservative estimate yields to the
	// recorded charge.
	SettleOutcomeChargeMismatch SettleOutcomeKind = "charge_mismatch"
	// SettleOutcomeLedgerDrift: the request is settled but the ledger holds
	// no charge row — a genuine ledger drift, never a benign race.
	SettleOutcomeLedgerDrift SettleOutcomeKind = "ledger_drift"
	// SettleOutcomeReservationDrift: the held reservations do not match the
	// admission (more held than the admission created) — a genuine drift.
	SettleOutcomeReservationDrift SettleOutcomeKind = "reservation_drift"
	// SettleOutcomeMissingReservation: the monetary reservation the
	// admission created is missing — the settlement cannot convert it.
	SettleOutcomeMissingReservation SettleOutcomeKind = "missing_reservation"
)

// SettleError is the typed terminal result of a settlement delivery that did
// not settle the request. Code is CodeConflict for every classification —
// the caller MUST inspect Kind, never the code alone.
type SettleError struct {
	// Kind is the classification (never empty).
	Kind SettleOutcomeKind
	// Code is the stable domain code of the failure.
	Code Code
	// Message is the human-readable reason. It never carries credentials,
	// prompts or completions.
	Message string
	// Status is the request's terminal status (settled/released).
	Status RequestStatus
	// ExistingChargeMicros is the amount already booked (nil when the
	// request carries no charge at all).
	ExistingChargeMicros *Microcredit
	// RequestedChargeMicros is the amount this delivery wanted to book.
	RequestedChargeMicros Microcredit
	// Trivial marks a delivery with nothing to book (zero charge and no
	// usage): a released request may absorb it without an anomaly.
	Trivial bool
	cause   error
}

func (e *SettleError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("inference/%s: %s (%s): %v", e.Code, e.Message, e.Kind, e.cause)
	}
	return fmt.Sprintf("inference/%s: %s (%s)", e.Code, e.Message, e.Kind)
}

// Unwrap keeps the cause chain (errors.Is/As) intact.
func (e *SettleError) Unwrap() error { return e.cause }

// AsSettleError extracts the typed terminal result from an error chain.
func AsSettleError(err error) (*SettleError, bool) {
	var se *SettleError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}
