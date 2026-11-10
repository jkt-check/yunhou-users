package workers

// key_concurrency_recovery_test.go — Task 5（评审修复第 2 项）：恢复路径上的
// Key 级并发租约。
//
// 钉住的事实（真实 recovery worker + 真实 store + 一次性 PostgreSQL；skip
// 不算通过）：
//
//  1. 一个**被恢复**的请求，它的 api_key 作用域并发租约在恢复发生时**已经
//     不是活授权**（TTL 已到期）—— 恢复扫描的 live-lease guard
//     （inference_concurrency_leases.state='held' AND expires_at > now()）
//     因此放行它；租约 TTL（10 分钟）短于恢复宽限期（15 分钟），所以生产
//     上「够老到可以被扫描」的请求，其租约必然已经到期。
//  2. 恢复之后，**同一个 Key 的并发槽可以再次取得**：过期租约在取锁时被
//     回收（state='expired'），随后的准入放行 —— 崩溃的请求不会永久冻结
//     该 Key 的并发槽。
//
// 用例刻意使用**无五小时窗口**的策略（赠送只限并发的合法形状）：恢复路径会
// 作废「未使用的五小时窗口」，那与本用例的断言面无关，剔除后断言只剩并发
// 槽这一件事。

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/quota"
)

// keyLeaseTTL 是本用例的租约寿命：崩溃之后租约自然到期（TTL 回收是并发槽
// 的收敛机制）。压缩它只是为了不必等满 10 分钟。
const keyLeaseTTL = 50 * time.Millisecond

// concurrentOnlyPolicy 插入一个**只限并发、没有五小时窗口**的权益策略。
func (f *workerFixture) concurrentOnlyPolicy(t *testing.T) string {
	t.Helper()
	pol := &postgres.PolicyVersion{
		Name: "concurrent-only-" + f.modelID, Revision: 1, ModelIDs: []string{f.modelID},
		WeeklyLimit: microP(10_000_000), MonthlyLimit: microP(100_000_000),
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	return pol.ID
}

// entitlementForPolicy 插入一条指向该策略的权益（请求行的 FK 目标）。
func (f *workerFixture) entitlementForPolicy(t *testing.T, policyID string) *domain.Entitlement {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	ent := &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: policyID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := f.store.InsertEntitlement(context.Background(), ent); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
	return ent
}

// creditPrice 插入 sale_credit 价目（请求行按准入价目钉住 price_version_id）。
func (f *workerFixture) creditPrice(t *testing.T) accounting.PriceVersion {
	t.Helper()
	pv := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: time.Now().UTC().Add(-time.Minute),
	}
	if err := f.store.InsertPriceVersion(context.Background(), pv); err != nil {
		t.Fatalf("insert credit price: %v", err)
	}
	pure, err := pv.Pure()
	if err != nil {
		t.Fatalf("pure price: %v", err)
	}
	return pure
}

// admitCmd 构造一次真实准入的入参（quota 服务 + 真实 store 走完整路径）。
func (f *workerFixture) admitCmd(t *testing.T, ent *domain.Entitlement, price accounting.PriceVersion) quota.AdmitCommand {
	t.Helper()
	weekly, monthly := domain.Microcredit(10_000_000), domain.Microcredit(100_000_000)
	one := int64(1)
	return quota.AdmitCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
			EntitlementID: ent.ID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
		},
		Entitlement: *ent,
		Policy: quota.Policy{
			Name: "concurrent-only", Revision: 1, ModelIDs: []string{f.modelID},
			WeeklyLimit: &weekly, MonthlyLimit: &monthly,
		},
		CreditPrice:          price,
		Model:                domain.Model{ID: f.modelID, ContextTokens: 200_000, MaxOutputTokens: 8192},
		EstimatedInputTokens: 100,
		ClientMaxTokens:      &one,
	}
}

