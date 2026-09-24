package workers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// start_panic_guard_test.go — 审查修复 Important-1：五个 worker 的 Start
// 循环必须兜底每轮 pass 的 panic（runGuarded）。注入会 panic 的依赖，断言
// worker 循环存活（继续跑后续 tick）且取消后干净退出——若 recover 缺失，
// panic 会直接崩掉整个测试进程，测试响亮失败。

func assertLoopSurvivesPanics(t *testing.T, name string, calls *atomic.Int32, start func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		start(ctx)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not stop after cancel", name)
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("%s ran %d passes, want >= 2 — worker loop must survive a panicking pass", name, n)
	}
}

// panicRecoveryStore 在阶段 1 扫描处 panic（嵌入 nil 接口，其余方法不可达）。
type panicRecoveryStore struct {
	RecoveryStore
	calls *atomic.Int32
}

func (s panicRecoveryStore) ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error) {
	s.calls.Add(1)
	panic("injected store panic")
}

func TestStart_SettlementRecoverySurvivesPanickingPass(t *testing.T) {
	calls := &atomic.Int32{}
	w := NewSettlementRecovery(panicRecoveryStore{calls: calls}, nil, RecoveryConfig{
		Interval: 10 * time.Millisecond, BatchLimit: 10,
	}, nil)
	assertLoopSurvivesPanics(t, "settlement-recovery", calls, w.Start)
}

// panicCredStore 在到期扫描处 panic。
type panicCredStore struct {
	CredentialRefreshStore
	calls *atomic.Int32
}

func (s panicCredStore) ListOAuthCredentialsExpiring(ctx context.Context, before time.Time, limit int) ([]domain.Credential, error) {
	s.calls.Add(1)
	panic("injected store panic")
}

func TestStart_CredentialRefreshSurvivesPanickingPass(t *testing.T) {
	calls := &atomic.Int32{}
	w := NewCredentialRefresh(panicCredStore{calls: calls}, nil,
		CredentialRefreshConfig{Interval: 10 * time.Millisecond, BatchLimit: 10}, nil)
	assertLoopSurvivesPanics(t, "credential-refresh", calls, w.Start)
}

// panicHealthStore 在 TTL 绑定清扫（pass 第一步）处 panic。
type panicHealthStore struct {
	UpstreamHealthStore
	calls *atomic.Int32
}

func (s panicHealthStore) EndExpiredSessionBindings(ctx context.Context, now time.Time, limit int) (int64, error) {
	s.calls.Add(1)
	panic("injected store panic")
}

func TestStart_UpstreamHealthSurvivesPanickingPass(t *testing.T) {
	calls := &atomic.Int32{}
	w := NewUpstreamHealth(panicHealthStore{calls: calls}, nil, nil, nil, nil, nil,
		UpstreamHealthConfig{Interval: 10 * time.Millisecond, BatchLimit: 10}, nil)
	assertLoopSurvivesPanics(t, "upstream-health", calls, w.Start)
}

// panicEntitlementStore 在 outbox 拉取处 panic（pass 级 panic，per-message
// recover 覆盖不到）。
type panicEntitlementStore struct {
	EntitlementSyncStore
	calls *atomic.Int32
}

func (s panicEntitlementStore) FetchPendingOutboxByTopic(ctx context.Context, topic string, limit int) ([]postgres.OutboxMessage, error) {
	s.calls.Add(1)
	panic("injected store panic")
}

func TestStart_EntitlementSyncSurvivesPanickingPass(t *testing.T) {
	calls := &atomic.Int32{}
	w := NewEntitlementSync(panicEntitlementStore{calls: calls}, nil,
		EntitlementSyncConfig{Interval: 10 * time.Millisecond, BatchLimit: 10})
	assertLoopSurvivesPanics(t, "entitlement-sync", calls, w.Start)
}

// panicWalletStore 在 outbox 拉取处 panic。
type panicWalletStore struct {
	WalletSyncStore
	calls *atomic.Int32
}

func (s panicWalletStore) FetchPendingOutboxByTopic(ctx context.Context, topic string, limit int) ([]postgres.OutboxMessage, error) {
	s.calls.Add(1)
	panic("injected store panic")
}

func TestStart_WalletSyncSurvivesPanickingPass(t *testing.T) {
	calls := &atomic.Int32{}
	w := NewWalletSync(panicWalletStore{calls: calls}, nil,
		EntitlementSyncConfig{Interval: 10 * time.Millisecond, BatchLimit: 10})
	assertLoopSurvivesPanics(t, "wallet-sync", calls, w.Start)
}
