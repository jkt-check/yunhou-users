package postgres

// reconciliation_reasons.go — 核对任务原因的**唯一词表**（DB CHECK 的镜像，
// migration 031/050/051）。原因 = 任务属于哪一类处置；恢复 worker 依据它决定
// 是否可以自动驱动该任务（证据驱动）还是必须等人（只能人工处理）。
//
// 两条规则：
//   - 证据驱动类（unknown_usage / crash_recovery）：请求的消费尚不确定，
//     恢复 worker 可以按保守估算结算，并保留证据窗口等待真实证据冲正；
//   - 只能人工处理类（ManualOnlyReconciliationReasons）：请求的账务事实已经
//     自相矛盾（缺预占 / 终态后晚到用量 / 金额不一致 / 账本漂移 / 预占漂移）
//     或根本没有可恢复的估算依据（证据不足）——自动驱动既不可能正确、也会
//     每轮重复告警，必须停放并等人工处置。
//
// 词表被两个地方共用：入库时的 upsert（人工类原因必须接管既有任务的原因，
// 否则恢复 worker 会继续按证据驱动路径反复重放）与恢复 worker 的跳过清单。

const (
	// ReasonUnknownUsage: the request's consumption is unknown — the
	// recovery may settle it at the conservative estimate (证据驱动).
	ReasonUnknownUsage = "unknown_usage"
	// ReasonCrashRecovery: a crash candidate — the recovery may settle it
	// at the conservative estimate (证据驱动).
	ReasonCrashRecovery = "crash_recovery"
	// ReasonLedgerMismatch: a window-level ledger rebuild difference
	// (request_id IS NULL).
	ReasonLedgerMismatch = "ledger_mismatch"
	// ReasonManual: an operator-created task.
	ReasonManual = "manual"
	// ReasonSettlementOverage: the charge exceeded the hold; the
	// correction channel (CorrectSettlement) resolves it.
	ReasonSettlementOverage = "settlement_overage"
	// ReasonMissingReservation: the monetary reservation the admission
	// created is missing (只能人工处理).
	ReasonMissingReservation = "missing_reservation"
	// ReasonLateUsageAfterRelease: a nonzero delivery arrived after the
	// request was released (只能人工处理).
	ReasonLateUsageAfterRelease = "late_usage_after_release"
	// ReasonSettlementMismatch: an inconsistent duplicate settlement — the
	// booked charge contradicts the delivered one (只能人工处理).
	ReasonSettlementMismatch = "settlement_mismatch"
	// ReasonLedgerDrift: the request is settled but the ledger holds no
	// charge row (只能人工处理).
	ReasonLedgerDrift = "ledger_drift"
	// ReasonReservationDrift: the held reservations do not match the
	// admission (只能人工处理).
	ReasonReservationDrift = "reservation_drift"
	// ReasonInsufficientEvidence: the request row carries no recoverable
	// estimate upper bound — a pre-migration row without any monetary
	// evidence (只能人工处理).
	ReasonInsufficientEvidence = "insufficient_evidence"
)

// ManualOnlyReconciliationReasons are the reasons that require a human: the
// recovery never drives them and never re-alarms them.
//
// The list is the single source of truth for both the upsert (a manual-only
// reason must take over an open task's reason, so the classification cannot
// be silently downgraded to an evidence-driven one) and the recovery
// worker's skip list.
var ManualOnlyReconciliationReasons = []string{
	ReasonMissingReservation,
	ReasonLateUsageAfterRelease,
	ReasonSettlementMismatch,
	ReasonLedgerDrift,
	ReasonReservationDrift,
	ReasonInsufficientEvidence,
}

// IsManualOnlyReconciliationReason reports whether a reason requires a human.
func IsManualOnlyReconciliationReason(reason string) bool {
	for _, r := range ManualOnlyReconciliationReasons {
		if r == reason {
			return true
		}
	}
	return false
}