// heldKeyLeases 统计该 Key 名下**状态为 held** 的 api_key 租约行（不论是否
// 已过期 —— 过期行在被下一次取锁回收前仍是 held）。
func (f *workerFixture) heldKeyLeases(t *testing.T, keyID string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
		WHERE scope='api_key' AND scope_id=$1 AND state='held'`, keyID).Scan(&n); err != nil {
		t.Fatalf("count held key leases: %v", err)
	}
	return n
}

// liveKeyLeases 统计该 Key 名下**仍是活授权**的 api_key 租约（held 且未到期）。
func (f *workerFixture) liveKeyLeases(t *testing.T, keyID string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
		WHERE scope='api_key' AND scope_id=$1 AND state='held' AND expires_at > now()`, keyID).Scan(&n); err != nil {
		t.Fatalf("count live key leases: %v", err)
	}
	return n
}

// waitKeyLeaseExpired 等到该 Key 的 api_key 租约到期（不再是活授权）。
func (f *workerFixture) waitKeyLeaseExpired(t *testing.T, keyID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var expired int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
			WHERE scope='api_key' AND scope_id=$1 AND state='held' AND expires_at <= now()`, keyID).
			Scan(&expired); err != nil {
			t.Fatalf("count expired key leases: %v", err)
		}
		if expired > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the api_key lease did not expire within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds — 恢复后的 Key
// 并发槽归还：被恢复请求的 api_key 租约已不是活授权，随后的准入放行。
func TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()

	// 该 Key 自设并发上限 1 → 准入时取一把 api_key 作用域的租约。
	if _, err := f.db.ExecContext(ctx,
		`UPDATE inference_api_keys SET concurrency_limit = 1 WHERE id = $1`, f.keyID); err != nil {
		t.Fatalf("set the key concurrency limit: %v", err)
	}
	policyID := f.concurrentOnlyPolicy(t)
	ent := f.entitlementForPolicy(t, policyID)
	price := f.creditPrice(t)

	// 真实准入：请求行、周/月窗口预占、Key 预算预占与 api_key 租约在**同一
	// 个事务**里提交。
	svc := quota.NewService(f.store, nil)
	svc.RequestLeaseTTL = keyLeaseTTL
	adm, err := svc.Admit(ctx, f.admitCmd(t, ent, price))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if held := f.heldKeyLeases(t, f.keyID); held != 1 {
		t.Fatalf("held api_key leases right after the admission = %d, want 1", held)
	}

	// 租约到期 → 不再是活授权（live-lease guard 因此放行恢复扫描）。
	f.waitKeyLeaseExpired(t, f.keyID)
	if live := f.liveKeyLeases(t, f.keyID); live != 0 {
		t.Fatalf("live api_key leases after the expiry = %d, want 0", live)
	}

	// 真实 recovery worker 跑一轮（固定时钟让它显得陈旧，grace 已过）。
	stats, err := f.newWorker().RunPass(ctx)
	if err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if stats.ReleasedNotSent != 1 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want one released-not-sent", stats)
	}
	status, usage, _, _ := f.requestState(t, adm.RequestID)
	if status != "released" || usage == "unknown" {
		t.Errorf("request = %s/%s, want released (预占后崩溃 → 确认零消费 → 释放)", status, usage)
	}
	// 恢复之后：该 Key 的 api_key 租约仍然不是活授权（恢复不释放它，TTL 才是
	// 收敛机制；这里断言的是「不是活授权」这一事实）。
	if live := f.liveKeyLeases(t, f.keyID); live != 0 {
		t.Errorf("live api_key leases after the recovery = %d, want 0", live)
	}

	// 随后的准入放行：过期租约在取锁时被回收，Key 的并发槽归还。
	if _, err := svc.Admit(ctx, f.admitCmd(t, ent, price)); err != nil {
		t.Fatalf("subsequent admission on the same key: %v", err)
	}
	// 回收证据：旧租约行被回收成 expired，新租约行持有槽位。
	var reclaimed, held int
	if err := f.db.QueryRow(`SELECT
		  COUNT(*) FILTER (WHERE state='expired'),
		  COUNT(*) FILTER (WHERE state='held')
		FROM inference_concurrency_leases WHERE scope='api_key' AND scope_id=$1`, f.keyID).
		Scan(&reclaimed, &held); err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 || held != 1 {
		t.Errorf("api_key lease rows = %d expired / %d held, want 1/1 (旧租约被 TTL 回收，新租约持有槽位)",
			reclaimed, held)
	}
}
