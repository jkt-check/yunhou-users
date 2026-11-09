package access

import (
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// inference_limits_test.go — T4 速率纯规则（无数据库、无时钟）。

func intp(v int64) *int64 { return &v }

// TestNormalizedTokens_CacheAndReasoningAreNotDoubleCounted: 归一化 token
// 口径必须与计价一致 —— 缓存桶已从输入扣除（不再重复），推理明细已含在
// 输出里（不再单独相加）。
func TestNormalizedTokens_CacheAndReasoningAreNotDoubleCounted(t *testing.T) {
	// OpenAI 形状：prompt 含 cached，completion 含 reasoning。
	// accounting.BillableBuckets 之后：input=8, cache_read=4, output=34。
	b := domain.UsageBuckets{
		InputTokens: intp(8), CacheReadTokens: intp(4),
		OutputTokens: intp(34), ReasoningTokens: intp(9),
	}
	if got, err := NormalizedTokens(b); err != nil || got != 46 {
		t.Errorf("normalized tokens = %d, err = %v, want 46 (8+4+34, reasoning not added twice)", got, err)
	}
	// 未上报的桶不贡献 0（unknown 不塌缩成 0 的事实：总量只加已知桶）。
	b2 := domain.UsageBuckets{InputTokens: intp(10)}
	if got, err := NormalizedTokens(b2); err != nil || got != 10 {
		t.Errorf("normalized tokens = %d, err = %v, want 10", got, err)
	}
	// 缓存写入单独计入（它不在输入里）。
	b3 := domain.UsageBuckets{InputTokens: intp(5), CacheWriteTokens: intp(7), OutputTokens: intp(3)}
	if got, err := NormalizedTokens(b3); err != nil || got != 15 {
		t.Errorf("normalized tokens = %d, err = %v, want 15", got, err)
	}
	// 全部未上报 → 0（没有任何已知 token）。
	if got, err := NormalizedTokens(domain.UsageBuckets{}); err != nil || got != 0 {
		t.Errorf("normalized tokens = %d, err = %v, want 0", got, err)
	}
}

// TestEvaluateRateLimits_AccountPolicyAndKeyAreSimultaneousHardLimits: 三个
// RPM 上限必须同时满足；任一被突破都是独立的速率拒绝（不是金额超额）。
func TestEvaluateRateLimits_AccountPolicyAndKeyAreSimultaneousHardLimits(t *testing.T) {
	limits := domain.RateLimits{AccountRPM: 5, PolicyRPM: 3, KeyRPM: 2}

	// 计数都在限额内 → 放行。
	if rle := EvaluateRateLimits(limits, domain.RateUsage{AccountRPMCount: 1, KeyRPMCount: 1}, 0); rle != nil {
		t.Fatalf("unexpected rejection: %v", rle)
	}
	// 账户级计数到达策略上限（3）：策略 RPM 与账户 RPM 同时被突破。
	// 本次请求自己也要计入 → used = count+1 = 6 > 5 且 > 3。
	rle := EvaluateRateLimits(limits, domain.RateUsage{AccountRPMCount: 5, KeyRPMCount: 0}, 0)
	if rle == nil {
		t.Fatal("the policy RPM limit must block")
	}
	if len(rle.Blocks) != 2 {
		t.Errorf("blocks = %d, want 2 (account + policy share the account count)", len(rle.Blocks))
	}
	if domain.CodeOf(rle) != domain.CodeRateLimited {
		t.Errorf("code = %q, want rate_limited (must never look like quota_exceeded)", domain.CodeOf(rle))
	}
	// Key 级独立计数：账户还有余额，Key 用尽自己的额度。
	rle = EvaluateRateLimits(limits, domain.RateUsage{AccountRPMCount: 0, KeyRPMCount: 2}, 0)
	if rle == nil {
		t.Fatal("the key RPM limit must block")
	}
	if len(rle.Blocks) != 1 || rle.Blocks[0].Kind != RateKindKeyRPM {
		t.Errorf("blocks = %+v, want a single key_rpm block", rle.Blocks)
	}
}

// TestEvaluateRateLimits_TPMIsAccountLevel: TPM = 窗口内已结算 + 全部在途
// + 本次预占；在途预占与年龄无关（长流跨窗口仍占额度）。
func TestEvaluateRateLimits_TPMIsAccountLevel(t *testing.T) {
	limits := domain.RateLimits{PolicyTPM: 100}
	// 在途 60 + 本次 50 = 110 > 100 → 拒绝。
	rle := EvaluateRateLimits(limits, domain.RateUsage{InflightTokens: 60}, 50)
	if rle == nil {
		t.Fatal("the TPM limit must block")
	}
	if rle.Blocks[0].Kind != RateKindPolicyTPM || rle.Blocks[0].Used != 110 {
		t.Errorf("block = %+v, want policy_tpm used=110", rle.Blocks[0])
	}
	// 在途 60 + 本次 50，窗口内另有已结算 0 → 同上。
	// 结算后（在途清零、已结算 20）+ 本次 50 = 70 ≤ 100 → 放行。
	if rle := EvaluateRateLimits(limits, domain.RateUsage{SettledTokens: 20}, 50); rle != nil {
		t.Fatalf("unexpected rejection: %v", rle)
	}
	// 已结算 60 + 本次 50 = 110 > 100 → 拒绝。
	if rle := EvaluateRateLimits(limits, domain.RateUsage{SettledTokens: 60}, 50); rle == nil {
		t.Fatal("the settled tokens must count against the TPM limit")
	}
}

// TestEvaluateRateLimits_RetryAfterOnlyWhenDeterminable: 只有每个阻断约束
// 都有可确定恢复时刻时才给 Retry-After；在途 TPM 无法确定（不承诺某一秒
// 必然恢复）。
func TestEvaluateRateLimits_RetryAfterOnlyWhenDeterminable(t *testing.T) {
	// 账户 RPM 被突破 → 阻断约束成立；恢复时刻由存储层填入。
	rle := EvaluateRateLimits(domain.RateLimits{AccountRPM: 2},
		domain.RateUsage{AccountRPMCount: 2}, 0)
	if rle == nil {
		t.Fatal("the account RPM limit must block")
	}
	// 恢复时刻未知（存储层还没填）→ 不给倒计时。
	if got := RecoverySeconds(rle.Blocks); got != 0 {
		t.Errorf("retry-after = %d, want 0 (no determinable recovery)", got)
	}
	rle.Blocks[0].RecoverySeconds = 17
	if got := RecoverySeconds(rle.Blocks); got != 17 {
		t.Errorf("retry-after = %d, want 17", got)
	}
	// TPM 被在途预占阻断 → 恢复时刻不可确定（存储层不填）。
	rle = EvaluateRateLimits(domain.RateLimits{PolicyTPM: 100},
		domain.RateUsage{InflightTokens: 200}, 0)
	if rle == nil {
		t.Fatal("the TPM limit must block")
	}
	if got := RecoverySeconds(rle.Blocks); got != 0 {
		t.Errorf("retry-after = %d, want 0 (an in-flight hold has no determinable recovery)", got)
	}
	// 多个阻断约束：只要有一个不可确定就不给倒计时（不承诺某一秒必然恢复）。
	rle = EvaluateRateLimits(domain.RateLimits{AccountRPM: 2, PolicyTPM: 100},
		domain.RateUsage{AccountRPMCount: 5, InflightTokens: 200}, 0)
	if rle == nil {
		t.Fatal("the limits must block")
	}
	rle.Blocks[0].RecoverySeconds = 12
	if got := RecoverySeconds(rle.Blocks); got != 0 {
		t.Errorf("retry-after = %d, want 0 (one blocker has no determinable recovery)", got)
	}
}

// TestEvaluateRateLimits_NilPolicyLimitsAddNoLimit: 策略未设置上限 = 显式
// 不限制（不是"无限额度"的另一种写法）；全局账户 RPM 的关闭规则照旧。
func TestEvaluateRateLimits_NilPolicyLimitsAddNoLimit(t *testing.T) {
	if rle := EvaluateRateLimits(domain.RateLimits{}, domain.RateUsage{AccountRPMCount: 9999, KeyRPMCount: 9999}, 1_000_000); rle != nil {
		t.Fatalf("unexpected rejection: %v", rle)
	}
}

// TestRateLimitError_UnwrapExposesTheDomainCode: HTTP 层通过 errors.As 取
// 详情，domain.CodeOf 必须认出 rate_limited。
func TestRateLimitError_UnwrapExposesTheDomainCode(t *testing.T) {
	rle := EvaluateRateLimits(domain.RateLimits{AccountRPM: 1},
		domain.RateUsage{AccountRPMCount: 1}, 0)
	if rle == nil {
		t.Fatal("expected a rejection")
	}
	var de *domain.Error
	if !asError(rle, &de) {
		t.Fatal("the domain error must be reachable through errors.As")
	}
	if de.Code != domain.CodeRateLimited {
		t.Errorf("code = %q, want rate_limited", de.Code)
	}
	if de.Message == "" {
		t.Error("the rejection must carry a customer-facing message")
	}
}

// asError is errors.As without importing errors in the test file twice.
func asError(err error, target **domain.Error) bool {
	for e := err; e != nil; {
		if de, ok := e.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
