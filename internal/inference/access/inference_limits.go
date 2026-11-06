// inference_limits.go — 业务速率限制（RPM/TPM，Task 4）的纯规则。
//
// 本文件无数据库、无时钟：判定与恢复时间推算都是纯函数，持久化在
// internal/inference/postgres（inference_limit_repo.go）。
//
// 确定语义（设计 §7.2 速率闸门）：
//
//   - RPM：滚动 60 秒内**已成功提交准入**的逻辑请求数。每个逻辑请求计 1
//     次，内部 attempt 重试不重复计；准入事务回滚不计数；准入成功后上游
//     失败仍计 RPM（请求已经消耗了一次调用额度）。账户级计数跨 Key、跨
//     /chat 与 /v1 共享；计数键**不含 policy version**（切套餐不重置这一
//     分钟的消费）。全局账户 RPM、选中权益策略 RPM、Key RPM 是**同时满足
//     的硬上限**，三者各自独立计数。
//   - TPM（账户级策略限制）：滚动 60 秒内已结算的归一化 token 消耗 + 全部
//     尚未结算的 token 预占。预占按准入的输入上界与强制输出 cap；结算按
//     真实/明确估算值校正，实际超过预占时按**实际总量**计（不得截到预占
//     值）；估算被真实证据修正时按同一请求的**净差额**校正，且不重置
//     原结算时刻（窗口归属不变）；unknown 永远不记零（预占保留到核对收敛）。
//
// 归一化 token 口径与计价完全一致（accounting.BillableBuckets 之后）：
// 输入 + 缓存读 + 缓存写 + 输出。推理明细（reasoning）已含在输出里，
// 不再单独相加；缓存桶已从输入里扣除，不重复计算。

package access

import (
	"strconv"
	"strings"

	"github.com/yunhou/users/internal/inference/domain"
)

// RateWindowSeconds 是 RPM/TPM 滚动窗口的秒数（60 秒）。
const RateWindowSeconds = 60

// Rate constraint kinds (the vocabulary of domain.RateBlock.Kind).
const (
	RateKindAccountRPM = "account_rpm"
	RateKindPolicyRPM  = "policy_rpm"
	RateKindKeyRPM     = "key_rpm"
	RateKindPolicyTPM  = "policy_tpm"
)

// NormalizedTokens is the normalized token consumption of one settlement —
// the same measure the price list bills (accounting.BillableBuckets has
// already made the buckets disjoint):
//
//   - input + cache read + cache write + output;
//   - reasoning is NOT added again (after normalization the output total
//     already carries it exactly once);
//   - a bucket nobody reported stays unknown (nil contributes nothing —
//     unknown never collapses into zero).
func NormalizedTokens(b domain.UsageBuckets) int64 {
	total := int64(0)
	for _, p := range []*int64{b.InputTokens, b.CacheReadTokens, b.CacheWriteTokens, b.OutputTokens} {
		if p != nil {
			total += *p
		}
	}
	return total
}

// EvaluateRateLimits decides whether one admission fits the business rate
// limits. It returns nil when the request fits, otherwise a typed
// *domain.RateLimitError (code=rate_limited — never the monetary
// quota_exceeded, which is the signal for the wallet fallback).
//
// reservedTokens is THIS request's TPM hold (input bound + forced output
// cap): it counts against the limit exactly like an already-in-flight hold,
// so the check is "settled in window + all in-flight + this one".
//
// Retry-After is only promised when EVERY blocker has a determinable
// recovery instant (account/policy/key RPM recover when the oldest counted
// requests leave the rolling window; the TPM recovers only when nothing is
// in flight and the settled tokens leave the window). A TPM blocked by
// in-flight holds has no determinable recovery — no countdown is invented.
func EvaluateRateLimits(limits domain.RateLimits, usage domain.RateUsage, reservedTokens int64) *domain.RateLimitError {
	var blocks []domain.RateBlock

	// The account-level count serves both account-level RPM limits: the
	// global account bucket and the selected policy's bucket.
	accountUsed := int64(usage.AccountRPMCount + 1)
	if limits.AccountRPM > 0 && accountUsed > int64(limits.AccountRPM) {
		blocks = append(blocks, domain.RateBlock{
			Scope: domain.RateScopeAccount, Kind: RateKindAccountRPM,
			Limit: int64(limits.AccountRPM), Used: accountUsed,
		})
	}
	if limits.PolicyRPM > 0 && accountUsed > int64(limits.PolicyRPM) {
		blocks = append(blocks, domain.RateBlock{
			Scope: domain.RateScopeAccount, Kind: RateKindPolicyRPM,
			Limit: int64(limits.PolicyRPM), Used: accountUsed,
		})
	}
	if limits.KeyRPM > 0 {
		keyUsed := int64(usage.KeyRPMCount + 1)
		if keyUsed > int64(limits.KeyRPM) {
			blocks = append(blocks, domain.RateBlock{
				Scope: domain.RateScopeKey, Kind: RateKindKeyRPM,
				Limit: int64(limits.KeyRPM), Used: keyUsed,
			})
		}
	}
	if limits.PolicyTPM > 0 {
		tpmUsed := usage.SettledTokens + usage.InflightTokens + reservedTokens
		if tpmUsed > limits.PolicyTPM {
			// The TPM recovers deterministically only when nothing is in
			// flight: an in-flight hold's real consumption is unknown, so no
			// second may be promised. The store leaves RecoverySeconds at 0
			// in that case (0 = 无法确定).
			blocks = append(blocks, domain.RateBlock{
				Scope: domain.RateScopeAccount, Kind: RateKindPolicyTPM,
				Limit: limits.PolicyTPM, Used: tpmUsed,
			})
		}
	}
	if len(blocks) == 0 {
		return nil
	}

	return &domain.RateLimitError{
		Detail: domain.NewError(domain.CodeRateLimited, rateLimitMessage(blocks)),
		Blocks: blocks,
	}
}

// RecoverySeconds is the Retry-After of a rejection: the maximum over the
// blockers — but ONLY when EVERY blocker has a determinable recovery
// instant. A blocker without one (an in-flight TPM hold, a Key budget that
// cannot be projected) yields 0: no countdown is invented (设计 §9.1:
// 未知恢复不能编造倒计时).
//
// The recovery instants themselves are filled in by the store (they need
// the rolling-window rows); this function is the pure rule over them.
func RecoverySeconds(blocks []domain.RateBlock) int {
	retryAfter := 0
	for _, b := range blocks {
		if b.RecoverySeconds <= 0 {
			return 0
		}
		if b.RecoverySeconds > retryAfter {
			retryAfter = b.RecoverySeconds
		}
	}
	return retryAfter
}

// rateLimitMessage renders the customer-facing reason: every blocking
// constraint is named (设计 §9.1: 多个约束共同阻断时返回全部约束).
func rateLimitMessage(blocks []domain.RateBlock) string {
	kinds := make([]string, 0, len(blocks))
	for _, b := range blocks {
		kinds = append(kinds, b.Kind)
	}
	msg := "rate limit exceeded: blocked by " + strings.Join(kinds, ",")
	for _, b := range blocks {
		if b.Kind == RateKindPolicyTPM {
			msg += "; tpm_used=" + strconv.FormatInt(b.Used, 10) +
				" tpm_limit=" + strconv.FormatInt(b.Limit, 10)
		}
	}
	return msg
}
