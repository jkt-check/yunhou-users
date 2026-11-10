package domain

// rate_limits.go — 业务速率限制（RPM/TPM，Task 4）的共享类型。
//
// 这一层只放跨包共享的数据形状与可识别的拒绝类型（与 domain/errors.go 的
// QuotaExceededError / settlement_outcome.go 的 SettleError 同一约定）：
// 纯规则在 internal/inference/access（inference_limits.go），存储在
// internal/inference/postgres（inference_limit_repo.go）。
//
// 为什么类型在 domain 而不是 access：access → quota 是既有依赖方向，
// quota 准入服务必须能命名速率预占的命令类型，quota → access 会成环。
// domain 是唯一被三方共同依赖的层。

// RateScope 是速率计数的口径。
type RateScope string

const (
	// RateScopeAccount 是账户级口径：全局账户 RPM 的计数键 + 账户级策略
	// TPM 的预占/结算载体。账户级计数跨 Key、跨 /chat 与 /v1 共享。
	RateScopeAccount RateScope = "account"
	// RateScopeKey 是 Key 级口径：Key RPM 的计数键（Key 之间互不影响，
	// 但都计入账户级计数）。
	RateScopeKey RateScope = "key"
)

// RateLimits 是一次准入适用的业务速率上限。0 = 不限制（显式关闭，绝不
// 解释为「无限额度」的另一种写法）。
type RateLimits struct {
	// AccountRPM 是全局账户 RPM 上限（INFERENCE_ACCOUNT_RPM）。
	AccountRPM int
	// PolicyRPM 是选中权益策略的 RPM 上限（策略未设置 = 不加该限制）。
	PolicyRPM int
	// PolicyTPM 是选中权益策略的 TPM 上限（账户级；策略未设置 = 不加该
	// 限制）。预占按准入的输入上界 + 强制输出 cap，结算按真实/明确估算值
	// 校正。
	PolicyTPM int64
	// KeyRPM 是客户 Key 的 RPM 上限。它是**数据库事实**：存储层在准入
	// 事务内读 inference_api_keys.rpm_limit 并覆盖本字段，调用方留 0。
	KeyRPM int
}

// RateReserveCommand 是持久速率预占的输入，在**准入事务内**执行。
type RateReserveCommand struct {
	// RequestID 是逻辑请求 ID（幂等身份：一个请求每个 scope 至多一行）。
	RequestID string
	// BillingAccountID 是消费主体（账户级计数的键）。
	BillingAccountID string
	// APIKeyID 是客户 Key（可空 = 无 Key 的入口，例如 /chat facade）。
	APIKeyID *string
	// PolicyVersionID 是准入时钉住的策略版本（审计用；计数键不含它）。
	PolicyVersionID string
	// Limits 是本次准入适用的上限。
	Limits RateLimits
	// ReservedTokens 是本次请求的 TPM 预占（输入上界 + 强制输出 cap）。
	ReservedTokens int64
}

// RateUsage 是滚动窗口内的聚合用量（存储层读出，交给纯规则裁决）。
type RateUsage struct {
	// AccountRPMCount 是滚动 60 秒内已成功准入的逻辑请求数（账户级）。
	// 全局账户 RPM 与选中权益策略 RPM 共用这一个计数（两者都是账户级
	// 上限）；计数键不含 policy version（切套餐不重置一分钟消费）。
	AccountRPMCount int
	// KeyRPMCount 是该 Key 在滚动 60 秒内的逻辑请求数。
	KeyRPMCount int
	// SettledTokens 是滚动 60 秒内已结算的归一化 token 消耗（账户级）。
	SettledTokens int64
	// InflightTokens 是全部尚未结算的 token 预占（**与年龄无关**：长流跨
	// 60 秒仍保留在途预占，不能靠 TTL 释放）。
	InflightTokens int64
	// KeyRPMLimit 是该 Key 的 RPM 上限（数据库事实：存储层在同一语句里
	// 一并读出，调用方不传）。
	KeyRPMLimit int
}

// RateBlock 描述一个阻断准入的速率约束。
type RateBlock struct {
	// Scope 是计数口径。
	Scope RateScope
	// Kind 是约束名：account_rpm / policy_rpm / key_rpm / policy_tpm。
	Kind string
	// Limit 是该约束的上限（0 = 未设置）。
	Limit int64
	// Used 是触发阻断时的用量（请求自身的预占已计入）。
	Used int64
	// RecoverySeconds 是可确定的恢复秒数（0 = 无法确定）。
	RecoverySeconds int
}

// RateLimitError 是**速率**拒绝（与金额 CodeQuotaExceeded 严格分开）：
// 速率拒绝不得触发钱包 fallback，PAYG/套餐外也不能花钱包绕过 RPM/TPM。
type RateLimitError struct {
	// Detail 是底层领域错误（code=rate_limited，消息面向客户）。
	Detail *Error
	// Blocks 是全部阻断约束（多个约束共同阻断时全部返回）。
	Blocks []RateBlock
	// RetryAfterSeconds 是可确定的恢复秒数（0 = 无法确定 —— 在途 TPM、
	// 累计 Key 预算等无法承诺某一秒必然恢复）。
	RetryAfterSeconds int
}

func (e *RateLimitError) Error() string {
	if e == nil || e.Detail == nil {
		return "inference/rate_limited"
	}
	return e.Detail.Error()
}

// Unwrap exposes the domain error so errors.As/CodeOf recognise the
// rate_limited code.
func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Detail
}
