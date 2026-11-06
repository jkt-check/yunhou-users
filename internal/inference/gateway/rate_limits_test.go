// rate_limits_test.go — T4：统一业务速率准入（RPM/TPM）的行为验收。
//
// 全部通过既有网关 API（ChatCompletions）+ 真实 PostgreSQL：
//   - 速率拒绝必须是可识别的速率错误（rate_limited），绝不能混进会触发
//     钱包 fallback 的金额 quota_exceeded（PAYG/超额不得买过 RPM/TPM）；
//   - 失败事务不留任何速率计数；一次成功准入按逻辑请求计一次（内部重试
//     不重复计）；准入成功后上游失败仍计 RPM；
//   - 账户级计数跨 Key、跨 /chat 与 /v1 共享；切套餐不重置一分钟消费；
//   - TPM = 窗口内已结算 token + 全部在途预占；长流跨窗口仍占额度；实际
//     超预占按实际总量计；未结算(unknown)的请求不释放预占。

package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
	"github.com/yunhou/users/internal/model"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// nonStreamUsage serves a non-streaming chat.completion carrying the given
// prompt/completion token counts.
func nonStreamUsage(prompt, completion int) func(w http.ResponseWriter, r *http.Request, body []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ns","object":"chat.completion","created":1,"model":"glm-4.6-upstream",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":`+strconv.Itoa(prompt)+`,"completion_tokens":`+strconv.Itoa(completion)+
			`,"total_tokens":`+strconv.Itoa(prompt+completion)+`}}`)
	}
}

// streamUsage serves a streaming completion whose final usage chunk carries
// the given token counts.
func streamUsage(prompt, completion int) func(w http.ResponseWriter, r *http.Request, body []byte) {
	usage := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{" +
		"\"prompt_tokens\":" + strconv.Itoa(prompt) +
		",\"completion_tokens\":" + strconv.Itoa(completion) +
		",\"total_tokens\":" + strconv.Itoa(prompt+completion) + "}}\n\n"
	return sseHandler(chunkA, chunkB, usage, chunkDone)
}

// tinyReq builds a request whose admission input bound is exactly 20 tokens
// (one 1-byte message: (1+8)/2 + 16 = 20) with the given output cap — the
// TPM reservation is therefore 20 + cap tokens.
func tinyReq(stream bool, maxTokens int64) *providers.ChatRequest {
	return &providers.ChatRequest{
		Model:     "glm-4.6",
		Stream:    stream,
		MaxTokens: &maxTokens,
		Messages:  []model.ChatMessage{{Role: "user", Content: "x"}},
	}
}

// setPolicyRateLimits pins the fixture policy's RPM/TPM limits (0 = no
// limit, i.e. NULL — a limit is only ever a positive integer).
func (f *fixture) setPolicyRateLimits(t *testing.T, rpm int, tpm int64) {
	t.Helper()
	var rpmArg, tpmArg interface{}
	if rpm > 0 {
		rpmArg = rpm
	}
	if tpm > 0 {
		tpmArg = tpm
	}
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET rpm_limit=$2, tpm_limit=$3 WHERE id=$1`,
		f.policyID, rpmArg, tpmArg); err != nil {
		t.Fatalf("set policy rate limits: %v", err)
	}
}

// setKeyRPMLimit pins one customer key's RPM limit (0 = no limit).
func (f *fixture) setKeyRPMLimit(t *testing.T, keyID string, rpm int) {
	t.Helper()
	var arg interface{}
	if rpm > 0 {
		arg = rpm
	}
	if _, err := f.db.Exec(`UPDATE inference_api_keys SET rpm_limit=$2 WHERE id=$1`, keyID, arg); err != nil {
		t.Fatalf("set key rpm limit: %v", err)
	}
}

// newKey mints a second customer key for the SAME billing account (the
// cross-Key sharing case: the account counter is shared, the key counter
// is not).
func (f *fixture) newKey(t *testing.T) *domain.APIKey {
	t.Helper()
	key := &domain.APIKey{
		BillingAccountID: f.accountID, Name: "cli-2",
		Prefix: "yk-test-" + uuid.NewString()[:8],
	}
	if err := f.store.InsertAPIKey(context.Background(), key, "sha256:"+uuid.NewString()); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return key
}

