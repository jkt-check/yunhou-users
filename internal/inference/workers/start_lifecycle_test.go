package workers

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/providers/connector"
)

// start_lifecycle_test.go — Task 16 覆盖率补强：四个 worker 的 Start 生命
// 周期（启动即跑一轮 → tick 循环 → ctx 取消后干净退出）。语义深测在各
// 专项测试文件；这里钉的是"循环会跑、会停"。

func startAndStop(t *testing.T, name string, start func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		start(ctx)
		close(done)
	}()
	time.Sleep(120 * time.Millisecond) // 覆盖启动轮 + 至少一次 tick
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not stop after cancel", name)
	}
}

func TestStart_SettlementRecoveryLifecycle(t *testing.T) {
	f := newWorkerFixture(t)
	w := NewSettlementRecovery(f.store, f.clock, RecoveryConfig{
		Interval: 20 * time.Millisecond, BatchLimit: 10,
		Grace: time.Hour, ReconciliationDeadline: 24 * time.Hour,
	}, nil)
	startAndStop(t, "settlement-recovery", w.Start)
}

func TestStart_EntitlementSyncLifecycle(t *testing.T) {
	f := newSyncFixture(t)
	w := NewEntitlementSync(f.store, nil, EntitlementSyncConfig{Interval: 20 * time.Millisecond, BatchLimit: 10})
	startAndStop(t, "entitlement-sync", w.Start)
}

func TestStart_WalletSyncLifecycle(t *testing.T) {
	f := newSyncFixture(t)
	w := NewWalletSync(f.store, nil, EntitlementSyncConfig{Interval: 20 * time.Millisecond, BatchLimit: 10})
	startAndStop(t, "wallet-sync", w.Start)
}

func TestStart_UpstreamHealthLifecycle(t *testing.T) {
	env := newRefreshEnv(t)
	w := NewUpstreamHealth(env.store, env.vault,
		&connector.Client{HTTP: env.vendor.srv.Client()}, env.refresher(), env.registry, env.store,
		UpstreamHealthConfig{Interval: 20 * time.Millisecond, Cooldown: 5 * time.Minute}, nil)
	startAndStop(t, "upstream-health", w.Start)
}

func TestStart_CredentialRefreshLifecycle(t *testing.T) {
	env := newRefreshEnv(t)
	w := NewCredentialRefresh(env.store, env.refresher(),
		CredentialRefreshConfig{Interval: 20 * time.Millisecond, BatchLimit: 10}, nil)
	startAndStop(t, "credential-refresh", w.Start)
}