// secondGateway builds an INDEPENDENT service instance over a SECOND
// database pool pointing at the same database — the multi-instance
// contention case (the counters must be shared through PostgreSQL, not
// through a process-local map). The pool is returned so a test can take
// the database down.
func (f *fixture) secondGateway(t *testing.T) (*Service, *sqlx.DB) {
	t.Helper()
	db2, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	store2 := postgres.NewStore(db2)
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	routing2 := routing.NewService(store2, adapters, nil)
	quota2 := quota.NewService(store2, nil)
	ent2 := access.NewEntitlementResolver(store2, nil)
	return NewService(&staticSnapshot{f.snap}, store2, ent2, quota2, routing2,
		f.credSvc, providers.NewHTTPClient(egress), egress, nil), db2
}

// intPtr returns a pointer to v (policy limits are optional).
func intPtr(v int) *int { return &v }

// int64Ptr returns a pointer to v.
func int64Ptr(v int64) *int64 { return &v }

// rateOutcome classifies one call's result: admitted / rate-rejected /
// other failure.
type rateOutcome int

const (
	outcomeAdmitted rateOutcome = iota
	outcomeRateLimited
	outcomeOther
)

func classify(err error) rateOutcome {
	switch {
	case err == nil:
		return outcomeAdmitted
	case domain.CodeOf(err) == domain.CodeRateLimited:
		return outcomeRateLimited
	default:
		return outcomeOther
	}
}

// ---------------------------------------------------------------------------
// RPM
// ---------------------------------------------------------------------------

// TestRateLimit_PolicyRPM_CapsConcurrentAdmissions: RPM=2，同账户并发 3 次
// → 最多 2 次成功；被拒的那次必须是可识别的速率错误（rate_limited），不是
// 金额 quota_exceeded（后者会触发钱包 fallback）。
func TestRateLimit_PolicyRPM_CapsConcurrentAdmissions(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)

	p, key := f.principal()
	results := make([]error, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			results[i] = err
		}(i)
	}
	wg.Wait()

	admitted, rateLimited, other := 0, 0, 0
	for _, err := range results {
		switch classify(err) {
		case outcomeAdmitted:
			admitted++
		case outcomeRateLimited:
			rateLimited++
		default:
			other++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if admitted != 2 {
		t.Errorf("admitted = %d, want 2 (RPM=2 是硬上限，第三次必须被拒)", admitted)
	}
	if rateLimited != 1 {
		t.Errorf("rate-limited = %d, want 1", rateLimited)
	}
	if other != 0 {
		t.Errorf("other failures = %d, want 0", other)
	}
}

// TestRateLimit_TwoInstancesTwoPools_ShareTheAccountCounter: 两个独立
// Service 实例、两个独立 DB pool 并发 3 次 → 合计最多 2 次成功（计数必须
// 经 PostgreSQL 共享，不能是进程内的）。
func TestRateLimit_TwoInstancesTwoPools_ShareTheAccountCounter(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)
	gw2, _ := f.secondGateway(t)

	p, key := f.principal()
	results := make([]error, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		gw := f.gateway
		if i%2 == 1 {
			gw = gw2
		}
		wg.Add(1)
		go func(i int, gw *Service) {
			defer wg.Done()
			_, err := gw.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			results[i] = err
		}(i, gw)
	}
	wg.Wait()

	admitted := 0
	for _, err := range results {
		if classify(err) == outcomeAdmitted {
			admitted++
		}
	}
	if admitted > 2 {
		t.Errorf("admitted = %d, want <= 2 (两个实例必须共享同一个账户计数)", admitted)
	}
}

// TestRateLimit_CrossKeyAndEntrypointShareTheAccountCounter: 账户级计数跨
// Key、跨 /chat 与 /v1 共享（/chat 与 /v1 是同一账户的同一分钟消费）。
func TestRateLimit_CrossKeyAndEntrypointShareTheAccountCounter(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)
	key2 := f.newKey(t)

	p, key := f.principal()
	// 1) /v1 入口 + Key A
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// 2) /v1 入口 + 另一个 Key（同一账户）
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key2,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	// 3) /chat 入口（同一账户，同一分钟）→ 必须被账户级上限拒绝
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolKayaChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 3 err = %v, want rate_limited (/chat 与 /v1 共用账户计数)", err)
	}
}

// TestRateLimit_KeyRPMIsEnforcedPerKey: Key 级 RPM 是独立硬上限：Key A 用
// 尽自己的额度后，Key B 不受影响（但账户级额度仍在）。
func TestRateLimit_KeyRPMIsEnforcedPerKey(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setKeyRPMLimit(t, f.keyID, 1)
	key2 := f.newKey(t)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited (Key RPM=1)", err)
	}
	// 另一个 Key 不受 Key A 的额度影响（账户级额度未设限）。
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key2,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 3 (other key): %v", err)
	}
}

// TestRateLimit_PolicyChangeDoesNotResetTheMinute: 换套餐（策略版本切换）
// 不能重置这一分钟的消费 —— 计数键不含 policy version。
func TestRateLimit_PolicyChangeDoesNotResetTheMinute(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)

	p, key := f.principal()
	for i := 0; i < 2; i++ {
		if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
			domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	// 切换到另一个策略版本（同样 RPM=2）——一分钟消费不得被重置。
	pol2 := &postgres.PolicyVersion{
		Name: "coding-plan-v2", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(1_000_000_000), WeeklyLimit: micro(10_000_000_000),
		MonthlyLimit: micro(100_000_000_000), RPMLimit: intPtr(2), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol2); err != nil {
		t.Fatalf("insert policy v2: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET policy_version_id=$1 WHERE id=$2`,
		pol2.ID, f.entID); err != nil {
		t.Fatalf("re-point entitlement: %v", err)
	}
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("after policy change err = %v, want rate_limited (换套餐不得重置一分钟消费)", err)
	}
}

// TestRateLimit_UpstreamFailureStillCountsTheRequest: 准入成功后上游失败
// 仍然计 RPM（请求已经消耗了一次调用额度）。上游用 400 拒绝（确定零消费
// → 释放预占，不冷却账号），因此第二次调用仍能出站。
func TestRateLimit_UpstreamFailureStillCountsTheRequest(t *testing.T) {
	var calls int
	var mu sync.Mutex
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
			return
		}
		nonStreamUsage(10, 5)(w, r, body)
	})
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("the first call must fail (upstream 500)")
	}
	if domain.CodeOf(err) == domain.CodeRateLimited {
		t.Fatalf("err = %v, want an upstream failure", err)
	}
	// 第二次仍然放行（只计了 1 次），第三次必须被拒（已计 2 次）。
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	_, err = f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 3 err = %v, want rate_limited (上游失败后仍计 RPM)", err)
	}
}

// ---------------------------------------------------------------------------
// TPM
// ---------------------------------------------------------------------------

// TestRateLimit_TPM_InFlightHoldBlocksAndSettlementReleases: TPM=100；已有
// 60 token 在途（长流未结束）时，再申请 50 必须被拒；第一个请求结算实际
// 20 token 后，50 可以准入（预占换成实际结算量）。
func TestRateLimit_TPM_InFlightHoldBlocksAndSettlementReleases(t *testing.T) {
	up := newUpstream(t, streamUsage(5, 15)) // 归一化总 token = 20
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 0, 100)

	p, key := f.principal()
	// 请求 1：输入上界 20 + 输出 cap 40 = 60 token 预占，保持在途。
	out1, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(true, 40))
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if out1.Stream == nil {
		t.Fatal("call 1 must be streaming")
	}
	// 请求 2：输入上界 20 + 输出 cap 30 = 50 token 预占 → 60 + 50 > 100。
	_, err = f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(true, 30))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited (60 在途 + 50 预占 > TPM 100)", err)
	}
	// 请求 1 结束并结算（实际 20 token）→ 在途预占消失，窗口内结算 20。
	drainStream(t, out1, EndCompleted)
	// 请求 3：20 + 50 = 70 ≤ 100 → 放行。
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(true, 30)); err != nil {
		t.Fatalf("call 3: %v", err)
	}
}

// TestRateLimit_TPM_OverHoldCountsTheActualTotal: 实际 token 超过预占时按
// 实际总量计（不得截到预占值）：TPM=120，实际 100（预占只有 60）→ 再申请
// 50 必须被拒（100 + 50 > 120；若截到预占 60 则 60 + 50 ≤ 120 会放行）。
func TestRateLimit_TPM_OverHoldCountsTheActualTotal(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(40, 60)) // 实际 100 token > 预占 60
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 0, 120)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(false, 40)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(false, 50))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited (实际 100 token 全额计入 TPM)", err)
	}
}

// TestRateLimit_TPM_UnknownUsageKeepsTheHold: 上游执行状态未知（停放进
// 核对）时不得释放 token 预占（更不得记零）——TPM 额度仍然被占用。
func TestRateLimit_TPM_UnknownUsageKeepsTheHold(t *testing.T) {
	var calls int
	var mu sync.Mutex
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// 200 + 非法 body：上游是否执行未知 → 请求停放，额度不释放。
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-broken","object":"chat.completion"`)
			return
		}
		nonStreamUsage(10, 5)(w, r, body)
	})
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 0, 100)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(false, 40))
	if err == nil {
		t.Fatal("the broken upstream must fail the call")
	}
	// 预占仍在途 → 再申请 50 必须被拒（60 + 50 > 100）。
	_, err = f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, tinyReq(false, 30))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited (unknown 用量不得释放预占)", err)
	}
}

// ---------------------------------------------------------------------------
// 钱包路径（PAYG / 套餐外）与 DB 故障
// ---------------------------------------------------------------------------

// TestRateLimit_WalletPathAppliesTheSameLimits: 套餐耗尽 + 策略允许套餐外
// + 客户已开启钱包消费 → 钱包路径同样受 RPM 限制；被速率拒绝的那次钱包
// 零扣款（不得用钱包绕过 RPM）。
func TestRateLimit_WalletPathAppliesTheSameLimits(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	seedWalletPrice(t, f)
	fundAndEnable(t, f, 100_000_000, 10_000_000)
	// 套餐耗尽 + allow_overage + RPM=2 的策略。
	zero := domain.Microcredit(0)
	pol := &postgres.PolicyVersion{
		Name: "exhausted-plan-rpm", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(0), WeeklyLimit: micro(0), MonthlyLimit: &zero,
		OveragePolicy: "allow_overage", RPMLimit: intPtr(2), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatalf("insert exhausted policy: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET policy_version_id=$1 WHERE id=$2`,
		pol.ID, f.entID); err != nil {
		t.Fatal(err)
	}

	p, key := f.principal()
	for i := 0; i < 2; i++ {
		if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
			domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	before := f.walletBalance(t)
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 3 err = %v, want rate_limited (钱包路径同样受 RPM 限制)", err)
	}
	if after := f.walletBalance(t); after != before {
		t.Errorf("wallet balance changed on a rate rejection: %d → %d (速率拒绝零扣款)", before, after)
	}
}

// TestRateLimit_DBOutageFailsClosed: 数据库不可用时拒绝新推理（fail
// closed），且失败原因绝不能伪装成用户额度耗尽。
func TestRateLimit_DBOutageFailsClosed(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 2, 0)
	gw, db2 := f.secondGateway(t)

	// 断开第二个实例的数据库连接 → 准入必须失败。
	_ = db2.Close()

	p, key := f.principal()
	_, err := gw.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("a database outage must not admit the request")
	}
	switch domain.CodeOf(err) {
	case domain.CodeQuotaExceeded, domain.CodeRateLimited:
		t.Fatalf("err = %v, want a service error (DB 故障不得伪装成额度耗尽)", err)
	}
}

// walletBalance returns the account wallet's cash available amount.
func (f *fixture) walletBalance(t *testing.T) int64 {
	t.Helper()
	view, err := f.store.WalletBalance(context.Background(), f.accountID, "CNY", time.Now().UTC())
	if err != nil {
		t.Fatalf("wallet balance: %v", err)
	}
	return view.Balance.CashAvailable
}

// TestRateLimit_WalletFreezeFailureRollsBackTheWholeAdmission: 钱包冻结
// 失败（余额不足）→ 整次准入回滚：没有速率计数、没有请求行、钱包零扣款。
func TestRateLimit_WalletFreezeFailureRollsBackTheWholeAdmission(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	seedWalletPrice(t, f)
	// 套餐耗尽 + 允许套餐外，但**没有钱包**（从未充值）→ 钱包冻结失败。
	zero := domain.Microcredit(0)
	pol := &postgres.PolicyVersion{
		Name: "exhausted-plan-nowallet", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(0), WeeklyLimit: micro(0), MonthlyLimit: &zero,
		OveragePolicy: "allow_overage", RPMLimit: intPtr(5), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatalf("insert exhausted policy: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET policy_version_id=$1 WHERE id=$2`,
		pol.ID, f.entID); err != nil {
		t.Fatal(err)
	}

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("the wallet admission must fail without a wallet")
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0 (the admission failed)", up.calls.Load())
	}
	// 整次回滚：没有任何速率计数残留（下一��请求仍按新的额度准入）。
	var rateRows int
	if err := f.db.Get(&rateRows, `SELECT COUNT(*) FROM inference_rate_reservations`); err != nil {
		t.Fatal(err)
	}
	if rateRows != 0 {
		t.Errorf("rate rows = %d, want 0 (the failed admission left no rate count)", rateRows)
	}
	var requests int
	if err := f.db.Get(&requests, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (the failed admission left no request row)", requests)
	}
	// 额度没有被消耗：额度上限仍是 5，速率上限也没被扣掉。
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err == nil {
		t.Fatal("the wallet admission must fail without a wallet (again)")
	}
	var rateRowsAfter int
	if err := f.db.Get(&rateRowsAfter, `SELECT COUNT(*) FROM inference_rate_reservations`); err != nil {
		t.Fatal(err)
	}
	if rateRowsAfter != 0 {
		t.Errorf("rate rows = %d, want 0 (a repeated failure must not accumulate counts)", rateRowsAfter)
	}
}

// TestRateLimit_RetryAfterOnlyWhenTheRecoveryIsDeterminable: 速率拒绝的
// Retry-After 只在**每个**阻断约束都有可确定恢复时刻时给出：账户 RPM 的
// 恢复时刻可确定（有倒计时）；被在途 TPM 阻断时不可确定（没有倒计时）。
func TestRateLimit_RetryAfterOnlyWhenTheRecoveryIsDeterminable(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up)
	f.setPolicyRateLimits(t, 1, 0)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited", err)
	}
	var rle *domain.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want a typed rate rejection", err)
	}
	if rle.RetryAfterSeconds <= 0 {
		t.Errorf("retry-after = %d, want > 0 (the account RPM recovery is determinable)", rle.RetryAfterSeconds)
	}

	// TPM 被在途预占阻断 → 不可确定 → 不给倒计时。
	up2 := newUpstream(t, streamUsage(5, 15))
	f2 := newFixture(t, up2)
	f2.setPolicyRateLimits(t, 0, 100)
	p2, key2 := f2.principal()
	out, err := f2.gateway.ChatCompletions(context.Background(), p2, key2,
		domain.ProtocolOpenAIChat, tinyReq(true, 40))
	if err != nil {
		t.Fatalf("call 1 (in flight): %v", err)
	}
	_, err = f2.gateway.ChatCompletions(context.Background(), p2, key2,
		domain.ProtocolOpenAIChat, tinyReq(true, 30))
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("call 2 err = %v, want rate_limited", err)
	}
	var rle2 *domain.RateLimitError
	if !errors.As(err, &rle2) {
		t.Fatalf("err = %v, want a typed rate rejection", err)
	}
	if rle2.RetryAfterSeconds != 0 {
		t.Errorf("retry-after = %d, want 0 (an in-flight TPM hold has no determinable recovery)", rle2.RetryAfterSeconds)
	}
	drainStream(t, out, EndCompleted)
}
